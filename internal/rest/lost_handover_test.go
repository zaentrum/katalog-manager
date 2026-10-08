package rest

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// lostFilm is a film of the v2 layout whose original arrived, its size and
// quick hash recorded with its source: the run whose handover is lost works
// on it.
type lostFilm struct {
	st       *store.Store
	cfg      config.Config
	h        http.Handler
	svc      string
	itemDir  string
	arrival  string
	sourceID string
}

func newLostFilm(t *testing.T) *lostFilm {
	t.Helper()
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	f := &lostFilm{st: st, cfg: cfg, h: h, svc: iss.Service(t, "zaentrum-manager"), itemDir: dir + "/movies/f1/" + filmItem,
		arrival: cfg.ArrivalsRoot + "/Sintel (2010)/Sintel (2010).mkv"}
	librarytest.Write(t, f.arrival, []byte(strings.Repeat("the picture and the sound ", 3000)))
	size, qh1, err := library.QH1(f.arrival)
	if err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, st, filmItem, "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes)
		VALUES ('src-f1', $1, $2, true, $3)`, filmItem, f.arrival, size)
	f.build(t) // the worker record makes the title's source
	if err := st.Pool().QueryRow(t.Context(), `SELECT id FROM com_nalet_katalog_itemsources WHERE item_id = $1`, filmItem).
		Scan(&f.sourceID); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND qh1 = $2`, f.sourceID, qh1); n != 1 {
		t.Fatal("the source is not recorded with the original's quick hash")
	}
	return f
}

// build reads the worker record: its path and its build.
func (f *lostFilm) build(t *testing.T) (string, map[string]any) {
	t.Helper()
	rec := recordOf(t, f.h, f.svc, "/api/analyze/items/"+filmItem)
	lib, _ := rec["library"].(map[string]any)
	b, _ := lib["build"].(map[string]any)
	path, _ := rec["path"].(string)
	return path, b
}

// place does what a run of the build b does up to its handover, which is
// lost: the source's record written, the version's folder placed with the
// original renamed into it under the build's name (with a package, unless
// takenIn). It answers the payload the run sends, and sends again.
func (f *lostFilm) place(t *testing.T, b map[string]any, takenIn bool) string {
	t.Helper()
	vid, vdir, name := b["versionId"].(string), b["versionDir"].(string), b["originalName"].(string)
	librarytest.WriteSource(t, library.SourceDir(f.itemDir, f.sourceID), map[string]any{"sourceId": f.sourceID}, nil)
	original := filepath.Join(vdir, name)
	if takenIn {
		librarytest.Write(t, filepath.Join(vdir, library.VersionFile), librarytest.JSON(t, map[string]any{
			"schema": "zaentrum.library.version/2", "versionId": vid, "sourceIds": []string{f.sourceID},
			"originalFiles": []string{name}}))
	} else {
		librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: vid, PackageID: filmPackage,
			SourceIDs: []string{f.sourceID}, CreatedAt: "2026-10-08T10:00:00Z", Version: map[string]any{"originalFiles": []string{name}}})
	}
	if err := os.Rename(f.arrival, original); err != nil {
		t.Fatal(err)
	}
	if takenIn {
		return fmt.Sprintf(`{"layout": "v2", "versionId": %q, "versionDir": %q, "sourceId": %q, "sourceRecorded": true,
			"takenIn": true, "sidecars": [], "source": {}, "original": {"path": %q, "name": %q}}`, vid, vdir, f.sourceID, original, name)
	}
	complete, _ := os.ReadFile(filepath.Join(vdir, library.CompleteFile))
	return fmt.Sprintf(`{"layout": "v2", "versionId": %q, "packageId": %q, "versionDir": %q, "complete": %q, "sourceId": %q,
		"sourceRecorded": true, "package": {}, "sidecars": [], "source": {}, "original": {"path": %q, "name": %q}}`,
		vid, filmPackage, vdir, strings.TrimSpace(string(complete)), f.sourceID, original, name)
}

