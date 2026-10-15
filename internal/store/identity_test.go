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
