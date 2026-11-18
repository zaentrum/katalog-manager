package library

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// What the schemas' record logic 11 rules at its merge (71999959), held
// here as the record logic and the validator's gate hold it.

// rateProbe is a stored probe of one picture at 0 whose avg_frame_rate is
// avg (a JSON value), and r_frame_rate r where r is not "".
func rateProbe(avg, r string) string {
	rr := ""
	if r != "" {
		rr = `, "r_frame_rate": ` + r
	}
	return `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "start_time": "0",
		"avg_frame_rate": ` + avg + rr + `}]}`
}

// A frame rate is a ratio "num/den" of digits alone: no space, sign or
// point, no digit of another script; r_frame_rate stands in for an
// avg_frame_rate of exactly 0/0 alone. Its digits are as many as Python
// reads, 4300 at most, leading zeros among them; its step is den/num as
// Python divides, the nearest float, so a rate a hair below one frame a
// second whose step rounds to 1 is taken, as Python takes it, and one whose
// step is past a float gives no plan.
func TestARateIsARatioOfDigitsAlone(t *testing.T) {
	zeros := func(n int) string { return strings.Repeat("0", n) }
	for _, c := range []struct {
		name, avg, r string
		step         float64 // 0: no plan
	}{
		{"24/1", `"24/1"`, "", 1.0 / 24},
		{"leading zeros", `"0024/0001"`, "", 1.0 / 24},
		{"a space before", `" 24/1"`, "", 0},
		{"spaces about the slash", `"24 / 1"`, "", 0},
		{"a space after", `"24/1 "`, "", 0},
		{"a sign", `"+24/1"`, "", 0},
		{"a point", `"24.0/1"`, "", 0},
		{"a point in its den", `"24/1.0"`, "", 0},
		{"a den of 0", `"24/0"`, "", 0},
		{"a num of 0", `"0/1"`, "", 0},
		{"digits of another script", `"\u0662\u0664/\u0661"`, "", 0},
		{"0/0, r_frame_rate spaced", `"0/0"`, `" 24/1"`, 0},
		{"0/0, r_frame_rate signed", `"0/0"`, `"+24/1"`, 0},
		{"0/0 spaced: no r_frame_rate", `" 0/0"`, `"24/1"`, 0},
		{"30 a second in thirty digits", `"3` + zeros(29) + `/1` + zeros(28) + `"`, "", 1.0 / 30},
		{"10 a second in 4300 digits", `"1` + zeros(4299) + `/1` + zeros(4298) + `"`, "", 0.1},
		{"4301 digits: more than Python reads", `"1` + zeros(4300) + `/1` + zeros(4299) + `"`, "", 0},
		{"a hair below one a second, its step 1.0", `"100000000000000000000/100000000000000000001"`, "", 1},
		{"a step past a float", `"1/1` + zeros(400) + `"`, "", 0},
	} {
		s, why := PictureSpan(film(600), doc(t, rateProbe(c.avg, c.r)))
		switch {
		case c.step == 0 && (s != nil || !strings.Contains(why, "no frame rate")):
			t.Errorf("%s: %+v (%s); want no plan", c.name, s, why)
		case c.step != 0 && (s == nil || s.Step != c.step):
			t.Errorf("%s: %+v (%s); want a step of %v", c.name, s, why, c.step)
		}
	}
}

// The gop's ceiling, 1200 packets at the frame step, holds every read, a
// read of the whole picture too: one exactly at it is taken, one a hair
// above it says nothing.
func TestTheGopsCeilingHoldsAWholeReadToo(t *testing.T) {
	step := 1.0 / 24
	ceiling := float64(GopPackets) * step
	for _, c := range []struct {
		name string
		gop  float64
		want string
	}{
		{"exactly at the ceiling", ceiling, ""},
		{"a hair above it", math.Nextafter(ceiling, math.Inf(1)), "is longer than the 1200"},
		{"50.1 s", 50.1, "is longer than the 1200"},
	} {
		f := wholeRead(40, 960, 960, 0)
		g := c.gop
		f.Captions.Gop = &g
		_, unknown := captionsRule([]Doc{film(40)}, planProbes, f)
		if !strings.Contains(unknown, c.want) || (c.want == "") != (unknown == "") {
			t.Errorf("a whole read of 40 s, its gop %s: unknown %q; want %q", c.name, unknown, c.want)
		}
	}
}

