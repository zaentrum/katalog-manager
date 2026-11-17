package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The retire job reads a version whose chain does not hold in a file a mend
// writes once more, a mend's time later: a mend that finishes meanwhile is
// verified, one that does not is broken, and a version broken elsewhere is
// broken at once.
func TestTheRetireJobReadsAVersionAMendIsWritingOnceMore(t *testing.T) {
	saved := mendSettle
	mendSettle = 300 * time.Millisecond
	t.Cleanup(func() { mendSettle = saved })
	dir := filepath.Join(t.TempDir(), "versions", "v1")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000002",
		PackageID: "4f1d0000-0000-4000-8000-000000000005", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z"})
	complete, err := os.ReadFile(filepath.Join(dir, CompleteFile))
	if err != nil {
		t.Fatal(err)
	}
	// a mend under way: package.json written, .complete not yet
	librarytest.Write(t, filepath.Join(dir, CompleteFile), []byte("sha256:"+SHA256([]byte("before"))+"\n"))
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(dir, CompleteFile), complete, 0o644)
	}()
	start := time.Now()
	if _, err := verifySettled(context.Background(), dir, true); err != nil {
		t.Errorf("a mend that finished while the job waited: %v", err)
	}
	if time.Since(start) < mendSettle {
		t.Errorf("the job read it once more before a mend's time")
	}
	// one that never finishes
	librarytest.Write(t, filepath.Join(dir, CompleteFile), []byte("sha256:"+SHA256([]byte("before"))+"\n"))
	if _, err := verifySettled(context.Background(), dir, false); err == nil {
		t.Errorf("a chain still broken after a mend's time holds")
	}
	librarytest.Write(t, filepath.Join(dir, CompleteFile), complete)
	// broken in a file no mend writes: at once
	librarytest.Write(t, filepath.Join(dir, "hls/v0/seg0.m4s"), []byte("another segment, of its size")[:24])
	start = time.Now()
	if _, err := verifySettled(context.Background(), dir, true); err == nil || time.Since(start) >= mendSettle {
		t.Errorf("a segment that fails its line: %v, after %v", err, time.Since(start))
	}
}
