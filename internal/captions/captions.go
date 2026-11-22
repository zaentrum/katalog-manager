package captions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The states of an original's read: pending while it is handed out and
// none came back, read, or failed.
const (
	StatePending = "pending" // handed out, no result yet
	StateRead    = "read"    // what the read found is kept
	StateFailed  = "failed"  // why it failed; handed out again after a backoff
)

// ErrNotReady is the answer of a catalog without migration 058.
var ErrNotReady = errors.New("the closed captions' migration (db/migrations/058_source_captions.sql) is not applied")

// ErrNoSource is the answer for an original the catalog has no source of.
var ErrNoSource = errors.New("no such original")

// ErrNotTheClaim is the answer for a report whose token is neither the
// claim's the original is under nor the one whose lease ended last: a
// report of a run the catalog handed it out to, or of none.
var ErrNotTheClaim = errors.New("the report's token is not the claim's the original was handed out with")

// BadReport is a report that can't be taken, and why.
type BadReport struct{ Why string }

func (e *BadReport) Error() string { return e.Why }

// Read is what the catalog keeps of an original's closed captions.
type Read struct {
	SourceID, ItemID, State  string
	Windows                  []byte
	Captions                 *bool
	FFprobeVersion           *string
	FramesDecoded            *int32
	Duration                 *float64
	Reason                   *string
	ReadAt                   *time.Time
	Attempts, Failures       int32
	NextAttemptAt, ClaimedAt *time.Time
	ClaimToken, LapsedToken  *string
	// ClaimSize and ClaimQH1 are of the file the claim handed out,
	// SourceSize and SourceQH1 of the file the kept read is of
	ClaimSize, SourceSize *int64
	ClaimQH1, SourceQH1   *string
	// ClaimDurationMs is the length the record of the file handed out says
	// (its container.durationMs), which the claim handed out with it, and
	// ClaimSpanStart and ClaimSpanEnd the span of its picture its records
	// gave (library.PictureSpan), which the read's windows are placed by;
	// none where the record said no length.
	ClaimDurationMs                             *int64
	ClaimSpanStart, ClaimSpanEnd, ClaimSpanStep *float64
	// ReadSpanStart and ReadSpanEnd are the picture's span the kept read
	// placed its windows over, as the read said it (seconds of the
	// picture's clock), and ReadGop the longest run between two keyframes of
	// its picture's first packets it found (seconds; nil: none said, which
	// places windows as 0 does).
	ReadSpanStart, ReadSpanEnd, ReadGop *float64
	// Finding is what a read found that keeps the original for good of its
	// file and its record (library.FindingLengthMismatch), nil none.
	Finding *string
}

const readColumns = `source_id, item_id, state, captionwindows, captions, ffprobeversion, framesdecoded, duration, reason,
	readat, attempts, failures, nextattemptat, claimedat, claimtoken, lapsedtoken, claimsize, sourcesize, claimqh1, sourceqh1,
	claimdurationms, finding, claimspanstart, claimspanend, claimspanstep, readspanstart, readspanend, readgop`

