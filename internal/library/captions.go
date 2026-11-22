package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"math/big"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// What the catalog knows of an original's closed captions (migration 058),
// window by window. A source record's closedCaptions comes from a probe that
// decodes no frame, which says false of an original whose video carries A53
// closed captions (EIA/CEA-608/708, a cc_data SEI with each picture): nothing
// in a container tells, the decoder's side data does. The captions backfill
// (package captions) hands the originals on disk to the analyzer, which
// reads each in windows across it and reports what each found;
// katalog-manager opens no media file. An original that carries closed
// captions, or whose captions are not known, is not retired: a few windows
// of a package can show that it has captions, never that it has all of them,
// so the deletion gate (captionsRule) asks of the original alone whether it
// carries them, and lets it go only where a read of every planned window
// found none.

// CaptionsUnknown begins why the gate keeps an original whose closed
// captions can't be told yet; a read of them lets the hold go
// (ReleaseCaptionsHold).
const CaptionsUnknown = "its closed captions aren't known"

// captionsUnknownWhy is why of an original whose captions no read of its
// frames found.
const captionsUnknownWhy = CaptionsUnknown + ": no read of its original's frames found whether it has them; the " +
	"captions backfill reads them (captions.backfill)"

// CaptionsCarried begins what the gate says of an original that carries
// closed captions: it is not retired, whatever its package carries.
const CaptionsCarried = "its original carries closed captions"

// captionsCarriedNote is why an original that carries closed captions is
// kept, after where they are.
const captionsCarriedNote = "an original that carries closed captions is not retired"

// CaptionWindow is a stretch of a video a read of its frames decoded: where
// in its picture (At, the fraction of the picture's span: 0.1 to 0.9, 0 its
// start),
// the presentation seconds it covered (Start to End; a window that failed
// says none), how many frames it decoded (Frames; 0, a window that failed)
// and whether one of them carried closed captions.
//
// InterlacedFrames and RepeatFrames are how many of its frames the decoder
// marked interlaced, and marked to repeat a field (telecine): a header that
// says progressive over pictures in fields is told by them. A read that says
// neither of a window (an older one) says nothing of its fields.
type CaptionWindow struct {
	At               float64 `json:"at"`
	Start            float64 `json:"start"`
	End              float64 `json:"end"`
	Frames           int     `json:"frames"`
	Captions         bool    `json:"captions"`
	InterlacedFrames *int    `json:"interlacedFrames,omitempty"`
	RepeatFrames     *int    `json:"repeatFrames,omitempty"`
	// KeyframeBefore says the read's packet listing showed no keyframe
	// between the window's first picture and its place: it starts at the
	// keyframe at or before its place, however far back (a long group of
	// pictures), and may lie up to WindowKeyframeSlack before it. A window
	// that decoded frames says it as a boolean or not at all (false); null
	// or any other value fails it, and so does true of a read of the whole
	// picture, which has no place before its start.
	KeyframeBefore bool `json:"keyframeBefore,omitempty"`
	// Unreadable says why the window, as the read reported it, is no window
	// the gate can take - a value of another type than a window's (a string
	// for a number, a number for a boolean), one out of its range, captions
	// of a window that decoded no frame, a count of more frames than it
	// decoded, an entry that is no object: it is a failed window (Frames
	// 0), never left out of the read.
	Unreadable string `json:"unreadable,omitempty"`
	// CountsUnreadable says a count of its interlaced or repeated frames
	// could not be read: its fields count as in fields.
	CountsUnreadable bool `json:"countsUnreadable,omitempty"`
	// Whole says the window is a read of the whole picture (WholeRead):
	// every packet decoded, alone, at the start; Packets is how many packets
	// of the picture it counted, and Leading its leading pictures (the
	// packets before its first keyframe in decode order and the first gop's
	// packets timed before it, at most WholeLeadingMax; none said: the read
	// is not complete).
	Whole   bool `json:"whole,omitempty"`
	Packets *int `json:"packets,omitempty"`
	Leading *int `json:"leading,omitempty"`
	// decoded is how many frames it decoded as its read reported them, of a
	// window that failed afterwards (Frames 0)
	decoded int
	// stored says it carries the catalog's own unreadable, as a window the
	// catalog kept as failed does (a read doesn't say it)
	stored bool
}

// Stored reports whether the window carries the catalog's own key
// (unreadable), which a window the catalog kept as failed has and a read
// doesn't say.
func (w CaptionWindow) Stored() bool { return w.stored }

