package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The deletion gate is the validator's: testdata/gate/cases.json holds what
// validate-library-v2.py's deletion_gate gives for each case, and the port
// gives the same, sorted.
func TestTheDeletionGateIsTheValidators(t *testing.T) {
	raw, err := os.ReadFile("testdata/gate/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases.([]any) {
		d := c.(Doc)
		name := str(d, "name")
		var sources []Doc
		ss, _ := d.Get("sources")
		for _, s := range ss.([]any) {
			sources = append(sources, s.(Doc))
		}
		p, _ := d.Get("package")
		want := stringsOf(must2(d.Get("gate")))
		got := DeletionGate(sources, p.(Doc))
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s:\n got  %v\n want %v", name, got, want)
		}
	}
}

func must2(v any, _ bool) any { return v }

// The gate of a version is read from its records: the essence of its
// sources' source.json minus its package.json's.
func TestGateOfReadsTheRecords(t *testing.T) {
	item := t.TempDir()
	librarytest.WriteSource(t, SourceDir(item, "s1"), map[string]any{"essence": map[string]any{
		"surround": true, "maxAudioChannels": 6, "subtitleLanguages": []string{"de", "en"}}}, nil)
	librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
		SourceIDs: []string{"s1"}, CreatedAt: "2026-10-06T09:00:00Z", Package: map[string]any{"essence": map[string]any{
			"surround": false, "maxAudioChannels": 2, "subtitleLanguages": []string{"en"}}}})
	got, err := GateOf(item, "v1", "s1")
	if err != nil || strings.Join(got, " ") != "maxAudioChannels subtitleLanguages:de surround" {
		t.Errorf("the gate: %v, %v", got, err)
	}
	if _, err := GateOf(item, "v1", "s2"); err == nil {
		t.Error("the gate of a source without a record")
	}
	if _, err := GateOf(filepath.Join(item, "none"), "v1", "s1"); err == nil {
		t.Error("the gate of a version without a record")
	}
}
