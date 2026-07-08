package rest

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// analyzerSteps are the per-file analyzer-owned steps (ANALYZER_STEPS in the
// Java controller). Ordered list (membership only matters for ANY()/reset).
var analyzerSteps = []string{"chapter", "chromaprint", "blackframe", "silence", "subtitle", "tidb"}

// allowedPasses mirrors AnalyzerController.ALLOWED_PASSES.
var allowedPasses = map[string]bool{
	"per_file": true, "tidb_first": true, "transcoder": true, "packager": true,
}

// claimItem is one row of the claim response. The JSON is hand-marshalled to
// reproduce the Java LinkedHashMap field order and the conditional presence of
// seriesTitle (present — possibly null — only for the packager pass, omitted
// otherwise, mirroring row.containsKey("series_title")).
type claimItem struct {
	ID            string
	Type          string
	Title         string
	Year          *int32
	DurationMs    *int64
	Path          *string
	SeasonNumber  *int32
	EpisodeNumber *int32
	SeriesTitle   *string
	includeSeries bool
	SeriesTmdbID  *string
	MovieTmdbID   *string
}

// MarshalJSON preserves key order and the conditional seriesTitle key.
func (c claimItem) MarshalJSON() ([]byte, error) {
	var b []byte
	b = append(b, '{')
	w := func(key string, val any, more bool) error {
		kb, _ := json.Marshal(key)
		vb, err := json.Marshal(val)
		if err != nil {
			return err
		}
		b = append(b, kb...)
		b = append(b, ':')
		b = append(b, vb...)
		if more {
			b = append(b, ',')
		}
		return nil
	}
	if err := w("id", c.ID, true); err != nil {
		return nil, err
	}
	_ = w("type", c.Type, true)
	_ = w("title", c.Title, true)
	_ = w("year", c.Year, true)
	_ = w("durationMs", c.DurationMs, true)
	_ = w("path", c.Path, true)
	_ = w("seasonNumber", c.SeasonNumber, true)
	_ = w("episodeNumber", c.EpisodeNumber, true)
	if c.includeSeries {
		_ = w("seriesTitle", c.SeriesTitle, true)
	}
	_ = w("seriesTmdbId", c.SeriesTmdbID, true)
	_ = w("movieTmdbId", c.MovieTmdbID, false)
	b = append(b, '}')
	return b, nil
}