// Cover tags are read without case: two names that differ only by it and
// give values Python takes for unequal ("1" and 1, two texts) are there,
// and make the stream no image, so no cover, whatever they name; values
// Python takes for equal (1 and 1.0, true and 1, null and null, lists and
// objects item by item) clash not. A capital I with a dot above lowers as
// Python lowers it, to an i and a combining dot: no filename, no image/*.
func TestTagsThatClashByCaseMakeNoCover(t *testing.T) {
	pic := `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, `
	for _, c := range []struct {
		name, stream string
		cover        bool
	}{
		{"a mimetype in two cases, unlike", `"codec_name": "mjpeg", "tags": {"filename": "c.jpg", "mimetype": "image/jpeg",
			"MIMETYPE": "video/mp4"}`, false},
		{"a filename in two cases, alike", `"codec_name": "mjpeg", "tags": {"filename": "c.jpg", "FILENAME": "c.jpg",
			"mimetype": "image/jpeg"}`, true},
		{"a title in two cases, unlike", `"codec_name": "mjpeg", "tags": {"filename": "c.jpg", "mimetype": "image/jpeg",
			"title": "a", "Title": "b"}`, false},
		{"a count as text and as a number", pic + `"tags": {"NUMBER_OF_FRAMES": "1", "number_of_frames": 1}`, false},
		{"1 and 1.0", pic + `"tags": {"x": 1, "X": 1.0}`, true},
		{"true and 1", pic + `"tags": {"x": true, "X": 1}`, true},
		{"null and null", pic + `"tags": {"x": null, "X": null}`, true},
		{"lists alike", pic + `"tags": {"x": [1, "a"], "X": [1.0, "a"]}`, true},
		{"lists unlike", pic + `"tags": {"x": [1, "a"], "X": [1, "b"]}`, false},
		{"lists of two lengths", pic + `"tags": {"x": [1], "X": [1, 1]}`, false},
		{"lists of two lengths, the longer first", pic + `"tags": {"x": [1, 1], "X": [1]}`, false},
		{"objects alike", pic + `"tags": {"x": {"a": 1, "b": null}, "X": {"b": null, "a": 1.0}}`, true},
		{"objects unlike", pic + `"tags": {"x": {"a": 1}, "X": {"a": "1"}}`, false},
		{"a filename of a dotted capital I", `"codec_name": "mjpeg", "tags": {"F\u0130LENAME": "c.jpg",
			"mimetype": "image/jpeg"}`, false},
		{"a mimetype of a dotted capital I", pic + `"tags": {"mimetype": "\u0130MAGE/JPEG"}`, false},
	} {
		st := doc(t, `{"index": 0, "codec_type": "video", `+c.stream+`}`)
		if got := probeCover(st); got != c.cover {
			t.Errorf("%s: a cover %v; want %v", c.name, got, c.cover)
		}
		if vs := viewsOf(doc(t, `{"streams": [{"index": 0, "codec_type": "video", `+c.stream+`}]}`), true, nil); len(vs) != 1 ||
			isCover(vs[0]) != c.cover {
			t.Errorf("%s: the closing rule's view a cover %v; want %v", c.name, !c.cover, c.cover)
		}
	}
	// a source record's stream read with its stored probe's tags: the
	// probe's clash holds there too
	rec := doc(t, `{"streams": [{"index": 0, "type": "video", "codec": "mjpeg", "dispositions": {"attachedPic": true}}]}`)
	probe := doc(t, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "tags": {"filename": "c.jpg",
		"mimetype": "image/jpeg", "MimeType": "image/png"}}]}`)
	if vs := viewsOf(rec, false, probe); len(vs) != 1 || isCover(vs[0]) {
		t.Errorf("a record's stream whose probe's tags clash: %+v; want no cover", vs)
	}
}

// On the retire path: a cover before the film whose tags clash by case is
// no cover, so a second video stream: the original is kept.
func TestTheRetireJobTakesACoverOfClashingTagsForAVideoStream(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "tags": {"filename": "cover.jpg",
		"mimetype": "image/jpeg", "MIMETYPE": "image/png"}}, {"index": 1, "codec_type": "video", "codec_name": "h264",
		"field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "24/1"}]}`
	f.rewriteRecord(t, func(rec map[string]any) {
		rec["streams"] = []any{map[string]any{"index": 0, "type": "video", "codec": "mjpeg"},
			map[string]any{"index": 1, "type": "video", "codec": "h264", "fieldOrder": "progressive"}}
	}, &probe)
	f.keptFor(t, "a cover of clashing tags", "video-streams:2")
}

