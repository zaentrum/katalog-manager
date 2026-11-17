package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The window plan is one, for every place that reads or checks a read's
// windows: a window at the picture's start, and one at 10, 30, 50, 70 and
// 90 % of its own presentation span (start + at × (end − start)), each at
// most 30 s short of its end; a window decodes 48 packets and is one of at
// least half of them as frames. The gate's constants are held to it, and so
// are its places: a picture that starts late, or ends before its file does,
// is read where it is (the span its records give: PictureSpan).
func TestTheWindowPlanIsOne(t *testing.T) {
	plan := struct {
		start        bool
		at           []float64
		tailS        float64
		minFrames    int
		packets      int
		packetsShare float64
	}{true, []float64{0.1, 0.3, 0.5, 0.7, 0.9}, 30, 24, 48, 0.5}
	if WindowAtStart != plan.start || !slices.Equal(PlannedWindows, plan.at) || WindowTail != plan.tailS ||
		MinWindowFrames != plan.minFrames || WindowPackets != plan.packets ||
		float64(MinWindowFrames) != float64(WindowPackets)*plan.packetsShare {
		t.Errorf("the gate's plan: start %v, at %v, tail %v s, %d frames of %d packets; want %+v", WindowAtStart,
			PlannedWindows, WindowTail, MinWindowFrames, WindowPackets, plan)
	}
	for _, c := range []struct {
		span   Span
		places []float64
	}{
		{Span{0, 600}, []float64{0, 60, 180, 300, 420, 540}},
		// a picture whose clock starts at an hour
		{Span{3600, 4200}, []float64{3600, 3660, 3780, 3900, 4020, 4140}},
		// 200 s: 90 % is more than 30 s short of nothing, so at the tail
		{Span{5, 205}, []float64{5, 25, 65, 105, 145, 175}},
	} {
		var got []float64
		for _, at := range plannedAts(c.span.Length()) {
			got = append(got, c.span.place(at))
		}
		if !slices.EqualFunc(got, c.places, func(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }) {
			t.Errorf("a picture of %v: places %v; want %v", c.span, got, c.places)
		}
	}
	for _, c := range []struct {
		name, probe string
		span        Span
	}{
		{"no stored probe", "", Span{0, 600}},
		{"a picture that starts late and ends before its file", `{"format": {"start_time": "0.000000",
			"duration": "600.000000"}, "streams": [{"index": 0, "codec_type": "video", "codec_name": "h264",
			"start_time": "12.500000", "duration": "500.000000"}]}`, Span{12.5, 512.5}},
		{"a file whose clock starts later", `{"format": {"start_time": "1.400000"}, "streams": [{"index": 0,
			"codec_type": "video", "codec_name": "h264"}]}`, Span{1.4, 601.4}},
	} {
		var probe Doc
		if c.probe != "" {
			probe = doc(t, c.probe)
		}
		if s, why := PictureSpan(film(600), probe); s == nil || *s != c.span {
			t.Errorf("%s: span %v (%s); want %v", c.name, s, why, c.span)
		}
	}
}

// A read of five windows and none at the start - one made before the plan
// was one - is not complete under the plan: the gate takes it as not known
// until the original is read again.
func TestAReadOfFiveWindowsAndNoStartIsNotKnown(t *testing.T) {
	carried, unknown := captionsRule([]Doc{film(600)}, nil, read(600, every()[1:]...))
	if carried != "" || !strings.HasPrefix(unknown, CaptionsUnknown+": a read of its original says nothing of the "+
		"video: it has no window at start") {
		t.Errorf("five windows, none at the start: carried %q, unknown %q; want it not known", carried, unknown)
	}
}

