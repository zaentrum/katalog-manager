package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The gate takes a source record, a probe and a package only in the forms
// the platform's writers write - json.dumps(doc, indent=2,
// ensure_ascii=False) in UTF-8 -, whatever their layout: a text as Python's
// encoder writes it, a whole number in plain digits within 64 bits, a float
// as Python's repr writes it (1e-05 and 1e+16 its exponents), keys of no
// space about them; anything else, with the reason.
func TestARecordIsTakenInTheFormsItsWritersWrite(t *testing.T) {
	for text, want := range map[string]string{
		`{"a": 1}`: "",
		"{\n  \"a\": [\n    1,\n    -2\n  ]\n}\n":                                                          "",
		`{"a": 0.1, "b": -0.0, "c": 1e-05, "d": 1e+16, "e": 1.5e-07, "f": 123456789012345.6, "g": 1e+308}`: "",
		`{"a": 9223372036854775807, "b": -9223372036854775808}`:                                            "",
		`{"a": "é", "b": "\n\r\t\b\f\"\\", "c": "\u0000\u001f", "d": "/", "e": "` + "\u2028\u007f" + `"}`:  "",
		`{"a": 7.0}`:                    "",
		`{"a": 1.0e-05}`:                "written \"1.0e-05\", which its writers write 1e-05",
		`{"a": 1e-5}`:                   "written \"1e-5\", which its writers write 1e-05",
		`{"a": 1E+16}`:                  "written \"1E+16\"",
		`{"a": 1e16}`:                   "written \"1e16\"",
		`{"a": 1e2}`:                    "written \"1e2\", which its writers write 100.0",
		`{"a": 0.10}`:                   "written \"0.10\", which its writers write 0.1",
		`{"a": 1e-1000001}`:             "written \"1e-1000001\", which its writers write 0.0",
		`{"a": 1e999}`:                  "past what a float holds",
		`{"a": -0}`:                     "-0, a whole number no writer writes",
		`{"a": 9223372036854775808}`:    "past 64 bits",
		`{"a": 0.30000000000000004441}`: "written \"0.30000000000000004441\"",
		`{"a": "\u0041"}`:               "a text of it is written \"\\\"\\\\u0041\\\"\", which its writers write \"\\\"A\\\"\"",
		`{"a": "\/"}`:                   "a text of it is written",
		`{"a": "\ud800"}`:               "a text of it is written",
		`{"a": "\u00e9"}`:               "a text of it is written",
		`{"a": "\u001F"}`:               "a text of it is written",
		`{"\u0061": 1}`:                 "a text of it is written",
		`{" a": 1}`:                     "its key \" a\" has space about it",
		`{"a ": 1}`:                     "its key \"a \" has space about it",
		"{\"a\\u001c\": 1}":             "has space about it",
		"{\"a\u00a0\": 1}":              "has space about it",
		`{"a": " b "}`:                  "",
		"{\"a\": \"\xe9\"}":             "its bytes are no UTF-8",
	} {
		got := writtenForm([]byte(text))
		if (got == "") != (want == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q; want %q", text, got, want)
		}
	}
	// the validator's own rules of texts and keys
	for text, want := range map[string]string{
		`{"container": {"title": "A film"}}`:                     "",
		`{"container": {"title": "A film\n"}}`:                   "its title \"A film\\n\" contains a line break",
		`{"container": {"title": "A\nfilm"}}`:                    "",
		`{"takenBy": "an analyzer\r"}`:                           "its takenBy",
		`{"file": {"name": "a\rb"}}`:                             "its name",
		`{"streams": [{"language": "e\nn"}]}`:                    "its language",
		`{"essence": {"audioLanguages": ["en\n"]}}`:              "its audioLanguages",
		`{"streams": [{"language": ["e\nn"]}]}`:                  "its language",
		`{"probe": {"note": "a note\n"}}`:                        "",
		`{"container": {"tags": {"title ": "x"}}}`:               "its key \"title \" has space about it",
		"{\"container\": {\"tags\": {\"\\u001ftitle\": \"x\"}}}": "has space about it",
	} {
		d, err := LoadRecord([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		got := textRules(d, "")
		if (got == "") != (want == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %q; want %q", text, got, want)
		}
	}
	if _, err := LoadRecord([]byte("{\"a\": \"\xff\"}")); err == nil {
		t.Error("bytes that are no UTF-8: read; want them refused")
	}
	// a read's report in bytes that are no UTF-8 is none the record logic
	// reads (its loaders refuse them)
	if r := RecordLogicRead([]byte("{\"captionWindows\": [], \"note\": \"caf\xe9\"}")); r != nil {
		t.Errorf("a report that is no UTF-8: %v; want none read", r)
	}
	if r := RecordLogicRead([]byte(`{"captionWindows": [], "note": "café"}`)); r == nil {
		t.Error("a report in UTF-8: none read; want it read")
	}
}

// At the gate: a source record, a probe or a package in a form no writer
// writes, or one the validator's rules of texts and keys take for none,
// keeps the original, saying why - a hold, no failure; one that is no JSON
// can't be read, as ever.
func TestTheGateKeepsARecordInAFormNoWriterWrites(t *testing.T) {
	for _, c := range []struct {
		name, file, old, new, want string
	}{
		{"a source record's number as 6e5", "source.json", `600000`, `6e5`, "no form the platform's writers write"},
		{"a source record's text escaped", "source.json", `"h264"`, `"\u0068264"`, "no form the platform's writers write"},
		{"a source record's bytes no UTF-8", "source.json", `"h264"`, "\"h\xe9264\"", "no UTF-8"},
		{"a source record's title ending in a line break", "source.json", `"durationMs"`, `"title": "A film\n", "durationMs"`,
			"contains a line break"},
		{"a source record's key twice", "source.json", `"durationMs"`, `"zz": 1, "zz": 2, "durationMs"`, "given twice"},
		{"a source record's number past a float", "source.json", `600000`, `1e999`, "past what a float holds"},
		{"a probe's number as 2.40e1", ProbeFile, `"index": 0`, `"index": 0, "level": 2.40e1`, "no form the platform's writers write"},
		{"a probe's key with space", ProbeFile, `"index": 0`, `"index": 0, "level ": 1`, "has space about it"},
		{"a package's bytes no UTF-8", PackageFile, `"packager"`, "\"pack\xffager\"", "no UTF-8"},
		{"a package's number as -0", PackageFile, `"bytes": `, `"zz": -0, "bytes": `, "-0"},
		{"a package's text ending in a line break", PackageFile, `"packager"`, `"packager\n"`, "contains a line break"},
		{"a package's key twice", PackageFile, `"bytes": `, `"zz": 1, "zz": 1, "bytes": `, "given twice"},
		{"a source record that is no JSON", "source.json", `"h264"`, `"h264`, ""},
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		p := filepath.Join(SourceDir(d, "a"), c.file)
		if c.file == PackageFile {
			p = filepath.Join(VersionDir(d, "v-a"), PackageFile)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), c.old) {
			t.Fatalf("%s: %s gives no %s", c.name, c.file, c.old)
		}
		librarytest.Write(t, p, []byte(strings.Replace(string(b), c.old, c.new, 1)))
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: %+v; want it unread, as ever", c.name, g)
			}
			continue
		}
		if err != nil || (g.Unknown == "" && len(g.Beyond()) == 0) || !strings.Contains(g.Unknown+g.Scan, c.want) {
			t.Errorf("%s: %+v, %v; want it kept, %q", c.name, g, err, c.want)
		}
	}
}

