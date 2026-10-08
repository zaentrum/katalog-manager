package rest

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/retry"
)

// TakeIn takes titles in without a package (retry.Service): those that get
// none now, and one an admin names.
type TakeIn interface {
	TakeIn(ctx context.Context, ids []string) (int, error)
	TakeInItem(ctx context.Context, id string) (retry.TakeInResult, error)
}

// postTakeIn serves POST /api/items/{id}/takein, an admin's take-in of a
// title with the library's v2 layout: its original goes into a version of
// its own, with no package, from which the title plays as one without a
// package does, whether or not its transcode or its package failed (see
// retry.TakeInItem). It answers {itemId, sent, message}: sent is false, and
// the message says why, for a title whose source has a version already,
// whose original something reads, or whose take-in waits, runs or failed,
// and while no title can be taken in (the legacy layout, migration 044
// missing, no event bus).
func (h *Handlers) postTakeIn(w http.ResponseWriter, r *http.Request) {
	if h.d.TakeIn == nil {
		writeError(w, http.StatusServiceUnavailable, "the take-in is not configured")
		return
	}
	res, err := h.d.TakeIn.TakeInItem(reqCtx(r), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the take-in: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
