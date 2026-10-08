package library

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A film recorded before 2026-10-08, as the neutral names left its folder:
// its original in its version's folder and the copies of its subtitle files
// in its source's record renamed as the library names them, the copy of its
// NFO text taken out of the record into the run's removed/, its records
// written again. The catalog still names them as they were.
const (
	nnFilm    = "a1a1a1a1-0000-4000-8000-0000000000a1"
	nnSource  = "c1c1c1c1-0000-4000-8000-0000000000c1"
	nnVersion = "d1d1d1d1-0000-4000-8000-0000000000d1"
	nnRun     = "neutral-names"
)

type namesFixture struct {
	st      *store.Store
	cfg     config.Config
	p       Paths
	itemDir string
	rel     string // the film's folder, as the journal names it
	journal []map[string]any
}

func newNamesFixture(t *testing.T) *namesFixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Share: dir, NFSRoot: filepath.Join(dir, "media"), PackagesRoot: filepath.Join(dir, "packages")}
	f := &namesFixture{st: storetest.Open(t), cfg: cfg, p: PathsOf(cfg)}
	f.itemDir = f.p.MovieDir(nnFilm)
	f.rel = "movies/a1/" + nnFilm
	storetest.AddItem(t, f.st, nnFilm, "movie", "Example Film", "")

	// The tree as the run left it.
	src, ver := SourceDir(f.itemDir, nnSource), VersionDir(f.itemDir, nnVersion)
	librarytest.Write(t, filepath.Join(ver, "original.mkv"), []byte("an original"))
	librarytest.Write(t, filepath.Join(ver, "subs", "0.vtt"), []byte("WEBVTT\n"))
	librarytest.Write(t, filepath.Join(src, "source.json"), librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.source/2", "sourceId": nnSource,
		"file": map[string]any{"name": "original.mkv", "kind": "stream-container", "sizeBytes": 11}}))
	librarytest.Write(t, filepath.Join(src, "subtitle-1.de.srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nHallo\n"))
	librarytest.Write(t, filepath.Join(src, "subtitle-2.en.forced.srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"))
	librarytest.Write(t, filepath.Join(f.p.MigrationDir(nnRun), "removed", filepath.FromSlash(f.rel), "sources", nnSource,
		"Example Film (2020).nfo"), []byte("<movie/>"))

	// The catalog as it was before.
	old := filepath.Join(ver, "Example Film (2020).mkv")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath, sizebytes,
			state, recordedat, recorddir)
		VALUES ($1, $2, 'Example Film (2020).mkv', $3, 'Example Film (2020)/Example Film (2020).mkv', 11, 'present', now(), $4)`,
		nnSource, nnFilm, old, src)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3], 'complete', 'p-1', $4, now())`, nnVersion, nnFilm, nnSource, ver)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('pa-original', $1, $2, true, 'primary', $3)`, nnFilm, old, nnSource)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, versionid)
		VALUES ('pa-package', $1, $2, false, 'packaged', $3)`, nnFilm, filepath.Join(ver, PackageFile), nnVersion)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang) VALUES
		('sa-de', $1, $2, 'srt', 'de'), ('sa-en', $1, $3, 'srt', 'en'), ('sa-rendition', $1, $4, 'webvtt', 'fr')`, nnFilm,
		filepath.Join(src, "Example Film (2020).de.srt"), filepath.Join(src, "Example Film (2020).en.forced.srt"),
		filepath.Join(ver, "subs", "0.vtt"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemdiagnostics (id, item_id, sourcepath) VALUES ('dg-1', $1, $2)`,
		nnFilm, old)

	// The journal, as the tool writes it.
	s, v := f.rel+"/sources/"+nnSource+"/", f.rel+"/versions/"+nnVersion+"/"
	f.run(f.rel, true,
		rename(s+"Example Film (2020).de.srt", s+"subtitle-1.de.srt"),
		rename(s+"Example Film (2020).en.forced.srt", s+"subtitle-2.en.forced.srt"),
		map[string]any{"op": "remove", "from": s + "Example Film (2020).nfo", "to": "removed/" + s + "Example Film (2020).nfo"},
		rewrite(s+"source.json"), rewrite(s+SumsFile),
		rename(v+"Example Film (2020).mkv", v+"original.mkv"),
		rewrite(v+"version.json"), rewrite(v+SumsFile), rewrite(v+PackageFile), rewrite(v+CompleteFile))
	return f
}