func scanRead(row pgx.Row) (*Read, error) {
	var r Read
	err := row.Scan(&r.SourceID, &r.ItemID, &r.State, &r.Windows, &r.Captions, &r.FFprobeVersion, &r.FramesDecoded,
		&r.Duration, &r.Reason, &r.ReadAt, &r.Attempts, &r.Failures, &r.NextAttemptAt, &r.ClaimedAt, &r.ClaimToken,
		&r.LapsedToken, &r.ClaimSize, &r.SourceSize, &r.ClaimQH1, &r.SourceQH1, &r.ClaimDurationMs, &r.Finding,
		&r.ClaimSpanStart, &r.ClaimSpanEnd, &r.ClaimSpanStep, &r.ReadSpanStart, &r.ReadSpanEnd, &r.ReadGop)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// sameFile reports whether the kept read is of the file the claim handed
// out: of its size, and of its quick hash where both are known.
func (r *Read) sameFile() bool {
	return r.SourceSize != nil && r.ClaimSize != nil && *r.SourceSize == *r.ClaimSize &&
		(r.SourceQH1 == nil || r.ClaimQH1 == nil || *r.SourceQH1 == *r.ClaimQH1)
}

// found reports whether the kept read found what keeps the original for
// good: captions, or a window that counted frames in fields.
func (r *Read) found() bool {
	if r.State != StateRead || r.Windows == nil {
		return false
	}
	if r.Captions != nil && *r.Captions {
		return true
	}
	var ws []library.CaptionWindow
	if json.Unmarshal(r.Windows, &ws) != nil {
		return false
	}
	for _, w := range ws {
		if w.Found() {
			return true
		}
	}
	return false
}

// Of reads what the catalog keeps of the closed captions of the original
// sourceID; nil when nothing.
func Of(ctx context.Context, q library.Querier, sourceID string) (*Read, error) {
	return scanRead(q.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.SourceCaptionsTable+` WHERE source_id = $1`,
		sourceID))
}

// Report is what the analyzer reports of a claim of an original: what it
// read of it (Original). Released gives an original the backfill handed out
// back unread (its run was stopped): nothing else is read. Token is the
// claim's the run read for (Job.Token). A report's package part, of a read
// of the original's package an older analyzer made, is no part of it: the
// gate asks the original alone, and the catalog takes no notice of one.
type Report struct {
	Token    string      `json:"token"`
	Original *FileReport `json:"original"`
	Released bool        `json:"released"`
}

// RepeatedKey is a key an object of the report b gives twice, "" when none
// does (or b is no JSON a report is read from, which its reading says): a
// report that says a thing twice says it neither way. Its windows are read
// on their own (library.CaptionWindow: a window that gives a key twice is a
// failed one), so a key repeated inside one is not this one's.
func RepeatedKey(b []byte) string {
	d, err := library.DecodeDoc(b)
	if err != nil {
		return ""
	}
	return repeated(d, "")
}

// repeated is a key an object of the value v, at the path path of the
// report, gives twice, its windows aside: in any case, as encoding/json
// takes "Token" for "token".
func repeated(v any, path string) string {
	switch x := v.(type) {
	case library.Doc:
		seen := make(map[string]bool, len(x))
		for _, f := range x {
			k := strings.ToLower(f.Key)
			if seen[k] {
				return f.Key
			}
			seen[k] = true
			if path+"/"+k == "/original/captionwindows" {
				continue
			}
			if r := repeated(f.Value, path+"/"+k); r != "" {
				return r
			}
		}
	case []any:
		for _, e := range x {
			if k := repeated(e, path+"/[]"); k != "" {
				return k
			}
		}
	}
	return ""
}

