package library

import (
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The package a test keeps the styled originals and the fonts of: its first
// subtitle's script beside it, two fonts — one of no format — and one whose
// path leaves the package, which is none of its fonts.
func styledPackage() map[string]any {
	return map[string]any{
		"subtitles": []any{
			map[string]any{"id": "s0", "path": "subs/0.vtt", "styled": map[string]any{"path": "subs/0.ass", "format": "ass"}},
			map[string]any{"id": "s1", "path": "subs/1.vtt", "styled": map[string]any{"path": "subs/1.ass", "format": "srt"}},
			map[string]any{"id": "s2", "path": "subs/2.vtt", "styled": map[string]any{"path": "../../2.ass", "format": "ass"}},
		},
		"fonts": []any{
			map[string]any{"path": "subs/fonts/0.ttf", "format": "ttf", "names": []any{"Example Sans", " ", "ExampleSans-Regular"},
				"sourceStreamIndex": json.Number("5")},
			map[string]any{"path": "subs/fonts/1.bin", "format": nil, "names": []any{}},
			map[string]any{"path": "../../evil.ttf", "format": "ttf", "names": []any{"Example Evil"}},
		},
	}
}

// SyncStyled gives a package's subtitle rows the scripts it keeps beside them
// — only in a format the catalog knows, only inside the package — and the
// title the package's fonts, each by a name-based id; called again it
// changes nothing; a package that keeps less takes the rest away; a sidecar's
// row, which names no file of the package, is left as it is.
func TestSyncStyledKeepsWhatThePackagesRecordSays(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const item, version = "a1a1a1a1-0000-4000-8000-0000000000a1", "b2b2b2b2-0000-4000-8000-0000000000b2"
	root := "/library/movies/a1/" + item + "/versions/" + version
	storetest.AddItem(t, st, item, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang) VALUES
		('s0', $1, $2, 'webvtt', 'en'), ('s1', $1, $3, 'webvtt', 'de'), ('s2', $1, $4, 'webvtt', 'fr'),
		('side', $1, '/library/.work/incoming/A Film/a.en.ass', 'ass', 'en')`,
		item, root+"/subs/0.vtt", root+"/subs/1.vtt", root+"/subs/2.vtt")
	pool := st.Pool()
	rows := func() string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'styled', styledpath,
				'format', styledformat) ORDER BY id), '[]')::text FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`,
			item).Scan(&out); err != nil {
			t.Fatal(err)
		}
		var fonts string
		if err := pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id', id, 'version', versionid,
				'path', path, 'format', format, 'names', names, 'ordinal', ordinal, 'stream', sourcestreamindex)
				ORDER BY ordinal), '[]')::text FROM com_nalet_katalog_fontassets WHERE item_id = $1`, item).Scan(&fonts); err != nil {
			t.Fatal(err)
		}
		return out + "\n" + fonts
	}
	n, err := SyncStyled(ctx, pool, item, root, version, styledPackage())
	if err != nil || n != (StyledCounts{Subtitles: 1, Fonts: 2}) {
		t.Fatalf("SyncStyled: %+v, %v", n, err)
	}
	want := `[{"id": "s0", "format": "ass", "styled": "` + root + `/subs/0.ass"}, {"id": "s1", "format": null, "styled": null}, ` +
		`{"id": "s2", "format": null, "styled": null}, {"id": "side", "format": null, "styled": null}]` + "\n" +
		`[{"id": "` + FontID(item, version, 0) + `", "path": "` + root + `/subs/fonts/0.ttf", "names": ["Example Sans", ` +
		`"ExampleSans-Regular"], "format": "ttf", "stream": 5, "ordinal": 0, "version": "` + version + `"}, ` +
		`{"id": "` + FontID(item, version, 1) + `", "path": "` + root + `/subs/fonts/1.bin", "names": [], "format": null, ` +
		`"stream": null, "ordinal": 1, "version": "` + version + `"}]`
	if got := rows(); got != want {
		t.Errorf("after SyncStyled:\n%s\nwant:\n%s", got, want)
	}
	if n, err := SyncStyled(ctx, pool, item, root, version, styledPackage()); err != nil || n != (StyledCounts{}) {
		t.Errorf("SyncStyled again: %+v, %v", n, err)
	}
	if got := rows(); got != want {
		t.Errorf("after SyncStyled again:\n%s", got)
	}
	// a package of the same subtitles that keeps no script and no font
	less := maps.Clone(styledPackage())
	less["subtitles"] = []any{map[string]any{"id": "s0", "path": "subs/0.vtt"}}
	delete(less, "fonts")
	if n, err := SyncStyled(ctx, pool, item, root, version, less); err != nil || n != (StyledCounts{Subtitles: 1, Fonts: 2}) {
		t.Errorf("SyncStyled of less: %+v, %v", n, err)
	}
	if got := rows(); got != `[{"id": "s0", "format": null, "styled": null}, {"id": "s1", "format": null, "styled": null}, `+
		`{"id": "s2", "format": null, "styled": null}, {"id": "side", "format": null, "styled": null}]`+"\n[]" {
		t.Errorf("after SyncStyled of less:\n%s", got)
	}
}

// The same font of the same package keeps its id, a font of the package
// store's is the title's at its place, and no two places share one. The id
// is the one the library's tools give the font's row
// (library-v2-rebuild.py: did(item, "font", version, ordinal)).
func TestFontIDIsTheSameForTheSameFont(t *testing.T) {
	const item, version = "a1a1a1a1-0000-4000-8000-0000000000a1", "b2b2b2b2-0000-4000-8000-0000000000b2"
	if got := FontID(item, version, 0); got != "62df731b-f6d8-5eab-af65-87a2ad1779e1" {
		t.Errorf("FontID: %s, not the id the library's tools give the font", got)
	}
	ids := map[string]bool{}
	for _, id := range []string{FontID(item, version, 0), FontID(item, version, 1), FontID(item, "", 0),
		FontID(item, "c3c3c3c3-0000-4000-8000-0000000000c3", 0)} {
		if !ValidID(id) || ids[id] {
			t.Errorf("font id %s is no id of its own", id)
		}
		ids[id] = true
	}
	if FontID(item, version, 0) != FontID(item, version, 0) || FontID(item, "", 0) != FontID(item, "store", 0) {
		t.Error("a font's id is not the same for the same font")
	}
}

// An adoption registers the styled originals and fonts of the package each
// title plays, as its package.json says, once the subtitles' rows name their
// files in the version's folder; its revert takes them back with the rest.
func TestAnAdoptionRegistersTheStyledOriginalsAndFonts(t *testing.T) {
	saved := mgPackage
	t.Cleanup(func() { mgPackage = saved })
	pkg := maps.Clone(saved)
	pkg["subtitles"] = []any{
		map[string]any{"id": "s0", "path": "subs/0.vtt", "styled": map[string]any{"path": "subs/0.ass", "format": "ass"}},
		map[string]any{"id": "s1", "path": "subs/1.vtt"}}
	pkg["fonts"] = []any{map[string]any{"path": "subs/fonts/0.ttf", "format": "ttf", "names": []any{"Example Sans"},
		"sourceStreamIndex": 3}}
	mgPackage = pkg
	f := newMigrationFixture(t)
	ctx := context.Background()
	before := f.rows(t, "subtitleassets", "t.item_id = $1", mgFilm) + "\n" + f.rows(t, "fontassets", "true")
	if rep, err := f.migration(t).Adopt(ctx, nil); err != nil || rep.Adopted != 4 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	vid := IDOf(mgFilm + ":version")
	fdir := filepath.Join(f.itemDir(mgFilm), "versions", vid)
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1
		AND path = $2 AND styledpath = $3 AND styledformat = 'ass'`, "sp-"+mgFilm[:4], fdir+"/subs/0.vtt", fdir+"/subs/0.ass"); n != 1 {
		t.Errorf("the film's subtitle names no script:\n%s", f.rows(t, "subtitleassets", "t.item_id = $1", mgFilm))
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_fontassets WHERE id = $1 AND item_id = $2
		AND versionid = $3 AND path = $4 AND format = 'ttf' AND names = '["Example Sans"]' AND sourcestreamindex = 3`,
		FontID(mgFilm, vid, 0), mgFilm, vid, fdir+"/subs/fonts/0.ttf"); n != 1 {
		t.Errorf("the film's font:\n%s", f.rows(t, "fontassets", "true"))
	}
	if rep, err := f.migration(t).Revert(ctx, nil); err != nil || rep.Reverted != 4 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if got := f.rows(t, "subtitleassets", "t.item_id = $1", mgFilm) + "\n" + f.rows(t, "fontassets", "true"); got != before {
		t.Errorf("after the revert:\n%s\nbefore the adopt:\n%s", got, before)
	}
}

// What the catalog cannot keep as the record gives it is left out, counted
// and logged, and the rest kept: a font whose name holds a NUL, one whose
// attachment's index no integer column holds, one whose path holds a NUL or
// is longer than its column, and a styled original whose path is.
func TestSyncStyledLeavesOutWhatTheCatalogCannotKeep(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const item, version = "a1a1a1a1-0000-4000-8000-0000000000a1", "b2b2b2b2-0000-4000-8000-0000000000b2"
	root := "/library/movies/a1/" + item + "/versions/" + version
	storetest.AddItem(t, st, item, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang) VALUES
		('s0', $1, $2, 'webvtt', 'en'), ('s1', $1, $3, 'webvtt', 'de')`, item, root+"/subs/0.vtt", root+"/subs/1.vtt")
	long := "subs/fonts/" + strings.Repeat("x", 2048) + ".ttf"
	pkg := map[string]any{
		"subtitles": []any{
			map[string]any{"id": "s0", "path": "subs/0.vtt", "styled": map[string]any{"path": "subs/0.ass", "format": "ass"}},
			map[string]any{"id": "s1", "path": "subs/1.vtt", "styled": map[string]any{"path": "subs/1\x00.ass", "format": "ass"}},
		},
		"fonts": []any{
			map[string]any{"path": "subs/fonts/0.ttf", "format": "ttf", "names": []any{"Example\x00Sans"}},
			map[string]any{"path": "subs/fonts/1.ttf", "format": "ttf", "names": []any{"Example Serif"},
				"sourceStreamIndex": json.Number("2147483648")},
			map[string]any{"path": "subs/fonts/2\x00.ttf", "format": "ttf", "names": []any{"Example Mono"}},
			map[string]any{"path": long, "format": "ttf", "names": []any{"Example Wide"}},
			map[string]any{"path": "subs/fonts/4.otf", "format": "otf", "names": []any{"Example Round"},
				"sourceStreamIndex": json.Number("2147483647")},
		},
	}
	n, err := SyncStyled(ctx, st.Pool(), item, root, version, pkg)
	if err != nil || n != (StyledCounts{Subtitles: 1, Fonts: 1, Left: 5}) {
		t.Fatalf("SyncStyled: %+v, %v", n, err)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
		AND ((id = 's0' AND styledpath = $2) OR (id = 's1' AND styledpath IS NULL))`, item, root+"/subs/0.ass"); got != 2 {
		t.Errorf("the styled originals: %d of 2 as they should be", got)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_fontassets WHERE item_id = $1`, item); got != 1 ||
		storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_fontassets WHERE id = $1 AND ordinal = 4
			AND sourcestreamindex = 2147483647`, FontID(item, version, 4)) != 1 {
		t.Errorf("the fonts kept: %d, want the one the catalog can keep", got)
	}
	// in a transaction the same, the transaction going on
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if n, err := SyncStyled(ctx, tx, item, root, version, pkg); err != nil || n != (StyledCounts{Left: 5}) {
		t.Errorf("SyncStyled in a transaction: %+v, %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("the transaction after it: %v", err)
	}
}

// A title deleted while its fonts are written keeps none: the delete waits
// for the writer's transaction (the fonts' foreign key holds the title's row)
// and then takes the fonts it wrote with the title.
func TestATitleDeletedWhileItsFontsAreWrittenKeepsNone(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	const item, version = "a1a1a1a1-0000-4000-8000-0000000000a1", "b2b2b2b2-0000-4000-8000-0000000000b2"
	storetest.AddItem(t, st, item, "movie", "A Film", "")
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if n, err := SyncStyled(ctx, tx, item, "/library/movies/a1/"+item+"/versions/"+version, version, styledPackage()); err != nil ||
		n.Fonts != 2 {
		t.Fatalf("SyncStyled: %+v, %v", n, err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, _, err := st.DeleteItemsAndUnlink(ctx, []string{item}, store.Deletion{By: "a test", Reason: "deleted while its fonts are written"})
		deleted <- err
	}()
	select {
	case err := <-deleted:
		t.Fatalf("the delete did not wait for the writer: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_fontassets`); n != 0 {
		t.Errorf("%d font row(s) of the deleted title", n)
	}
}
