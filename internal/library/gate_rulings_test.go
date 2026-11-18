package library

import (
	"math"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// filmProbe is a stored probe of one picture, h264 or the codec codec, of
// the field order order, at 0 and 24 frames a second, its disposition disp
// (an object's fields, or "").
func filmProbe(codec, order, disp string) string {
	d := ""
	if disp != "" {
		d = `, "disposition": {` + disp + `}`
	}
	return `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "` + codec + `", "field_order": "` + order +
		`", "start_time": "0.000000", "avg_frame_rate": "24/1"` + d + `}]}`
}

// The codec and the field order are the stored probe's film's (by the cover
// rule) as well as the record's picture's (its first video stream not
// marked an attached picture); where the record's picture is not that
// stream, its closed captions and its fields are both kept: the probe's
// film hevc against the record's h264 needs its packets' signature; a record
// of no video stream, or whose picture is marked an attached picture, or of
// another index than the film's keeps both; one h264 stream of field order
// tt marked an attached picture in both is in fields (the cover rule takes
// it for the film) and not the record's picture.
func TestTheRecordsPictureIsTheProbesFilm(t *testing.T) {
	video := func(index int, codec, order string, attached bool) map[string]any {
		return map[string]any{"index": index, "type": "video", "codec": codec, "fieldOrder": order,
			"dispositions": map[string]any{"attachedPic": attached}}
	}
	for _, c := range []struct {
		name          string
		streams       []any
		probe         string
		unknown, scan string // what each begins with; "" none
	}{
		{"the same stream", []any{video(0, "h264", "progressive", false)}, filmProbe("h264", "progressive", ""), "", ""},
		{"the probe's film hevc, the record's picture h264", []any{video(0, "h264", "progressive", false)},
			filmProbe("hevc", "progressive", ""), CaptionsUnscanned, ""},
		{"a record of no video stream", []any{map[string]any{"index": 1, "type": "audio", "codec": "aac"}},
			filmProbe("h264", "progressive", ""), CaptionsUnknown + ": " + PictureNotFilm, FieldsKept + ": " + PictureNotFilm},
		{"a record picture marked an attached picture", []any{video(0, "h264", "progressive", true)},
			filmProbe("h264", "progressive", ""), CaptionsUnknown + ": " + PictureNotFilm, FieldsKept + ": " + PictureNotFilm},
		{"a record picture of another index", []any{video(1, "h264", "progressive", false)},
			filmProbe("h264", "progressive", ""), CaptionsUnknown + ": " + PictureNotFilm, FieldsKept + ": " + PictureNotFilm},
		{"one h264 stream of field order tt, an attached picture in both", []any{video(0, "h264", "tt", true)},
			filmProbe("h264", "tt", `"attached_pic": 1`), CaptionsUnknown + ": " + PictureNotFilm, FieldsKept + ": its record's field order is tt"},
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", c.streams, c.probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		switch {
		case err != nil:
			t.Errorf("%s: %v", c.name, err)
		case !strings.HasPrefix(g.Unknown, c.unknown) || (c.unknown == "") != (g.Unknown == ""):
			t.Errorf("%s: unknown %q; want it to begin %q", c.name, g.Unknown, c.unknown)
		case !strings.HasPrefix(g.Scan, c.scan) || (c.scan == "") != (g.Scan == ""):
			t.Errorf("%s: scan %q; want it to begin %q", c.name, g.Scan, c.scan)
		}
	}
	if g := (Gate{captionsUnknown: CaptionsUnknown + ": " + PictureNotFilm + ": x",
		Unknown: CaptionsUnknown + ": " + PictureNotFilm + ": x"}); g.OnlyCaptionsUnknown() {
		t.Error("a record's picture that is not the film: a read is waited on; want none")
	}
}

