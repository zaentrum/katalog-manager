package library

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The retire job holds an original whose closed captions can't be told for
// want of a read, and names it kept; a read of
// another original of the title lets go of nothing, a read of its own (the
// captions backfill's, in the transaction that keeps it) lets go of the
// retirement held, and the next pass retires it.
func TestAReadOfTheCaptionsReleasesTheRetirementHeldForThem(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	rep, err := f.r.Pass(ctx)
	if err != nil || rep.Held != 1 || rep.Originals != 0 || keptOf(rep.Kept, KeptCaptions) != 1 ||
		!strings.Contains(rep.String(), "1 originals kept until their closed captions are read") {
		t.Fatalf("the pass: %+v, %v", rep, err)
	}
	f.kept(t)
	held := f.step(t, rtFilm)
	if !strings.Contains(held, "error="+captionsUnknownWhy) {
		t.Fatalf("the retire step: %s", held)
	}
	if err := ReleaseCaptionsHold(ctx, f.st.Pool(), rtFilm, "another-source"); err != nil {
		t.Fatal(err)
	}
	if got := f.step(t, rtFilm); got != held {
		t.Errorf("a read of another original let go of the hold: %s", got)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, captionwindows, captions,
			ffprobeversion, framesdecoded, duration, readspanstart, readspanend, sourcesize, sourceqh1, sourcerecordedat, readat)
		SELECT $1::varchar, $2::varchar, 'read', '`+noCaptionsWindows+`', false, '7.1.1', 288, 600, 0, 600, s.sizebytes, s.qh1, s.recordedat, now()
		FROM com_nalet_katalog_itemsources s WHERE s.id = $1::varchar
		ON CONFLICT (source_id) DO UPDATE SET state = 'read', captionwindows = EXCLUDED.captionwindows, captions = false,
			ffprobeversion = '7.1.1', framesdecoded = 288, duration = 600, readspanstart = 0, readspanend = 600, sourcesize = EXCLUDED.sourcesize,
			sourceqh1 = EXCLUDED.sourceqh1, sourcerecordedat = EXCLUDED.sourcerecordedat, readat = now()`, rtSource, rtFilm)
	if err := ReleaseCaptionsHold(ctx, f.st.Pool(), rtFilm, rtSource); err != nil {
		t.Fatal(err)
	}
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step once the captions are read: %s", got)
	}
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("the pass once the captions are read: %+v", rep)
	}
}

// planProbe is a stored probe that gives the plan of its picture (its start
// at 0, 24 frames a second; its length the record's), and planProbes the
// probes of the one original it is of.
var planProbe = Doc{{Key: "format", Value: Doc{{Key: "format_name", Value: "matroska,webm"}, {Key: "start_time", Value: "0.000000"}}},
	{Key: "streams", Value: []any{Doc{{Key: "index", Value: 0}, {Key: "codec_type", Value: "video"},
		{Key: "codec_name", Value: "h264"}, {Key: "field_order", Value: "progressive"}, {Key: "avg_frame_rate", Value: "24/1"}}}}}
var planProbes = []Doc{planProbe}

// window is a window of a read that decoded frames frames (0: failed, no
// times) from start on.
func window(at, start float64, frames int, captions bool) CaptionWindow {
	if frames == 0 {
		return CaptionWindow{At: at}
	}
	none := 0
	return CaptionWindow{At: at, Start: start, End: start + 1.88, Frames: frames, Captions: captions,
		InterlacedFrames: &none, RepeatFrames: &none}
}

// read is the gate's facts of a read of the original's windows ws, of the
// length duration (0: none said).
func read(duration float64, ws ...CaptionWindow) GateFacts {
	r := &CaptionsRead{Windows: ws}
	if duration > 0 {
		// a read says its length and the span it read, as long
		r.Duration, r.Span = &duration, &Span{End: duration}
	}
	return GateFacts{Captions: r}
}

// film is the source record of a film of length seconds (600 where it is 0
// or less), as a record writer writes one: its container, of that length,
// its essence's field order (no fields) and its one picture, h264,
// progressive.
func film(length float64) Doc {
	if length <= 0 {
		length = 600
	}
	return Doc{{Key: "container", Value: Doc{{Key: "format", Value: "matroska,webm"}, {Key: "durationMs", Value: length * 1000}}},
		{Key: "essence", Value: Doc{{Key: "interlaced", Value: false}}},
		{Key: "streams", Value: []any{Doc{{Key: "index", Value: 0}, {Key: "type", Value: "video"}, {Key: "codec", Value: "h264"},
			{Key: "fieldOrder", Value: "progressive"}}}}}
}

// filmWith is film with the essence essence (its field order no fields
// where it says none).
func filmWith(length float64, essence Doc) Doc {
	if _, has := essence.Get("interlaced"); !has {
		essence = essence.With("interlaced", false)
	}
	return film(length).With("essence", essence)
}

// recordOfProbe is the source record a record writer makes of the stored
// probe probe, as far as the captions rule reads it: its container (its
// format and length) and its streams (their kinds, codecs, field orders,
// heights and attached pictures).
func recordOfProbe(probe Doc) Doc {
	f, _ := probe.Get("format")
	fd, _ := f.(Doc)
	container := Doc{{Key: "format", Value: str(fd, "format_name")}}
	if d, ok := seconds(valueOf(fd.Get("duration"))); ok {
		container = append(container, Field{"durationMs", d * 1000})
	}
	var streams []any
	ps, _ := listOf(probe, "streams")
	for _, e := range ps {
		p, ok := e.(Doc)
		if !ok {
			continue
		}
		st := Doc{{Key: "index", Value: valueOf(p.Get("index"))}, {Key: "type", Value: str(p, "codec_type")}}
		if c := str(p, "codec_name"); c != "" {
			st = append(st, Field{"codec", c})
		}
		if o := str(p, "field_order"); o != "" {
			st = append(st, Field{"fieldOrder", o})
		}
		if h, ok := p.Get("height"); ok {
			st = append(st, Field{"height", h})
		}
		if flag(p, "disposition", "attached_pic") {
			st = append(st, Field{"dispositions", Doc{{Key: "attachedPic", Value: true}}})
		}
		streams = append(streams, st)
	}
	return Doc{{Key: "container", Value: container}, {Key: "streams", Value: streams}}
}

// every is a read of every planned window of a ten-minute video, captions
// in those at captioned.
func every(captioned ...float64) []CaptionWindow {
	var out []CaptionWindow
	for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		out = append(out, window(at, at*600, 48, slices.Contains(captioned, at)))
	}
	return out
}

// The captions rule asks of the original alone: one a window of whose read
// shows captions, or whose record says it has them, is kept for good,
// saying where; one a read of every planned window found none in goes; no
// read, a failed window and a read short of its windows can't tell.
func TestTheCaptionsRuleAsksTheOriginalAlone(t *testing.T) {
	says := filmWith(600, Doc{{Key: "closedCaptions", Value: true}})
	for _, c := range []struct {
		name             string
		record           Doc
		f                GateFacts
		carried, unknown string // what each begins with; "" none
	}{
		{name: "captions in two windows", f: read(600, every(0, 0.7)...), carried: CaptionsCarried + " at start, 70%"},
		{name: "none in any window", f: read(600, every()...)},
		{name: "the record says them, none in any window", record: says, f: read(600, every()...),
			carried: CaptionsCarried + " (its record says so)"},
		{name: "the record says them, no read", record: says, f: GateFacts{},
			carried: CaptionsCarried + " (its record says so)"},
		{name: "no read", f: GateFacts{}, unknown: captionsUnknownWhy},
		{name: "a failed window", f: read(600, append(every()[:3], window(0.5, 0, 0, false), every()[4], every()[5])...),
			unknown: CaptionsUnknown + ": its original's windows at 50% decoded no frame"},
		{name: "a failed window beside one with captions", f: read(600, append(every(0.1)[:3], window(0.5, 0, 0, false),
			every()[4], every()[5])...), carried: CaptionsCarried + " at 10%"},
	} {
		rec := c.record
		if rec == nil {
			rec = film(600)
		}
		carried, unknown := captionsRule([]Doc{rec}, planProbes, c.f)
		if !strings.HasPrefix(carried, c.carried) || (c.carried == "") != (carried == "") ||
			!strings.HasPrefix(unknown, c.unknown) || (c.unknown == "") != (unknown == "") {
			t.Errorf("%s: carried %q, unknown %q; want them to begin %q, %q", c.name, carried, unknown, c.carried, c.unknown)
		}
	}
}

// A transport stream whose clock starts again: the reader takes it for a
// wrap, and its windows at 10 to 90 % all decode the same frames after the
// new start. That read says nothing of the video, so it can't let the
// original go, whatever it found; nor can windows that lie on one another,
// or two at the same place.
func TestCaptionWindowsThatReadTheSameFramesOverTellNothing(t *testing.T) {
	again := []CaptionWindow{window(0, 1001.4, 48, false)}
	for _, at := range []float64{0.1, 0.3, 0.5, 0.7, 0.9} {
		again = append(again, CaptionWindow{At: at, Start: 95445.118, End: 95446.958, Frames: 47})
	}
	onOne := every()
	onOne[2].Start, onOne[2].End = onOne[1].Start+1, onOne[1].End+1
	twice := append(every(), window(0.5, 300, 48, false))
	for name, ws := range map[string][]CaptionWindow{"the same frames": again, "on one another": onOne, "twice": twice} {
		if carried, unknown := captionsRule([]Doc{film(0)}, planProbes, read(0, ws...)); carried != "" ||
			!strings.HasPrefix(unknown, CaptionsUnknown+": a read of its original says nothing of the video: windows at") {
			t.Errorf("%s: carried %q, unknown %q; want it not known", name, carried, unknown)
		}
	}
}

// A read is of the plan's six windows: the start, and 10 to 90 % of the
// video, each saying its fraction and starting at its place (at most 30 s
// short of its end). A read of the start alone, or of five, can't say there
// are none, nor can one whose window at the tail says where it lies (0.85)
// rather than its fraction; a picture two of whose places lie closer than a
// window reaches is read whole, so windows of it say nothing.
func TestAReadShortOfItsPlannedWindowsTellsNothing(t *testing.T) {
	if carried, unknown := captionsRule([]Doc{film(600)}, planProbes, read(600, window(0, 0, 48, false))); carried != "" ||
		!strings.Contains(unknown, "has no window at 10%, 30%, 50%, 70%, 90%") {
		t.Errorf("the start alone: carried %q, unknown %q", carried, unknown)
	}
	if _, unknown := captionsRule([]Doc{film(0)}, planProbes, read(0, every()...)); !strings.Contains(unknown, "says no length") {
		t.Errorf("a read that says no length: unknown %q; want it not known", unknown)
	}
	for _, c := range []struct {
		duration float64
		unknown  string // what its read's why says; "" it goes
	}{
		{600, ""},
		{200, ""},
		{120, ""},
		// 100 s: its windows at 70 and 90 % lie at 70 s both
		{100, "is read whole, every packet decoded, not in windows"},
		{40, "is read whole, every packet decoded, not in windows"},
		{4, "is read whole, every packet decoded, not in windows"},
	} {
		var ws []CaptionWindow
		for _, at := range planAts() {
			ws = append(ws, window(at, (Span{End: c.duration}).Place(at), 48, false))
		}
		carried, unknown := captionsRule([]Doc{film(c.duration)}, planProbes, read(c.duration, ws...))
		if carried != "" || !strings.Contains(unknown, c.unknown) || (c.unknown == "") != (unknown == "") {
			t.Errorf("a video of %v s read at its places: carried %q, unknown %q; want %q", c.duration, carried, unknown,
				c.unknown)
		}
		if _, unknown := captionsRule([]Doc{film(c.duration)}, planProbes, read(c.duration, ws[:len(ws)-1]...)); unknown == "" {
			t.Errorf("a video of %v s read short of a window: unknown %q", c.duration, unknown)
		}
	}
	// the window at the tail of a 200 s video says its fraction, 0.9, and
	// starts at 170 s; one that says where it lies (0.85) is no window of the
	// plan's, and the read short of its 90 %
	var tail []CaptionWindow
	for _, at := range planAts() {
		tail = append(tail, window(at, (Span{End: 200}).Place(at), 48, false))
	}
	tail[5].At = 0.85
	if _, unknown := captionsRule([]Doc{film(200)}, planProbes, read(200, tail...)); !strings.Contains(unknown,
		"its windows at 85% are at no fraction of the plan's") {
		t.Errorf("a 200 s video whose tail window says 0.85: unknown %q; want it not known", unknown)
	}
}

// gopRead120 is read of a 120 s video's windows ws, of a read that found a
// gop of 2 s.
func gopRead120(ws ...CaptionWindow) GateFacts {
	f := read(120, ws...)
	two := 2.0
	f.Captions.Gop = &two
	return f
}

// A read lands between the retire pass's look at the facts (no read yet)
// and its hold: the read's release finds no hold, and the hold, written on
// what the pass read before, would keep the original for good, as nothing
// hands it out again. The hold reads the captions again under the item's
// lock (which the read's report takes too) and is not written; the next
// pass retires the original.
func TestAHoldWrittenAfterTheReadLandedIsNotWritten(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	s := f.source(t, rtSource)
	v, err := VersionByID(ctx, f.st.Pool(), rtVersion)
	if err != nil || v == nil {
		t.Fatalf("the version: %v %v", v, err)
	}
	// the read's transaction: every planned window read, none with
	// captions, and the hold it looks for not there yet
	if err := ReleaseCaptionsHold(ctx, f.st.Pool(), rtFilm, rtSource); err != nil {
		t.Fatal(err)
	}
	// the pass, on the facts it read before the read landed
	err = f.r.hold(ctx, &retirement{src: s, itemDir: f.itemDir, ver: v}, captionsUnknownWhy+"; the original is kept")
	if err != errCalledOff {
		t.Fatalf("the hold: %v; want it called off", err)
	}
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step: %s; want no hold", got)
	}
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("the read is in, and the original is not retired: %+v; step %s", rep, f.step(t, rtFilm))
	}
	// with the captions still not known, the hold is written
	g := newRetireFixture(t)
	storetest.Exec(t, g.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	gv, _ := VersionByID(ctx, g.st.Pool(), rtVersion)
	if err := g.r.hold(ctx, &retirement{src: g.source(t, rtSource), itemDir: g.itemDir, ver: gv},
		captionsUnknownWhy+"; the original is kept"); err != nil || !strings.Contains(g.step(t, rtFilm), captionsUnknownWhy) {
		t.Errorf("a hold whose captions are still not known: %v, step %s", err, g.step(t, rtFilm))
	}
}

// The analyzer's own reads of made clips (testdata/captions/reads.json: its
// PUT bodies, decoded as the catalog decodes one, a failed window's null
// times as 0) through the rule: every original a window of which shows
// captions is kept for good, whatever its package carries (a copy or an
// encode); a file it could not read (a transport stream whose clock starts
// again, a raw stream: {error, ffprobe}, which the catalog keeps as a failed
// read, no read the gate takes) or two windows of which landed elsewhere
// (one whose clock goes back) is not known; of those only a read of every
// planned window of a file with no captions (plain-mkv__none) goes.
func TestTheAnalyzersReadsThroughTheCaptionsRule(t *testing.T) {
	b, err := os.ReadFile("testdata/captions/reads.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Body struct {
			Original struct {
				CaptionWindows []CaptionWindow `json:"captionWindows"`
				Duration       *float64        `json:"duration"`
				Span           *Span           `json:"span"`
				Error          string          `json:"error"`
			} `json:"original"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	notKnown := map[string]bool{"clockreset-ts__enc-nocc": true, "raw-cc__lost-after-40pct": true,
		"back-jump-ts__failed-windows": true}
	// these reads say no span (an analyzer from before it reported one):
	// none goes on them but with the span its picture has
	none := map[string]bool{"plain-mkv__none": true}
	if len(cases) < 10 {
		t.Fatalf("%d reads", len(cases))
	}
	for name, c := range cases {
		o := c.Body.Original
		f := GateFacts{}
		if o.Error == "" {
			f.Captions = &CaptionsRead{Windows: o.CaptionWindows, Duration: o.Duration, Span: o.Span}
		}
		length := 600.0
		if o.Duration != nil {
			length = *o.Duration
		}
		if none[name] {
			if _, unknown := captionsRule([]Doc{film(length)}, planProbes, f); !strings.Contains(unknown, "says no span") {
				t.Errorf("%s, of no span: unknown %q; want it not known", name, unknown)
			}
			f.Captions.Span = &Span{End: length}
		}
		carried, unknown := captionsRule([]Doc{film(length)}, planProbes, f)
		switch {
		case notKnown[name]:
			if carried != "" || !strings.HasPrefix(unknown, CaptionsUnknown) {
				t.Errorf("%s: carried %q, unknown %q; want it not known", name, carried, unknown)
			}
		case none[name]:
			if carried != "" || unknown != "" {
				t.Errorf("%s: carried %q, unknown %q; want a read of no captions", name, carried, unknown)
			}
		case !strings.HasPrefix(carried, CaptionsCarried+" at "):
			t.Errorf("%s: carried %q, unknown %q; want it kept for its captions", name, carried, unknown)
		}
	}
}

