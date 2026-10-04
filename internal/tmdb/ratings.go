package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/graph"
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
// a series, typ, with TMDB id tmdbID) and keeps its rating: the first of
// countries that rates it wins (ratings.Choose), its certification, the
// country and the minimum age it means; no certification at all when none of
// them rates it. Either way it stamps when TMDB was read, and a title whose
// rating changed is modified with it. rated says whether a country rated it.
//
// When TMDB could not be read, the title keeps the rating it had, and err says
// why; on a catalog without migration 036 there is nothing to keep it in, and
// nothing is read. metadataLocked does not stop it: an admin rates a title by
// hand with its min_age_override, which wins over this rating.
func (s *Service) rateTitle(ctx context.Context, itemID, typ string, tmdbID int64, countries []string) (rated bool, err error) {
	if !s.ratingsReady(ctx) {
		return false, errRatingsMissing
	}
	certs, err := s.tmdb.getCertifications(ctx, typ, tmdbID)
	if err != nil {
		return false, err
	}
	r, rated := ratings.Choose(countries, certs)
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
	if _, err := s.rateTitle(ctx, itemID, typ, tmdbID, s.ratingCountries(ctx)); err != nil && !errors.Is(err, errRatingsMissing) {
		log.Printf("tmdb: the certifications of %s %s (TMDB %d) could not be read: %v", typ, itemID, tmdbID, err)
	}
}

// BackfillRatings is the operator's backfill of ratings
// (graph.RatingsBackfiller): it reads TMDB's certifications of the movies and
// series that have a TMDB id and rates each as enrichment does (rateTitle),
// in the countries ratings.countries names when the run starts. With all it
// reads every one of them; without, only those TMDB has never been read for,
// so running it again picks up what a failure left. Episodes are rated as
// their series and are not read. The run goes on even when the caller stops
// waiting (its counts are logged), and only one runs at a time.
func (s *Service) BackfillRatings(ctx context.Context, all bool) (graph.RatingsBackfillResult, error) {
	var res graph.RatingsBackfillResult
	if !s.tmdb.enabled() {
		return res, errors.New("TMDB API key not configured")
	}
	if !s.ratingsReady(ctx) {
		return res, errRatingsMissing
	}
	if !s.backfillingRatings.CompareAndSwap(false, true) {
		return res, errors.New("a ratings backfill is already running")
	}
	defer s.backfillingRatings.Store(false)
	ctx = context.WithoutCancel(ctx)
	res.StartedAt = time.Now().UTC()
	res.Countries = s.ratingCountries(ctx)

	rows, err := s.pool.Query(ctx, `SELECT i.id, i.type, e.externalid
		FROM com_nalet_katalog_items i
		JOIN com_nalet_katalog_itemexternalids e ON e.item_id = i.id AND e.source = 'tmdb'
		WHERE i.type IN ('movie', 'series') AND ($1 OR i.certification_fetched_at IS NULL)
		ORDER BY i.type, i.id`, all)
	if err != nil {
		return res, err
	}
	var titles []titleRef
	for rows.Next() {
		var t titleRef
		var ext string
		if err := rows.Scan(&t.id, &t.typ, &ext); err != nil {
			rows.Close()
			return res, err
		}
		if t.tmdbID, err = strconv.ParseInt(strings.TrimSpace(ext), 10, 64); err == nil {
			titles = append(titles, t)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	for _, t := range titles {
		rated, err := s.rateTitle(ctx, t.id, t.typ, t.tmdbID, res.Countries)
		switch {
		case err != nil:
			res.TitlesFailed++
			log.Printf("tmdb: ratings backfill: the certifications of %s %s (TMDB %d) could not be read: %v", t.typ, t.id, t.tmdbID, err)
		case rated:
			res.TitlesRead++
			res.TitlesRated++
		default:
			res.TitlesRead++
			res.TitlesUnrated++
		}
	}
	res.FinishedAt = time.Now().UTC()
	log.Printf("tmdb: ratings backfill all=%v in %s: titles read=%d rated=%d unrated=%d failed=%d (%s)", all,
		strings.Join(res.Countries, ","), res.TitlesRead, res.TitlesRated, res.TitlesUnrated, res.TitlesFailed,
		res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond))
	return res, nil
}
