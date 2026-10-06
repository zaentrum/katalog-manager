package rest

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/sourceprobe"
)

// analyzerSteps are the per-file analyzer-owned steps (ANALYZER_STEPS in the
// Java controller). Ordered list (membership only matters for ANY()/reset).
var analyzerSteps = []string{"chapter", "chromaprint", "blackframe", "silence", "subtitle", "tidb"}

// claimItem is one per-item analyzer detail row (returned by getAnalyzeItem).
// The JSON is hand-marshalled to reproduce the Java LinkedHashMap field order
// and the conditional presence of seriesTitle.
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
	// Whether the item has its OWN artwork of each kind (ignoring the series
	// fallback), so the analyzer only extracts a keyframe for genuine gaps.
	HasOwnPoster   bool
	HasOwnBackdrop bool
	// The languages an admin set for the source's tracks, which the packager
	// labels them with, and the subtitle files beside the source, which it
	// packages as subtitles; each key left out when there is none, as a
	// packager that reads neither expects.
	TrackLanguages []trackLanguage
	SubtitleFiles  []subtitleFile
	// With the setting library.layout=v2, where the item's records go and
	// what the run works on; left out otherwise, and the workers keep their
	// legacy ways.
	Library *itemLibrary
}

// trackLanguage is an admin's language of a track of the item's source: kind
// audio or subtitle, ordinal its place among the source's streams of the
// kind in ffprobe's order (0 first), language an ISO 639-2 code.
type trackLanguage struct {
	Kind     string `json:"kind"`
	Ordinal  int32  `json:"ordinal"`
	Language string `json:"language"`
}

// subtitleFile is a subtitle file the scanner found beside the source
// (<video>.<lang>.srt|vtt|ass|ssa): its absolute path on the library
// storage, as the workers see it too, its language as an ISO 639-2 code (und
// when its name gives none), its label, and whether it is forced (a JSON
// boolean; the scanner records no file forced). The packager converts it to
// WebVTT and packages it after the source's own subtitles, never as the
// default unless forced.
type subtitleFile struct {
	ID       string `json:"id"` // its subtitle asset's id, which packaging-complete maps the file's rendition back to
	Path     string `json:"path"`
	Language string `json:"language"`
	Label    string `json:"label"`
	Forced   bool   `json:"forced"`
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
	_ = w("movieTmdbId", c.MovieTmdbID, true)
	_ = w("hasOwnPoster", c.HasOwnPoster, true)
	_ = w("hasOwnBackdrop", c.HasOwnBackdrop, false)
	if len(c.TrackLanguages) > 0 {
		b = append(b, ',')
		if err := w("trackLanguages", c.TrackLanguages, false); err != nil {
			return nil, err
		}
	}
	if len(c.SubtitleFiles) > 0 {
		b = append(b, ',')
		if err := w("subtitleFiles", c.SubtitleFiles, false); err != nil {
			return nil, err
		}
	}
	if c.Library != nil {
		b = append(b, ',')
		if err := w("library", c.Library, false); err != nil {
			return nil, err
		}
	}
	b = append(b, '}')
	return b, nil
}

