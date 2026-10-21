package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// searchIndexes are 049's indexes.
var searchIndexes = []string{
	"idx_items_search_words", "idx_items_search_prefix", "idx_items_search_trigrams", "idx_items_search_scantitle",
	"idx_items_search_plot", "idx_people_search_words", "idx_people_search_prefix", "idx_people_search_trigrams",
	"idx_people_search_aka", "idx_itempeople_person",
}

func searchReady(t *testing.T, st *store.Store) bool {
	t.Helper()
	ok, err := st.CatalogSearchReady(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// validIndexes counts the indexes of 049 that are there and valid.
func validIndexes(t *testing.T, st *store.Store) int {
	t.Helper()
	return storetest.Count(t, st, `SELECT count(*) FROM unnest($1::text[]) AS n(name)
		JOIN pg_index x ON x.indexrelid = to_regclass(n.name) WHERE x.indisvalid AND x.indisready`, searchIndexes)
}

// 049 applies to a catalog without it, and again, unchanged, to one with it;
// a catalog with it is ready, one without is not. The catalog's writes go on
// meanwhile, and its indexes survive a REINDEX and an ANALYZE, which compute
// them under a search_path of pg_catalog alone from PostgreSQL 17 on.
func TestTheCatalogSearchMigration(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "m1", "movie", "Widow's Bay", "")
	if searchReady(t, st) {
		t.Fatal("a catalog without 049 is ready")
	}
	var said []string
	logf := func(format string, args ...any) {
		said = append(said, format)
		t.Logf(format, args...)
	}
	for i := 0; i < 2; i++ {
		if err := st.EnsureCatalogSearch(ctx, logf); err != nil {
			t.Fatalf("EnsureCatalogSearch, %d. time: %v", i+1, err)
		}
		if !searchReady(t, st) {
			t.Fatalf("not ready after EnsureCatalogSearch %d times", i+1)
		}
		if n := validIndexes(t, st); n != len(searchIndexes) {
			t.Fatalf("%d of the %d indexes are valid", n, len(searchIndexes))
		}
	}
	if len(said) != 1+len(searchIndexes) {
		t.Errorf("said %d things, want the functions and each index once: %q", len(said), said)
	}

	storetest.AddItem(t, st, "m2", "movie", "Léon", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET title = 'Widow´s Bay', description = 'A town.' WHERE id = 'm1'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Zoë Example')`)
	for _, ix := range searchIndexes {
		storetest.Exec(t, st, `REINDEX INDEX `+ix)
	}
	storetest.Exec(t, st, `ANALYZE com_nalet_katalog_items`)
	storetest.Exec(t, st, `ANALYZE com_nalet_katalog_people`)
	if !searchReady(t, st) {
		t.Fatal("not ready after a REINDEX")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items
		WHERE com_nalet_katalog_search_words(title) @@ 'widows:* & bay:*'::tsquery`); n != 1 {
		t.Errorf("the words index finds %d titles of widows bay", n)
	}
}

// A build cut short leaves its index invalid; the next run drops it and
// builds it again.
func TestAnInvalidSearchIndexIsBuiltAgain(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "m1", "movie", "Black Widow", "")
	if err := st.EnsureCatalogSearch(ctx, t.Logf); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `DROP INDEX idx_items_search_words`)

	// A transaction that began before the build holds a snapshot it waits for;
	// the statement timeout cuts it short there.
	cfg := st.Pool().Config().ConnConfig.Copy()
	holder, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ`); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT count(*) FROM com_nalet_katalog_genres`); err != nil {
		t.Fatal(err)
	}
	builder, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close(ctx)
	if _, err := builder.Exec(ctx, `SET statement_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = builder.Exec(ctx, `CREATE INDEX CONCURRENTLY idx_items_search_words ON com_nalet_katalog_items
		USING gin (com_nalet_katalog_search_words(title))`)
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("the build was not cut short: %v", err)
	}
	if _, err := holder.Exec(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_index WHERE indexrelid = to_regclass('idx_items_search_words')
		AND NOT indisvalid`); n != 1 {
		t.Fatal("no invalid index was left")
	}
	if searchReady(t, st) {
		t.Fatal("ready with an invalid index")
	}

	done := make(chan error, 1)
	go func() { done <- st.EnsureCatalogSearch(ctx, t.Logf) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("EnsureCatalogSearch did not finish")
	}
	if !searchReady(t, st) || validIndexes(t, st) != len(searchIndexes) {
		t.Fatal("the invalid index was not built again")
	}
}

// Without 046's scantitle and 030's other names, the indexes of them are left
// out, and the rest is ready.
func TestTheCatalogSearchWithoutEarlierColumns(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if err := st.EnsureCatalogSearch(ctx, t.Logf); err != nil {
		t.Fatal(err)
	}
	if !searchReady(t, st) {
		t.Fatal("not ready")
	}
	if n := validIndexes(t, st); n != len(searchIndexes)-2 {
		t.Errorf("%d indexes, want all but the two of the columns the catalog lacks", n)
	}
}

// The functions normalize as 049 says: case, accents, apostrophes and every
// other mark; the joined text drops the marks inside a word.
func TestTheSearchFunctions(t *testing.T) {
	st := storetest.OpenBase(t)
	if err := st.EnsureCatalogSearch(context.Background(), t.Logf); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ in, text, joined string }{
		{"Widow's Bay", "widows bay", "widows bay"},
		{"Widow´s Bay", "widows bay", "widows bay"},
		{"Widow’s  Bay!", "widows bay", "widows bay"},
		{"widows bay", "widows bay", "widows bay"},
		{"Léon", "leon", "leon"},
		{"Spider-Man: Far From Home", "spider man far from home", "spiderman far from home"},
		{"S.W.A.T.", "s w a t", "swat"},
		{"WALL·E", "wall e", "walle"},
		{"Face/Off", "face off", "faceoff"},
		{"Fast & Furious", "fast furious", "fast furious"},
		{"Ｆｕｌｌ Ｍｅｔａｌ", "full metal", "full metal"},
		{"(500) Days of Summer", "500 days of summer", "500 days of summer"},
		{"Straße", "strasse", "strasse"},
		{"日本の夏", "日本の夏", "日本の夏"},
		{"“Quoted” – Title…", "quoted title", "quoted title"},
		{"  ", "", ""},
	} {
		var text, joined string
		if err := st.Pool().QueryRow(context.Background(), `SELECT com_nalet_katalog_search_text($1),
			com_nalet_katalog_search_joined($1)`, c.in).Scan(&text, &joined); err != nil {
			t.Fatal(err)
		}
		if text != c.text || joined != c.joined {
			t.Errorf("%q: %q and %q, want %q and %q", c.in, text, joined, c.text, c.joined)
		}
	}
}