// FileReport is what a read of the original found, window by window
// (CaptionWindows: where in its picture's span each is, the presentation
// seconds it covered, the frames it decoded and whether they showed closed
// captions), or why it could not read the file (Error); the ffprobe that
// read it (FFprobe, its version as `ffprobe -version` names it, its first
// line or the version alone: 7.1); and the length of the video it read
// (Duration, seconds; none when it read no length), which its record's is
// held to. The windows are held to the plan of the span of the original's
// picture its records give (library.PictureSpan), never to the read's own
// length. A window that decoded no frame failed: it says nothing of its
// captions, and a read none of whose windows decoded a frame is a failed
// one.
//
// SizeBytes is the size of the file it read, as it found it: a read of
// another file than the one the claim handed out (one copied in its place,
// or still being copied) is no read of the original. CaptionStreams is how
// many streams of closed captions of their own it found (an EIA-608 track,
// one tagged c608 or c708), OffList what of the file is off the analyzer's
// closed list ("codec:vc1"); each window says how many of its frames are
// interlaced and repeat a field (interlacedFrames, repeatFrames).
type FileReport struct {
	CaptionWindows []library.CaptionWindow `json:"captionWindows"`
	Duration       *float64                `json:"duration"`
	// Span is the picture's span the read placed its windows over (seconds
	// of the picture's clock: its first frame, start, to the end of its last
	// video packet, end), each end of which the span of the picture its
	// records give is held to.
	Span *SpanReport `json:"span"`
	// Gop is the longest run between two keyframes among the picture's first
	// packets the read listed (seconds, 0 or more; 0 or none said: fewer than
	// two keyframes), which sets how far from its place a window may lie.
	Gop *float64 `json:"gop"`
	// CaptionsKnown false says the read can't tell its picture's captions,
	// Reason why: one whose packets need to be read whole for a caption
	// signature (library.UnscannedReason) is kept as the finding unscanned.
	CaptionsKnown  *bool    `json:"captionsKnown"`
	Reason         string   `json:"reason,omitempty"`
	SizeBytes      *int64   `json:"sizeBytes"`
	CaptionStreams *int     `json:"captionStreams"`
	OffList        []string `json:"offList"`
	FFprobe        string   `json:"ffprobe"`
	Error          string   `json:"error,omitempty"`
	// raw is the report as the analyzer sent it, which the catalog keeps
	// with a read (readreport) for the contract's deletion gate to read as
	// the record logic does (library.RecordLogicHolds)
	raw []byte
}

// UnmarshalJSON reads a report of the original, keeping its text as sent.
func (f *FileReport) UnmarshalJSON(b []byte) error {
	type plain FileReport
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*f = FileReport(p)
	f.raw = append([]byte(nil), b...)
	return nil
}

// SpanReport is the span of its picture a read says: the seconds of its
// first frame (Start) and of the end of its last video packet (End).
type SpanReport struct {
	Start *float64 `json:"start"`
	End   *float64 `json:"end"`
}

// reportText is a report's text to keep (readreport), nil of none.
func reportText(b []byte) *string {
	if b == nil {
		return nil
	}
	t := string(b)
	return &t
}

// spanEnd is the end (end true) or the start of the span s, nil of none.
func spanEnd(s *library.Span, end bool) *float64 {
	switch {
	case s == nil:
		return nil
	case end:
		return &s.End
	}
	return &s.Start
}

// FFprobeTested is the one release of ffprobe whose reads the catalog
// takes: 7.1, the one the captions' reads were tested with, which exports
// the A53 closed captions' side data of the frames it decodes and says a
// stream's closed_captions of them (a later one, 9.0, says none of the
// latter). Another is taken once it is tested.
var FFprobeTested = [2]int{7, 1}

// ffprobeRE reads a release's major and minor of what `ffprobe -version`
// names: "7.1", "7.1.1", "n7.1.1", "7.1.1-static", "7.1.1-1+b1",
// "n7.1.5-12-g1fdbca85aa-20260731" (a build of a release's branch), its
// whole first line, "ffprobe version 7.1.1 Copyright ...", or the
// analyzer's "7.1 by its libraries (...)".
var ffprobeRE = regexp.MustCompile(`^(?i:ffprobe\s+version\s+)?n?(\d+)\.(\d+)(?:\.\d+)*(?:[-+~_ ].*)?$`)

// FFprobeRelease is the release (major, minor) the version v names, or why
// it names none (a build of the development branch, "N-..." or a git hash,
// names none).
func FFprobeRelease(v string) ([2]int, error) {
	m := ffprobeRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [2]int{}, fmt.Errorf("%q names no release of ffprobe (as 7.1.1)", v)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return [2]int{major, minor}, nil
}