// A count of a cover's frames is a whole number in digits alone, at most
// one: "1.0" and 1.0 can't be read and make no cover; "01", 0 and 1 are
// counts; a run of digits longer than Python reads is none; a whole number
// past one is one past one, however long.
func TestACoversCountIsInDigitsAlone(t *testing.T) {
	pic := `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, `
	for _, c := range []struct {
		name, stream string
		cover        bool
	}{
		{"nb_frames \"1.0\"", pic + `"nb_frames": "1.0"`, false},
		{"nb_frames 1.0, a number", pic + `"nb_frames": 1.0`, false},
		{"nb_frames 1, a number", pic + `"nb_frames": 1`, true},
		{"nb_frames 0, a number", pic + `"nb_frames": 0`, true},
		{"nb_frames \"01\"", pic + `"nb_frames": "01"`, true},
		{"nb_frames \"+1\", a sign", pic + `"nb_frames": "+1"`, false},
		{"nb_frames \"-0\", a sign", pic + `"nb_frames": "-0"`, false},
		{"nb_frames -1, a number below 0", pic + `"nb_frames": -1`, false},
		{"nb_frames -0, a number", pic + `"nb_frames": -0`, true},
		{"nb_frames 1 in 4300 digits", pic + `"nb_frames": "` + strings.Repeat("0", 4299) + `1"`, true},
		{"nb_frames 1 in 4301 digits", pic + `"nb_frames": "` + strings.Repeat("0", 4300) + `1"`, false},
		{"nb_frames of thirty digits", pic + `"nb_frames": 1` + strings.Repeat("0", 29), false},
		{"NUMBER_OF_FRAMES \"1.0\"", pic + `"tags": {"NUMBER_OF_FRAMES": "1.0"}`, false},
		{"an empty mimetype, the disposition", pic + `"tags": {"filename": "c.jpg", "mimetype": "  "}`, false},
		{"a mimetype of null, the disposition", pic + `"tags": {"mimetype": null}`, false},
	} {
		if got := probeCover(doc(t, `{"index": 0, "codec_type": "video", `+c.stream+`}`)); got != c.cover {
			t.Errorf("%s: a cover %v; want %v", c.name, got, c.cover)
		}
	}
}

// A whole number too large for a float counts as not stated, never a
// crash: a record's container.durationMs of 10^400 says no length (and the
// plan takes the format's duration), a picture's start or duration of it
// is none (the format's start, the record's length). A run of digits past
// what Python reads is no count.
func TestAWholeNumberPastAFloatIsNotStated(t *testing.T) {
	huge := "1" + strings.Repeat("0", 400)
	rec := doc(t, `{"container": {"format": "matroska,webm", "durationMs": `+huge+`}, "streams": [{"index": 0,
		"type": "video", "codec": "h264", "fieldOrder": "progressive"}]}`)
	if _, ok := RecordedLengthMs(rec); ok {
		t.Error("a record's durationMs of 10^400: a length; want none")
	}
	s, why := PictureSpan(rec, doc(t, `{"format": {"duration": "590.000000"}, "streams": [{"index": 0, "codec_type": "video",
		"start_time": "0", "avg_frame_rate": "25/1"}]}`))
	if s == nil || s.End != 590 {
		t.Errorf("a record's durationMs of 10^400: %+v (%s); want the format's duration", s, why)
	}
	if _, unknown := captionsRule([]Doc{rec}, planProbes, read(600, every()...)); !strings.HasPrefix(unknown, CaptionsNoLength) {
		t.Errorf("a record's durationMs of 10^400: unknown %q; want no length", unknown)
	}
	s, why = PictureSpan(film(600), doc(t, `{"format": {"start_time": "2"}, "streams": [{"index": 0, "codec_type": "video",
		"start_time": `+huge+`, "duration": `+huge+`, "avg_frame_rate": "25/1"}]}`))
	if s == nil || s.Start != 2 || s.End != 602 {
		t.Errorf("a picture's start and duration of 10^400: %+v (%s); want the format's start, the record's length", s, why)
	}
	var w CaptionWindow
	if err := w.UnmarshalJSON([]byte(`{"at": 0, "start": ` + huge + `, "end": 2, "frames": 48, "captions": false}`)); err != nil ||
		w.Unreadable == "" || w.Frames != 0 {
		t.Errorf("a window starting at 10^400: %+v, %v; want a failed one", w, err)
	}
	if _, ok := gateDigits(strings.Repeat("1", 4301)); ok {
		t.Error("a count of 4301 digits: read; want none")
	}
}

