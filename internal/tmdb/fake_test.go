package tmdb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	movies   map[int64]map[string]any            // GET /movie/{id}
	tvs      map[int64]map[string]any            // GET /tv/{id}
	credits  map[string][]map[string]any         // "movie/10" or "tv/20": cast entries; a "job" makes crew
	every    map[string][]aggPerson              // "tv/20": a series' credits over every season (aggregate_credits)
	allRaw   map[string]map[string]any           // "tv/20": the same, as TMDB answers it (wins over every)
	creators map[string][]map[string]any         // "tv/20": a series' created_by
	bare     map[int64]bool                      // GET /tv/{id} answers no appended credits
	people   map[int64]*fakePerson               // GET /person/{id}
	certs    map[string][]byte                   // "movie/10" or "tv/20": its release_dates or content_ratings, as TMDB answers
	images   map[string][]byte                   // GET /t/p/{size}{path}, by path
	videos   map[string][]map[string]any         // "movie/10" or "tv/20": GET .../videos results (none: an empty list)
	episodes map[string]map[string]any           // "tv/20/1/2": GET /tv/20/season/1/episode/2
	changes  map[string]map[string][]int64       // kind → day (YYYY-MM-DD) → ids changed that day
	fail     map[string]int                      // path → status to answer instead
	held     map[string]chan struct{}            // path → answered once the channel closes
	failIf   func(path string, q url.Values) int // a status to answer a request with instead, or 0
	pageSize int                                 // change-list results per page (TMDB: 100)
	maxPage  int                                 // the highest page TMDB serves (0: any)
	requests []string                            // path?query of every request, in order
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
		every:    map[string][]aggPerson{},
		allRaw:   map[string]map[string]any{},
		creators: map[string][]map[string]any{},
		bare:     map[int64]bool{},
		people:   map[int64]*fakePerson{},
		certs:    map[string][]byte{},
		images:   map[string][]byte{},
		videos:   map[string][]map[string]any{},
		episodes: map[string]map[string]any{},
		changes:  map[string]map[string][]int64{},
		fail:     map[string]int{},
		held:     map[string]chan struct{}{},
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
	return newTestServiceWith(t, st, f, config.Config{TMDBLanguage: language})
}

// newTestServiceWith is newTestService configured with cfg, its TMDB key the
// fake's.
func newTestServiceWith(t testing.TB, st *store.Store, f *fakeTMDB, cfg config.Config) *Service {
	t.Helper()
	cfg.TMDBAPIKey = "test-token"
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

// details sets more of a title's details ("movie/10" or "tv/20"), as TMDB
// names them: genres (names, as TMDB's {"name"} objects), poster_path,
// backdrop_path.
func (f *fakeTMDB) details(title string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind, id, _ := strings.Cut(title, "/")
	n, _ := strconv.ParseInt(id, 10, 64)
	target := f.movies[n]
	if kind == "tv" {
		target = f.tvs[n]
	}
	for k, v := range fields {
		if k == "genres" {
			var gs []any
			for _, g := range v.([]string) {
				gs = append(gs, map[string]any{"id": len(gs) + 1, "name": g})
			}
			v = gs
		}
		target[k] = v
	}
}

// episode sets what TMDB says of a series' episode: its id, name and still
// (a file path, "" for none).
func (f *fakeTMDB) episode(tvID int64, season, number int, id int64, name, still string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := map[string]any{"id": id, "name": name, "overview": "About " + name, "air_date": "2021-02-01"}
	if still != "" {
		e["still_path"] = still
	}
	f.episodes[fmt.Sprintf("tv/%d/%d/%d", tvID, season, number)] = e
}

// trailers sets a title's videos ("movie/10"): each a YouTube trailer with
// its key and name.
func (f *fakeTMDB) trailers(title string, keyNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vs := []map[string]any{}
	for i := 0; i+1 < len(keyNames); i += 2 {
		vs = append(vs, map[string]any{"type": "Trailer", "site": "YouTube", "key": keyNames[i], "name": keyNames[i+1],
			"published_at": "2020-01-0" + strconv.Itoa(1+i/2) + "T10:00:00.000Z"})
	}
	f.videos[title] = vs
}

// cast sets a title's credits ("movie/10"): each entry is a TMDB id and a name,
// billed in the order given; a third string makes it crew with that job, in
// the department a fourth names (by default the one TMDB files the job under).
func (f *fakeTMDB) cast(title string, entries ...[]string) {
	var cast, crew []map[string]any
	for _, e := range entries {
		id, _ := strconv.ParseInt(e[0], 10, 64)
		m := map[string]any{"id": id, "name": e[1]}
		switch {
		case len(e) > 3:
			m["job"], m["department"] = e[2], e[3]
		case len(e) > 2:
			m["job"], m["department"] = e[2], departmentOf(e[2])
		default:
			m["order"], m["character"] = len(cast), ""
		}
		if m["job"] != nil {
			crew = append(crew, m)
		} else {
			cast = append(cast, m)
		}
	}
	f.titleCredits(title, cast, crew)
}

// titleCredits sets a title's credits ("movie/10") as TMDB answers them: its
// cast and crew entries.
func (f *fakeTMDB) titleCredits(title string, cast, crew []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.credits[title] = append(append([]map[string]any{}, cast...), crew...)
}

// departmentOf is the department TMDB files a crew job under, for the jobs
// these tests use.
func departmentOf(job string) string {
	switch job {
	case "Director", "Co-Director", "Series Director", "Assistant Director", "Second Unit Director":
		return "Directing"
	case "Writer", "Screenplay", "Story", "Co-Writer", "Teleplay", "Novel":
		return "Writing"
	case "Director of Photography", "Camera Operator":
		return "Camera"
	case "Original Music Composer", "Music", "Composer", "Music Supervisor":
		return "Sound"
	case "Editor", "Colorist":
		return "Editing"
	case "Art Direction", "Production Design":
		return "Art"
	}
	if strings.Contains(job, "Producer") {
		return "Production"
	}
	return "Crew"
}

// aggPerson is someone in a series' credits over every season: the episodes
// they are in, in all, and their billing order; a job makes them crew.
type aggPerson struct {
	id       int64
	name     string
	episodes int
	order    int
	job      string
}

// aggregate sets a series' credits over every season ("tv/20"). Without it, a
// series' aggregate credits are its plain credits (cast), one episode each.
func (f *fakeTMDB) aggregate(title string, people ...aggPerson) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.every[title] = people
}

