package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// doc is the JSON js decoded, as a record is.
func doc(t *testing.T, js string) Doc {
	t.Helper()
	d, err := DecodeDoc([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A codec whose captions a read of windows may not see (ScannedCodecs:
// HEVC's suffix SEI) is told as carrying none only by a read of every packet
// of its picture (its record's captionSignature false): a read of every
// planned window that found none leaves its captions not known, by its
// record's codec or its probe's; a signature true carries them; another
// codec goes on its windows.
func TestAnHEVCOriginalsCaptionsNeedEveryPacketRead(t *testing.T) {
	for _, c := range []struct {
		name, record, probe string
		carried, unknown    string // what each begins with; "" none
	}{
		{"hevc, windows alone", `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "hevc"}]}`, "",
			"", CaptionsUnscanned},
		{"hevc by its probe alone", `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video"}]}`,
			`{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc", "start_time": "0.000000",
				"avg_frame_rate": "24/1"}]}`, "", CaptionsUnscanned},
		{"hevc, its packets read whole, no signature", `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "hevc",
			"captionSignature": false}]}`, "", "", ""},
		{"hevc, a signature found", `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "hevc",
			"captionSignature": true}]}`, "", CaptionsCarried, ""},
		{"h264, windows alone", `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "h264"}]}`, "", "", ""},
	} {
		probes := planProbes
		if c.probe != "" {
			probes = []Doc{doc(t, c.probe)}
		}
		carried, unknown := captionsRule([]Doc{doc(t, c.record)}, probes, read(600, every()...))
		if !strings.HasPrefix(carried, c.carried) || (c.carried == "") != (carried == "") ||
			!strings.HasPrefix(unknown, c.unknown) || (c.unknown == "") != (unknown == "") {
			t.Errorf("%s: carried %q, unknown %q; want them to begin %q, %q", c.name, carried, unknown, c.carried,
				c.unknown)
		}
	}
}

// The windows tell none only where a read has a window at each planned
// place, in place, apart and rising, each of at least MinWindowFrames frames,
// none failed (ReadRefused, the one helper the PUT and the gate both ask):
// one window at the start, five that all decoded the first two seconds,
// five at places no plan has, five of one or three frames, a failed one,
// one landed far from its place - each leaves captions not known.
func TestTheWindowsTellNoneOnlyWhenEveryPlannedOneIsRead(t *testing.T) {
	five := func(change func(i int, w *CaptionWindow)) []CaptionWindow {
		ws := every()
		for i := range ws {
			change(i, &ws[i])
		}
		return ws
	}
	for name, ws := range map[string][]CaptionWindow{
		"one window at the start": {window(0, 0, 48, false)},
		"all on the first two seconds": five(func(i int, w *CaptionWindow) {
			w.Start, w.End = float64(i)*0.001, 2
		}),
		"at places no plan has": five(func(i int, w *CaptionWindow) { w.At = 0.01 * float64(i+1) }),
		"of one frame":          five(func(_ int, w *CaptionWindow) { w.Frames = 1 }),
		"of three frames":       five(func(_ int, w *CaptionWindow) { w.Frames = 3 }),
		"one failed":            five(func(i int, w *CaptionWindow) { *w = map[bool]CaptionWindow{true: {At: w.At}, false: *w}[i == 2] }),
		"one landed far from its place": five(func(i int, w *CaptionWindow) {
			if i == 3 {
				w.Start, w.End = w.Start+100, w.End+100
			}
		}),
	} {
		carried, unknown := captionsRule([]Doc{film(600)}, planProbes, read(600, ws...))
		if carried != "" || !strings.HasPrefix(unknown, CaptionsUnknown) {
			t.Errorf("%s: carried %q, unknown %q; want captions not known", name, carried, unknown)
		}
	}
}

// A window that is no window - a string for a boolean or a number, a float
// for a count, null captions of a window that decoded frames, a place past
// the video, an end before its start, no start, an entry that is no object,
// a count out of range or of another type, captions of more interlaced
// frames than frames - is read as a failed window (Unreadable), never left
// out: the read keeps all six, its captions are not known, and a count that
// can't be read puts the original in fields. What such a window that decoded
// frames showed stays (the worst a read says is kept): its captions true
// carry them, as it is read and as it is stored and read again.
func TestAWindowThatIsNoWindowIsAFailedOne(t *testing.T) {
	good := func(at float64) string {
		w, _ := json.Marshal(window(at, at*600, 48, false))
		return string(w)
	}
	for _, c := range []struct {
		name, third          string
		countsUnread, showed bool
	}{
		{"captions a string", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": "true"}`, false, false},
		{"frames a string", `{"at": 0.3, "start": 180, "end": 182, "frames": "0", "captions": false}`, false, false},
		{"frames a float", `{"at": 0.3, "start": 180, "end": 182, "frames": 48.0, "captions": false}`, false, false},
		{"captions null of 48 frames", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": null}`, false,
			false},
		{"at past the video, captions", `{"at": 1.5, "start": 180, "end": 182, "frames": 48, "captions": true}`, false,
			true},
		{"end before start", `{"at": 0.3, "start": 180, "end": 179, "frames": 48, "captions": true}`, false, true},
		{"no start", `{"at": 0.3, "end": 182, "frames": 48, "captions": true}`, false, true},
		{"no object", `"failed"`, false, false},
		{"interlacedFrames a float", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": false,
			"interlacedFrames": 12.0, "repeatFrames": 0}`, true, false},
		{"interlacedFrames a string", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": false,
			"interlacedFrames": "12", "repeatFrames": 0}`, true, false},
		{"repeatFrames below 0", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": false,
			"interlacedFrames": 12, "repeatFrames": -1}`, true, false},
		{"captions, 49 interlaced of 48 frames", `{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": true,
			"interlacedFrames": 49, "repeatFrames": 0}`, true, true},
	} {
		var ws []CaptionWindow
		body := "[" + good(0) + "," + good(0.1) + "," + c.third + "," + good(0.5) + "," + good(0.7) + "," + good(0.9) + "]"
		if err := json.Unmarshal([]byte(body), &ws); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(ws) != 6 || ws[2].Unreadable == "" || ws[2].Frames != 0 || ws[2].Captions != c.showed ||
			ws[2].CountsUnreadable != c.countsUnread {
			t.Errorf("%s: %d windows, the third %+v; want six, the third failed (captions %v)", c.name, len(ws), ws[2],
				c.showed)
			continue
		}
		carried, unknown := captionsRule([]Doc{film(600)}, planProbes, read(600, ws...))
		switch {
		case c.showed && (!strings.HasPrefix(carried, CaptionsCarried+" at ") || unknown != ""):
			t.Errorf("%s: carried %q, unknown %q; want the captions it showed carried", c.name, carried, unknown)
		case !c.showed && (carried != "" || !strings.HasPrefix(unknown, CaptionsUnknown)):
			t.Errorf("%s: carried %q, unknown %q; want captions not known", c.name, carried, unknown)
		}
		fields, _ := inFields(t.TempDir(), "", nil, nil, []Doc{doc(t, `{"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video",
			"codec": "h264", "fieldOrder": "progressive"}]}`)}, nil, read(600, ws...).Captions)
		if (fields != "") != c.countsUnread {
			t.Errorf("%s: fields %q; want in fields %v", c.name, fields, c.countsUnread)
		}
		// and kept as it is read: stored and read again, the same
		b, _ := json.Marshal(ws)
		var again []CaptionWindow
		if err := json.Unmarshal(b, &again); err != nil || len(again) != 6 || again[2].Unreadable != ws[2].Unreadable ||
			again[2].CountsUnreadable != ws[2].CountsUnreadable || again[2].Captions != ws[2].Captions {
			t.Errorf("%s: stored and read again: %+v, %v", c.name, again, err)
		}
	}
}

// A source record or a stored probe that is no record of a file - broken,
// no object, an object of nothing ({}), streams that are no list of
// objects, none, a format that is no object - can't be read, and holds the
// original.
func TestARecordOrProbeThatIsNoneHolds(t *testing.T) {
	const film = `{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"}`
	for name, probe := range map[string]string{
		"broken":             `{ not json`,
		"no object":          `[]`,
		"a string":           `"x"`,
		"an empty object":    `{}`,
		"streams null":       `{"format": {"format_name": "matroska,webm"}, "streams": null}`,
		"no streams":         `{"format": {"format_name": "matroska,webm"}, "streams": []}`,
		"a stream no object": `{"format": {"format_name": "matroska,webm"}, "streams": ["x"]}`,
		"streams an object":  `{"format": {"format_name": "matroska,webm"}, "streams": {"0": ` + film + `}}`,
		"format a string":    `{"format": "matroska,webm", "streams": [` + film + `]}`,
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "h264",
			"fieldOrder": "progressive"}}, probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || (g.Scan == "" && g.Unknown == "") || !strings.Contains(g.Scan+g.Unknown, "can't be read") {
			t.Errorf("a probe %s: %+v, %v; want it held as can't be read", name, g, err)
		}
	}
	for name, rec := range map[string]map[string]any{
		"no essence":           {"sourceId": "a", "container": map[string]any{"format": "matroska,webm"}},
		"an essence no object": {"sourceId": "a", "essence": []any{}},
		"streams no list":      {"sourceId": "a", "essence": fullEssence(), "streams": map[string]any{}},
		"a stream no object":   {"sourceId": "a", "essence": fullEssence(), "streams": []any{"x"}},
	} {
		d := t.TempDir()
		librarytest.WriteSource(t, SourceDir(d, "a"), rec, statedProbe(nil))
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.Contains(g.Unknown, "its source record can't be read") {
			t.Errorf("a record %s: %+v, %v; want it held as can't be read", name, g, err)
		}
	}
	// the gate of a broken record is an error, which every path keeps the
	// original on
	d := t.TempDir()
	librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a", "essence": fullEssence()}, nil)
	if err := os.WriteFile(filepath.Join(SourceDir(d, "a"), "source.json"), []byte(`{ not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	if _, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err == nil {
		t.Error("a broken source record: no error")
	}
}

// The scan of the package of the version an original is retired from holds
// it in fields unless it found exactly NothingFound, did nothing to the
// picture (ScanDone) and verified it whole (verified.ok and
// verified.complete true): a word found, a blank, another type, none, a done
// of something, a verification not ok or not complete or none, a scan that
// is no object - each holds, and none of them breaks the gate. A scan that
// found nothing lets nothing go either. Of another version made of it, only
// a found other than NothingFound holds, or NothingFound its own counts say
// otherwise of (chunks in fields, more fields than frames: scanCountsHold).
func TestAPackagesScanHoldsUnlessItVerifiedThePictureProgressive(t *testing.T) {
	ok := map[string]any{"ok": true, "complete": true}
	for name, scan := range map[string]any{
		"found fields":                    map[string]any{"found": "fields", "done": "deinterlaced-frame"},
		"found a word unknown":            map[string]any{"found": "woven", "done": "none", "verified": ok},
		"found blank":                     map[string]any{"found": " ", "done": "deinterlaced-frame", "verified": ok},
		"found empty":                     map[string]any{"found": "", "done": "none", "verified": ok},
		"found null":                      map[string]any{"found": nil, "done": "deinterlaced-frame", "verified": ok},
		"no found":                        map[string]any{"done": "deinterlaced-frame", "verified": ok},
		"found a number":                  map[string]any{"found": 7, "verified": ok},
		"found a list":                    map[string]any{"found": []any{"fields"}, "verified": ok},
		"found progressive, deinterlaced": map[string]any{"found": "progressive", "done": "deinterlaced-frame", "verified": ok},
		"found progressive, fields kept":  map[string]any{"found": "progressive", "done": "fields-kept", "verified": ok},
		"found progressive, not ok": map[string]any{"found": "progressive", "done": "progressive",
			"verified": map[string]any{"ok": false, "complete": true, "chunksInFields": 40}},
		"found progressive, not complete": map[string]any{"found": "progressive", "done": "progressive",
			"verified": map[string]any{"ok": true, "complete": false}},
		"found progressive, no verification": map[string]any{"found": "progressive", "done": "progressive",
			"idet": map[string]any{"frames": 1000, "tff": 980}},
		"found Progressive, spaced": map[string]any{"found": "Progressive ", "verified": ok},
		"an empty scan":             map[string]any{},
		"a scan that is a string":   "fields",
		"a scan that is a list":     []any{"fields"},
	} {
		d := t.TempDir()
		rec := sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video",
			"codec": "h264", "fieldOrder": "progressive"}}, "")
		pkg := packageRecord(fullEssence())
		pkg["scan"] = scan
		librarytest.WriteVersion(t, VersionDir(d, "v-a"), librarytest.Version{VersionID: "v-a", PackageID: "p-v-a",
			SourceIDs: []string{"a"}, CreatedAt: "2026-10-01T10:00:00Z", Package: pkg})
		own, err := readRecordDoc(filepath.Join(VersionDir(d, "v-a"), PackageFile))
		if err != nil {
			t.Fatal(err)
		}
		if why, _ := inFields(d, "v-a", own, []string{"a"}, []Doc{rec}, nil, nil); !strings.HasPrefix(why,
			"the scan of its package of version v-a ") {
			t.Errorf("%s: %q; want it held by the scan", name, why)
		}
		// of another version made of it: a found other than NothingFound,
		// or one its counts say otherwise of
		var found any
		hasFound := false
		if x, ok := scan.(map[string]any); ok {
			found, hasFound = x["found"]
		}
		countsSay := name == "found progressive, not ok" || name == "found progressive, no verification"
		other := hasFound && found != nil && (found != NothingFound || countsSay)
		if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); other != strings.HasPrefix(why,
			"the scan of its package of version v-a found") {
			t.Errorf("%s, of another version: %q; want it held %v", name, why, other)
		}
	}
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "h264",
		"fieldOrder": "progressive"}}, "")
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why != "" {
		t.Errorf("a scan that verified the picture progressive: %q; want it to hold nothing", why)
	}
}

// The gate holds what it is not told: facts that set nothing hold the
// original to the captions rule (no read: not known); a gate asked of no
// original lets nothing go; an essence's closedCaptions or interlaced that is
// no boolean ("false", 1, [0]) can't be read, and holds it.
func TestTheGateHoldsWhatItIsNotTold(t *testing.T) {
	d := t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "h264",
		"fieldOrder": "progressive"}}, progressiveProbe)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	if g, err := ReadGateWith(d, "v-a", GateFacts{}, "a"); err != nil || !strings.HasPrefix(g.Unknown, CaptionsUnknown) {
		t.Errorf("facts that set nothing: %+v, %v; want captions not known", g, err)
	}
	if g, err := ReadGateWith(d, "v-a", read(600, every()...)); err != nil || g.Unknown == "" {
		t.Errorf("a gate of no original: %+v, %v; want it held", g, err)
	}
	if g, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err != nil || g.Unknown != "" || g.Scan != "" {
		t.Errorf("a clean original: %+v, %v; want it to go", g, err)
	}
	for _, v := range []any{"false", 1, []any{0}, "true"} {
		e := Doc{{Key: "essence", Value: Doc{{Key: "closedCaptions", Value: v}}}}
		if carried, unknown := captionsRule([]Doc{e}, planProbes, read(600, every()...)); carried == "" &&
			!strings.HasPrefix(unknown, CaptionsUnknown) {
			t.Errorf("closedCaptions %v: carried %q, unknown %q; want it held", v, carried, unknown)
		}
		i := Doc{{Key: "essence", Value: Doc{{Key: "interlaced", Value: v}}}, {Key: "streams", Value: []any{
			Doc{{Key: "index", Value: json.Number("0")}, {Key: "type", Value: "video"}, {Key: "codec", Value: "h264"},
				{Key: "fieldOrder", Value: "progressive"}}}}}
		if why, _ := inFields(t.TempDir(), "", nil, nil, []Doc{i}, nil, nil); why == "" {
			t.Errorf("interlaced %v: not in fields; want it held", v)
		}
	}
}
