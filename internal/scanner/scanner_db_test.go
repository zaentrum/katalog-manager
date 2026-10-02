package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A trailer that was once scanned as an item of its own moves to its movie;
// the item it leaves behind without files is removed, and the deletion log
// says the scanner removed it, and why.
func TestAttachTrailerRecordsTheItemItRemoves(t *testing.T) {
	st := storetest.Open(t)
	const movie, orphan = "f1f1f1f1-0000-4000-8000-000000000001", "f2f2f2f2-0000-4000-8000-000000000002"
	root := t.TempDir()
	dir := filepath.Join(root, "Example (2024)")
	feature := filepath.Join(dir, "Example (2024).mkv")
	trailer := filepath.Join(dir, "trailers", "Example (2024)-trailer.mkv")
	for _, f := range []string{feature, trailer} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, movie, "movie", "Example", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-feature', $1, $2, true)`, movie, feature)
	storetest.AddItem(t, st, orphan, "movie", "Example (2024)-trailer", "")
	// Not primary, so the parent lookup (any primary file in the movie's
	// folder) can only find the feature.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-trailer', $1, $2, false)`, orphan, trailer)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('step-orphan', $1, 'scan', 'done')`, orphan)

	s := New(st, config.Config{NFSRoot: root}, processing.New(st.Pool()), nil)
	var res scanResult
	s.attachTrailer(context.Background(), st.Pool(), trailer, trailer, &res)

	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets
		WHERE id = 'asset-trailer' AND item_id = $1 AND kind = 'trailer'`, movie) != 1 {
		t.Fatal("the trailer did not move to its movie")
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, orphan) != 0 {
		t.Fatal("the item left without files was not removed")
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, orphan) != 0 {
		t.Error("the removed item's processing steps were left behind")
	}
	d, ok := storetest.Deleted(t, st, orphan)
	if !ok {
		t.Fatal("the scanner removed an item without a row in the deletion log")
	}
	if d.DeletedBy != deletedByScanner || d.Type != "movie" || d.Title != "Example (2024)-trailer" ||
		d.Reason == nil || !strings.Contains(*d.Reason, movie) {
		t.Fatalf("log row = %+v, reason %v; want the scanner, naming the movie", d, d.Reason)
	}
	if _, ok := storetest.Deleted(t, st, movie); ok {
		t.Fatal("the movie is in the deletion log")
	}
}
