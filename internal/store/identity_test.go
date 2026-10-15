package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An item's identity as the catalog holds it: its own ids, each source in
// lower case to the id of its row of the lowest id; of an episode its
// numbers and its series, its parent or, under a season, its parent's
// parent, with the series' ids. A series names no series; an item there is
// not has none. Its event names an episode's series and numbers, and of
// any other item its ids alone.
func TestIdentitiesOf(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "s1", "series", "Show", "")
	storetest.AddItem(t, st, "season1", "season", "Season 1", "s1")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "season1")
	storetest.AddItem(t, st, "e2", "episode", "Orphan", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2 WHERE id = 'e1'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', 's1', 'tmdb', '97546'), ('x2', 's1', 'TVDB', ' 371572 '), ('x3', 's1', 'tvdb', '999'),
		('x4', 'e1', 'tmdb-episode', '937631'), ('x5', 'e1', 'imdb', '')`)
	got, err := store.IdentitiesOf(ctx, st.Pool(), []string{"s1", "e1", "e2", "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if s := got["s1"]; s.Type != "series" || fmt.Sprint(s.ExternalIDs) != "map[tmdb:97546 tvdb:371572]" || s.SeriesID != "" {
		t.Errorf("the series: %+v", s)
	}
	e := got["e1"]
	if e.Type != "episode" || e.SeriesID != "s1" || fmt.Sprint(e.ExternalIDs) != "map[tmdb-episode:937631]" ||
		fmt.Sprint(e.SeriesExternalIDs) != "map[tmdb:97546 tvdb:371572]" || *e.SeasonNumber != 1 || *e.EpisodeNumber != 2 {
		t.Errorf("the episode under a season: %+v", e)
	}
	if o := got["e2"]; o.Type != "episode" || o.SeriesID != "" || o.ExternalIDs != nil {
		t.Errorf("an episode of no series: %+v", o)
	}
	if _, ok := got["gone"]; ok {
		t.Error("an item there is not has an identity")
	}
	if ev := e.Event(); fmt.Sprint(ev.ExternalIDs, ev.SeriesExternalIDs, *ev.SeasonNumber, *ev.EpisodeNumber) !=
		"map[tmdb-episode:937631] map[tmdb:97546 tvdb:371572] 1 2" {
		t.Errorf("the episode's event: %+v", ev)
	}
	if ev := got["s1"].Event(); ev.SeriesExternalIDs != nil || ev.SeasonNumber != nil || ev.EpisodeNumber != nil {
		t.Errorf("the series' event names a series or numbers: %+v", ev)
	}
}

// A title is found by its identity: by the first of tmdb, tvdb and imdb that
// finds any of its type, the one made first first (of a title held twice,
// the oldest), never by an id of another source or a title of another type.
func TestItemsByIdentity(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, it := range []struct{ id, typ, created string }{
		{"new", "series", "2026-10-02"}, {"old", "series", "2026-10-01"}, {"tvdb-only", "series", "2001-01-01"},
		{"movie", "movie", "2001-01-01"}} {
		storetest.AddItem(t, st, it.id, it.typ, it.id, "")
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET createdat = $2 WHERE id = $1`, it.id, it.created)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', 'new', 'tmdb', '1'), ('x2', 'old', 'tmdb', '1'), ('x3', 'tvdb-only', 'tvdb', '2'), ('x4', 'movie', 'tmdb', '1')`)
	for _, c := range []struct {
		ids  map[string]string
		want string
	}{
		{map[string]string{"tmdb": "1"}, "[old new]"},
		{map[string]string{"tmdb": "1", "tvdb": "2"}, "[old new]"},
		{map[string]string{"tmdb": "3", "tvdb": "2"}, "[tvdb-only]"},
		{map[string]string{"imdb": "tt1"}, "[]"},
		{map[string]string{"tvdb": "1"}, "[]"},
	} {
		got, err := store.ItemsByIdentity(ctx, st.Pool(), "series", c.ids)
		if err != nil || fmt.Sprint(got) != c.want {
			t.Errorf("%v: %v %v, want %s", c.ids, got, err, c.want)
		}
	}
	if got, _ := store.ItemsByIdentity(ctx, st.Pool(), "movie", map[string]string{"tmdb": "1"}); fmt.Sprint(got) != "[movie]" {
		t.Errorf("the movie of tmdb 1: %v", got)
	}
}

// A series is made as the scanner makes one: its title, the title in lower
// case as its sort title and the name it is found by, its year and its
// maker; without migration 046 no name it is found by. Its ids are written a
// row a source, never one it holds already.
func TestMakeSeriesAndItsIDs(t *testing.T) {
	ctx := context.Background()
	for _, scanTitle := range []bool{true, false} {
		st := storetest.Open(t)
		if !scanTitle {
			storetest.Exec(t, st, `DROP INDEX idx_items_series_scantitle`)
			storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_items DROP COLUMN scantitle`)
		}
		year, by := int32(2020), "katalog-manager/scanner"
		id, err := store.MakeSeries(ctx, st.Pool(), "Example Show", &year, &by)
		if err != nil || len(id) != 36 {
			t.Fatalf("046 %v: made %q, %v", scanTitle, id, err)
		}
		where := `type = 'series' AND title = 'Example Show' AND sorttitle = 'example show' AND year = 2020
			AND createdby = 'katalog-manager/scanner' AND createdat IS NOT NULL`
		if scanTitle {
			where += ` AND scantitle = 'example show'`
		}
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND `+where, id); n != 1 {
			t.Errorf("046 %v: the series is not made as the scanner makes one", scanTitle)
		}
		if err := store.AddExternalIDs(ctx, st.Pool(), id, map[string]string{"tvdb": "371572", "tmdb": "97546", "imdb": ""}); err != nil {
			t.Fatal(err)
		}
		if err := store.AddExternalIDs(ctx, st.Pool(), id, map[string]string{"tmdb": "1"}); err != nil {
			t.Fatal(err)
		}
		if got, err := store.IdentitiesOf(ctx, st.Pool(), []string{id}); err != nil ||
			fmt.Sprint(got[id].ExternalIDs) != "map[tmdb:97546 tvdb:371572]" {
			t.Errorf("046 %v: its ids %v, %v", scanTitle, got[id].ExternalIDs, err)
		}
	}
}
