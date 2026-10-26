package rest

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/compliance"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// Compliance is the library's compliance check (compliance.Service): it keeps
// the rules the workers report they run with, checks a title again once its
// package completed or its pipeline ended, and writes its report as CSV.
type Compliance interface {
	ReportRules(ctx context.Context, worker string, body []byte) (compliance.Reported, error)
	Touch(ctx context.Context, ids ...string)
	// Export is the report's CSV of the titles f matches, in the order o.
	Export(ctx context.Context, f graph.ComplianceFilter, o graph.ComplianceOrder) (compliance.Export, error)
}

// putRules serves PUT /api/analyze/rules/{worker}, for the workers' service
// account and admins: the transcoder and the packager report the rules they
// run with, at their start and every ten minutes. The body is {"version":
// "<image commit or release>", "rules": {...}}: the transcoder's caps,
// 2160 bucket, encoder and ladders, the packager's 5.1 companion, audio bit
// rates and image subtitles (README "Library compliance"). It answers
// {worker, version, reportedAt, changed}, changed when the rules are not
// those the worker reported last: the titles are checked again against them.
// 400 for a worker that reports no rules here or a body that is no report of
// its rules (a value of the wrong kind, a negative cap); 503 without migration
// 051.
func (h *Handlers) putRules(w http.ResponseWriter, r *http.Request) {
	if h.d.Compliance == nil {
		writeError(w, http.StatusServiceUnavailable, "the library's compliance check is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	res, err := h.d.Compliance.ReportRules(reqCtx(r), chi.URLParam(r, "worker"), body)
	var bad *compliance.BadReport
	switch {
	case errors.Is(err, compliance.ErrUnknownWorker):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
		return
	case errors.Is(err, compliance.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the rules could not be kept: "+err.Error())
		return
	}
	res.ReportedAt = res.ReportedAt.UTC()
	writeJSON(w, http.StatusOK, res)
}

// touch has the compliance check look at the title itemID again: its package
// completed, or its transcode, package or take-in ended.
func (h *Handlers) touch(ctx context.Context, itemID string) {
	if h.d.Compliance != nil {
		h.d.Compliance.Touch(ctx, itemID)
	}
}

// ended reports whether status ends a step's run.
func ended(status string) bool {
	switch status {
	case processing.StatusDone, processing.StatusFailed, processing.StatusSkipped, processing.StatusNotApplicable:
		return true
	}
	return false
}