// recorded is the source and the asset of the title's file as the catalog
// holds them, and the state of the version vid.
func (f *lostFilm) recorded(t *testing.T, vid string) string {
	t.Helper()
	var out string
	if err := f.st.Pool().QueryRow(t.Context(), `SELECT s.arrivalpath || ' ' || a.path || ' ' || v.state
		FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets a ON a.sourceid = s.id
		JOIN com_nalet_katalog_itemversions v ON v.id = $2 WHERE s.id = $1`, f.sourceID, vid).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// An establish whose handover was lost (the run placed its version's folder,
// the original renamed into it, and the catalog recorded nothing) is taken
// again as it was: the worker record names the same version, its folder,
// the same name for the original, mode establish, and the path where the
// original lies now (never its arrival, where nothing is); the transcode's
// end makes no other version. The payload the run sends again is taken: the
// version complete, the source and the title's file where the original lies.
func TestAnEstablishWhoseHandoverWasLostIsTakenAgainTheSame(t *testing.T) {
	f := newLostFilm(t)
	path, b := f.build(t)
	if path != f.arrival || b["mode"] != "establish" || b["originalName"] != "original.mkv" {
		t.Fatalf("the first run: %s %v", path, b)
	}
	payload := f.place(t, b, false)
	moved := filepath.Join(b["versionDir"].(string), "original.mkv")

	if w := do(f.h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/transcode", `{"status": "done"}`, f.svc); w.Code != http.StatusOK {
		t.Fatalf("the transcode's end: %d %s", w.Code, w.Body.String())
	}
	again, b2 := f.build(t)
	for _, k := range []string{"versionId", "versionDir", "stagingDir", "originalName", "mode"} {
		if b2[k] != b[k] {
			t.Errorf("the run taken again: %s is %v, the run whose handover was lost had %v", k, b2[k], b[k])
		}
	}
	if again != moved {
		t.Errorf("the run taken again is handed %s, want the original where it lies, %s", again, moved)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE item_id = $1`, filmItem); n != 1 {
		t.Errorf("%d versions, want the one", n)
	}
	w := do(f.h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", payload, f.svc)
	if w.Code != http.StatusOK {
		t.Fatalf("the payload sent again: %d %s", w.Code, w.Body.String())
	}
	if got, want := f.recorded(t, b["versionId"].(string)), moved+" "+moved+" complete"; got != want {
		t.Errorf("recorded: %q, want %q", got, want)
	}
}

// A take-in whose handover was lost the same: its run taken again names the
// same version, folder and name, mode takein, and the path where the
// original lies; the payload sent again is taken.
func TestATakeInWhoseHandoverWasLostIsTakenAgainTheSame(t *testing.T) {
	f := newLostFilm(t)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('k1', $1, 'takein', 'pending')`, filmItem)
	_, b := f.build(t)
	if b["mode"] != "takein" || b["originalName"] != "original.mkv" {
		t.Fatalf("the take-in's run: %v", b)
	}
	payload := f.place(t, b, true)
	moved := filepath.Join(b["versionDir"].(string), "original.mkv")
	// Its step failed (the handover lost), and its retry waits again.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed' WHERE id = 'k1'`)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'pending' WHERE id = 'k1'`)
	again, b2 := f.build(t)
	for _, k := range []string{"versionId", "versionDir", "originalName", "mode"} {
		if b2[k] != b[k] {
			t.Errorf("the take-in taken again: %s is %v, the run whose handover was lost had %v", k, b2[k], b[k])
		}
	}
	if again != moved {
		t.Errorf("the take-in taken again is handed %s, want %s", again, moved)
	}
	w := do(f.h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", payload, f.svc)
	if w.Code != http.StatusOK {
		t.Fatalf("the payload sent again: %d %s", w.Code, w.Body.String())
	}
	if got, want := f.recorded(t, b["versionId"].(string)), moved+" "+moved+" taken"; got != want {
		t.Errorf("recorded: %q, want %q", got, want)
	}
}

// No worker is sent to an arrival where nothing is when the original lies in
// a version's folder whose handover was lost: neither the title's own
// worker record nor an episode's siblings, which the analyzer reads. An
// original that is in neither place stays where the catalog has it.
func TestNoWorkerIsSentWhereNothingIs(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	const series, e1, e2 = "5e5e5e5e-0000-4000-8000-000000000001", "e1e1e1e1-0000-4000-8000-000000000003",
		"e2e2e2e2-0000-4000-8000-000000000004"
	storetest.AddItem(t, st, series, "series", "Show", "")
	storetest.AddItem(t, st, e1, "episode", "Pilot", series)
	storetest.AddItem(t, st, e2, "episode", "Second", series)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = $1`, e1)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2 WHERE id = $1`, e2)
	arrival := map[string]string{e1: cfg.ArrivalsRoot + "/Show/S01E01.mkv", e2: cfg.ArrivalsRoot + "/Show/S01E02.mkv"}
	for id, path := range arrival {
		librarytest.Write(t, path, []byte("the episode "+id))
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ($1, $1, $2, true)`, id, path)
		recordOf(t, h, svc, "/api/analyze/items/"+id) // its source, its version being built
	}
	var vid string
	if err := st.Pool().QueryRow(t.Context(), `SELECT id FROM com_nalet_katalog_itemversions WHERE item_id = $1`, e2).Scan(&vid); err != nil {
		t.Fatal(err)
	}
	vdir := dir + "/series/5e/" + series + "/episodes/" + e2 + "/versions/" + vid
	librarytest.Write(t, filepath.Join(vdir, library.VersionFile), []byte("{}\n"))
	moved := filepath.Join(vdir, "original.mkv")
	if err := os.Rename(arrival[e2], moved); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, h, svc, "/api/analyze/items/"+e2); rec["path"] != moved {
		t.Errorf("the episode's worker record: %v, want %s", rec["path"], moved)
	}
	w := do(h, http.MethodGet, "/api/analyze/items/"+e1+"/siblings", "", svc)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"path":"`+moved+`"`) {
		t.Errorf("the siblings: %d %s, want %s", w.Code, w.Body.String(), moved)
	}
	// Gone from both: the path stays the catalog's.
	if err := os.Remove(moved); err != nil {
		t.Fatal(err)
	}
	if rec := recordOf(t, h, svc, "/api/analyze/items/"+e2); rec["path"] != arrival[e2] {
		t.Errorf("an original gone: %v, want %s", rec["path"], arrival[e2])
	}
}

// The version being built whose folder a run placed is never made another
// file's: given another file meanwhile (replaceSource refuses it; a catalog
// changed by hand), the worker record says why it builds nothing, the
// transcode's end makes nothing, and the version stays its own file's.
func TestAPlacedVersionIsNeverMadeAnotherFiles(t *testing.T) {
	f := newLostFilm(t)
	_, b := f.build(t)
	f.place(t, b, false)
	other := f.cfg.ArrivalsRoot + "/Sintel 4K.mkv"
	librarytest.Write(t, other, []byte("another file"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('0c0c0c0c-0000-4000-8000-000000000009', $1, 'Sintel 4K.mkv', $2, 12, 'present')`, filmItem, other)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_playbackassets SET path = $1, sourceid = '0c0c0c0c-0000-4000-8000-000000000009'
		WHERE id = 'src-f1'`, other)
	rec := recordOf(t, f.h, f.svc, "/api/analyze/items/"+filmItem)
	lib, _ := rec["library"].(map[string]any)
	if why, _ := lib["blocked"].(string); !strings.Contains(why, "is in the library for another file of the title") || lib["build"] != nil {
		t.Errorf("the worker record: blocked %v, build %v", lib["blocked"], lib["build"])
	}
	if w := do(f.h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/transcode", `{"status": "done"}`, f.svc); w.Code != http.StatusOK {
		t.Fatalf("the transcode's end: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND sourceids = ARRAY[$2]::varchar[]`,
		b["versionId"], f.sourceID); n != 1 {
		t.Error("the placed version was made another file's")
	}
}