// outcome is what a report keeps: a read (its windows, whether any showed
// captions, the frames they decoded, whether a window failed, the length it
// says) or a failure (why), with the ffprobe; found says a window that
// decoded frames showed captions or counted frames in fields, which keeps
// the original whatever else of the read failed.
type outcome struct {
	read      bool
	found     bool
	unscanned bool
	span      *library.Span
	gop       *float64
	ws        []library.CaptionWindow
	windows   []byte
	captions  bool
	frames    int32
	partial   bool
	duration  *float64
	size      *int64
	streams   *int
	offList   []byte
	version   string
	why       string
	report    []byte // the report as sent (FileReport.raw)
}

// versionWord is the version alone of what ffprobe names: of its first
// line, the word after "ffprobe version".
func versionWord(v string) string {
	if f := strings.Fields(v); len(f) >= 3 && strings.EqualFold(f[0], "ffprobe") && strings.EqualFold(f[1], "version") {
		return f[2]
	}
	return v
}

// noFrame is why a read none of whose windows decoded a video frame failed.
const noFrame = "no window of the read decoded a video frame, so it can't say whether there are closed captions"

// maxWindows is how many windows a read has at most.
const maxWindows = 64

// finite reports whether every one of vs is a number.
func finite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// check is what the report of the original keeps, or why it can't be taken.
func (f *FileReport) check() (outcome, error) {
	const what = "original"
	v := strings.TrimSpace(f.FFprobe)
	if v == "" {
		return outcome{}, &BadReport{fmt.Sprintf("%s: a report names the ffprobe that read it (ffprobe, %d.%d)", what,
			FFprobeTested[0], FFprobeTested[1])}
	}
	rel, err := FFprobeRelease(v)
	if err != nil {
		return outcome{}, &BadReport{fmt.Sprintf("%s: ffprobe %v: the catalog takes reads of ffprobe %d.%d", what, err,
			FFprobeTested[0], FFprobeTested[1])}
	}
	if rel != FFprobeTested {
		return outcome{}, &BadReport{fmt.Sprintf("%s: ffprobe %s is not %d.%d, the release the catalog takes reads of "+
			"(the one tested)", what, v, FFprobeTested[0], FFprobeTested[1])}
	}
	o := outcome{version: clip(versionWord(v), 100), report: f.raw}
	if f.CaptionStreams != nil && *f.CaptionStreams < 0 {
		return outcome{}, &BadReport{what + ": captionStreams is how many streams of closed captions it found, 0 or more"}
	}
	if f.SizeBytes != nil && *f.SizeBytes < 0 {
		return outcome{}, &BadReport{what + ": sizeBytes is the size of the file it read, 0 or more"}
	}
	// what it found of the file, a failure too (once it listed the file's
	// streams: its streams of captions, what is off the closed lists)
	o.size, o.streams = f.SizeBytes, f.CaptionStreams
	if len(f.OffList) > 0 {
		if o.offList, err = json.Marshal(f.OffList); err != nil {
			return outcome{}, err
		}
	}
	if len(f.CaptionWindows) > maxWindows {
		return outcome{}, &BadReport{fmt.Sprintf("%s: a read has %d windows at most", what, maxWindows)}
	}
	// what its windows found, a failed read's too: a window that decoded
	// frames and showed captions or counted frames in fields keeps the
	// original (the worst a read says is kept)
	var failed []float64
	var unread []string
	for i, w := range f.CaptionWindows {
		if w.Stored() {
			return outcome{}, &BadReport{fmt.Sprintf("%s: captionWindows[%d] says unreadable, the catalog's own key of a "+
				"window it kept as failed: a read doesn't say it", what, i)}
		}
		if w.Unreadable != "" {
			unread = append(unread, fmt.Sprintf("captionWindows[%d]: %s", i, w.Unreadable))
		}
		o.frames += int32(min(w.Decoded(), 1<<20))
		o.captions = o.captions || w.Captions
		o.found = o.found || w.Found()
		if w.Frames == 0 {
			failed = append(failed, w.At)
		}
	}
	o.ws = f.CaptionWindows
	if len(f.CaptionWindows) > 0 {
		if o.windows, err = json.Marshal(f.CaptionWindows); err != nil {
			return outcome{}, err
		}
	}
	if sp := f.Span; sp != nil {
		if sp.Start == nil || sp.End == nil || !finite(*sp.Start) || !finite(*sp.End) || *sp.End <= *sp.Start {
			return outcome{}, &BadReport{what + ": span is the picture's span it read, {start, end}: the seconds of its " +
				"first frame and of the end of its last packet, end after start"}
		}
		o.span = &library.Span{Start: *sp.Start, End: *sp.End}
	}
	if g := f.Gop; g != nil && (!finite(*g) || *g < 0) {
		return outcome{}, &BadReport{what + ": gop is the longest run between two keyframes of the picture's first " +
			"packets, in seconds, 0 or more"}
	}
	o.gop = f.Gop
	if strings.TrimSpace(f.Error) != "" {
		o.why = clip(strings.TrimSpace(f.Error), 500)
		return o, nil
	}
	if f.CaptionsKnown != nil && !*f.CaptionsKnown {
		// a read that says it can't tell: of a picture whose packets need
		// to be read whole, the finding unscanned; else a failure
		reason := strings.TrimSpace(f.Reason)
		o.unscanned = strings.HasPrefix(reason, library.UnscannedReason)
		if reason == "" {
			reason = "the read says its picture's captions aren't known"
		}
		o.why = clip(reason, 500)
		return o, nil
	}
	if len(f.CaptionWindows) == 0 {
		return outcome{}, &BadReport{what + ": a read names the windows it decoded (captionWindows: [{at, start, end, " +
			"frames, captions}]), a failure why it could not read the file (error)"}
	}
	if d := f.Duration; d != nil && (!finite(*d) || *d <= 0) {
		return outcome{}, &BadReport{what + ": duration is the video's length in seconds, above 0"}
	}
	o.duration = f.Duration
	if o.frames == 0 {
		o.why = noFrame
		if len(unread) > 0 {
			o.why += "; " + strings.Join(unread, "; ")
		}
		return o, nil
	}
	if f.SizeBytes == nil {
		return outcome{}, &BadReport{what + ": a read says the size of the file it read (sizeBytes)"}
	}
	// a window that is no window (a value of another type, one out of its
	// range, a key given twice: library.CaptionWindow's reading) is a failed
	// one, kept and said, never left out; where its windows lie is held to
	// the claim's span when it is kept (Record)
	o.read, o.partial = true, len(failed) > 0
	if o.partial {
		names := make([]string, len(failed))
		for i, at := range failed {
			names[i] = windowName(at)
		}
		o.why = "its windows at " + strings.Join(names, ", ") + " decoded no frame"
	}
	if len(unread) > 0 {
		o.why = strings.TrimPrefix(o.why+"; "+strings.Join(unread, "; "), "; ")
	}
	return o, nil
}