func rename(from, to string) map[string]any {
	return map[string]any{"op": "rename", "from": from, "to": to}
}

func rewrite(path string) map[string]any {
	return map[string]any{"op": "rewrite", "path": path, "before": "sha256:" + strings.Repeat("a", 64),
		"after": "sha256:" + strings.Repeat("b", 64)}
}

// run journals a run of the tool over the item folder rel: its plan, a line
// for each step as it is made, and, when the run finished it, its done line.
func (f *namesFixture) run(rel string, done bool, steps ...map[string]any) {
	line := func(entry map[string]any) {
		entry["seq"], entry["at"], entry["item"] = len(f.journal)+1, "2026-10-08T10:00:00.000000Z", rel
		f.journal = append(f.journal, entry)
	}
	plan := make([]any, len(steps))
	for i, s := range steps {
		plan[i] = s
	}
	line(map[string]any{"op": "plan", "steps": plan})
	for _, s := range steps {
		c := map[string]any{}
		for k, v := range s {
			c[k] = v
		}
		line(c)
	}
	if done {
		line(map[string]any{"op": "done"})
	}
}

// names writes the journal and points the catalog at the names.
func (f *namesFixture) names(t *testing.T, only ...string) (NamesReport, error) {
	t.Helper()
	var b []byte
	for _, e := range f.journal {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b = append(append(b, line...), '\n')
	}
	librarytest.Write(t, filepath.Join(f.p.MigrationDir(nnRun), "journal.jsonl"), b)
	m, err := NewMigration(f.st.Pool(), f.cfg, nnRun)
	if err != nil {
		t.Fatal(err)
	}
	return m.Names(context.Background(), only)
}

