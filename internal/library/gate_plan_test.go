package library

import (
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// wholeRead is a read of the whole picture of a film of length seconds at
// 24 frames a second: one window at its start, frames of its packets
// decoded, its leading pictures leading (below 0: none said), the read
// saying the span it read, the length.
func wholeRead(length float64, frames, packets, leading int) GateFacts {
	none := 0
	w := CaptionWindow{At: 0, Whole: true, Start: 0, End: length - 1.0/24, Frames: frames, Packets: &packets,
		InterlacedFrames: &none, RepeatFrames: &none}
	if leading >= 0 {
		w.Leading = &leading
	}
	return read(length, w)
}

// The plan's details: no window is placed more than half the span in for a
// short one (the tail alone caps them); a picture two of whose places lie
// closer than a window reaches (48 packets at its frame step, the read's gop
// and half a second) is read whole, so a read of it in windows says
// nothing; a read of the whole picture stands for the six windows - of a
// short picture, and of a long one - where it decoded every packet after its
// first keyframe (the leading pictures before it counted apart: leading; a
// decoder that outputs them too is complete) and its frames reach the span's
// start and end, within a frame step and half a second; one that doesn't,
// one among other windows, or one not at the start says nothing; HEVC still
// needs its packets' signature.
func TestAWholeReadStandsForTheWindows(t *testing.T) {
	// at 24 frames a second, 48 packets reach 2 s: of a gop of 0 a 108.3 s
	// picture's places at 70 and 90 % (75.8 and 78.3 s) lie 2.49 s apart,
	// closer than 2.5 s, a 108.4 s one's 2.52 s; of a gop of 2 s, 4.5 s
	step := 1.0 / 24
	for _, c := range []struct {
		length, gop float64
		short       bool
	}{{108.3, 0, true}, {108.4, 0, false}, {114.9, 2, true}, {115.1, 2, false}, {100, 0, true}, {40, 0, true},
		{600, 0, false}, {600, 12, false}} {
		if got := ShortPicture(Span{End: c.length, Step: step}, c.gop); got != c.short {
			t.Errorf("a %v s picture, a gop of %v s: read whole %v; want %v", c.length, c.gop, got, c.short)
		}
	}
	frames := 40 * 24
	for _, c := range []struct {
		name   string
		length float64
		f      GateFacts
		want   string // what the why holds; "" it goes
	}{
		{"40 s read whole", 40, wholeRead(40, frames, frames, 0), ""},
		{"40 s read whole, leading 3: three pictures before its first keyframe not decoded", 40,
			wholeRead(40, frames-3, frames, 3), ""},
		{"40 s read whole, leading 3, its decoder outputting them too", 40, wholeRead(40, frames, frames, 3), ""},
		{"40 s read whole, leading 3, a packet more short", 40, wholeRead(40, frames-4, frames, 3), "not every packet"},
		{"40 s read whole, a packet short", 40, wholeRead(40, frames-1, frames, 0), "not every packet"},
		{"40 s read whole, no leading said", 40, wholeRead(40, frames, frames, -1), "says no leading pictures"},
		{"40 s read whole, leading 16", 40, wholeRead(40, frames-16, frames, 16), ""},
		{"40 s read whole, leading 17", 40, wholeRead(40, frames-17, frames, 17), "says no leading pictures"},
		{"40 s read whole, its frames short of the end", 40, func() GateFacts {
			f := wholeRead(40, frames, frames, 0)
			f.Captions.Windows[0].End = 38
			return f
		}(), "don't reach its picture's start and end"},
		{"40 s read in windows", 40, read(40, window(0, 0, 48, false), window(0.1, 4, 48, false),
			window(0.3, 10, 48, false), window(0.5, 10, 48, false), window(0.7, 10, 48, false), window(0.9, 10, 48, false)),
			"is read whole, every packet decoded, not in windows"},
		{"108.4 s read in windows", 108.4, func() GateFacts {
			var ws []CaptionWindow
			for _, at := range planAts() {
				ws = append(ws, window(at, (Span{End: 108.4}).Place(at), 48, false))
			}
			return read(108.4, ws...)
		}(), ""},
		{"600 s read whole", 600, wholeRead(600, 600*24, 600*24, 0), ""},
		{"a whole read among windows", 600, func() GateFacts {
			f := read(600, every()...)
			w := wholeRead(600, 600*24, 600*24, 0).Captions.Windows[0]
			f.Captions.Windows[0] = w
			return f
		}(), "stands alone"},
		{"a whole read not at the start", 600, func() GateFacts {
			f := wholeRead(600, 600*24, 600*24, 0)
			f.Captions.Windows[0].At = 0.5
			return f
		}(), "one window at its start"},
	} {
		carried, unknown := captionsRule([]Doc{film(c.length)}, planProbes, c.f)
		if carried != "" || !strings.Contains(unknown, c.want) || (c.want == "") != (unknown == "") {
			t.Errorf("%s: carried %q, unknown %q; want %q", c.name, carried, unknown, c.want)
		}
	}
	hevc := film(40)
	streams, _ := hevc.Get("streams")
	streams.([]any)[0] = Doc{{Key: "index", Value: 0}, {Key: "type", Value: "video"}, {Key: "codec", Value: "hevc"},
		{Key: "fieldOrder", Value: "progressive"}}
	if _, unknown := captionsRule([]Doc{hevc}, planProbes, wholeRead(40, frames, frames, 0)); !strings.HasPrefix(unknown,
		CaptionsUnscanned) {
		t.Errorf("an HEVC picture read whole: unknown %q; want its packets' signature needed", unknown)
	}
	f := wholeRead(40, frames, frames, 0)
	f.Captions.Windows[0].Captions = true
	if carried, _ := captionsRule([]Doc{film(40)}, planProbes, f); !strings.HasPrefix(carried, CaptionsCarried) {
		t.Errorf("a whole read that showed captions: carried %q; want them carried", carried)
	}
}

// A read is held to the span of the picture its records give: each end of
// the span it says it read lies within a frame step (of the stored probe's
// frame rate) and half a second of the span's, or it says nothing - a span
// of the same length shifted by 0.6 s too; a read that says no span says
// nothing either.
func TestAReadIsHeldToItsPicturesSpan(t *testing.T) {
	probe := doc(t, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"start_time": "0.000000", "avg_frame_rate": "25/1"}]}`)
	for _, c := range []struct {
		start, end float64
		goes       bool
	}{{0, 600, true}, {0, 600.53, true}, {0, 599.47, true}, {0.53, 600, true}, {-0.53, 600, true}, {0, 600.6, false},
		{0, 599.3, false}, {0.6, 600, false}, {-0.6, 600, false}, {0.6, 600.6, false}, {0, 0, false}} {
		f := read(600, every()...)
		if c.end > c.start {
			f.Captions.Span = &Span{Start: c.start, End: c.end}
		} else {
			f.Captions.Span = nil
		}
		_, unknown := captionsRule([]Doc{film(600)}, []Doc{probe}, f)
		if (unknown == "") != c.goes {
			t.Errorf("a read of a span from %v s to %v s of a 600 s picture of 25 frames a second: unknown %q; want it "+
				"to go %v", c.start, c.end, unknown, c.goes)
		}
	}
}

// The plan's span is the record logic's plan_of, in its order: the stored
// probe's one video stream that is no cover by the closed lists' rule
// (attachedPicture), wherever a cover lies - none, or two that could each be
// it, no plan; its start_time, else the format's, else no plan; its
// duration (above 0), else the record's container.durationMs (a number
// above 0), else the format's duration, else no plan - a Matroska stream's
// DURATION tag none; its frame step one over avg_frame_rate, or
// r_frame_rate where avg_frame_rate is exactly 0/0 ("num/den" in digits
// alone, no space, both above 0, one frame a second at least), else no plan
// - an avg_frame_rate missing or of no ratio takes no r_frame_rate; the
// record's frameRate none, nor a
// 24th of a second. No probe, a probe of no streams:
// no plan.
func TestThePlansSpanIsPlanOfs(t *testing.T) {
	ntsc := 1001.0 / 24000
	for _, c := range []struct {
		name, probe string
		record      Doc
		span        *Span // nil: no plan
		why         string
	}{
		{"start, duration and rate of the picture", `{"streams": [{"codec_type": "video", "start_time": "12.500000",
			"duration": "500.000000", "avg_frame_rate": "25/1"}]}`, film(600), &Span{12.5, 512.5, 0.04}, ""},
		{"the format's start", `{"format": {"start_time": "1.400000"}, "streams": [{"codec_type": "video",
			"avg_frame_rate": "25/1"}]}`, film(600), &Span{1.4, 601.4, 0.04}, ""},
		{"a start that is a number", `{"streams": [{"codec_type": "video", "start_time": 3, "avg_frame_rate": "25/1"}]}`,
			film(600), &Span{3, 603, 0.04}, ""},
		{"no start", `{"format": {"duration": "600.000000"}, "streams": [{"codec_type": "video",
			"avg_frame_rate": "25/1"}]}`, film(600), nil, "no start"},
		{"a start that is a boolean", `{"format": {"start_time": true}, "streams": [{"codec_type": "video",
			"start_time": false, "avg_frame_rate": "25/1"}]}`, film(600), nil, "no start"},
		{"a start that is no number", `{"streams": [{"codec_type": "video", "start_time": "N/A",
			"avg_frame_rate": "25/1"}]}`, film(600), nil, "no start"},
		{"a duration of N/A: the record's", `{"streams": [{"codec_type": "video", "start_time": "0", "duration": "N/A",
			"avg_frame_rate": "25/1"}]}`, film(600), &Span{0, 600, 0.04}, ""},
		{"a duration of 0: the record's", `{"streams": [{"codec_type": "video", "start_time": "0", "duration": "0",
			"avg_frame_rate": "25/1"}]}`, film(600), &Span{0, 600, 0.04}, ""},
		{"a Matroska DURATION tag: the record's", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "25/1", "tags": {"DURATION": "00:09:59.959000000"}}]}`, film(601), &Span{0, 601, 0.04}, ""},
		{"no duration and a record of none", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "25/1"}]}`, Doc{{Key: "container", Value: Doc{{Key: "durationMs", Value: "600000"}}}}, nil,
			"no length"},
		{"the record's length before the format's duration", `{"format": {"duration": "590.000000"}, "streams":
			[{"codec_type": "video", "start_time": "0", "avg_frame_rate": "25/1"}]}`, film(600), &Span{0, 600, 0.04}, ""},
		{"the format's duration where the record gives none", `{"format": {"duration": "590.000000"}, "streams":
			[{"codec_type": "video", "start_time": "0", "avg_frame_rate": "25/1"}]}`,
			Doc{{Key: "container", Value: Doc{{Key: "durationMs", Value: 0}}}}, &Span{0, 590, 0.04}, ""},
		{"avg_frame_rate 0/0: r_frame_rate", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "0/0", "r_frame_rate": "24000/1001"}]}`, film(600), &Span{0, 600, ntsc}, ""},
		{"avg_frame_rate 0/0, r_frame_rate spaced: no ratio of digits alone", `{"streams": [{"codec_type": "video",
			"start_time": "0", "avg_frame_rate": "0/0", "r_frame_rate": " 24000 / 1001 "}]}`, film(600), nil, "no frame rate"},
		{"no avg_frame_rate: r_frame_rate not taken", `{"streams": [{"codec_type": "video", "start_time": "0",
			"r_frame_rate": "24/1"}]}`, film(600), nil, "no frame rate"},
		{"an avg_frame_rate of no ratio: r_frame_rate not taken", `{"streams": [{"codec_type": "video",
			"start_time": "0", "avg_frame_rate": "N/A", "r_frame_rate": "24/1"}]}`, film(600), nil, "no frame rate"},
		{"an avg_frame_rate of 0/1: r_frame_rate not taken", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "0/1", "r_frame_rate": "24/1"}]}`, film(600), nil, "no frame rate"},
		{"an avg_frame_rate of a number: r_frame_rate not taken", `{"streams": [{"codec_type": "video",
			"start_time": "0", "avg_frame_rate": 24, "r_frame_rate": "24/1"}]}`, film(600), nil, "no frame rate"},
		{"rates of 0/0 both", `{"streams": [{"codec_type": "video", "start_time": "0", "avg_frame_rate": "0/0",
			"r_frame_rate": "0/0"}]}`, film(600), nil, "no frame rate"},
		{"a rate of no fraction", `{"streams": [{"codec_type": "video", "start_time": "0", "avg_frame_rate": "25",
			"r_frame_rate": 25}]}`, film(600), nil, "no frame rate"},
		{"a rate below one frame a second", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "1/2", "r_frame_rate": "25/1"}]}`, film(600), nil, "no frame rate"},
		{"a rate of one frame a second", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "1/1"}]}`, film(600), &Span{0, 600, 1}, ""},
		{"avg_frame_rate 0/0, r_frame_rate below one a second", `{"streams": [{"codec_type": "video", "start_time": "0",
			"avg_frame_rate": "0/0", "r_frame_rate": "2/3"}]}`, film(600), nil, "no frame rate"},
		{"no rate, the record's frameRate", `{"streams": [{"codec_type": "video", "start_time": "0"}]}`,
			Doc{{Key: "container", Value: Doc{{Key: "durationMs", Value: 600000}}}, {Key: "streams", Value: []any{
				Doc{{Key: "index", Value: 0}, {Key: "type", Value: "video"}, {Key: "frameRate", Value: 25}}}}}, nil,
			"no frame rate"},
		{"an attached picture first", `{"streams": [{"codec_type": "video", "codec_name": "mjpeg", "start_time": "5",
			"disposition": {"attached_pic": 1}}, {"codec_type": "video", "start_time": "0", "avg_frame_rate": "25/1"}]}`,
			film(600), &Span{0, 600, 0.04}, ""},
		{"attached_pic \"0\", no mark: two films", `{"streams": [{"codec_type": "video", "codec_name": "png",
			"start_time": "5", "avg_frame_rate": "25/1", "disposition": {"attached_pic": "0"}}, {"codec_type": "video",
			"start_time": "0", "avg_frame_rate": "25/1"}]}`, film(600), nil, "2 video streams"},
		{"attached_pic 0", `{"streams": [{"codec_type": "video", "codec_name": "mjpeg", "start_time": "5",
			"avg_frame_rate": "25/1", "disposition": {"attached_pic": 0}}]}`, film(600), &Span{5, 605, 0.04}, ""},
		{"attached_pic of a film's codec: two films", `{"streams": [{"codec_type": "video", "codec_name": "h264",
			"start_time": "5", "avg_frame_rate": "25/1", "disposition": {"attached_pic": 1}}, {"codec_type": "video",
			"start_time": "0", "avg_frame_rate": "25/1"}]}`, film(600), nil, "2 video streams"},
		{"no picture", `{"streams": [{"codec_type": "audio", "start_time": "0"}]}`, film(600), nil, "no picture"},
		{"a cover marked by its tags alone before the film", `{"streams": [{"codec_type": "video", "codec_name": "mjpeg",
			"start_time": "5", "avg_frame_rate": "25/1", "tags": {"filename": "c.jpg", "mimetype": "image/jpeg"}},
			{"codec_type": "video", "codec_name": "h264", "start_time": "0", "avg_frame_rate": "25/1"}]}`, film(600),
			&Span{0, 600, 0.04}, ""},
		{"two films", `{"streams": [{"codec_type": "video", "codec_name": "h264", "start_time": "0",
			"avg_frame_rate": "25/1"}, {"codec_type": "video", "codec_name": "h264", "start_time": "0",
			"avg_frame_rate": "25/1"}]}`, film(600), nil, "2 video streams"},
		{"no streams", `{"format": {"start_time": "0"}}`, film(600), nil, "no streams"},
	} {
		s, why := PictureSpan(c.record, doc(t, c.probe))
		switch {
		case c.span == nil && (s != nil || !strings.Contains(why, c.why)):
			t.Errorf("%s: %+v (%s); want no plan, %q", c.name, s, why, c.why)
		case c.span != nil && (s == nil || math.Abs(s.Start-c.span.Start) > 1e-9 || math.Abs(s.End-c.span.End) > 1e-9 ||
			math.Abs(s.Step-c.span.Step) > 1e-12):
			t.Errorf("%s: %+v (%s); want %+v", c.name, s, why, *c.span)
		}
	}
	if s, why := PictureSpan(film(600), nil); s != nil || !strings.Contains(why, "no stored probe") {
		t.Errorf("no stored probe: %+v (%s); want no plan", s, why)
	}
	// the rule: no plan keeps the original, a read of it not known, and no
	// read lets it go (not one the backfill hands out first)
	if _, unknown := captionsRule([]Doc{film(600)}, nil, read(600, every()...)); !strings.HasPrefix(unknown,
		CaptionsNoPlan+": it has no stored probe") {
		t.Errorf("a read of an original of no stored probe: unknown %q; want no plan", unknown)
	}
	if g := (Gate{captionsUnknown: CaptionsNoPlan + ": x", Unknown: CaptionsNoPlan + ": x"}); g.OnlyCaptionsUnknown() {
		t.Error("an original of no plan: a read is waited on; want none")
	}
}

// A stored probe that marks its picture as captions (the disposition
// captions) carries them, as its closed_captions does.
func TestAProbesCaptionsDispositionOfItsPictureIsCarried(t *testing.T) {
	probe := doc(t, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"disposition": {"captions": 1}}]}`)
	if carried, _ := captionsRule([]Doc{film(600)}, []Doc{probe}, read(600, every()...)); !strings.HasPrefix(carried,
		CaptionsCarried) {
		t.Errorf("a picture its probe marks as captions: carried %q; want them carried", carried)
	}
}

