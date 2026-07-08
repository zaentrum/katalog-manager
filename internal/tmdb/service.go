// Package tmdb ports the CAP EnrichmentService — per-item TMDB metadata
// enrichment for movies and series (+ child episodes). It implements
// graph.Enricher. All writes go through st.Pool() (bespoke SQL); identifiers
// are lowercase. A blank TMDB key disables the client: enrichment becomes a
// no-op that still reports a non-error 'skipped' status. See SPEC §30/1.
package tmdb

import (
	"context"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/chaptersdb"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// Status values returned by EnrichOne (lowercase, matching Result.toMap()).
const (
	statusDone     = "done"
	statusNotFound = "not_found"
	statusFailed   = "failed"
	statusSkipped  = "skipped"
)

// Service is the TMDB enrichment service.
type Service struct {
	pool  *pgxpool.Pool
	cfg   config.Config
	steps *processing.Steps
	ch    *chaptersdb.Client
	tmdb  *client
}

// New builds the enrichment Service. The TMDB client is disabled when
// cfg.TMDBAPIKey is blank (every call no-ops; EnrichOne reports 'skipped').
func New(st storePool, cfg config.Config, steps *processing.Steps, ch *chaptersdb.Client) *Service {
	return &Service{
		pool:  st.Pool(),
		cfg:   cfg,
		steps: steps,
		ch:    ch,
		tmdb:  newClient(cfg.TMDBAPIKey, cfg.TMDBLanguage),
	}
}

// Poll cadence for the background enrichment drain. Each tick sweeps up to
// pollBatch pending items synchronously (no overlap); the client backs off on
// TMDB rate limits, so the sweep self-paces under a shared/bundled token.
const (
	pollInterval = 60 * time.Second
	pollBatch    = 50
)

// RunPoller drains the pending tmdb queue in the background for the lifetime of
// ctx — the auto-enrichment worker (mirrors the trailers/download pollers).
// Without it nothing advances items whose tmdb step is pending, so enrichment
// stalls (the June regression: 74k pending, 0 processed). No-op when TMDB is
// disabled. Runs one batch immediately, then one per tick.
func (s *Service) RunPoller(ctx context.Context) {
	if !s.cfg.TMDBEnabled() {
		return
	}
	log.Printf("tmdb: enrichment poller started (interval=%s batch=%d)", pollInterval, pollBatch)
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		s.sweepPending(pollBatch, "") // synchronous: one batch per tick, no overlap
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// storePool is the subset of *store.Store the service needs. The concrete
// *store.Store satisfies it; declaring it locally keeps the dependency narrow.
type storePool interface {
	Pool() *pgxpool.Pool
}

var titleYearRE = regexp.MustCompile(`\((19\d{2}|20\d{2})\)`)

// ===================== graph.Enricher =====================

// EnrichOne synchronously enriches a single item. Item-missing maps to 'failed'
// (still a non-error return); a disabled client maps to 'skipped'. Returns
// (status, message, nil) — the error channel is only for unexpected DB faults.
func (s *Service) EnrichOne(ctx context.Context, id string) (string, string, error) {
	typ, title, year, ok := s.loadItem(ctx, id)
	if !ok {
		return statusFailed, "item not found", nil
	}
	status, msg := s.enrichRow(ctx, id, typ, title, year)
	return status, msg, nil
}

// IdentifyOne re-enriches an item from an operator-supplied match: a corrected
// title to re-search and/or a specific TMDB id to pin. A pinned id wins (skips
// the search); otherwise the existing tmdb link is dropped so the (override)
// title re-resolves from scratch. On success it overwrites the metadata +
// artwork (applyMovie/applyTv set the title from TMDB, so the name is corrected too).
func (s *Service) IdentifyOne(ctx context.Context, id, titleOverride string, tmdbID *int64) (string, string, error) {
	typ, title, year, ok := s.loadItem(ctx, id)
	if !ok {
		return statusFailed, "item not found", nil
	}
	if strings.TrimSpace(titleOverride) != "" {
		title = titleOverride
	}
	if tmdbID != nil {
		s.upsertExternalID(ctx, id, "tmdb", strconv.FormatInt(*tmdbID, 10))
	} else {
		s.clearExternalID(ctx, id, "tmdb")
	}
	status, msg := s.enrichRow(ctx, id, typ, title, year)
	return status, msg, nil
}

// EnrichPending queues up to limit items whose tmdb step is pending or absent
// (ORDER BY createdat ASC) and runs the sweep asynchronously. It returns the
// clamped queue size immediately (matching the CAP controller's 202 {queued}).
func (s *Service) EnrichPending(ctx context.Context, limit int32, typ string) (int32, error) {
	lim := limit
	if lim <= 0 || lim > 1000 {
		lim = 50
	}
	go s.sweepPending(int(lim), typ)
	return lim, nil
}

// sweepPending runs the background enrichment sweep on its own context (the
// request context is gone by the time this runs).
func (s *Service) sweepPending(limit int, typ string) {
	ctx := context.Background()
	rows, err := s.pendingQueue(ctx, limit, typ)
	if err != nil {
		log.Printf("tmdb: enrich pending queue failed: %v", err)
		return
	}
	considered, enriched, failed := len(rows), 0, 0
	for _, r := range rows {
		status, _ := s.enrichRow(ctx, r.id, r.typ, r.title, r.year)
		switch status {
		case statusDone:
			enriched++
		case statusFailed:
			failed++
		}
	}
	t := typ
	if strings.TrimSpace(t) == "" {
		t = "movie+series"
	}
	log.Printf("tmdb: enrich pending type=%s considered=%d enriched=%d failed=%d", t, considered, enriched, failed)
}

type queueRow struct {
	id, typ, title string
	year           *int
}

func (s *Service) pendingQueue(ctx context.Context, limit int, typ string) ([]queueRow, error) {
	var rows pgx.Rows
	var err error
	base := `SELECT i.id, i.type, i.title, i.year FROM com_nalet_katalog_items i WHERE `
	tail := ` AND (
		  EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
		          WHERE s.item_id = i.id AND s.step = 'tmdb' AND s.status = 'pending')
		  OR NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
		                 WHERE s.item_id = i.id AND s.step = 'tmdb')
		) ORDER BY i.createdat ASC LIMIT $1`
	if strings.TrimSpace(typ) == "" {
		rows, err = s.pool.Query(ctx, base+`type IN ('movie','series')`+tail, limit)
	} else {
		rows, err = s.pool.Query(ctx, base+`type = $2`+tail, limit, typ)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []queueRow
	for rows.Next() {
		var qr queueRow
		if err := rows.Scan(&qr.id, &qr.typ, &qr.title, &qr.year); err != nil {
			return nil, err
		}
		out = append(out, qr)
	}
	return out, rows.Err()
}

// BackfillEpisodeBackdrops clones every episode poster artwork row into a
// backdrop row (both byte and URL tables), idempotently. Returns
// (artworkData rows, artwork rows) inserted.
func (s *Service) BackfillEpisodeBackdrops(ctx context.Context) (int32, int32, error) {
	dataTag, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_itemartworkdata
		  (id, item_id, kind, contenttype, bytes, fetchedat)
		SELECT gen_random_uuid()::text, p.item_id, 'backdrop',
		       p.contenttype, p.bytes, p.fetchedat
		FROM com_nalet_katalog_itemartworkdata p
		JOIN com_nalet_katalog_items i ON i.id = p.item_id
		WHERE p.kind = 'poster' AND i.type = 'episode'
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemartworkdata b
		                  WHERE b.item_id = p.item_id AND b.kind = 'backdrop')`)
	if err != nil {
		return 0, 0, err
	}
	urlTag, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_itemartwork (id, item_id, kind, url)
		SELECT gen_random_uuid()::text, a.item_id, 'backdrop', a.url
		FROM com_nalet_katalog_itemartwork a
		JOIN com_nalet_katalog_items i ON i.id = a.item_id
		WHERE a.kind = 'poster' AND i.type = 'episode'
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemartwork b
		                  WHERE b.item_id = a.item_id AND b.kind = 'backdrop')`)
	if err != nil {
		return 0, 0, err
	}
	return int32(dataTag.RowsAffected()), int32(urlTag.RowsAffected()), nil
}

// RetryNotFound flips every tmdb step in status 'skipped' back to 'pending'
// (optionally narrowed by item type). Returns the number of rows reset.
func (s *Service) RetryNotFound(ctx context.Context, typ string) (int32, error) {
	if strings.TrimSpace(typ) == "" {
		tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemprocessingsteps SET
			status = 'pending', startedat = NULL, finishedat = NULL, error = NULL, modifiedat = now()
			WHERE step = 'tmdb' AND status = 'skipped'`)
		if err != nil {
			return 0, err
		}
		return int32(tag.RowsAffected()), nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemprocessingsteps s SET
		status = 'pending', startedat = NULL, finishedat = NULL, error = NULL, modifiedat = now()
		FROM com_nalet_katalog_items i
		WHERE s.step = 'tmdb' AND s.status = 'skipped'
		  AND s.item_id = i.id AND i.type = $1`, typ)
	if err != nil {
		return 0, err
	}
	return int32(tag.RowsAffected()), nil
}

// ===================== enrichment core =====================

func (s *Service) loadItem(ctx context.Context, id string) (typ, title string, year *int, ok bool) {
	err := s.pool.QueryRow(ctx,
		`SELECT type, title, year FROM com_nalet_katalog_items WHERE id = $1`, id).
		Scan(&typ, &title, &year)
	if err != nil {
		return "", "", nil, false
	}
	return typ, title, year, true
}

// enrichRow dispatches by type. Mirrors EnrichmentService.enrichRow including
// the in-title year fallback and the disabled-client short-circuit.
func (s *Service) enrichRow(ctx context.Context, id, typ, title string, year *int) (status, message string) {
	if year == nil && title != "" {
		if m := titleYearRE.FindStringSubmatch(title); m != nil {
			if y, err := strconv.Atoi(m[1]); err == nil {
				year = &y
			}
		}
	}
	if !s.tmdb.enabled() {
		return statusSkipped, "TMDB API key not configured"
	}
	switch typ {
	case "movie":
		return s.enrichMovie(ctx, id, title, year)
	case "series":
		return s.enrichSeries(ctx, id, title, year)
	default:
		return statusSkipped, "type '" + typ + "' not implemented yet"
	}
}

func (s *Service) enrichMovie(ctx context.Context, id, title string, year *int) (string, string) {
	searchTitle := cleanTitle(title)
	s.markStatus(ctx, id, "in_progress", nil)

	tmdbID, ok := s.existingTmdbID(ctx, id)
	if !ok {
		tmdbID, ok = s.tmdb.searchMovie(ctx, searchTitle, year)
	}
	if !ok {
		s.markStatus(ctx, id, "not_found", nil)
		return statusNotFound, ""
	}
	s.upsertExternalID(ctx, id, "tmdb", strconv.FormatInt(tmdbID, 10))

	m, ok := s.tmdb.getMovie(ctx, tmdbID)
	if !ok {
		msg := "movie detail fetch returned nothing"
		s.markStatus(ctx, id, "failed", &msg)
		return statusFailed, msg
	}
	s.applyMovie(ctx, id, m)

	if c, ok := s.tmdb.getCredits(ctx, tmdbID); ok {
		s.applyCredits(ctx, id, c)
	}
	s.applyTrailerLinks(ctx, id, s.tmdb.getMovieVideos(ctx, tmdbID))

	var dur *int64
	if m.Runtime > 0 {
		d := int64(m.Runtime) * 60_000
		dur = &d
	}
	s.applyChaptersDb(ctx, id, m.Title, year, dur)

	if posterURL := s.tmdb.imageURL(m.PosterPath, "w780"); posterURL != "" {
		s.persistArtwork(ctx, id, "poster", posterURL)
	}
	if backdropURL := s.tmdb.imageURL(m.BackdropPath, "w1280"); backdropURL != "" {
		s.persistArtwork(ctx, id, "backdrop", backdropURL)
	}

	s.markStatus(ctx, id, "done", nil)
	return statusDone, ""
}

func (s *Service) enrichSeries(ctx context.Context, id, title string, year *int) (string, string) {
	searchTitle := cleanTitle(title)
	s.markStatus(ctx, id, "in_progress", nil)

	tmdbID, ok := s.existingTmdbID(ctx, id)
	if !ok {
		tmdbID, ok = s.tmdb.searchTv(ctx, searchTitle, year)
	}
	if !ok {
		s.markStatus(ctx, id, "not_found", nil)
		return statusNotFound, ""
	}
	s.upsertExternalID(ctx, id, "tmdb", strconv.FormatInt(tmdbID, 10))

	t, ok := s.tmdb.getTv(ctx, tmdbID)
	if !ok {
		msg := "tv detail fetch returned nothing"
		s.markStatus(ctx, id, "failed", &msg)
		return statusFailed, msg
	}
	s.applyTv(ctx, id, t)

	if c, ok := s.tmdb.getTvCredits(ctx, tmdbID); ok {
		s.applyCredits(ctx, id, c)
	}
	s.applyTrailerLinks(ctx, id, s.tmdb.getTvVideos(ctx, tmdbID))

	if posterURL := s.tmdb.imageURL(t.PosterPath, "w780"); posterURL != "" {
		s.persistArtwork(ctx, id, "poster", posterURL)
	}
	if backdropURL := s.tmdb.imageURL(t.BackdropPath, "w1280"); backdropURL != "" {
		s.persistArtwork(ctx, id, "backdrop", backdropURL)
	}

	s.enrichEpisodesOf(ctx, id, tmdbID)

	s.markStatus(ctx, id, "done", nil)
	return statusDone, ""
}

func (s *Service) enrichEpisodesOf(ctx context.Context, seriesID string, tmdbTvID int64) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, seasonnumber, episodenumber FROM com_nalet_katalog_items
		 WHERE parent_id = $1 AND type = 'episode'
		   AND seasonnumber IS NOT NULL AND episodenumber IS NOT NULL`, seriesID)
	if err != nil {
		return
	}
	type ep struct {
		id           string
		season, epis int
	}
	var eps []ep
	for rows.Next() {
		var e ep
		if err := rows.Scan(&e.id, &e.season, &e.epis); err != nil {
			rows.Close()
			return
		}
		eps = append(eps, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}
	for _, e := range eps {
		epd, ok := s.tmdb.getTvEpisode(ctx, tmdbTvID, e.season, e.epis)
		if !ok || strings.TrimSpace(epd.Name) == "" {
			continue
		}
		s.applyEpisode(ctx, e.id, epd)
	}
}

// ===================== field appliers =====================

func (s *Service) applyMovie(ctx context.Context, itemID string, m *tmdbMovie) {
	year := parseYear(m.ReleaseDate)
	var dur *int64
	if m.Runtime > 0 {
		d := int64(m.Runtime) * 60_000
		dur = &d
	}
	var rating *float64
	if m.VoteAverage > 0 {
		rating = &m.VoteAverage
	}
	var title, sort *string
	if strings.TrimSpace(m.Title) != "" {
		t := m.Title
		l := strings.ToLower(t)
		title, sort = &t, &l
	}
	s.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET
		title = COALESCE($1, title),
		sorttitle = COALESCE($2, sorttitle),
		description = COALESCE($3, description),
		rating = COALESCE($4, rating),
		durationms = COALESCE($5, durationms),
		year = COALESCE($6, year),
		tagline = $7,
		modifiedat = now()
		WHERE id = $8`,
		title, sort, nullStr(m.Overview), rating, dur, year, nullStr(m.Tagline), itemID)

	s.upsertGenres(ctx, itemID, m.Genres)
	if strings.TrimSpace(m.ImdbID) != "" {
		s.upsertExternalID(ctx, itemID, "imdb", m.ImdbID)
	}
}

