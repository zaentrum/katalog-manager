package library

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"

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
			ffprobeversion, framesdecoded, duration, sourcesize, sourceqh1, readat)
		SELECT $1::varchar, $2::varchar, 'read', '`+noCaptionsWindows+`', false, '7.1.1', 288, 600, s.sizebytes, s.qh1, now()
		FROM com_nalet_katalog_itemsources s WHERE s.id = $1::varchar
		ON CONFLICT (source_id) DO UPDATE SET state = 'read', captionwindows = EXCLUDED.captionwindows, captions = false,
			ffprobeversion = '7.1.1', framesdecoded = 288, duration = 600, sourcesize = EXCLUDED.sourcesize,
			sourceqh1 = EXCLUDED.sourceqh1, readat = now()`, rtSource, rtFilm)
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
		r.Duration = &duration
	}
	return GateFacts{Captions: r}
}

// film is the source record of a film of length seconds (600 where it is 0
// or less), as a record writer writes one: its container, of that length,
// and its one picture, h264, progressive.
func film(length float64) Doc {
	if length <= 0 {
		length = 600
	}
	return Doc{{Key: "container", Value: Doc{{Key: "format", Value: "matroska,webm"}, {Key: "durationMs", Value: length * 1000}}},
		{Key: "streams", Value: []any{Doc{{Key: "index", Value: 0}, {Key: "type", Value: "video"}, {Key: "codec", Value: "h264"},
			{Key: "fieldOrder", Value: "progressive"}}}}}
}

// filmWith is film with the essence essence.
func filmWith(length float64, essence Doc) Doc {
	return append(film(length), Field{"essence", essence})
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
		carried, unknown := captionsRule([]Doc{rec}, nil, c.f)
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
		if carried, unknown := captionsRule([]Doc{film(0)}, nil, read(0, ws...)); carried != "" ||
			!strings.HasPrefix(unknown, CaptionsUnknown+": a read of its original says nothing of the video: windows at") {
			t.Errorf("%s: carried %q, unknown %q; want it not known", name, carried, unknown)
		}
	}
}

// A read is of the planned windows: the start, and 10 to 90 % of the video,
// as many as its length allows (each 30 s, or half the video, short of its
// end; those that fall on one another one). A read of the start alone, or of
// fewer than its length explains, can't say there are none.
func TestAReadShortOfItsPlannedWindowsTellsNothing(t *testing.T) {
	if carried, unknown := captionsRule([]Doc{film(600)}, nil, read(600, window(0, 0, 48, false))); carried != "" ||
		!strings.Contains(unknown, "has no window at 10%, 30%, 50%, 70%, 90%") {
		t.Errorf("the start alone: carried %q, unknown %q", carried, unknown)
	}
	if _, unknown := captionsRule([]Doc{film(0)}, nil, read(0, every()...)); !strings.Contains(unknown, "says no length") {
		t.Errorf("a read that says no length: unknown %q; want it not known", unknown)
	}
	for _, c := range []struct {
		duration float64
		ats      []float64
		unknown  string // what its read's why says; "" it goes
	}{
		{600, []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9}, ""},
		{120, []float64{0, 0.1, 0.3, 0.5, 0.7, 0.75}, ""},
		{40, []float64{0, 0.1, 0.3, 0.5}, ""},
		{4, []float64{0, 0.1, 0.3, 0.5}, TooShortForFive},
	} {
		got := plannedAts(c.duration)
		if !slices.EqualFunc(got, c.ats, func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }) {
			t.Errorf("a video of %v s: windows at %v, want %v", c.duration, got, c.ats)
			continue
		}
		var ws []CaptionWindow
		for _, at := range c.ats {
			ws = append(ws, window(at, at*c.duration, 48, false))
		}
		carried, unknown := captionsRule([]Doc{film(c.duration)}, nil, read(c.duration, ws...))
		if carried != "" || !strings.Contains(unknown, c.unknown) || (c.unknown == "") != (unknown == "") {
			t.Errorf("a video of %v s read at %v: carried %q, unknown %q; want %q", c.duration, c.ats, carried, unknown,
				c.unknown)
		}
		if _, unknown := captionsRule([]Doc{film(c.duration)}, nil, read(c.duration, ws[:len(ws)-1]...)); unknown == "" {
			t.Errorf("a video of %v s read short of a window: unknown %q", c.duration, unknown)
		}
	}
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
				Error          string          `json:"error"`
			} `json:"original"`
		} `json:"body"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	notKnown := map[string]bool{"clockreset-ts__enc-nocc": true, "raw-cc__lost-after-40pct": true,
		"back-jump-ts__failed-windows": true}
	none := map[string]bool{"plain-mkv__none": true}
	if len(cases) < 10 {
		t.Fatalf("%d reads", len(cases))
	}
	for name, c := range cases {
		o := c.Body.Original
		f := GateFacts{}
		if o.Error == "" {
			f.Captions = &CaptionsRead{Windows: o.CaptionWindows, Duration: o.Duration}
		}
		length := 600.0
		if o.Duration != nil {
			length = *o.Duration
		}
		carried, unknown := captionsRule([]Doc{film(length)}, nil, f)
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
	// a 20 s video with a keyframe every 5 s: the windows at 0, 2, 6 and
	// 10 s are read from the keyframes at 0, 0, 5 and 10 s
	short := []CaptionWindow{window(0, 0, 48, false), window(0.1, 0, 48, false), window(0.3, 5, 48, false),
		window(0.5, 10, 48, false)}
	if carried, unknown := captionsRule([]Doc{film(20)}, nil, read(20, short...)); carried != "" ||
		unknown != CaptionsUnknown+": windows overlap: the video is too short for five" {
		t.Errorf("a 20 s video: carried %q, unknown %q", carried, unknown)
	}
	// a ten-minute one whose windows read the same frames over
	long := every()
	for i := 1; i < len(long); i++ {
		long[i].Start, long[i].End = 900, 901.88
	}
	if _, unknown := captionsRule([]Doc{film(600)}, nil, read(600, long...)); strings.Contains(unknown, TooShortForFive) ||
		!strings.Contains(unknown, "start in the wrong order") {
		t.Errorf("a ten-minute video whose windows read the same frames: unknown %q", unknown)
	}
	// and the same short read, no length said: the general why
	if _, unknown := captionsRule([]Doc{film(0)}, nil, read(0, short...)); strings.Contains(unknown, TooShortForFive) {
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
				OffList: o.OffList}
		} else {
			f.CaptionsFailed = o.Error
			if len(o.OffList) > 0 || (o.CaptionStreams != nil && *o.CaptionStreams > 0) {
				f.CaptionsFinds = &CaptionsRead{CaptionStreams: o.CaptionStreams, OffList: o.OffList}
			}
		}
		yes, why := captionsRule([]Doc{recordOfProbe(probe)}, []Doc{probe}, f)
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
	if carried, unknown := captionsRule([]Doc{film(600)}, nil, f); carried != "" ||
		unknown != CaptionsLengthMismatch+": a read of its original says it lasts 610 s, and its record 600 s" ||
		!strings.HasPrefix(unknown, CaptionsUnknown) {
		t.Errorf("the finding: carried %q, unknown %q", carried, unknown)
	}
	rec := film(600)
	if _, unknown := captionsRule([]Doc{rec}, nil, read(610, every()...)); !strings.HasPrefix(unknown,
		CaptionsLengthMismatch+": ") {
		t.Errorf("a read of another length: unknown %q", unknown)
	}
	if _, unknown := captionsRule([]Doc{rec}, nil, read(600, every()...)); unknown != "" {
		t.Errorf("a read of its length: unknown %q", unknown)
	}
}