// A video too short for its planned windows to lie apart (of the length its
// read says, its windows read from keyframes before their places) has
// windows that overlap: the read is not known, and its why says exactly so,
// that nobody takes it for a title with captions. Windows that overlap in a
// video long enough for them to lie apart (a clock that starts again) keep
// the general why.
func TestWindowsOfAVideoTooShortForFiveSaySo(t *testing.T) {
	// a 120 s video, a gop of 2 s found among its first packets, a keyframe
	// at 80 s and none until 90 s: its windows at 70 and 90 % (84 and 90 s)
	// are both read from the keyframe at 80 s
	short := []CaptionWindow{window(0, 0, 48, false), window(0.1, 12, 48, false), window(0.3, 36, 48, false),
		window(0.5, 60, 48, false), window(0.7, 80, 48, false), window(0.9, 80, 48, false)}
	short[4].KeyframeBefore, short[5].KeyframeBefore = true, true
	if carried, unknown := captionsRule([]Doc{film(120)}, planProbes, gopRead120(short...)); carried != "" ||
		unknown != CaptionsUnknown+": windows overlap: the video is too short for five" {
		t.Errorf("a 120 s video: carried %q, unknown %q", carried, unknown)
	}
	// a 70 s one is read whole (its windows at 70 and 90 % lie at 40 s
	// both): windows of it say so
	if _, unknown := captionsRule([]Doc{film(70)}, planProbes, read(70, short[:4]...)); !strings.Contains(unknown,
		"is read whole") {
		t.Errorf("a 70 s video read in windows: unknown %q; want it read whole", unknown)
	}
	// a ten-minute one whose windows read the same frames over
	long := every()
	for i := 1; i < len(long); i++ {
		long[i].Start, long[i].End = 900, 901.88
	}
	if _, unknown := captionsRule([]Doc{film(600)}, planProbes, read(600, long...)); strings.Contains(unknown, TooShortForFive) ||
		!strings.Contains(unknown, "start in the wrong order") {
		t.Errorf("a ten-minute video whose windows read the same frames: unknown %q", unknown)
	}
	// and the same short read, no length said: the general why
	if _, unknown := captionsRule([]Doc{film(0)}, planProbes, read(0, short...)); strings.Contains(unknown, TooShortForFive) {
		t.Errorf("a read that says no length: unknown %q", unknown)
	}
}

