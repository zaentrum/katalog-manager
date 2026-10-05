package tmdb

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// extrasRows are the extras the catalog holds, every column of each, one
// per line.
func extrasRows(t *testing.T, st *store.Store) string {
	t.Helper()
	return column(t, st, `SELECT x::text FROM com_nalet_katalog_itemextras x`)
}

// someExtras gives the title id extras as the operator's API, the scanner
// and a removal leave them: one packaged, one waiting, one removed.
func someExtras(t *testing.T, st *store.Store, id string) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath,
		state, packagepath, packagedat, removedat) VALUES
		('x-ready-'||$2, $1, 'trailer', 'Trailer', 'api', '/extras/'||$2||'/trailer.mov', 'ready', '/p/extras/x', now(), NULL),
		('x-pending-'||$2, $1, 'featurette', 'The Score', 'scanner', '/media/'||$2||'-featurette.mkv', 'pending', NULL, NULL, NULL),
		('x-removed-'||$2, $1, 'teaser', 'Teaser', 'api', '/extras/'||$2||'/teaser.mov', 'ready', NULL, now(), now())`, id, id[:2])
}

// An identify is a re-match of a title's genres, trailers and artwork, and
// nothing of its extras: a film's and a series' extras are as they were,
// every column of them, after a re-match, a refresh, the same match again
// and an identify that finds none.
func TestIdentifyLeavesTheExtrasAlone(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)
	someExtras(t, st, rematchFilm)
	addTitle(t, st, rematchSeries, "series", "Pioneers", 300)
	f.tv(300, "The Wrong Pioneers")
	f.details("tv/300", map[string]any{"genres": []string{"Reality"}})
	f.trailers("tv/300", "w1", "Wrong trailer")
	f.tv(400, "Pioneer One")
	f.details("tv/400", map[string]any{"genres": []string{"Drama"}})
	someExtras(t, st, rematchSeries)
	before := extrasRows(t, st)
	if n := strings.Count(before, "|"); n != 5 {
		t.Fatalf("%d extras before, want 6", n+1)
	}

	identify(t, s, rematchFilm, 200)
	if got := trailersOf(t, st, rematchFilm); got != "manual mine | tmdb k1 local" {
		t.Fatalf("the identify did not re-match: trailers %q", got)
	}
	identify(t, s, rematchSeries, 400)
	enrich(t, s, rematchFilm)
	identify(t, s, rematchFilm, 200)
	if status, _, err := s.IdentifyOne(context.Background(), rematchFilm, "Nothing Like It", nil); err != nil || status != statusNotFound {
		t.Fatalf("an identify without a match: %s %v", status, err)
	}
	if after := extrasRows(t, st); after != before {
		t.Errorf("the extras changed:\nbefore %s\nafter  %s", before, after)
	}
}