// aggregateAsTMDB sets a series' credits over every season ("tv/20") as TMDB
// answers them: its cast entries (with their roles) and crew entries (one per
// person and department, with their jobs).
func (f *fakeTMDB) aggregateAsTMDB(title string, cast, crew []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allRaw[title] = map[string]any{"cast": cast, "crew": crew}
}

// createdBy sets a series' creators ("tv/20"), in TMDB's order: each entry is
// a TMDB id and a name.
func (f *fakeTMDB) createdBy(title string, people ...[]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, p := range people {
		id, _ := strconv.ParseInt(p[0], 10, 64)
		out = append(out, map[string]any{"id": id, "name": p[1], "credit_id": "c" + p[0], "gender": 0})
	}
	f.creators[title] = out
}

// withoutCredits makes GET /tv/{id} answer the series' details alone, whatever
// append_to_response asks for.
func (f *fakeTMDB) withoutCredits(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bare[id] = true
}

// tvJSON is GET /tv/{id} as TMDB answers it, with what append_to_response
// asks for: aggregate_credits, empty when the fake holds none for the series,
// as TMDB answers for a series nobody is credited in.
func (f *fakeTMDB) tvJSON(id int64, q url.Values) map[string]any {
	key := "tv/" + strconv.FormatInt(id, 10)
	out := map[string]any{"created_by": []any{}}
	for k, v := range f.tvs[id] {
		out[k] = v
	}
	if f.creators[key] != nil {
		out["created_by"] = f.creators[key]
	}
	if strings.Contains(q.Get("append_to_response"), "aggregate_credits") && !f.bare[id] {
		every, ok := f.aggregateJSON(key)
		if !ok {
			every = map[string]any{"cast": []any{}, "crew": []any{}}
		}
		out["aggregate_credits"] = every
	}
	return out
}

// hasAggregate reports whether the fake holds a series' aggregate credits.
// Like aggregateJSON it is called with f.mu held.
func (f *fakeTMDB) hasAggregate(title string) bool {
	return f.allRaw[title] != nil || f.every[title] != nil || f.credits[title] != nil
}

