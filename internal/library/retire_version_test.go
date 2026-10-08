package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// intoVersion moves the fixture's original into the folder of the version
// vid as the packager renames it in (original.mkv), as the catalog then
// holds it (its source and the asset of the title's file), and writes the
// source's record as it is written now: copies of the subtitle files that
// came with it under the library's names, by their content, and no name of
// where they came from. It answers where the original lies.
func (f *retireFixture) intoVersion(t *testing.T, vid string) string {
	t.Helper()
	to := filepath.Join(VersionDir(f.itemDir, vid), "original.mkv")
	if err := os.Rename(f.original, to); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2, filename = 'original.mkv' WHERE id = $1`,
		rtSource, to)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_playbackassets SET path = $2 WHERE id = $1`, "orig-"+rtFilm[:8], to)
	size, qh1, err := QH1(to)
	if err != nil {
		t.Fatal(err)
	}
	copies := map[string]string{}
	var listed []any
	for i, from := range []string{f.en, f.de} {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		name := map[int]string{0: "subtitle-1.en.srt", 1: "subtitle-2.de.srt"}[i]
		copies[name] = string(b)
		listed = append(listed, map[string]any{"file": "sources/" + rtSource + "/" + name, "kind": "subtitle",
			"sha256": "sha256:" + SHA256(b), "sizeBytes": len(b)})
	}
	if err := os.RemoveAll(f.sourceDir); err != nil {
		t.Fatal(err)
	}
	librarytest.WriteSource(t, f.sourceDir, map[string]any{"sourceId": rtSource, "file": map[string]any{"name": "original.mkv",
		"sizeBytes": size, "fixity": map[string]any{"qh1": qh1}}, "sidecars": listed, "essence": f.src}, copies)
	return to
}

// newer gives the fixture's film a newer version of the same source, vid,
// complete an hour ago, its package's essence pkg: the older is superseded
// two days ago, its grace over.
func (f *retireFixture) newer(t *testing.T, vid, pid string, pkg map[string]any) {
	t.Helper()
	dir := VersionDir(f.itemDir, vid)
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: vid, PackageID: pid, SourceIDs: []string{rtSource},
		CreatedAt: "2026-10-07T10:00:00Z", Package: map[string]any{"essence": pkg}})
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2,
		supersededat = now() - interval '2 days' WHERE id = $1`, rtVersion, vid)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`, vid, rtFilm, rtSource, pid, dir)
}

// An original in its version's folder is retired from there: moved to the
// trash as .work/trash/<day>/<sourceId>/original.mkv, with the subtitle
// files that came with it from where it arrived (the one the package made a
// rendition of, and the one the source's record keeps a copy of, found by
// its content), its event recorded; the version's folder stays, with its
// version.json and its package. The catalog says it is deleted: the asset
// is an original's in its record, the subtitle rows point at the rendition
// and at the copy. A file that came with it and no record keeps stays where
// it arrived.
func TestTheRetireJobRetiresAnOriginalFromItsVersionsFolder(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	moved := f.intoVersion(t, rtVersion)
	rep := f.pass(t)
	if rep.Originals != 1 || rep.Failed != 0 {
		t.Fatalf("the pass: %+v", rep)
	}
	s := f.source(t, rtSource)
	if s.State != SourceDeleted || s.ArrivalPath != nil || s.RetireEventAt == nil {
		t.Fatalf("the source: %+v", s)
	}
	trash := f.p.TrashDir(*s.RetireEventAt, rtSource)
	want := []string{"Example Film.de.srt", "Example Film.en.srt", "original.mkv"}
	if got := files(t, trash); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the trash holds %v, want %v", got, want)
	}
	if exists(moved) || !exists(filepath.Join(f.versionDir, VersionFile)) || !exists(filepath.Join(f.versionDir, CompleteFile)) {
		t.Errorf("the version's folder after the retirement: original %v, its records %v", exists(moved),
			exists(filepath.Join(f.versionDir, VersionFile)))
	}
	if _, err := VerifyVersion(f.versionDir, true); err != nil {
		t.Errorf("the version does not verify after its original went: %v", err)
	}
	if !exists(f.nfo) {
		t.Error("a file no record keeps was moved from where it arrived")
	}
	var kind, path string
	if err := f.st.Pool().QueryRow(ctx, `SELECT kind, path FROM com_nalet_katalog_playbackassets WHERE id = $1`,
		"orig-"+rtFilm[:8]).Scan(&kind, &path); err != nil || kind != "original" || path != f.sourceDir {
		t.Errorf("the original's asset: %s %s, %v", kind, path, err)
	}
	for id, want := range map[string]string{
		"en-" + rtFilm[:8]: filepath.Join(f.versionDir, "subs/0.vtt") + " webvtt",
		"de-" + rtFilm[:8]: filepath.Join(f.sourceDir, "subtitle-2.de.srt") + " srt",
	} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, `SELECT path || ' ' || format FROM com_nalet_katalog_subtitleassets WHERE id = $1`,
			id).Scan(&got); err != nil || got != want {
			t.Errorf("subtitle row %s: %q, %v; want %q", id, got, err, want)
		}
	}
	if dir, err := FindEvent(f.itemDir, deref(s.RetireEventID), EventOriginalDeleted); err != nil || dir == "" {
		t.Errorf("the event: %q, %v", dir, err)
	}
}