// The loader refuses, as the schemas' loaders do, a record that gives a
// number no reader holds: one written past a float (1e999, -1e999, as
// Infinity), or a whole number longer than Python reads; 1e-999 (0) and
// 4300 digits are read. At the gate a record so written can't be read, and
// a stored probe so written keeps the original in fields with no plan.
func TestALoaderRefusesANumberNoReaderHolds(t *testing.T) {
	for _, c := range []struct {
		name, value string
		read        bool
	}{
		{"1e999", "1e999", false},
		{"-1e999", "-1e999", false},
		{"1.5E+999", "1.5E+999", false},
		{"1e-999", "1e-999", true},
		{"4301 digits", "1" + strings.Repeat("0", 4300), false},
		{"-4301 digits", "-1" + strings.Repeat("0", 4300), false},
		{"-4300 digits", "-1" + strings.Repeat("0", 4299), true},
		{"4300 digits", "1" + strings.Repeat("0", 4299), true},
	} {
		p := filepath.Join(t.TempDir(), "source.json")
		if err := os.WriteFile(p, []byte(`{"container": {"durationMs": [`+c.value+`]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readRecordDoc(p); (err == nil) != c.read {
			t.Errorf("a record giving %s: %v; want it read %v", c.name, err, c.read)
		}
	}
	d := t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), "")
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	b, _ := os.ReadFile(filepath.Join(SourceDir(d, "a"), "source.json"))
	librarytest.Write(t, filepath.Join(SourceDir(d, "a"), "source.json"),
		[]byte(strings.Replace(string(b), `"durationMs": 600000`, `"durationMs": 1e999`, 1)))
	if _, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err == nil || !strings.Contains(err.Error(), "past what a float") {
		t.Errorf("a record giving 1e999: %v; want it unreadable", err)
	}
	d = t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), strings.Replace(progressiveProbe, `"start_time": "0.000000"`,
		`"start_time": 1e999`, 1))
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
	if err != nil || !strings.HasPrefix(g.Scan, FieldsKept+": its stored probe can't be read") ||
		!strings.HasPrefix(g.Unknown, CaptionsNoPlan) {
		t.Errorf("a probe giving 1e999: %+v, %v; want fields kept and no plan", g, err)
	}
}

// The record's picture is the record logic's picture_of: its first video
// stream that its dispositions don't mark attachedPic, by any value Python
// takes for true - "yes" and 2 mark it, "" and 0 don't -, which the record
// logic marks on what the cover rule calls a cover and nothing else.
func TestARecordsPictureIsItsFirstVideoStreamNotMarked(t *testing.T) {
	probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "tags": {"filename": "c.jpg",
		"mimetype": "image/jpeg"}}, {"index": 1, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"start_time": "0.000000", "avg_frame_rate": "24/1"}]}`
	for _, c := range []struct {
		name string
		mark any
		held bool
	}{
		{"marked true", true, false},
		{"marked \"yes\"", "yes", false},
		{"marked 2", 2, false},
		{"marked \"\"", "", true},
		{"marked 0", 0, true},
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", []any{
			map[string]any{"index": 0, "type": "video", "codec": "mjpeg", "dispositions": map[string]any{"attachedPic": c.mark}},
			map[string]any{"index": 1, "type": "video", "codec": "h264", "fieldOrder": "progressive"}}, probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		held := strings.HasPrefix(g.Unknown, CaptionsUnknown+": "+PictureNotFilm)
		if err != nil || held != c.held || (!c.held && g.Unknown != "") {
			t.Errorf("the cover %s: %+v, %v; want its picture not the film %v", c.name, g, err, c.held)
		}
	}
}

// A stored probe that gives no film - no video stream that is no cover -
// keeps the original for both, whatever the policy for an absent field
// order says: no plan, and its fields can't be told.
func TestAProbeOfNoFilmKeepsBoth(t *testing.T) {
	was := AbsentFieldOrder
	AbsentFieldOrder = func(FieldOrderGroup) bool { return false }
	defer func() { AbsentFieldOrder = was }()
	d := t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "audio", "codec": "aac"}},
		`{"streams": [{"index": 0, "codec_type": "audio", "codec_name": "aac"}]}`)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
	if err != nil || !strings.HasPrefix(g.Scan, FieldsKept+": its stored probe can't be read: it lists no picture") ||
		g.Unknown == "" {
		t.Errorf("a probe of no film: %+v, %v; want both kept", g, err)
	}
}

