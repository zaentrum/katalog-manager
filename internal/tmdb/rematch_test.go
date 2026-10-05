package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	rematchFilm   = "a1a1a1a1-0000-4000-8000-000000000001"
	rematchSeries = "a2a2a2a2-0000-4000-8000-000000000002"
	rematchEp     = "a3a3a3a3-0000-4000-8000-000000000003"
)

// column is one column of the rows a query selects, sorted, joined with " | ".
func column(t *testing.T, st *store.Store, sql string, args ...any) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, " | ")
}

func genresOf(t *testing.T, st *store.Store, id string) string {
	return column(t, st, `SELECT g.name FROM com_nalet_katalog_itemgenres ig
		JOIN com_nalet_katalog_genres g ON g.id = ig.genre_id WHERE ig.item_id = $1`, id)
}

// trailersOf are the item's trailers: "source externalid", local for one with
// a local copy.
func trailersOf(t *testing.T, st *store.Store, id string) string {
	return column(t, st, `SELECT source || ' ' || COALESCE(externalid, '-') ||
		CASE WHEN downloadedat IS NOT NULL OR localpath IS NOT NULL THEN ' local' ELSE '' END
		FROM com_nalet_katalog_itemtrailerlinks WHERE item_id = $1`, id)
}

// artworkOf is the item's artwork: its URL rows ("kind url", the fake's
// address cut) and the image of each kind ("kind=<its first bytes>").
func artworkOf(t *testing.T, st *store.Store, f *fakeTMDB, id string) string {
	urls := column(t, st, `SELECT kind || ' ' || replace(url, $2, '') FROM com_nalet_katalog_itemartwork WHERE item_id = $1`,
		id, f.srv.URL+"/t/p")
	images := column(t, st, `SELECT kind || '=' || convert_from(bytes, 'UTF8') FROM com_nalet_katalog_itemartworkdata
		WHERE item_id = $1`, id)
	return urls + " || " + images
}

// springBreakers is the film the catalog first matched "Spring" with (TMDB
// 100), with its genres, three trailers, a poster and a backdrop; "Spring"
// (200) is the right match, with other genres, no trailer and no backdrop.
func springBreakers(t *testing.T, st *store.Store, f *fakeTMDB, s *Service) {
	t.Helper()
	addTitle(t, st, rematchFilm, "movie", "Spring", 100)
	f.movie(100, "Spring Breakers")
	f.details("movie/100", map[string]any{"genres": []string{"Comedy", "Crime", "Drama"},
		"poster_path": "/sb-poster.jpg", "backdrop_path": "/sb-backdrop.jpg"})
	f.image("/sb-poster.jpg", []byte("SB poster"))
	f.image("/sb-backdrop.jpg", []byte("SB backdrop"))
	f.trailers("movie/100", "k1", "Trailer", "k2", "Red Band Trailer", "k3", "Teaser")
	f.movie(200, "Spring")
	f.details("movie/200", map[string]any{"genres": []string{"Animation", "Fantasy"}, "poster_path": "/spring-poster.jpg"})
	f.image("/spring-poster.jpg", []byte("Spring poster"))
	enrich(t, s, rematchFilm)
	// A local copy of a trailer, and one added by hand.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemtrailerlinks SET downloadedat = now(), localpath = '/media/k1.mp4'
		WHERE item_id = $1 AND externalid = 'k1'`, rematchFilm)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtrailerlinks (id, item_id, source, url, externalid)
		VALUES ('by-hand', $1, 'manual', 'https://example.com/trailer.mp4', 'mine')`, rematchFilm)
	if got, want := genresOf(t, st, rematchFilm), "Comedy | Crime | Drama"; got != want {
		t.Fatalf("genres of the first match: %s", got)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "backdrop /w1280/sb-backdrop.jpg | poster /w780/sb-poster.jpg || "+
		"backdrop=SB backdrop | poster=SB poster"; got != want {
		t.Fatalf("artwork of the first match:\n got  %s\n want %s", got, want)
	}
}