// The analyzer's reads of made clips (testdata/captions/clips.json: its PUT
// body, of the analyzer that keeps to the closed lists, and the stored probe
// of each, ffprobe 7.1's) through the rule, as the gate takes them: no clip
// that carries closed captions anywhere goes on them. Those read where the
// reads find them carry them, HEVC's suffix SEI too, and a track of
// captions of its own; a second video stream, a picture track not marked as
// an attachment, an error or a window that landed elsewhere keep it as not
// known; only a clip with none goes.
func TestNoMadeClipWithCaptionsGoesOnTheAnalyzersRead(t *testing.T) {
	b, err := os.ReadFile("testdata/captions/clips.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Carries bool            `json:"carries"`
		Probe   json.RawMessage `json:"probe"`
		Body    struct {
			Original struct {
				CaptionWindows []CaptionWindow `json:"captionWindows"`
				Duration       *float64        `json:"duration"`
				Span           *Span           `json:"span"`
				CaptionStreams *int            `json:"captionStreams"`
				OffList        []string        `json:"offList"`
				Error          string          `json:"error"`
			} `json:"original"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	const carried, unknown, goes = "carried", "not known", "goes"
	want := map[string]string{
		"back-jump-ts": unknown, "c608-mov": carried, "clock-reset-ts": unknown, "cover-first-mkv": unknown,
		"cover-first-mp4": carried, "fwd-jump-ts": carried, "hevc-prefix-sei-mkv": carried,
		"hevc-suffix-sei-mkv": carried, "hevc-suffix-sei-ts": carried, "joined-recordings-ts": carried,
		"mpeg2-a53-mpg": carried, "mpeg2-a53-ts": carried, "mpeg2-dvd-mpg": carried, "mpeg2-dvd-ts": carried,
		"mpeg2-scte20-mpg": carried, "mpeg2-scte20-ts": carried, "open-gop-h264": carried, "open-gop-hevc": carried,
		"partial-arrival-ts": goes, "partial-arrival-ts-complete": carried, "plain-mkv": goes,
		"second-video-5fps-mkv": unknown, "second-video-has-captions-mkv": unknown, "short-duration-mkv": unknown,
		"still-first-mkv": unknown, "vfr-mkv": carried,
	}
	if len(cases) != len(want) {
		t.Fatalf("%d clips, %d verdicts", len(cases), len(want))
	}
	for name, c := range cases {
		probe, err := DecodeDoc(c.Probe)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		o := c.Body.Original
		f := GateFacts{}
		if o.Error == "" {
			f.Captions = &CaptionsRead{Windows: o.CaptionWindows, Duration: o.Duration, CaptionStreams: o.CaptionStreams,
				OffList: o.OffList, Span: o.Span}
		} else {
			f.CaptionsFailed = o.Error
			if len(o.OffList) > 0 || (o.CaptionStreams != nil && *o.CaptionStreams > 0) {
				f.CaptionsFinds = &CaptionsRead{CaptionStreams: o.CaptionStreams, OffList: o.OffList}
			}
		}
		rec := recordOfProbe(probe)
		if want[name] == goes {
			// the probes kept here give no start and no frame rate of their
			// picture: no plan, so nothing goes; with a start of 0 and 25
			// frames a second, as such a file's probe gives them, it may
			if _, why := captionsRule([]Doc{rec}, []Doc{probe}, f); !strings.HasPrefix(why, CaptionsNoPlan) {
				t.Errorf("%s, of a probe of no start and no rate: %q; want no plan", name, why)
			}
			var p map[string]any
			if err := json.Unmarshal(c.Probe, &p); err != nil {
				t.Fatal(err)
			}
			pic := p["streams"].([]any)[0].(map[string]any)
			pic["start_time"], pic["avg_frame_rate"] = "0.000000", "25/1"
			b, _ := json.Marshal(p)
			if probe, err = DecodeDoc(b); err != nil {
				t.Fatal(err)
			}
			// a read that says no span (the analyzer's of before it reported
			// one) goes on nothing; with the span its picture has, it goes
			if _, why := captionsRule([]Doc{rec}, []Doc{probe}, f); !strings.Contains(why, "says no span") {
				t.Errorf("%s, of no span: %q; want it not known", name, why)
			}
			span, _ := PictureSpan(rec, probe)
			f.Captions.Span = &Span{Start: span.Start, End: span.End}
			// nor does a window the tail short of the end that says where it
			// lies (the analyzer's of before: 0.751) rather than its fraction;
			// it is said as the plan's 0.9 below
			ws := f.Captions.Windows
			for i := range ws {
				if ws[i].At > 0.7+atSlack && ws[i].At < 0.9-atSlack {
					if _, why := captionsRule([]Doc{rec}, []Doc{probe}, f); !strings.Contains(why,
						"at no fraction of the plan's") {
						t.Errorf("%s, its tail window at %v: %q; want it not known", name, ws[i].At, why)
					}
					ws[i].At = 0.9
				}
			}
			// nor, of windows that start at their keyframes seconds from
			// their places, does a read that says no gop (the analyzer's of
			// before it reported one): such windows lie off their places; of
			// a gop of 2 s, a group of pictures the analyzer reports, it goes
			if _, why := captionsRule([]Doc{rec}, []Doc{probe}, f); name == "partial-arrival-ts" &&
				!strings.Contains(why, "lie off their places") {
				t.Errorf("%s, of no gop: %q; want it not known", name, why)
			}
			two := 2.0
			f.Captions.Gop = &two
		}
		yes, why := captionsRule([]Doc{rec}, []Doc{probe}, f)
		got := goes
		switch {
		case yes != "":
			got = carried
		case why != "":
			got = unknown
		}
		if got != want[name] || (c.Carries && got == goes) {
			t.Errorf("%s: %s (%q %q); want %s", name, got, yes, why, want[name])
		}
	}
	// the clips a second video stream keeps are kept by the closing rule,
	// which no read lets go of
	for _, name := range []string{"cover-first-mkv", "second-video-5fps-mkv", "still-first-mkv"} {
		probe, _ := DecodeDoc(cases[name].Probe)
		if _, why := captionsRule([]Doc{recordOfProbe(probe)}, []Doc{probe}, GateFacts{}); !strings.Contains(why,
			"video-streams:2") {
			t.Errorf("%s by its probe alone: %q; want it off the closed lists", name, why)
		}
	}
}

// A read of another length than its record's is a finding (lengthMismatch)
// that keeps the original for good, with the standing of captions not
// known: the gate says so of the failure it was kept as, and of a read the
// gate finds of another length itself.
func TestALengthMismatchKeepsTheOriginal(t *testing.T) {
	f := GateFacts{CaptionsFailed: "a read of its original says it lasts 610 s, and its record 600 s",
		CaptionsFinds: &CaptionsRead{Finding: FindingLengthMismatch}}
	if carried, unknown := captionsRule([]Doc{film(600)}, planProbes, f); carried != "" ||
		unknown != CaptionsLengthMismatch+": a read of its original says it lasts 610 s, and its record 600 s" ||
		!strings.HasPrefix(unknown, CaptionsUnknown) {
		t.Errorf("the finding: carried %q, unknown %q", carried, unknown)
	}
	rec := film(600)
	if _, unknown := captionsRule([]Doc{rec}, planProbes, read(610, every()...)); !strings.HasPrefix(unknown,
		CaptionsLengthMismatch+": ") {
		t.Errorf("a read of another length: unknown %q", unknown)
	}
	if _, unknown := captionsRule([]Doc{rec}, planProbes, read(600, every()...)); unknown != "" {
		t.Errorf("a read of its length: unknown %q", unknown)
	}
}

// A read's windows lie where they were planned: each starts within
// WindowPlaceSlack of its place, of a read that found a gop (one of 0 holds
// them to their places exactly). Six windows in order and apart that all lie
// in the first 12 s of a two-hour video say nothing of the rest of it.
func TestWindowsFarFromTheirPlacesTellNothing(t *testing.T) {
	var ws []CaptionWindow
	for i, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		ws = append(ws, window(at, float64(2*i), 48, false))
	}
	if carried, unknown := captionsRule([]Doc{film(7200)}, planProbes, read(7200, ws...)); carried != "" ||
		!strings.Contains(unknown, "its windows at 10%, 30%, 50%, 70%, 90% lie off their places") {
		t.Errorf("windows at 0-12 s of a 7200 s video: carried %q, unknown %q; want it not known", carried, unknown)
	}
	// within the slack (a keyframe before its place) is where it was planned
	near := every()
	for i := range near[1:] {
		near[i+1].Start -= WindowPlaceSlack - 1
	}
	two := 2.0
	if carried, unknown := captionsRule([]Doc{film(600)}, planProbes, gopRead(&two, near...)); carried != "" || unknown != "" {
		t.Errorf("windows a keyframe before their places: carried %q, unknown %q", carried, unknown)
	}
}

// A window is in place by where it starts: at the keyframe at or before its
// place, which a long group of pictures puts more than WindowPlaceSlack
// before it. Where the read says so (keyframeBefore), a window up to
// WindowKeyframeSlack before its place is in place, of a read that found a
// gop; without it the general slack holds; further back, or after its place
// beyond the general slack, it is not, either way.
func TestAWindowAtTheKeyframeBeforeItsPlaceIsInPlace(t *testing.T) {
	for _, c := range []struct {
		name           string
		back           float64 // how far before its place the window at 50 % starts
		keyframeBefore bool
		inPlace        bool
	}{
		{"25 s back at the keyframe before", 25, true, true},
		{"25 s back, no keyframe said", 25, false, false},
		{"40 s back at the keyframe before", 40, true, false},
		{"40 s back, no keyframe said", 40, false, false},
		{"20 s after, the keyframe before said", -20, true, false},
	} {
		ws := every()
		ws[3].Start -= c.back
		ws[3].End -= c.back
		ws[3].KeyframeBefore = c.keyframeBefore
		two := 2.0
		carried, unknown := captionsRule([]Doc{film(600)}, planProbes, gopRead(&two, ws...))
		far := strings.Contains(unknown, "its windows at 50% lie off their places")
		if carried != "" || far == c.inPlace || (c.inPlace && unknown != "") {
			t.Errorf("%s: carried %q, unknown %q; want in place %v", c.name, carried, unknown, c.inPlace)
		}
	}
}

// A window is a read of at least MinWindowFrames frames: six windows of one
// frame each say nothing of their part of the video.
func TestAWindowOfOneFrameTellsNothing(t *testing.T) {
	ws := every()
	for i := range ws {
		ws[i].Frames, ws[i].End = 1, ws[i].Start
	}
	if carried, unknown := captionsRule([]Doc{film(600)}, planProbes, read(600, ws...)); carried != "" ||
		!strings.Contains(unknown, "decoded fewer than 24 frames") {
		t.Errorf("six windows of one frame: carried %q, unknown %q; want it not known", carried, unknown)
	}
}

// Closed captions in a stream of their own (an EIA-608 track, one tagged
// c608 or c708, as a QuickTime file keeps them) are carried, in the record,
// in the stored probe or as the read found them (captionStreams), whatever
// the windows found: the original is kept for good.
func TestCaptionsInAStreamOfTheirOwnAreCarried(t *testing.T) {
	doc := func(js string) Doc {
		d, err := DecodeDoc([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	recorded := doc(`{"streams": [{"index": 0, "type": "video", "codec": "h264"},
		{"index": 1, "type": "subtitle", "codec": "eia_608"}]}`)
	// a record keeps no codec tag: its probe's stream of the same index says it
	tagged := doc(`{"streams": [{"index": 0, "type": "video", "codec": "h264"},
		{"index": 1, "type": "subtitle", "codec": null}]}`)
	taggedProbe := doc(`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264"},
		{"index": 1, "codec_type": "subtitle", "codec_tag_string": "c708"}]}`)
	probed := doc(`{"format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2"}, "streams": [{"index": 0,
		"codec_type": "video", "codec_name": "h264"}, {"index": 1, "codec_type": "subtitle", "codec_name": "eia_608",
		"codec_tag_string": "c608"}]}`)
	one := 1
	found := read(600, every()...)
	found.Captions.CaptionStreams = &one
	for _, c := range []struct {
		name          string
		record, probe Doc
		f             GateFacts
		carried       string
	}{
		{"the record's EIA-608 stream", recorded, nil, read(600, every()...), "its record lists a stream of them, eia_608"},
		{"the record's c708 tag, by its probe", tagged, taggedProbe, GateFacts{},
			"its record lists a stream of them, c708"},
		{"the probe's c608 track", Doc{}, probed, read(600, every()...), "its probe lists a stream of them, eia_608"},
		{"the read's stream of them", Doc{}, nil, found, "a read found a stream of them of its own"},
	} {
		var probes []Doc
		if c.probe != nil {
			probes = []Doc{c.probe}
		}
		carried, unknown := captionsRule([]Doc{c.record}, probes, c.f)
		if !strings.HasPrefix(carried, CaptionsCarried+" ("+c.carried) || unknown != "" {
			t.Errorf("%s: carried %q, unknown %q; want it carried (%s)", c.name, carried, unknown, c.carried)
		}
	}
	none := 0
	found.Captions.CaptionStreams = &none
	if carried, unknown := captionsRule([]Doc{film(600)}, planProbes, found); carried != "" || unknown != "" {
		t.Errorf("a read that found no stream of them: carried %q, unknown %q", carried, unknown)
	}
}

// A read reads one picture stream: an original whose record or probe lists
// more than one (a video track beside the film's, a picture not marked as an
// attachment) has captions not known, whatever the read found in the one.
func TestCaptionsOfMoreThanOnePictureStreamAreNotKnown(t *testing.T) {
	two, err := DecodeDoc([]byte(`{"format": {"format_name": "matroska,webm"}, "streams": [{"index": 0,
		"codec_type": "video", "codec_name": "h264"}, {"index": 1, "codec_type": "video", "codec_name": "h264"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if carried, unknown := captionsRule([]Doc{film(600)}, []Doc{two}, read(600, every()...)); carried != "" ||
		unknown != closedNotKnown+": off the closed lists: video-streams:2" {
		t.Errorf("two picture streams: carried %q, unknown %q", carried, unknown)
	}
	if carried, _ := captionsRule([]Doc{film(600)}, []Doc{two}, read(600, every(0.5)...)); !strings.HasPrefix(carried,
		CaptionsCarried+" at 50%") {
		t.Errorf("two picture streams, captions in the one read: carried %q; want them carried", carried)
	}
}

// waitingSince is since when the retire job notes that the original source
// waits on its closed captions alone; nil when it doesn't.
func waitingSince(t *testing.T, f *retireFixture, source string) *time.Time {
	t.Helper()
	var at *time.Time
	err := f.st.Pool().QueryRow(context.Background(), `SELECT retirewaitingsince FROM com_nalet_katalog_sourcecaptions
		WHERE source_id = $1`, source).Scan(&at)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	return at
}

// The retire job notes an original it holds for its closed captions alone -
// its title has a complete version, and nothing else of the gate, nor its
// chapters and picture area, keeps it - that the captions backfill reads it
// first; not one the gate keeps for more besides, and the note goes once
// anything else keeps it. The note decides nothing: the original is held
// the same, its why the same.
func TestTheRetireJobNotesTheOriginalsThatWaitOnTheirCaptionsAlone(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	if rep := f.pass(t); rep.Held != 1 {
		t.Fatalf("the pass: %+v", rep)
	}
	if !strings.Contains(f.step(t, rtFilm), "error="+captionsUnknownWhy) || waitingSince(t, f, rtSource) == nil {
		t.Fatalf("an original held for its captions alone: %s, waiting since %v", f.step(t, rtFilm),
			waitingSince(t, f, rtSource))
	}
	// its chapters not known besides: once the job looks again, no note
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_chapterreads WHERE item_id = $1`, rtFilm)
	if err := ReleaseCaptionsHold(ctx, f.st.Pool(), rtFilm, rtSource); err != nil {
		t.Fatal(err)
	}
	if rep := f.pass(t); rep.Originals != 0 {
		t.Fatalf("the pass: %+v", rep)
	}
	if at := waitingSince(t, f, rtSource); at != nil {
		t.Errorf("an original whose chapters aren't known either is noted waiting on its captions alone (%v)", at)
	}

	// the gate keeps it for a lower bit depth too: held for its captions, as
	// before, and not noted
	full := fullEssence()
	g := newRetireFixtureOf(t, full, full)
	librarytest.WriteVersion(t, g.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
		SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: packageRecord(with2(full, "videoBitDepth", 8))})
	storetest.Exec(t, g.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	if rep := g.pass(t); rep.Held != 1 || !strings.Contains(g.step(t, rtFilm), "error="+captionsUnknownWhy) {
		t.Fatalf("the pass: %+v, %s", rep, g.step(t, rtFilm))
	}
	if at := waitingSince(t, g, rtSource); at != nil {
		t.Errorf("an original the gate keeps for more than its captions is noted waiting on them alone (%v)", at)
	}

	// its container off the closing rule's list (its stored probe's): held
	// for that, which no read lets go of, and not noted
	h := newRetireFixture(t)
	librarytest.Write(t, filepath.Join(h.sourceDir, ProbeFile), []byte(`{"format": {"format_name": "flv"}, "streams": [{"index": 0,
		"codec_type": "video", "codec_name": "h264", "field_order": "progressive"}]}`))
	h.rewriteSource(t, stated(sevenOne), nil)
	storetest.Exec(t, h.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	if rep := h.pass(t); rep.Held != 1 || !strings.Contains(h.step(t, rtFilm), "error="+closedNotKnown+": off the closed lists: container:flv") {
		t.Fatalf("the pass: %+v, %s", rep, h.step(t, rtFilm))
	}
	if at := waitingSince(t, h, rtSource); at != nil {
		t.Errorf("an original the closing rule keeps is noted waiting on its captions alone (%v)", at)
	}

	// a read of another length than its record's: held on the finding,
	// final for its file and record, and not noted
	m := newRetireFixture(t)
	storetest.Exec(t, m.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	storetest.Exec(t, m.st, `INSERT INTO com_nalet_katalog_sourcecaptions (source_id, item_id, state, reason, finding,
			sourcesize, sourceqh1, sourcerecordedat)
		SELECT s.id, s.item_id, 'failed', 'a read of its original says it lasts 610 s, and its record 600 s', $2,
			s.sizebytes, s.qh1, s.recordedat FROM com_nalet_katalog_itemsources s WHERE s.id = $1`, rtSource,
		FindingLengthMismatch)
	if rep := m.pass(t); rep.Held != 1 || !strings.Contains(m.step(t, rtFilm), "error="+CaptionsLengthMismatch) {
		t.Fatalf("the pass: %+v, %s", rep, m.step(t, rtFilm))
	}
	if at := waitingSince(t, m, rtSource); at != nil {
		t.Errorf("an original kept on a length mismatch is noted waiting on its captions alone (%v)", at)
	}

	// a record that says no length, and a picture of hevc whose record says
	// no read of its every packet: held for what no read lets go of, and not
	// noted
	for name, change := range map[string]func(rec map[string]any){
		"no length": func(rec map[string]any) { delete(rec, "container") },
		"hevc": func(rec map[string]any) {
			rec["streams"] = []any{map[string]any{"index": 0, "type": "video", "codec": "hevc", "fieldOrder": "progressive"}}
		},
	} {
		n := newRetireFixture(t)
		n.rewriteRecord(t, change, nil)
		storetest.Exec(t, n.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
		if rep := n.pass(t); rep.Held != 1 {
			t.Fatalf("%s: the pass: %+v, %s", name, rep, n.step(t, rtFilm))
		}
		if at := waitingSince(t, n, rtSource); at != nil {
			t.Errorf("%s: an original held for what no read lets go of is noted waiting on its captions alone (%v)", name, at)
		}
	}
}