// UnmarshalJSON reads a window as a read reports it, strictly: what is no
// window's value makes it a failed one (Unreadable), never an error that
// drops it nor a default that reads as "none". A window that decoded frames
// says whether one showed captions (a boolean) and the seconds it covered;
// one that decoded none says no captions; its counts are whole numbers no
// greater than its frames, or none; no key of it is given twice. What a
// window that decoded frames found stays though the rest of it fails - its
// captions shown (true in any value it gives of them) and the frames it
// counted interlaced or repeating a field (the most it gives): the worst a
// read reports is what is kept.
func (w *CaptionWindow) UnmarshalJSON(b []byte) error {
	*w = CaptionWindow{At: -1}
	fields, ok := objectFields(b)
	if !ok {
		w.Unreadable = "it is no window (no JSON object)"
		return nil
	}
	m := map[string]json.RawMessage{}
	all := map[string][]json.RawMessage{}
	var repeated []string
	for _, f := range fields {
		// a key in another case is the same key (as encoding/json reads it)
		k := strings.ToLower(f.key)
		if _, seen := m[k]; seen && !slices.Contains(repeated, f.key) {
			repeated = append(repeated, f.key)
		}
		m[k] = f.value
		all[k] = append(all[k], f.value)
	}
	isNull := func(raw json.RawMessage) bool { return raw == nil || strings.TrimSpace(string(raw)) == "null" }
	number := func(k string) (float64, bool) {
		var f float64
		if isNull(m[k]) || json.Unmarshal(m[k], &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	whole := func(raw json.RawMessage) (int, bool) {
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		return n, err == nil && !isNull(raw)
	}
	boolean := func(raw json.RawMessage) (bool, bool) {
		var v bool
		return v, !isNull(raw) && json.Unmarshal(raw, &v) == nil
	}
	// what it found, of every value it gives: captions true in any, the
	// most frames counted interlaced or repeating a field
	showed := func() bool {
		for _, raw := range all["captions"] {
			if c, ok := boolean(raw); ok && c {
				return true
			}
		}
		return false
	}
	most := func(k string, frames int) *int {
		var out *int
		for _, raw := range all[k] {
			if n, ok := whole(raw); ok && n > 0 && (frames < 0 || n <= frames) && (out == nil || n > *out) {
				out = &n
			}
		}
		return out
	}
	// fail makes it a failed window, why, keeping what it found where it
	// decoded frames (frames, -1 for a count that can't be told)
	fail := func(why string, frames int) error {
		w.Frames, w.Captions, w.Start, w.End, w.InterlacedFrames, w.RepeatFrames = 0, false, 0, 0, nil, nil
		w.KeyframeBefore = false
		if frames != 0 {
			w.Captions = showed()
			w.InterlacedFrames, w.RepeatFrames = most("interlacedframes", frames), most("repeatframes", frames)
			w.decoded = max(frames, 0)
		}
		if w.Unreadable == "" {
			w.Unreadable = why
		}
		return nil
	}
	at, atOK := number("at")
	if atOK && at >= 0 && at <= 1 {
		w.At = at
	}
	// a read of the whole picture is told by its shape, whatever else of it
	// fails
	if b, ok := boolean(m["whole"]); ok && b {
		w.Whole = true
	}
	if why, ok := m["unreadable"]; ok {
		// a window the catalog kept as one it could not take, with what it
		// found
		var s string
		if json.Unmarshal(why, &s) != nil || strings.TrimSpace(s) == "" {
			s = "it could not be read"
		}
		w.CountsUnreadable = strings.TrimSpace(string(m["countsunreadable"])) == "true"
		w.stored = true
		return fail(strings.TrimSpace(s), -1)
	}
	if len(repeated) > 0 {
		frames, ok := whole(m["frames"])
		if !ok || frames < 0 {
			frames = 0
		}
		for _, raw := range all["frames"] {
			if n, ok := whole(raw); ok && n > frames {
				frames = n
			}
		}
		return fail("its "+strings.Join(repeated, ", ")+" is given more than once", frames)
	}
	frames, ok := whole(m["frames"])
	if !ok || frames < 0 {
		// how many frames it decoded can't be told: what it found stays
		frames = -1
	}
	if w.At < 0 {
		return fail("its at is no fraction of the video from 0 to 1", frames)
	}
	if frames < 0 {
		return fail("its frames is no whole number of frames, 0 or more", frames)
	}
	w.Frames = frames
	if frames == 0 {
		if c, ok := boolean(m["captions"]); ok && c {
			return fail("a window that decoded no frame shows no captions", 0)
		}
		if !isNull(m["captions"]) {
			if _, ok := boolean(m["captions"]); !ok {
				return fail("its captions is no boolean", 0)
			}
		}
		w.Frames = 0
		return nil
	}
	c, ok := boolean(m["captions"])
	if !ok {
		return fail("a window that decoded frames says whether they showed captions (a boolean)", frames)
	}
	w.Captions = c
	start, okS := number("start")
	end, okE := number("end")
	if !okS || !okE || start < 0 || end < start {
		return fail("start and end are the seconds it covered, end not before start", frames)
	}
	w.Start, w.End = start, end
	for _, k := range []string{"interlacedFrames", "repeatFrames"} {
		raw := m[strings.ToLower(k)]
		if isNull(raw) {
			continue
		}
		n, ok := whole(raw)
		if !ok || n < 0 || n > frames {
			w.CountsUnreadable = true
			return fail("its "+k+" is no count of its frames", frames)
		}
		if k == "interlacedFrames" {
			w.InterlacedFrames = &n
		} else {
			w.RepeatFrames = &n
		}
	}
	if raw, has := m["keyframebefore"]; has {
		kb, ok := boolean(raw)
		if !ok {
			return fail("its keyframeBefore is no boolean", frames)
		}
		w.KeyframeBefore = kb
	}
	if raw, ok := m["whole"]; ok && !w.Whole && !isNull(raw) {
		return fail("its whole is not true (a read of the whole picture says whole: true)", frames)
	}
	for _, k := range []string{"packets", "leading"} {
		raw, has := m[k]
		if !has || (k == "packets" && isNull(raw)) {
			continue
		}
		n, ok := whole(raw)
		if !ok || n < 0 || (k == "leading" && n > WholeLeadingMax) {
			return fail(fmt.Sprintf("its %s is no count of packets (leading: 0 to %d)", k, WholeLeadingMax), frames)
		}
		if k == "packets" {
			w.Packets = &n
		} else {
			w.Leading = &n
		}
	}
	if w.Whole {
		switch {
		case w.Packets == nil:
			return fail("a read of the whole picture says how many packets it counted (packets)", frames)
		case frames > *w.Packets:
			return fail("a read of the whole picture decoded more frames than its packets", frames)
		case w.Leading != nil && *w.Leading > *w.Packets:
			return fail("a read of the whole picture counts more packets before its first keyframe than it has", frames)
		case w.KeyframeBefore:
			return fail("a read of the whole picture starts at its start, at no keyframe before a place "+
				"(keyframeBefore)", frames)
		}
	}
	return nil
}

// Decoded is how many frames the window decoded as its read reported it,
// one that failed afterwards too (Frames is 0 for one that failed).
func (w CaptionWindow) Decoded() int { return max(w.Frames, w.decoded) }

// Found reports whether the window shows captions or counted frames in
// fields (interlaced, or repeating a field): what keeps an original for good,
// whatever else of the read failed.
func (w CaptionWindow) Found() bool {
	return w.Captions || (w.InterlacedFrames != nil && *w.InterlacedFrames > 0) ||
		(w.RepeatFrames != nil && *w.RepeatFrames > 0)
}

// field is a key of a JSON object and its value, as the object gives them.
type field struct {
	key   string
	value json.RawMessage
}

// objectFields are the keys of the JSON object b and their values, in its
// order, a key given twice twice; ok is false for what is no object.
func objectFields(b []byte) ([]field, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var out []field
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false
		}
		k, ok := t.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, field{k, v})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return out, true
}

// CaptionsRead is what a read of an original's frames found of its closed
// captions, window by window, the length of the video the read placed its
// windows by (Duration, seconds; nil when it said none), how many streams of
// closed captions of their own it found (CaptionStreams: an EIA-608 track,
// one tagged c608 or c708) and what of the file is off the analyzer's
// closed list (OffList, as "codec:vc1").
type CaptionsRead struct {
	// Finding is what the read found that keeps the original for good of
	// its file and its record (FindingLengthMismatch), "" none.
	Finding        string `json:"finding,omitempty"`
	Windows        []CaptionWindow
	Duration       *float64
	CaptionStreams *int
	OffList        []string
	// Span is the picture's span the read placed its windows over, as the
	// read says it (seconds of the picture's clock: its first frame to the
	// end of its last packet; Step unused; nil: not said), each end of which
	// the span its records give is held to.
	Span *Span
	// Gop is the longest run between two keyframes of the picture's first
	// packets the read found (seconds; nil or 0: fewer than two keyframes,
	// or not said), which sets how far from its place a window may lie
	// (inPlace).
	Gop *float64
	// ReadAt is when it was read, FFprobe the release that read it.
	ReadAt  *time.Time
	FFprobe string
	// Report is the read's report as the analyzer sent it (its "original"),
	// which the contract's deletion gate reads as the record logic reads a
	// deletion's read (RecordLogicHolds); nil: none kept.
	Report []byte
}

// SourceCaptions is what a read that decoded frames found of the closed
// captions of the source sourceID, window by window, of the file the source
// is now: a read of a file of another size, or quick hash where both say
// one, than the source's (one the scanner's Refix or an adoption put in its
// place since) is none, and so is a read that found nothing (no captions,
// no frames in fields) of another record of it than the one it is now: its
// length and its span were held to that record.
// Nil when there is none (or the catalog has no migration 058); failed says
// why the last read of it was not taken, "" when there was none, and finds
// what that read found of the file before it stopped (its streams of
// captions and what is off the closed lists; no windows), nil when nothing.
func SourceCaptions(ctx context.Context, q Querier, sourceID string) (read *CaptionsRead, failed string,
	finds *CaptionsRead, err error) {
	if ok, err := relationReady(ctx, q, store.SourceCaptionsTable); err != nil || !ok {
		return nil, "", nil, err
	}
	// without migration 060 (the span a read says) no read is one the gate
	// takes: none
	if ok, err := relationReady(ctx, q, store.CaptionsSpanIndex); err != nil || !ok {
		return nil, "", nil, err
	}
	var state string
	var windows, offList []byte
	var duration, spanStart, spanEnd, gop *float64
	var streams *int
	var reason, report, ffprobe *string
	var readAt *time.Time
	var same bool
	var finding *string
	err = q.QueryRow(ctx, `SELECT c.state, c.captionwindows, c.duration, c.readspanstart, c.readspanend, c.readgop,
			c.readreport, c.readat, c.ffprobeversion, c.captionstreams, c.offlist, c.reason,
			c.sourcesize IS NOT DISTINCT FROM s.sizebytes
			AND (c.sourceqh1 IS NULL OR s.qh1 IS NULL OR c.sourceqh1 = s.qh1)
			AND (c.finding IS NULL OR c.sourcerecordedat IS NOT DISTINCT FROM s.recordedat)
			AND (c.state <> 'read' OR c.sourcerecordedat IS NOT DISTINCT FROM s.recordedat OR c.captions IS TRUE
				OR COALESCE(c.captionstreams, 0) > 0 OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(c.captionwindows,
					'[]'::jsonb)) w WHERE (jsonb_typeof(w->'interlacedFrames') = 'number' AND (w->>'interlacedFrames')::numeric > 0)
					OR (jsonb_typeof(w->'repeatFrames') = 'number' AND (w->>'repeatFrames')::numeric > 0))), c.finding
		FROM `+store.SourceCaptionsTable+` c JOIN com_nalet_katalog_itemsources s ON s.id = c.source_id
		WHERE c.source_id = $1`, sourceID).Scan(&state, &windows, &duration, &spanStart, &spanEnd, &gop, &report, &readAt,
		&ffprobe, &streams, &offList, &reason, &same, &finding)
	var off []string
	if err == nil && offList != nil {
		if err = json.Unmarshal(offList, &off); err != nil {
			err = fmt.Errorf("closed captions' offList: %w", err)
		}
	}
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, "", nil, nil
	case err != nil:
		return nil, "", nil, err
	case !same:
		return nil, "the file read is not the one the original is now, or a read that found nothing is of another " +
			"record of it", nil, nil
	case state == "failed" && reason != nil:
		if (streams != nil && *streams > 0) || len(off) > 0 || finding != nil {
			finds = &CaptionsRead{CaptionStreams: streams, OffList: off, Duration: duration}
			if finding != nil {
				finds.Finding = *finding
			}
		}
		return nil, *reason, finds, nil
	case state != "read" || windows == nil:
		return nil, "", nil, nil
	}
	r := CaptionsRead{Duration: duration, CaptionStreams: streams, OffList: off, Gop: gop}
	if report != nil {
		r.Report = []byte(*report)
	}
	r.ReadAt = readAt
	if ffprobe != nil {
		r.FFprobe = *ffprobe
	}
	if spanStart != nil && spanEnd != nil {
		r.Span = &Span{Start: *spanStart, End: *spanEnd}
	}
	if err := json.Unmarshal(windows, &r.Windows); err != nil {
		return nil, "", nil, fmt.Errorf("closed captions' windows: %w", err)
	}
	return &r, "", nil, nil
}

