package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// writeJournal writes a run's mends journal in the library at root, one line
// per op, of the version folder dir.
func writeJournal(t *testing.T, root, run, dir string, ops ...string) {
	t.Helper()
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, op := range ops {
		out += `{"at": "2026-11-17T00:00:00.000001Z", "op": "` + op + `", "dir": "` + filepath.ToSlash(rel) + `"}` + "\n"
	}
	librarytest.Write(t, filepath.Join(root, ".work", "migration", run, MendsJournal), []byte(out))
}

// A version whose chain does not hold in a file a mend writes is told apart
// from a broken one by the runs' journals, at once: with a step of it open,
// the answer is a MendUnderWay that names the run, whatever its bytes; with
// none, it is read once more and is whole or broken; a version broken in a
// file no mend writes is broken.
func TestAVersionAMendIsWritingIsToldApartByTheJournals(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "movies", "9a", "9a2e0000-0000-4000-8000-000000000001", "versions", "v1")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000002",
		PackageID: "4f1d0000-0000-4000-8000-000000000005", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z"})
	complete, err := os.ReadFile(filepath.Join(dir, CompleteFile))
	if err != nil {
		t.Fatal(err)
	}
	// a mend under way: package.json written, .complete not yet
	librarytest.Write(t, filepath.Join(dir, CompleteFile), []byte("sha256:"+SHA256([]byte("before"))+"\n"))
	writeJournal(t, root, "mend-a", dir, "mend")
	start := time.Now()
	_, err = verifyOrMend(root, dir, true)
	var mw *MendUnderWay
	if !errors.As(err, &mw) || mw.Run != "mend-a" || mw.Op != "mend" {
		t.Errorf("a version a run has a step of open: %v", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("the job waited %v", time.Since(start))
	}
	// the run finished meanwhile: whole, read once more
	librarytest.Write(t, filepath.Join(dir, CompleteFile), complete)
	writeJournal(t, root, "mend-a", dir, "mend", "mended")
	if _, err := verifyOrMend(root, dir, true); err != nil {
		t.Errorf("a version the run finished: %v", err)
	}
	// broken, no step open: broken
	librarytest.Write(t, filepath.Join(dir, CompleteFile), []byte("sha256:"+SHA256([]byte("before"))+"\n"))
	if _, err := verifyOrMend(root, dir, false); err == nil || errors.As(err, &mw) {
		t.Errorf("a chain broken with no step open: %v", err)
	}
	librarytest.Write(t, filepath.Join(dir, CompleteFile), complete)
	// broken in a file no mend writes, a step open: broken
	writeJournal(t, root, "mend-b", dir, "undo")
	librarytest.Write(t, filepath.Join(dir, "hls/v0/seg0.m4s"), []byte("another segment, of its size")[:24])
	if _, err := verifyOrMend(root, dir, true); err == nil || errors.As(err, &mw) {
		t.Errorf("a segment that fails its line: %v", err)
	}
}

// A retire pass that meets a version a mend is writing — slower than any
// wait — keeps the original without failing the title's retire step, saying
// which run it waits for; once the run finished the mend, the next pass
// retires the original. A mend that stopped mid-way keeps it waiting too.
func TestARetirePassWaitsForAMendOfTheVersion(t *testing.T) {
	f := newRetireFixture(t)
	mark := filepath.Join(f.versionDir, CompleteFile)
	complete, err := os.ReadFile(mark)
	if err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, mark, []byte("sha256:"+SHA256([]byte("package.json as it was"))+"\n"))
	writeJournal(t, f.p.Root, "mend-avsync-editlist", f.versionDir, "mend")
	rep, err := f.r.Pass(context.Background())
	if err != nil || rep.Originals != 0 || rep.Failed != 0 {
		t.Errorf("the pass during the mend: %+v, %v", rep, err)
	}
	if s := f.source(t, rtSource); s.State != SourcePresent {
		t.Errorf("the original during the mend: %s", s.State)
	}
	if st := f.step(t, rtFilm); strings.HasPrefix(st, "failed") || !strings.Contains(st, "mend-avsync-editlist") {
		t.Errorf("the retire step during the mend: %s", st)
	}
	// a pass while the run has stopped mid-way waits as well
	if rep, err := f.r.Pass(context.Background()); err != nil || rep.Originals != 0 || rep.Failed != 0 {
		t.Errorf("the pass while the mend is stopped mid-way: %+v, %v", rep, err)
	}
	// the run finished it
	librarytest.Write(t, mark, complete)
	writeJournal(t, f.p.Root, "mend-avsync-editlist", f.versionDir, "mend", "mended")
	if rep, err := f.r.Pass(context.Background()); err != nil || rep.Originals != 1 {
		t.Errorf("the pass once the mend is finished: %+v, %v; step %s", rep, err, f.step(t, rtFilm))
	}
}