// A read's windows are placed by the span of the picture its records give,
// never by the read's own length: a read of a picture whose clock starts at
// an hour goes where its stored probe says so, and is out of place where
// nothing says it (the picture's start is then 0); one whose windows lie in
// the first seconds of a ten-minute record, saying it lasts ten seconds, is
// of another length than its record's.
func TestTheWindowsArePlacedByTheRecordsSpan(t *testing.T) {
	var late []CaptionWindow
	for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		late = append(late, window(at, 3600+at*600, 48, false))
	}
	probe := doc(t, `{"format": {"start_time": "3600.000000"}, "streams": [{"index": 0, "codec_type": "video",
		"codec_name": "h264", "field_order": "progressive"}]}`)
	if carried, unknown := captionsRule([]Doc{film(600)}, []Doc{probe}, read(600, late...)); carried != "" ||
		unknown != "" {
		t.Errorf("a picture whose clock starts at an hour, said: carried %q, unknown %q; want it to go", carried, unknown)
	}
	if _, unknown := captionsRule([]Doc{film(600)}, nil, read(600, late...)); !strings.Contains(unknown,
		"lie more than 15 s from their places") {
		t.Errorf("a picture whose clock starts at an hour, not said: unknown %q; want it out of place", unknown)
	}
	// six windows in the first ten seconds, as a read of a ten-second video
	// places them, of a record that says ten minutes
	var first []CaptionWindow
	none := 0
	for i, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		first = append(first, CaptionWindow{At: at, Start: float64(2 * i), End: float64(2*i) + 0.9, Frames: 48,
			InterlacedFrames: &none, RepeatFrames: &none})
	}
	if _, unknown := captionsRule([]Doc{film(600)}, nil, read(10, first...)); !strings.Contains(unknown,
		"lie more than 15 s from their places") {
		t.Errorf("a read of ten seconds of a ten-minute record: unknown %q; want its windows out of place", unknown)
	}
	if _, unknown := captionsRule([]Doc{film(600)}, nil, read(600, first...)); !strings.Contains(unknown,
		"lie more than 15 s from their places") {
		t.Errorf("the same windows, the read saying ten minutes: unknown %q; want them out of place", unknown)
	}
}

// A record that says no length of its file (no container, no durationMs,
// one that is a string, a boolean or 0) leaves where a read's windows lie
// not known: the original is kept, whatever its read says.
func TestARecordWithoutALengthKeepsItsOriginal(t *testing.T) {
	stream := `"streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"}]`
	for name, rec := range map[string]string{
		"no container":           `{` + stream + `}`,
		"no durationMs":          `{"container": {"format": "matroska,webm"}, ` + stream + `}`,
		"durationMs a string":    `{"container": {"format": "matroska,webm", "durationMs": "600000"}, ` + stream + `}`,
		"durationMs a boolean":   `{"container": {"format": "matroska,webm", "durationMs": true}, ` + stream + `}`,
		"durationMs 0":           `{"container": {"format": "matroska,webm", "durationMs": 0}, ` + stream + `}`,
		"a container of nothing": `{"container": null, ` + stream + `}`,
	} {
		carried, unknown := captionsRule([]Doc{doc(t, rec)}, nil, read(600, every()...))
		if carried != "" || !strings.HasPrefix(unknown, CaptionsNoLength) {
			t.Errorf("%s: carried %q, unknown %q; want it kept, %q", name, carried, unknown, CaptionsNoLength)
		}
	}
}

// A film whose codec neither its record nor its stored probe states may be
// one whose captions a read of windows doesn't see (hevc): it is held as
// one, whatever its windows found.
func TestAFilmWhoseCodecNothingStatesIsHeld(t *testing.T) {
	const c = `"container": {"format": "matroska,webm", "durationMs": 600000}`
	for name, rec := range map[string]string{
		"no codec":          `{` + c + `, "streams": [{"index": 0, "type": "video", "fieldOrder": "progressive"}]}`,
		"codec unknown":     `{` + c + `, "streams": [{"index": 0, "type": "video", "codec": "unknown", "fieldOrder": "progressive"}]}`,
		"codec a number":    `{` + c + `, "streams": [{"index": 0, "type": "video", "codec": 265, "fieldOrder": "progressive"}]}`,
		"codec blank":       `{` + c + `, "streams": [{"index": 0, "type": "video", "codec": " ", "fieldOrder": "progressive"}]}`,
		"no picture at all": `{` + c + `, "streams": [{"index": 0, "type": "audio", "codec": "aac"}]}`,
	} {
		for _, probe := range []string{"", `{"streams": [{"index": 0, "codec_type": "video", "field_order": "progressive"}]}`} {
			var probes []Doc
			if probe != "" {
				probes = []Doc{doc(t, probe)}
			}
			carried, unknown := captionsRule([]Doc{doc(t, rec)}, probes, read(600, every()...))
			if carried != "" || !strings.HasPrefix(unknown, CaptionsUnscanned+": neither its record nor its stored probe") {
				t.Errorf("%s (probe %q): carried %q, unknown %q; want it held as unscanned", name, probe, carried, unknown)
			}
		}
	}
}

