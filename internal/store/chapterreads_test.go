package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// 055 keeps the last read of each title's chapters, and the view of the
// titles whose original is still needed for them; the view reads 040's
// originals and 051's records, so it waits for both. Running it again changes
// nothing, the startup check adds it where any of it is missing, and a read
// holds only a state the reads give.
func TestChapterReadsMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if ready, err := st.ChapterReadsReady(ctx); err != nil || ready {
		t.Fatalf("ChapterReadsReady on a catalog without 055: %v, %v", ready, err)
	}
	if err := st.EnsureChapterReads(ctx); err == nil {
		t.Fatal("055 applied on a catalog without the originals and the records its view reads")
	}
	for _, ensure := range []func(context.Context) error{st.EnsureItemExtras, st.EnsureLibrary, st.EnsureCompliance} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureChapterReads(ctx); err != nil {
			t.Fatalf("EnsureChapterReads, %d. time: %v", run, err)
		}
	}
	if _, err := st.Pool().Exec(ctx, migrations.ChapterReads); err != nil {
		t.Fatalf("applying 055 again: %v", err)
	}
	if ready, err := st.ChapterReadsReady(ctx); err != nil || !ready {
		t.Fatalf("ChapterReadsReady after EnsureChapterReads: %v, %v", ready, err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_chapterreads (item_id, state, found, written) VALUES ('m1', 'none', 2, 0)`)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_chapterreads (item_id, state) VALUES ('m2', 'lost')`); err == nil {
		t.Error("a read took a state no read gives")
	}

	// The view missing brings the migration back; the reads keep their rows.
	storetest.Exec(t, st, `DROP VIEW com_nalet_katalog_chaptersmissing`)
	if ready, err := st.ChapterReadsReady(ctx); err != nil || ready {
		t.Fatalf("ChapterReadsReady with its view missing: %v, %v", ready, err)
	}
	if err := st.EnsureChapterReads(ctx); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_chapterreads WHERE item_id = 'm1'`); n != 1 {
		t.Errorf("the reads after the startup check: %d, want 1", n)
	}
}
