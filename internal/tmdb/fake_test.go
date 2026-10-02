package tmdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// fakeTMDB stands in for TMDB's API and image CDN in tests: the real service is
// never called. It answers the paths the enrichment uses from what a test put
// in it, and keeps every request it got.
type fakeTMDB struct {
	t   testing.TB
	srv *httptest.Server

	mu       sync.Mutex
	movies   map[int64]map[string]any      // GET /movie/{id}
	tvs      map[int64]map[string]any      // GET /tv/{id}
	credits  map[string][]map[string]any   // "movie/10" or "tv/20": cast entries; a "job" makes crew
	people   map[int64]*fakePerson         // GET /person/{id}
	images   map[string][]byte             // GET /t/p/{size}{path}, by path
	changes  map[string]map[string][]int64 // kind → day (YYYY-MM-DD) → ids changed that day
	fail     map[string]int                // path → status to answer instead
	pageSize int                           // change-list results per page (TMDB: 100)
	maxPage  int                           // the highest page TMDB serves (0: any)
	requests []string                      // path?query of every request, in order
}

// fakePerson is what TMDB knows about a person.
type fakePerson struct {
	Name       string
	Aliases    []string
	Bio        map[string]string // by language (primary subtag)
	Birthday   string
	Deathday   string
	Place      string
	Department string
	Imdb       string
	Profile    string // file path, e.g. "/ada.jpg"
}

func newFakeTMDB(t testing.TB) *fakeTMDB {
	f := &fakeTMDB{
		t:        t,
		movies:   map[int64]map[string]any{},
		tvs:      map[int64]map[string]any{},
		credits:  map[string][]map[string]any{},
		people:   map[int64]*fakePerson{},
		images:   map[string][]byte{},
		changes:  map[string]map[string][]int64{},
		fail:     map[string]int{},
		pageSize: 100,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// newTestService is an enrichment service that talks to f, and to nothing
// else: every client's transport refuses a host that is not this machine.
func newTestService(t testing.TB, st *store.Store, f *fakeTMDB, language string) *Service {
	t.Helper()
	cfg := config.Config{TMDBAPIKey: "test-token", TMDBLanguage: language}
	s := New(st, cfg, processing.New(st.Pool()), nil, nil)
	s.tmdb.apiBase, s.tmdb.imageBase = f.srv.URL+"/3", f.srv.URL+"/t/p"
	s.tmdb.retryUnit = time.Millisecond
	guarded := &http.Client{Transport: loopbackOnly{}, Timeout: 10 * time.Second}
	s.tmdb.http, s.fanart.http, s.omdb.http = guarded, guarded, guarded
	return s
}

// loopbackOnly refuses any request that would leave the machine.
type loopbackOnly struct{}

func (loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	switch r.URL.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return http.DefaultTransport.RoundTrip(r)
	}
	return nil, fmt.Errorf("a test tried to reach %s", r.URL.Host)
}

func (f *fakeTMDB) movie(id int64, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.movies[id] = map[string]any{"id": id, "title": title, "overview": "About " + title, "release_date": "2020-01-01"}
}

func (f *fakeTMDB) tv(id int64, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tvs[id] = map[string]any{"id": id, "name": name, "overview": "About " + name, "first_air_date": "2021-01-01"}
}

// cast sets a title's credits ("movie/10"): each entry is a TMDB id and a name;
// a third string makes it crew with that job.
func (f *fakeTMDB) cast(title string, entries ...[]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range entries {
		id, _ := strconv.ParseInt(e[0], 10, 64)
		m := map[string]any{"id": id, "name": e[1]}
		if len(e) > 2 {
			m["job"] = e[2]
		}
		out = append(out, m)
	}
	f.credits[title] = out
}

func (f *fakeTMDB) person(id int64, p *fakePerson) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.people[id] = p
}

func (f *fakeTMDB) image(path string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.images[path] = b
}

// changed says TMDB's change list for kind names ids on day.
func (f *fakeTMDB) changed(kind, day string, ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.changes[kind] == nil {
		f.changes[kind] = map[string][]int64{}
	}
	f.changes[kind][day] = append(f.changes[kind][day], ids...)
}

// failing makes path answer status (0: answer normally again).
func (f *fakeTMDB) failing(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if status == 0 {
		delete(f.fail, path)
		return
	}
	f.fail[path] = status
}

// calls are the requests whose path starts with prefix.
func (f *fakeTMDB) calls(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeTMDB) forget() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

