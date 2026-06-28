package rest

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// getArtwork serves cached artwork bytes from com_nalet_katalog_itemartworkdata.
// Ports ArtworkController#get: first matching (item_id, kind) row, Content-Type
// from the row (default image/jpeg), Cache-Control public 7d; 404 empty body
// when no row, the bytes are absent, or zero-length. (stream-token/JWT auth is
// applied by middleware ahead of this handler.)
func (h *Handlers) getArtwork(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	kind := chi.URLParam(r, "kind")

	var contentType *string
	var bytes []byte
	err := h.d.Store.Pool().QueryRow(reqCtx(r),
		`SELECT contenttype, bytes FROM com_nalet_katalog_itemartworkdata
		 WHERE item_id = $1 AND kind = $2 LIMIT 1`, itemID, kind).Scan(&contentType, &bytes)
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
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(bytes)
}