// claim ports AnalyzerController#claim: atomic per-pass dequeue with FOR UPDATE
// SKIP LOCKED, returning {pass, claimed, items[]}. Non-per_file passes flip
// their owning step to in_progress at claim time.
func (h *Handlers) claim(w http.ResponseWriter, r *http.Request) {
	ctx := reqCtx(r)
	pass := r.URL.Query().Get("pass")
	if pass == "" {
		pass = "per_file"
	}
	if !allowedPasses[pass] {
		writeError(w, http.StatusBadRequest,
			"unknown pass '"+pass+"'; allowed: [per_file, tidb_first, transcoder, packager]")
		return
	}
	limit := 4
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	safeLimit := limit
	if safeLimit < 1 {
		safeLimit = 1
	}
	if safeLimit > 32 {
		safeLimit = 32
	}

	pool := h.d.Store.Pool()
	var rows pgx.Rows
	var err error
	packager := pass == "packager"

	switch pass {
	case "tidb_first":
		rows, err = pool.Query(ctx, `
			WITH next_items AS (
			  SELECT i.id
			  FROM com_nalet_katalog_items i
			  WHERE i.type IN ('movie','episode')
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemexternalids x
			                WHERE x.source='tmdb'
			                  AND ((i.type='movie'   AND x.item_id = i.id)
			                    OR (i.type='episode' AND x.item_id = i.parent_id)))
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                WHERE s.item_id = i.id AND s.step='tidb' AND s.status='pending')
			    AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                    WHERE s.item_id = i.id AND s.status = 'in_progress')
			  ORDER BY i.createdat DESC NULLS LAST
			  FOR UPDATE SKIP LOCKED
			  LIMIT $1
			)
			SELECT i.id, i.type, i.title, i.year, i.durationms, NULL::text AS path,
			       i.seasonnumber, i.episodenumber,
			       parent_ext.externalid AS series_tmdb_id,
			       self_ext.externalid   AS movie_tmdb_id
			FROM next_items n
			JOIN com_nalet_katalog_items i ON i.id = n.id
			LEFT JOIN com_nalet_katalog_itemexternalids parent_ext
			       ON parent_ext.item_id = i.parent_id AND parent_ext.source = 'tmdb'
			LEFT JOIN com_nalet_katalog_itemexternalids self_ext
			       ON self_ext.item_id = i.id AND self_ext.source = 'tmdb'`, safeLimit)
	case "transcoder":
		rows, err = pool.Query(ctx, `
			WITH next_items AS (
			  SELECT i.id
			  FROM com_nalet_katalog_items i
			  WHERE i.type IN ('movie','episode')
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p
			                WHERE p.item_id = i.id AND p.isprimary = true)
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                WHERE s.item_id = i.id AND s.step='transcode' AND s.status='pending')
			    AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                    WHERE s.item_id = i.id AND s.status = 'in_progress')
			  ORDER BY i.createdat DESC NULLS LAST
			  FOR UPDATE SKIP LOCKED
			  LIMIT $1
			)
			SELECT i.id, i.type, i.title, i.year, i.durationms, p.path,
			       i.seasonnumber, i.episodenumber,
			       parent_ext.externalid AS series_tmdb_id,
			       self_ext.externalid   AS movie_tmdb_id
			FROM next_items n
			JOIN com_nalet_katalog_items i ON i.id = n.id
			JOIN com_nalet_katalog_playbackassets p ON p.item_id = i.id AND p.isprimary = true
			LEFT JOIN com_nalet_katalog_itemexternalids parent_ext
			       ON parent_ext.item_id = i.parent_id AND parent_ext.source = 'tmdb'
			LEFT JOIN com_nalet_katalog_itemexternalids self_ext
			       ON self_ext.item_id = i.id AND self_ext.source = 'tmdb'`, safeLimit)
	case "packager":
		rows, err = pool.Query(ctx, `
			WITH next_items AS (
			  SELECT i.id
			  FROM com_nalet_katalog_items i
			  WHERE i.type IN ('movie','episode')
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p
			                WHERE p.item_id = i.id AND p.isprimary = true)
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                WHERE s.item_id = i.id AND s.step='package' AND s.status='pending')
			    AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                    WHERE s.item_id = i.id AND s.status = 'in_progress')
			    AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                    WHERE s.item_id = i.id AND s.step='transcode' AND s.status='pending')
			  ORDER BY i.createdat DESC NULLS LAST
			  FOR UPDATE SKIP LOCKED
			  LIMIT $1
			)
			SELECT i.id, i.type, i.title, i.year, i.durationms, p.path,
			       i.seasonnumber, i.episodenumber,
			       parent_item.title     AS series_title,
			       parent_ext.externalid AS series_tmdb_id,
			       self_ext.externalid   AS movie_tmdb_id
			FROM next_items n
			JOIN com_nalet_katalog_items i ON i.id = n.id
			JOIN com_nalet_katalog_playbackassets p ON p.item_id = i.id AND p.isprimary = true
			LEFT JOIN com_nalet_katalog_items parent_item
			       ON parent_item.id = i.parent_id
			LEFT JOIN com_nalet_katalog_itemexternalids parent_ext
			       ON parent_ext.item_id = i.parent_id AND parent_ext.source = 'tmdb'
			LEFT JOIN com_nalet_katalog_itemexternalids self_ext
			       ON self_ext.item_id = i.id AND self_ext.source = 'tmdb'`, safeLimit)
	default: // per_file
		rows, err = pool.Query(ctx, `
			WITH next_items AS (
			  SELECT i.id
			  FROM com_nalet_katalog_items i
			  WHERE i.type IN ('movie','episode')
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p
			                WHERE p.item_id = i.id AND p.isprimary = true)
			    AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                WHERE s.item_id = i.id AND s.status = 'pending' AND s.step = ANY($1))
			    AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps s
			                    WHERE s.item_id = i.id AND s.status = 'in_progress')
			  ORDER BY i.createdat DESC NULLS LAST
			  FOR UPDATE SKIP LOCKED
			  LIMIT $2
			)
			SELECT i.id, i.type, i.title, i.year, i.durationms, p.path,
			       i.seasonnumber, i.episodenumber,
			       parent_ext.externalid AS series_tmdb_id,
			       self_ext.externalid   AS movie_tmdb_id
			FROM next_items n
			JOIN com_nalet_katalog_items i ON i.id = n.id
			JOIN com_nalet_katalog_playbackassets p ON p.item_id = i.id AND p.isprimary = true
			LEFT JOIN com_nalet_katalog_itemexternalids parent_ext
			       ON parent_ext.item_id = i.parent_id AND parent_ext.source = 'tmdb'
			LEFT JOIN com_nalet_katalog_itemexternalids self_ext
			       ON self_ext.item_id = i.id AND self_ext.source = 'tmdb'`, analyzerSteps, safeLimit)
	}
	if err != nil {
		http.Error(w, "claim query failed", http.StatusInternalServerError)
		return
	}

	items := make([]claimItem, 0)
	func() {
		defer rows.Close()
		for rows.Next() {
			it := claimItem{includeSeries: packager}
			var scanErr error
			if packager {
				scanErr = rows.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.DurationMs, &it.Path,
					&it.SeasonNumber, &it.EpisodeNumber, &it.SeriesTitle, &it.SeriesTmdbID, &it.MovieTmdbID)
			} else {
				scanErr = rows.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.DurationMs, &it.Path,
					&it.SeasonNumber, &it.EpisodeNumber, &it.SeriesTmdbID, &it.MovieTmdbID)
			}
			if scanErr != nil {
				err = scanErr
				return
			}
			items = append(items, it)
		}
		err = rows.Err()
	}()
	if err != nil {
		http.Error(w, "claim scan failed", http.StatusInternalServerError)
		return
	}

	// Claim-time step flip for the non-per_file passes (per_file relies on the
	// worker flipping its first detector step itself).
	var flipStep string
	switch pass {
	case "tidb_first":
		flipStep = "tidb"
	case "transcoder":
		flipStep = "transcode"
	case "packager":
		flipStep = "package"
	}
	if flipStep != "" {
		for i := range items {
			_ = h.d.Steps.Upsert(ctx, items[i].ID, flipStep, processing.StatusInProgress, nil, nil)
		}
	}

	writeJSON(w, http.StatusOK, claimResponse{Pass: pass, Claimed: len(items), Items: items})
}