// A stored probe that says its picture carries closed captions
// (closed_captions 1, as ffprobe 7.1 writes it once it decoded a frame)
// carries them, and so does a record whose picture says closedCaptions; a
// 0 or false says nothing, and another value can't be read.
func TestAProbesClosedCaptionsOfItsPictureAreCarried(t *testing.T) {
	for v, want := range map[string]string{`1`: "carried", `"1"`: "carried", `true`: "carried", `0`: "",
		`"0"`: "", `false`: "", `null`: "", `"s"`: "unknown", `2`: "unknown"} {
		probe := doc(t, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
			"closed_captions": `+v+`}]}`)
		carried, unknown := captionsRule([]Doc{film(600)}, []Doc{probe}, read(600, every()...))
		got := ""
		switch {
		case carried != "":
			got = "carried"
		case unknown != "":
			got = "unknown"
		}
		if got != want {
			t.Errorf("closed_captions %s: carried %q, unknown %q; want %q", v, carried, unknown, want)
		}
	}
	rec := film(600)
	streams, _ := rec.Get("streams")
	streams.([]any)[0] = append(streams.([]any)[0].(Doc), Field{"closedCaptions", true})
	if carried, _ := captionsRule([]Doc{rec}, nil, read(600, every()...)); !strings.HasPrefix(carried, CaptionsCarried) {
		t.Errorf("a record whose picture says closedCaptions: carried %q; want them carried", carried)
	}
}

// A record or a window that gives a key twice says it neither way: the
// record can't be read, and holds the original; the window is a failed
// one, which keeps the worst of what it gives (captions true in any value,
// the most frames counted in fields).
func TestARepeatedKeyIsNoneEitherWay(t *testing.T) {
	ess := essJSONOf(t)
	for name, c := range map[string]struct{ record, probe, version string }{
		"essence closedCaptions false then true": {record: `{"essence": ` + strings.Replace(ess, `"closedCaptions":false`,
			`"closedCaptions":false,"closedCaptions":true`, 1) + `, ` + okRecordRest},
		"two essences":            {record: `{"essence": ` + ess + `, "essence": {"closedCaptions": true}, ` + okRecordRest},
		"two streams":             {record: `{"essence": ` + ess + `, "streams": [], ` + okRecordRest},
		"field order twice":       {record: `{"essence": ` + ess + `, "container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive", "fieldOrder": "tt"}]}`},
		"signature twice":         {record: `{"essence": ` + ess + `, "container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "hevc", "fieldOrder": "progressive", "captionSignature": false, "captionSignature": true}]}`},
		"probe streams twice":     {record: `{"essence": ` + ess + `, ` + okRecordRest, probe: `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"}], "streams": [{"index": 0, "codec_type": "subtitle", "codec_name": "eia_608"}]}`},
		"version sourceIds twice": {record: `{"essence": ` + ess + `, ` + okRecordRest, version: "twice"},
	} {
		d := t.TempDir()
		rawRecord(t, d, "a", c.record, c.probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		if c.version != "" {
			p := filepath.Join(VersionDir(d, "v-a"), VersionFile)
			b, _ := os.ReadFile(p)
			b = []byte(strings.Replace(string(b), `"sourceIds": [`, `"sourceIds": ["b"], "sourceIds": [`, 1))
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err == nil && g.Unknown == "" && g.Scan == "" && g.Captions == "" {
			t.Errorf("%s: %+v; want it held", name, g)
		}
	}
	var w CaptionWindow
	if err := json.Unmarshal([]byte(`{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": true,
		"captions": false, "interlacedFrames": 0, "repeatFrames": 0}`), &w); err != nil || w.Unreadable == "" ||
		w.Frames != 0 || !w.Captions {
		t.Errorf("captions true then false: %+v, %v; want a failed window that showed captions", w, err)
	}
	if err := json.Unmarshal([]byte(`{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": false,
		"interlacedFrames": 5, "interlacedFrames": 0, "repeatFrames": 0}`), &w); err != nil || w.Unreadable == "" ||
		w.InterlacedFrames == nil || *w.InterlacedFrames != 5 {
		t.Errorf("interlaced 5 then 0: %+v, %v; want a failed window that counted 5", w, err)
	}
	// and the gate counts what such a window found: frames in fields
	ws := every()
	ws[2] = w
	if fields, _ := inFields(t.TempDir(), "", nil, nil, []Doc{film(600)}, nil, read(600, ws...).Captions); !strings.Contains(fields,
		"found interlaced ones at 30%") {
		t.Errorf("a failed window that counted 5 interlaced frames: fields %q; want it in fields", fields)
	}
}

// okRecordRest is the rest of a source record a test writes after its
// essence: its container and its one picture.
const okRecordRest = `"container": {"format": "matroska,webm", "durationMs": 600000}, "streams": [{"index": 0,
	"type": "video", "codec": "h264", "fieldOrder": "progressive"}]}`

// essJSONOf is fullEssence as JSON.
func essJSONOf(t *testing.T) string {
	b, err := json.Marshal(fullEssence())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// rawRecord writes sources/<sid>/source.json (and ffprobe.json) as given.
func rawRecord(t *testing.T, d, sid, record, probe string) {
	t.Helper()
	librarytest.Write(t, filepath.Join(SourceDir(d, sid), "source.json"), []byte(record))
	if probe != "" {
		librarytest.Write(t, filepath.Join(SourceDir(d, sid), ProbeFile), []byte(probe))
	}
}

// An essence that doesn't state a fact the gate compares is no "nothing to
// lose": the gate holds the original, naming what it doesn't state; so does
// a record that lists no streams of its file.
func TestARecordThatDoesNotTellWhatTheGateComparesHolds(t *testing.T) {
	noDepth := fullEssence()
	delete(noDepth, "videoBitDepth")
	delete(noDepth, "maxVideoHeight")
	for name, c := range map[string]struct {
		essence map[string]any
		streams any
		want    string
	}{
		"an essence of nothing":          {map[string]any{}, filmStreams(), "its essence doesn't state surround"},
		"no height or bit depth":         {noDepth, filmStreams(), "its essence doesn't state maxVideoHeight, videoBitDepth"},
		"no streams":                     {fullEssence(), nil, "it lists no streams of its file"},
		"streams null":                   {fullEssence(), "null", "it lists no streams of its file"},
		"streams that are an empty list": {fullEssence(), []any{}, "it lists no streams of its file"},
	} {
		d := t.TempDir()
		rec := map[string]any{"sourceId": "a", "essence": c.essence,
			"container": map[string]any{"format": "matroska,webm", "durationMs": 600000}}
		switch s := c.streams.(type) {
		case nil:
		case string:
			rec["streams"] = nil
		default:
			rec["streams"] = s
		}
		librarytest.WriteSource(t, SourceDir(d, "a"), rec, statedProbe(nil))
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.Contains(g.Unknown, c.want) {
			t.Errorf("%s: %+v, %v; want it held: %s", name, g, err, c.want)
		}
	}
}

// The version the catalog says is made of the original says so too: one
// whose version.json names no list of sourceIds, names another, or is not
// there, holds it; and its package's own scan is read whatever its
// version.json says.
func TestTheAskedVersionsOwnRecordAndScanAreRead(t *testing.T) {
	for name, ver := range map[string]map[string]any{
		"sourceIds null":         {"sourceIds": nil},
		"sourceIds a string":     {"sourceIds": "a"},
		"sourceIds of another":   {"sourceIds": []string{"b"}},
		"no version.json at all": nil,
	} {
		for _, scan := range []map[string]any{verifiedProgressive(), {"found": "fields", "done": "deinterlaced-frame"}} {
			d := t.TempDir()
			sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
			pkg := packageRecord(fullEssence())
			pkg["scan"] = scan
			librarytest.WriteVersion(t, VersionDir(d, "v-a"), librarytest.Version{VersionID: "v-a", PackageID: "p-a",
				SourceIDs: []string{"a"}, CreatedAt: "2026-10-01T10:00:00Z", Package: pkg, Version: ver})
			if ver == nil {
				if err := os.Remove(filepath.Join(VersionDir(d, "v-a"), VersionFile)); err != nil {
					t.Fatal(err)
				}
			}
			g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
			if err != nil || !strings.Contains(g.Unknown, "doesn't name it among the originals") {
				t.Errorf("%s, scan %v: %+v, %v; want it held for its version's record", name, scan["found"], g, err)
			}
			if scan["found"] == "fields" && !strings.HasPrefix(g.Scan, FieldsKept+": the scan of its package of version v-a") {
				t.Errorf("%s, scan fields: scan %q; want the version's own scan read", name, g.Scan)
			}
		}
	}
}

// The scans of every version of the title made of the original are read,
// in a folder whose path a pattern would read otherwise too ("[new]"); a
// folder of versions that can't be listed holds it; a scan of null, and
// one that says nothing of what was done (done absent or null), holds it.
func TestEveryScanIsReadAndAScanOfNothingHolds(t *testing.T) {
	d := filepath.Join(t.TempDir(), "Film [new] (2020)")
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	packageOf(t, d, "v-old", map[string]any{"found": "fields", "done": "deinterlaced-frame"}, "a")
	if g, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err != nil ||
		!strings.HasPrefix(g.Scan, FieldsKept+": the scan of its package of version v-old found fields") {
		t.Errorf("another version's scan under a path with brackets: %+v, %v; want it read", g, err)
	}
	versions := filepath.Join(d, "versions")
	if err := os.Chmod(versions, 0o300); err != nil {
		t.Fatal(err)
	}
	g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
	_ = os.Chmod(versions, 0o755)
	if err != nil || !strings.Contains(g.Scan, "the versions of its title can't be listed") {
		t.Errorf("a folder of versions that can't be listed: %+v, %v; want it held", g, err)
	}
	for name, scan := range map[string]any{
		"scan null": nil,
		"no done":   map[string]any{"found": NothingFound, "verified": map[string]any{"ok": true, "complete": true}},
		"done null": map[string]any{"found": NothingFound, "done": nil, "verified": map[string]any{"ok": true, "complete": true}},
	} {
		pkg := packageRecord(fullEssence())
		pkg["scan"] = scan
		if why := scanHolds(doc(t, string(librarytest.JSON(t, pkg)))); why == "" {
			t.Errorf("%s: the scan holds nothing; want it held", name)
		}
	}
}

// A window the tail short of its end is placed by the length a read reads,
// which may differ from its record's within DurationSlack: such a window is
// the plan's window all the same (88 % of a 250 s record, 88.1 % of the
// 252.4 s read), and a read of every window so placed goes.
func TestAWindowPlacedByALengthWithinTheSlackIsThePlans(t *testing.T) {
	for _, c := range []struct{ record, read float64 }{{250, 252.4}, {250, 248}, {100, 98}, {150, 152}, {600, 605}} {
		var ws []CaptionWindow
		for _, at := range plannedAts(c.read) {
			ws = append(ws, window(at, at*c.read, 48, false))
		}
		if carried, unknown := captionsRule([]Doc{film(c.record)}, nil, read(c.read, ws...)); carried != "" || unknown != "" {
			t.Errorf("a record of %v s read as %v s: carried %q, unknown %q; want it to go", c.record, c.read, carried, unknown)
		}
	}
}