// MinWindowFrames is how many frames a window decodes at least to be one:
// half of the packets a read decodes of it (WindowPackets).
const MinWindowFrames = 24

// WindowPlaceSlack is how far from its place (the plan's, of the picture's
// span as its records give it: PictureSpan), before or after it, a
// window's first frame may lie at least: the keyframe its seek lands on.
// Of a picture whose groups of pictures run longer (the read's gop), it may
// lie the gop and WindowGopPlus from it (inPlace).
const WindowPlaceSlack = 10.0

// WindowGopPlus is how far beyond the read's gop a window's first frame may
// lie from its place.
const WindowGopPlus = 0.5

// WindowKeyframeSlack is how far before its place a window's first frame may
// lie where the read says it is the keyframe at or before the place
// (KeyframeBefore): a group of pictures that long. A window is in place by
// where it starts, the keyframe its seek lands on, not by its distance.
const WindowKeyframeSlack = 30.0

// ReadRefused says why the read r of the picture of the span s (its records',
// PictureSpan) says nothing of it, "" when it may be taken: a picture whose
// places lie too close for windows (ShortPicture) read in windows, windows
// that don't lie in the order of the video (a video too short for them said
// as that, TooShortForFive), no length of its own (which its record's is
// held to), no span of the picture it read or one whose start or end lies
// further than a frame step and SpanSlack from the span's, a gop that is no
// length from 0, windows that are not the plan's six - one at each of its
// fractions (the start among them: a read of five windows is not
// complete), none at another -, a window off its place (Span.Place, by
// inPlace), or one that decoded fewer than MinWindowFrames frames (a window
// that decoded none failed, which a read may have). The read's own length
// places nothing. The catalog keeps such a read as a failed one, and the
// gate holds every read to it.
func ReadRefused(r *CaptionsRead, s Span) string {
	whole := len(r.Windows) == 1 && r.Windows[0].Whole
	if !whole && slices.ContainsFunc(r.Windows, func(w CaptionWindow) bool { return w.Whole }) {
		return "a read of the whole picture stands alone, one window at its start, not among others"
	}
	if whole && r.Windows[0].At >= atSlack {
		return "a read of the whole picture is one window at its start"
	}
	gop := 0.0
	if r.Gop != nil {
		if *r.Gop < 0 || !finiteNumber(*r.Gop) {
			return "its gop is no length from 0 of the picture's longest group of pictures"
		}
		gop = *r.Gop
	}
	if gop > float64(GopPackets)*s.Step {
		return fmt.Sprintf("its gop (%g s) is longer than the %d packets a read lists keyframes among last at its "+
			"frame step (%g s): it can't be one", gop, GopPackets, float64(GopPackets)*s.Step)
	}
	if !whole && ShortPicture(s, gop) {
		return fmt.Sprintf("two of its picture's places lie closer than a window reaches (%.3g s: %d packets at its "+
			"frame step, its gop and %g s): such a picture is read whole, every packet decoded, not in windows",
			shortReach(s, gop), WindowPackets, ShortReachSlack)
	}
	if !whole {
		if why, overlap := windowsOutOfPlace(r.Windows); why != "" {
			if overlap && tooShortForFive(r, s) {
				return TooShortForFive
			}
			return why
		}
	}
	if r.Duration == nil || *r.Duration <= 0 || !finiteNumber(*r.Duration) {
		return "it says no length of the video, which its record's is held to"
	}
	if r.Span == nil || !finiteNumber(r.Span.Start) || !finiteNumber(r.Span.End) || r.Span.End <= r.Span.Start {
		return "it says no span of the picture it read, which its records' is held to"
	}
	if math.Abs(r.Span.Start-s.Start) > s.slack() || math.Abs(r.Span.End-s.End) > s.slack() {
		return fmt.Sprintf("it read a picture from %g s to %g s, and its records give one from %g s to %g s (each end "+
			"within %.3g s: a frame step and %g s)", r.Span.Start, r.Span.End, s.Start, s.End, s.slack(), SpanSlack)
	}
	if whole {
		return wholeRefused(r.Windows[0], s)
	}
	if other := offPlan(r.Windows); len(other) > 0 {
		return "its windows at " + percents(other) + " are at no fraction of the plan's (the start, 10, 30, 50, 70 and " +
			"90 % of its picture's span: a window the tail short of the end says its fraction, and starts where it lies)"
	}
	if missing := missingWindows(r); len(missing) > 0 {
		return "it has no window at " + percents(missing) + " (a read is of its start and 10, 30, 50, 70 and 90 % of " +
			"its picture's span, six windows)"
	}
	var far, few []float64
	for _, w := range r.Windows {
		switch {
		case w.Frames <= 0:
		case w.Frames < MinWindowFrames:
			few = append(few, w.At)
		case !inPlace(w, s.Place(w.At), gop, s):
			far = append(far, w.At)
		}
	}
	switch {
	case len(far) > 0:
		return fmt.Sprintf("its windows at %s lie off their places (the start, and 10 to 90 %% of its picture's span "+
			"from %g s to %g s, as its records give it, each at most %g s short of its end): %s", percents(far), s.Start,
			s.End, WindowTail, placeWords(gop, s))
	case len(few) > 0:
		return fmt.Sprintf("its windows at %s decoded fewer than %d frames", percents(few), MinWindowFrames)
	}
	return ""
}

// GopPackets is how many of a picture's first packets a read lists
// keyframes among for its gop: a gop longer than they last at the frame
// step can't be one, and the read is not known.
const GopPackets = 1200

