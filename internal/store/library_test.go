package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Migrations 040 and 041 apply to a catalog that has neither, and again,
// unchanged, to one that has them: EnsureLibrary twice, and the files
// themselves twice. A catalog with them is ready, one without is not.
func TestTheLibraryMigrationsAreIdempotent(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	for _, ensure := range []func(context.Context) error{st.EnsureDeletionLog, st.EnsurePeople, st.EnsureItemExtras} {
		if err := ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	ready := func() (bool, bool) {
		t.Helper()
		a, err := st.LibraryReady(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, err := st.LibraryProjectionReady(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return a, b
	}
	if a, b := ready(); a || b {
		t.Fatalf("a catalog without 040 and 041 is ready: %v %v", a, b)
	}
	for i := 0; i < 2; i++ {
		if err := st.EnsureLibrary(ctx); err != nil {
			t.Fatalf("EnsureLibrary, %d. time: %v", i+1, err)
		}
		if a, b := ready(); !a || !b {
			t.Fatalf("after EnsureLibrary %d times: ready %v %v", i+1, a, b)
		}
	}
	for i := 0; i < 2; i++ {
		storetest.Exec(t, st, migrations.LibraryV2)
		storetest.Exec(t, st, migrations.LibraryProjection)
		storetest.Exec(t, st, migrations.PlaybackItemIndex)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_index WHERE indrelid = to_regclass('com_nalet_katalog_playbackassets')
		AND indexrelid = to_regclass('idx_playbackassets_item')`); n != 1 {
		t.Error("042 did not index the playback assets by their item")
	}
	if a, b := ready(); !a || !b {
		t.Fatalf("after the files twice: ready %v %v", a, b)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_trigger g JOIN pg_class c ON c.oid = g.tgrelid
		WHERE g.tgname LIKE 'library_%' AND NOT g.tgisinternal AND c.relnamespace = current_schema()::regnamespace`); n != 27 {
		t.Errorf("%d triggers, want 27: three for each of six item tables and the people's images, one for the extras, "+
			"four for the items, one for the people", n)
	}
	// A trigger missing is not ready, and EnsureLibrary puts it back.
	storetest.Exec(t, st, `DROP TRIGGER library_update ON com_nalet_katalog_itemtags`)
	if _, b := ready(); b {
		t.Error("041 without one of its triggers is ready")
	}
	if err := st.EnsureLibrary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, b := ready(); !b {
		t.Error("EnsureLibrary did not put the trigger back")
	}
	// 040 without one of its columns is not ready.
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_itemextras DROP COLUMN sourcedeletedat`)
	if a, _ := ready(); a {
		t.Error("040 without one of its columns is ready")
	}
}

// Without the tables of migrations 030 and 039, 041 gives the tables there
// are their triggers, and is ready.
func TestTheProjectionTriggersWithoutPeopleAndExtras(t *testing.T) {
	st := storetest.OpenBase(t)
	storetest.Exec(t, st, migrations.LibraryProjection)
	if ok, err := st.LibraryProjectionReady(context.Background()); err != nil || !ok {
		t.Fatalf("041 on a catalog older than 030 and 039: ready %v, %v", ok, err)
	}
}

// bumps counts the statements' marks of an item: an AFTER UPDATE trigger
// logs every row whose modifiedat changed.
func bumps(t *testing.T, st *store.Store) func() map[string]int {
	t.Helper()
	storetest.Exec(t, st, `CREATE TABLE bumps (id VARCHAR(36))`)
	storetest.Exec(t, st, `CREATE FUNCTION log_bump() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN INSERT INTO bumps VALUES (NEW.id); RETURN NULL; END $$`)
	storetest.Exec(t, st, `CREATE TRIGGER log_bump AFTER UPDATE ON com_nalet_katalog_items FOR EACH ROW
		WHEN (OLD.modifiedat IS DISTINCT FROM NEW.modifiedat) EXECUTE FUNCTION log_bump()`)
	return func() map[string]int {
		t.Helper()
		rows, err := st.Pool().Query(context.Background(), `SELECT id, count(*) FROM bumps GROUP BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatal(err)
			}
			out[id] = n
		}
		storetest.Exec(t, st, `DELETE FROM bumps`)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = '2000-01-01'`)
		storetest.Exec(t, st, `DELETE FROM bumps`)
		return out
	}
}

func same(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A statement on an item's genres, tags, credits, reference ids, images or
// trailer links marks each item it touched exactly once, however many of its
// rows it touched, whether it inserts, changes or deletes them.
func TestAStatementOnAnItemsRowsMarksItOnce(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "movie", "Movie B", "")
	storetest.AddItem(t, st, movieC, "movie", "Movie C", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
	marks := bumps(t, st)
	marks()
	for _, tc := range []struct{ table, cols, values, set string }{
		{"com_nalet_katalog_itemgenres", "id, item_id, genre_id", "g || n, item, 'g'", "genre_id = 'h'"},
		{"com_nalet_katalog_itemtags", "id, item_id, tag", "'t' || n, item, 'tag' || n", "tag = tag || '!'"},
		{"com_nalet_katalog_itempeople", "id, item_id, person_id, role", "'c' || n, item, 'p1', 'role' || n", "role = role || 'x'"},
		{"com_nalet_katalog_itemexternalids", "id, item_id, source, externalid", "'x' || n, item, 'src' || n, '1'", "externalid = '2'"},
		{"com_nalet_katalog_itemartworkdata", "id, item_id, kind, contenttype", "'a' || n, item, 'poster', 'image/png'", "contenttype = 'image/jpeg'"},
		{"com_nalet_katalog_itemtrailerlinks", "id, item_id, source, url", "'l' || n, item, 'tmdb', 'u' || n", "url = url || '/'"},
	} {
		// Three rows of A and two of B in one statement.
		storetest.Exec(t, st, `INSERT INTO `+tc.table+` (`+tc.cols+`)
			SELECT `+tc.values+` FROM (VALUES ($1::varchar, 1), ($1, 2), ($1, 3), ($2, 4), ($2, 5)) AS v(item, n)
			CROSS JOIN (SELECT 'g') AS gg(g)`, movieA, movieB)
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
		storetest.Exec(t, st, `DELETE FROM `+tc.table)
		if got := marks(); !same(got, map[string]int{movieA: 1, movieC: 1}) {
			t.Errorf("%s: a delete marked %v, want each item once", tc.table, got)
		}
		storetest.Exec(t, st, `UPDATE `+tc.table+` SET item_id = item_id`)
		if got := marks(); len(got) != 0 {
			t.Errorf("%s: an update of no rows marked %v", tc.table, got)
		}
	}
}

// An item is marked when a column its projection shows changes, unless the
// statement says when itself; never when only recordedat, libraryprojectedat
// or another column changes. An episode inserted, deleted or moved to
// another season marks its series too, its parent's series under a season;
// an extra shown otherwise, removed or recorded marks its title; a person's
// images mark them, their name the titles that credit them.
func TestWhatMarksAnItemOrAPerson(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "series", "Series B", "")
	storetest.AddItem(t, st, movieC, "season", "Season 1", movieB)
	marks := bumps(t, st)
	marks()
	modified := func(id string) string {
		t.Helper()
		var s string
		if err := st.Pool().QueryRow(context.Background(), `SELECT to_char(modifiedat, 'YYYY-MM-DD') FROM com_nalet_katalog_items
			WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	for _, set := range []string{"title = 'New'", "sorttitle = 'new'", "year = 1999", "description = 'd'", "rating = 7.5",
		"durationms = 1", "tagline = 't'", "metadatalocked = true", "seasonnumber = 1", "episodenumber = 2"} {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET `+set+` WHERE id = $1`, movieA)
		if got := marks(); !same(got, map[string]int{movieA: 1}) {
			t.Errorf("SET %s marked %v", set, got)
		}
	}
	for _, set := range []string{"recordedat = now()", "libraryprojectedat = localtimestamp", "retirehold = true",
		"createdby = 'x'", "title = title"} {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET `+set+` WHERE id = $1`, movieA)
		if got := marks(); len(got) != 0 {
			t.Errorf("SET %s marked %v", set, got)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET title = 'Newer', modifiedat = '2001-02-03' WHERE id = $1`, movieA)
	if got := modified(movieA); got != "2001-02-03" {
		t.Errorf("a statement that says when: modifiedat %s", got)
	}
	marks()

	// Episodes: under the series, and under its season.
	const e1, e2 = "e1e1e1e1-0000-4000-8000-000000000001", "e2e2e2e2-0000-4000-8000-000000000002"
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, parent_id, seasonnumber, episodenumber)
		VALUES ($1, 'episode', 'One', $2, 1, 1), ($3, 'episode', 'Two', $4, 1, 2)`, e1, movieB, e2, movieC)
	if got := marks(); !same(got, map[string]int{movieB: 1}) {
		t.Errorf("two episodes inserted marked %v, want their series once", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 2 WHERE id = $1`, e2)
	if got := marks(); !same(got, map[string]int{e2: 1, movieB: 1}) {
		t.Errorf("an episode moved to another season marked %v, want it and its series", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET episodenumber = 3 WHERE id = $1`, e2)
	if got := marks(); !same(got, map[string]int{e2: 1}) {
		t.Errorf("an episode renumbered marked %v, want it alone", got)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_items WHERE id IN ($1, $2)`, e1, e2)
	if got := marks(); !same(got, map[string]int{movieB: 1}) {
		t.Errorf("two episodes deleted marked %v, want their series once", got)
	}

	// Extras.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby) VALUES
		('x1', $1, 'trailer', 'Trailer', 'api'), ('x2', $1, 'teaser', 'Teaser', 'api'), ('x3', $2, 'trailer', 'T', 'api')`, movieA, movieB)
	if got := marks(); len(got) != 0 {
		t.Errorf("extras taken in marked %v", got)
	}
	for _, set := range []string{"sortorder = 1", "hidden = true", "label = 'L'", "removedat = now()", "recordedat = now()"} {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET `+set+` WHERE id IN ('x1', 'x2')`)
		if got := marks(); !same(got, map[string]int{movieA: 1}) {
			t.Errorf("extras: SET %s marked %v, want their title once", set, got)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued', error = 'e', heartbeatat = now()`)
	if got := marks(); len(got) != 0 {
		t.Errorf("an extra's packaging marked %v", got)
	}

	// People.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, modifiedat) VALUES ('p1', 'Ada', '2000-01-01')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES ('c1', $1, 'p1', 'actor')`, movieA)
	marks()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, kind, contenttype, bytes, sha256)
		VALUES ('pa1', 'p1', 'profile', 'image/png', '\x89', repeat('a', 64)), ('pa2', 'p1', 'profile', 'image/png', '\x88', repeat('b', 64))`)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people WHERE id = 'p1' AND modifiedat > '2001-01-01'`); n != 1 {
		t.Error("a person's images did not mark them")
	}
	if got := marks(); len(got) != 0 {
		t.Errorf("a person's images marked %v", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET name = 'Ada L.' WHERE id = 'p1'`)
	if got := marks(); !same(got, map[string]int{movieA: 1}) {
		t.Errorf("a person renamed marked %v, want the title that credits them", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET biography = '{}' WHERE id = 'p1'`)
	if got := marks(); len(got) != 0 {
		t.Errorf("a person's biography marked %v", got)
	}
}

// An item's originals and versions go with it, in the delete that records it.
func TestDeleteTakesTheItemsOriginalsAndVersions(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes) VALUES ('s1', $1, 'a.mkv', 1)`, movieA)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state) VALUES ('v1', $1, 'complete')`, movieA)
	if _, err := st.DeleteItem(context.Background(), movieA, store.Deletion{By: "subject-1"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{store.SourcesTable, store.VersionsTable} {
		if n := storetest.Count(t, st, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s kept %d rows of the deleted item", table, n)
		}
	}
}

// Migration 043 applies to a catalog without it, and again, unchanged:
// EnsureReencodeQueue twice, and the file twice. A catalog with it is ready,
// one without it or its index of the titles waiting or sent is not; a title
// waits or is sent once, its titles done or failed aside.
func TestTheReencodeQueueMigrationIsIdempotent(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	ready := func() bool {
		t.Helper()
		ok, err := st.ReencodeQueueReady(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if ready() {
		t.Fatal("a catalog without 043 is ready")
	}
	for i := 0; i < 2; i++ {
		if err := st.EnsureReencodeQueue(ctx); err != nil {
			t.Fatalf("EnsureReencodeQueue, %d. time: %v", i+1, err)
		}
		storetest.Exec(t, st, migrations.ReencodeQueue)
		if !ready() {
			t.Fatalf("after EnsureReencodeQueue and the file %d times: not ready", i+1)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state) VALUES
		('q1', 'm1', 'done'), ('q2', 'm1', 'failed'), ('q3', 'm1', 'queued')`)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state) VALUES ('q4', 'm1', 'sent')`); err == nil {
		t.Error("a title waiting is queued twice")
	}
	storetest.Exec(t, st, `DROP INDEX idx_reencodequeue_live`)
	if ready() {
		t.Error("043 without its index of the titles waiting or sent is ready")
	}
}

// A deleted item leaves the re-encode queue.
func TestADeletedItemLeavesTheReencodeQueue(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "movie", "Movie B", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state) VALUES
		('q1', $1, 'done'), ('q2', $1, 'queued'), ('q3', $2, 'queued')`, movieA, movieB)
	if n, err := st.DeleteItems(context.Background(), []string{movieA}, store.Deletion{By: "subject-1", Reason: "a duplicate"}); err != nil || n != 1 {
		t.Fatalf("DeleteItems: %d, %v", n, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = $1`, movieA); n != 0 {
		t.Errorf("the deleted item's titles in the queue: %d, want none", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`); n != 1 {
		t.Errorf("the queue holds %d titles, want the other item's", n)
	}
}