// A scan that found progressive holds by its own counts as the record
// logic's scan_holds reads them: chunks in fields its read-through counted
// (a count above 0, or one that can't be read: text, a float, null), an
// examination (idet) that is no object, gives a count that can't be read,
// or counted more top- and bottom-field pictures than progressive ones (a
// count it doesn't give is 0). Its own version's scan, and an older
// version's alike: what they say, not what they lack (an older one without
// its read-through says nothing).
func TestAScansOwnCountsThatSayFieldsHold(t *testing.T) {
	scan := func(verified, idet string) Doc {
		s := `"found": "progressive", "done": "none", "verified": {"ok": true, "complete": true` + verified + `}`
		if idet != "" {
			s += `, "idet": ` + idet
		}
		return doc(t, `{"scan": {`+s+`}}`)
	}
	for _, c := range []struct {
		name, verified, idet string
		held                 bool
	}{
		{"no counts", "", "", false},
		{"no chunks in fields", `, "chunksInFields": 0`, "", false},
		{"a chunk in fields", `, "chunksInFields": 1`, "", true},
		{"chunks in fields as text", `, "chunksInFields": "0"`, "", true},
		{"chunks in fields as a float", `, "chunksInFields": 0.0`, "", true},
		{"chunks in fields null", `, "chunksInFields": null`, "", true},
		{"fields no more than frames", "", `{"tff": 2, "bff": 3, "progressive": 5}`, false},
		{"fields more than frames", "", `{"tff": 3, "bff": 3, "progressive": 5}`, true},
		{"top fields alone, more", "", `{"tff": 1}`, true},
		{"frames alone", "", `{"progressive": 5}`, false},
		{"an empty examination", "", `{}`, false},
		{"an examination of null", "", `null`, false},
		{"an examination that is no object", "", `"none"`, true},
		{"a count as text", "", `{"tff": "0", "progressive": 5}`, true},
		{"a count below 0", "", `{"bff": -1, "progressive": 5}`, true},
		{"a count as a float", "", `{"progressive": 5.0}`, true},
	} {
		s := scan(c.verified, c.idet)
		if got := scanHolds(s) != ""; got != c.held {
			t.Errorf("%s, its own version's: held %v (%q); want %v", c.name, got, scanHolds(s), c.held)
		}
		if got := scanFoundHolds(s) != ""; got != c.held {
			t.Errorf("%s, an older version's: held %v (%q); want %v", c.name, got, scanFoundHolds(s), c.held)
		}
	}
	// an older version's scan without its read-through: its chunks can't be
	// counted, and it says nothing
	if why := scanFoundHolds(doc(t, `{"scan": {"found": "progressive"}}`)); why != "" {
		t.Errorf("an older version's scan of progressive without its read-through: %q; want nothing", why)
	}
	// at the gate: the version's own scan, and an older version's
	for _, c := range []struct {
		name  string
		older bool
	}{{"its own version's", false}, {"an older version's", true}} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
		counted := verifiedProgressive()
		counted["verified"].(map[string]any)["chunksInFields"] = 2
		if c.older {
			packageOf(t, d, "v-a", verifiedProgressive(), "a")
			packageOf(t, d, "v-old", counted, "a")
		} else {
			packageOf(t, d, "v-a", counted, "a")
		}
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.Contains(g.Scan, "counted chunks in fields") {
			t.Errorf("%s scan counting chunks in fields: %+v, %v; want it in fields", c.name, g, err)
		}
	}
}

// On the retire path: a package whose scan found progressive and counted
// more fields than frames keeps the original in fields.
func TestTheRetireJobKeepsAScanWhoseCountsSayFields(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	f.rewritePackage(t, func(pkg map[string]any) {
		scan := verifiedProgressive()
		scan["idet"] = map[string]any{"tff": 400, "bff": 0, "progressive": 100}
		pkg["scan"] = scan
	}, nil)
	f.keptFor(t, "a scan counting more fields than frames", "counted more fields than frames")
	if s := f.source(t, rtSource); !strings.Contains(deref(s.Error), FieldsKept) {
		t.Errorf("kept for %q; want it in fields", deref(s.Error))
	}
}
