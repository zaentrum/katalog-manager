// Package chaptersdb ports the CAP ChaptersDbClient — a minimal HTTP client for
// chaptersdb.com used as an opt-in sidecar of TMDB movie enrichment (SPEC §1.9,
// §30/5). Disabled by default (cfg.ChaptersDBEnabled=false): every call is a
// no-op returning empty. Only HTTP 200 responses are parsed.
package chaptersdb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// Client is the chaptersdb HTTP client. Construct with New.
type Client struct {
	enabled bool
	baseURL string
	http    *http.Client
}

// New builds a Client from config. When cfg.ChaptersDBEnabled is false the
// client is disabled and every method short-circuits to an empty result. The
// base URL's trailing slashes are trimmed (mirrors the Java replaceAll("/+$")).
func New(cfg config.Config) *Client {
	return &Client{
		enabled: cfg.ChaptersDBEnabled,
		baseURL: strings.TrimRight(cfg.ChaptersDBBaseURL, "/"),
		// connectTimeout 10s is folded into the overall request timeout per call.
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether the sidecar is turned on.
func (c *Client) Enabled() bool { return c.enabled }

// Show is a chaptersdb show record.
type Show struct {
	ID     string
	Slug   string
	Title  string
	Type   string // "movie" | "tv"
	Year   string
	TvdbID string
	ImdbID string
}

// ChapterEntry is one chapter marker (start offset + optional name).
type ChapterEntry struct {
	StartMs int64
	Name    string
}

// rawShow mirrors the JSON shape of a /api/shows/search result element.
type rawShow struct {
	ID     *json.RawMessage `json:"id"`
	Slug   string           `json:"slug"`
	Title  string           `json:"title"`
	Type   string           `json:"type"`
	Year   *json.RawMessage `json:"year"`
	TvdbID *json.RawMessage `json:"tvdbId"`
	ImdbID *json.RawMessage `json:"imdbId"`
}

// rawChapterSet mirrors one /api/chapters/by-show/<id> set.
type rawChapterSet struct {
	Entries []rawChapterEntry `json:"entries"`
}

type rawChapterEntry struct {
	Time string `json:"time"`
	Name string `json:"name"`
}

// FindShow returns the chaptersdb show best matching title+year for the given
// type ("movie"/"tv"). Same-titled shows are disambiguated by year; otherwise
// the first type match wins. Disabled client / blank title => no result.
func (c *Client) FindShow(ctx context.Context, typ string, year *int, title string) (*Show, bool) {
	if !c.enabled || strings.TrimSpace(title) == "" {
		return nil, false
	}
	u := c.baseURL + "/api/shows/search?q=" + url.QueryEscape(title)
	body, ok := c.getJSON(ctx, u, 15*time.Second)
	if !ok {
		return nil, false
	}
	var arr []rawShow
	if err := json.Unmarshal(body, &arr); err != nil || len(arr) == 0 {
		return nil, false
	}
	var yearMatch, typeMatch *Show
	for i := range arr {
		s := asShow(&arr[i])
		if s == nil {
			continue
		}
		if typ != "" && !strings.EqualFold(typ, s.Type) {
			continue
		}
		if typeMatch == nil {
			typeMatch = s
		}
		if year != nil && strconv.Itoa(*year) == s.Year {
			yearMatch = s
			break
		}
	}
	if yearMatch != nil {
		return yearMatch, true
	}
	if typeMatch != nil {
		return typeMatch, true
	}
	return nil, false
}

// GetMovieChapters returns the first chapter set's entries (sorted by start)
// for a show id. Disabled client / blank id / non-200 => empty slice.
func (c *Client) GetMovieChapters(ctx context.Context, showID string) []ChapterEntry {
	if !c.enabled || showID == "" {
		return nil
	}
	u := c.baseURL + "/api/chapters/by-show/" + showID
	body, ok := c.getJSON(ctx, u, 15*time.Second)
	if !ok {
		return nil
	}
	var sets []rawChapterSet
	if err := json.Unmarshal(body, &sets); err != nil || len(sets) == 0 {
		return nil
	}
	out := make([]ChapterEntry, 0, len(sets[0].Entries))
	for _, e := range sets[0].Entries {
		ms, ok := parseTimestampMs(e.Time)
		if !ok {
			continue
		}
		out = append(out, ChapterEntry{StartMs: ms, Name: e.Name})
	}
	// stable sort by start offset (mirrors the Java Long.compare sort)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].StartMs > out[j].StartMs; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// getJSON performs an HTTP GET, returning the raw body only on HTTP 200.
func (c *Client) getJSON(ctx context.Context, rawURL string, timeout time.Duration) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}
	return body, true
}

// asShow maps a raw show record; returns nil when id is absent.
func asShow(n *rawShow) *Show {
	id := rawText(n.ID)
	if id == "" {
		return nil
	}
	return &Show{
		ID:     id,
		Slug:   n.Slug,
		Title:  n.Title,
		Type:   n.Type,
		Year:   rawText(n.Year),
		TvdbID: rawText(n.TvdbID),
		ImdbID: rawText(n.ImdbID),
	}
}

// rawText coerces a JSON value (string or number) to its text form, matching
// Jackson's asText(null) behaviour. Missing/null => "".
func rawText(m *json.RawMessage) string {
	if m == nil {
		return ""
	}
	s := strings.TrimSpace(string(*m))
	if s == "" || s == "null" {
		return ""
	}
	if len(s) >= 2 && s[0] == '"' {
		var v string
		if err := json.Unmarshal(*m, &v); err == nil {
			return v
		}
	}
	return s
}

// parseTimestampMs parses "HH:MM:SS.mmm" into total milliseconds.
func parseTimestampMs(ts string) (int64, bool) {
	if ts == "" {
		return 0, false
	}
	parts := strings.Split(ts, ":")
	if len(parts) != 3 {
		return 0, false
	}
	hours, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	minutes, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	seconds, err := strconv.ParseFloat(parts[2], 64)
	if err != nil {
		return 0, false
	}
	return int64(hours)*3_600_000 + int64(minutes)*60_000 + int64(seconds*1000), true
}
