package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOmdbHelpers(t *testing.T) {
	if omdbVal("N/A") != "" || omdbVal("n/a") != "" {
		t.Error("N/A should map to empty")
	}
	if omdbVal("  hello ") != "hello" {
		t.Error("value should be trimmed")
	}
	for in, want := range map[string]int{"2019": 2019, "2019–": 2019, "2019–2021": 2019, "N/A": 0, "": 0} {
		if got := omdbFirstYear(in); got != want {
			t.Errorf("omdbFirstYear(%q) = %d, want %d", in, got, want)
		}
	}
	if got := omdbGenres("Action, Drama ,  Sci-Fi"); !reflect.DeepEqual(got, []string{"Action", "Drama", "Sci-Fi"}) {
		t.Errorf("omdbGenres = %v", got)
	}
	if omdbGenres("N/A") != nil {
		t.Error("N/A genre should be nil")
	}
}

func TestOmdbFetchParsesAndMapsNA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"Title": "Spring", "Year": "2019", "Genre": "Animation, Fantasy",
			"Plot": "A shepherd girl and her dog.", "Poster": "https://img/spring.jpg",
			"imdbRating": "8.1", "imdbID": "tt8443294", "Type": "movie", "Response": "True",
		})
	}))
	defer srv.Close()
	o := &omdbClient{key: func() string { return "k" }, http: srv.Client()}
	// Point the client at the test server by overriding fetch's base via byTitle URL.
	// Simplest: call fetch through a tiny wrapper by temporarily swapping omdbBase is
	// not possible (const); instead exercise the decode path directly.
	res, ok := o.fetchURL(context.Background(), srv.URL)
	if !ok || res == nil {
		t.Fatal("expected a hit")
	}
	if res.Title != "Spring" || res.Year != 2019 || res.Rating != 8.1 {
		t.Errorf("bad result: %+v", res)
	}
	if res.Plot == "" || res.Poster != "https://img/spring.jpg" || res.ImdbID != "tt8443294" {
		t.Errorf("bad result: %+v", res)
	}
	if !reflect.DeepEqual(res.Genres, []string{"Animation", "Fantasy"}) {
		t.Errorf("genres = %v", res.Genres)
	}
}

func TestOmdbNotFoundResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"Response": "False", "Error": "Movie not found!"})
	}))
	defer srv.Close()
	o := &omdbClient{key: func() string { return "k" }, http: srv.Client()}
	if _, ok := o.fetchURL(context.Background(), srv.URL); ok {
		t.Error(`Response:"False" must yield ok=false`)
	}
}

func TestOmdbDisabled(t *testing.T) {
	o := newOMDbClient(func() string { return "  " })
	if o.enabled() {
		t.Error("blank key should be disabled")
	}
}