func identify(t *testing.T, s *Service, id string, tmdbID int64) {
	t.Helper()
	if status, msg, err := s.IdentifyOne(context.Background(), id, "", &tmdbID); err != nil || status != statusDone {
		t.Fatalf("IdentifyOne(%s, %d) = %s %q %v, want done", id, tmdbID, status, msg, err)
	}
}

// Identified with another match, a film's genres, trailers and artwork are
// the new match's: the old match's genres go, its trailers go although the
// new match has none, but a trailer with a local copy and one added by hand
// stay; its poster is the new match's, alone, and its backdrop, which the new
// match has none of, goes.
func TestIdentifyReplacesWhatTheOldMatchLeft(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)

	identify(t, s, rematchFilm, 200)
	if got, want := genresOf(t, st, rematchFilm), "Animation | Fantasy"; got != want {
		t.Errorf("genres %q, want the new match's %q", got, want)
	}
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local"; got != want {
		t.Errorf("trailers %q, want %q: the new match has none", got, want)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "poster /w780/spring-poster.jpg || poster=Spring poster"; got != want {
		t.Errorf("artwork:\n got  %s\n want %s", got, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND title = 'Spring'`, rematchFilm); n != 1 {
		t.Error("the film is not titled as its new match")
	}
}

// Identified again with the match it has, a film is cleaned of what an
// earlier match left: an identify is a re-match whatever the match.
func TestIdentifyWithTheSameMatchCleansUp(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)
	// What an identify before this one left: merged, not replaced.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemexternalids SET externalid = '200' WHERE item_id = $1 AND source = 'tmdb'`, rematchFilm)
	f.trailers("movie/200", "s1", "Spring trailer")
	enrich(t, s, rematchFilm)
	if got := genresOf(t, st, rematchFilm); got != "Animation | Comedy | Crime | Drama | Fantasy" {
		t.Fatalf("a refresh adds genres: %s", got)
	}

	identify(t, s, rematchFilm, 200)
	if got, want := genresOf(t, st, rematchFilm), "Animation | Fantasy"; got != want {
		t.Errorf("genres %q, want %q", got, want)
	}
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local | tmdb s1"; got != want {
		t.Errorf("trailers %q, want %q", got, want)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "poster /w780/spring-poster.jpg || poster=Spring poster"; got != want {
		t.Errorf("artwork:\n got  %s\n want %s", got, want)
	}
}