// On the retire path: a short original read whole is deleted, one read in
// windows, or read whole short of a packet, is kept; an original read with
// another span than its picture's is kept; a probe that marks the picture as
// captions keeps it for good; a read that found its picture's captions can't
// be told by windows (unscanned) keeps it as unscanned.
func TestTheRetireJobKnowsThePlansDetails(t *testing.T) {
	short := func(rec map[string]any) { rec["container"].(map[string]any)["durationMs"] = 40000 }
	lasting := func(ms int) func(rec map[string]any) {
		return func(rec map[string]any) { rec["container"].(map[string]any)["durationMs"] = ms }
	}
	whole := func(frames int) string {
		return `[{"at": 0, "start": 0, "end": 39.958, "frames": ` + strconv.Itoa(frames) + `, "packets": 960, "leading": 0,
			"captions": false, "interlacedFrames": 0, "repeatFrames": 0, "whole": true}]`
	}
	for _, c := range []struct {
		name, windows string
		duration      float64
		span          [2]float64
		record        func(map[string]any)
		probe         *string
		finding       string
		kept          string // what the source's error holds; "" deleted
	}{
		{name: "40 s read whole", windows: whole(960), duration: 40, span: [2]float64{0, 40}, record: short},
		{name: "40 s read whole, leading 3", windows: strings.Replace(whole(957), `"leading": 0`, `"leading": 3`, 1),
			duration: 40, span: [2]float64{0, 40}, record: short},
		{name: "40 s read whole, no leading said", windows: strings.Replace(whole(960), ` "leading": 0,`, ``, 1),
			duration: 40, span: [2]float64{0, 40}, record: short, kept: "says no leading pictures"},
		{name: "40 s read whole, a packet short", windows: whole(959), duration: 40, span: [2]float64{0, 40},
			record: short, kept: "not every packet"},
		{name: "40 s read in windows", windows: `[{"at": 0, "start": 0, "end": 1.9, "frames": 48, "captions": false,
			"interlacedFrames": 0, "repeatFrames": 0}, {"at": 0.1, "start": 4, "end": 5.9, "frames": 48, "captions": false,
			"interlacedFrames": 0, "repeatFrames": 0}, {"at": 0.25, "start": 10, "end": 11.9, "frames": 48, "captions": false,
			"interlacedFrames": 0, "repeatFrames": 0}]`, duration: 40, span: [2]float64{0, 40}, record: short,
			kept: "is read whole"},
		{name: "600 s read with a span to 590 s", windows: noCaptionsWindows, duration: 600, span: [2]float64{0, 590},
			kept: "it read a picture from 0 s to 590 s"},
		{name: "600 s read with its span shifted by 1 s", windows: noCaptionsWindows, duration: 600,
			span: [2]float64{1, 601}, kept: "it read a picture from 1 s to 601 s"},
		{name: "a probe that marks the picture as captions", windows: noCaptionsWindows, duration: 600,
			span: [2]float64{0, 600},
			probe: ptr(`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
				"disposition": {"captions": 1}}]}`), kept: "marks its picture as captions"},
		{name: "100 s read in its six windows", windows: planWindows(100, nil, -1), duration: 100,
			span: [2]float64{0, 100}, record: lasting(100000), kept: "is read whole"},
		{name: "120 s read in its six windows", windows: planWindows(120, nil, -1), duration: 120,
			span: [2]float64{0, 120}, record: lasting(120000)},
		{name: "600 s read in five windows", windows: planWindows(600, nil, 0.3), duration: 600,
			span: [2]float64{0, 600}, kept: "it has no window at 30%"},
		{name: "200 s, the tail window at its fraction", windows: planWindows(200, nil, -1), duration: 200,
			span: [2]float64{0, 200}, record: lasting(200000)},
		{name: "200 s, the tail window said where it lies", windows: planWindows(200, map[float64][2]float64{0.9: {170,
			0.85}}, -1), duration: 200, span: [2]float64{0, 200}, record: lasting(200000),
			kept: "its windows at 85% are at no fraction of the plan's"},
		{name: "a read that found its picture unscanned", duration: 600, finding: FindingUnscanned,
			kept: CaptionsUnscanned},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			if c.record != nil || c.probe != nil {
				change := c.record
				if change == nil {
					change = func(map[string]any) {}
				}
				f.rewriteRecord(t, change, c.probe)
			}
			if c.finding != "" {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET state = 'failed', captionwindows = NULL,
					captions = NULL, duration = NULL, readspanstart = NULL, readspanend = NULL, finding = $2, reason = $3,
					sourcerecordedat = s.recordedat FROM com_nalet_katalog_itemsources s
					WHERE s.id = com_nalet_katalog_sourcecaptions.source_id AND source_id = $1`, rtSource, c.finding,
					UnscannedReason)
			} else {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2::jsonb, duration = $3,
					readreport = NULL,
					readspanstart = $4, readspanend = $5 WHERE source_id = $1`, rtSource, c.windows, c.duration, c.span[0],
					c.span[1])
			}
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
}

