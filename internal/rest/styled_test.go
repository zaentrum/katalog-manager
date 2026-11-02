package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// styledRows are the item's subtitle rows with their styled originals, by
// path: path styledpath styledformat ("-" for none).
func styledRows(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT path, COALESCE(styledpath, '-'), COALESCE(styledformat, '-')
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY path COLLATE "C"`, item)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var path, styled, format string
		if err := rows.Scan(&path, &styled, &format); err != nil {
			t.Fatal(err)
		}
		out = append(out, path+" "+styled+" "+format)
	}
	return strings.Join(out, "\n")
}

// fontRows are the item's font rows in their order: id versionid path
// format names ordinal sourcestreamindex.
func fontRows(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT id, COALESCE(versionid, '-'), path, COALESCE(format, '-'),
		names::text, ordinal, COALESCE(sourcestreamindex::text, '-')
		FROM com_nalet_katalog_fontassets WHERE item_id = $1 ORDER BY ordinal, id`, item)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, version, path, format, names, index string
		var ordinal int
		if err := rows.Scan(&id, &version, &path, &format, &names, &ordinal, &index); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %s %d %s", id, version, path, format, names, ordinal, index))
	}
	return strings.Join(out, "\n")
}

// styledVersion writes the version vid of package pid whose first subtitle
// keeps its styled original and that carries two fonts — one of no format
// its bytes are — or, plain, neither; it answers its .complete.
func (f *v2Film) styledVersion(t *testing.T, vid, pid string, plain bool) string {
	t.Helper()
	files := maps.Clone(librarytest.DefaultFiles)
	subtitles := []map[string]any{
		{"id": "sub0", "path": "subs/0.vtt", "language": "ger", "title": "", "default": false, "forced": false,
			"format": "webvtt", "sourceStreamIndex": 4},
		{"id": "sub1", "path": "subs/1.vtt", "language": "spa", "title": "Signs", "name": "Spanish (signs)",
			"default": false, "forced": true, "format": "webvtt", "sourceStreamIndex": 3}}
	pkg := map[string]any{"durationMs": 600000, "peakBandwidthBps": 4200000, "subtitles": subtitles,
		"renditions": map[string]any{
			"video": []map[string]any{{"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920,
				"height": 800, "bitrateBps": 4000000}},
			"audio": []map[string]any{{"id": "a0", "dir": "hls/a0", "codec": "mp4a.40.2", "language": "eng",
				"title": "", "default": true, "channels": 2, "bitrateBps": 128000, "sourceStreamIndex": 2}}}}
	if !plain {
		files["subs/0.ass"] = "[Script Info]\nScriptType: v4.00+\n\n[Events]\n"
		files["subs/fonts/0.ttf"] = "\x00\x01\x00\x00 a font"
		files["subs/fonts/1.bin"] = "bytes of no font's format"
		subtitles[0]["styled"] = map[string]any{"path": "subs/0.ass", "format": "ass"}
		pkg["fonts"] = []map[string]any{
			{"path": "subs/fonts/0.ttf", "format": "ttf", "names": []string{"Example Sans", "ExampleSans-Regular"},
				"sourceStreamIndex": 5},
			{"path": "subs/fonts/1.bin", "format": nil, "names": []string{}, "sourceStreamIndex": 6}}
	}
	return librarytest.WriteVersion(t, library.VersionDir(f.itemDir, vid), librarytest.Version{VersionID: vid,
		PackageID: pid, SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Files: files, Package: pkg})
}