// The package of another version of the title, which the scan rule reads,
// is held to its writers' forms too.
func TestAnotherVersionsPackageInAFormNoWriterWritesHolds(t *testing.T) {
	d := t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	packageOf(t, d, "v-b", verifiedProgressive(), "a")
	if g, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err != nil || g.Unknown != "" || len(g.Beyond()) != 0 {
		t.Fatalf("two versions in their writers' forms: %+v, %v; want it let go", g, err)
	}
	p := filepath.Join(VersionDir(d, "v-b"), PackageFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, p, []byte(strings.Replace(string(b), `"bytes": `, `"zz": 1e2, "bytes": `, 1)))
	if g, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err != nil || !strings.Contains(g.Scan,
		"its package of version v-b can't be read") || !strings.Contains(g.Scan, "no form the platform's writers write") {
		t.Errorf("the other version's package in a form no writer writes: %+v, %v; want its fields held, why said", g, err)
	}
}

// A number no writer writes never reaches the schema library: a window's at
// written 1e-1000001, on which that library fails, is a form the record is
// held for; and where a check fails all the same, the record is refused, no
// crash.
func TestANumberNoWriterWritesNeverReachesTheSchema(t *testing.T) {
	text := strings.Replace(exampleRecord(t), `"fieldOrder": "progressive"`, `"captionWindows": [{"at": 1e-1000001, "start": 0, `+
		`"end": 1.6, "frames": 48, "captions": false}], "fieldOrder": "progressive"`, 1)
	if !strings.Contains(text, "1e-1000001") {
		t.Fatal("the example's picture gives no field order to write the windows beside")
	}
	if _, why := sourceRecordTaken([]byte(text), exampleSource); !strings.Contains(why, "no form the platform's writers write") {
		t.Errorf("a window's at of 1e-1000001: %q; want the form held", why)
	}
	rec, err := LoadRecord([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	// the schema library fails on it (a nil dereference of its own): refused
	if why := sourceRecordRefused(rec, exampleSource); !strings.Contains(why, "it failed") {
		t.Errorf("the same record past the form: %q; want it refused for the schema's failing", why)
	}
}

// A reading of the gate that fails - here its check of a source record
// panics - is no crash: the retire pass keeps that original, saying the gate
// failed, and goes on to the next title, whose original it retires.
func TestAGateThatFailsKeepsItsOriginalAndThePassGoesOn(t *testing.T) {
	f := newRetireFixture(t)
	const film2, src2, ver2, pkg2 = "7e7e7e7e-1111-4222-8333-444444444444", "8f8f8f8f-1111-4222-8333-444444444444",
		"9a9a9a9a-1111-4222-8333-444444444444", "abababab-1111-4222-8333-444444444444"
	f.addMovie(t, film2, src2, ver2, pkg2, "Second Film")
	keep := sourceRecordCheck
	sourceRecordCheck = func(rec Doc, folder string) string {
		if folder == rtSource {
			panic("a check that fails")
		}
		return keep(rec, folder)
	}
	t.Cleanup(func() { sourceRecordCheck = keep })
	rep := f.pass(t)
	if s := f.source(t, rtSource); s.State == SourceDeleted || !strings.Contains(deref(s.Error), GateFailed) ||
		!strings.Contains(deref(s.Error), "a check that fails") {
		t.Errorf("the original whose gate failed: %s (%s); want it kept, the gate's failing said", s.State, deref(s.Error))
	}
	if s := f.source(t, src2); s.State != SourceDeleted || rep.Originals != 1 {
		t.Errorf("the next title's original: %s; the pass %+v; want it retired", s.State, rep)
	}
}
