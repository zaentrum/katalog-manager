package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Migration 050 applies after 041 and refuses before it, to a catalog
// without it, and again, unchanged: EnsureLibraryWriteThrough twice, and the
// file twice. A catalog with it is ready; one missing a trigger is not, and
// the ensure puts it back.
func TestTheWriteThroughMigrationIsIdempotent(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	for _, ensure := range []func(context.Context) error{st.EnsureDeletionLog, st.EnsurePeople, st.EnsureItemExtras} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnsureLibraryWriteThrough(ctx); err == nil || !strings.Contains(err.Error(), "041") {
		t.Fatalf("before 041: %v", err)
	}
	if err := st.EnsureLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	ready := func() bool {
		t.Helper()
		ok, err := st.LibraryWriteThroughReady(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if ready() {
		t.Fatal("a catalog without 050 is ready")
	}
	for i := 0; i < 2; i++ {
		if err := st.EnsureLibraryWriteThrough(ctx); err != nil {
			t.Fatalf("EnsureLibraryWriteThrough, %d. time: %v", i+1, err)
		}
		if !ready() {
			t.Fatalf("after EnsureLibraryWriteThrough %d times: not ready", i+1)
		}
	}
	for i := 0; i < 2; i++ {
		storetest.Exec(t, st, migrations.LibraryWriteThrough)
	}
	if !ready() {
		t.Fatal("after the file twice: not ready")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		WHERE g.tgname LIKE 'library_%' AND NOT g.tgisinternal AND c.relnamespace = current_schema()::regnamespace`); n != 27+10 {
		t.Errorf("%d triggers, want 041's 27 and 050's 10: three for the ranges and the chapters each, two for the extras, "+
			"one for the genres and one for the people", n)
	}
	storetest.Exec(t, st, `DROP TRIGGER library_delete ON com_nalet_katalog_itemchapters`)
	if ready() {
		t.Error("050 without one of its triggers is ready")
	}
	if err := st.EnsureLibraryWriteThrough(ctx); err != nil {
		t.Fatal(err)
	}
	if !ready() {
		t.Error("EnsureLibraryWriteThrough did not put the trigger back")
	}
}

// Without migration 030's columns of a person, 050 gives the person no
// trigger, and is ready.
func TestTheWriteThroughWithoutAPersonsDetails(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	for _, ensure := range []func(context.Context) error{st.EnsureItemExtras, st.EnsureLibrary, st.EnsureMultiEpisodeFiles,
		st.EnsureLibraryWriteThrough} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := st.LibraryWriteThroughReady(ctx); err != nil || !ok {
		t.Fatalf("050 on a catalog older than 030: ready %v, %v", ok, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_trigger WHERE tgrelid = to_regclass('com_nalet_katalog_people')
		AND tgname = 'library_changed'`); n != 0 {
		t.Error("a person without 030's columns got the trigger that reads them")
	}
}

// A statement on an item's chapters or ranges marks each item it touched
// exactly once, however many of its rows it touched, whether it inserts,
// changes or deletes them — whoever writes it: the analyzer's replacement
// of a title's ranges is a delete and an insert, a series' reset a delete.
func TestAStatementOnAnItemsMarksMarksItOnce(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "movie", "Movie B", "")
	storetest.AddItem(t, st, movieC, "movie", "Movie C", "")
	marks := bumps(t, st)
	marks()
	for _, tc := range []struct{ table, cols, values, set string }{
		{"com_nalet_katalog_mediasegments", "id, item_id, kind, startms, endms, source",
			"'s' || n, item, 'intro', n * 1000, n * 1000 + 500, 'chromaprint'", "label = 'x'"},
		{"com_nalet_katalog_itemchapters", "id, item_id, startms, endms, title, ordinal",
			"'c' || n, item, n * 1000, n * 1000 + 500, 'Chapter', n", "title = ' Chapter'"},
	} {
		storetest.Exec(t, st, `INSERT INTO `+tc.table+` (`+tc.cols+`)
			SELECT `+tc.values+` FROM (VALUES ($1::varchar, 1), ($1, 2), ($1, 3), ($2, 4), ($2, 5)) AS v(item, n)`,
			movieA, movieB)
		if got := marks(); !same(got, map[string]int{movieA: 1, movieB: 1}) {
			t.Errorf("%s: an insert of 3 and 2 rows marked %v, want each item once", tc.table, got)
		}
		storetest.Exec(t, st, `UPDATE `+tc.table+` SET `+tc.set)
		if got := marks(); !same(got, map[string]int{movieA: 1, movieB: 1}) {
			t.Errorf("%s: an update marked %v, want each item once", tc.table, got)
		}
		storetest.Exec(t, st, `UPDATE `+tc.table+` SET item_id = $2 WHERE item_id = $1`, movieB, movieC)
		if got := marks(); !same(got, map[string]int{movieB: 1, movieC: 1}) {
			t.Errorf("%s: rows moved to another item marked %v, want both items once", tc.table, got)
		}
		storetest.Exec(t, st, `DELETE FROM `+tc.table+` WHERE item_id = $1`, movieA)
		if got := marks(); !same(got, map[string]int{movieA: 1}) {
			t.Errorf("%s: a delete marked %v, want the item once", tc.table, got)
		}
		storetest.Exec(t, st, `DELETE FROM `+tc.table)
		if got := marks(); !same(got, map[string]int{movieC: 1}) {
			t.Errorf("%s: the delete of the rest marked %v", tc.table, got)
		}
		storetest.Exec(t, st, `UPDATE `+tc.table+` SET item_id = item_id`)
		if got := marks(); len(got) != 0 {
			t.Errorf("%s: an update of no rows marked %v", tc.table, got)
		}
	}
}

// An extra inserted or deleted recorded and not removed marks its title; a
// genre renamed the items it is a genre of.
func TestWhatElseMarksAnItem(t *testing.T) {
	st := storetest.Open(t)
	const series = "5e5e5e5e-0000-4000-8000-000000000001"
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, series, "series", "Series", "")
	marks := bumps(t, st)
	marks()

	// Extras.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, recordedat, removedat)
		VALUES ('x1', $1, 'trailer', 'Trailer', 'api', now(), NULL), ('x2', $1, 'teaser', 'Teaser', 'api', NULL, NULL),
		       ('x3', $2, 'trailer', 'T', 'api', now(), now())`, movieA, series)
	if got := marks(); !same(got, map[string]int{movieA: 1}) {
		t.Errorf("extras inserted marked %v, want the title of the one recorded and not removed", got)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_itemextras WHERE id IN ('x2', 'x3')`)
	if got := marks(); len(got) != 0 {
		t.Errorf("extras deleted unrecorded or removed marked %v", got)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_itemextras`)
	if got := marks(); !same(got, map[string]int{movieA: 1}) {
		t.Errorf("an extra deleted recorded marked %v, want its title", got)
	}

	// Genres.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_genres (id, name) VALUES ('g1', 'Drama'), ('g2', 'Comedy')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES ('ig1', $1, 'g1'), ('ig2', $2, 'g1')`,
		movieA, series)
	marks()
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_genres SET name = 'Dramas' WHERE id = 'g1'`)
	if got := marks(); !same(got, map[string]int{movieA: 1, series: 1}) {
		t.Errorf("a genre renamed marked %v, want its items", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_genres SET name = name`)
	if got := marks(); len(got) != 0 {
		t.Errorf("genres named as they were marked %v", got)
	}
}

