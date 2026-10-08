package rest

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The catalog's side of a run of the neutral names: a journal that names a
// file outside the folder of its item is refused (422), saying where; a run
// with nothing journaled answers that it did nothing; a run that is not
// there is 404, and a name that is no run's 400.
func TestTheNamesOfARun(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.LibraryRoot = dir
	const film = "movies/a1/a1a1a1a1-0000-4000-8000-0000000000a1"
	librarytest.Write(t, filepath.Join(dir, ".work", "migration", "neutral-names", "journal.jsonl"),
		[]byte(`{"seq": 1, "at": "2026-10-08T10:00:00.000000Z", "item": "`+film+`", "op": "plan", "steps": [`+
			`{"op": "rename", "from": "`+film+`/sources/x/a.srt", "to": "people/a.srt"}]}`+"\n"))
	librarytest.Write(t, filepath.Join(dir, ".work", "migration", "empty", "units", ".keep"), nil)
	h, iss := server(t, st, cfg)
	token := iss.Service(t, "zaentrum-manager")

	w := do(h, http.MethodPost, "/api/library/migrations/neutral-names/names", "", token)
	if w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), `the journal is refused: line 1: its plan holds a rename of \"`+film+`/sources/x/a.srt\" to \"people/a.srt\"`) {
		t.Errorf("a journal naming a file outside its item: %d %s, want 422 saying so", w.Code, w.Body.String())
	}
	w = do(h, http.MethodPost, "/api/library/migrations/empty/names", `{"items": []}`, token)
	if got, want := strings.TrimSpace(w.Body.String()), `{"run":"empty","items":0,"rows":0,"skipped":[]}`; w.Code != http.StatusOK || got != want {
		t.Errorf("a run with nothing journaled: %d %s, want 200 %s", w.Code, got, want)
	}
	if w = do(h, http.MethodPost, "/api/library/migrations/gone/names", "", token); w.Code != http.StatusNotFound {
		t.Errorf("a run that is not there: %d %s, want 404", w.Code, w.Body.String())
	}
	if w = do(h, http.MethodPost, "/api/library/migrations/a..b/names", "", token); w.Code != http.StatusBadRequest {
		t.Errorf("no run's name: %d %s, want 400", w.Code, w.Body.String())
	}
}
