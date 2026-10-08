package library

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The mode of a run is the source's: establish (or takein, when the run
// takes the title in) while no version of it is there, add while its version
// holds its original alone (taken), repackage once a version of it is
// packaged (complete or superseded), whatever the title's other sources'
// versions are, and whether or not the original lies in a version's folder.
func TestTheModeOfARun(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	p := PathsOf(config.Config{LibraryRoot: t.TempDir()})
	const item, src, other = "a1a1a1a1-0000-4000-8000-000000000001", "b1b1b1b1-0000-4000-8000-000000000002",
		"c1c1c1c1-0000-4000-8000-000000000003"
	storetest.AddItem(t, st, item, "movie", "A Film", "")
	itemDir := p.MovieDir(item)
	arrival := filepath.Join(p.Arrivals, "A Film (2024)", "A Film (2024).mkv")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath, sizebytes, state)
		VALUES ($1, $2, 'A Film (2024).mkv', $3, 'A Film (2024)/A Film (2024).mkv', 10, 'present'),
		       ($4, $2, 'original.mkv', NULL, NULL, 10, 'deleted')`, src, item, arrival, other)
	version := func(id, state string, sources ...string) {
		t.Helper()
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir)
			VALUES ($1, $2, $3, $4, $5)`, id, item, sources, state, VersionDir(itemDir, id))
	}
	mode := func(takeIn bool) (string, string) {
		t.Helper()
		s, err := SourceByID(ctx, st.Pool(), src)
		if err != nil {
			t.Fatal(err)
		}
		m, v, err := RunOf(ctx, st.Pool(), s, takeIn)
		if err != nil {
			t.Fatal(err)
		}
		if v == nil {
			return m, ""
		}
		return m, v.ID
	}
	check := func(what string, takeIn bool, want, wantVersion string) {
		t.Helper()
		if m, v := mode(takeIn); m != want || v != wantVersion {
			t.Errorf("%s: %s %s, want %s %s", what, m, v, want, wantVersion)
		}
	}

	// Another source's packaged version, and a version being built of this
	// one: no version of it yet.
	version("d1d1d1d1-0000-4000-8000-000000000004", VersionComplete, other)
	version("e1e1e1e1-0000-4000-8000-000000000005", VersionBuilding, src)
	check("no version of the source", false, ModeEstablish, "")
	check("no version of the source, taken in", true, ModeTakeIn, "")

	// Taken in: its folder holds the original.
	const taken = "f1f1f1f1-0000-4000-8000-000000000006"
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'taken', id = $1, dir = $2 WHERE state = 'building'`,
		taken, VersionDir(itemDir, taken))
	moved := filepath.Join(VersionDir(itemDir, taken), "original.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2, filename = 'original.mkv' WHERE id = $1`, src, moved)
	check("taken", false, ModeAdd, taken)
	check("taken, taken in again", true, ModeAdd, taken)
	s, _ := SourceByID(ctx, st.Pool(), src)
	if v, err := TakenOf(ctx, st.Pool(), s); err != nil || v == nil || v.ID != taken {
		t.Errorf("TakenOf: %v, %v", v, err)
	}
	if got := p.ArrivalOf(s); got != arrival {
		t.Errorf("an original in its version's folder arrived at %q, want %q", got, arrival)
	}

	// Packaged: the next run is a new version, the original staying in the
	// folder of the one it was added to; so too once that one is superseded.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE state = 'complete'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'complete' WHERE id = $1`, taken)
	check("complete", false, ModeRepackage, "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, taken)
	version("a2a2a2a2-0000-4000-8000-000000000007", VersionComplete, src)
	check("superseded by another of its packages", true, ModeRepackage, "")
	if v, err := TakenOf(ctx, st.Pool(), s); err != nil || v != nil {
		t.Errorf("TakenOf a packaged source: %v, %v", v, err)
	}

	// Packaged before the original went into the library (it lies where it
	// arrived): a new version as well.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2 WHERE id = $1`, src, arrival)
	check("packaged, the original where it arrived", false, ModeRepackage, "")
	s, _ = SourceByID(ctx, st.Pool(), src)
	if got := p.ArrivalOf(s); got != arrival {
		t.Errorf("an original among the arrivals arrived at %q, want %q", got, arrival)
	}
}

// Where an original lies: directly in a version's folder of its item, by
// its name; the originals a folder holds.
func TestAnOriginalInAVersionsFolder(t *testing.T) {
	itemDir := "/lib/movies/a1/a1a1a1a1-0000-4000-8000-000000000001"
	const vid = "f1f1f1f1-0000-4000-8000-000000000006"
	for path, want := range map[string]bool{
		itemDir + "/versions/" + vid + "/original.mkv":     true,
		itemDir + "/versions/" + vid + "/hls/original.mkv": false,
		itemDir + "/versions/not-an-id/original.mkv":       false,
		itemDir + "/sources/" + vid + "/original.mkv":      false,
		"/lib/.work/incoming/" + vid + "/original.mkv":     false,
	} {
		got, ok := VersionFolderOf(itemDir, path)
		if ok != want || (ok && got != vid) {
			t.Errorf("VersionFolderOf(%s) = %s %v, want %v", path, got, ok, want)
		}
	}
	dir := t.TempDir()
	for _, name := range []string{"original.mkv", "original-2.ts", "version.json", "Original.mkv"} {
		librarytest.Write(t, filepath.Join(dir, name), []byte("x"))
	}
	librarytest.Write(t, filepath.Join(dir, "original.bin", "a"), []byte("a folder"))
	if got, err := OriginalsIn(dir); err != nil || len(got) != 2 || got[0] != "original-2.ts" || got[1] != "original.mkv" {
		t.Errorf("OriginalsIn: %v, %v", got, err)
	}
	if got, err := OriginalsIn(filepath.Join(dir, "none")); err != nil || got != nil {
		t.Errorf("OriginalsIn a folder that is not there: %v, %v", got, err)
	}
}
