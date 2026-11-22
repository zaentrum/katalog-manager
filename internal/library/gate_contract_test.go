package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The gate lets go nothing the contract holds: a scan's read-through is the
// record logic's read_through, a record's own offList and its attachedPic
// marks are read beside what the gate reads of its streams, and where the
// two disagree the gate holds.

// A scan that found progressive and verified it ok and complete holds
// unless its read-through decoded every packet but its leading pictures,
// as the record logic's read_through counts it: frames = packets - leading,
// each a whole number from 0, no other slack - one that gives no leading
// (a scan from before that rule), no packets or no frames, or one of them
// as text, a float, below 0 or null, is never complete, and nor is one
// whose decoder output its leading pictures too (a whole read of captions
// takes that, a scan's read-through doesn't). An older version's scan is
// held to the same rule: every package of the title is.
func TestAScansReadThroughIsHeldToItsCounts(t *testing.T) {
	scan := func(counts string) Doc {
		return doc(t, `{"scan": {"found": "progressive", "done": "none", "verified": {"ok": true, "complete": true`+
			counts+`}}}`)
	}
	for _, c := range []struct {
		name, counts string
		held         bool
	}{
		{"every packet", `, "frames": 1000, "packets": 1000, "leading": 0`, false},
		{"every packet but 3 leading", `, "frames": 997, "packets": 1000, "leading": 3`, false},
		{"a packet more short", `, "frames": 996, "packets": 1000, "leading": 3`, true},
		{"a packet short, none leading", `, "frames": 999, "packets": 1000, "leading": 0`, true},
		{"the leading pictures output too", `, "frames": 1000, "packets": 1000, "leading": 3`, true},
		{"more frames than packets", `, "frames": 1001, "packets": 1000, "leading": 0`, true},
		{"no leading", `, "frames": 1000, "packets": 1000`, true},
		{"no packets", `, "frames": 1000, "leading": 0`, true},
		{"no frames", `, "packets": 1000, "leading": 0`, true},
		{"no counts", ``, true},
		{"frames as text", `, "frames": "1000", "packets": 1000, "leading": 0`, true},
		{"packets as a float", `, "frames": 1000, "packets": 1000.0, "leading": 0`, true},
		{"leading below 0", `, "frames": 1001, "packets": 1000, "leading": -1`, true},
		{"leading null", `, "frames": 1000, "packets": 1000, "leading": null`, true},
	} {
		s := scan(c.counts)
		if got := scanHolds(s) != ""; got != c.held {
			t.Errorf("%s, its own version's: held %v (%q); want %v", c.name, got, scanHolds(s), c.held)
		}
	}
	// at the gate, both ways
	for _, c := range []struct {
		name     string
		verified map[string]any
		held     bool
	}{
		{"a read-through of every packet", readThrough(), false},
		{"one that says ok and complete alone", map[string]any{"ok": true, "complete": true}, true},
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
		packageOf(t, d, "v-a", map[string]any{"found": NothingFound, "done": NothingFound, "verified": c.verified}, "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		held := strings.Contains(g.Scan, "its read-through gives no frames")
		if err != nil || held != c.held || (!c.held && (g.Scan != "" || g.Unknown != "")) {
			t.Errorf("%s: %+v, %v; want it held %v", c.name, g, err, c.held)
		}
	}
}

// On the retire path: a package whose scan verified the picture ok and
// complete but gives no counts, or decoded a packet more than its leading
// pictures short, keeps the original in fields; one that read every packet
// lets it go the ordinary way.
func TestTheRetireJobHoldsAScanToItsReadThrough(t *testing.T) {
	for _, c := range []struct {
		name     string
		verified map[string]any
		kept     string
	}{
		{"ok and complete alone", map[string]any{"ok": true, "complete": true}, "its read-through gives no frames"},
		{"a packet more short", map[string]any{"ok": true, "complete": true, "frames": 996, "packets": 1000, "leading": 3},
			"not every packet but its leading pictures"},
		{"every packet", readThrough(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			f.rewritePackage(t, func(pkg map[string]any) {
				pkg["scan"] = map[string]any{"found": NothingFound, "done": NothingFound, "verified": c.verified}
			}, nil)
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
}

// A source record's own offList, as the record logic's record_off_list
// reads it, is held beside what the gate reads of its streams: what it
// names is off, as it names it ("stream:data:bin_data", " "), and one that
// is no list of names (text, null, an empty name, a number) is
// offList:unreadable; an empty one says nothing, and where it and the
// streams disagree - the record says nothing off, its stored probe a codec
// off the lists - what either says holds.
func TestARecordsOwnOffListHolds(t *testing.T) {
	rec := func(off string) Doc {
		d := film(600)
		if off != "" {
			d = append(d, Field{"offList", doc(t, `{"x": `+off+`}`)[0].Value})
		}
		return d
	}
	for _, c := range []struct {
		name, off, want string
	}{
		{"none", "", ""},
		{"an empty list", `[]`, ""},
		{"a name", `["stream:data:bin_data"]`, "stream:data:bin_data"},
		{"two names", `["codec:vc1", "container:avi"]`, "codec:vc1, container:avi"},
		{"a name of a space", `[" "]`, " "},
		{"text", `"codec:vc1"`, "offList:unreadable"},
		{"null", `null`, "offList:unreadable"},
		{"an empty name", `["codec:vc1", ""]`, "offList:unreadable"},
		{"a number", `[1]`, "offList:unreadable"},
	} {
		if got := strings.Join(offListOf([]Doc{rec(c.off)}, planProbes, nil), ", "); got != c.want {
			t.Errorf("an offList of %s: off %q; want %q", c.name, got, c.want)
		}
		// and the contract's gate reads it alike (record_off_list)
		if got := strings.Join(contractRecordOffList(rec(c.off)), ", "); got != c.want {
			t.Errorf("an offList of %s, the contract's gate: off %q; want %q", c.name, got, c.want)
		}
	}
	// the record says nothing off, its stored probe a codec off the lists
	vc1 := doc(t, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "vc1", "start_time": "0",
		"avg_frame_rate": "24/1"}]}`)
	if got := offListOf([]Doc{rec(`[]`)}, []Doc{vc1}, nil); strings.Join(got, ",") != "codec:vc1" {
		t.Errorf("an empty offList over a probe of vc1: off %v; want the probe's codec held", got)
	}
	// at the gate: a record whose offList names a stream its streams don't
	// show keeps the original; an empty one lets it go
	for _, c := range []struct {
		off  []any
		want string
	}{{[]any{"stream:data:bin_data"}, "stream:data:bin_data"}, {[]any{}, ""}} {
		d := t.TempDir()
		librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a",
			"file": map[string]any{"name": "a.file", "sizeBytes": 1}, "essence": fullEssence(), "streams": filmStreams(),
			"container": map[string]any{"format": "matroska,webm", "durationMs": 600000}, "offList": c.off},
			map[string]string{ProbeFile: progressiveProbe})
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.Contains(g.Unknown, c.want) || (c.want == "") != (g.Unknown == "") ||
			(c.want != "" && !strings.HasPrefix(g.Unknown, closedNotKnown)) {
			t.Errorf("a record's offList %s: %+v, %v; want %q held", c.off, g, err, c.want)
		}
	}
}

// On the retire path: a record whose own offList names what its streams
// don't show keeps the original.
func TestTheRetireJobHoldsARecordsOwnOffList(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	f.rewriteRecord(t, func(rec map[string]any) { rec["offList"] = []any{"stream:data:bin_data"} }, nil)
	f.keptFor(t, "a record's offList", "off the closed lists: stream:data:bin_data")
}

// A record's attachedPic mark is held to the cover rule's reading of the
// same stream (by its stored probe's tags and counts): where they disagree -
// an older record that leaves a cover its tags alone mark unmarked, or one
// that marks a stream the rule calls none (a webp) -, which is the picture
// can't be told and the gate holds; a record that marks the rule's cover,
// as the record logic writes it, goes. The gate asked of the records alone
// (ReadGate) holds it the same, and of one not read yet waits on no read:
// none lets it go.
func TestARecordsMarkTheCoverRuleDoesNotBearOutHolds(t *testing.T) {
	tagged := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"start_time": "0.000000", "avg_frame_rate": "24/1"}, {"index": 1, "codec_type": "video", "codec_name": "mjpeg",
		"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}]}`
	webp := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"start_time": "0.000000", "avg_frame_rate": "24/1"}, {"index": 1, "codec_type": "video", "codec_name": "webp",
		"disposition": {"attached_pic": 1}}]}`
	streams := func(codec string, marked bool) []any {
		second := map[string]any{"index": 1, "type": "video", "codec": codec}
		if marked {
			second["dispositions"] = map[string]any{"attachedPic": true}
		}
		return []any{map[string]any{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"}, second}
	}
	for _, c := range []struct {
		name, probe string
		streams     []any
		mark        bool // held by the mark's why (else by another, or not held)
		held        bool
	}{
		{"a tagged cover the record marks", tagged, streams("mjpeg", true), false, false},
		{"a tagged cover an older record leaves unmarked", tagged, streams("mjpeg", false), true, true},
		{"a webp the record marks", webp, streams("webp", true), false, true},
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", c.streams, c.probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		byMark := strings.Contains(g.Unknown, "attachedPic")
		if err != nil || (g.Unknown != "") != c.held || byMark != c.mark {
			t.Errorf("%s: %+v, %v; want it held %v, by its mark %v", c.name, g, err, c.held, c.mark)
		}
		if g, err := ReadGate(d, "v-a", "a"); err != nil || strings.Contains(g.Unknown, "attachedPic") != c.mark {
			t.Errorf("%s, the gate of the records alone: %+v, %v; want it held by its mark %v", c.name, g, err, c.mark)
		}
		// not read yet: no read lets it go, so none is waited on
		if g, err := ReadGateWith(d, "v-a", GateFacts{}, "a"); err != nil || (c.mark &&
			(!strings.Contains(g.Unknown, "attachedPic") || g.OnlyCaptionsUnknown())) {
			t.Errorf("%s, not read yet: %+v, %v; want it held by its mark, no read waited on", c.name, g, err)
		}
	}
}

// On the retire path: an older record that leaves a cover its tags alone
// mark unmarked keeps the original.
func TestTheRetireJobHoldsAMarkTheCoverRuleDoesNotBearOut(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive",
		"start_time": "0.000000", "avg_frame_rate": "24/1"}, {"index": 1, "codec_type": "video", "codec_name": "mjpeg",
		"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}]}`
	f.rewriteRecord(t, func(rec map[string]any) {
		rec["streams"] = append(rec["streams"].([]any), map[string]any{"index": 1, "type": "video", "codec": "mjpeg"})
	}, &probe)
	f.keptFor(t, "an unmarked cover", "attachedPic")
}