// finiteNumber reports whether f is a number a rule reads: neither NaN nor
// an infinity, which count as not stated.
func finiteNumber(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// WholeLeadingMax is how many leading pictures (the packets before its
// first keyframe in decode order, and the first gop's packets timed before
// it) a read of the whole picture counts apart at most.
const WholeLeadingMax = 16

// wholeRefused says why the read of the whole picture w of the span s says
// nothing of it, "" when it stands for the plan's windows: it counted
// packets of at least the picture's span at its frame step (within a frame
// step and SpanSlack); it is complete by the plan's wholeComplete - it says
// its leading pictures (leading, at most
// WholeLeadingMax), and decoded every packet but them, or them too
// (packets − leading ≤ frames ≤ packets) -, and its frames are timed (it
// says where they start and end) and reach the span's start and end, each
// within a frame step and SpanSlack. A read that gives no leading is not
// complete.
func wholeRefused(w CaptionWindow, s Span) string {
	if w.Frames <= 0 || w.Packets == nil {
		return "its read of the whole picture decoded no frame it could place"
	}
	leading := -1
	if w.Leading != nil {
		leading = *w.Leading
	}
	if leading < 0 || leading > WholeLeadingMax {
		return fmt.Sprintf("its read of the whole picture says no leading pictures (leading, 0 to %d), so whether it "+
			"decoded every packet can't be told: not complete", WholeLeadingMax)
	}
	if float64(*w.Packets)*s.Step < s.Length()-s.slack() {
		return fmt.Sprintf("its read of the whole picture counted %d packets, fewer than its picture's span (%g s) "+
			"holds at its frame step (%g s, within a frame step and %g s)", *w.Packets, s.Length(), s.Step, SpanSlack)
	}
	if w.Frames < *w.Packets-leading || w.Frames > *w.Packets {
		return fmt.Sprintf("its read of the whole picture decoded %d frames of its %d packets (%d leading): not every "+
			"packet, or more frames than packets", w.Frames, *w.Packets, leading)
	}
	if math.Abs(w.Start-s.Start) > s.slack() || math.Abs(w.End-s.End) > s.slack() {
		return fmt.Sprintf("its read of the whole picture's frames (%g to %g s) don't reach its picture's start and end "+
			"(%g to %g s, within %.3g s)", w.Start, w.End, s.Start, s.End, s.slack())
	}
	return ""
}

// inPlace reports whether the window w lies at its place place (seconds),
// of a read that found the gop gop (seconds) of the picture of the span s,
// by the plan's slack: its first frame within the greater of
// WindowPlaceSlack and gop + WindowGopPlus of it, before or after; or,
// where the read says it starts at the keyframe at or before it
// (KeyframeBefore), up to WindowKeyframeSlack before it. Of a gop of 0
// (fewer than two keyframes, or none said) a window lies only at its own
// place, within a frame step and SpanSlack, and KeyframeBefore moves it
// nowhere.
func inPlace(w CaptionWindow, place, gop float64, s Span) bool {
	back := place - w.Start
	if gop == 0 {
		return math.Abs(back) <= s.slack()
	}
	return math.Abs(back) <= math.Max(WindowPlaceSlack, gop+WindowGopPlus) ||
		(w.KeyframeBefore && back >= 0 && back <= WindowKeyframeSlack)
}

// placeWords says where a window of a read that found the gop gop of the
// picture of the span s lies at its place (inPlace).
func placeWords(gop float64, s Span) string {
	if gop == 0 {
		return fmt.Sprintf("of a read that says no gop, or one of 0, a window lies within %.3g s of its place (a "+
			"frame step and %g s)", s.slack(), SpanSlack)
	}
	return fmt.Sprintf("a window lies within %g s of its place (the greater of %g s and its gop, %g s, and %g s), or "+
		"up to %g s before it at the keyframe before it (keyframeBefore)", math.Max(WindowPlaceSlack, gop+WindowGopPlus),
		WindowPlaceSlack, gop, WindowGopPlus, WindowKeyframeSlack)
}

// The window plan, one for every place that reads or checks a read's
// windows: a window at the picture's start (WindowAtStart), and one at each
// of PlannedWindows of the picture's own presentation span (Span.Place:
// start + at × (end − start), each at most WindowTail short of its end and
// never before its start) - six windows, each saying its own fraction (one
// the tail short of the end says 0.9 and starts where it lies), every one
// of which a read has before it can say "none". A picture two of whose
// places lie closer than a window reaches (ShortPicture: WindowPackets
// packets at its frame step, the read's gop and ShortReachSlack) is read
// whole instead: every packet decoded, one window at its start that stands
// for the six (WholeRead); a read whole of a longer one stands for them
// too. A window decodes WindowPackets packets and is one when at least
// MinWindowFrames of them gave a frame. The
// span is the picture's own: a picture that starts late, or ends before the
// file does, is read where it is (PictureSpan), and a read is held to it
// within a frame step and SpanSlack. These are constants of the code; the
// plan's file (library/v2/caption-windows.json of the schemas, vendored in
// testdata pinned by its sha256) is read by a test alone, never at run
// time.

// WindowAtStart says the plan has a window at the picture's start.
const WindowAtStart = true

// PlannedWindows are where the plan places its windows besides the start's,
// as fractions of the picture's span: 10, 30, 50, 70 and 90 %.
var PlannedWindows = []float64{0.1, 0.3, 0.5, 0.7, 0.9}

// WindowTail is how far short of the picture's end a window lies at least,
// in seconds.
const WindowTail = 30.0

// WindowPackets is how many video packets a read decodes of a window.
const WindowPackets = 48

// SpanSlack is how far, besides a frame step, the span a read says of its
// picture, and where a whole read starts and ends, may lie from the span its
// records give, in seconds.
const SpanSlack = 0.5

// ShortReachSlack is how far, besides its packets at the frame step and the
// read's gop, a window reaches (shortReach), in seconds.
const ShortReachSlack = 0.5

// planAts are the plan's six windows, as fractions of the picture's span,
// in their order: the start (0) and PlannedWindows.
func planAts() []float64 { return append([]float64{0}, PlannedWindows...) }

// shortReach is how far a window of a read that found the gop gop reaches
// on the picture of the span s: WindowPackets packets at its frame step,
// from a keyframe up to gop before its place, and ShortReachSlack.
func shortReach(s Span, gop float64) float64 {
	return float64(WindowPackets)*s.Step + gop + ShortReachSlack
}

// ShortPicture reports whether the plan's windows can't be read on frames
// of their own on the picture of the span s, of a read that found the gop
// gop: two of its places (Span.Place) lie closer than a window reaches
// (shortReach). Such a picture is read whole, not in windows.
func ShortPicture(s Span, gop float64) bool {
	var places []float64
	for _, at := range planAts() {
		places = append(places, s.Place(at))
	}
	sort.Float64s(places)
	reach := shortReach(s, gop)
	for i := 1; i < len(places); i++ {
		if places[i]-places[i-1] < reach {
			return true
		}
	}
	return false
}

// Span is the presentation span of an original's picture, in seconds of its
// own clock: from its first picture (Start) to its end (End), and its frame
// step (Step, seconds: a frame's length, of its frame rate; a 24th of a
// second where nothing says one).
type Span struct{ Start, End, Step float64 }

// slack is how far a span a read says, or where it starts and ends, may lie
// from s: a frame step and SpanSlack.
func (s Span) slack() float64 { return s.Step + SpanSlack }

// Length is how long the span s lasts.
func (s Span) Length() float64 { return s.End - s.Start }

// Place is where in the picture of the span s the plan's window at at (a
// fraction of it) lies, in seconds: the start's at its start, never capped;
// each other at start + at × (end − start), at most WindowTail short of its
// end and never before its start.
func (s Span) Place(at float64) float64 {
	if at < atSlack {
		return s.Start
	}
	return math.Max(s.Start, math.Min(s.Start+at*s.Length(), s.End-WindowTail))
}

// CaptionsNoLength begins why the gate keeps an original whose record says
// no length (container.durationMs): where a read's windows lie can't be told
// without one, so no read of it says anything until its record says one.
const CaptionsNoLength = CaptionsUnknown + ": its record says no length"

// FindingNoLength is the finding of a read of an original whose record said
// no length when it was handed out: final for that file and that record,
// like FindingLengthMismatch.
const FindingNoLength = "noRecordLength"

// CaptionsNoPlan begins why the gate keeps an original whose records give
// no plan of its picture (PictureSpan: its stored probe's picture's start,
// length and frame rate): where a read's windows lie can't be told without
// one, so no read of it says anything until its records give one.
const CaptionsNoPlan = CaptionsUnknown + ": its records give no plan of its picture"

// FindingNoPlan is the finding of a read of an original whose records gave
// no plan of its picture when it was handed out: final for that file and
// that record, like FindingNoLength.
const FindingNoPlan = "noPlan"

// FindingUnscanned is the finding of a read that says the captions of its
// picture can't be told by windows (UnscannedReason: an HEVC picture whose
// packets were not read whole for a caption signature): the hold of
// CaptionsUnscanned, final for that file and that record, which a record
// written again with the picture's captionSignature lets go (its original
// is read again).
const FindingUnscanned = "unscanned"

// UnscannedReason begins the reason of a read that says its picture's
// captions can't be told by windows (captionsKnown false): its packets need
// to be read whole for a caption signature.
const UnscannedReason = "hevc: needs the packets' signature"

// RecordedLengthMs is the length the source record rec says of its file
// (container.durationMs, milliseconds), and whether it says one: a JSON
// number above 0. A record without a container, without durationMs, or with
// one that is a string, a boolean, 0 or below says none.
func RecordedLengthMs(rec Doc) (float64, bool) {
	c, _ := rec.Get("container")
	cd, ok := c.(Doc)
	if !ok {
		return 0, false
	}
	v, _ := cd.Get("durationMs")
	if _, isBool := v.(bool); isBool {
		return 0, false
	}
	ms, ok := toFloat(v)
	if !ok || ms <= 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return 0, false
	}
	return ms, true
}