// A refresh keeps what it cannot know better: trailers TMDB could not be
// asked about stay, and genres add to those the title has. Trailers TMDB
// lists replace the TMDB ones without a local copy, also when it lists none;
// one with a local copy is not linked twice.
func TestARefreshReplacesTrailersOnlyWithWhatTMDBLists(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)

	f.failing("/3/movie/100/videos", http.StatusServiceUnavailable)
	enrich(t, s, rematchFilm)
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local | tmdb k2 | tmdb k3"; got != want {
		t.Errorf("TMDB not answering: %q, want %q", got, want)
	}
	f.failing("/3/movie/100/videos", 0)
	f.trailers("movie/100", "k1", "Trailer", "k4", "New trailer")
	enrich(t, s, rematchFilm)
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local | tmdb k4"; got != want {
		t.Errorf("TMDB's list: %q, want %q", got, want)
	}
	f.trailers("movie/100")
	enrich(t, s, rematchFilm)
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local"; got != want {
		t.Errorf("TMDB listing none: %q, want %q", got, want)
	}

	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_genres (id, name) VALUES ('g-fav', 'Favourite')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES ('ig-fav', $1, 'g-fav')`, rematchFilm)
	enrich(t, s, rematchFilm)
	if got, want := genresOf(t, st, rematchFilm), "Comedy | Crime | Drama | Favourite"; got != want {
		t.Errorf("genres after a refresh %q, want %q", got, want)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "backdrop /w1280/sb-backdrop.jpg | poster /w780/sb-poster.jpg || "+
		"backdrop=SB backdrop | poster=SB poster"; got != want {
		t.Errorf("artwork after a refresh:\n got  %s\n want %s", got, want)
	}
}

// Identified while TMDB cannot be asked for the new match's trailers, a film
// still loses the old match's.
func TestIdentifyDropsTheOldTrailersWhenTMDBCannotSayTheNewOnes(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)
	f.failing("/3/movie/200/videos", http.StatusBadGateway)
	identify(t, s, rematchFilm, 200)
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local"; got != want {
		t.Errorf("trailers %q, want %q", got, want)
	}
}

// The image the analyzer extracted from the film's own file is no match's:
// a re-match to a match without an image of its kind keeps it. A match's
// image replaces it, marker and all.
func TestIdentifyKeepsAKeyframe(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, rematchFilm, "movie", "Spring", 100)
	f.movie(100, "Spring Breakers")
	f.details("movie/100", map[string]any{"poster_path": "/sb-poster.jpg"})
	f.image("/sb-poster.jpg", []byte("SB poster"))
	f.movie(200, "Spring")
	f.details("movie/200", map[string]any{"backdrop_path": "/spring-backdrop.jpg"})
	f.image("/spring-backdrop.jpg", []byte("Spring backdrop"))
	enrich(t, s, rematchFilm)
	// the analyzer's keyframe, a poster's URL row before it whose image it
	// replaced, and its marker
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartwork (id, item_id, kind, url)
		VALUES ('kf', $1, 'backdrop', 'extracted:keyframe')`, rematchFilm)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes)
		VALUES ('kf-data', $1, 'backdrop', 'image/jpeg', 'a keyframe')`, rematchFilm)

	identify(t, s,
		rematchFilm, 100)
	if got, want := artworkOf(t, st, f, rematchFilm), "backdrop extracted:keyframe | poster /w780/sb-poster.jpg || "+
		"backdrop=a keyframe | poster=SB poster"; got != want {
		t.Errorf("a match without a backdrop:\n got  %s\n want %s", got, want)
	}
	identify(t, s, rematchFilm, 200)
	if got, want := artworkOf(t, st, f, rematchFilm), "backdrop /w1280/spring-backdrop.jpg || backdrop=Spring backdrop"; got != want {
		t.Errorf("a match with a backdrop:\n got  %s\n want %s", got, want)
	}
}

// An identify that finds no match changes none of what the title has.
func TestIdentifyWithoutAMatchKeepsWhatTheTitleHas(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	springBreakers(t, st, f, s)
	status, _, err := s.IdentifyOne(context.Background(), rematchFilm, "Nothing Like It", nil)
	if err != nil || status != statusNotFound {
		t.Fatalf("IdentifyOne = %s %v, want not_found", status, err)
	}
	if got, want := genresOf(t, st, rematchFilm), "Comedy | Crime | Drama"; got != want {
		t.Errorf("genres %q, want %q", got, want)
	}
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local | tmdb k2 | tmdb k3"; got != want {
		t.Errorf("trailers %q, want %q", got, want)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "backdrop /w1280/sb-backdrop.jpg | poster /w780/sb-poster.jpg || "+
		"backdrop=SB backdrop | poster=SB poster"; got != want {
		t.Errorf("artwork:\n got  %s\n want %s", got, want)
	}
}

// Identified with another match, a series' genres, trailers and artwork are
// the new match's, and so is the artwork of each episode the new match
// knows: an episode without a still of its own loses the old one's, and
// shows its series' artwork.
func TestIdentifyASeriesReplacesItsAndItsEpisodesArtwork(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	addTitle(t, st, rematchSeries, "series", "Pioneers", 300)
	storetest.AddItem(t, st, rematchEp, "episode", "Pilot", rematchSeries)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = $1`, rematchEp)
	f.tv(300, "The Wrong Pioneers")
	f.details("tv/300", map[string]any{"genres": []string{"Reality"}, "poster_path": "/wrong-poster.jpg", "backdrop_path": "/wrong-backdrop.jpg"})
	f.trailers("tv/300", "w1", "Wrong trailer")
	f.episode(300, 1, 1, 3011, "Wrong Pilot", "/wrong-still.jpg")
	for _, p := range []string{"/wrong-poster.jpg", "/wrong-backdrop.jpg", "/wrong-still.jpg", "/right-poster.jpg"} {
		f.image(p, []byte(strings.TrimSuffix(strings.TrimPrefix(p, "/"), ".jpg")))
	}
	f.tv(400, "Pioneer One")
	f.details("tv/400", map[string]any{"genres": []string{"Drama", "Sci-Fi & Fantasy"}, "poster_path": "/right-poster.jpg"})
	f.episode(400, 1, 1, 4011, "Earthfall", "")
	enrich(t, s, rematchSeries)
	if got := artworkOf(t, st, f, rematchEp); got != "backdrop /w500/wrong-still.jpg | poster /w500/wrong-still.jpg || "+
		"backdrop=wrong-still | poster=wrong-still" {
		t.Fatalf("the episode's artwork of the first match: %s", got)
	}

	identify(t, s, rematchSeries, 400)
	if got, want := genresOf(t, st, rematchSeries), "Drama | Sci-Fi & Fantasy"; got != want {
		t.Errorf("genres %q, want %q", got, want)
	}
	if got := trailersOf(t, st, rematchSeries); got != "" {
		t.Errorf("trailers %q, want none", got)
	}
	if got, want := artworkOf(t, st, f, rematchSeries), "poster /w780/right-poster.jpg || poster=right-poster"; got != want {
		t.Errorf("the series' artwork:\n got  %s\n want %s", got, want)
	}
	if got := artworkOf(t, st, f, rematchEp); got != " || " {
		t.Errorf("the episode's artwork: %q, want none of the old match's", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND title = 'Earthfall'`, rematchEp); n != 1 {
		t.Error("the episode is not titled as the new match says")
	}
}

// toServer sends every call to the server at u, whatever host it names.
type toServer struct{ u *url.URL }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = s.u.Scheme, s.u.Host, s.u.Host
	return http.DefaultTransport.RoundTrip(r)
}

// Identified with a title TMDB does not know and OMDb does, a film's genres
// are OMDb's, the old match's TMDB trailers go (OMDb lists none), and its
// poster is OMDb's while the old match's backdrop goes.
func TestIdentifyToAnOMDbMatchReplacesWhatTheOldMatchLeft(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestServiceWith(t, st, f, config.Config{TMDBLanguage: "en-US", OMDBAPIKey: "omdb-test-key"})
	springBreakers(t, st, f, s)
	f.image("/omdb-poster.jpg", []byte("OMDb poster"))
	omdb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("t") != "Spring Shorts" {
			_ = json.NewEncoder(w).Encode(map[string]string{"Response": "False", "Error": "Movie not found!"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Title": "Spring Shorts", "Year": "2019", "Genre": "Animation, Short",
			"Plot": "Shorts.", "Poster": f.srv.URL + "/t/p/original/omdb-poster.jpg", "imdbRating": "7.1",
			"imdbID": "tt0000001", "Type": "movie", "Response": "True"})
	}))
	t.Cleanup(omdb.Close)
	u, _ := url.Parse(omdb.URL)
	s.omdb.http = &http.Client{Transport: toServer{u}}

	status, msg, err := s.IdentifyOne(context.Background(), rematchFilm, "Spring Shorts", nil)
	if err != nil || status != statusDone || msg != "matched via OMDb" {
		t.Fatalf("IdentifyOne = %s %q %v, want an OMDb match", status, msg, err)
	}
	if got, want := genresOf(t, st, rematchFilm), "Animation | Short"; got != want {
		t.Errorf("genres %q, want %q", got, want)
	}
	if got, want := trailersOf(t, st, rematchFilm), "manual mine | tmdb k1 local"; got != want {
		t.Errorf("trailers %q, want %q", got, want)
	}
	if got, want := artworkOf(t, st, f, rematchFilm), "poster /original/omdb-poster.jpg || poster=OMDb poster"; got != want {
		t.Errorf("artwork:\n got  %s\n want %s", got, want)
	}
}
