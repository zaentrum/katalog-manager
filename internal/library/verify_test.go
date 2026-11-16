package library_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

// mendInit changes the version's hls/v0/init.mp4 to each of inits in turn,
// same size, and records an entry per change in package.json's mends —
// sha256Before and sha256After filled in where entries leaves them out —
// with the file's line in checksums.sha256, package.json's hash of it and
// .complete written again, as the schemas' library-v2-mend.py mends a
// package. summed false leaves the checksums as they were.
func mendInit(t *testing.T, dir string, summed bool, inits [][]byte, entries ...map[string]any) {
	t.Helper()
	p := filepath.Join(dir, "hls/v0/init.mp4")
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	prev := "sha256:" + library.SHA256(first)
	var list []any
	for i, init := range inits {
		librarytest.Write(t, p, init)
		e := map[string]any{"kind": "avsync-editlist", "file": "hls/v0/init.mp4", "box": "moov/trak/edts/elst", "old": 1328,
			"new": 0, "sha256Before": prev, "sha256After": "sha256:" + library.SHA256(init),
			"at": fmt.Sprintf("2026-10-09T02:00:0%dZ", i), "by": "an operator"}
		if i < len(entries) {
			for k, v := range entries[i] {
				e[k] = v
			}
		}
		prev = "sha256:" + library.SHA256(init)
		list = append(list, e)
	}
	var pkg map[string]any
	pj := filepath.Join(dir, "package.json")
	b, err := os.ReadFile(pj)
	if err != nil || json.Unmarshal(b, &pkg) != nil {
		t.Fatalf("package.json: %v", err)
	}
	if summed {
		sums := filepath.Join(dir, "checksums.sha256")
		listing, err := os.ReadFile(sums)
		if err != nil {
			t.Fatal(err)
		}
		listing = []byte(strings.Replace(string(listing), library.SHA256(first)+"  hls/v0/init.mp4\n",
			library.SHA256(inits[len(inits)-1])+"  hls/v0/init.mp4\n", 1))
		librarytest.Write(t, sums, listing)
		pkg["checksums"].(map[string]any)["sha256"] = "sha256:" + library.SHA256(listing)
	}
	pkg["mends"] = list
	b = librarytest.JSON(t, pkg)
	librarytest.Write(t, pj, b)
	librarytest.Write(t, filepath.Join(dir, ".complete"), []byte("sha256:"+library.SHA256(b)+"\n"))
}

// A package mended after it completed has its mended file's line in
// checksums.sha256 written again, so the file matches its line as any other;
// its entries in package.json's mends must run with no gap and end at that
// line. A gap, the checksums not written again after a mend, a file changed
// with no entry, and an entry of a kind the format does not list break it.
func TestAMendedVersionIsHeldToItsMends(t *testing.T) {
	once := [][]byte{[]byte("INIT")}
	twice := [][]byte{[]byte("INIT"), []byte("Init")}
	for name, mend := range map[string]func(dir string){
		"mended":       func(dir string) { mendInit(t, dir, true, once) },
		"mended twice": func(dir string) { mendInit(t, dir, true, twice) },
	} {
		dir := aVersion(t)
		mend(dir)
		for _, full := range []bool{false, true} {
			if _, err := library.VerifyVersion(dir, full); err != nil {
				t.Errorf("%s, full %v: %v", name, full, err)
			}
		}
	}
	for name, c := range map[string]struct {
		mend      func(dir string)
		file, say string
		full      bool
	}{
		"a gap between two mends": {func(dir string) {
			mendInit(t, dir, true, twice, nil, map[string]any{"sha256Before": "sha256:" + strings.Repeat("0", 64)})
		}, "package.json", "have a gap", false},
		"the checksums not written again after a mend": {func(dir string) { mendInit(t, dir, false, once) },
			"package.json", "the checksums were not written again", false},
		"an entry of a kind the format does not list": {func(dir string) {
			mendInit(t, dir, true, once, map[string]any{"kind": "avsync-anything"})
		}, "package.json", "which the format does not list", false},
		"an init segment changed with no entry": {func(dir string) {
			librarytest.Write(t, filepath.Join(dir, "hls/v0/init.mp4"), []byte("INIT"))
		}, "hls/v0/init.mp4", "checksums.sha256 lists", true},
	} {
		dir := aVersion(t)
		c.mend(dir)
		if _, err := library.VerifyVersion(dir, c.full); !brokenAt(err, c.file, c.say) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
