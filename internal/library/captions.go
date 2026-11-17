package library

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

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
	// pictures), and may lie up to WindowKeyframeSlack before it.
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
	// decoded is how many frames it decoded as its read reported them, of a
	// window that failed afterwards (Frames 0)
	decoded int
}

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
		if _, seen := m[f.key]; seen && !slices.Contains(repeated, f.key) {
			repeated = append(repeated, f.key)
		}
		m[f.key] = f.value
		all[f.key] = append(all[f.key], f.value)
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
			w.InterlacedFrames, w.RepeatFrames = most("interlacedFrames", frames), most("repeatFrames", frames)
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
	if why, ok := m["unreadable"]; ok {
		// a window the catalog kept as one it could not take, with what it
		// found
		var s string
		if json.Unmarshal(why, &s) != nil || strings.TrimSpace(s) == "" {
			s = "it could not be read"
		}
		w.CountsUnreadable = strings.TrimSpace(string(m["countsUnreadable"])) == "true"
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
	if w.At < 0 {
		if !ok || frames < 0 {
			frames = 0
		}
		return fail("its at is no fraction of the video from 0 to 1", frames)
	}
	if !ok || frames < 0 {
		return fail("its frames is no whole number of frames, 0 or more", 0)
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
		if isNull(m[k]) {
			continue
		}
		n, ok := whole(m[k])
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
	if !isNull(m["keyframeBefore"]) {
		kb, ok := boolean(m["keyframeBefore"])
		if !ok {
			return fail("its keyframeBefore is no boolean", frames)
		}
		w.KeyframeBefore = kb
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
}

// SourceCaptions is what a read that decoded frames found of the closed
// captions of the source sourceID, window by window, of the file the source
// is now: a read of a file of another size, or quick hash where both say
// one, than the source's (one the scanner's Refix or an adoption put in its
// place since) is none.
// Nil when there is none (or the catalog has no migration 058); failed says
// why the last read of it was not taken, "" when there was none, and finds
// what that read found of the file before it stopped (its streams of
// captions and what is off the closed lists; no windows), nil when nothing.
func SourceCaptions(ctx context.Context, q Querier, sourceID string) (read *CaptionsRead, failed string,
	finds *CaptionsRead, err error) {
	if ok, err := relationReady(ctx, q, store.SourceCaptionsTable); err != nil || !ok {
		return nil, "", nil, err
	}
	var state string
	var windows, offList []byte
	var duration *float64
	var streams *int
	var reason *string
	var same bool
	var finding *string
	err = q.QueryRow(ctx, `SELECT c.state, c.captionwindows, c.duration, c.captionstreams, c.offlist, c.reason,
			c.sourcesize IS NOT DISTINCT FROM s.sizebytes
			AND (c.sourceqh1 IS NULL OR s.qh1 IS NULL OR c.sourceqh1 = s.qh1)
			AND (c.finding IS NULL OR c.sourcerecordedat IS NOT DISTINCT FROM s.recordedat), c.finding
		FROM `+store.SourceCaptionsTable+` c JOIN com_nalet_katalog_itemsources s ON s.id = c.source_id
		WHERE c.source_id = $1`, sourceID).Scan(&state, &windows, &duration, &streams, &offList, &reason, &same, &finding)
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
		return nil, "the file read is not the one the original is now", nil, nil
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
	r := CaptionsRead{Duration: duration, CaptionStreams: streams, OffList: off}
	if err := json.Unmarshal(windows, &r.Windows); err != nil {
		return nil, "", nil, fmt.Errorf("closed captions' windows: %w", err)
	}
	return &r, "", nil, nil
}

// MinWindowFrames is how many frames a window decodes at least to be one:
// half of the packets a read decodes of it (WindowPackets).
const MinWindowFrames = 24

// WindowPlaceSlack is how far from its place (the plan's, of the picture's
// span as its records give it: PictureSpan) a window's first frame may lie:
// a keyframe before it.
const WindowPlaceSlack = 15.0

// WindowKeyframeSlack is how far before its place a window's first frame may
// lie where the read says it is the keyframe at or before the place
// (KeyframeBefore): a group of pictures that long. A window is in place by
// where it starts, the keyframe its seek lands on, not by its distance.
const WindowKeyframeSlack = 30.0

// ReadRefused says why the read r of the picture of the span s (its records',
// PictureSpan) says nothing of it, "" when it may be taken: its windows
// don't lie in the order of the video (a video too short for them said as
// that, TooShortForFive), it says no length of its own (which its record's
// is held to), it lacks a window of the plan (the start among them: a read
// of five windows and none at the start is not complete), a window lies
// further than WindowPlaceSlack from its place, the start + at × (end −
// start) of the span (up to WindowKeyframeSlack before it, where the read
// says it starts at the keyframe at or before it: KeyframeBefore), or one
// decoded fewer than MinWindowFrames frames (a window that decoded none
// failed, which a read may have). The read's own length places nothing. The
// catalog keeps such a read as a failed one, and the gate holds every read
// to it.
func ReadRefused(r *CaptionsRead, s Span) string {
	if why, overlap := windowsOutOfPlace(r.Windows); why != "" {
		if overlap && tooShortForFive(r, s) {
			return TooShortForFive
		}
		return why
	}
	if r.Duration == nil || *r.Duration <= 0 {
		return "it says no length of the video, which its record's is held to"
	}
	if missing := missingWindows(r, s); len(missing) > 0 {
		return "it has no window at " + percents(missing) + " (a read is of its start and 10 to 90 % of its picture's " +
			"span, or as many as its length allows)"
	}
	var far, few []float64
	for _, w := range r.Windows {
		switch {
		case w.Frames <= 0:
		case w.Frames < MinWindowFrames:
			few = append(few, w.At)
		case !inPlace(w, s.place(w.At)):
			far = append(far, w.At)
		}
	}
	switch {
	case len(far) > 0:
		return fmt.Sprintf("its windows at %s lie more than %g s from their places (the start, and 10 to 90 %% of its "+
			"picture's span from %g s to %g s, as its records give it)", percents(far), WindowPlaceSlack, s.Start, s.End)
	case len(few) > 0:
		return fmt.Sprintf("its windows at %s decoded fewer than %d frames", percents(few), MinWindowFrames)
	}
	return ""
}

// inPlace reports whether the window w lies at its place place (seconds):
// within WindowPlaceSlack of it, or, where the read says it starts at the
// keyframe at or before it (KeyframeBefore), up to WindowKeyframeSlack
// before it.
func inPlace(w CaptionWindow, place float64) bool {
	back := place - w.Start
	return math.Abs(back) <= WindowPlaceSlack || (w.KeyframeBefore && back >= 0 && back <= WindowKeyframeSlack)
}

// The window plan, one for every place that reads or checks a read's
// windows: a window at the picture's start (WindowAtStart), and one at each
// of PlannedWindows of the picture's own presentation span (Span: start +
// at × (end − start)), each at most WindowTail short of its end - six
// windows, every one of which a read has before it can say "none". A video
// shorter than twice the tail has them at most half its span in (the ones
// that fall on one another are one). A window decodes WindowPackets packets
// and is one when at least MinWindowFrames of them gave a frame. The span is
// the picture's own: a picture that starts late, or ends before the file
// does, is read where it is (PictureSpan). These are constants of the code;
// the plan's reference file is read by a test alone, never at run time.

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

// plannedAts are the windows of the plan of a picture whose span lasts
// length seconds, as fractions of it: the start (0), and PlannedWindows each
// at most WindowTail, or half the span, short of its end, those that fall on
// one another one.
func plannedAts(length float64) []float64 {
	limit := 1.0
	if length > 0 {
		limit = 1 - math.Min(WindowTail/length, 0.5)
	}
	out := []float64{0}
	for _, f := range PlannedWindows {
		at := math.Min(f, limit)
		if !slices.ContainsFunc(out, func(a float64) bool { return math.Abs(a-at) < atSlack }) {
			out = append(out, at)
		}
	}
	return out
}

// Span is the presentation span of an original's picture, in seconds of its
// own clock: from its first picture (Start) to its end (End).
type Span struct{ Start, End float64 }

// Length is how long the span s lasts.
func (s Span) Length() float64 { return s.End - s.Start }

// place is where in the picture of the span s the window at at (a fraction
// of it) lies, in seconds.
func (s Span) place(at float64) float64 { return s.Start + at*s.Length() }

// CaptionsNoLength begins why the gate keeps an original whose record says
// no length (container.durationMs): where a read's windows lie can't be told
// without one, so no read of it says anything until its record says one.
const CaptionsNoLength = CaptionsUnknown + ": its record says no length"

// FindingNoLength is the finding of a read of an original whose record said
// no length when it was handed out: final for that file and that record,
// like FindingLengthMismatch.
const FindingNoLength = "noRecordLength"

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

// PictureSpan is the presentation span of the picture of the original whose
// source record is rec and stored probe probe (ffprobe.json; nil: none), as
// its records give it, or why there is none. It is the part of the record
// logic's picture_span (libv2_records.py) the catalog can tell without
// opening the file: the start is the stored probe's picture's start_time,
// else the probe's format's, else 0; the length the probe's picture's
// duration, else the record's (container.durationMs); the end the start plus
// the length. (The record logic reads the end of the picture's last packet
// from the file as well, which no record keeps.) A record that says no
// length has no span, whatever its probe says.
func PictureSpan(rec, probe Doc) (*Span, string) {
	ms, ok := RecordedLengthMs(rec)
	if !ok {
		return nil, "its record says no length of its file (container.durationMs), so where a read's windows lie " +
			"can't be told"
	}
	start, length := 0.0, ms/1000
	if probe != nil {
		f, _ := probe.Get("format")
		fd, _ := f.(Doc)
		if fd != nil {
			if v, ok := seconds(valueOf(fd.Get("start_time"))); ok {
				start = v
			}
		}
		if films := filmsOf(probe, true, nil); len(films) == 1 {
			if v, ok := seconds(valueOf(films[0].Get("start_time"))); ok {
				start = v
			}
			if v, ok := seconds(valueOf(films[0].Get("duration"))); ok && v > 0 {
				length = v
			}
		}
	}
	return &Span{Start: start, End: start + length}, ""
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
			if v, _ := film.Get("captionSignature"); v == true {
				recordSays = "its record's read of every packet of its picture found a caption signature"
			}
			switch v, has := film.Get("closedCaptions"); {
			case !has || v == nil || v == false:
			case v == true:
				recordSays = "its record says so of its picture"
			default:
				flagUnread = "its record's closedCaptions of its picture is no boolean"
			}
		}
		if probe != nil {
			for _, film := range filmsOf(probe, true, nil) {
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
	if why := unscanned(records, probes); why != "" {
		return "", why
	}
	var probe Doc
	if len(probes) > 0 {
		probe = probes[0]
	}
	span, why := PictureSpan(records[0], probe)
	if span == nil {
		return "", CaptionsUnknown + ": " + why
	}
	if fd := f.CaptionsFinds; r == nil && fd != nil {
		// final for this file and this record: no read again changes it
		switch fd.Finding {
		case FindingLengthMismatch:
			return "", CaptionsLengthMismatch + ": " + strings.TrimSpace(f.CaptionsFailed)
		case FindingNoLength:
			return "", CaptionsNoLength + " (as it said when its original was handed out to be read): " +
				strings.TrimSpace(f.CaptionsFailed)
		}
	}
	switch {
	case r == nil && f.CaptionsFailed != "":
		return "", CaptionsUnknown + ": its last read was not taken: " + f.CaptionsFailed + "; the captions backfill " +
			"reads it again (captions.backfill)"
	case r == nil:
		return "", captionsUnknownWhy
	}
	if why := ReadRefused(r, *span); why == TooShortForFive {
		// no title with captions: a video whose planned windows can't but
		// fall on one another
		return "", CaptionsUnknown + ": " + TooShortForFive
	} else if why != "" {
		return "", CaptionsUnknown + ": a read of its original says nothing of the video: " + why + "; the captions " +
			"backfill reads it again (captions.backfill)"
	}
	if why := durationMismatch(records, r); why != "" {
		return "", CaptionsLengthMismatch + ": " + why
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
		back := math.Min(math.Max(s.place(w.At)-w.Start, 0), WindowKeyframeSlack)
		reach = math.Max(reach, w.End-w.Start+back)
	}
	ats := plannedAts(d)
	for i := 1; i < len(ats); i++ {
		if (ats[i]-ats[i-1])*d < reach {
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

// missingWindows are the planned windows of a picture of the span s
// (plannedAts) the read r has none at, failed or not.
func missingWindows(r *CaptionsRead, s Span) []float64 {
	l := s.Length()
	var out []float64
	for _, at := range plannedAts(l) {
		if !slices.ContainsFunc(r.Windows, func(w CaptionWindow) bool { return sameAt(w.At, at, l) }) {
			out = append(out, at)
		}
	}
	return out
}

// sameAt reports whether the window a read placed at at (a fraction of the
// length it read) is the plan's window at planned of a picture whose span
// lasts length seconds: the same fraction, or one a length that differs
// within DurationSlack moves it to (a window the tail short of the end is
// placed by the length: 88 % of 250 s, 88.1 % of 252.4 s).
func sameAt(at, planned, length float64) bool {
	d := math.Abs(at - planned)
	return d < atSlack || d*length <= DurationSlack(length)
}

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