func (s *Service) applyTv(ctx context.Context, itemID string, t *tmdbTv) {
	year := parseYear(t.FirstAirDate)
	var dur *int64
	if t.EpisodeRunTime > 0 {
		d := int64(t.EpisodeRunTime) * 60_000
		dur = &d
	}
	var rating *float64
	if t.VoteAverage > 0 {
		rating = &t.VoteAverage
	}
	var title, sort *string
	if strings.TrimSpace(t.Name) != "" {
		n := t.Name
		l := strings.ToLower(n)
		title, sort = &n, &l
	}
	s.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET
		title = COALESCE($1, title),
		sorttitle = COALESCE($2, sorttitle),
		description = COALESCE($3, description),
		rating = COALESCE($4, rating),
		durationms = COALESCE($5, durationms),
		year = COALESCE($6, year),
		tagline = $7,
		modifiedat = now()
		WHERE id = $8`,
		title, sort, nullStr(t.Overview), rating, dur, year, nullStr(t.Tagline), itemID)

	s.upsertGenres(ctx, itemID, t.Genres)
}

func (s *Service) applyEpisode(ctx context.Context, itemID string, ep *tmdbEpisode) {
	year := parseYear(ep.AirDate)
	var dur *int64
	if ep.Runtime > 0 {
		d := int64(ep.Runtime) * 60_000
		dur = &d
	}
	var rating *float64
	if ep.VoteAverage > 0 {
		rating = &ep.VoteAverage
	}
	var title, sort *string
	if strings.TrimSpace(ep.Name) != "" {
		n := ep.Name
		l := strings.ToLower(n)
		title, sort = &n, &l
	}
	s.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET
		title = COALESCE($1, title),
		sorttitle = COALESCE($2, sorttitle),
		description = COALESCE($3, description),
		rating = COALESCE($4, rating),
		durationms = COALESCE($5, durationms),
		year = COALESCE($6, year),
		modifiedat = now()
		WHERE id = $7`,
		title, sort, nullStr(ep.Overview), rating, dur, year, itemID)

	// Episode-side completion goes through the shared audit path.
	_ = s.steps.Upsert(ctx, itemID, "tmdb", processing.StatusDone, nil, nil)

	if ep.ID > 0 {
		s.upsertExternalID(ctx, itemID, "tmdb-episode", strconv.FormatInt(ep.ID, 10))
	}
	if strings.TrimSpace(ep.StillPath) != "" {
		if still := s.tmdb.imageURL(ep.StillPath, "w500"); still != "" {
			s.persistArtwork(ctx, itemID, "poster", still)
			s.persistArtwork(ctx, itemID, "backdrop", still)
		}
	}
}