// seconds is v as ffprobe writes a time (a string, "0.000000", or a number),
// and whether it is one.
func seconds(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return 0, false
		}
	case bool, nil:
		return 0, false
	default:
		var ok bool
		if f, ok = toFloat(x); !ok {
			return 0, false
		}
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// PictureSpan is the plan's span of the picture of the original whose
// source record is rec and stored probe probe (ffprobe.json; nil: none), as
// the gate knows it without reading, or why there is none: the record
// logic's plan_of (libv2_records.py), in its order. The picture is the
// probe's film (film_of): its one stream whose codec_type is video and that
// is no cover by the closed lists' rule (attachedPicture), wherever a cover
// lies; its start its start_time, else the probe's format's start_time; its
// length its duration, else the record's container.durationMs (a number
// above 0), and the format's duration only where the record gives none; its
// end the start plus the length; its frame step one over its
// avg_frame_rate, or its r_frame_rate where that is exactly 0/0, each in
// digits alone (frameStepOf). A probe
// that is none, lists no streams, no film or more than one, or gives no
// start, no length above 0 or no frame rate gives no plan: a read of the
// original is then not known.
func PictureSpan(rec, probe Doc) (*Span, string) {
	if probe == nil {
		return nil, "it has no stored probe it can read (ffprobe.json) to place a read's windows by"
	}
	list, ok := valueOf(probe.Get("streams")).([]any)
	if !ok {
		return nil, "its stored probe lists no streams"
	}
	films := probeFilms(list)
	switch len(films) {
	case 0:
		return nil, "its stored probe lists no picture (a video stream that is no cover)"
	case 1:
	default:
		return nil, fmt.Sprintf("its stored probe lists %d video streams that are no cover, each of which could be "+
			"its picture", len(films))
	}
	pic := films[0]
	format, _ := valueOf(probe.Get("format")).(Doc)
	start, okStart := planSeconds(valueOf(pic.Get("start_time")))
	if !okStart {
		start, okStart = planSeconds(valueOf(format.Get("start_time")))
	}
	length, okLength := planSeconds(valueOf(pic.Get("duration")))
	if !okLength || length <= 0 {
		container, _ := valueOf(rec.Get("container")).(Doc)
		if ms, ok := planNumber(valueOf(container.Get("durationMs"))); ok && ms > 0 {
			length, okLength = ms/1000, true
		} else {
			length, okLength = planSeconds(valueOf(format.Get("duration")))
		}
	}
	step, okStep := frameStepOf(pic)
	switch {
	case !okStart:
		return nil, "its stored probe gives no start of its picture (its start_time, or its format's)"
	case !okLength || length <= 0:
		return nil, "its records give no length of its picture (its stored probe's duration of it, the record's " +
			"container.durationMs, or its stored probe's format's duration)"
	case !okStep:
		return nil, "its stored probe gives no frame rate of its picture (avg_frame_rate, or r_frame_rate where " +
			"avg_frame_rate is exactly 0/0: num/den in digits alone, both above 0, one frame a second at least)"
	}
	return &Span{Start: start, End: start + length, Step: step}, ""
}

// probeFilms are the streams of a stored probe's list that could each be
// its film (film_of): those whose codec_type is video and that are no cover
// (attachedPicture), wherever a cover lies.
func probeFilms(list []any) []Doc {
	var films []Doc
	for _, e := range list {
		if x, ok := e.(Doc); ok && valueOf(x.Get("codec_type")) == "video" && !probeCover(x) {
			films = append(films, x)
		}
	}
	return films
}

// recordPicture is the source record rec's picture as the record logic's
// picture_of takes it: its first stream of type video whose dispositions
// don't mark it attachedPic (any value Python takes for true marks it), nil
// where there is none. The record logic marks attachedPic on what the cover
// rule calls a cover (attachedPicture) and nothing else, so a record's
// picture is its stored probe's film (pictureNotFilm holds one that is not).
func recordPicture(rec Doc) Doc {
	list, _ := valueOf(rec.Get("streams")).([]any)
	for _, e := range list {
		if x, ok := e.(Doc); ok && valueOf(x.Get("type")) == "video" && !flag(x, "dispositions", "attachedPic") {
			return x
		}
	}
	return nil
}

// PictureNotFilm begins why the gate keeps an original whose record's
// picture is not its stored probe's film: what the one says of the codec
// and the field order may be another stream's than the other's, so neither
// its closed captions nor its fields are known.
const PictureNotFilm = "its record's picture is not its stored probe's film"

// pictureNotFilm says why the picture of the source record rec
// (recordPicture) is not the film of its stored probe probe (its one video
// stream that is no cover: probeFilms), the same stream by its index; ""
// where it is, or where the probe gives no one film (no plan says so).
func pictureNotFilm(rec, probe Doc) string {
	if probe == nil {
		return ""
	}
	list, _ := valueOf(probe.Get("streams")).([]any)
	films := probeFilms(list)
	if len(films) != 1 {
		return ""
	}
	film, pic := films[0], recordPicture(rec)
	if pic == nil {
		return fmt.Sprintf("%s: its record lists no picture (a video stream not marked an attached picture), and its "+
			"stored probe's film is stream %s", PictureNotFilm, pyStr(valueOf(film.Get("index"))))
	}
	a, okA := pyNumber(valueOf(pic.Get("index")))
	b, okB := pyNumber(valueOf(film.Get("index")))
	if !okA || !okB || a != b {
		return fmt.Sprintf("%s: its record's picture is stream %s, and its stored probe's film stream %s",
			PictureNotFilm, pyStr(valueOf(pic.Get("index"))), pyStr(valueOf(film.Get("index"))))
	}
	return ""
}