// planWindows are the stored windows of a read of the plan's six windows of
// a picture of length seconds, each at its fraction and its place
// (Span.Place), those at ats said at the place given there (start, at), the
// one at drop left out (-1: none).
func planWindows(length float64, said map[float64][2]float64, drop float64) string {
	sp := Span{End: length}
	var ws []string
	for _, at := range planAts() {
		if at == drop {
			continue
		}
		start, a := sp.Place(at), at
		if s, ok := said[at]; ok {
			start, a = s[0], s[1]
		}
		ws = append(ws, `{"at": `+ftoa(a)+`, "start": `+ftoa(start)+`, "end": `+ftoa(start+1.88)+`, "frames": 48, `+
			`"captions": false, "interlacedFrames": 0, "repeatFrames": 0}`)
	}
	return "[" + strings.Join(ws, ", ") + "]"
}

// On the retire path the plan is plan_of's: an original of no stored probe,
// of a probe that gives no start or no frame rate, is kept, no read of it
// known; one whose probe gives its rate in r_frame_rate goes; a Matroska
// DURATION tag places nothing (the record's length does).
func TestTheRetireJobPlacesByPlanOf(t *testing.T) {
	probe := func(js string) *string { return &js }
	for _, c := range []struct {
		name  string
		probe *string
		kept  string // what the source's error holds; "" deleted
	}{
		{"no stored probe", probe(""), CaptionsNoPlan + ": it has no stored probe"},
		{"no start", probe(`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264",
			"field_order": "progressive", "avg_frame_rate": "24/1"}]}`), CaptionsNoPlan + ": its stored probe gives no start"},
		{"rates of 0/0", probe(`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264",
			"field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "0/0", "r_frame_rate": "0/0"}]}`),
			CaptionsNoPlan + ": its stored probe gives no frame rate"},
		{"no avg_frame_rate, r_frame_rate 24/1", probe(`{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "r_frame_rate": "24/1"}]}`),
			CaptionsNoPlan + ": its stored probe gives no frame rate"},
		{"avg_frame_rate 0/0, r_frame_rate 24/1", probe(`{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "0/0",
			"r_frame_rate": "24/1"}]}`), ""},
		{"a Matroska DURATION tag of 590 s", probe(`{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "24/1",
			"tags": {"DURATION": "00:09:50.000000000"}}]}`), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewriteRecord(t, func(map[string]any) {}, c.probe)
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
	// a read kept as the finding noPlan holds the original for that file and
	// that record, a probe that gives a plan written beside it since or not
	t.Run("a read kept as the finding noPlan", func(t *testing.T) {
		f := newRetireFixtureOf(t, sameEssence, sameEssence)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET state = 'failed', captionwindows = NULL,
			captions = NULL, duration = NULL, readspanstart = NULL, readspanend = NULL, finding = $2, reason = $3,
			sourcerecordedat = s.recordedat FROM com_nalet_katalog_itemsources s
			WHERE s.id = com_nalet_katalog_sourcecaptions.source_id AND source_id = $1`, rtSource, FindingNoPlan,
			"its records gave no plan of its picture when it was handed out")
		f.keptFor(t, "a read kept as the finding noPlan", CaptionsNoPlan+" (as they gave none when its original was "+
			"handed out to be read)")
	})
}
