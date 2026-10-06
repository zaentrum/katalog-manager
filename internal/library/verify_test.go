package library_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

func aVersion(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "versions", "v1")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000002",
		PackageID: "4f1d0000-0000-4000-8000-000000000005", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z"})
	return dir
}

// A version's chain holds when every link does: .complete is package.json's
// hash, package.json holds the checksums' hash, count and bytes, the
// checksums list exactly version.json and the package's files, and
// version.json is the one listed. In full, every file's hash is checked too.
func TestAVersionsChainIsVerified(t *testing.T) {
	dir := aVersion(t)
	for _, full := range []bool{false, true} {
		c, err := library.VerifyVersion(dir, full)
		if err != nil {
			t.Fatalf("full %v: %v", full, err)
		}
		if len(c.Listed) != 10 || c.Complete == "" || str(c.Package, "packageId") != "4f1d0000-0000-4000-8000-000000000005" {
			t.Errorf("full %v: %+v", full, c)
		}
	}
	// A file of the package changed: the chain holds, the full check does not.
	librarytest.Write(t, filepath.Join(dir, "hls/v0/seg0.m4s"), []byte("another segment, of its size")[:24])
	if _, err := library.VerifyVersion(dir, false); err != nil {
		t.Errorf("a changed segment broke the chain: %v", err)
	}
	if _, err := library.VerifyVersion(dir, true); !brokenAt(err, "hls/v0/seg0.m4s", "its hash is") {
		t.Errorf("a changed segment, in full: %v", err)
	}
}

func brokenAt(err error, file, says string) bool {
	var b *library.Broken
	return errors.As(err, &b) && strings.HasSuffix(b.File, file) && strings.Contains(b.Reason, says)
}

func str(d library.Doc, key string) string {
	v, _ := d.Get(key)
	s, _ := v.(string)
	return s
}

// Each link that does not hold breaks the chain, naming its file.
func TestEachBrokenLinkIsNamed(t *testing.T) {
	for _, c := range []struct {
		name string
		harm func(dir string)
		file string
		says string
	}{
		{"no .complete", func(d string) { os.Remove(filepath.Join(d, ".complete")) }, ".complete", "no such file"},
		{".complete of another package.json", func(d string) {
			os.WriteFile(filepath.Join(d, ".complete"), []byte("sha256:"+strings.Repeat("0", 64)+"\n"), 0o664)
		}, ".complete", "package.json's hash is"},
		{"checksums changed", func(d string) {
			f := filepath.Join(d, "checksums.sha256")
			b, _ := os.ReadFile(f)
			os.WriteFile(f, append(b, '\n'), 0o664)
		}, "checksums.sha256", "package.json says its hash is"},
		{"a listed file gone", func(d string) { os.Remove(filepath.Join(d, "subs/1.vtt")) }, "subs/1.vtt", "it is not there"},
		{"a file unlisted", func(d string) { os.WriteFile(filepath.Join(d, "hls/v0/seg9.m4s"), []byte("x"), 0o664) },
			"hls/v0/seg9.m4s", "not listed"},
		{"version.json changed", func(d string) {
			f := filepath.Join(d, "version.json")
			b, _ := os.ReadFile(f)
			os.WriteFile(f, []byte(strings.Replace(string(b), "packager", "packagers", 1)), 0o664)
		}, "version.json", "its hash is"},
		{"a file that grew", func(d string) {
			os.WriteFile(filepath.Join(d, "trickplay/sprite-0001.jpg"), []byte("\xff\xd8 a larger sprite"), 0o664)
		}, "checksums.sha256", "the files it lists hold"},
	} {
		dir := aVersion(t)
		c.harm(dir)
		if _, err := library.VerifyVersion(dir, false); !brokenAt(err, c.file, c.says) {
			t.Errorf("%s: %v, want %s broken saying %q", c.name, err, c.file, c.says)
		}
	}
	// OS artefacts beside the package are no files of it.
	dir := aVersion(t)
	librarytest.Write(t, filepath.Join(dir, "hls/.DS_Store"), []byte("x"))
	librarytest.Write(t, filepath.Join(dir, "subs/@eaDir/thumb"), []byte("x"))
	if _, err := library.VerifyVersion(dir, true); err != nil {
		t.Errorf("with OS artefacts: %v", err)
	}
}

// An extra's chain is a version's, extra.json its record.
func TestAnExtrasChainIsVerified(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extras", "x1")
	librarytest.WriteExtra(t, dir, librarytest.Extra{ExtraID: "16aa0000-0000-4000-8000-000000000009",
		PackageID: "5e1d0000-0000-4000-8000-000000000001", CreatedAt: "2026-10-06T09:00:00Z"})
	if _, err := library.VerifyExtra(dir, true); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "extra.json"))
	if _, err := library.VerifyExtra(dir, false); !brokenAt(err, "extra.json", "it is not there") {
		t.Errorf("without extra.json: %v", err)
	}
}
