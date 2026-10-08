package rest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// recordOf reads the worker record at path as the service account.
func recordOf(t *testing.T, h http.Handler, token, path string) map[string]any {
	t.Helper()
	w := do(h, http.MethodGet, path, "", token)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// With the v2 layout an item's worker record carries the library key: the
// contract, the share's root, the item's folder (recorded now: its item.json
// is written), its source (made now for a title from before 040: its size,
// quick hash and place among the arrivals), the inbox, the version the run
// builds (made now, the same across the run's retries) with the chapters and
// the detected ranges the version keeps, and the version that plays. The
// legacy layout's record has no such key.
func TestTheWorkerRecordNamesTheLibrary(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/Sintel (2010)/Sintel (2010).mkv": 3000})
	original := cfg.ArrivalsRoot + "/Sintel (2010)/Sintel (2010).mkv"
	storetest.AddItem(t, st, filmItem, "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes)
		VALUES ('src-f1', $1, $2, true, 3000)`, filmItem, original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemchapters (id, item_id, startms, endms, title, ordinal) VALUES
		('c2', $1, 61000, 120000, 'The Gate', 2), ('c1', $1, 0, 61000, ' Opening ', 1)`, filmItem)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_mediasegments (id, item_id, kind, startms, endms, source, confidence, label) VALUES
		('g1', $1, 'credits', 828000, 888000, 'chapter', 0.9, NULL), ('g2', $1, 'sponsor', 1000, 0, 'manual', NULL, 'x')`, filmItem)
	if rec := recordOf(t, h, svc, "/api/analyze/items/"+filmItem); rec["library"] != nil {
		t.Fatalf("the legacy layout's record names a library: %v", rec["library"])
	}
	v2Layout(t, st)

	rec := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	if rec["path"] != original {
		t.Errorf("the record's path: %v", rec["path"])
	}
	lib, _ := rec["library"].(map[string]any)
	itemDir := dir + "/movies/f1/" + filmItem
	src, _ := lib["source"].(map[string]any)
	build, _ := lib["build"].(map[string]any)
	vid, _ := build["versionId"].(string)
	sid, _ := src["sourceId"].(string)
	if lib["contract"] != 1.0 || lib["root"] != dir || lib["itemDir"] != itemDir || lib["blocked"] != nil ||
		lib["inboxDir"] != dir+"/.work/inbox/"+filmItem || lib["current"] != nil {
		t.Errorf("the library: %v", lib)
	}
	_, qh1, _ := library.QH1(original)
	if len(sid) != 36 || src["recorded"] != false || src["recordDir"] != itemDir+"/sources/"+sid ||
		src["libraryPath"] != "Sintel (2010)/Sintel (2010).mkv" || src["sizeBytes"] != 3000.0 || src["qh1"] != qh1 {
		t.Errorf("the source: %v", src)
	}
	if len(vid) != 36 || build["stagingDir"] != dir+"/.work/staging/"+vid || build["versionDir"] != itemDir+"/versions/"+vid ||
		build["createdBy"] != "katalog-manager" || build["chaptersFrom"] != "original-file" {
		t.Errorf("the build: %v", build)
	}
	marks, _ := json.Marshal([]any{build["chapters"], build["segments"]})
	if want := `[[{"endMs":61000,"startMs":0,"title":"Opening"},{"endMs":120000,"startMs":61000,"title":"The Gate"}],` +
		`[{"confidence":null,"detector":"manual","endMs":1000,"kind":"other","label":"sponsor","startMs":1000},` +
		`{"confidence":0.9,"detector":"chapter","endMs":888000,"kind":"credits","label":null,"startMs":828000}]]`; string(marks) != want {
		t.Errorf("the marks:\n%s\nwant\n%s", marks, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND recordedat IS NOT NULL`, filmItem); n != 1 {
		t.Error("the item was not recorded")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'src-f1' AND sourceid = $1`, sid); n != 1 {
		t.Error("the primary asset does not name its source")
	}
	// The run's retries build the same version; once it is complete, it plays,
	// and the next run builds another.
	again, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
	if b, _ := again["build"].(map[string]any); b["versionId"] != vid {
		t.Errorf("a retry builds %v, want %s", b["versionId"], vid)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'complete', dir = $2 WHERE id = $1`, vid, itemDir+"/versions/"+vid)
	next, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
	cur, _ := next["current"].(map[string]any)
	nb, _ := next["build"].(map[string]any)
	if cur["versionId"] != vid || cur["dir"] != itemDir+"/versions/"+vid || nb["versionId"] == vid {
		t.Errorf("after the version is complete: current %v, build %v", cur, nb["versionId"])
	}
	// Without chapters, the version keeps none.
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_itemchapters`)
	none, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
	if b, _ := none["build"].(map[string]any); b["chaptersFrom"] != nil || len(b["chapters"].([]any)) != 0 {
		t.Errorf("without chapters: %v", b)
	}
}

// An item that cannot be recorded says why in its record, the packager's to
// fail its step with: an episode without its numbers in its series' folder,
// one without a series without a folder at all, nor a version to build.
func TestTheWorkerRecordSaysWhyAnItemIsNotRecorded(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	const series, episode, orphan = "5e5e5e5e-0000-4000-8000-000000000001", "e1e1e1e1-0000-4000-8000-000000000003",
		"e3e3e3e3-0000-4000-8000-000000000005"
	files(t, dir, map[string]int64{".work/incoming/series/Show/S01E01.mkv": 10, ".work/incoming/Orphan S01E01.mkv": 10})
	storetest.AddItem(t, st, series, "series", "Show", "")
	storetest.AddItem(t, st, episode, "episode", "Pilot", series)
	storetest.AddItem(t, st, orphan, "episode", "Orphan", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a-e', $1, $2, true), ('a-o', $3, $4, true)`, episode, cfg.ArrivalsRoot+"/series/Show/S01E01.mkv", orphan,
		cfg.ArrivalsRoot+"/Orphan S01E01.mkv")
	lib, _ := recordOf(t, h, svc, "/api/analyze/items/"+episode)["library"].(map[string]any)
	if lib["blocked"] != "an episode needs its season and episode numbers before it is recorded" ||
		lib["itemDir"] != dir+"/series/5e/"+series+"/episodes/"+episode || lib["build"] == nil {
		t.Errorf("an episode without its numbers: %v", lib)
	}
	lib, _ = recordOf(t, h, svc, "/api/analyze/items/"+orphan)["library"].(map[string]any)
	if lib["blocked"] != "an episode needs its series before it is recorded" || lib["itemDir"] != nil || lib["build"] != nil ||
		lib["source"] == nil {
		t.Errorf("an episode without a series: %v", lib)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE recordedat IS NOT NULL AND id <> $1`, series); n != 0 {
		t.Errorf("%d blocked items recorded", n)
	}
}

// The transcode's end makes, with the v2 layout, the version its package
// builds.
func TestTheTranscodesEndMakesTheVersion(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	files(t, dir, map[string]int64{".work/incoming/Sintel.mkv": 100})
	storetest.AddItem(t, st, filmItem, "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('src-f1', $1, $2, true)`,
		filmItem, cfg.ArrivalsRoot+"/Sintel.mkv")
	if w := do(h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/transcode", `{"status": "done"}`,
		iss.Service(t, "zaentrum-manager")); w.Code != http.StatusOK {
		t.Fatalf("the transcode's end: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemversions v JOIN com_nalet_katalog_itemsources s
		ON s.id = ANY(v.sourceids) WHERE v.item_id = $1 AND v.state = 'building'`, filmItem); n != 1 {
		t.Error("no version is built of the title's source")
	}
}

// With the v2 layout an extra's worker record carries the library key: its
// title's folder (recorded now), its own in it, the inbox and the staging,
// whether it is recorded, what its extra.json records, and its original.
func TestTheExtrasWorkerRecordNamesTheLibrary(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	storetest.AddItem(t, st, filmItem, "movie", "Sintel", "")
	trailer := filepath.Join(cfg.ExtrasRoot, "sintel", "trailer.mp4")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, sourcesize,
		sourceqh1, language, localizedtitles, createdat) VALUES ($1, $2, 'trailer', 'Trailer', 'api', $3, 1234, $4, 'zxx',
		'{"de": "Vorschau"}', '2026-10-06 08:00:00+00')`, extraX1, filmItem, trailer, "sha256:"+strings.Repeat("a", 64))
	w := do(h, http.MethodGet, "/api/analyze/extras/"+extraX1, "", iss.Service(t, "zaentrum-manager"))
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatalf("%d %s: %v", w.Code, w.Body.String(), err)
	}
	got := rec["library"]
	itemDir := dir + "/movies/f1/" + filmItem
	want := `{"contract":1,"itemDir":"` + itemDir + `","inboxDir":"` + dir + `/.work/inbox/extra-` + extraX1 + `",` +
		`"stagingDir":"` + dir + `/.work/staging/extra-` + extraX1 + `","extraDir":"` + itemDir + `/extras/` + extraX1 + `",` +
		`"recorded":false,"record":{"kind":"trailer","title":"Trailer","localizedTitles":{"de":"Vorschau"},"language":"zxx",` +
		`"seasonNumber":null,"origin":null,"createdAt":"2026-10-06T08:00:00Z","createdBy":"katalog-manager/api"},` +
		`"original":{"name":"trailer.mp4","sizeBytes":1234,"qh1":"sha256:` + strings.Repeat("a", 64) + `"}}`
	if string(got) != want {
		t.Errorf("the extra's library:\n%s\nwant\n%s", got, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND recordedat IS NOT NULL`, filmItem); n != 1 {
		t.Error("the extra's title was not recorded")
	}
}

// The worker record's build says what the run does with the source
// (library.RunOf), and the name the original gets in the version's folder
// when the run renames it in: establish while the source has no version, or
// takein while the title's step takein waits or runs, both naming the
// original by its extension alone (original.mkv); add while the source's
// version holds its original alone (taken), that version and its folder,
// no version made for it; repackage once a version of it is packaged, a new
// version, the original read where it is, in the older version's folder.
// Neither of the last two names the original. An original in its version's
// folder came with the files beside it where it arrived: its subtitle files,
// for add and for repackage, are the copies its source's record keeps of
// them, in sources/<sourceId>/.
func TestTheWorkerRecordSaysWhatTheRunDoes(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/Sintel (2010)/Sintel (2010) Bluray-1080p.MKV": 3000,
		".work/incoming/Sintel (2010)/Sintel (2010) Bluray-1080p.en.srt": 0})
	arrival := cfg.ArrivalsRoot + "/Sintel (2010)/Sintel (2010) Bluray-1080p.MKV"
	sub := cfg.ArrivalsRoot + "/Sintel (2010)/Sintel (2010) Bluray-1080p.en.srt"
	storetest.AddItem(t, st, filmItem, "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes)
		VALUES ('src-f1', $1, $2, true, 3000)`, filmItem, arrival)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label)
		VALUES ('s-en', $1, $2, 'srt', 'en', 'English')`, filmItem, sub)
	itemDir := dir + "/movies/f1/" + filmItem
	build := func() map[string]any {
		t.Helper()
		lib, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
		b, _ := lib["build"].(map[string]any)
		return b
	}
	subtitles := func() []any {
		t.Helper()
		files, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["subtitleFiles"].([]any)
		return files
	}

	b := build()
	vid, _ := b["versionId"].(string)
	if b["mode"] != "establish" || b["originalName"] != "original.mkv" || b["versionDir"] != itemDir+"/versions/"+vid {
		t.Errorf("a source with no version: %v", b)
	}
	if s := subtitles(); len(s) != 1 || s[0].(map[string]any)["path"] != sub {
		t.Errorf("the subtitle files beside an original where it arrived: %v", s)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('k1', $1, 'takein', 'pending')`, filmItem)
	if b := build(); b["mode"] != "takein" || b["originalName"] != "original.mkv" || b["versionId"] != vid {
		t.Errorf("a source taken in: %v", b)
	}

	// Taken in: the original in the version's folder, the source recorded
	// with a copy of its subtitle file.
	vdir := itemDir + "/versions/" + vid
	moved := vdir + "/original.mkv"
	if err := os.MkdirAll(vdir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(arrival, moved); err != nil {
		t.Fatal(err)
	}
	var sid string
	if err := st.Pool().QueryRow(t.Context(), `SELECT id FROM com_nalet_katalog_itemsources WHERE item_id = $1`, filmItem).Scan(&sid); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(sub)
	copyName := "subtitle-1.en.srt"
	librarytest.WriteSource(t, library.SourceDir(itemDir, sid), map[string]any{"sourceId": sid,
		"sidecars": []map[string]any{{"file": "sources/" + sid + "/" + copyName, "kind": "subtitle",
			"sha256": "sha256:" + library.SHA256(content)}}}, map[string]string{copyName: string(content)})
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'taken', dir = $2 WHERE id = $1`, vid, vdir)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2, filename = 'original.mkv', recordedat = now(),
		librarypath = NULL WHERE id = $1`, sid, moved)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET path = $1 WHERE id = 'src-f1'`, moved)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done' WHERE id = 'k1'`)
	if b := build(); b["mode"] != "add" || b["originalName"] != nil || b["versionId"] != vid || b["versionDir"] != vdir ||
		b["stagingDir"] != dir+"/.work/staging/"+vid {
		t.Errorf("a source taken in, its package next: %v", b)
	}
	if w := do(h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/transcode", `{"status": "done"}`, svc); w.Code != http.StatusOK {
		t.Fatalf("the transcode's end: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE state = 'building'`); n != 0 {
		t.Errorf("%d versions made for a run that adds its package to the one taken in", n)
	}
	wantCopy := library.SourceDir(itemDir, sid) + "/" + copyName
	if s := subtitles(); len(s) != 1 || s[0].(map[string]any)["path"] != wantCopy || s[0].(map[string]any)["id"] != "s-en" {
		t.Errorf("the subtitle files of an original in its version's folder: %v, want the copy %s", s, wantCopy)
	}

	// Packaged: the next run is a new version, the original read in the
	// folder of the one it was added to.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'complete' WHERE id = $1`, vid)
	rec := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	lib, _ := rec["library"].(map[string]any)
	b, _ = lib["build"].(map[string]any)
	if b["mode"] != "repackage" || b["originalName"] != nil || b["versionId"] == vid || rec["path"] != moved {
		t.Errorf("a packaged source: path %v, build %v", rec["path"], b)
	}
	if s, _ := rec["subtitleFiles"].([]any); len(s) != 1 || s[0].(map[string]any)["path"] != wantCopy {
		t.Errorf("the subtitle files of a packaged source whose original lies in a version's folder: %v, want the copy", s)
	}
}