func (s *Service) applyCredits(ctx context.Context, itemID string, c *tmdbCredits) {
	for _, director := range c.Crew {
		s.upsertPerson(ctx, itemID, director, "director")
	}
	for _, actor := range c.Cast {
		s.upsertPerson(ctx, itemID, actor, "actor")
	}
}

// ===================== relation upserts =====================

func (s *Service) existingTmdbID(ctx context.Context, itemID string) (int64, bool) {
	var ext string
	err := s.pool.QueryRow(ctx,
		`SELECT externalid FROM com_nalet_katalog_itemexternalids WHERE item_id = $1 AND source = 'tmdb'`,
		itemID).Scan(&ext)
	if err != nil {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(ext), 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// clearExternalID drops an item's external-id link so enrichment re-resolves it
// from scratch (used by Identify to discard a stale/wrong match before re-search).
func (s *Service) clearExternalID(ctx context.Context, itemID, source string) {
	s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_itemexternalids WHERE item_id = $1 AND source = $2`, itemID, source)
}

func (s *Service) upsertExternalID(ctx context.Context, itemID, source, externalID string) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM com_nalet_katalog_itemexternalids WHERE item_id = $1 AND source = $2`,
		itemID, source).Scan(&n); err != nil {
		return
	}
	if n > 0 {
		s.pool.Exec(ctx,
			`UPDATE com_nalet_katalog_itemexternalids SET externalid = $1 WHERE item_id = $2 AND source = $3`,
			externalID, itemID, source)
		return
	}
	s.pool.Exec(ctx,
		`INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES (gen_random_uuid()::varchar, $1, $2, $3)`,
		itemID, source, externalID)
}

func (s *Service) upsertGenres(ctx context.Context, itemID string, genres []string) {
	if len(genres) == 0 {
		return
	}
	seen := map[string]bool{}
	for _, name := range genres {
		if strings.TrimSpace(name) == "" || seen[name] {
			continue
		}
		seen[name] = true
		var genreID string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM com_nalet_katalog_genres WHERE name = $1`, name).Scan(&genreID)
		if err == pgx.ErrNoRows {
			genreID = ""
		} else if err != nil {
			continue
		}
		if genreID == "" {
			if err := s.pool.QueryRow(ctx,
				`INSERT INTO com_nalet_katalog_genres (id, name) VALUES (gen_random_uuid()::varchar, $1) RETURNING id`,
				name).Scan(&genreID); err != nil {
				continue
			}
		}
		var linked int
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM com_nalet_katalog_itemgenres WHERE item_id = $1 AND genre_id = $2`,
			itemID, genreID).Scan(&linked); err != nil {
			continue
		}
		if linked == 0 {
			s.pool.Exec(ctx,
				`INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES (gen_random_uuid()::varchar, $1, $2)`,
				itemID, genreID)
		}
	}
}