// holdTo holds the read o to the record its claim handed out: the length
// it said (durationMs, milliseconds), within DurationSlack, and the span of
// its picture its records gave (spanStart, spanEnd, seconds), where the
// plan places its windows (library.ReadRefused). A record that said no
// length, records that gave no plan of its picture (library.PictureSpan),
// and a read of another length, are findings about the record or the file
// (noRecordLength, noPlan, lengthMismatch), which it answers; a read whose
// windows say nothing of the picture is a failed one. A read it refuses is
// no read (o.read false, o.why why).
func (o *outcome) holdTo(durationMs *int64, spanStart, spanEnd, spanStep *float64) (finding string) {
	if !o.read {
		return ""
	}
	switch {
	case durationMs == nil || *durationMs <= 0:
		o.read = false
		o.why = "its record said no length of its file (container.durationMs) when it was handed out, so where " +
			"the read's windows lie can't be told"
		return library.FindingNoLength
	case spanStart == nil || spanEnd == nil || spanStep == nil || !finite(*spanStart) || !finite(*spanEnd) ||
		!finite(*spanStep) || *spanStep <= 0:
		o.read = false
		o.why = "its records gave no plan of its picture when it was handed out (its stored probe's picture's " +
			"start, length and frame rate), so where the read's windows lie can't be told"
		return library.FindingNoPlan
	case o.duration != nil && math.Abs(*o.duration-float64(*durationMs)/1000) >
		library.DurationSlack(float64(*durationMs)/1000):
		recorded := float64(*durationMs) / 1000
		o.read = false
		o.why = fmt.Sprintf("a read of its original says it lasts %g s, and its record %g s (within %g s)",
			*o.duration, recorded, library.DurationSlack(recorded))
		return library.FindingLengthMismatch
	}
	span := library.Span{Start: *spanStart, End: *spanEnd, Step: *spanStep}
	if why := library.ReadRefused(&library.CaptionsRead{Windows: o.ws, Duration: o.duration, Span: o.span,
		Gop: o.gop}, span); why != "" {
		// a read that says nothing of its video is kept as a failed one,
		// that it is read again and counted not read (the gate holds every
		// read to the same)
		o.read = false
		o.why = strings.TrimSuffix(why+"; "+o.why, "; ")
	}
	return ""
}