// planDecimal is a time or a length as text the record logic's gate_seconds
// takes: a decimal number, spaces about it.
var planDecimal = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// planSeconds is v as the record logic's gate_seconds reads a time or a
// length (ffprobe's "12.345000", or a number), and whether it is one: no
// boolean, finite.
func planSeconds(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case nil, bool:
		return 0, false
	case string:
		t := strings.TrimSpace(x)
		if !planDecimal.MatchString(t) {
			return 0, false
		}
		var err error
		if f, err = strconv.ParseFloat(t, 64); err != nil {
			return 0, false
		}
	default:
		var ok bool
		if f, ok = toFloat(x); !ok {
			return 0, false
		}
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// planNumber is v as the record logic's gate_number reads one: a number, no
// boolean, no text.
func planNumber(v any) (float64, bool) {
	switch v.(type) {
	case nil, bool, string:
		return 0, false
	}
	f, ok := toFloat(v)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// planRate is a frame rate as the record logic's frame_step_of reads one:
// "num/den" of digits alone (0-9: no space, sign or point).
var planRate = regexp.MustCompile(`^([0-9]+)/([0-9]+)$`)

// MinFrameRate is the slowest rate, in frames a second, a stored probe may
// state of its picture and give a plan (the plan's minRate): a slower one
// would stretch a frame step's tolerance past any window.
const MinFrameRate = 1

// PyIntDigits is how many digits Python's int() reads of a text at most
// (sys.int_info.default_max_str_digits): the record logic takes a longer
// run of digits for no number.
const PyIntDigits = 4300

// pyDigits is the run of digits s as Python's int() reads it, and whether
// it reads it: none of more than PyIntDigits digits.
func pyDigits(s string) (*big.Int, bool) {
	if len(s) > PyIntDigits {
		return nil, false
	}
	return new(big.Int).SetString(s, 10)
}

// frameStepOf is one over the frame rate of the probe's picture stream pic,
// in seconds, and whether it gives one, as the record logic's frame_step_of
// takes it: its avg_frame_rate, or its r_frame_rate only where
// avg_frame_rate is exactly the text "0/0" (ffprobe's of a rate it couldn't
// average), each a ratio "num/den" of digits alone (planRate), both above
// 0, of no more digits than Python reads (PyIntDigits). An avg_frame_rate
// that is missing or no such ratio gives none, r_frame_rate whatever it
// says; so does a rate it states below MinFrameRate, as Python divides:
// den/num, the nearest float, at most 1/MinFrameRate.
func frameStepOf(pic Doc) (float64, bool) {
	v, _ := valueOf(pic.Get("avg_frame_rate")).(string)
	if v == "0/0" {
		v, _ = valueOf(pic.Get("r_frame_rate")).(string)
	}
	m := planRate.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	num, ok1 := pyDigits(m[1])
	den, ok2 := pyDigits(m[2])
	if !ok1 || !ok2 || num.Sign() <= 0 || den.Sign() <= 0 {
		return 0, false
	}
	// den/num as Python's int division gives it: the float nearest the
	// ratio (one too large for a float, an infinity here, is past the floor
	// too)
	step, _ := new(big.Rat).SetFrac(den, num).Float64()
	if step > 1.0/MinFrameRate {
		return 0, false
	}
	return step, true
}

// RecordReadable says why the source record in the source folder dir can't
// be read as one the gate reads (it can't be opened, it is no JSON, an
// object of it gives a key twice, no essence object, streams that are no
// list of objects), "" when it can or when there is none (no source.json:
// a read of its original is a finding that its record says no length).
func RecordReadable(dir string) string {
	rec, err := readRecordDoc(filepath.Join(dir, "source.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		return err.Error()
	}
	return recordShape(rec)
}

// RecordedSpan is PictureSpan of the source folder dir's records
// (source.json, and ffprobe.json where it is one that can be read), nil
// where its record can't be read or says no length.
func RecordedSpan(dir string) *Span {
	rec, err := readRecordDoc(filepath.Join(dir, "source.json"))
	if err != nil {
		return nil
	}
	probe, err := readRecordDoc(filepath.Join(dir, ProbeFile))
	if err != nil || probeShape(probe) != "" {
		probe = nil
	}
	s, _ := PictureSpan(rec, probe)
	return s
}

// atSlack is how far apart two windows' places may lie and be the same: a
// read's fraction of a length it rounded.
const atSlack = 1e-3

// percent names the window at at: "start", or its share of the video,
// "10%".
func percent(at float64) string {
	if at < 0 {
		return "a place it doesn't say"
	}
	if at < atSlack {
		return "start"
	}
	return strconv.FormatFloat(math.Round(at*1000)/10, 'f', -1, 64) + "%"
}

// percents names the windows at ats, "start, 10%".
func percents(ats []float64) string {
	var out []string
	for _, at := range ats {
		out = append(out, percent(at))
	}
	return strings.Join(out, ", ")
}

// captionsRule is what keeps the original on its closed captions, of its
// source records records, their stored probes probes and what a read of its
// frames found (f.Captions; f.CaptionsFailed why the last read was not
// taken), whatever its package carries: carried says it carries them, which
// keeps it for good - a window of the read shows them (one the read
// reported otherwise malformed too: the worst a read says is kept), the read
// found a stream of closed captions of its own (its windows, or a read that
// failed once it had listed the file's streams: f.CaptionsFinds), or its
// record says it has them (its essence's closedCaptions, its picture's
// closedCaptions or caption signature, an EIA-608 stream, one tagged c608 or
// c708, of the record or of the probe, or the probe's closed_captions of its
// picture); unknown says why it can't be told, which a read may let go of.
// It fails closed: off the closing rule's lists (a codec, a container, a
// stream, more than one film, a picture the reads were not made for, in the
// records or as a read said it), a picture whose codec no record states or
// whose captions windows alone don't tell (unscanned), a record that says no
// length (PictureSpan), no read, a read not taken, a read whose windows say
// nothing of the picture (ReadRefused, of the span its records give) and a
// window that decoded no frame keep it as not known. It is asked of one
// original at a time. Only an original a read of every planned window found
// no captions in goes on them.
func captionsRule(records, probes []Doc, f GateFacts) (carried, unknown string) {
	recordSays := captionStreamOf(records, probes)
	flagUnread := ""
	for i, rec := range records {
		if recordSays != "" {
			break
		}
		switch v, has := essenceOf(rec).Get("closedCaptions"); {
		case !has || v == nil:
		case v == true:
			recordSays = "its record says so"
		case v != false:
			// a value that is no boolean says nothing it can be read by
			flagUnread = "its record's closedCaptions is no boolean"
		}
		var probe Doc
		if i < len(probes) {
			probe = probes[i]
		}
		for _, film := range filmsOf(rec, false, probe) {
			switch v, has := film.Get("captionSignature"); {
			case v == true:
				recordSays = "its record's read of every packet of its picture found a caption signature"
			case has && v != nil && v != false:
				// of any codec: a value that is no boolean says nothing it can
				// be read by
				flagUnread = "its record's captionSignature of its picture is no boolean"
			}
			switch v, has := film.Get("closedCaptions"); {
			case !has || v == nil || v == false:
			case v == true:
				recordSays = "its record says so of its picture"
			default:
				flagUnread = "its record's closedCaptions of its picture is no boolean"
			}
		}
		for _, film := range filmsOf(rec, false, probe) {
			if flag(film, "dispositions", "captions") {
				recordSays = "its record marks its picture as captions (its dispositions)"
			}
			if contractWindowsShow(pyGet(film, "captionWindows")) {
				recordSays = "a caption window its record keeps of its picture shows them"
			}
		}
		if probe != nil {
			for _, film := range filmsOf(probe, true, nil) {
				if flag(film, "disposition", "captions") {
					recordSays = "its stored probe marks its picture as captions (its disposition)"
				}
				switch probeCaptions(film) {
				case probeCaptionsSaid:
					recordSays = "its stored probe says closed_captions of its picture"
				case probeCaptionsNone:
				default:
					flagUnread = "its stored probe's closed_captions of its picture is no 0 or 1"
				}
			}
		}
	}
	r := f.Captions
	var captioned, failed, unread []float64
	var unreadWhy string
	if r != nil {
		for _, w := range r.Windows {
			switch {
			case w.Captions:
				// a window that decoded frames and showed captions, whatever
				// else of it could not be read
				captioned = append(captioned, w.At)
			case w.Unreadable != "":
				unread = append(unread, w.At)
				if unreadWhy == "" {
					unreadWhy = w.Unreadable
				}
			case w.Frames <= 0:
				failed = append(failed, w.At)
			}
		}
	}
	switch {
	case len(captioned) > 0:
		sort.Float64s(captioned)
		return CaptionsCarried + " at " + percents(captioned) + "; " + captionsCarriedNote, ""
	case (r != nil && r.CaptionStreams != nil && *r.CaptionStreams > 0) ||
		(f.CaptionsFinds != nil && f.CaptionsFinds.CaptionStreams != nil && *f.CaptionsFinds.CaptionStreams > 0):
		return CaptionsCarried + " (a read found a stream of them of its own); " + captionsCarriedNote, ""
	case recordSays != "":
		return CaptionsCarried + " (" + recordSays + "); " + captionsCarriedNote, ""
	}
	if off := offListOf(records, probes, f.offListSaid()); len(off) > 0 {
		return "", offListWhy(off)
	}
	if flagUnread != "" {
		return "", CaptionsUnknown + ": " + flagUnread
	}
	if len(records) != 1 {
		return "", fmt.Sprintf("%s: the rule is asked of one original at a time, and the gate of %d", CaptionsUnknown,
			len(records))
	}
	var probe Doc
	if len(probes) > 0 {
		probe = probes[0]
	}
	if why := pictureNotFilm(records[0], probe); why != "" {
		return "", CaptionsUnknown + ": " + why
	}
	if why := coverMarkWhy(records, probes); why != "" {
		return "", why
	}
	if why := unscanned(records, probes); why != "" {
		return "", why
	}
	if _, ok := RecordedLengthMs(records[0]); !ok {
		return "", CaptionsNoLength + " of its file (container.durationMs), so where a read's windows lie can't be told"
	}
	span, why := PictureSpan(records[0], probe)
	if span == nil {
		return "", CaptionsNoPlan + ": " + why
	}
	if fd := f.CaptionsFinds; r == nil && fd != nil {
		// final for this file and this record: no read again changes it
		switch fd.Finding {
		case FindingLengthMismatch:
			return "", CaptionsLengthMismatch + ": " + strings.TrimSpace(f.CaptionsFailed)
		case FindingNoLength:
			return "", CaptionsNoLength + " (as it said when its original was handed out to be read): " +
				strings.TrimSpace(f.CaptionsFailed)
		case FindingNoPlan:
			return "", CaptionsNoPlan + " (as they gave none when its original was handed out to be read): " +
				strings.TrimSpace(f.CaptionsFailed)
		case FindingUnscanned:
			// kept until its record carries a caption signature, which has
			// it read again
			return "", CaptionsUnscanned + ": a read of its original says so: " + strings.TrimSpace(f.CaptionsFailed)
		}
	}
	switch {
	case r == nil && f.CaptionsFailed != "":
		return "", CaptionsUnknown + ": its last read was not taken: " + f.CaptionsFailed + "; the captions backfill " +
			"reads it again (captions.backfill)"
	case r == nil:
		return "", captionsUnknownWhy
	}
	// held as the PUT holds it: of its record's length first, then where
	// its windows lie
	if why := durationMismatch(records, r); why != "" {
		return "", CaptionsLengthMismatch + ": " + why
	}
	if why := ReadRefused(r, *span); why == TooShortForFive {
		// no title with captions: a video whose planned windows can't but
		// fall on one another
		return "", CaptionsUnknown + ": " + TooShortForFive
	} else if why != "" {
		return "", CaptionsUnknown + ": a read of its original says nothing of the video: " + why + "; the captions " +
			"backfill reads it again (captions.backfill)"
	}
	if len(unread) > 0 {
		return "", CaptionsUnknown + ": its read's windows at " + percents(unread) + " can't be read (" + unreadWhy +
			"); the captions backfill reads them again (captions.backfill)"
	}
	if len(failed) > 0 {
		sort.Float64s(failed)
		return "", CaptionsUnknown + ": its original's windows at " + percents(failed) + " decoded no frame; the " +
			"captions backfill reads them again (captions.backfill)"
	}
	return "", ""
}

// What a stored probe's closed_captions of a picture says (probeCaptions).
const (
	probeCaptionsNone    = iota // absent, null, 0, "0" or false
	probeCaptionsSaid           // 1, "1" or true
	probeCaptionsUnclear        // anything else
)

// probeCaptions is what the stored probe's picture film says of its closed
// captions (closed_captions, as ffprobe 7.1 writes it once it decoded a
// frame: 0 or 1).
func probeCaptions(film Doc) int {
	v, has := film.Get("closed_captions")
	if !has || v == nil || v == false {
		return probeCaptionsNone
	}
	if v == true {
		return probeCaptionsSaid
	}
	if s, ok := v.(string); ok {
		v = json.Number(strings.TrimSpace(s))
	}
	switch n, ok := toFloat(v); {
	case ok && n == 0:
		return probeCaptionsNone
	case ok && n == 1:
		return probeCaptionsSaid
	}
	return probeCaptionsUnclear
}

// CaptionsUnscanned begins why the gate keeps an original of a codec whose
// captions windows alone don't tell (ScannedCodecs), or whose picture's
// codec no record states: only a read of every packet of its picture does,
// its record's captionSignature false; no read of windows lets it go.
const CaptionsUnscanned = "its closed captions can't be told by windows alone"

// unscanned says why the captions of the original of the records records
// (their stored probes probes) can't be told by a read of windows: its film
// is of a codec of ScannedCodecs and its record says of no read of every
// packet of it that it found no caption signature (captionSignature false),
// or neither its record nor its probe states its film's codec (absent, no
// string, blank, ffprobe's unknown: it may be one of them); "" otherwise.
func unscanned(records, probes []Doc) string {
	for i, rec := range records {
		var probe Doc
		if i < len(probes) {
			probe = probes[i]
		}
		films := filmsOf(rec, false, probe)
		codecs := map[string]bool{}
		for _, film := range films {
			if c := norm(str(film, "codec")); c != "" && c != "unknown" {
				codecs[c] = true
			}
		}
		if probe != nil {
			for _, film := range filmsOf(probe, true, nil) {
				if c := norm(str(film, "codec_name")); c != "" && c != "unknown" {
					codecs[c] = true
				}
			}
		}
		if len(codecs) == 0 {
			return CaptionsUnscanned + ": neither its record nor its stored probe states the codec of its picture, " +
				"which may be one that carries them where a read of windows may not see them (" +
				strings.Join(ScannedCodecs, ", ") + ")"
		}
		for _, c := range ScannedCodecs {
			if !codecs[c] {
				continue
			}
			read := false
			for _, film := range films {
				if v, has := film.Get("captionSignature"); has && v == false {
					read = true
				}
			}
			if !read {
				return CaptionsUnscanned + ": " + c + " carries them where a read of windows may not see them, and " +
					"its record says of no read of every packet of its picture that it found none (captionSignature)"
			}
		}
	}
	return ""
}

// captionStreamOf says what stream of closed captions of their own the
// records or the probes list (CaptionStreamCodecs, CaptionStreamTags: an
// EIA-608 stream, one tagged c608 or c708), "" when none.
func captionStreamOf(records, probes []Doc) string {
	for i, rec := range records {
		var probe Doc
		if i < len(probes) {
			probe = probes[i]
		}
		if _, _, cs := closedOf(viewsOf(rec, false, probe)); len(cs) > 0 {
			return "its record lists a stream of them, " + saidOr(cs[0].codec, cs[0].tag)
		}
	}
	for _, probe := range probes {
		if probe == nil {
			continue
		}
		if _, _, cs := closedOf(viewsOf(probe, true, nil)); len(cs) > 0 {
			return "its probe lists a stream of them, " + saidOr(cs[0].codec, cs[0].tag)
		}
	}
	return ""
}

// RecordedDurationMs is the length the source record in the folder dir
// says (container.durationMs, milliseconds), nil where it says none or
// can't be read.
func RecordedDurationMs(dir string) *int64 {
	rec, err := readRecordDoc(filepath.Join(dir, "source.json"))
	if err != nil {
		return nil
	}
	ms, ok := RecordedLengthMs(rec)
	if !ok {
		return nil
	}
	n := int64(math.Round(ms))
	return &n
}

// FindingLengthMismatch is the finding of a read whose length is not its
// record's (container.durationMs, by more than DurationSlack): a finding
// about the record or the file, final for that file and that record.
const FindingLengthMismatch = "lengthMismatch"

// CaptionsLengthMismatch begins why the gate keeps an original whose read
// found its length not its record's: kept for good, as one whose captions
// are not known, until its file or its record is another; no read of the
// same lets it go.
const CaptionsLengthMismatch = "its closed captions aren't known: its length is not its record's"

// DurationSlack is how far a read's length may lie from its record's
// (container.durationMs) and be the same video's: 2 s, or 1 % of it,
// whichever is more (a container's length and its last frame's differ by a
// frame or an audio tail).
func DurationSlack(recorded float64) float64 { return math.Max(2, recorded/100) }

// durationMismatch says how the read r's length is not its record's, ""
// when it is (a read or a record that says none is held elsewhere:
// ReadRefused, PictureSpan).
func durationMismatch(records []Doc, r *CaptionsRead) string {
	if r.Duration == nil {
		return ""
	}
	for _, rec := range records {
		ms, ok := RecordedLengthMs(rec)
		if !ok {
			continue
		}
		recorded := ms / 1000
		if math.Abs(*r.Duration-recorded) > DurationSlack(recorded) {
			return fmt.Sprintf("a read of its original says it lasts %g s, and its record %g s: the read is of "+
				"another file than the one recorded", *r.Duration, recorded)
		}
	}
	return ""
}

// TooShortForFive is why a read whose windows overlap says nothing of a
// video too short for its planned windows to lie apart (tooShortForFive).
const TooShortForFive = "windows overlap: the video is too short for five"

// tooShortForFive reports whether the picture of the span s is too short
// for the read r's planned windows to lie apart: two of them lie closer than
// a window reaches - its own span, and how far before its place its first
// frame lies (the keyframe it was read from) - as the read's windows show
// them.
func tooShortForFive(r *CaptionsRead, s Span) bool {
	d := s.Length()
	if d <= 0 {
		return false
	}
	reach := 0.0
	for _, w := range r.Windows {
		if w.Frames <= 0 {
			continue
		}
		// a window read from a keyframe before its place reaches back as
		// far as a keyframe may lie (one further back is out of place, not
		// a reach)
		back := math.Min(math.Max(s.Place(w.At)-w.Start, 0), WindowKeyframeSlack)
		reach = math.Max(reach, w.End-w.Start+back)
	}
	var places []float64
	for _, at := range planAts() {
		places = append(places, s.Place(at))
	}
	sort.Float64s(places)
	for i := 1; i < len(places); i++ {
		if places[i]-places[i-1] < reach {
			return true
		}
	}
	return false
}

// windowsOutOfPlace says how the windows ws that decoded frames don't lie
// in the order of the video, "" when they do: by where they were placed
// (at), each starts after the one before it and after its end, none places
// twice; overlap says two of them cover a time both.
func windowsOutOfPlace(ws []CaptionWindow) (why string, overlap bool) {
	decoded := slices.DeleteFunc(slices.Clone(ws), func(w CaptionWindow) bool { return w.Frames <= 0 })
	sort.SliceStable(decoded, func(i, j int) bool { return decoded[i].At < decoded[j].At })
	seen := map[string]bool{}
	for _, w := range ws {
		if w.At < 0 {
			continue // a window that says no place: no planned window
		}
		if p := percent(w.At); seen[p] {
			return "windows at " + p + " repeat", false
		} else {
			seen[p] = true
		}
	}
	for i := 1; i < len(decoded); i++ {
		a, b := decoded[i-1], decoded[i]
		on := b.Start < a.End && a.Start < b.End
		switch {
		case b.Start <= a.Start:
			return "windows at " + percents([]float64{a.At, b.At}) + " start in the wrong order", on
		case on:
			return "windows at " + percents([]float64{a.At, b.At}) + " lie on one another", true
		}
	}
	return "", false
}

// missingWindows are the plan's windows (planAts) the read r has none at,
// failed or not.
func missingWindows(r *CaptionsRead) []float64 {
	var out []float64
	for _, at := range planAts() {
		if !slices.ContainsFunc(r.Windows, func(w CaptionWindow) bool { return sameAt(w.At, at) }) {
			out = append(out, at)
		}
	}
	return out
}

// offPlan are the places (at) of the windows of ws that say a place at no
// fraction of the plan's (planAts), such as one the tail short of the end
// that says where it lies (0.88) rather than its fraction (0.9).
func offPlan(ws []CaptionWindow) []float64 {
	var out []float64
	for _, w := range ws {
		if w.At >= 0 && !slices.ContainsFunc(planAts(), func(at float64) bool { return sameAt(w.At, at) }) {
			out = append(out, w.At)
		}
	}
	return out
}

// sameAt reports whether the window a read placed at at is the plan's
// window at planned: the same fraction, within atSlack.
func sameAt(at, planned float64) bool { return math.Abs(at-planned) < atSlack }

// readReleases are the beginnings of the whys a read of an original's
// frames may let go of: its closed captions not known, its fields not told
// by its frames, and its captions not told by windows alone (a hold of an
// HEVC original, which its record's captionSignature, once a rewritten
// record carries it, lets go: the record rewritten has its original read
// again).
var readReleases = []string{CaptionsUnknown, FramesUnknown, CaptionsUnscanned}

// FramesUnknown begins why the gate keeps an original a read of whose frames
// says nothing of interlaced or repeated frames.
const FramesUnknown = "its frames' fields aren't known"

// MarkCaptionsWaiting notes, of the original s, whether a retirement waits on
// its closed captions alone (Gate.OnlyCaptionsUnknown, and nothing else of
// the retire job keeps it): since when it does, from the first pass that
// found it so, or that it doesn't. The captions backfill hands these
// originals out first; the note decides nothing. Without migration 059 it
// notes nothing.
func MarkCaptionsWaiting(ctx context.Context, q Querier, s *Source, waiting bool) error {
	if ok, err := relationReady(ctx, q, store.CaptionsWaitingIndex); err != nil || !ok {
		return err
	}
	if !waiting {
		_, err := q.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET retirewaitingsince = NULL
			WHERE source_id = $1 AND retirewaitingsince IS NOT NULL`, s.ID)
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO `+store.SourceCaptionsTable+` (source_id, item_id, state, retirewaitingsince)
		VALUES ($1, $2, 'pending', now())
		ON CONFLICT (source_id) DO UPDATE SET retirewaitingsince = COALESCE(`+store.SourceCaptionsTable+`.retirewaitingsince,
			EXCLUDED.retirewaitingsince)`, s.ID, s.ItemID)
	return err
}

// unshownNotWaiting takes the note that its retirement waits on its captions
// alone (MarkCaptionsWaiting) off an original whose chapters or picture area
// aren't known now, which keep it too; the job, which looks at such an
// original no more until they are, notes it again then.
func unshownNotWaiting(ctx context.Context, q Querier) error {
	if ok, err := relationReady(ctx, q, store.CaptionsWaitingIndex); err != nil || !ok {
		return err
	}
	if ready, err := unshownReady(ctx, q); err != nil || !ready {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` c SET retirewaitingsince = NULL
		FROM com_nalet_katalog_itemsources s
		WHERE c.retirewaitingsince IS NOT NULL AND s.id = c.source_id
		  AND NOT (`+chaptersSettled("s.item_id", "s.id")+` AND `+pictureAreaSettled("s.item_id", "s.id")+`)`)
	return err
}

// ReleaseCaptionsHold lets go of a retirement of the original source of
// the title item the retire job held for want of its closed captions, in q
// (the transaction that keeps what a read of them found, the item locked):
// the source's error goes, and the title's retire step held for its version
// with it, so that the job looks at it again. A source not held so releases
// nothing.
func ReleaseCaptionsHold(ctx context.Context, q Querier, item, source string) error {
	like := make([]string, len(readReleases))
	for i, p := range readReleases {
		like[i] = p + "%"
	}
	tag, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET error = NULL, modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND error LIKE ANY ($3)`, source, item, like)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = q.Exec(ctx, `DELETE FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = 'retire'
		AND status = 'failed' AND COALESCE(details, '') LIKE '`+heldFor+`%' AND error LIKE ANY ($2)`, item, like)
	return err
}