// A person is marked when a column their projection shows changes, unless
// the statement says when itself; when the catalog last asked TMDB about
// them marks nothing, nor does a projection noted.
func TestWhatMarksAPerson(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, modifiedat) VALUES ('p1', 'Ada', '2000-01-01')`)
	marked := func() bool {
		t.Helper()
		n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE id = 'p1' AND modifiedat > '2000-01-02'`)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET modifiedat = '2000-01-01'`)
		return n == 1
	}
	for _, set := range []string{"name = 'Ada L.'", "sortname = 'L., Ada'", `alsoknownas = '["A. L."]'`, "birthdate = '1815-12-10'",
		"deathdate = '1852-11-27'", "birthplace = 'London'", `biography = '{"en": "A mathematician."}'`, "tmdbpersonid = '1'",
		"imdbid = 'nm1'", "knownfordepartment = 'Writing'", "metadatalocked = true", `lockedfields = '["name"]'`,
		`fieldorigins = '{"name": "manual"}'`} {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET `+set+` WHERE id = 'p1'`)
		if !marked() {
			t.Errorf("SET %s did not mark the person", set)
		}
	}
	for _, set := range []string{"tmdbfetchedat = now()", "tmdbchangedat = '2026-10-01'", "libraryprojectedat = now()", "name = name"} {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET `+set+` WHERE id = 'p1'`)
		if marked() {
			t.Errorf("SET %s marked the person", set)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET name = 'Ada King', modifiedat = '2001-02-03' WHERE id = 'p1'`)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE id = 'p1'
		AND modifiedat = '2001-02-03'`); n != 1 {
		t.Error("a statement that says when did not keep its modifiedat")
	}
}