func (s *Service) upsertPerson(ctx context.Context, itemID, name, role string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	var personID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM com_nalet_katalog_people WHERE name = $1`, name).Scan(&personID)
	if err == pgx.ErrNoRows {
		personID = ""
	} else if err != nil {
		return
	}
	if personID == "" {
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO com_nalet_katalog_people (id, name) VALUES (gen_random_uuid()::varchar, $1) RETURNING id`,
			name).Scan(&personID); err != nil {
			return
		}
	}
	var linked int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM com_nalet_katalog_itempeople WHERE item_id = $1 AND person_id = $2 AND role = $3`,
		itemID, personID, role).Scan(&linked); err != nil {
		return
	}
	if linked == 0 {
		s.pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES (gen_random_uuid()::varchar, $1, $2, $3)`,
			itemID, personID, role)
	}
}

// applyTrailerLinks idempotently replaces TMDB-sourced, not-yet-downloaded
// trailer rows (manual + downloaded rows survive).
func (s *Service) applyTrailerLinks(ctx context.Context, itemID string, videos []tmdbVideo) {
	if len(videos) == 0 {
		return
	}
	s.pool.Exec(ctx,
		`DELETE FROM com_nalet_katalog_itemtrailerlinks WHERE item_id = $1 AND source = 'tmdb' AND downloadedat IS NULL`,
		itemID)
	for _, v := range videos {
		published := parseTmdbTs(v.PublishedAt)
		s.pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_itemtrailerlinks
			 (id, createdat, modifiedat, item_id, source, site, externalid, url, title, publishedat)
			 VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'tmdb', $2, $3, $4, $5, $6)`,
			itemID, v.Site, v.ExternalID, v.URL, v.Name, published)
	}
}

// persistArtwork stores the URL row + fetched bytes for an item/kind, keyed so a
// re-run updates rather than duplicates.
func (s *Service) persistArtwork(ctx context.Context, itemID, kind, url string) {
	var linked int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM com_nalet_katalog_itemartwork WHERE item_id = $1 AND kind = $2 AND url = $3`,
		itemID, kind, url).Scan(&linked); err == nil && linked == 0 {
		s.pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_itemartwork (id, item_id, kind, url) VALUES (gen_random_uuid()::varchar, $1, $2, $3)`,
			itemID, kind, url)
	}

	bytes, ok := s.tmdb.fetchImage(ctx, url)
	if !ok || len(bytes) == 0 {
		return
	}
	contentType := "image/jpeg"
	if strings.HasSuffix(url, ".png") {
		contentType = "image/png"
	}

	var existing int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM com_nalet_katalog_itemartworkdata WHERE item_id = $1 AND kind = $2`,
		itemID, kind).Scan(&existing); err != nil {
		return
	}
	if existing > 0 {
		s.pool.Exec(ctx,
			`UPDATE com_nalet_katalog_itemartworkdata SET contenttype = $1, bytes = $2, fetchedat = now() WHERE item_id = $3 AND kind = $4`,
			contentType, bytes, itemID, kind)
		return
	}
	s.pool.Exec(ctx,
		`INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes, fetchedat) VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, now())`,
		itemID, kind, contentType, bytes)
}