// A version whose package keeps a styled subtitle's script beside its WebVTT
// file and carries fonts is taken with them: the subtitle's row names its
// script (styledpath, styledformat), every other row none, and the title's
// font rows are the package's, each by its name-based id, in their order,
// a font of no format kept without one. Taken again, nothing changes; a
// version that supersedes it with a package of neither takes them away.
func TestPackagingCompleteKeepsTheStyledOriginalsAndTheFonts(t *testing.T) {
	f := newV2Film(t)
	complete := f.styledVersion(t, filmVersion, filmPackage, false)
	if code, answer := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	vdir := library.VersionDir(f.itemDir, filmVersion)
	wantStyled := fmt.Sprintf(`%[1]s/A Film (2024)/A Film (2024).de.srt - -
%[2]s/subs/0.vtt %[2]s/subs/0.ass ass
%[2]s/subs/1.vtt - -`, f.cfg.Roots(true).Arrivals, vdir)
	if got := styledRows(t, f.st, filmItem); got != wantStyled {
		t.Errorf("the subtitles' styled originals:\n%s\nwant:\n%s", got, wantStyled)
	}
	wantFonts := fmt.Sprintf(`%[1]s %[2]s %[3]s/subs/fonts/0.ttf ttf ["Example Sans", "ExampleSans-Regular"] 0 5
%[4]s %[2]s %[3]s/subs/fonts/1.bin - [] 1 6`, library.FontID(filmItem, filmVersion, 0), filmVersion, vdir,
		library.FontID(filmItem, filmVersion, 1))
	if got := fontRows(t, f.st, filmItem); got != wantFonts {
		t.Errorf("the fonts:\n%s\nwant:\n%s", got, wantFonts)
	}
	// Again: nothing changes, the fonts' ids neither.
	if code, _ := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("taken again: %d", code)
	}
	if got := styledRows(t, f.st, filmItem) + "\n" + fontRows(t, f.st, filmItem); got != wantStyled+"\n"+wantFonts {
		t.Errorf("taken again:\n%s", got)
	}
	// A version of a package of neither supersedes it: no styled original,
	// no font.
	const next, nextPackage = "9a2e0000-0000-4000-8000-0000000000a9", "4f1d0000-0000-4000-8000-0000000000a9"
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		next, filmItem, []string{filmSource})
	plain := f.styledVersion(t, next, nextPackage, true)
	if code, answer := f.complete(t, f.payload(next, nextPackage, plain)); code != http.StatusOK {
		t.Fatalf("the version after: %d %v", code, answer)
	}
	if got := fontRows(t, f.st, filmItem); got != "" {
		t.Errorf("a package of no font leaves fonts:\n%s", got)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
		AND styledpath IS NOT NULL`, filmItem); n != 0 {
		t.Errorf("%d subtitle(s) keep a styled original their package has none of", n)
	}
}

// What a package the packager made before the catalog kept its styled
// originals and fonts carries is registered from its package.json, by
// POST /api/library/styled: the rows the handover left without them get
// them; asked again, nothing changes; a title whose package's record is gone
// is left, with why, and the others are registered all the same. For the
// workers and admins alone.
func TestRegisterStyledFromThePackagesRecords(t *testing.T) {
	f := newV2Film(t)
	complete := f.styledVersion(t, filmVersion, filmPackage, false)
	if code, answer := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	want := styledRows(t, f.st, filmItem) + "\n" + fontRows(t, f.st, filmItem)
	// as a catalog from before kept them: no styled original, no font
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_subtitleassets SET styledpath = NULL, styledformat = NULL`)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_fontassets`)
	// and a title whose package's record is gone
	const gone = "f1000000-0000-4000-8000-00000000dead"
	storetest.AddItem(t, f.st, gone, "movie", "Another Film", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, kind, isprimary)
		VALUES ('pkg-gone', $1, $2, 'packaged', false)`, gone, f.itemDir+"/../gone/package.json")

	register := func(body string) map[string]any {
		t.Helper()
		w := do(f.h, http.MethodPost, "/api/library/styled", body, f.svc)
		if w.Code != http.StatusOK {
			t.Fatalf("POST /api/library/styled: %d %s", w.Code, w.Body.String())
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := register("")
	if first["items"] != 1.0 || first["subtitles"] != 1.0 || first["fonts"] != 2.0 {
		t.Errorf("registered: %v", first)
	}
	skipped, _ := first["skipped"].([]any)
	if len(skipped) != 1 || !strings.Contains(fmt.Sprint(skipped[0]), gone) ||
		!strings.Contains(fmt.Sprint(skipped[0]), "is not there") {
		t.Errorf("the title whose record is gone: %v", skipped)
	}
	if got := styledRows(t, f.st, filmItem) + "\n" + fontRows(t, f.st, filmItem); got != want {
		t.Errorf("registered:\n%s\nwant:\n%s", got, want)
	}
	again := register(`{"items": ["` + filmItem + `"]}`)
	if again["items"] != 1.0 || again["subtitles"] != 0.0 || again["fonts"] != 0.0 {
		t.Errorf("registered again: %v", again)
	}
	if w := do(f.h, http.MethodPost, "/api/library/styled", "", f.iss.Viewer(t)); w.Code != http.StatusForbidden {
		t.Errorf("a viewer registers: %d", w.Code)
	}
	if w := do(f.h, http.MethodPost, "/api/library/styled", `{"items": 3}`, f.svc); w.Code != http.StatusBadRequest {
		t.Errorf("a body of no items: %d", w.Code)
	}
}

// The package store's manifest names its styled originals and fonts as
// package.json does: packaging-complete keeps them, each font of no version.
func TestPackagingCompleteKeepsAStoredPackagesStyledOriginalsAndFonts(t *testing.T) {
	st := storetest.Open(t)
	cfg := testConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	pkg := cfg.PackagesRoot + "/movies/f1/" + filmItem
	var manifest map[string]any
	if err := json.Unmarshal([]byte(packagedManifest), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["subtitles"].([]any)[1].(map[string]any)["styled"] = map[string]any{"path": "subs/1.ass", "format": "ass"}
	manifest["fonts"] = []any{map[string]any{"path": "subs/fonts/0.otf", "format": "otf", "names": []any{"Example Serif"},
		"sourceStreamIndex": 7}, map[string]any{"path": "../../elsewhere.ttf", "format": "ttf", "names": []any{}}}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", string(body), svc); w.Code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %s", w.Code, w.Body.String())
	}
	if got, want := styledRows(t, st, filmItem), fmt.Sprintf("%[1]s/subs/0.vtt - -\n%[1]s/subs/1.vtt %[1]s/subs/1.ass ass", pkg); got != want {
		t.Errorf("the subtitles' styled originals:\n%s\nwant:\n%s", got, want)
	}
	// a font outside the package is none of its fonts
	if got, want := fontRows(t, st, filmItem), fmt.Sprintf(`%s - %s/subs/fonts/0.otf otf ["Example Serif"] 0 7`,
		library.FontID(filmItem, "", 0), pkg); got != want {
		t.Errorf("the fonts:\n%s\nwant:\n%s", got, want)
	}
}
