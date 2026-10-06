package library

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// A record is written byte for byte as the library's tools write it (Python's
// json.dumps(doc, indent=2, ensure_ascii=False) and a newline): read and
// written again, testdata/record/doc.json, which Python wrote, is the same
// bytes, its keys in their order, its numbers as written, every character as
// it is but those JSON escapes.
func TestARecordIsWrittenAsTheToolsWriteIt(t *testing.T) {
	want, err := os.ReadFile("testdata/record/doc.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("written again:\n%s\nwant:\n%s", got, want)
	}
	// Built rather than read: the same bytes.
	doc := Doc{{"b", []any{}}, {"a", Doc{}}, {"s", "é\"\\\n\x01"}, {"n", nil}, {"t", true}, {"i", int64(-3)},
		{"p", (*string)(nil)}, {"l", []string{"x", "y"}}}
	got, err = Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{\n  \"b\": [],\n  \"a\": {},\n  \"s\": \"é\\\"\\\\\\n\\u0001\",\n  \"n\": null,\n  \"t\": true,\n  \"i\": -3,\n"+
		"  \"p\": null,\n  \"l\": [\n    \"x\",\n    \"y\"\n  ]\n}\n" {
		t.Errorf("built:\n%s", got)
	}
	if _, err := Encode(map[string]any{"a": 1}); err == nil {
		t.Error("a map, whose keys have no order, was written")
	}
}

// A float is written as Python writes one: testdata/record/floats.json holds
// what Python's repr and json.dumps make of each.
func TestAFloatIsWrittenAsPythonWritesIt(t *testing.T) {
	raw, err := os.ReadFile("testdata/record/floats.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases [][2]string
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		f, err := strconv.ParseFloat(c[0], 64)
		if err != nil {
			t.Fatal(err)
		}
		got, err := pyFloat(f)
		if err != nil || got != c[1] {
			t.Errorf("%s: %q, %v, want %q", c[0], got, err, c[1])
		}
	}
	if _, err := pyFloat(math.NaN()); err == nil {
		t.Error("NaN was written")
	}
}

// A file is written whole or not at all, group-writable, and a checksums
// file lists exactly the files it covers, by path.
func TestWriteFileAndChecksums(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a", "b", "item.json")
	if err := WriteFile(p, []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "two\n" {
		t.Errorf("the file holds %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("the folder holds %d entries, want the file alone (no temporary file left)", len(entries))
	}
	if err := WriteCovered(filepath.Join(dir, "c"), map[string][]byte{"z.json": []byte("z"), "a.json": []byte("a")}); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(dir, "c", SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	if want := SHA256([]byte("a")) + "  a.json\n" + SHA256([]byte("z")) + "  z.json\n"; string(sums) != want {
		t.Errorf("checksums:\n%s\nwant:\n%s", sums, want)
	}
	listed, err := ReadChecksums(filepath.Join(dir, "c", SumsFile))
	if err != nil || len(listed) != 2 || listed["z.json"] != SHA256([]byte("z")) {
		t.Errorf("read back: %v, %v", listed, err)
	}
	for _, bad := range []string{"nohash  a\n", SHA256(nil) + " a\n", SHA256(nil) + "  a\n" + SHA256(nil) + "  a\n"} {
		f := filepath.Join(dir, "bad")
		if err := os.WriteFile(f, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadChecksums(f); err == nil {
			t.Errorf("%q read as checksums", bad)
		}
	}
}