type claimResponse struct {
	Pass    string      `json:"pass"`
	Claimed int         `json:"claimed"`
	Items   []claimItem `json:"items"`
}

// getAnalyzeItem ports AnalyzerController#getItem: single-item lookup joined to
// its primary playback path. 404 when there is no primary asset.
func (h *Handlers) getAnalyzeItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var (
		typ, title string
		year       *int32
		durationMs *int64
		path       *string
	)
	err := h.d.Store.Pool().QueryRow(reqCtx(r), `
		SELECT i.id, i.type, i.title, i.year, i.durationms, p.path
		FROM com_nalet_katalog_items i
		JOIN com_nalet_katalog_playbackassets p ON p.item_id = i.id AND p.isprimary = true
		WHERE i.id = $1 LIMIT 1`, id).Scan(&id, &typ, &title, &year, &durationMs, &path)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, analyzeItemView{
		ID: id, Type: typ, Title: title, Year: year, DurationMs: durationMs, Path: path,
	})
}

type analyzeItemView struct {
	ID         string  `json:"id"`
	Type       string  `json:"type"`
	Title      string  `json:"title"`
	Year       *int32  `json:"year"`
	DurationMs *int64  `json:"durationMs"`
	Path       *string `json:"path"`
}

// getSteps ports AnalyzerController#stepsForItem: {itemId, steps:{step:status}}.
func (h *Handlers) getSteps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	rows, err := h.d.Store.Pool().Query(reqCtx(r),
		`SELECT step, status FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, id)
	if err != nil {
		http.Error(w, "steps lookup failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	steps := map[string]string{}
	for rows.Next() {
		var step, status string
		if err := rows.Scan(&step, &status); err != nil {
			http.Error(w, "steps scan failed", http.StatusInternalServerError)
			return
		}
		steps[step] = status
	}
	if rows.Err() != nil {
		http.Error(w, "steps scan failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": id, "steps": steps})
}

// skipSteps ports AnalyzerController#markStepsNotApplicable: bulk-mark steps
// not_applicable. Missing/empty steps array -> 400; unknown step names are
// silently skipped (the batch continues).
func (h *Handlers) skipSteps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var body struct {
		Steps  []string `json:"steps"`
		Reason *string  `json:"reason"`
	}
	if err := decodeJSON(r, &body); err != nil {
		// Java treats a null body the same as a missing steps array.
		writeError(w, http.StatusBadRequest, "missing 'steps' array")
		return
	}
	if len(body.Steps) == 0 {
		writeError(w, http.StatusBadRequest, "missing 'steps' array")
		return
	}
	reason := "tidb_first short-circuited the per_file pipeline"
	if body.Reason != nil && trimBlank(*body.Reason) != "" {
		reason = *body.Reason
	}
	ctx := reqCtx(r)
	updated := 0
	for _, step := range body.Steps {
		if trimBlank(step) == "" {
			continue
		}
		if err := h.d.Steps.Upsert(ctx, id, step, processing.StatusNotApplicable, &reason, nil); err != nil {
			// Unknown step (ErrBadStep) is swallowed; a real DB error also just
			// drops this entry, matching the Java catch-and-continue semantics.
			continue
		}
		updated++
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": id, "updated": updated})
}

// getSiblings ports AnalyzerController#siblings: same-series same-season episode
// siblings with primary paths, limit clamped [1,12].
func (h *Handlers) getSiblings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 12 {
		limit = 12
	}
	rows, err := h.d.Store.Pool().Query(reqCtx(r), `
		SELECT s.id, s.type, s.title, s.year, s.durationms, p.path
		FROM com_nalet_katalog_items me
		JOIN com_nalet_katalog_items s ON s.parent_id = me.parent_id
		  AND s.id <> me.id
		  AND s.seasonnumber = me.seasonnumber
		  AND s.type = 'episode'
		JOIN com_nalet_katalog_playbackassets p ON p.item_id = s.id AND p.isprimary = true
		WHERE me.id = $1
		ORDER BY s.episodenumber NULLS LAST
		LIMIT $2`, id, limit)
	if err != nil {
		http.Error(w, "siblings query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	items := make([]analyzeItemView, 0)
	for rows.Next() {
		var it analyzeItemView
		if err := rows.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.DurationMs, &it.Path); err != nil {
			http.Error(w, "siblings scan failed", http.StatusInternalServerError)
			return
		}
		items = append(items, it)
	}
	if rows.Err() != nil {
		http.Error(w, "siblings scan failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": id, "items": items})
}

// resetSeries ports AnalyzerController#resetSeries: bump episode createdat, reset
// analyzer steps to pending (attempts preserved), purge chromaprint segments.
func (h *Handlers) resetSeries(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := reqCtx(r)
	pool := h.d.Store.Pool()

	tag, err := pool.Exec(ctx,
		`UPDATE com_nalet_katalog_items SET createdat = now()
		 WHERE parent_id = $1 AND type = 'episode'`, id)
	if err != nil {
		http.Error(w, "series reset (bump) failed", http.StatusInternalServerError)
		return
	}
	episodes := tag.RowsAffected()

	epRows, err := pool.Query(ctx,
		`SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1 AND type = 'episode'`, id)
	if err != nil {
		http.Error(w, "series reset (episode list) failed", http.StatusInternalServerError)
		return
	}
	var episodeIDs []string
	for epRows.Next() {
		var eid string
		if err := epRows.Scan(&eid); err != nil {
			epRows.Close()
			http.Error(w, "series reset (episode scan) failed", http.StatusInternalServerError)
			return
		}
		episodeIDs = append(episodeIDs, eid)
	}
	epRows.Close()
	if epRows.Err() != nil {
		http.Error(w, "series reset (episode scan) failed", http.StatusInternalServerError)
		return
	}

	stepsReset, err := h.d.Steps.ResetForItems(ctx, episodeIDs, analyzerSteps)
	if err != nil {
		http.Error(w, "series reset (steps) failed", http.StatusInternalServerError)
		return
	}

	segTag, err := pool.Exec(ctx, `
		DELETE FROM com_nalet_katalog_mediasegments
		WHERE source = 'chromaprint' AND item_id IN (
		  SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1 AND type = 'episode')`, id)
	if err != nil {
		http.Error(w, "series reset (segments) failed", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"seriesId":       id,
		"episodes":       episodes,
		"stepsReset":     stepsReset,
		"segmentsPurged": segTag.RowsAffected(),
	})
}

// failItem ports AnalyzerController#fail: attribute the failure to the synthetic
// scan step, then skip remaining pending/in_progress analyzer steps.
func (h *Handlers) failItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := reqCtx(r)

	var body struct {
		Reason *string `json:"reason"`
	}
	_ = decodeJSON(r, &body) // body optional
	reason := "unspecified analyzer error"
	if body.Reason != nil && trimBlank(*body.Reason) != "" {
		reason = *body.Reason
	}

	if err := h.d.Steps.Upsert(ctx, id, "scan", processing.StatusFailed, &reason, nil); err != nil {
		if errors.Is(err, processing.ErrBadStep) || errors.Is(err, processing.ErrBadStatus) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "step upsert affected 0 rows")
		return
	}

	tag, err := h.d.Store.Pool().Exec(ctx, `
		UPDATE com_nalet_katalog_itemprocessingsteps
		SET status = 'skipped', finishedat = COALESCE(finishedat, now()), modifiedat = now()
		WHERE item_id = $1 AND status IN ('pending','in_progress') AND step = ANY($2)`,
		id, analyzerSteps)
	if err != nil {
		http.Error(w, "fail (skip steps) failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"itemId": id, "status": "failed", "stepsSkipped": tag.RowsAffected(),
	})
}

// putStep ports AnalyzerController#upsertStep: the PRIMARY worker step-status
// write, with the transcode->package chain promotion.
func (h *Handlers) putStep(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	step := chi.URLParam(r, "step")
	ctx := reqCtx(r)

	var body struct {
		Status  *string `json:"status"`
		Error   *string `json:"error"`
		Details *string `json:"details"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	if body.Status == nil || trimBlank(*body.Status) == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	status := *body.Status

	if err := h.d.Steps.Upsert(ctx, id, step, status, body.Error, body.Details); err != nil {
		if errors.Is(err, processing.ErrBadStep) || errors.Is(err, processing.ErrBadStatus) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		// Surface the real cause (and log it): a masked "0 rows" string once hid a
		// pgx type-deduction failure that stalled the whole analyzer pipeline.
		log.Printf("putStep: upsert item=%s step=%s status=%s failed: %v", id, step, status, err)
		writeError(w, http.StatusInternalServerError, "step upsert failed: "+err.Error())
		return
	}

	// Chain promotion: transcode -> package (best-effort; failure swallowed).
	if step == "transcode" {
		switch status {
		case processing.StatusDone, processing.StatusNotApplicable, processing.StatusSkipped:
			_ = h.d.Steps.PromoteTranscodeToPackage(ctx, id, status)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"itemId": id, "step": step, "status": status})
}

// decodeJSON decodes the request body using json.Number for numbers so the
// lenient long/double coercions in segments/chapters behave like Jackson.
func decodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return errEmptyBody
	}
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	return dec.Decode(v)
}

var errEmptyBody = errors.New("empty body")

func trimBlank(s string) string {
	// Java's isBlank() trims Unicode whitespace; ASCII trim is sufficient here.
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