// ===================== chaptersdb sidecar (movies only, opt-in) =====================

var (
	creditsNameRE = regexp.MustCompile(`(?i)\b(end credits|closing credits|credits)\b`)
	introNameRE   = regexp.MustCompile(`(?i)\b(opening credits|opening titles|main titles|intro(?:duction)?|title sequence)\b`)
	recapNameRE   = regexp.MustCompile(`(?i)\b(recap|previously on)\b`)
)

func classifyChapterName(name string) string {
	if name == "" {
		return ""
	}
	if creditsNameRE.MatchString(name) {
		return "credits"
	}
	if introNameRE.MatchString(name) {
		return "intro"
	}
	if recapNameRE.MatchString(name) {
		return "recap"
	}
	return ""
}

func (s *Service) applyChaptersDb(ctx context.Context, itemID, title string, year *int, durationMs *int64) {
	if s.ch == nil || !s.ch.Enabled() {
		return
	}
	show, ok := s.ch.FindShow(ctx, "movie", year, title)
	if !ok || show == nil {
		return
	}
	entries := s.ch.GetMovieChapters(ctx, show.ID)
	if len(entries) == 0 {
		return
	}

	s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_mediasegments WHERE item_id = $1 AND source = 'chaptersdb'`, itemID)
	s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, itemID)

	for i := range entries {
		startMs := entries[i].StartMs
		var endMs int64
		if i+1 < len(entries) {
			endMs = entries[i+1].StartMs
		} else if durationMs != nil && *durationMs > startMs {
			endMs = *durationMs
		} else {
			endMs = startMs + 1000
		}
		if endMs <= startMs {
			continue
		}

		kind := classifyChapterName(entries[i].Name)
		label := entries[i].Name
		if len(label) > 120 {
			label = label[:120]
		}
		var labelArg *string
		if entries[i].Name != "" {
			labelArg = &label
		}
		if kind != "" {
			s.pool.Exec(ctx,
				`INSERT INTO com_nalet_katalog_mediasegments
				 (id, createdat, modifiedat, item_id, kind, startms, endms, source, confidence, label)
				 VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, 'chaptersdb', $5, $6)`,
				itemID, kind, startMs, endMs, 0.95, labelArg)
		} else {
			s.pool.Exec(ctx,
				`INSERT INTO com_nalet_katalog_itemchapters
				 (id, createdat, modifiedat, item_id, startms, endms, title, ordinal)
				 VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5)`,
				itemID, startMs, endMs, labelArg, i+1)
		}
	}
}

// ===================== status + parsing helpers =====================

// markStatus funnels enrichment transitions through the audit table, mapping
// not_found -> skipped (TMDB had no match; not a failure).
func (s *Service) markStatus(ctx context.Context, itemID, status string, errMsg *string) {
	mapped := status
	if status == "not_found" {
		mapped = processing.StatusSkipped
	}
	_ = s.steps.Upsert(ctx, itemID, "tmdb", mapped, errMsg, nil)
}

// parseYear extracts the year from an ISO date (YYYY-MM-DD). Returns nil on
// blank/parse error (matches LocalDate.parse(...).getYear()).
func parseYear(date string) *int {
	if strings.TrimSpace(date) == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return nil
	}
	y := t.Year()
	return &y
}

// parseTmdbTs parses an ISO-8601 timestamp; nil on blank/parse error.
func parseTmdbTs(iso string) *time.Time {
	if strings.TrimSpace(iso) == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return &t
	}
	// TMDB publishedAt sometimes carries a millis fraction and/or no offset.
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05.000-07:00",
	} {
		if t, err := time.Parse(layout, iso); err == nil {
			return &t
		}
	}
	return nil
}

// nullStr returns nil for an empty string so COALESCE keeps the existing value.
func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
