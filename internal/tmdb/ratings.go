package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/ratings"
)

// A title's age rating comes from the certifications TMDB gives it, country
// by country; internal/ratings says what each of them means. A film's are
// those of its theatrical (type 3) and digital (type 4) releases, in
// GET /movie/{id}/release_dates; a series' are its content ratings, in
// GET /tv/{id}/content_ratings. An episode is rated as its series and keeps
// no certification of its own.

// TMDB's release types that rate a film.
const (
	releaseTheatrical = 3
	releaseDigital    = 4
)

// movieCertifications reads GET /movie/{id}/release_dates as TMDB answers it:
// per country (upper case), the certifications of the film's theatrical and
// digital releases there, in TMDB's order; a release without one, and every
// other release (premiere, limited, physical, TV), is left out.
func movieCertifications(body []byte) (map[string][]string, error) {
	var n struct {
		Results []struct {
			Country  string `json:"iso_3166_1"`
			Releases []struct {
				Certification string `json:"certification"`
				Type          int    `json:"type"`
			} `json:"release_dates"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, errors.New("tmdb: release dates: " + err.Error())
	}
	out := map[string][]string{}
	for _, r := range n.Results {
		country := strings.ToUpper(strings.TrimSpace(r.Country))
		for _, rel := range r.Releases {
			if (rel.Type == releaseTheatrical || rel.Type == releaseDigital) && strings.TrimSpace(rel.Certification) != "" {
				out[country] = append(out[country], rel.Certification)
			}
		}
	}
	return out, nil
}

// tvCertifications reads GET /tv/{id}/content_ratings as TMDB answers it: per
// country (upper case), the series' rating there; an empty one is left out.
func tvCertifications(body []byte) (map[string][]string, error) {
	var n struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &n); err != nil {
		return nil, errors.New("tmdb: content ratings: " + err.Error())
	}
	out := map[string][]string{}
	for _, r := range n.Results {
		if strings.TrimSpace(r.Rating) != "" {
			country := strings.ToUpper(strings.TrimSpace(r.Country))
			out[country] = append(out[country], r.Rating)
		}
	}
	return out, nil
}

// getCertifications reads the certifications TMDB gives the title of typ
// (movie, or series) with TMDB id id, by country. TMDB not knowing the title
// is a *statusError 404 (isNotFound).
func (c *client) getCertifications(ctx context.Context, typ string, id int64) (map[string][]string, error) {
	if !c.enabled() {
		return nil, errors.New("tmdb: no API key")
	}
	if typ == "series" {
		body, err := c.get(ctx, c.apiBase+"/tv/"+strconv.FormatInt(id, 10)+"/content_ratings")
		if err != nil {
			return nil, err
		}
		return tvCertifications(body)
	}
	body, err := c.get(ctx, c.apiBase+"/movie/"+strconv.FormatInt(id, 10)+"/release_dates")
	if err != nil {
		return nil, err
	}
	return movieCertifications(body)
}

// ratingCountries are the countries whose certifications rate a title, in
// order: the setting ratings.countries, else ratings.DefaultCountries.
func (s *Service) ratingCountries(ctx context.Context) []string {
	if s.lookup != nil {
		if v, ok := s.lookup(ctx, ratings.CountriesSetting); ok {
			return ratings.Countries(v)
		}
	}
	return ratings.Countries(ratings.DefaultCountries)
}

// ratingsReady reports whether migration 036 is in place, remembering once it
// is.
func (s *Service) ratingsReady(ctx context.Context) bool {
	if s.ratingsOK.Load() {
		return true
	}
	if s.ratingsCheck == nil {
		return false
	}
	ok, err := s.ratingsCheck(ctx)
	if err == nil && ok {
		s.ratingsOK.Store(true)
	}
	return ok && err == nil
}

// rateTitle reads the certifications TMDB gives the title itemID (a movie or
// a series, typ, with TMDB id tmdbID) and keeps its rating: the first country
// of ratings.countries that rates it wins (ratings.Choose), its certification,
// the country and the minimum age it means; no certification at all when none
// of them rates it. Either way it stamps when TMDB was read, and a title whose
// rating changed is modified with it. rated says whether a country rated it.
//
// When TMDB could not be read, the title keeps the rating it had, and err says
// why; on a catalog without migration 036 there is nothing to keep it in, and
// nothing is read. metadataLocked does not stop it: an admin rates a title by
// hand with its min_age_override, which wins over this rating.
func (s *Service) rateTitle(ctx context.Context, itemID, typ string, tmdbID int64) (rated bool, err error) {
	if !s.ratingsReady(ctx) {
		return false, errRatingsMissing
	}
	certs, err := s.tmdb.getCertifications(ctx, typ, tmdbID)
	if err != nil {
		return false, err
	}
	r, rated := ratings.Choose(s.ratingCountries(ctx), certs)
	var cert, country *string
	var age *int32
	if rated {
		a := int32(r.MinAge)
		cert, country, age = &r.Certification, &r.Country, &a
	}
	by := clip(auth.Actor(ctx, "katalog-manager/tmdb"), 255)
	_, err = s.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET
			certification = $2::varchar, certification_country = $3::varchar, min_age = $4::smallint,
			certification_fetched_at = now(),
			modifiedat = CASE WHEN (certification, certification_country, min_age)
				IS DISTINCT FROM ($2::varchar, $3::varchar, $4::smallint) THEN now() ELSE modifiedat END,
			modifiedby = CASE WHEN (certification, certification_country, min_age)
				IS DISTINCT FROM ($2::varchar, $3::varchar, $4::smallint) THEN $5 ELSE modifiedby END
		WHERE id = $1`, itemID, cert, country, age, by)
	return rated, err
}

// errRatingsMissing says the catalog has no migration 036 to keep ratings in.
var errRatingsMissing = errors.New("the ratings migration (db/migrations/036_item_ratings.sql) is not applied")

// rateEnriched rates a movie or series enrichment has just read from TMDB.
// Its rating failing fails nothing else: the title stays as it was rated, and
// the next enrichment, the change lists or backfillRatings read it again.
func (s *Service) rateEnriched(ctx context.Context, itemID, typ string, tmdbID int64) {
	if _, err := s.rateTitle(ctx, itemID, typ, tmdbID); err != nil && !errors.Is(err, errRatingsMissing) {
		log.Printf("tmdb: the certifications of %s %s (TMDB %d) could not be read: %v", typ, itemID, tmdbID, err)
	}
}
