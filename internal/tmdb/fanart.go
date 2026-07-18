package tmdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// fanart.tv artwork fallback. TMDB stays the primary metadata + artwork source;
// fanartClient only fills poster/backdrop that TMDB is MISSING (community artwork,
// keyed by TMDB id for movies and TheTVDB id for series). Licensing-clean and
// free (a project api_key; an optional personal client_key returns fresher
// images). A blank api key disables it — every call returns "".
//
// It supplies artwork only, never metadata, so it can never override a TMDB match.

const fanartBase = "https://webservice.fanart.tv/v3"

type fanartClient struct {
	apiKey    string
	clientKey string
	http      *http.Client
}

func newFanartClient(apiKey, clientKey string) *fanartClient {
	return &fanartClient{
		apiKey:    strings.TrimSpace(apiKey),
		clientKey: strings.TrimSpace(clientKey),
		http:      &http.Client{Timeout: 20 * time.Second},
	}
}

func (f *fanartClient) enabled() bool { return f.apiKey != "" }

// fanartImage is one artwork entry. `likes` is a string in the fanart.tv JSON.
type fanartImage struct {
	URL   string `json:"url"`
	Lang  string `json:"lang"`
	Likes string `json:"likes"`
}

// movieArtwork returns the best (poster, background) URLs for a movie by TMDB id
// (fanart.tv also accepts an imdb id here). Empty strings when unavailable.
func (f *fanartClient) movieArtwork(ctx context.Context, id string) (poster, background string) {
	var r struct {
		MoviePoster     []fanartImage `json:"movieposter"`
		MovieBackground []fanartImage `json:"moviebackground"`
	}
	if !f.get(ctx, "/movies/"+url.PathEscape(id), &r) {
		return "", ""
	}
	return bestFanart(r.MoviePoster), bestFanart(r.MovieBackground)
}

// tvArtwork returns the best (poster, background) URLs for a series by TheTVDB id
// (fanart.tv keys TV on TheTVDB, not TMDB — resolve it via TMDB external_ids).
func (f *fanartClient) tvArtwork(ctx context.Context, tvdbID string) (poster, background string) {
	var r struct {
		TVPoster       []fanartImage `json:"tvposter"`
		ShowBackground []fanartImage `json:"showbackground"`
	}
	if !f.get(ctx, "/tv/"+url.PathEscape(tvdbID), &r) {
		return "", ""
	}
	return bestFanart(r.TVPoster), bestFanart(r.ShowBackground)
}

// bestFanart picks the highest-ranked image: prefer an English (or textless)
// entry, then most likes. Returns "" for an empty set.
func bestFanart(imgs []fanartImage) string {
	best, bestScore := "", -1
	for _, im := range imgs {
		if strings.TrimSpace(im.URL) == "" {
			continue
		}
		score := 0
		switch strings.ToLower(im.Lang) {
		case "en":
			score += 100
		case "", "00":
			score += 50
		}
		if likes, err := strconv.Atoi(strings.TrimSpace(im.Likes)); err == nil {
			if likes > 40 {
				likes = 40
			}
			score += likes
		}
		if score > bestScore {
			best, bestScore = im.URL, score
		}
	}
	return best
}

// get performs an authorized fanart.tv GET into out; false on any non-200/parse
// error (fanart.tv returns 404 for an unknown id — a normal "no artwork" miss).
func (f *fanartClient) get(ctx context.Context, path string, out any) bool {
	if !f.enabled() {
		return false
	}
	u := fanartBase + path + "?api_key=" + url.QueryEscape(f.apiKey)
	if f.clientKey != "" {
		u += "&client_key=" + url.QueryEscape(f.clientKey)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}
