package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// exampleRecord is the source record of the example library's episode the
// record logic wrote, which its validator takes.
func exampleRecord(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractDir, "library", "v2", "examples", "series", exampleSeries[:2],
		exampleSeries, "episodes", exampleEpisode, "sources", exampleSource, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The contract's validator takes a source record only where its schema
// does and it names its folder; the gate reads the record the same way
// (sourceRecordRefused): the example the record logic wrote is taken, in its
// own folder alone; a key the schema doesn't know, one it requires left out,
// a fact of another type, a value off its pattern or its format, another
// id, none. A number Python's json reads as no int (7.0, 1e2) or whose int
// is past 64 bits is no integer to the validator, as it is none here; one
// such where a number stands is taken.
func TestTheContractsSchemaOfASourceRecord(t *testing.T) {
	if _, err := sourceSchema(); err != nil {
		t.Fatalf("the contract's schema of a source record: %v", err)
	}
	text := exampleRecord(t)
	for _, c := range []struct {
		name, old, new string
		folder         string
		want           string // in why it refuses it, "" taken
	}{
		{"the example", "", "", exampleSource, ""},
		{"in another folder", "", "", "a0a0a0a0-0000-4000-8000-0000000000a0", "doesn't name its folder"},
		{"a key it doesn't know", `"schema": `, `"zz": 1, "schema": `, exampleSource, "additional properties 'zz'"},
		{"no takenAt", `"takenAt": "2026-09-18T09:05:00Z",`, ``, exampleSource, "missing property 'takenAt'"},
		{"a size as 53.0", `"sizeBytes": 53,`, `"sizeBytes": 53.0,`, exampleSource, "/file/sizeBytes"},
		{"a size as 5.3e1", `"sizeBytes": 53,`, `"sizeBytes": 5.3e1,`, exampleSource, "/file/sizeBytes"},
		{"a size past 64 bits", `"sizeBytes": 53,`, `"sizeBytes": 9223372036854775808,`, exampleSource, "/file/sizeBytes"},
		{"a size of 2^63 - 1", `"sizeBytes": 53,`, `"sizeBytes": 9223372036854775807,`, exampleSource, ""},
		{"a size as text", `"sizeBytes": 53,`, `"sizeBytes": "53",`, exampleSource, "/file/sizeBytes"},
		{"a size below 0", `"sizeBytes": 53,`, `"sizeBytes": -1,`, exampleSource, "/file/sizeBytes"},
		{"a time of no day", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-02-30T09:05:00Z"`, exampleSource,
			"/takenAt"},
		{"a time of a leap second", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-09-18T23:59:60Z"`,
			exampleSource, "/takenAt"},
		{"a time of the year 0", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "0000-09-18T09:05:00Z"`, exampleSource,
			"/takenAt"},
		{"a time 24 hours off UTC", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-09-18T09:05:00+24:00"`,
			exampleSource, "/takenAt"},
		{"a time 23:60 off UTC", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-09-18T09:05:00-23:60"`,
			exampleSource, "/takenAt"},
		{"a time 23:59 off UTC", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-09-18T09:05:00-23:59"`,
			exampleSource, ""},
		{"a time in lower case", `"takenAt": "2026-09-18T09:05:00Z"`, `"takenAt": "2026-09-18t09:05:00z"`, exampleSource,
			"/takenAt"},
		{"a name of ..", `"name": "original.mkv"`, `"name": ".."`, exampleSource, "/file/name"},
		{"a name with a /", `"name": "original.mkv"`, `"name": "a/original.mkv"`, exampleSource, "/file/name"},
		{"a name of two lines", `"name": "original.mkv"`, `"name": "original.mkv\n"`, exampleSource, "/file/name"},
		{"an id in capitals", `"sourceId": "9c9e9620`, `"sourceId": "9C9E9620`, exampleSource, "/sourceId"},
	} {
		changed := text
		if c.old != "" {
			if !strings.Contains(text, c.old) {
				t.Fatalf("%s: the example gives no %s", c.name, c.old)
			}
			changed = strings.Replace(text, c.old, c.new, 1)
		}
		rec, err := LoadRecord([]byte(changed))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		why := sourceRecordRefused(rec, c.folder)
		if (why == "") != (c.want == "") || !strings.Contains(why, c.want) {
			t.Errorf("%s: %q; want %q", c.name, why, c.want)
		}
	}
	// the patterns Go's regular expressions can't read (they look ahead),
	// as Python reads them of a string of one line
	for pattern, cases := range map[string]map[string]bool{
		`^(?!\.{1,2}$)[^/]+$`: {"original.mkv": true, ".x": true, "...": true, "": false, ".": false, "..": false,
			"a/b": false, "a\n": false},
		`^(?!/)(?!.*(^|/)\.\.(/|$)).+$`: {"sources/x/ffprobe.json": true, "a/..b": true, "a//b": true, "": false,
			"/a": false, "..": false, "../a": false, "a/..": false, "a/../b": false, "a\nb": false},
	} {
		re, err := schemaPattern(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for s, want := range cases {
			if got := re.MatchString(s); got != want {
				t.Errorf("%s of %q: %v; want %v", pattern, s, got, want)
			}
		}
	}
	if _, err := schemaPattern(`^(?=x)`); err == nil {
		t.Error("a pattern Go can't read, of no field the schema gives: compiled; want it refused, so no record is taken")
	}
}

// The gate keeps the original of a source record the contract's schema
// refuses, a record that says everything the gate reads all the same, and
// says so; of the same record that the schema takes, it lets it go.
func TestTheGateKeepsARecordTheContractsSchemaRefuses(t *testing.T) {
	for name, extra := range map[string]map[string]any{"taken": nil, "a key it doesn't know": {"zz": true},
		"a takenBy that is no text": {"takenBy": 5}} {
		d := t.TempDir()
		rec := map[string]any{"sourceId": "a", "file": map[string]any{"name": "a.file", "sizeBytes": 1},
			"essence": fullEssence(), "streams": filmStreams(),
			"container": map[string]any{"format": "matroska,webm", "durationMs": 600000}}
		for k, v := range extra {
			rec[k] = v
		}
		librarytest.WriteSource(t, SourceDir(d, "a"), rec, map[string]string{ProbeFile: progressiveProbe})
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		refused := strings.Contains(g.Unknown, "its source record is none the contract takes")
		if err != nil || refused != (extra != nil) || (extra == nil && g.Unknown != "") {
			t.Errorf("%s: %+v, %v; want it refused %v", name, g, err, extra != nil)
		}
	}
}

// A window may start at any second, as the schema allows a number: one
// before 0, of a picture whose clock starts before 0, is read, not failed;
// and a picture whose clock starts at -5 s, read in its six windows, goes.
func TestAWindowBeforeZeroIsRead(t *testing.T) {
	var w CaptionWindow
	if err := json.Unmarshal([]byte(`{"at": 0, "start": -5, "end": -3.12, "frames": 48, "captions": false,
		"interlacedFrames": 0, "repeatFrames": 0}`), &w); err != nil || w.Unreadable != "" || w.Start != -5 {
		t.Errorf("a window from -5 s: %+v, %v; want it read", w, err)
	}
	d := t.TempDir()
	probe := strings.Replace(progressiveProbe, `"start_time": "0.000000"`, `"start_time": "-5.000000"`, 1)
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), probe)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	var ws []CaptionWindow
	for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
		ws = append(ws, window(at, -5+at*600, 48, false))
	}
	f := read(600, ws...)
	f.Captions.Span = &Span{Start: -5, End: 595}
	if g, err := ReadGateWith(d, "v-a", f, "a"); err != nil || g.Unknown != "" || len(g.Beyond()) != 0 {
		t.Errorf("a picture from -5 s read in its windows: %+v, %v; want it let go", g, err)
	}
}

