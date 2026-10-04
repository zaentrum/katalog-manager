package rest

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// artworkCaching is how long a client may keep an image it was served.
const artworkCaching = "public, max-age=604800"

// maxArtworkBytes caps an uploaded artwork image (an analyzer-extracted keyframe
// is well under this; the limit just bounds a hostile/oversized upload).
const maxArtworkBytes = 25 << 20 // 25 MiB

// putArtwork stores uploaded artwork bytes for an item/kind (poster|backdrop) —
// used by the analyzer to submit a self-extracted keyframe for items TMDB and
// fanart.tv had no image for. Upserts the bytes into itemartworkdata and a marker
// url row into itemartwork. Body = raw image; Content-Type sets the stored type.
// 400 for a bad kind / empty body, 404 for an unknown item, 204 on success.
func (h *Handlers) putArtwork(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	kind := chi.URLParam(r, "kind")
	if kind != "poster" && kind != "backdrop" {
		http.Error(w, "kind must be poster or backdrop", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxArtworkBytes))
	if err != nil || len(body) == 0 {
		http.Error(w, "empty or unreadable body", http.StatusBadRequest)
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}

	ctx := reqCtx(r)
	pool := h.d.Store.Pool()

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM com_nalet_katalog_items WHERE id = $1)`, itemID).Scan(&exists); err != nil || !exists {
		http.NotFound(w, r)
		return
	}

	// Upsert the bytes (one row per item+kind).
	tag, err := pool.Exec(ctx,
		`UPDATE com_nalet_katalog_itemartworkdata SET contenttype = $1, bytes = $2, fetchedat = now()
		 WHERE item_id = $3 AND kind = $4`, ct, body, itemID, kind)
	if err != nil {
		http.Error(w, "artwork store failed", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes, fetchedat)
			 VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, now())`, itemID, kind, ct, body); err != nil {
			http.Error(w, "artwork store failed", http.StatusInternalServerError)
			return
		}
	}
	// Marker url row for provenance / url-based "has artwork" checks (idempotent).
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM com_nalet_katalog_itemartwork WHERE item_id = $1 AND kind = $2`,
		itemID, kind).Scan(&n); err == nil && n == 0 {
		pool.Exec(ctx,
			`INSERT INTO com_nalet_katalog_itemartwork (id, item_id, kind, url)
			 VALUES (gen_random_uuid()::varchar, $1, $2, 'extracted:keyframe')`, itemID, kind)
	}
	w.WriteHeader(http.StatusNoContent)
}

// getArtwork serves cached artwork bytes from com_nalet_katalog_itemartworkdata.
// Ports ArtworkController#get: first matching (item_id, kind) row, Content-Type
// from the row (default image/jpeg), Cache-Control public 7d; 404 empty body
// when no row, the bytes are absent, or zero-length. (stream-token/JWT auth is
// applied by middleware ahead of this handler.)
//
// Fallback: an episode without its own artwork of this kind (e.g. TMDB has no
// still for the episode) inherits its series parent's artwork, so episode tiles /
// backdrops render the show image instead of a blank. Own artwork always wins;
// items with no parent (movies) are unaffected.
//
// A viewer capped at an age is answered 404 for a title it may not be served,
// as for one without artwork (ratings.go).
func (h *Handlers) getArtwork(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	kind := chi.URLParam(r, "kind")
	if hidden, err := h.hiddenFrom(r, itemID); err != nil {
		http.Error(w, "artwork lookup failed", http.StatusInternalServerError)
		return
	} else if hidden { // as for a title without artwork
		http.NotFound(w, r)
		return
	}

	var contentType *string
	var bytes []byte
	err := h.d.Store.Pool().QueryRow(reqCtx(r),
		`SELECT a.contenttype, a.bytes FROM com_nalet_katalog_itemartworkdata a
		 WHERE a.kind = $2
		   AND (a.item_id = $1
		        OR a.item_id = (SELECT parent_id FROM com_nalet_katalog_items WHERE id = $1))
		 ORDER BY (a.item_id = $1) DESC
		 LIMIT 1`, itemID, kind).Scan(&contentType, &bytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "artwork lookup failed", http.StatusInternalServerError)
		return
	}
	if len(bytes) == 0 {
		http.NotFound(w, r)
		return
	}
	ct := "image/jpeg"
	if contentType != nil && *contentType != "" {
		ct = *contentType
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", artworkCaching)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
}

// getPersonProfile serves a person's primary profile portrait from
// com_nalet_katalog_personartwork: its bytes, with their content type, an ETag
// of their sha256 and the caching of a title's artwork; 304 when the caller
// holds them already (If-None-Match names the ETag), 404 when the person has
// none, and on a catalog that keeps no images of people (older than migration
// 030). It sits beside a title's artwork, so it takes what that takes: a
// bearer JWT, or a ?stream= token.
func (h *Handlers) getPersonProfile(w http.ResponseWriter, r *http.Request) {
	var contentType, sum string
	var bytes []byte
	err := h.d.Store.Pool().QueryRow(reqCtx(r), `SELECT contenttype, sha256, bytes FROM com_nalet_katalog_personartwork
		WHERE person_id = $1 AND kind = 'profile' AND isprimary`, chi.URLParam(r, "personId")).Scan(&contentType, &sum, &bytes)
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.As(err, &pe) && pe.Code == "42P01": // no image, or no table of them
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "portrait lookup failed", http.StatusInternalServerError)
		return
	case len(bytes) == 0:
		http.NotFound(w, r)
		return
	}
	etag := `"` + strings.TrimSpace(sum) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", artworkCaching)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if contentType = strings.TrimSpace(contentType); contentType == "" {
		contentType = "image/jpeg"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
}

// etagMatches reports whether an If-None-Match header names etag, compared
// weakly as a GET's is; "*" names any.
func etagMatches(header, etag string) bool {
	for _, tag := range strings.Split(header, ",") {
		if tag = strings.TrimSpace(tag); tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}
