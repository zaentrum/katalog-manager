package tmdb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// TMDB bases. API uses the v4 bearer token; images come off the CDN.
const (
	apiBase   = "https://api.themoviedb.org/3"
	imageBase = "https://image.tmdb.org/t/p"
)

// client is the minimal TMDB v3 HTTP client (ported from TmdbClient.java). The
// api key is resolved per call via `key` (a settings-backed resolver → the
// `tmdb.api_key` setting overrides the env/build default) so an operator can
// change it at runtime without a restart. A blank key disables the client: every
// call returns a zero result so the caller can treat enrichment as a no-op.
type client struct {
	key      func() string
	language string
	http     *http.Client
}

func newClient(key func() string, language string) *client {
	return &client{
		key:      key,
		language: language,
		// connect timeout 10s folds into the per-call request timeout.
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) apiKey() string { return strings.TrimSpace(c.key()) }

func (c *client) enabled() bool { return c.apiKey() != "" }

// --- detail structs (only the parsed fields) ---

type tmdbMovie struct {
	ID           int64
	Title        string
	Tagline      string
	Overview     string
	ReleaseDate  string
	Runtime      int
	VoteAverage  float64
	PosterPath   string
	BackdropPath string
	ImdbID       string
	Genres       []string
}

type tmdbTv struct {
	ID             int64
	Name           string
	Tagline        string
	Overview       string
	FirstAirDate   string
	EpisodeRunTime int
	VoteAverage    float64
	PosterPath     string
	BackdropPath   string
	Genres         []string
}

type tmdbEpisode struct {
	ID          int64
	Name        string
	Overview    string
	AirDate     string
	Runtime     int
	VoteAverage float64
	StillPath   string
}

type tmdbCredits struct {
	Cast []string // first 12 cast names
	Crew []string // directors only
}

type tmdbVideo struct {
	ExternalID  string // YouTube video id / Vimeo numeric id
	Site        string // "YouTube" | "Vimeo"
	URL         string
	Name        string
	PublishedAt string // ISO-8601
}

// --- search ---
//
// Ranking, not "first hit". TMDB search is ordered by popularity, so blindly
// taking results[0] mis-identifies an obscure title that shares a name with a
// famous film ("Spring" -> Spring Breakers, "Hero" -> a popular Hero). Instead we
// fetch the candidate list query-only (no year filter — that filter can drop the
// real match entirely) and pick the best with pickBest: a candidate must have a
// TITLE relation (exact normalised match, or a substring either way), and the
// release year — when known — disambiguates among those. A pure year coincidence
// with no title relation never matches, so we degrade to not-found rather than
// attach a wrong-but-popular film.

type searchCandidate struct {
	ID    int64
	Title string
	Year  int // 0 when unknown
}

func (c *client) searchMovie(ctx context.Context, title string, year *int) (int64, bool) {
	if !c.enabled() || strings.TrimSpace(title) == "" {
		return 0, false
	}
	base := apiBase + "/search/movie?language=" + url.QueryEscape(c.language) +
		"&query=" + url.QueryEscape(title)
	cands := c.searchCandidates(ctx, base, true)
	if year != nil {
		// Also pull the year-filtered page: a correct-year match that is
		// unpopular (page 2+ of the unfiltered list, never seen) still surfaces
		// here. We rank the merged set ourselves, so the filter is a candidate
		// SOURCE — never the decider — which avoids the old "filter drops the
		// real match -> blind popularity" failure.
		cands = mergeCandidates(cands, c.searchCandidates(ctx, base+"&year="+strconv.Itoa(*year), true))
	}
	return pickBest(cands, title, year)
}

func (c *client) searchTv(ctx context.Context, title string, year *int) (int64, bool) {
	if !c.enabled() || strings.TrimSpace(title) == "" {
		return 0, false
	}
	base := apiBase + "/search/tv?language=" + url.QueryEscape(c.language) +
		"&query=" + url.QueryEscape(title)
	cands := c.searchCandidates(ctx, base, false)
	if year != nil {
		cands = mergeCandidates(cands, c.searchCandidates(ctx, base+"&first_air_date_year="+strconv.Itoa(*year), false))
	}
	return pickBest(cands, title, year)
}

// mergeCandidates appends b's candidates that a doesn't already contain (by id),
// preserving a's (popularity) order first. Used to combine the plain-query and
// year-filtered result pages.
func mergeCandidates(a, b []searchCandidate) []searchCandidate {
	if len(b) == 0 {
		return a
	}
	seen := make(map[int64]bool, len(a))
	for _, c := range a {
		seen[c.ID] = true
	}
	for _, c := range b {
		if !seen[c.ID] {
			seen[c.ID] = true
			a = append(a, c)
		}
	}
	return a
}

// searchCandidates fetches a TMDB search page and extracts id/title/year in the
// server's (popularity) order. movie selects the title/release_date fields; TV
// uses name/first_air_date.
func (c *client) searchCandidates(ctx context.Context, u string, movie bool) []searchCandidate {
	body, ok := c.getJSON(ctx, u)
	if !ok {
		return nil
	}
	var resp struct {
		Results []struct {
			ID           int64  `json:"id"`
			Title        string `json:"title"`
			Name         string `json:"name"`
			ReleaseDate  string `json:"release_date"`
			FirstAirDate string `json:"first_air_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	out := make([]searchCandidate, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.ID <= 0 {
			continue
		}
		title, date := r.Title, r.ReleaseDate
		if !movie {
			title, date = r.Name, r.FirstAirDate
		}
		out = append(out, searchCandidate{ID: r.ID, Title: title, Year: yearOf(date)})
	}
	return out
}

// pickBest scores candidates and returns the highest — requiring a title
// relation so a popularity-driven wrong match can't win.
//
// A candidate qualifies only via:
//   - EXACT normalised title (score 1000) — a strong signal, accepted on its own; or
//   - SUBSTRING either way (score 300) but ONLY when the shorter side is ≥4 chars
//     AND the year corroborates within ±1. A bare substring (short token like
//     "up"/"her", or a loose match with no/mismatched year) is NOT enough — it
//     would let a popular namesake win, the exact bug this guards against.
//
// Year proximity then adds (exact 500 / ±1 300 / ±2 150), with the original
// popularity order as a sub-tiebreak. No qualifier ⇒ not-found (a wrong match is
// worse than none; unmatched items stay playable and can be pinned via identify).
func pickBest(cands []searchCandidate, wantTitle string, wantYear *int) (int64, bool) {
	nw := normTitle(wantTitle)
	if nw == "" || len(cands) == 0 {
		return 0, false
	}
	var bestID int64
	bestScore := -1.0
	for i, c := range cands {
		nt := normTitle(c.Title)
		if nt == "" {
			continue
		}
		yearDiff := -1 // -1 => year unknown on one side
		if wantYear != nil && c.Year != 0 {
			yearDiff = absInt(c.Year - *wantYear)
		}
		title := 0
		switch {
		case nt == nw:
			title = 1000
		case minLen(nt, nw) >= 4 && (strings.Contains(nt, nw) || strings.Contains(nw, nt)) &&
			yearDiff >= 0 && yearDiff <= 1:
			title = 300
		default:
			continue // no accepted title relation — never match on popularity alone
		}
		year := 0
		switch yearDiff {
		case 0:
			year = 500
		case 1:
			year = 300
		case 2:
			year = 150
		}
		// Popularity sub-tiebreak: earlier results rank higher, but only enough to
		// break exact score ties (never to override a better title/year match).
		score := float64(title+year) + float64(len(cands)-i)*0.001
		if score > bestScore {
			bestScore, bestID = score, c.ID
		}
	}
	if bestID > 0 {
		return bestID, true
	}
	return 0, false
}

func minLen(a, b string) int {
	if len(a) < len(b) {
		return len(a)
	}
	return len(b)
}

// normTitle lowercases and keeps only letters/digits (Unicode-aware, so non-Latin
// titles survive), dropping punctuation and spaces — "Caminandes: Llama Drama" and
// "Caminandes Llama Drama" compare equal, and a Japanese/Cyrillic title still
// normalises to itself rather than to an empty string.
func normTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// yearOf parses the leading YYYY of a TMDB date ("2019-04-04" -> 2019); 0 if absent.
func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, err := strconv.Atoi(date[:4])
	if err != nil {
		return 0
	}
	return y
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// --- detail fetches ---

func (c *client) getMovie(ctx context.Context, id int64) (*tmdbMovie, bool) {
	if !c.enabled() {
		return nil, false
	}
	body, ok := c.getJSON(ctx, apiBase+"/movie/"+strconv.FormatInt(id, 10)+"?language="+url.QueryEscape(c.language))
	if !ok {
		return nil, false
	}
	var n struct {
		ID           int64   `json:"id"`
		Title        *string `json:"title"`
		Tagline      *string `json:"tagline"`
		Overview     *string `json:"overview"`
		ReleaseDate  *string `json:"release_date"`
		Runtime      int     `json:"runtime"`
		VoteAverage  float64 `json:"vote_average"`
		PosterPath   *string `json:"poster_path"`
		BackdropPath *string `json:"backdrop_path"`
		ImdbID       *string `json:"imdb_id"`
		Genres       []struct {
			Name string `json:"name"`
		} `json:"genres"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, false
	}
	return &tmdbMovie{
		ID:           n.ID,
		Title:        deref(n.Title),
		Tagline:      deref(n.Tagline),
		Overview:     deref(n.Overview),
		ReleaseDate:  deref(n.ReleaseDate),
		Runtime:      n.Runtime,
		VoteAverage:  n.VoteAverage,
		PosterPath:   deref(n.PosterPath),
		BackdropPath: deref(n.BackdropPath),
		ImdbID:       deref(n.ImdbID),
		Genres:       genreNames(n.Genres),
	}, true
}

func (c *client) getTv(ctx context.Context, id int64) (*tmdbTv, bool) {
	if !c.enabled() {
		return nil, false
	}
	body, ok := c.getJSON(ctx, apiBase+"/tv/"+strconv.FormatInt(id, 10)+"?language="+url.QueryEscape(c.language))
	if !ok {
		return nil, false
	}
	var n struct {
		ID             int64   `json:"id"`
		Name           *string `json:"name"`
		Tagline        *string `json:"tagline"`
		Overview       *string `json:"overview"`
		FirstAirDate   *string `json:"first_air_date"`
		EpisodeRunTime []int   `json:"episode_run_time"`
		VoteAverage    float64 `json:"vote_average"`
		PosterPath     *string `json:"poster_path"`
		BackdropPath   *string `json:"backdrop_path"`
		Genres         []struct {
			Name string `json:"name"`
		} `json:"genres"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, false
	}
	runtime := 0
	if len(n.EpisodeRunTime) > 0 {
		runtime = n.EpisodeRunTime[0]
	}
	return &tmdbTv{
		ID:             n.ID,
		Name:           deref(n.Name),
		Tagline:        deref(n.Tagline),
		Overview:       deref(n.Overview),
		FirstAirDate:   deref(n.FirstAirDate),
		EpisodeRunTime: runtime,
		VoteAverage:    n.VoteAverage,
		PosterPath:     deref(n.PosterPath),
		BackdropPath:   deref(n.BackdropPath),
		Genres:         genreNames(n.Genres),
	}, true
}

// getTvExternalIDs returns a series' imdb + TheTVDB ids (the latter keys fanart.tv
// TV artwork). Empty strings when absent/unavailable.
func (c *client) getTvExternalIDs(ctx context.Context, tvID int64) (imdbID, tvdbID string) {
	if !c.enabled() {
		return "", ""
	}
	body, ok := c.getJSON(ctx, apiBase+"/tv/"+strconv.FormatInt(tvID, 10)+"/external_ids?language="+url.QueryEscape(c.language))
	if !ok {
		return "", ""
	}
	var n struct {
		ImdbID *string `json:"imdb_id"`
		TvdbID *int64  `json:"tvdb_id"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return "", ""
	}
	if n.TvdbID != nil && *n.TvdbID > 0 {
		tvdbID = strconv.FormatInt(*n.TvdbID, 10)
	}
	return deref(n.ImdbID), tvdbID
}

func (c *client) getTvEpisode(ctx context.Context, tvID int64, season, episode int) (*tmdbEpisode, bool) {
	if !c.enabled() {
		return nil, false
	}
	u := apiBase + "/tv/" + strconv.FormatInt(tvID, 10) +
		"/season/" + strconv.Itoa(season) + "/episode/" + strconv.Itoa(episode) +
		"?language=" + url.QueryEscape(c.language)
	body, ok := c.getJSON(ctx, u)
	if !ok {
		return nil, false
	}
	var n struct {
		ID          int64   `json:"id"`
		Name        *string `json:"name"`
		Overview    *string `json:"overview"`
		AirDate     *string `json:"air_date"`
		Runtime     int     `json:"runtime"`
		VoteAverage float64 `json:"vote_average"`
		StillPath   *string `json:"still_path"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, false
	}
	return &tmdbEpisode{
		ID:          n.ID,
		Name:        deref(n.Name),
		Overview:    deref(n.Overview),
		AirDate:     deref(n.AirDate),
		Runtime:     n.Runtime,
		VoteAverage: n.VoteAverage,
		StillPath:   deref(n.StillPath),
	}, true
}

// --- credits ---

func (c *client) getCredits(ctx context.Context, id int64) (*tmdbCredits, bool) {
	if !c.enabled() {
		return nil, false
	}
	return c.credits(ctx, apiBase+"/movie/"+strconv.FormatInt(id, 10)+"/credits?language="+url.QueryEscape(c.language))
}

func (c *client) getTvCredits(ctx context.Context, id int64) (*tmdbCredits, bool) {
	if !c.enabled() {
		return nil, false
	}
	return c.credits(ctx, apiBase+"/tv/"+strconv.FormatInt(id, 10)+"/credits?language="+url.QueryEscape(c.language))
}

func (c *client) credits(ctx context.Context, u string) (*tmdbCredits, bool) {
	body, ok := c.getJSON(ctx, u)
	if !ok {
		return nil, false
	}
	var n struct {
		Cast []struct {
			Name string `json:"name"`
		} `json:"cast"`
		Crew []struct {
			Name string `json:"name"`
			Job  string `json:"job"`
		} `json:"crew"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, false
	}
	cr := &tmdbCredits{}
	for i, p := range n.Cast {
		if i >= 12 {
			break
		}
		cr.Cast = append(cr.Cast, p.Name)
	}
	for _, p := range n.Crew {
		if strings.EqualFold(p.Job, "Director") {
			cr.Crew = append(cr.Crew, p.Name)
		}
	}
	return cr, true
}

// --- videos / trailers ---

func (c *client) getMovieVideos(ctx context.Context, id int64) []tmdbVideo {
	return c.getVideos(ctx, "/movie/"+strconv.FormatInt(id, 10)+"/videos")
}

func (c *client) getTvVideos(ctx context.Context, id int64) []tmdbVideo {
	return c.getVideos(ctx, "/tv/"+strconv.FormatInt(id, 10)+"/videos")
}

func (c *client) getVideos(ctx context.Context, pathSuffix string) []tmdbVideo {
	if !c.enabled() {
		return nil
	}
	body, ok := c.getJSON(ctx, apiBase+pathSuffix+"?language="+url.QueryEscape(c.language))
	if !ok {
		return nil
	}
	var n struct {
		Results []struct {
			Type        string `json:"type"`
			Site        string `json:"site"`
			Key         string `json:"key"`
			Name        string `json:"name"`
			PublishedAt string `json:"published_at"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil
	}
	var out []tmdbVideo
	for _, v := range n.Results {
		if !strings.EqualFold(v.Type, "Trailer") && !strings.EqualFold(v.Type, "Teaser") {
			continue
		}
		if !strings.EqualFold(v.Site, "YouTube") && !strings.EqualFold(v.Site, "Vimeo") {
			continue
		}
		if strings.TrimSpace(v.Key) == "" {
			continue
		}
		var canonical string
		if strings.EqualFold(v.Site, "YouTube") {
			canonical = "https://www.youtube.com/watch?v=" + v.Key
		} else {
			canonical = "https://vimeo.com/" + v.Key
		}
		out = append(out, tmdbVideo{
			ExternalID:  v.Key,
			Site:        v.Site,
			URL:         canonical,
			Name:        v.Name,
			PublishedAt: v.PublishedAt,
		})
	}
	return out
}

// --- images ---

// imageURL builds the absolute CDN URL for a TMDB path at the given size.
// Empty path => "".
func (c *client) imageURL(path, size string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	sz := size
	if strings.TrimSpace(sz) == "" {
		sz = "original"
	}
	return imageBase + "/" + sz + path
}

// fetchImage downloads raw image bytes; only HTTP 200 yields bytes.
func (c *client) fetchImage(ctx context.Context, absoluteURL string) ([]byte, bool) {
	if strings.TrimSpace(absoluteURL) == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, absoluteURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, false
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return b, true
}

// getJSON performs an authorized GET; returns the body only on HTTP 200.
// getJSON does an authenticated GET, retrying on TMDB rate-limit (429) with the
// server's Retry-After (capped) then exponential backoff. GET is idempotent so
// retry is safe. Returns (body, true) only on 200. A shared/bundled token hits
// the rate limit sooner, so this pacing keeps the sweep from failing under load.
func (c *client) getJSON(ctx context.Context, rawURL string) ([]byte, bool) {
	const maxAttempts = 4
	for attempt := 0; attempt < maxAttempts; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
		if err != nil {
			cancel()
			return nil, false
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey())
		resp, err := c.http.Do(req)
		if err != nil {
			cancel()
			return nil, false
		}
		if resp.StatusCode == http.StatusOK {
			b, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			if rerr != nil {
				return nil, false
			}
			return b, true
		}
		rateLimited := resp.StatusCode == http.StatusTooManyRequests
		wait := parseRetryAfter(resp.Header.Get("Retry-After"))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		if !rateLimited || attempt == maxAttempts-1 {
			return nil, false
		}
		if wait <= 0 {
			wait = time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
		}
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(wait):
		}
	}
	return nil, false
}

// parseRetryAfter reads a delta-seconds Retry-After header; 0 if absent/invalid.
func parseRetryAfter(h string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func genreNames(gs []struct {
	Name string `json:"name"`
}) []string {
	var out []string
	for _, g := range gs {
		if strings.TrimSpace(g.Name) != "" {
			out = append(out, g.Name)
		}
	}
	return out
}