// A version whose package was superseded is removed after its grace only
// once its original is retired: while its folder holds the original it
// stays, whatever its grace says, and others are removed past it. The
// original's surround is held against the newest package of its source, not
// the one of the folder it lies in: the newer's 5.1 has it go (and its
// folder with it), the newer's stereo keeps it, though the older carried
// the surround.
func TestAVersionHoldingAnOriginalIsNeverRemoved(t *testing.T) {
	const next, nextPkg = "77c1d2e3-f4a5-4b6c-8d7e-9f0a1b2c3d4e", "88c1d2e3-f4a5-4b6c-8d7e-9f0a1b2c3d4e"
	t.Run("retired by the newer package, then removed", func(t *testing.T) {
		f := newRetireFixtureOf(t, sevenOne, stereo)
		moved := f.intoVersion(t, rtVersion)
		f.newer(t, next, nextPkg, fiveOne)
		f.set(t, SettingOriginals, OriginalsKeep)
		if rep := f.pass(t); rep.Versions != 0 || rep.Failed != 0 {
			t.Errorf("the pass that keeps originals: %+v", rep)
		}
		if !exists(moved) || !exists(filepath.Join(f.versionDir, VersionFile)) {
			t.Fatal("the superseded version that holds the original was removed")
		}
		if v, _ := VersionByID(context.Background(), f.st.Pool(), rtVersion); v.State != VersionSuperseded || v.RemovedAt != nil {
			t.Errorf("the version that holds the original: %s, removal %v", v.State, v.RemovedAt)
		}
		f.set(t, SettingOriginals, OriginalsDelete)
		rep := f.pass(t)
		if rep.Originals != 1 || rep.Versions != 1 || rep.Held != 0 || rep.Failed != 0 {
			t.Fatalf("the pass that deletes originals: %+v", rep)
		}
		s := f.source(t, rtSource)
		b, _ := os.ReadFile(filepath.Join(EventDir(f.itemDir, *s.RetireEventAt, *s.RetireEventID, EventOriginalDeleted), EventFile))
		if !strings.Contains(string(b), `"versionId": "`+next+`"`) {
			t.Errorf("the original's deletion names another version than the newest package:\n%s", b)
		}
		if exists(f.versionDir) || !exists(VersionDir(f.itemDir, next)) {
			t.Errorf("after its original went, the older version's folder: %v; the newer's: %v", exists(f.versionDir),
				exists(VersionDir(f.itemDir, next)))
		}
		if v, _ := VersionByID(context.Background(), f.st.Pool(), rtVersion); v.State != VersionRemoved {
			t.Errorf("the older version: %s", v.State)
		}
	})
	t.Run("held by the newer package", func(t *testing.T) {
		f := newRetireFixtureOf(t, sevenOne, fiveOne)
		moved := f.intoVersion(t, rtVersion)
		f.newer(t, next, nextPkg, stereo)
		rep := f.pass(t)
		if rep.Originals != 0 || rep.Held != 1 || rep.Versions != 0 {
			t.Errorf("the pass: %+v", rep)
		}
		if !exists(moved) || !exists(f.versionDir) {
			t.Error("the original held for its surround, or its folder, is gone")
		}
		if got := f.step(t, rtFilm); !strings.Contains(got, "details="+HeldDetails(next)) {
			t.Errorf("the retire step: %s", got)
		}
	})
	t.Run("a file that may be an original", func(t *testing.T) {
		f := newRetireFixture(t)
		f.newer(t, next, nextPkg, fiveOne)
		// The catalog has no original there, and the folder holds a file
		// named as one: it stays, said in the log.
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL`)
		librarytest.Write(t, filepath.Join(f.versionDir, "original.mkv"), []byte("an original"))
		if rep := f.pass(t); rep.Versions != 0 || rep.Failed != 0 {
			t.Errorf("the pass: %+v", rep)
		}
		if !exists(filepath.Join(f.versionDir, "original.mkv")) {
			t.Error("a version's folder holding a file named as an original was removed")
		}
		// Nor one holding any other file beside its record: it may be one.
		if err := os.Rename(filepath.Join(f.versionDir, "original.mkv"), filepath.Join(f.versionDir, "A Film.mkv")); err != nil {
			t.Fatal(err)
		}
		librarytest.Write(t, filepath.Join(f.versionDir, ".DS_Store"), []byte("an artefact"))
		if rep := f.pass(t); rep.Versions != 0 || rep.Failed != 0 {
			t.Errorf("the pass with a file beside the record: %+v", rep)
		}
		// Without it, the version goes.
		if err := os.Remove(filepath.Join(f.versionDir, "A Film.mkv")); err != nil {
			t.Fatal(err)
		}
		if rep := f.pass(t); rep.Versions != 1 {
			t.Errorf("the pass once the folder holds no original: %+v", rep)
		}
	})
}

// A retirement from a version's folder that a crash stopped once the files
// were in the trash goes on: the original and its sidecars are not moved
// again, and the subtitle row of the file the record keeps a copy of, found
// by its content in the trash, points at the copy.
func TestARetirementFromAVersionsFolderResumes(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	moved := f.intoVersion(t, rtVersion)
	set, _ := ReadSettings(ctx, f.st.Pool())
	rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir}
	if err := f.r.prepare(ctx, f.p, set, time.Now().UTC(), rt); err != nil || rt.src == nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := f.r.deleteFiles(f.p, set, rt); err != nil {
		t.Fatal(err)
	}
	if exists(moved) || exists(f.de) {
		t.Fatal("the files are not in the trash")
	}
	if rep := f.pass(t); rep.Originals != 1 || rep.Failed != 0 {
		t.Fatalf("the pass after the crash: %+v", rep)
	}
	var got string
	if err := f.st.Pool().QueryRow(ctx, `SELECT path FROM com_nalet_katalog_subtitleassets WHERE id = $1`, "de-"+rtFilm[:8]).
		Scan(&got); err != nil || got != filepath.Join(f.sourceDir, "subtitle-2.de.srt") {
		t.Errorf("the subtitle row of the copy: %q, %v", got, err)
	}
	s := f.source(t, rtSource)
	want := []string{"Example Film.de.srt", "Example Film.en.srt", "original.mkv"}
	if got := files(t, deref(s.TrashPath)); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the trash holds %v, want %v", got, want)
	}
}

// An original a run renamed into the folder of a version it built whose
// handover was lost (the version is being built, the catalog has the
// original where it arrived) is not touched by the retire job: its pass
// finds the original gone from where the catalog has it, fails the retire
// step saying so, and deletes nothing, and the folder is no version it
// removes.
func TestTheRetireJobLeavesAnOriginalWhoseHandoverWasLost(t *testing.T) {
	f := newRetireFixture(t)
	const building = "6b0b0b0b-0000-4000-8000-000000000001"
	dir := VersionDir(f.itemDir, building)
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: building, PackageID: NewID(), SourceIDs: []string{rtSource},
		CreatedAt: "2026-10-08T10:00:00Z", Version: map[string]any{"originalFiles": []string{"original.mkv"}}})
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, ARRAY[$3::varchar], 'building')`,
		building, rtFilm, rtSource)
	moved := filepath.Join(dir, "original.mkv")
	if err := os.Rename(f.original, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("the pass: %v; want the original said gone from where the catalog has it", err)
	}
	if !exists(moved) || !exists(filepath.Join(dir, VersionFile)) {
		t.Error("the original whose handover was lost, or its version's folder, is gone")
	}
	if got := files(t, filepath.Join(f.p.Work, WorkTrash)); len(got) > 0 {
		t.Errorf("the trash holds %v", got)
	}
	if found, err := UnrecordedOriginal(context.Background(), f.st.Pool(), f.itemDir, f.source(t, rtSource)); err != nil || found != moved {
		t.Errorf("UnrecordedOriginal: %q, %v; want %s", found, err, moved)
	}
}
