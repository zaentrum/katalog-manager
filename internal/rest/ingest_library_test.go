package rest

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// v2Config is testConfig with the library's v2 layout's roots under dir.
func v2Config(dir string) config.Config {
	cfg := testConfig(dir)
	cfg.LibraryRoot, cfg.WorkRoot = dir, dir+"/.work"
	cfg.ArrivalsRoot, cfg.ExtrasRoot = dir+"/.work/incoming", dir+"/.work/extras"
	return cfg
}

// v2Layout switches the catalog to the library's v2 layout.
func v2Layout(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('layout', 'library.layout', 'v2')`)
}

// With the v2 layout an ingest takes a file in from the arrivals alone, as
// the scanner does: with its source, which its asset names; a file there is
// not is refused; one whose original moved here is that title, moved; a copy
// of an original still in its place, and one deleted after packaging, are
// refused, naming whose they are.
func TestIngestTakesArrivalsWithTheirSources(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/Sintel (2010).mkv": 3000, "media/Other.mkv": 10})
	ingest := func(path string) (int, string) {
		w := do(h, http.MethodPost, "/api/ingest", `{"path": "`+path+`", "type": "movie", "title": "Sintel"}`, svc)
		return w.Code, w.Body.String()
	}
	sintel := cfg.ArrivalsRoot + "/Sintel (2010).mkv"
	if code, body := ingest(sintel); code != http.StatusOK || !strings.Contains(body, `"created":true`) {
		t.Fatalf("an arrival: %d %s", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets p
		ON p.sourceid = s.id AND p.path = s.arrivalpath WHERE s.arrivalpath = $1 AND s.librarypath = 'Sintel (2010).mkv'
		AND s.sizebytes = 3000 AND s.qh1 LIKE 'sha256:%'`, sintel); n != 1 {
		t.Error("the arrival has no source its asset names")
	}
	if code, body := ingest(sintel); code != http.StatusOK || !strings.Contains(body, `"created":false`) {
		t.Errorf("the same file again: %d %s", code, body)
	}
	for path, says := range map[string]string{
		dir + "/media/Other.mkv":       "must be under the arrivals' root",
		cfg.ArrivalsRoot + "/None.mkv": "there is no file to take in",
	} {
		if code, body := ingest(path); code != http.StatusBadRequest || !strings.Contains(body, says) {
			t.Errorf("%s: %d %s, want 400 saying %s", path, code, body, says)
		}
	}
	// A copy, then the original moved.
	copied := cfg.ArrivalsRoot + "/copy/Sintel.mkv"
	files(t, dir, map[string]int64{".work/incoming/copy/Sintel.mkv": 3000})
	if code, body := ingest(copied); code != http.StatusConflict || !strings.Contains(body, "is a copy of the original of item") {
		t.Errorf("a copy: %d %s", code, body)
	}
	if err := os.Remove(sintel); err != nil {
		t.Fatal(err)
	}
	code, body := ingest(copied)
	if code != http.StatusOK || !strings.Contains(body, `"created":false`) {
		t.Errorf("the original moved: %d %s", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE path = $1`, copied); n != 1 {
		t.Error("the title did not follow its original")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 1 {
		t.Errorf("%d titles, want the one", n)
	}
	// Retired, it arrives again.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL`)
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets`)
	again := filepath.Join(cfg.ArrivalsRoot, "again", "Sintel.mkv")
	files(t, dir, map[string]int64{".work/incoming/again/Sintel.mkv": 3000})
	if code, body := ingest(again); code != http.StatusConflict || !strings.Contains(body, "use replaceSource to make it a new version") {
		t.Errorf("a retired original again: %d %s", code, body)
	}
}

// An original another file took the place of as its title's is no title of
// its own to an ingest either: neither superseded, nor a copy of it, nor one
// kept beside the file that took its place, at its very path, even recorded
// without its quick hash — each refused, naming whose it is.
func TestIngestRefusesAnOriginalAnotherFileTookThePlaceOf(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/Sintel.mkv": 3000, ".work/incoming/Bunny.mkv": 2000})
	ingest := func(path, title string) (int, string) {
		w := do(h, http.MethodPost, "/api/ingest", `{"path": "`+path+`", "type": "movie", "title": "`+title+`"}`, svc)
		return w.Code, w.Body.String()
	}
	sintel, bunny := cfg.ArrivalsRoot+"/Sintel.mkv", cfg.ArrivalsRoot+"/Bunny.mkv"
	for path, title := range map[string]string{sintel: "Sintel", bunny: "Bunny"} {
		if code, body := ingest(path, title); code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, code, body)
		}
	}
	// Each title has another file now: Sintel's original superseded by it,
	// Bunny's kept beside it, without its quick hash.
	files(t, dir, map[string]int64{".work/incoming/Sintel 4K.mkv": 5000, ".work/incoming/Bunny 4K.mkv": 4000})
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET path = replace(path, '.mkv', ' 4K.mkv'), sourceid = NULL`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'superseded', supersededby = id, supersededat = now()
		WHERE arrivalpath = $1`, sintel)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET qh1 = NULL WHERE arrivalpath = $1`, bunny)
	files(t, dir, map[string]int64{".work/incoming/copy/Sintel.mkv": 3000})
	for path, says := range map[string]string{
		sintel:                                "is an old original of item",
		cfg.ArrivalsRoot + "/copy/Sintel.mkv": "another file took its place (superseded): it is no title of its own",
		bunny:                                 "is an old original of item",
	} {
		if code, body := ingest(path, "Again"); code != http.StatusConflict || !strings.Contains(body, says) {
			t.Errorf("%s: %d %s, want 409 saying %s", path, code, body, says)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 2 {
		t.Errorf("%d titles, want the two", n)
	}
}

// An ingest of a file a replace gives a title in its transaction waits for
// it, under the lock both take on the path, and finds the file that title's:
// no title of its own is made of it.
func TestAnIngestWaitsForAReplaceOfItsFile(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/Sintel (2010).mkv": 3000})
	sintel := cfg.ArrivalsRoot + "/Sintel (2010).mkv"
	storetest.AddItem(t, st, "5e5e5e5e-0000-4000-8000-000000000005", "movie", "Sintel", "")
	ctx := context.Background()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('com_nalet_katalog_playbackassets:' || $1))`, sintel); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind)
		VALUES ('a-replaced', '5e5e5e5e-0000-4000-8000-000000000005', $1, 3000, true, 'primary')`, sintel); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		w := do(h, http.MethodPost, "/api/ingest", `{"path": "`+sintel+`", "type": "movie", "title": "Sintel"}`, svc)
		done <- fmt.Sprintf("%d %s", w.Code, w.Body.String())
	}()
	select {
	case got := <-done:
		t.Fatalf("the ingest did not wait for the replace: %s", got)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if !strings.HasPrefix(got, "200 ") || !strings.Contains(got, `"created":false`) ||
			!strings.Contains(got, "5e5e5e5e-0000-4000-8000-000000000005") {
			t.Errorf("the ingest once the replace is committed: %s", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the ingest did not go on")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 1 {
		t.Errorf("%d titles, want the one the replace gave the file", n)
	}
}
