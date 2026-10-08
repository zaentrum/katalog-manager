package library

import (
	"context"
	"slices"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The ids of a series of episodes a file covers, for the tests.
const (
	cvSeries = "c0c0c0c0-0000-4000-8000-000000000000"
	cvE15    = "c0c0c0c0-0000-4000-8000-000000000015"
	cvE16    = "c0c0c0c0-0000-4000-8000-000000000016"
	cvE17    = "c0c0c0c0-0000-4000-8000-000000000017"
	cvOther  = "c0c0c0c0-0000-4000-8000-0000000000ff"
)

// A link has the file of a holder cover an episode: the episode names it,
// its steps of a file do not apply (its scan and its enrichment its own),
// and both are marked changed; linked again nothing changes. The holder's
// covers list it first and the others in episode order; an episode linked
// to another holder marks the one it left too. Unlinked, an episode names no
// holder, its steps still do not apply, saying it has no file, and it and its
// holder are marked changed.
func TestAHoldersCovers(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	pool := st.Pool()
	storetest.AddItem(t, st, cvSeries, "series", "Show", "")
	// S05E17 made first: the covers are in episode order, not in the order made.
	for _, e := range []struct {
		id string
		n  int
	}{{cvE17, 17}, {cvE15, 15}, {cvE16, 16}, {cvOther, 18}} {
		storetest.AddItem(t, st, e.id, "episode", "E", cvSeries)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 5, episodenumber = $2 WHERE id = $1`, e.id, e.n)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, nextretryat, failures)
		VALUES ('s1', $1, 'tmdb', 'done', NULL, 0), ('s2', $1, 'transcode', 'failed', now(), 1), ('s3', $1, 'retire', 'pending', NULL, 0)`, cvE16)
	marked := func() {
		t.Helper()
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	}
	changed := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT id FROM com_nalet_katalog_items WHERE modifiedat > '2001-01-01' ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}

	marked()
	for _, id := range []string{cvE17, cvE16} {
		if linked, err := Link(ctx, pool, cvE15, id); err != nil || !linked {
			t.Fatalf("Link(%s): %v, %v", id, linked, err)
		}
	}
	if got := changed(); !slices.Equal(got, []string{cvE15, cvE16, cvE17}) {
		t.Errorf("marked changed %v, want the holder and both it covers", got)
	}
	covers, err := CoversOf(ctx, pool, cvE15)
	if err != nil || !slices.Equal(covers, []string{cvE15, cvE16, cvE17}) {
		t.Errorf("CoversOf: %v, %v; want the holder, then S05E16, S05E17", covers, err)
	}
	if h, err := HolderOf(ctx, pool, cvE16); err != nil || h != cvE15 {
		t.Errorf("HolderOf(S05E16) = %q, %v", h, err)
	}
	if h, err := HolderOrSelf(ctx, pool, cvE15); err != nil || h != cvE15 {
		t.Errorf("HolderOrSelf(the holder) = %q, %v", h, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2 AND failures = 0 AND nextretryat IS NULL`, cvE16,
		processing.CoveredReason(cvE15)); n != len(processing.CoveredSteps)+1 {
		t.Errorf("%d of S05E16's steps do not apply, saying so; want its steps of a file and its retire", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND step = 'tmdb' AND status = 'done'`, cvE16); n != 1 {
		t.Error("S05E16's enrichment is not its own")
	}
	marked()
	if linked, err := Link(ctx, pool, cvE15, cvE16); err != nil || linked {
		t.Errorf("linked again: %v, %v", linked, err)
	}
	if got := changed(); len(got) != 0 {
		t.Errorf("a link again marked %v changed", got)
	}
	if _, err := Link(ctx, pool, cvE15, cvE15); err == nil {
		t.Error("an episode covers itself")
	}
	// S05E17 goes to another holder: both holders number it otherwise.
	if linked, err := Link(ctx, pool, cvOther, cvE17); err != nil || !linked {
		t.Fatalf("Link to another: %v, %v", linked, err)
	}
	if got := changed(); !slices.Equal(got, []string{cvE15, cvE17, cvOther}) {
		t.Errorf("marked changed %v, want the holder left, the episode and its new holder", got)
	}
	marked()
	gone, err := Unlink(ctx, pool, []string{cvE16, cvE15}, "that covered it was removed")
	if err != nil || !slices.Equal(gone, []string{cvE16}) {
		t.Errorf("Unlink: %v, %v; want S05E16", gone, err)
	}
	if got := changed(); !slices.Equal(got, []string{cvE15, cvE16}) {
		t.Errorf("marked changed %v, want the holder and the one it covers no more", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2`, cvE16, processing.UncoveredReason(cvE15, "that covered it was removed")); n != len(processing.CoveredSteps)+1 {
		t.Errorf("%d of S05E16's steps say it has no file", n)
	}
	if covers, err := CoversOf(ctx, pool, cvE15); err != nil || covers != nil {
		t.Errorf("the holder covers %v, %v; want none", covers, err)
	}
	marked()
	if err := MarkCoveredChanged(ctx, pool, cvOther); err != nil {
		t.Fatal(err)
	}
	if got := changed(); !slices.Equal(got, []string{cvE17}) {
		t.Errorf("the covered of a holder whose version changed: %v", got)
	}
}

// On a catalog without migration 045 no episode covers another: none has a
// holder, a holder covers none, and an unlink does nothing.
func TestCoversWithoutTheMigration(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_items DROP COLUMN coveredby`)
	storetest.AddItem(t, st, cvE15, "episode", "E", "")
	pool := st.Pool()
	if ok, err := CoversReady(ctx, pool); err != nil || ok {
		t.Errorf("CoversReady: %v, %v", ok, err)
	}
	if h, err := HolderOf(ctx, pool, cvE15); err != nil || h != "" {
		t.Errorf("HolderOf: %q, %v", h, err)
	}
	if c, err := CoversOf(ctx, pool, cvE15); err != nil || c != nil {
		t.Errorf("CoversOf: %v, %v", c, err)
	}
	if gone, err := Unlink(ctx, pool, []string{cvE15}, "x"); err != nil || gone != nil {
		t.Errorf("Unlink: %v, %v", gone, err)
	}
	if err := MarkCoveredChanged(ctx, pool, cvE15); err != nil {
		t.Error(err)
	}
}