func (f *fakeTMDB) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	path := r.URL.Path
	if status, ok := f.fail[path]; ok {
		http.Error(w, `{"status_message":"injected"}`, status)
		return
	}
	if strings.HasPrefix(path, "/t/p/") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/t/p/"), "/", 2)
		if b, ok := f.images["/"+parts[len(parts)-1]]; ok {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(b)
			return
		}
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"status_code":7}`, http.StatusUnauthorized)
		return
	}
	seg := strings.Split(strings.Trim(strings.TrimPrefix(path, "/3"), "/"), "/")
	q := r.URL.Query()
	answer := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	id := int64(0)
	if len(seg) > 1 {
		id, _ = strconv.ParseInt(seg[1], 10, 64)
	}
	switch {
	case len(seg) == 2 && seg[1] == "changes":
		f.serveChanges(w, seg[0], q)
	case seg[0] == "search":
		answer(map[string]any{"results": []any{}})
	case len(seg) == 2 && seg[0] == "movie" && f.movies[id] != nil:
		answer(f.movies[id])
	case len(seg) == 2 && seg[0] == "tv" && f.tvs[id] != nil:
		answer(f.tvs[id])
	case len(seg) == 3 && seg[2] == "credits" && f.credits[seg[0]+"/"+seg[1]] != nil:
		var cast, crew []map[string]any
		for _, e := range f.credits[seg[0]+"/"+seg[1]] {
			if e["job"] != nil {
				crew = append(crew, e)
			} else {
				cast = append(cast, e)
			}
		}
		answer(map[string]any{"id": id, "cast": cast, "crew": crew})
	case len(seg) == 3 && (seg[2] == "videos" || seg[2] == "external_ids"):
		answer(map[string]any{"id": id, "results": []any{}})
	case len(seg) == 2 && seg[0] == "person" && f.people[id] != nil:
		answer(f.personJSON(id, q))
	default:
		http.Error(w, `{"status_code":34,"status_message":"The resource you requested could not be found."}`, http.StatusNotFound)
	}
}

// personJSON is GET /person/{id} as TMDB answers it, in the asked language,
// with what append_to_response asks for.
func (f *fakeTMDB) personJSON(id int64, q map[string][]string) map[string]any {
	p := f.people[id]
	lang := ""
	if l := q["language"]; len(l) > 0 {
		lang = strings.SplitN(l[0], "-", 2)[0]
	}
	null := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	aliases := p.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	out := map[string]any{
		"id": id, "name": p.Name, "also_known_as": aliases, "biography": p.Bio[lang],
		"birthday": null(p.Birthday), "deathday": null(p.Deathday), "place_of_birth": null(p.Place),
		"known_for_department": null(p.Department), "imdb_id": null(p.Imdb), "profile_path": null(p.Profile),
		"adult": false, "gender": 0, "popularity": 1.5,
	}
	appended := ""
	if a := q["append_to_response"]; len(a) > 0 {
		appended = a[0]
	}
	if strings.Contains(appended, "external_ids") {
		out["external_ids"] = map[string]any{"imdb_id": null(p.Imdb), "wikidata_id": nil}
	}
	if strings.Contains(appended, "images") {
		profiles := []any{}
		if p.Profile != "" {
			profiles = append(profiles, map[string]any{"file_path": p.Profile, "width": 421, "height": 632})
		}
		out["images"] = map[string]any{"profiles": profiles}
	}
	return out
}

// serveChanges is GET /{kind}/changes: the ids changed between start_date and
// end_date, a page at a time. Like TMDB it refuses a range longer than 14 days.
func (f *fakeTMDB) serveChanges(w http.ResponseWriter, kind string, q map[string][]string) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	start, err1 := time.Parse(time.DateOnly, get("start_date"))
	end, err2 := time.Parse(time.DateOnly, get("end_date"))
	page, err3 := strconv.Atoi(get("page"))
	if err1 != nil || err2 != nil || err3 != nil || page < 1 || end.Before(start) {
		http.Error(w, `{"status_code":22,"status_message":"Invalid parameters"}`, http.StatusBadRequest)
		return
	}
	if end.Sub(start) > 14*24*time.Hour {
		http.Error(w, `{"status_code":47,"status_message":"Invalid date range: Should be a range no longer than 14 days."}`, http.StatusUnprocessableEntity)
		return
	}
	if f.maxPage > 0 && page > f.maxPage {
		http.Error(w, `{"status_code":22,"status_message":"Invalid page: Pages start at 1 and max at 500."}`, http.StatusBadRequest)
		return
	}
	seen := map[int64]bool{}
	var ids []int64
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		for _, id := range f.changes[kind][d.Format(time.DateOnly)] {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	total := (len(ids) + f.pageSize - 1) / f.pageSize
	results := []any{}
	for i := (page - 1) * f.pageSize; i < len(ids) && i < page*f.pageSize; i++ {
		results = append(results, map[string]any{"id": ids[i], "adult": false})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": results, "page": page, "total_pages": total, "total_results": len(ids)})
}
