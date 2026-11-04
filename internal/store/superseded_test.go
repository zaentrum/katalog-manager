package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Migration 056 lets a source be superseded, with the source that took its
// place, since when, and the key a replace was asked with: applied to a
// catalog with 040, and again, unchanged, by EnsureSupersededSources and by
// the file itself, whatever 040's check on the state was named; a state there
// is none of is still refused, and one key gives a title one source. A
// catalog without 040 keeps no sources, and EnsureSupersededSources leaves it
// so.
func TestTheSupersededSourcesMigrationIsIdempotent(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	ready := func() bool {
		t.Helper()
		ok, err := st.SupersededSourcesReady(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if err := st.EnsureSupersededSources(ctx); err != nil || ready() {
		t.Fatalf("EnsureSupersededSources without 040: ready %v, %v", ready(), err)
	}
	for _, ensure := range []func(context.Context) error{st.EnsureDeletionLog, st.EnsurePeople, st.EnsureItemExtras, st.EnsureLibrary} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ready() {
		t.Fatal("a catalog with 040 alone is ready")
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ('early', 'i', 'f.mkv', 1, 'superseded')`); err == nil {
		t.Fatal("040 alone takes a superseded source")
	}
	// A check named otherwise than Postgres names 040's goes too.
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_itemsources RENAME CONSTRAINT com_nalet_katalog_itemsources_state_check
		TO itemsources_state`)
	for i := 0; i < 2; i++ {
		if err := st.EnsureSupersededSources(ctx); err != nil {
			t.Fatalf("EnsureSupersededSources, %d. time: %v", i+1, err)
		}
		storetest.Exec(t, st, migrations.SupersededSources)
		if !ready() {
			t.Fatalf("after EnsureSupersededSources and the file %d times: not ready", i+1)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'com_nalet_katalog_itemsources'::regclass
		AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%state%'`); n != 1 {
		t.Errorf("%d checks on the state, want 1", n)
	}
	for _, state := range []string{"present", "retiring", "superseded", "deleted", "removed"} {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
			VALUES ($1, 'i', 'f.mkv', 1, $1)`, state)
	}
	if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ('x', 'i', 'f.mkv', 1, 'replaced')`); err == nil {
		t.Error("a source is of a state there is none of")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET supersededby = 'present', supersededat = now(),
		replacekey = 'grab-1' WHERE id = 'superseded'`)
	if _, err := st.Pool().Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET replacekey = 'grab-1' WHERE id = 'present'`); err == nil {
		t.Error("one key names two sources of a title")
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, replacekey)
		VALUES ('other', 'j', 'f.mkv', 1, 'grab-1')`)
}