// An original-deleted event says the read the gate went on (captionsRead) as
// the record logic keeps one: each window as window_of gives it - one it
// can't read (a keyframeBefore that is no boolean, a count above its frames)
// as a window that decoded nothing, a sound one with the counts
// and the whole read's keys it gave, keyframeBefore only where true, nothing
// else -, the span where both its ends are numbers, the gop where it is one
// from 0, the release of the ffprobe that read it and when. Of no read, one
// without a moment or a report, a report that can't be read (a key given
// twice) or one with no window, it says none.
func TestTheEventSaysTheReadAsTheRecordLogicKeepsIt(t *testing.T) {
	at := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	report := `{"captionWindows": [{"at": 0, "start": 0, "end": 1.8804, "frames": 48, "captions": false,
		"interlacedFrames": 0, "repeatFrames": 0, "whole": true, "packets": 50, "leading": 2, "keyframeBefore": false},
		{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false, "keyframeBefore": "yes"},
		{"at": 0.7, "start": 420, "end": 421.88, "frames": 48, "captions": false, "interlacedFrames": 49},
		{"at": 0.9, "start": 540, "end": 541.88, "frames": 48, "captions": false, "keyframeBefore": true, "seen": 1}],
		"span": {"start": 0, "end": 600}, "gop": 2, "duration": 600}`
	b, err := json.Marshal(CaptionsReadOf(&CaptionsRead{ReadAt: &at, FFprobe: " 7.1.1 ", Report: []byte(report)}))
	want := `{"windows":[{"at":0.0,"start":0.0,"end":1.88,"frames":48,"captions":false,"interlacedFrames":0,` +
		`"repeatFrames":0,"whole":true,"packets":50,"leading":2},{"at":0.5,"start":300.0,"end":300.0,"frames":0,` +
		`"captions":false},{"at":0.7,"start":420.0,"end":420.0,"frames":0,"captions":false},` +
		`{"at":0.9,"start":540.0,"end":541.88,"frames":48,"captions":false,"keyframeBefore":true}],` +
		`"span":{"start":0.0,"end":600.0},"gop":2.0,"ffprobe":"7.1.1","at":"` + Timestamp(at) + `"}`
	if err != nil || string(b) != want {
		t.Errorf("the event's read: %s, %v; want %s", b, err, want)
	}
	unstated := strings.NewReplacer(`"end": 600}`, `"end": "600"}`, `"gop": 2`, `"gop": -1`).Replace(report)
	b, err = json.Marshal(CaptionsReadOf(&CaptionsRead{ReadAt: &at, Report: []byte(unstated)}))
	if err != nil || strings.Contains(string(b), `"span"`) || strings.Contains(string(b), `"gop"`) ||
		strings.Contains(string(b), `"ffprobe"`) {
		t.Errorf("a span with an end as text, a gop below 0 and no ffprobe: %s, %v; want none of them said", b, err)
	}
	aWindow := `{"at": 0, "start": 0, "end": 1.88, "frames": 48, "captions": false}`
	if d := CaptionsReadOf(&CaptionsRead{ReadAt: &at, Report: []byte(`{"captionWindows": [` + aWindow + `]}`)}); d == nil {
		t.Errorf("a read of one window: none said")
	}
	for name, r := range map[string]*CaptionsRead{
		"no read":   nil,
		"no moment": {Report: []byte(report)},
		"no report": {ReadAt: &at},
		"a key twice": {ReadAt: &at, Report: []byte(`{"captionWindows": [` + aWindow + `], "captionWindows": [` +
			aWindow + `]}`)},
		"no window":       {ReadAt: &at, Report: []byte(`{"captionWindows": [], "span": {"start": 0, "end": 600}}`)},
		"windows no list": {ReadAt: &at, Report: []byte(`{"captionWindows": {"at": 0}}`)},
	} {
		if d := CaptionsReadOf(r); d != nil {
			t.Errorf("%s: %v; want no read said", name, d)
		}
	}
}

