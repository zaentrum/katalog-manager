package library_test

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// An attached subtitle's file is named by its bytes, its language and its
// flags, and its name gives back the hash; no other name is one's.
func TestAnAttachedSubtitleIsNamedByItsBytes(t *testing.T) {
	sum := hex.EncodeToString(make([]byte, 32))
	for _, c := range []struct {
		lang        string
		forced, sdh bool
		want        string
	}{
		{"de", false, false, "attached-" + sum + ".de.vtt"},
		{"zh-Hans", true, false, "attached-" + sum + ".zh-hans.forced.vtt"},
		{"en", true, true, "attached-" + sum + ".en.forced.sdh.vtt"},
		{"pt-BR", false, true, "attached-" + sum + ".pt-br.sdh.vtt"},
	} {
		name := library.AttachedName(sum, c.lang, c.forced, c.sdh)
		if name != c.want {
			t.Errorf("%s: %s, want %s", c.lang, name, c.want)
		}
		if got, ok := library.AttachedSum(name); !ok || got != sum {
			t.Errorf("%s: its name gives %q %v", name, got, ok)
		}
	}
	for _, name := range []string{"0.vtt", "attached-abc.de.vtt", "attached-" + sum + ".vtt", "attached-" + sum + ".de.srt",
		"attached-" + sum + ".DE.vtt", "x-attached-" + sum + ".de.vtt"} {
		if _, ok := library.AttachedSum(name); ok {
			t.Errorf("%s is taken for an attached subtitle's", name)
		}
	}
}

// Sum64 is the file's size plus its first and last 64 KiB as little-endian
// 64-bit words, summed modulo 2^64, written as 16 hex digits; a file
// smaller than both ends has none.
func TestSum64(t *testing.T) {
	dir := t.TempDir()
	zeros := filepath.Join(dir, "zeros")
	if err := os.WriteFile(zeros, make([]byte, 200000), 0o644); err != nil {
		t.Fatal(err)
	}
	if size, sum, err := library.Sum64(zeros); err != nil || size != 200000 || sum != "0000000000030d40" {
		t.Errorf("200000 zeros: %d %s %v", size, sum, err)
	}

	b := make([]byte, 300007)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	path := filepath.Join(dir, "pattern")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	want := uint64(len(b))
	for _, at := range []int{0, len(b) - 65536} {
		for i := 0; i < 65536; i += 8 {
			want += binary.LittleEndian.Uint64(b[at+i:])
		}
	}
	if _, sum, err := library.Sum64(path); err != nil || sum != fmt.Sprintf("%016x", want) {
		t.Errorf("a pattern: %s %v, want %016x", sum, err, want)
	}

	small := filepath.Join(dir, "small")
	if err := os.WriteFile(small, make([]byte, 131071), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := library.Sum64(small); !errors.Is(err, library.ErrTooSmall) {
		t.Errorf("a file of less than 128 KiB: %v", err)
	}
}

// A subtitle attached to a version after its package completed is no file
// of the package: its chain holds without listing it, and a full check
// holds its bytes to its name. Any other file the checksums do not list
// still breaks the chain, an attached subtitle's name in another folder too.
func TestAnAttachedSubtitleLeavesTheChainWhole(t *testing.T) {
	dir := aVersion(t)
	vtt := []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nGuten Tag\n")
	s := sha256.Sum256(vtt)
	name := library.AttachedName(hex.EncodeToString(s[:]), "de", false, false)
	librarytest.Write(t, library.AttachedPath(dir, name), vtt)
	for _, full := range []bool{false, true} {
		c, err := library.VerifyVersion(dir, full)
		if err != nil {
			t.Fatalf("full %v: %v", full, err)
		}
		if _, listed := c.Listed["subs/"+name]; listed || len(c.Listed) != 10 {
			t.Errorf("full %v: the checksums list %d files", full, len(c.Listed))
		}
	}

	// Its bytes are not its name's: the chain holds, the full check does not.
	librarytest.Write(t, library.AttachedPath(dir, name), []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nGuten Abend\n"))
	if _, err := library.VerifyVersion(dir, false); err != nil {
		t.Errorf("an attached subtitle changed broke the chain: %v", err)
	}
	if _, err := library.VerifyVersion(dir, true); !brokenAt(err, "subs/"+name, "attached subtitle") {
		t.Errorf("an attached subtitle changed, in full: %v", err)
	}
	librarytest.Write(t, library.AttachedPath(dir, name), vtt)

	for _, rel := range []string{"subs/2.vtt", "subs/more/" + name, "hls/" + name} {
		librarytest.Write(t, filepath.Join(dir, rel), vtt)
		if _, err := library.VerifyVersion(dir, false); !brokenAt(err, rel, "not listed in checksums.sha256") {
			t.Errorf("%s: %v, want the chain broken", rel, err)
		}
		os.Remove(filepath.Join(dir, rel))
	}

	// A version adopted with its checksums pending counts the files of its
	// package alone; the attached subtitle is none of them.
	pending := filepath.Join(t.TempDir(), "versions", "v2")
	librarytest.WriteVersion(t, pending, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000012",
		PackageID: "4f1d0000-0000-4000-8000-000000000015", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z", Pending: true})
	librarytest.Write(t, library.AttachedPath(pending, name), vtt)
	if c, err := library.VerifyVersion(pending, false); err != nil || !c.Pending {
		t.Errorf("a pending version with an attached subtitle: %+v %v", c, err)
	}
}
