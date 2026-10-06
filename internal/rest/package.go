package rest

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
)

// Packager enqueues an item's packaging (itemactions).
type Packager interface {
	PackageItem(ctx context.Context, id string) (graph.PackageResult, error)
}

// postPackage serves POST /api/items/{id}/package, the admin action chino-api's
// admin packaging route forwards to (the CAP service's enqueuePackaging): it
// enqueues the item's packaging as GraphQL's packageItem does, and answers its
// result, {status, alreadyActive, message} for a movie or an episode and
// {episodesEnqueued, episodesTotal, message} for a series; 404 for an unknown
// item, 400 for one that cannot be packaged and 409 for a title whose
// original was deleted after packaging, with {"error": "..."}.
func (h *Handlers) postPackage(w http.ResponseWriter, r *http.Request) {
	if h.d.Packager == nil {
		writeError(w, http.StatusServiceUnavailable, "packaging is not configured")
		return
	}
	id := chi.URLParam(r, "id")
	res, err := h.d.Packager.PackageItem(reqCtx(r), id)
	switch {
	case errors.Is(err, itemactions.ErrUnknownItem):
		writeError(w, http.StatusNotFound, "unknown item: "+id)
		return
	case errors.Is(err, itemactions.ErrNotPackageable):
		writeError(w, http.StatusBadRequest, "Packaging is only available for movies and episodes.")
		return
	case errors.Is(err, itemactions.ErrRetired):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "packaging could not be enqueued: "+err.Error())
		return
	}
	body := map[string]any{}
	if res.Status != nil {
		body["status"] = *res.Status
	}
	if res.AlreadyActive != nil {
		body["alreadyActive"] = *res.AlreadyActive
	}
	if res.EpisodesEnqueued != nil {
		body["episodesEnqueued"] = *res.EpisodesEnqueued
	}
	if res.EpisodesTotal != nil {
		body["episodesTotal"] = *res.EpisodesTotal
	}
	if res.Message != nil {
		body["message"] = *res.Message
	}
	writeJSON(w, http.StatusOK, body)
}