// Numbers that are not finite (NaN, an infinity) count as not stated: a read
// of such a length, span or gop says nothing; a gop longer than the first
// 1200 packets last at the frame step can't be one; a read of the whole
// picture needs packets of at least its span at the frame step.
func TestNumbersThatCantBeAndAGopTooLongTellNothing(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	for _, c := range []struct {
		name   string
		change func(f GateFacts) GateFacts
		want   string // what the why holds; "" it goes
	}{
		{"a length of NaN", func(f GateFacts) GateFacts { f.Captions.Duration = &nan; return f }, "says no length"},
		{"a span ending at an infinity", func(f GateFacts) GateFacts { f.Captions.Span.End = inf; return f },
			"says no span"},
		{"a span starting at NaN", func(f GateFacts) GateFacts { f.Captions.Span.Start = nan; return f }, "says no span"},
		{"a gop of NaN", func(f GateFacts) GateFacts { f.Captions.Gop = &nan; return f }, "its gop is no length"},
		{"a gop of an infinity", func(f GateFacts) GateFacts { f.Captions.Gop = &inf; return f }, "its gop is no length"},
		{"a gop of 50 s, 1200 packets at 24 a second", func(f GateFacts) GateFacts { g := 50.0; f.Captions.Gop = &g; return f },
			""},
		{"a gop of 50.1 s", func(f GateFacts) GateFacts { g := 50.1; f.Captions.Gop = &g; return f }, "is longer than the 1200"},
	} {
		_, unknown := captionsRule([]Doc{film(600)}, planProbes, c.change(read(600, every()...)))
		if !strings.Contains(unknown, c.want) || (c.want == "") != (unknown == "") {
			t.Errorf("%s: unknown %q; want %q", c.name, unknown, c.want)
		}
	}
	// 40 s at 24 frames a second: 960 packets; within a frame step and 0.5 s
	// (39.46 s), 948 are enough and 946 are not
	for _, c := range []struct {
		packets int
		want    string
	}{{960, ""}, {948, ""}, {946, "fewer than its picture's span"}} {
		_, unknown := captionsRule([]Doc{film(40)}, planProbes, wholeRead(40, c.packets, c.packets, 0))
		if !strings.Contains(unknown, c.want) || (c.want == "") != (unknown == "") {
			t.Errorf("a whole read of %d packets of 40 s: unknown %q; want %q", c.packets, unknown, c.want)
		}
	}
}

// On the retire path: a record whose picture is marked an attached picture,
// and a probe's film hevc against the record's h264, keep the original; a
// stored read whose span ends at NaN, whose gop is an infinity or longer
// than 1200 packets at the frame step keeps it; so does a whole read of too
// few packets for its span; a probe that states a rate below one frame a
// second gives no plan; a cover whose disposition says "0" is no cover, so
// two films and no plan.
func TestTheRetireJobKeepsWhatTheSecondLookRuled(t *testing.T) {
	stored := func(set string) func(f *retireFixture) {
		return func(f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET `+set+` WHERE source_id = $1`, rtSource)
		}
	}
	record := func(change func(rec map[string]any), probe string) func(f *retireFixture) {
		return func(f *retireFixture) { f.rewriteRecord(t, change, &probe) }
	}
	same := func(map[string]any) {}
	for _, c := range []struct {
		name   string
		change func(f *retireFixture)
		kept   string
	}{
		{"a record picture marked an attached picture", record(func(rec map[string]any) {
			rec["streams"].([]any)[0].(map[string]any)["dispositions"] = map[string]any{"attachedPic": true}
		}, progressiveProbe), CaptionsUnknown + ": " + PictureNotFilm},
		{"the probe's film hevc, the record's picture h264", record(same, filmProbe("hevc", "progressive", "")),
			CaptionsUnscanned},
		{"a span ending at NaN", stored(`readspanend = 'NaN'::float8`), "says no span"},
		{"a gop of an infinity", stored(`readgop = 'Infinity'::float8`), "its gop is no length"},
		{"a gop of 51 s", stored(`readgop = 51`), "is longer than the 1200"},
		{"a rate below one frame a second", record(same, `{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "1/2"}]}`),
			CaptionsNoPlan + ": its stored probe gives no frame rate"},
		{"a cover marked \"0\" before the film", record(func(rec map[string]any) {
			rec["streams"] = []any{map[string]any{"index": 0, "type": "video", "codec": "mjpeg"},
				map[string]any{"index": 1, "type": "video", "codec": "h264", "fieldOrder": "progressive"}}
		}, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": "0"}},
			{"index": 1, "codec_type": "video", "codec_name": "h264", "field_order": "progressive", "start_time": "0.000000",
			"avg_frame_rate": "24/1"}]}`), "video-streams:2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			c.change(f)
			f.keptFor(t, c.name, c.kept)
		})
	}
	// a whole read of too few packets for its span
	t.Run("a whole read of too few packets", func(t *testing.T) {
		f := newRetireFixtureOf(t, sameEssence, sameEssence)
		f.rewriteRecord(t, func(rec map[string]any) { rec["container"].(map[string]any)["durationMs"] = 40000 }, nil)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2::jsonb, duration = 40,
			readspanstart = 0, readspanend = 40 WHERE source_id = $1`, rtSource, `[{"at": 0, "start": 0, "end": 39.958,
			"frames": 900, "packets": 900, "leading": 0, "captions": false, "interlacedFrames": 0, "repeatFrames": 0,
			"whole": true}]`)
		f.keptFor(t, "a whole read of too few packets", "fewer than its picture's span")
	})
}