// The plan places a window at start + at x (end - start), the product
// rounded before it is added, as Python computes it, and a window's reach at
// packets x step + gop + 0.5 alike: never one fused multiply-add, which
// rounds once (the compiler fuses one on arm64, and on amd64 built for
// x86-64-v3). Of a picture of 600 s at 50 a second that starts at 0.08 s,
// the window at 10 % lies at 60.08 s, not 60.080000000000005.
func TestThePlacesAreRoundedAsPythonRoundsThem(t *testing.T) {
	start, at, length, step := fusedValues()
	s := Span{Start: start, End: start + length, Step: step}
	plan := &contractPlan{start: s.Start, end: s.End, step: step}
	want := 60.08
	if got := s.Place(at); got != want {
		t.Errorf("the gate's place: %.17g; want %.17g", got, want)
	}
	if got := contractPlaces(plan)[1]; got != want {
		t.Errorf("the port's place: %.17g; want %.17g", got, want)
	}
	// 48 x 1/3 + 0.1, rounded twice (16.1) and once (16.099999999999998)
	third, gop := fusedReach()
	if got := shortReach(Span{Step: third}, gop); got != 16.1+ShortReachSlack {
		t.Errorf("the gate's reach: %.17g; want %.17g", got, 16.1+ShortReachSlack)
	}
	if got := contractReach(&contractPlan{step: third}, gop); got != 16.1+ShortReachSlack {
		t.Errorf("the port's reach: %.17g; want %.17g", got, 16.1+ShortReachSlack)
	}
}

//go:noinline
func fusedValues() (start, at, length, step float64) { return 0.08, 0.1, 600, 1.0 / 50 }

//go:noinline
func fusedReach() (step, gop float64) { return 1.0 / 3, 0.1 }