// What the contract's deletion gate keeps, the gate keeps, beside its own
// rules: where they let an original go and the record logic's reading of the
// same records and of the report its read kept holds it, the gate holds - of
// the closed captions as its unknown, of the fields as interlaced lost, each
// saying the contract's gate keeps it. A kept report that says what the read
// as the gate took it doesn't is such. And the contract's gate reads the
// package of every version of the title, of another source too.
func TestTheGateKeepsWhatTheContractsGateKeeps(t *testing.T) {
	d := t.TempDir()
	librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a",
		"file": map[string]any{"name": "a.file", "sizeBytes": 1}, "essence": fullEssence(), "streams": filmStreams(),
		"container": map[string]any{"format": "matroska,webm", "durationMs": 600000}},
		map[string]string{ProbeFile: progressiveProbe})
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	withReport := func(report string) GateFacts {
		f := read(600, every()...)
		f.Captions.Report = []byte(report)
		return f
	}
	goes := func(g Gate) bool { return g.Unknown == "" && len(g.Beyond()) == 0 }
	if g, err := ReadGateWith(d, "v-a", withReport(noCaptionsReport), "a"); err != nil || !goes(g) {
		t.Fatalf("a clean read and its report: %+v, %v; want it let go", g, err)
	}
	captioned := strings.Replace(noCaptionsReport, `"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false`,
		`"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true`, 1)
	g, err := ReadGateWith(d, "v-a", withReport(captioned), "a")
	if err != nil || !strings.HasPrefix(g.Unknown, RecordLogicKeeps+" for its closed captions: ") ||
		slices.Contains(g.Lost, "interlaced") {
		t.Errorf("a report that shows captions the read doesn't: %+v, %v; want the contract's gate keeping its captions",
			g, err)
	}
	if g, err := ReadGateWith(d, "v-a", GateFacts{Captions: withReport(captioned).Captions, CaptionsNotAsked: true}, "a"); err != nil ||
		strings.HasPrefix(g.Unknown, RecordLogicKeeps) {
		t.Errorf("the same, its captions not asked of: %+v, %v; want them not held by the contract's gate", g, err)
	}
	fields := strings.Replace(noCaptionsReport, `"at": 0.3, "start": 180, "end": 181.88, "frames": 48, "captions": false, `+
		`"interlacedFrames": 0`, `"at": 0.3, "start": 180, "end": 181.88, "frames": 48, "captions": false, "interlacedFrames": 5`, 1)
	g, err = ReadGateWith(d, "v-a", withReport(fields), "a")
	if err != nil || !slices.Contains(g.Lost, "interlaced") || !strings.HasPrefix(g.Scan, FieldsKept+": "+RecordLogicKeeps+": ") {
		t.Errorf("a report that counts interlaced frames the read doesn't: %+v, %v; want the contract's gate keeping its "+
			"fields", g, err)
	}
	// the contract's gate of every version's package
	rec, err := readRecordDoc(filepath.Join(SourceDir(d, "a"), "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := readRecordDoc(filepath.Join(VersionDir(d, "v-a"), PackageFile))
	if err != nil {
		t.Fatal(err)
	}
	if held := recordLogicOf(d, "v-a", []Doc{rec}, []string{"a"}, pkg, withReport(noCaptionsReport)); len(held) != 0 {
		t.Fatalf("the contract's gate of one version: %v; want nothing held", held)
	}
	packageOf(t, d, "v-b", map[string]any{"found": "fields"}, "b")
	if held := recordLogicOf(d, "v-a", []Doc{rec}, []string{"a"}, pkg, withReport(noCaptionsReport)); !strings.Contains(
		held["interlaced"], "found fields") {
		t.Errorf("the contract's gate with another source's version that found fields: %v; want its fields held", held)
	}
	// and of one whose package record can't be read
	if err := os.RemoveAll(VersionDir(d, "v-b")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(VersionDir(d, "v-c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(VersionDir(d, "v-c"), PackageFile), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if held := recordLogicOf(d, "v-a", []Doc{rec}, []string{"a"}, pkg, withReport(noCaptionsReport)); !strings.Contains(
		held["interlaced"], "a package record of the title can't be read") {
		t.Errorf("the contract's gate with a version whose package can't be read: %v; want its fields held", held)
	}
}

// The gate's own rules read what the record logic reads of a record and a
// probe: a caption window the record keeps of its picture that shows
// captions carries them, and one that counted interlaced frames, or gives a
// count of them that can't be read (or windows that are no list), puts it in
// fields; a stream whose dispositions are no object, and not empty
// (picture_of can't read them), makes the record one that can't be read;
// and a probe that gives no format (an object) can't be read.
func TestTheGatesOwnRulesReadWhatTheRecordLogicReads(t *testing.T) {
	withWindows := func(windows string) Doc {
		d := film(600)
		streams, _ := d.Get("streams")
		pic := streams.([]any)[0].(Doc).With("captionWindows", doc(t, `{"x": `+windows+`}`)[0].Value)
		return d.With("streams", []any{pic})
	}
	window := `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": `
	for windows, want := range map[string]bool{
		`[` + window + `true}]`:                         true,
		`[` + window + `false}]`:                        false,
		`[` + window + `true, "interlacedFrames": 49}]`: false, // no window: a failed one
	} {
		carried, _ := captionsRule([]Doc{withWindows(windows)}, nil, read(600, every()...))
		if got := strings.Contains(carried, "a caption window its record keeps of its picture shows them"); got != want {
			t.Errorf("the record's windows %s: carried %q; want them carried by its windows %v", windows, carried, want)
		}
	}
	for windows, want := range map[string]bool{
		`[` + window + `false, "interlacedFrames": 0, "repeatFrames": 0}]`: false,
		`[` + window + `false, "interlacedFrames": 3}]`:                    true,
		`[` + window + `false, "interlacedFrames": "0"}]`:                  true,
		`[` + window + `false, "repeatFrames": 49}]`:                       true,
		`"none"`: true,
	} {
		why, _ := inFields(t.TempDir(), "", nil, []string{"s1"}, []Doc{withWindows(windows)}, nil, nil)
		if got := strings.HasPrefix(why, "a caption window its record keeps of its picture counted"); got != want {
			t.Errorf("the record's windows %s: %q; want them in fields %v", windows, why, want)
		}
	}
	for dispositions, want := range map[string]string{
		`{"attachedPic": false}`: "", `{}`: "", `null`: "", `[]`: "", `""`: "", `0`: "", `false`: "",
		`["attachedPic"]`: "a stream of it gives dispositions that are no object",
		`"attachedPic"`:   "a stream of it gives dispositions that are no object",
		`1`:               "a stream of it gives dispositions that are no object",
	} {
		rec := doc(t, `{"essence": {}, "streams": [{"index": 0, "type": "video", "dispositions": `+dispositions+`}]}`)
		if got := recordShape(rec); got != want {
			t.Errorf("dispositions %s: %q; want %q", dispositions, got, want)
		}
	}
	for format, want := range map[string]string{
		`, "format": {}`: "", `, "format": {"format_name": "matroska,webm"}`: "",
		``: "it gives no format (an object)", `, "format": null`: "it gives no format (an object)",
		`, "format": "matroska,webm"`: "it gives no format (an object)", `, "format": []`: "it gives no format (an object)",
	} {
		if got := probeShape(doc(t, `{"streams": [{"index": 0, "codec_type": "video"}]`+format+`}`)); got != want {
			t.Errorf("a probe of format %q: %q; want %q", format, got, want)
		}
	}
}

// The contract's gate keeps both of an original whose stored probe gives no
// one film (none: a cover alone; or two), whatever its record's picture says,
// one that gives no index too (none to match the probe's by).
func TestTheContractsGateKeepsAProbeOfNoOneFilm(t *testing.T) {
	rec := doc(t, `{"essence": {"interlaced": false, "closedCaptions": false}, "container": {"format": "matroska,webm",
		"durationMs": 600000}, "offList": [], "streams": [{"type": "video", "codec": "h264", "fieldOrder": "progressive"}]}`)
	for name, streams := range map[string]string{
		"a cover alone": `{"index": 1, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": 1}}`,
		"two films": `{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"},
			{"index": 1, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"}`,
	} {
		probe := doc(t, `{"format": {"format_name": "matroska,webm"}, "streams": [`+streams+`]}`)
		held := RecordLogicHolds(rec, probe, nil, nil, nil)
		for _, k := range []string{"closedCaptions", "interlaced"} {
			if !strings.Contains(held[k], "its probe gives no one film") {
				t.Errorf("%s: %s held %q; want it held for the probe's film", name, k, held[k])
			}
		}
	}
}