// catalog is what the catalog says of the film's files, as JSON.
func (f *namesFixture) catalog(t *testing.T) string {
	t.Helper()
	var out string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT jsonb_build_object(
		'sources', (SELECT jsonb_agg(jsonb_build_object('id', id, 'filename', filename, 'arrivalpath', arrivalpath,
			'librarypath', librarypath, 'recorddir', recorddir) ORDER BY id) FROM com_nalet_katalog_itemsources WHERE item_id = $1),
		'assets', (SELECT jsonb_agg(jsonb_build_object('id', id, 'path', path) ORDER BY id)
			FROM com_nalet_katalog_playbackassets WHERE item_id = $1),
		'subtitles', (SELECT jsonb_agg(jsonb_build_object('id', id, 'path', path) ORDER BY id)
			FROM com_nalet_katalog_subtitleassets WHERE item_id = $1),
		'diagnostics', (SELECT jsonb_agg(sourcepath ORDER BY id) FROM com_nalet_katalog_itemdiagnostics WHERE item_id = $1))::text`,
		nnFilm).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(out, f.itemDir, "FILM")
}

// The catalog's side of the neutral names: every row of the film that named
// a file the run renamed names it where it is now (its source's place, the
// asset of its file, the subtitle rows of the copies, its diagnostics), its
// source has the name its record gives its file and no place among the
// arrivals, and nothing else changes: the package and its rendition keep
// their rows, and no row named the NFO text's copy. Called again, it changes
// nothing.
func TestNamesPointsTheCatalogAtTheFilesTheRunRenamed(t *testing.T) {
	f := newNamesFixture(t)
	rep, err := f.names(t)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Run != nnRun || rep.Items != 1 || rep.Rows != 5 || len(rep.Skipped) != 0 {
		t.Fatalf("names: %+v, want the film taken and its 5 rows changed", rep)
	}
	s, v := "FILM/sources/"+nnSource+"/", "FILM/versions/"+nnVersion+"/"
	want := `{"assets": [{"id": "pa-original", "path": "` + v + `original.mkv"}, {"id": "pa-package", "path": "` + v +
		`package.json"}], "sources": [{"id": "` + nnSource + `", "filename": "original.mkv", "recorddir": "FILM/sources/` +
		nnSource + `", "arrivalpath": "` + v + `original.mkv", "librarypath": null}], "subtitles": [{"id": "sa-de", "path": "` + s +
		`subtitle-1.de.srt"}, {"id": "sa-en", "path": "` + s + `subtitle-2.en.forced.srt"}, {"id": "sa-rendition", "path": "` + v +
		`subs/0.vtt"}], "diagnostics": ["` + v + `original.mkv"]}`
	if got := f.catalog(t); got != want {
		t.Errorf("the catalog:\n%s\nwant\n%s", got, want)
	}

	again, err := f.names(t)
	if err != nil || again.Items != 1 || again.Rows != 0 || len(again.Skipped) != 0 {
		t.Fatalf("names again: %+v, %v, want nothing changed", again, err)
	}
	if got := f.catalog(t); got != want {
		t.Errorf("the catalog after names again:\n%s\nwant\n%s", got, want)
	}
}

// A journal that names a file outside the folder of its item is refused
// whole, and nothing changes, the film's own steps neither: a rename into
// another item's folder or out of the record, a removal anywhere but into
// the run's removed/, a series' step in one of its episodes' folders, a
// path that climbs out.
func TestNamesRefusesAJournalThatNamesAFileOutsideItsItem(t *testing.T) {
	const other = "movies/b1/b1b1b1b1-0000-4000-8000-0000000000b1"
	const show = "series/e1/e1e1e1e1-0000-4000-8000-0000000000e1"
	for name, bad := range map[string]struct {
		rel  string
		step map[string]any
	}{
		"into another item": {"", rename("SRC/subtitle-3.de.srt", other+"/sources/x/subtitle-1.de.srt")},
		"out of the record": {"", rename("SRC/subtitle-3.de.srt", ".work/incoming/Example Film (2020).de.srt")},
		"a removal":         {"", map[string]any{"op": "remove", "from": "SRC/x.nfo", "to": "elsewhere/x.nfo"}},
		"a climb":           {"", rename("SRC/../../../../b1/x.srt", "SRC/subtitle-3.de.srt")},
		"absolute":          {"", rename("SRC/x.srt", "/etc/x.srt")},
		"a rewrite":         {"", rewrite(other + "/item.json")},
		"an episode's":      {show, rename(show+"/episodes/f1f1f1f1-0000-4000-8000-0000000000f1/sources/x/a.srt", show+"/x.srt")},
		"an unknown op":     {"", map[string]any{"op": "copy", "from": "SRC/a.srt", "to": "SRC/b.srt"}},
		"an item's folder":  {"movies/../series/x", rename("movies/../series/x/a", "movies/../series/x/b")},
		"another's journal": {"-", nil},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNamesFixture(t)
			before := f.catalog(t)
			switch rel := bad.rel; {
			case rel == "-": // a line of the adoption's journal
				f.journal = append(f.journal, map[string]any{"seq": 99, "itemId": nnFilm, "op": "move", "state": "done"})
			default:
				if rel == "" {
					rel = f.rel
				}
				step := map[string]any{}
				for k, v := range bad.step {
					if s, ok := v.(string); ok {
						v = strings.ReplaceAll(s, "SRC/", f.rel+"/sources/"+nnSource+"/")
					}
					step[k] = v
				}
				f.run(rel, true, step)
			}
			rep, err := f.names(t)
			if !errors.Is(err, ErrNamesRefused) || rep.Items != 0 || rep.Rows != 0 {
				t.Fatalf("names: %+v, %v, want the journal refused", rep, err)
			}
			if got := f.catalog(t); got != before {
				t.Errorf("a refused journal changed the catalog:\n%s\nwas\n%s", got, before)
			}
		})
	}
}

// An item the run did not finish, one the catalog has not, one it keeps in
// another folder, and one a row of which would name a file that is not there
// are skipped, each saying why, their rows as they were; an item asked for
// that the journal does not name is too.
func TestNamesSkipsWhatItCannotTake(t *testing.T) {
	f := newNamesFixture(t)
	if err := os.Remove(filepath.Join(VersionDir(f.itemDir, nnVersion), "original.mkv")); err != nil {
		t.Fatal(err)
	}
	const unknown = "movies/f2/f2f2f2f2-0000-4000-8000-0000000000f2"
	const unfinished = "series/e1/e1e1e1e1-0000-4000-8000-0000000000e1"
	storetest.AddItem(t, f.st, "e1e1e1e1-0000-4000-8000-0000000000e1", "series", "Example Show", "")
	f.run(unknown, true, rename(unknown+"/versions/x/a.mkv", unknown+"/versions/x/original.mkv"))
	f.run(unfinished, false, rename(unfinished+"/extras/x/a.mkv", unfinished+"/extras/x/original.mkv"))
	f.run("movies/zz/"+nnFilm, true, rename("movies/zz/"+nnFilm+"/a.srt", "movies/zz/"+nnFilm+"/b.srt"))
	before := f.catalog(t)
	rep, err := f.names(t)
	if err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, s := range rep.Skipped {
		why[s.ItemID] += s.Reason + ";"
	}
	for id, says := range map[string][]string{
		nnFilm: {"where the run left a file the catalog names, is not there",
			"the catalog keeps the item in " + f.rel + ", not in movies/zz/" + nnFilm},
		"f2f2f2f2-0000-4000-8000-0000000000f2": {"the catalog has no item of " + unknown},
		"e1e1e1e1-0000-4000-8000-0000000000e1": {"the run did not finish " + unfinished},
	} {
		for _, s := range says {
			if !strings.Contains(why[id], s) {
				t.Errorf("%s: skipped %q, want it to say %q", id, why[id], s)
			}
		}
	}
	if rep.Items != 0 || rep.Rows != 0 || len(rep.Skipped) != 4 {
		t.Errorf("names: %+v, want 4 skipped and nothing changed", rep)
	}
	if got := f.catalog(t); got != before {
		t.Errorf("the catalog of a film skipped:\n%s\nwas\n%s", got, before)
	}

	rep, err = f.names(t, nnFilm, "a2a2a2a2-0000-4000-8000-0000000000a2")
	if err != nil || len(rep.Skipped) != 3 || rep.Skipped[0].ItemID != "a2a2a2a2-0000-4000-8000-0000000000a2" ||
		rep.Skipped[0].Reason != "the run's journal names no folder of it" {
		t.Errorf("names of two items: %+v, %v, want the one it does not name said first", rep, err)
	}
}

// Where the steps left each file: a file renamed and renamed again where the
// last rename left it (from each place on its way, where a call between two
// runs pointed its rows), a removed one in the run's removed/, a file
// renamed back where it was (from the place it was at on its way); two files
// through one path cannot be told apart.
func TestTheMovesOfAJournal(t *testing.T) {
	p := Paths{Root: "/lib", Work: "/lib/.work"}
	m := &Migration{p: p, Run: nnRun, Dir: p.MigrationDir(nnRun)}
	moves, twice := m.movesOf([]namesStep{
		{Op: "rename", From: "m/a", To: "m/b"}, {Op: "rewrite", Path: "m/r"}, {Op: "rename", From: "m/b", To: "m/c"},
		{Op: "remove", From: "m/n", To: "removed/m/n"}, {Op: "rename", From: "m/x", To: "m/y"}, {Op: "rename", From: "m/y", To: "m/x"},
	})
	want := map[string]string{"/lib/m/a": "/lib/m/c", "/lib/m/b": "/lib/m/c", "/lib/m/n": "/lib/.work/migration/neutral-names/removed/m/n",
		"/lib/m/y": "/lib/m/x"}
	if twice != "" || len(moves) != len(want) {
		t.Fatalf("moves %v (twice %q), want %v", moves, twice, want)
	}
	for from, to := range want {
		if moves[from] != to {
			t.Errorf("%s: moved to %q, want %q", from, moves[from], to)
		}
	}
	if _, twice := m.movesOf([]namesStep{{Op: "rename", From: "m/a", To: "m/b"}, {Op: "rename", From: "m/c", To: "m/a"}}); twice != "/lib/m/a" {
		t.Errorf("two files through one path: %q, want /lib/m/a", twice)
	}
}
