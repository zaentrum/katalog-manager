package tmdb

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/chaptersdb"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The chapters database's chapters, written in place of a title's, are of no
// original (migration 055): the read that had settled them of its original
// no longer says so, and the title misses its original's chapters until a
// read of it agrees.
func TestTheChaptersDatabasesChaptersAreOfNoOriginal(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	db := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/shows/search":
			fmt.Fprint(w, `[{"id": 7, "title": "First Film", "type": "movie", "year": 2020}]`)
		case "/api/chapters/by-show/7":
			fmt.Fprint(w, `[{"entries": [{"time": "00:00:00.000", "name": "Start"}, {"time": "00:10:00.000", "name": "Middle"}]}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(db.Close)
	s := newTestService(t, st, f, "en-US")
	s.ch = chaptersdb.New(config.Config{ChaptersDBEnabled: true, ChaptersDBBaseURL: db.URL})
	addTitle(t, st, film1, "movie", "First Film", 10)
	f.movie(10, "First Film")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemchapters (id, item_id, startms, endms, title, ordinal)
		VALUES ('c1', $1, 0, 5000, 'Read', 1)`, film1)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_chapterreads (item_id, state, readfrom, sourceid, rowssourceid)
		VALUES ($1, 'read', 'header', 's1', 's1')`, film1)

	enrich(t, s, film1)

	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemchapters WHERE item_id = $1
		AND title IN ('Start', 'Middle')`, film1); n != 2 {
		t.Fatalf("%d of the chapters database's chapters written, want 2", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_chapterreads WHERE item_id = $1
		AND rowssourceid IS NULL`, film1); n != 1 {
		t.Error("the read still says the chapters in the catalog were read of its original")
	}
}