// getAnalyzeItem is the single-item detail lookup the event-driven workers use.
// Since the pipeline is now Kafka-triggered (no batch claim), a worker consumes
// an item event and fetches the FULL per-item detail by id here: primary path,
// season/episode coords, parent series title, and the TMDB ids the tidb pass and
// output naming need. Returns the same rich shape the old batch claim returned
// (claimItem with seriesTitle), so workers get everything in one call. 404 when
// there is no primary asset. The record names the languages an admin set for
// the source's tracks (trackLanguages) and the subtitle files beside it
// (subtitleFiles), the packager's to label and package.
func (h *Handlers) getAnalyzeItem(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	it := claimItem{includeSeries: true}
	err := h.d.Store.Pool().QueryRow(reqCtx(r), `
		SELECT i.id, i.type, i.title, i.year, i.durationms, p.path,
		       i.seasonnumber, i.episodenumber,
		       parent_item.title     AS series_title,
		       parent_ext.externalid AS series_tmdb_id,
		       self_ext.externalid   AS movie_tmdb_id,
		       EXISTS(SELECT 1 FROM com_nalet_katalog_itemartworkdata a WHERE a.item_id = i.id AND a.kind = 'poster')   AS has_own_poster,
		       EXISTS(SELECT 1 FROM com_nalet_katalog_itemartworkdata a WHERE a.item_id = i.id AND a.kind = 'backdrop') AS has_own_backdrop
		FROM com_nalet_katalog_items i
		JOIN com_nalet_katalog_playbackassets p ON p.item_id = i.id AND p.isprimary = true
		LEFT JOIN com_nalet_katalog_items parent_item ON parent_item.id = i.parent_id
		LEFT JOIN com_nalet_katalog_itemexternalids parent_ext ON parent_ext.item_id = i.parent_id AND parent_ext.source = 'tmdb'
		LEFT JOIN com_nalet_katalog_itemexternalids self_ext ON self_ext.item_id = i.id AND self_ext.source = 'tmdb'
		WHERE i.id = $1 LIMIT 1`, id).Scan(
		&it.ID, &it.Type, &it.Title, &it.Year, &it.DurationMs, &it.Path,
		&it.SeasonNumber, &it.EpisodeNumber, &it.SeriesTitle, &it.SeriesTmdbID, &it.MovieTmdbID,
		&it.HasOwnPoster, &it.HasOwnBackdrop)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	// A packager handed a record without them would label and package the
	// title wrongly, so a failure to read them fails the lookup: the worker
	// asks again.
	if it.TrackLanguages, err = h.trackLanguagesOf(reqCtx(r), it.ID); err != nil {
		log.Printf("getAnalyzeItem: the track languages of %s: %v", it.ID, err)
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	source := ""
	if it.Path != nil {
		source = *it.Path
	}
	if it.SubtitleFiles, err = h.subtitleFilesOf(reqCtx(r), it.ID, source); err != nil {
		log.Printf("getAnalyzeItem: the subtitle files of %s: %v", it.ID, err)
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	set, err := h.settings(reqCtx(r))
	if err != nil {
		log.Printf("getAnalyzeItem: the library's settings: %v", err)
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	if set.V2() {
		if it.Library, err = h.itemLibraryOf(reqCtx(r), it.ID); err != nil {
			log.Printf("getAnalyzeItem: the library of %s: %v", it.ID, err)
			http.Error(w, "item lookup failed", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, it)
}

// trackLanguagesOf are the languages an admin set for the tracks of the
// item's source, as the worker record names them; none on a catalog without
// migration 037.
func (h *Handlers) trackLanguagesOf(ctx context.Context, itemID string) ([]trackLanguage, error) {
	ls, err := h.d.Store.TrackLanguages(ctx, itemID)
	if err != nil {
		return nil, err
	}
	out := make([]trackLanguage, 0, len(ls))
	for _, l := range ls {
		out = append(out, trackLanguage{Kind: l.Kind, Ordinal: l.Ordinal, Language: l.Language})
	}
	return out, nil
}

// sidecarSubtitle reports whether an item's subtitle at path is a file beside
// its source, as the scanner records them, rather than one of its package's:
// it is under neither the packages root nor root, the item's package, where
// packaging-complete writes the package's subtitles.
func (h *Handlers) sidecarSubtitle(path, root string) bool {
	return !underRoot(h.d.Cfg.PackagesRoot, path) && !underRoot(root, path)
}

// subtitleFileSuffixes are the subtitle files the packager takes, by their
// extension; maxSubtitleFileBytes is the most one may hold, as the packager
// reads a file whole and takes a larger one for no subtitle file.
var subtitleFileSuffixes = map[string]bool{".srt": true, ".vtt": true, ".ass": true, ".ssa": true}

const maxSubtitleFileBytes = 50 << 20

// subtitleFilesOf are the subtitle files beside the item's source, at source,
// by path, as the packager takes them: the item's subtitles that are no
// package's (sidecarSubtitle), each at an absolute path in the source's
// folder or below it, a .srt, .vtt, .ass or .ssa file of at most 50 MB. A
// file that is gone, or larger, is left out.
func (h *Handlers) subtitleFilesOf(ctx context.Context, itemID, source string) ([]subtitleFile, error) {
	if strings.TrimSpace(source) == "" {
		return nil, nil
	}
	folder := filepath.Dir(filepath.Clean(source))
	root, err := h.packageRootFor(ctx, itemID)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Store.Pool().Query(ctx, `SELECT id, path, COALESCE(lang, ''), COALESCE(label, '')
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subtitleFile
	for rows.Next() {
		var id, path, lang, label string
		if err := rows.Scan(&id, &path, &lang, &label); err != nil {
			return nil, err
		}
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) || !underRoot(folder, path) || !h.sidecarSubtitle(path, root) ||
			!subtitleFileSuffixes[strings.ToLower(filepath.Ext(path))] {
			continue
		}
		if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxSubtitleFileBytes {
			continue
		}
		out = append(out, subtitleFile{ID: id, Path: path, Language: languages.ISO6392(lang), Label: strings.TrimSpace(label)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// analyzeItemView is the lightweight sibling-episode shape returned by
// getSiblings (id/type/title/year/duration/path only — no TMDB/series joins).
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
	// With the v2 layout the package's run builds a version, made now.
	if step == "transcode" {
		switch status {
		case processing.StatusDone, processing.StatusNotApplicable, processing.StatusSkipped:
			_ = h.d.Steps.PromoteTranscodeToPackage(ctx, id, status)
			h.mintVersion(ctx, id)
		}
		// The transcoder probed the source before it planned, and says its
		// codec and resolution in the details: the source asset keeps them
		// (best-effort, as the promotion).
		if (status == processing.StatusDone || status == processing.StatusNotApplicable) && body.Details != nil {
			if _, err := sourceprobe.Fill(ctx, h.d.Store.Pool(), id, sourceprobe.FromTranscodeDetails(*body.Details)); err != nil {
				log.Printf("putStep: %v", err)
			}
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
