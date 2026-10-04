package rest

import (
	"os"
	"path/filepath"
	"testing"
)

// The packager stages a new package in .next/ beside the live one and keeps a
// replaced one as X.old-<stamp> for a grace period: neither is the package, so
// neither counts in its size.
func TestSumPackageBytesCountsOnlyTheLivePackage(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", 10)
	write("hls/master.m3u8", 20)
	write("hls/v0/seg-1.m4s", 300)
	write(".next/hls/v0/seg-1.m4s", 4000)           // being staged
	write("hls.old-20261004T171500/seg.m4s", 50000) // replaced, in its grace period
	write("trickplay.old-20261004T171500/s.jpg", 600000)

	got := sumPackageBytes(root)
	if got == nil || *got != 330 {
		t.Fatalf("sumPackageBytes = %v, want 330 (the live package only)", got)
	}
}
