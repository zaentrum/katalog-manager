package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OMDb (omdbapi.com) metadata fallback. TMDB stays the primary source; OMDb only
// (a) fills description/rating/poster that TMDB left blank (keyed by the IMDb id
// TMDB already resolved), and (b) matches items TMDB misses entirely, by
// title+year. OMDb's free tier is per-user + 1,000 req/day, so the key is never
// bundled — it comes from the `omdb.api_key` setting (or OMDB_API_KEY env). A
// blank key disables it: every call returns (nil, false).
//
// The key is resolved per call via `key` so an operator can set it at runtime
// (settings editor) without a restart.

const omdbBase = "https://www.omdbapi.com/"

type omdbClient struct {
	key  func() string
	http *http.Client
}

func newOMDbClient(key func() string) *omdbClient {
	return &omdbClient{key: key, http: &http.Client{Timeout: 20 * time.Second}}
}

func (o *omdbClient) enabled() bool { return strings.TrimSpace(o.key()) != "" }

// omdbResult is the normalised subset of an OMDb response the enricher consumes.
type omdbResult struct {
	Title  string
	Year   int
	Plot   string  // -> description
	Rating float64 // parsed imdbRating -> rating
	Poster string  // artwork URL ("" when N/A)
	Genres []string
	ImdbID string
	Type   string // movie | series | episode
}

// byIMDb fetches by IMDb id — the most reliable OMDb key (TMDB provides it).
func (o *omdbClient) byIMDb(ctx context.Context, imdbID string) (*omdbResult, bool) {
	if strings.TrimSpace(imdbID) == "" {
		return nil, false
	}
	return o.fetch(ctx, url.Values{"i": {strings.TrimSpace(imdbID)}, "plot": {"full"}})
}

// byTitle fetches by title (+ optional year, + type movie|series) — used when no
// IMDb id is known, e.g. TMDB missed the item entirely.
func (o *omdbClient) byTitle(ctx context.Context, title string, year *int, kind string) (*omdbResult, bool) {
	if strings.TrimSpace(title) == "" {
		return nil, false
	}
	q := url.Values{"t": {strings.TrimSpace(title)}, "plot": {"full"}}
	if year != nil && *year > 0 {
		q.Set("y", strconv.Itoa(*year))
	}
	if kind == "movie" || kind == "series" {
		q.Set("type", kind)
	}
	return o.fetch(ctx, q)
}

func (o *omdbClient) fetch(ctx context.Context, q url.Values) (*omdbResult, bool) {
	if !o.enabled() {
		return nil, false
	}
	q.Set("apikey", strings.TrimSpace(o.key()))
	return o.fetchURL(ctx, omdbBase+"?"+q.Encode())
}

// fetchURL performs the OMDb GET + decode against a fully-formed URL (split out so
// the decode/mapping path is unit-testable against a stub server).
func (o *omdbClient) fetchURL(ctx context.Context, rawURL string) (*omdbResult, bool) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var n struct {
		Title      string `json:"Title"`
		Year       string `json:"Year"`
		Genre      string `json:"Genre"`
		Plot       string `json:"Plot"`
		Poster     string `json:"Poster"`
		ImdbRating string `json:"imdbRating"`
		ImdbID     string `json:"imdbID"`
		Type       string `json:"Type"`
		Response   string `json:"Response"`
	}
	if json.NewDecoder(resp.Body).Decode(&n) != nil {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(n.Response), "true") {
		return nil, false // "Response":"False" (not found / invalid key / quota)
	}
	res := &omdbResult{
		Title:  strings.TrimSpace(n.Title),
		Plot:   omdbVal(n.Plot),
		Poster: omdbVal(n.Poster),
		ImdbID: omdbVal(n.ImdbID),
		Type:   strings.ToLower(strings.TrimSpace(n.Type)),
		Genres: omdbGenres(n.Genre),
	}
	if y := omdbFirstYear(n.Year); y > 0 {
		res.Year = y
	}
	if r, err := strconv.ParseFloat(strings.TrimSpace(n.ImdbRating), 64); err == nil && r > 0 {
		res.Rating = r
	}
	// Poster must be a usable http(s) URL (OMDb serves Amazon-hosted images).
	if !strings.HasPrefix(res.Poster, "http") {
		res.Poster = ""
	}
	return res, true
}

var omdbYearRE = regexp.MustCompile(`(19|20)\d{2}`)

// omdbVal maps OMDb's "N/A" sentinel to an empty string.
func omdbVal(s string) string {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "N/A") {
		return ""
	}
	return s
}

// omdbFirstYear extracts the first 4-digit year (OMDb series years look like
// "2019–" or "2019–2021").
func omdbFirstYear(s string) int {
	if m := omdbYearRE.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}

// omdbGenres splits OMDb's "Action, Drama" comma list into trimmed names.
func omdbGenres(s string) []string {
	s = omdbVal(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
