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
)

// TMDB bases. API uses the v4 bearer token; images come off the CDN.
const (
	apiBase   = "https://api.themoviedb.org/3"
	imageBase = "https://image.tmdb.org/t/p"
)

// client is the minimal TMDB v3 HTTP client (ported from TmdbClient.java).
// A blank apiKey disables the client: every call returns a zero result so the
// caller can treat enrichment as a no-op.
type client struct {
	apiKey   string
	language string
	http     *http.Client
}

func newClient(apiKey, language string) *client {
	return &client{
		apiKey:   apiKey,
		language: language,
		// connect timeout 10s folds into the per-call request timeout.
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) enabled() bool { return strings.TrimSpace(c.apiKey) != "" }

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

// searchMovie returns the first hit's TMDB id, with a year-less retry when a
// year-filtered query returns nothing (mirrors TmdbClient.searchMovie).
func (c *client) searchMovie(ctx context.Context, title string, year *int) (int64, bool) {
	if !c.enabled() || strings.TrimSpace(title) == "" {
		return 0, false
	}
	id, ok := c.searchMovieOnce(ctx, title, year)
	if ok || year == nil {
		return id, ok
	}
	return c.searchMovieOnce(ctx, title, nil)
}

func (c *client) searchMovieOnce(ctx context.Context, title string, year *int) (int64, bool) {
	u := apiBase + "/search/movie?language=" + url.QueryEscape(c.language) +
		"&query=" + url.QueryEscape(title)
	if year != nil {
		u += "&year=" + strconv.Itoa(*year)
	}
	return c.firstSearchID(ctx, u)
}

func (c *client) searchTv(ctx context.Context, title string, year *int) (int64, bool) {
	if !c.enabled() || strings.TrimSpace(title) == "" {
		return 0, false
	}
	id, ok := c.searchTvOnce(ctx, title, year)
	if ok || year == nil {
		return id, ok
	}
	return c.searchTvOnce(ctx, title, nil)
}

func (c *client) searchTvOnce(ctx context.Context, title string, year *int) (int64, bool) {
	u := apiBase + "/search/tv?language=" + url.QueryEscape(c.language) +
		"&query=" + url.QueryEscape(title)
	if year != nil {
		u += "&first_air_date_year=" + strconv.Itoa(*year)
	}
	return c.firstSearchID(ctx, u)
}

func (c *client) firstSearchID(ctx context.Context, u string) (int64, bool) {
	body, ok := c.getJSON(ctx, u)
	if !ok {
		return 0, false
	}
	var resp struct {
		Results []struct {
			ID int64 `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Results) == 0 {
		return 0, false
	}
	id := resp.Results[0].ID
	if id > 0 {
		return id, true
	}
	return 0, false
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
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
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
