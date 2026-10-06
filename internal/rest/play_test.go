package rest

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Playback serves a title's own file: its primary one, else the first of its
// files by path (a row from before kinds counts as one); never a package's
// record, nor a retired original's row, which names its record's folder.
func TestPlaybackServesOnlyATitlesOwnFile(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	h, iss := server(t, st, cfg)
	write := func(rel, content string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	film := write("media/A Film.mkv", "the film")
	record := write("a/movies/f1/f1/versions/v1/package.json", `{"a": "record"}`)
	source := filepath.Join(dir, "a/movies/f1/f1/sources/s1")
	write("a/movies/f1/f1/sources/s1/source.json", "{}")
	storetest.AddItem(t, st, "f1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind) VALUES
		('a-pkg', 'f1', $1, false, 'packaged'), ('a-src', 'f1', $2, true, 'primary')`, record, film)
	play := func() (int, string) {
		w := do(h, http.MethodGet, "/api/play/f1", "", iss.Viewer(t))
		return w.Code, w.Body.String()
	}
	if code, body := play(); code != http.StatusOK || body != "the film" {
		t.Fatalf("the primary file: %d %q", code, body)
	}
	// Retired: the original's row names its record's folder, and only the
	// package's record is a file.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET isprimary = false, kind = 'original', path = $1
		WHERE id = 'a-src'`, source)
	if code, body := play(); code != http.StatusNotFound {
		t.Errorf("a retired original and a package: %d %q, want 404", code, body)
	}
	// A row from before kinds, no primary: served by path.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET kind = NULL, path = $1 WHERE id = 'a-src'`, film)
	if code, body := play(); code != http.StatusOK || body != "the film" {
		t.Errorf("a file without a kind: %d %q", code, body)
	}
}
