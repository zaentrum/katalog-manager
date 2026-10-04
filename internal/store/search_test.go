package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// someTitles gives the catalog seven titles that match "Film" (five movies,
// three of them from 2020 and two of those in Drama, and two series) and two
// that do not, one of them in Drama too.
func someTitles(t *testing.T, st *store.Store) {
	t.Helper()
	for _, it := range []struct {
		id, typ, title string
		year           int
	}{
		{"m1", "movie", "Film One", 2020}, {"m2", "movie", "Film Two", 2020}, {"m3", "movie", "Film Three", 2020},
		{"m4", "movie", "Film Four", 2021}, {"m5", "movie", "Film Five", 2021},
		{"s1", "series", "Film Club", 2020}, {"s2", "series", "The Film Show", 2019},
		{"m6", "movie", "Another Title", 2020}, {"s3", "series", "Series X", 2018},
	} {
		storetest.AddItem(t, st, it.id, it.typ, it.title, "")
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET year = $2 WHERE id = $1`, it.id, it.year)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_genres (id, name) VALUES ('g-drama', 'Drama')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES
		('ig1', 'm1', 'g-drama'), ('ig2', 'm2', 'g-drama'), ('ig3', 'm6', 'g-drama')`)
}

func str(s string) *string { return &s }
func i32(n int32) *int32   { return &n }

// A search answers the page asked for and how many items match in all,
// counted with the same filters: whether the page is full (counted), the last
// one (it ends the matches), past the end (counted) or the first and only one;
// with the query, the type, the year and the genre each narrowing the count as
// they narrow the page.
func TestSearchItemsCountsEveryMatch(t *testing.T) {
	st := storetest.Open(t)
	someTitles(t, st)
	for _, tc := range []struct {
		name        string
		f           store.SearchFilter
		page, total int32
		first       string // the best match, "" for an empty page
	}{
		{"a full page", store.SearchFilter{Q: str("Film"), Limit: 3}, 3, 7, "s1"},
		{"the last page", store.SearchFilter{Q: str("Film"), Limit: 3, Offset: 6}, 1, 7, "s2"},
		{"past the end", store.SearchFilter{Q: str("Film"), Limit: 3, Offset: 9}, 0, 7, ""},
		{"the one page", store.SearchFilter{Q: str("Film")}, 7, 7, "s1"},
		{"another case", store.SearchFilter{Q: str("fILM"), Limit: 2}, 2, 7, "s1"},
		{"movies", store.SearchFilter{Q: str("Film"), Type: str("movie"), Limit: 2}, 2, 5, "m5"},
		{"movies of 2020", store.SearchFilter{Q: str("Film"), Type: str("movie"), Year: i32(2020), Limit: 1}, 1, 3, "m1"},
		{"in Drama", store.SearchFilter{Q: str("Film"), Genre: str("Drama"), Limit: 1}, 1, 2, "m1"},
		{"no match", store.SearchFilter{Q: str("Nothing Like It"), Limit: 1}, 0, 0, ""},
		{"no match, past the end", store.SearchFilter{Q: str("Nothing Like It"), Limit: 1, Offset: 3}, 0, 0, ""},
		{"series, no query", store.SearchFilter{Type: str("series"), Limit: 1}, 1, 3, "s1"},
		{"everything", store.SearchFilter{Limit: 2}, 2, 9, "m6"},
		{"a negative offset", store.SearchFilter{Q: str("Film"), Limit: 3, Offset: -4}, 3, 7, "s1"},
	} {
		hits, total, err := st.SearchItems(context.Background(), tc.f)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		first := ""
		if len(hits) > 0 {
			first = hits[0].ID
		}
		if int32(len(hits)) != tc.page || total != tc.total || first != tc.first {
			t.Errorf("%s: a page of %d (first %q), %d in all; want %d (first %q), %d in all",
				tc.name, len(hits), first, total, tc.page, tc.first, tc.total)
		}
	}
}

// The catalog's counts are of all it holds: its movies, its series, their
// episodes and its people, and nothing else counts as one of them.
func TestCatalogCounts(t *testing.T) {
	st := storetest.Open(t)
	if c, err := st.CatalogCounts(context.Background()); err != nil || c != (store.CatalogCounts{}) {
		t.Fatalf("an empty catalog: %+v, %v", c, err)
	}
	someTitles(t, st)
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "e2", "episode", "Second", "s1")
	storetest.AddItem(t, st, "e3", "episode", "Only", "s2")
	storetest.AddItem(t, st, "e4", "episode", "Fourth", "s1")
	storetest.AddItem(t, st, "a1", "album", "Soundtrack", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada'), ('p2', 'Grace'), ('p3', 'Alan')`)
	c, err := st.CatalogCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := (store.CatalogCounts{Movies: 6, Series: 3, Episodes: 4, People: 3}); c != want {
		t.Errorf("counts %+v, want %+v", c, want)
	}
}