// aggregateJSON is a series' aggregate credits as TMDB answers them; ok is
// false when the fake holds none for it.
func (f *fakeTMDB) aggregateJSON(title string) (map[string]any, bool) {
	if raw, ok := f.allRaw[title]; ok {
		return raw, true
	}
	every := f.every[title]
	if every == nil {
		if f.credits[title] == nil {
			return nil, false
		}
		for i, e := range f.credits[title] {
			job, _ := e["job"].(string)
			every = append(every, aggPerson{e["id"].(int64), e["name"].(string), 1, i, job})
		}
	}
	cast, crew := []any{}, []any{}
	for _, p := range every {
		if p.job == "" {
			cast = append(cast, map[string]any{"id": p.id, "name": p.name, "order": p.order,
				"total_episode_count": p.episodes, "known_for_department": "Acting",
				"roles": []any{map[string]any{"character": "Someone", "episode_count": p.episodes}}})
		} else {
			crew = append(crew, map[string]any{"id": p.id, "name": p.name, "department": departmentOf(p.job),
				"total_episode_count": p.episodes,
				"jobs":                []any{map[string]any{"job": p.job, "episode_count": p.episodes}}})
		}
	}
	return map[string]any{"cast": cast, "crew": crew}, true
}

// certify makes TMDB answer a title's certifications with body: a film's
// ("movie/10") at GET /movie/10/release_dates, a series' ("tv/20") at GET
// /tv/20/content_ratings. Without it TMDB answers 404 there.
func (f *fakeTMDB) certify(title string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.certs[title] = body
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

// hold keeps requests to path waiting; arrived receives once one is waiting,
// and release lets them all through.
func (f *fakeTMDB) hold(path string) (arrived <-chan struct{}, release func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate, here := make(chan struct{}), make(chan struct{}, 1)
	f.held[path] = gate
	f.held[path+"#arrived"] = here
	var once sync.Once
	return here, func() { once.Do(func() { close(gate) }) }
}

// failWhen makes requests answer what fail returns for them (0: answer).
func (f *fakeTMDB) failWhen(fail func(path string, q url.Values) int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failIf = fail
}

// changeReads lists how kind's change list was read: "start..end pN", in order.
func (f *fakeTMDB) changeReads(kind string) []string {
	var out []string
	for _, r := range f.calls("/3/" + kind + "/changes?") {
		q, _ := url.ParseQuery(r[strings.Index(r, "?")+1:])
		out = append(out, q.Get("start_date")+".."+q.Get("end_date")+" p"+q.Get("page"))
	}
	return out
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
	gate, here := f.held[r.URL.Path], f.held[r.URL.Path+"#arrived"]
	f.mu.Unlock()
	if gate != nil {
		select {
		case here <- struct{}{}:
		default:
		}
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	path := r.URL.Path
	if status, ok := f.fail[path]; ok {
		http.Error(w, `{"status_message":"injected"}`, status)
		return
	}
	if f.failIf != nil {
		if status := f.failIf(path, r.URL.Query()); status != 0 {
			http.Error(w, `{"status_message":"injected"}`, status)
			return
		}
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
		http.Error(w, `{"status_code":7,"status_message":"Invalid API key: You must be granted a valid key.","success":false}`,
			http.StatusUnauthorized)
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
	case len(seg) == 1 && seg[0] == "authentication":
		answer(map[string]any{"success": true, "status_code": 1, "status_message": "Success."})
	case len(seg) == 2 && seg[1] == "changes":
		f.serveChanges(w, seg[0], q)
	case seg[0] == "search":
		answer(map[string]any{"results": []any{}})
	case len(seg) == 2 && seg[0] == "movie" && f.movies[id] != nil:
		answer(f.movies[id])
	case len(seg) == 2 && seg[0] == "tv" && f.tvs[id] != nil:
		answer(f.tvJSON(id, q))
	case len(seg) == 6 && seg[0] == "tv" && seg[2] == "season" && seg[4] == "episode" &&
		f.episodes["tv/"+seg[1]+"/"+seg[3]+"/"+seg[5]] != nil:
		answer(f.episodes["tv/"+seg[1]+"/"+seg[3]+"/"+seg[5]])
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
	case len(seg) == 3 && seg[2] == "aggregate_credits" && f.hasAggregate(seg[0]+"/"+seg[1]):
		every, _ := f.aggregateJSON(seg[0] + "/" + seg[1])
		answer(map[string]any{"id": id, "cast": every["cast"], "crew": every["crew"]})
	case len(seg) == 3 && seg[2] == "videos" && f.videos[seg[0]+"/"+seg[1]] != nil:
		answer(map[string]any{"id": id, "results": f.videos[seg[0]+"/"+seg[1]]})
	case len(seg) == 3 && (seg[2] == "videos" || seg[2] == "external_ids"):
		answer(map[string]any{"id": id, "results": []any{}})
	case len(seg) == 3 && (seg[0] == "movie" && seg[2] == "release_dates" || seg[0] == "tv" && seg[2] == "content_ratings") &&
		f.certs[seg[0]+"/"+seg[1]] != nil:
		w.Header().Set("Content-Type", "application/json")
		w.Write(f.certs[seg[0]+"/"+seg[1]])
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