// A read's windows lie where they were planned: each starts within
// WindowPlaceSlack of where the read's start window starts plus its share of
// the video. Six windows in order and apart that all lie in the first 12 s
// of a two-hour video say nothing of the rest of it.
func TestWindowsFarFromTheirPlacesTellNothing(t *testing.T) {
	var ws []CaptionWindow
	for i, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		ws = append(ws, window(at, float64(2*i), 48, false))
	}
	if carried, unknown := captionsRule([]Doc{film(7200)}, nil, read(7200, ws...)); carried != "" ||
		!strings.Contains(unknown, "its windows at 10%, 30%, 50%, 70%, 90% lie more than 15 s from their places") {
		t.Errorf("windows at 0-12 s of a 7200 s video: carried %q, unknown %q; want it not known", carried, unknown)
	}
	// within the slack (a keyframe before its place) is where it was planned
	near := every()
	for i := range near[1:] {
		near[i+1].Start -= WindowPlaceSlack - 1
	}
	if carried, unknown := captionsRule([]Doc{film(600)}, nil, read(600, near...)); carried != "" || unknown != "" {
		t.Errorf("windows a keyframe before their places: carried %q, unknown %q", carried, unknown)
	}
}

// A window is in place by where it starts: at the keyframe at or before its
// place, which a long group of pictures puts more than WindowPlaceSlack
// before it. Where the read says so (keyframeBefore), a window up to
// WindowKeyframeSlack before its place is in place; without it the general
// slack holds; further back, or after its place, it is not, either way.
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
		carried, unknown := captionsRule([]Doc{film(600)}, nil, read(600, ws...))
		far := strings.Contains(unknown, "its windows at 50% lie more than 15 s from their places")
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
	if carried, unknown := captionsRule([]Doc{film(600)}, nil, read(600, ws...)); carried != "" ||
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
	if carried, unknown := captionsRule([]Doc{film(600)}, nil, found); carried != "" || unknown != "" {
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
