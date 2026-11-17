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

// aWrittenVersion is aVersion whose package lists its renditions and
// whose hls/v0/init.mp4 is an init segment of a video track as the packager
// wrote it, its edit list cutting 1328 (83 ms at 16000).
func aWrittenVersion(t *testing.T) string {
	t.Helper()
	files := map[string]string{}
	for k, v := range librarytest.DefaultFiles {
		files[k] = v
	}
	files["hls/v0/init.mp4"] = string(librarytest.InitSegment(1328, "vide"))
	files["hls/a0/init.mp4"] = string(librarytest.InitSegment(1024, "soun"))
	dir := filepath.Join(t.TempDir(), "versions", "v1")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000002",
		PackageID: "4f1d0000-0000-4000-8000-000000000005", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z", Files: files, Package: map[string]any{"renditions": librarytest.Renditions()}})
	return dir
}

// mendInit changes the version's file (hls/v0/init.mp4 when "") to each of
// inits in turn, and records an entry per change in package.json's mends —
// sha256Before and sha256After filled in where entries leaves them out —
// with the file's line in checksums.sha256, package.json's hash of it and
// .complete written again, as the schemas' library-v2-mend.py mends a
// package. summed false leaves the checksums as they were.
func mendInit(t *testing.T, dir string, summed bool, inits [][]byte, entries ...map[string]any) {
	t.Helper()
	mendFile(t, dir, "hls/v0/init.mp4", summed, inits, entries...)
}

func mendFile(t *testing.T, dir, file string, summed bool, inits [][]byte, entries ...map[string]any) {
	t.Helper()
	p := filepath.Join(dir, file)
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	prev := "sha256:" + library.SHA256(first)
	var list []any
	for i, init := range inits {
		librarytest.Write(t, p, init)
		e := map[string]any{"kind": "avsync-editlist", "file": file, "box": "moov/trak/edts/elst", "old": 1328,
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
		listing = []byte(strings.Replace(string(listing), library.SHA256(first)+"  "+file+"\n",
			library.SHA256(inits[len(inits)-1])+"  "+file+"\n", 1))
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
// line, and each must have changed the one field it names and nothing else:
// its old value written back gives the bytes it was made to. The mended
// file is read at every level. A gap, the checksums not written again after
// a mend, a file changed with no entry or since its mend, an entry of a
// kind or with a field the format does not list, of a file that is no video
// rendition's init segment, out of order, or that changed more than its
// field, and a mend's file left beside the package, break it.
func TestAMendedVersionIsHeldToItsMends(t *testing.T) {
	zero := librarytest.InitSegment(0, "vide")
	once := [][]byte{zero}
	twice := [][]byte{zero, zero}
	for name, mend := range map[string]func(dir string){
		"mended":       func(dir string) { mendInit(t, dir, true, once) },
		"mended twice": func(dir string) { mendInit(t, dir, true, twice, nil, map[string]any{"old": 0}) },
	} {
		dir := aWrittenVersion(t)
		mend(dir)
		for _, full := range []bool{false, true} {
			if _, err := library.VerifyVersion(dir, full); err != nil {
				t.Errorf("%s, full %v: %v", name, full, err)
			}
		}
	}
	beyond := append([]byte(nil), zero...)
	beyond[strings.Index(string(beyond), "tkhd")+20] ^= 0xff
	for name, c := range map[string]struct {
		mend      func(dir string)
		file, say string
		full      bool
	}{
		"a gap between two mends": {func(dir string) {
			mendInit(t, dir, true, twice, nil, map[string]any{"old": 0, "sha256Before": "sha256:" + strings.Repeat("0", 64)})
		}, "package.json", "have a gap", false},
		"the checksums not written again after a mend": {func(dir string) { mendInit(t, dir, false, once) },
			"package.json", "the checksums were not written again", false},
		"an entry of a kind the format does not list": {func(dir string) {
			mendInit(t, dir, true, once, map[string]any{"kind": "avsync-anything"})
		}, "package.json", "which the format does not list", false},
		"an entry with a field the format does not list": {func(dir string) {
			mendInit(t, dir, true, once, map[string]any{"reason": "lip sync"})
		}, "package.json", "which the format does not list", false},
		"an init segment changed with no entry": {func(dir string) {
			librarytest.Write(t, filepath.Join(dir, "hls/v0/init.mp4"), zero)
		}, "hls/v0/init.mp4", "checksums.sha256 lists", true},
		"a mended init changed since, at the chain's level": {func(dir string) {
			mendInit(t, dir, true, once)
			librarytest.Write(t, filepath.Join(dir, "hls/v0/init.mp4"), beyond)
		}, "hls/v0/init.mp4", "changed since, with no entry", false},
		"a mend that changed more of the file than its field": {func(dir string) {
			mendInit(t, dir, true, [][]byte{beyond})
		}, "hls/v0/init.mp4", "the mend changed more of the file than the field it names", false},
		"a mend whose old is not the value the file had": {func(dir string) {
			mendInit(t, dir, true, once, map[string]any{"old": 1000})
		}, "hls/v0/init.mp4", "the mend changed more of the file than the field it names", false},
		"an avsync-editlist of the stereo rendition": {func(dir string) {
			mendFile(t, dir, "hls/a0/init.mp4", true, [][]byte{librarytest.InitSegment(0, "soun")}, map[string]any{"old": 1024})
		}, "package.json", "the init segment of no video rendition", false},
		"an avsync-editlist of a video rendition's init whose track is a sound track": {func(dir string) {
			p := filepath.Join(dir, "hls/v0/init.mp4")
			librarytest.Write(t, p, librarytest.InitSegment(1328, "soun"))
			librarytest.Fill(t, dir)
			mendInit(t, dir, true, [][]byte{librarytest.InitSegment(0, "soun")})
		}, "hls/v0/init.mp4", "not a video track's", false},
		"a mend of version.json": {func(dir string) {
			mendFile(t, dir, "version.json", true, [][]byte{[]byte("{}\n")})
		}, "package.json", "which is no rendition's init segment", false},
		"mends out of order": {func(dir string) {
			mendInit(t, dir, true, twice, map[string]any{"at": "2026-10-09T03:00:00Z"}, map[string]any{"old": 0})
		}, "package.json", "dated before the one before it", false},
		"a mend's file beside the package": {func(dir string) {
			mendInit(t, dir, true, once)
			librarytest.Write(t, filepath.Join(dir, "package.json.mending"), []byte("{"))
		}, "package.json.mending", "a mend of the package stopped mid-way", false},
		"a mend's file in a rendition": {func(dir string) {
			librarytest.Write(t, filepath.Join(dir, "hls/v0/init.mp4.mending"), zero)
		}, "hls/v0/init.mp4.mending", "a mend of the package stopped mid-way", false},
	} {
		dir := aWrittenVersion(t)
		c.mend(dir)
		if _, err := library.VerifyVersion(dir, c.full); !brokenAt(err, c.file, c.say) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