// sizeSaid says the size n, "no" for none.
func sizeSaid(n *int64) string {
	if n == nil {
		return "no"
	}
	return strconv.FormatInt(*n, 10)
}

// windowName names the window at at as the gate does: "start", "50%".
func windowName(at float64) string {
	if at < 0 {
		return "a place it doesn't say"
	}
	if at < 1e-3 {
		return "start"
	}
	return strconv.FormatFloat(math.Round(at*1000)/10, 'f', -1, 64) + "%"
}

// Record keeps what the analyzer reported of the original sourceID (Record).
func (s *Service) Record(ctx context.Context, sourceID string, r Report) (*Read, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return Record(ctx, s.st, sourceID, r, s.now())
}

// Record keeps what the analyzer reported of the original sourceID, and
// answers what the catalog keeps of its captions now. A report names the
// claim it is of (its token): the one the original is under, which it ends,
// or the one whose lease ended last, whose failure was counted then; any
// other is ErrNotTheClaim and changes nothing. A read is held to what the
// claim handed out: the file's size, the length its record said, and the
// plan of windows of the span of its picture its records gave
// (library.ReadRefused); a record that said no length, or a read of another
// length, is a finding of its own (noRecordLength, lengthMismatch), final
// for that file and that record. A read keeps whether each window showed
// closed captions, the ffprobe that read it and the frames it decoded, and
// lets go of a retirement of the original the retire job held for want of
// them (library's ReleaseCaptionsHold), in the same transaction and under
// the title's lock, which the retire job's hold takes too, so that a hold
// written while a read lands sees it. The worst a read says decides: a
// window of the file handed out that decoded frames and showed captions, or
// counted frames in fields, is kept as found, whatever else of the read
// failed or was refused, of a lapsed claim too, and once kept no later read
// of the same file replaces it. A report that leaves the original not read
// in full (a window that decoded no frame or said no counts of its fields, a
// read refused, a failure) counts as a failure and waits its backoff; a
// failure leaves a read as it is. Released gives the original's claim back,
// which waits ReleaseWait, nothing else. An unknown original is ErrNoSource;
// a report that can't be taken (no read, no ffprobe or one that is not 7.1, a
// read without windows, more than maxWindows of them, a length that is no
// number above 0) a BadReport. A window that is no window (a value of
// another type, one out of its range, a key given twice) is no BadReport: it
// is kept as a failed window and said.
func Record(ctx context.Context, st *store.Store, sourceID string, r Report, now time.Time) (*Read, error) {
	var o outcome
	if !r.Released {
		if r.Original == nil {
			return nil, &BadReport{"a report says what a read found of the original (original), or gives the claim " +
				"back (released)"}
		}
		var err error
		if o, err = r.Original.check(); err != nil {
			return nil, err
		}
	}
	pool := st.Pool()
	var itemID string
	err := pool.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_itemsources WHERE id = $1`, sourceID).Scan(&itemID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSource
	}
	if err != nil {
		return nil, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// the title's lock, as the retire job's hold takes it: a hold and the
	// read that would release it go one after the other
	if err := library.LockItem(ctx, tx, itemID); err != nil {
		return nil, err
	}
	kept, err := scanRead(tx.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.SourceCaptionsTable+`
		WHERE source_id = $1 FOR UPDATE`, sourceID))
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(r.Token)
	// owns: the report is of the claim the original is under; lapsed: of the
	// claim whose lease ended last, counted a failure then
	owns := kept != nil && token != "" && kept.ClaimedAt != nil && kept.ClaimToken != nil && token == *kept.ClaimToken
	lapsed := kept != nil && token != "" && kept.LapsedToken != nil && token == *kept.LapsedToken
	if !owns && !lapsed {
		return nil, ErrNotTheClaim
	}
	if r.Released {
		if owns {
			if _, err := tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET claimedat = NULL, claimedby = NULL,
				claimtoken = NULL, nextattemptat = GREATEST(nextattemptat, $2), modifiedat = $3
				WHERE source_id = $1`, sourceID, now.Add(ReleaseWait), now); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return Of(ctx, pool, sourceID)
	}
	// the claim ends with the report of it
	if _, err := tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET
			claimedat = CASE WHEN $2 THEN NULL ELSE claimedat END,
			claimedby = CASE WHEN $2 THEN NULL ELSE claimedby END,
			claimtoken = CASE WHEN $2 THEN NULL ELSE claimtoken END,
			lapsedtoken = CASE WHEN $3 THEN NULL ELSE lapsedtoken END, modifiedat = $4
		WHERE source_id = $1`, sourceID, owns, lapsed, now); err != nil {
		return nil, err
	}
	// the file read is the one handed out: of the size the claim took
	sameSize := o.size != nil && kept.ClaimSize != nil && *o.size == *kept.ClaimSize
	if o.read && !sameSize {
		o.read, o.why = false, fmt.Sprintf("the file read is of %s bytes, not the %s of the original handed out: "+
			"another file, or one still being copied", sizeSaid(o.size), sizeSaid(kept.ClaimSize))
	}
	// what a window of the file handed out found is kept as found (the worst
	// a read says), whatever else of the read failed or was refused, of a
	// claim whose lease ended too
	found := o.found && sameSize
	// held to its record: of the length its record says (the claim's
	// durationMs), within DurationSlack, its windows where the plan of the
	// span its records gave places them (ReadRefused): a record that said no
	// length, or a read of the file handed out of another length, is a
	// finding about the record or the file (noRecordLength, lengthMismatch),
	// final for that file and that record, which keeps the original for good
	finding := o.holdTo(kept.ClaimDurationMs, kept.ClaimSpanStart, kept.ClaimSpanEnd, kept.ClaimSpanStep)
	if finding == "" && o.unscanned && sameSize {
		// final for that file and that record: its record, written again
		// with the picture's caption signature, has it read again
		finding = library.FindingUnscanned
	}
	// a failure that found what keeps the original anyway (a stream of
	// captions of its own, something off the closed lists), of the file
	// handed out, is kept as found: no read again of the same file changes
	// it
	finds := !o.read && sameSize && ((o.streams != nil && *o.streams > 0) || o.offList != nil)
	const (
		keep      = iota // what is kept stays
		takeRead         // the read, or what it found
		takeFind         // the finding
		takeFinds        // what the failure found
		takeFail         // the failure
	)
	what := takeFail
	switch {
	case kept.found() && kept.sameFile():
		// the worst read of the file once kept: no read since replaces it
		what = keep
	case found, o.read && !lapsed:
		what = takeRead
	case lapsed && kept.State == StateRead:
		// a late report of a claim whose lease ended replaces no read made
		// since (one that found what keeps the original does: above)
		what = keep
	case o.read:
		what = takeRead
	case finding != "":
		what = takeFind
	case kept.State == StateRead:
		// a read of it stays over a failure
		what = keep
	case finds:
		what = takeFinds
	}
	switch what {
	case takeRead:
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET state = 'read', captionwindows = $2,
			captions = $3, ffprobeversion = $4, framesdecoded = $5, duration = $6, reason = NULLIF($7, ''), readat = $8,
			sourcesize = claimsize, sourceqh1 = claimqh1, sourcerecordedat = claimrecordedat, captionstreams = $9,
			offlist = $10, finding = NULL, readspanstart = $11, readspanend = $12, readgop = $13, readreport = $14
			WHERE source_id = $1`, sourceID, o.windows, o.captions, o.version, max(o.frames, 1), o.duration, o.why, now,
			o.streams, o.offList, spanEnd(o.span, false), spanEnd(o.span, true), o.gop, reportText(o.report))
		// (a read kept as found decoded a frame at least, the one that
		// found what it found)
	case takeFind:
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET state = 'failed', captionwindows = $2,
			captions = NULL, ffprobeversion = $3, framesdecoded = $4, duration = $5, reason = $6, readat = $7,
			captionstreams = $8, offlist = $9, sourcesize = claimsize, sourceqh1 = claimqh1,
			sourcerecordedat = claimrecordedat, finding = $10, readreport = NULL WHERE source_id = $1`,
			sourceID, o.windows, o.version, o.frames, o.duration, o.why, now, o.streams, o.offList, finding)
	case takeFinds:
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET state = 'failed', captionwindows = NULL,
			captions = NULL, ffprobeversion = $2, framesdecoded = $3, duration = NULL, reason = $4,
			captionstreams = $5, offlist = $6, sourcesize = claimsize, sourceqh1 = claimqh1, finding = NULL,
			readreport = NULL WHERE source_id = $1`, sourceID, o.version, o.frames, o.why, o.streams, o.offList)
	case takeFail:
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET state = 'failed', captionwindows = NULL,
			captions = NULL, ffprobeversion = $2, framesdecoded = $3, duration = NULL, reason = $4,
			captionstreams = NULL, offlist = NULL, sourcesize = NULL, sourceqh1 = NULL, finding = NULL, readreport = NULL
			WHERE source_id = $1`,
			sourceID, o.version, o.frames, o.why)
	}
	if err != nil {
		return nil, err
	}
	// a report that leaves the original not read in full, nor found what
	// keeps it, waits its backoff before it is handed out again (a lapsed
	// claim's was counted when its lease ended); one that does is done
	var usable bool
	if err := tx.QueryRow(ctx, `SELECT `+readUsable("c", "s")+` FROM `+store.SourceCaptionsTable+` c
		JOIN com_nalet_katalog_itemsources s ON s.id = c.source_id WHERE c.source_id = $1`, sourceID).Scan(&usable); err != nil {
		return nil, err
	}
	switch {
	case usable:
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET failures = 0, nextattemptat = NULL
			WHERE source_id = $1`, sourceID)
	case !lapsed:
		f := kept.Failures + 1
		_, err = tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET failures = $2, nextattemptat = $3
			WHERE source_id = $1`, sourceID, f, now.Add(backoff(f)))
	}
	if err != nil {
		return nil, err
	}
	if what != keep && what != takeFail {
		// the retirement held for want of them is looked at again
		if err := library.ReleaseCaptionsHold(ctx, tx, itemID, sourceID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return Of(ctx, pool, sourceID)
}
