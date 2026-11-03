package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/picture"
)

// PictureAreas are the titles' picture areas and their backfill
// (picture.Service): the analyzer reads a title's, reports what it measured,
// and asks the backfill for the titles to measure.
type PictureAreas interface {
	Of(ctx context.Context, kind, id string) (*picture.Area, error)
	Record(ctx context.Context, kind, id string, r picture.Report) (*picture.Area, error)
	Claim(ctx context.Context, worker string, limit int, method string) (picture.Claim, error)
}

// pictureAreaJSON is a title's picture area on the wire.
type pictureAreaJSON struct {
	ID            string          `json:"id"`
	Kind          string          `json:"kind"`
	ItemID        string          `json:"itemId"`
	State         string          `json:"state"`
	Current       bool            `json:"current"`
	Frame         *picture.Size   `json:"frame"`
	Area          *picture.Rect   `json:"area"`
	Path          *string         `json:"path"`
	SourceID      *string         `json:"sourceId"`
	VersionID     *string         `json:"versionId"`
	PackageID     *string         `json:"packageId"`
	MeasuredOn    *string         `json:"measuredOn"`
	FileSize      *int64          `json:"fileSize"`
	Method        *string         `json:"method"`
	Samples       *int32          `json:"samples"`
	Informative   *int32          `json:"informative"`
	BytesRead     *int64          `json:"bytesRead"`
	Reason        *string         `json:"reason"`
	Evidence      json.RawMessage `json:"evidence,omitempty"`
	Sampling      json.RawMessage `json:"sampling,omitempty"`
	MeasuredAt    *time.Time      `json:"measuredAt"`
	Attempts      int32           `json:"attempts"`
	Failures      int32           `json:"failures"`
	Releases      int32           `json:"releases"`
	NextAttemptAt *time.Time      `json:"nextAttemptAt"`
	ClaimedAt     *time.Time      `json:"claimedAt"`
}

func pictureAreaOut(a *picture.Area) pictureAreaJSON {
	out := pictureAreaJSON{ID: a.ID, Kind: a.Kind, ItemID: a.ItemID, State: a.State, Current: a.Current,
		Path: a.Path, SourceID: a.SourceID, VersionID: a.VersionID, PackageID: a.PackageID,
		MeasuredOn: a.MeasuredOn, FileSize: a.FileSize, Method: a.Method, Samples: a.Samples,
		Informative: a.Informative, BytesRead: a.BytesRead, Reason: a.Reason, Evidence: a.Evidence,
		Sampling: a.Sampling, MeasuredAt: utc(a.MeasuredAt), Attempts: a.Attempts, Failures: a.Failures,
		Releases: a.Releases, NextAttemptAt: utc(a.NextAttemptAt), ClaimedAt: utc(a.ClaimedAt)}
	if a.FrameWidth != nil && a.FrameHeight != nil {
		out.Frame = &picture.Size{Width: *a.FrameWidth, Height: *a.FrameHeight}
	}
	if a.State == picture.StateMeasured && a.X != nil && a.Y != nil && a.Width != nil && a.Height != nil {
		out.Area = &picture.Rect{X: *a.X, Y: *a.Y, Width: *a.Width, Height: *a.Height}
	}
	return out
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// pictureKind is the kind of title a route's {kind} segment names: items or
// extras.
func pictureKind(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/api/picture/extras/") {
		return picture.KindExtra
	}
	return picture.KindItem
}

// getPictureArea serves GET /api/picture/items/{id} and
// /api/picture/extras/{id}, for the workers' service account and admins: the
// title's picture area as the catalog keeps it, and whether it is of what the
// title plays now (current), which the analyzer's pass asks before it
// measures. A covered episode is answered its holder's. 404 when it has none,
// 503 without migration 054.
func (h *Handlers) getPictureArea(w http.ResponseWriter, r *http.Request) {
	if h.d.Picture == nil {
		writeError(w, http.StatusServiceUnavailable, "the picture areas are not configured")
		return
	}
	a, err := h.d.Picture.Of(reqCtx(r), pictureKind(r), chi.URLParam(r, "id"))
	if pictureError(w, err) {
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "no picture area")
		return
	}
	writeJSON(w, http.StatusOK, pictureAreaOut(a))
}

// putPictureArea serves PUT /api/picture/items/{id} and
// /api/picture/extras/{id}, for the workers' service account and admins: the
// analyzer reports what it measured of a title (picture.Report: the file, its
// frame and the area, or why it could not tell, or that it could not read the
// file, or that it gives a title the backfill handed out back unmeasured),
// and is answered the title's picture area as it is now. 400 for a report
// that can't be taken (an area outside its frame, a covered episode, a
// series), 404 for no such title, 503 without migration 054.
func (h *Handlers) putPictureArea(w http.ResponseWriter, r *http.Request) {
	if h.d.Picture == nil {
		writeError(w, http.StatusServiceUnavailable, "the picture areas are not configured")
		return
	}
	var rep picture.Report
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&rep); err != nil {
		writeError(w, http.StatusBadRequest, "the body is no report of a measurement: "+err.Error())
		return
	}
	kind, id := pictureKind(r), chi.URLParam(r, "id")
	a, err := h.d.Picture.Record(reqCtx(r), kind, id, rep)
	if pictureError(w, err) {
		return
	}
	if a == nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "kind": kind, "released": true})
		return
	}
	writeJSON(w, http.StatusOK, pictureAreaOut(a))
}

// postPictureClaim serves POST /api/picture/claim, for the workers' service
// account and admins: the analyzer's backfill asks for the titles to measure,
// {"worker": "<its name>", "limit": n, "method": "<its measurement>"}, and is
// handed out at most what the backfill's pace allows (picture.Claim):
// {"items": [{"kind", "id", "itemId", "type", "path", "durationMs",
// "token"}], "deadline"}, or no items, "idle" (why) and "retryAfterSeconds".
// A title whose area is of another method than the one named is measured
// again. 503 without migration 054.
func (h *Handlers) postPictureClaim(w http.ResponseWriter, r *http.Request) {
	if h.d.Picture == nil {
		writeError(w, http.StatusServiceUnavailable, "the picture areas are not configured")
		return
	}
	var body struct {
		Worker string `json:"worker"`
		Limit  int    `json:"limit"`
		Method string `json:"method"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "the body is no claim: "+err.Error())
			return
		}
	}
	worker := strings.TrimSpace(body.Worker)
	if worker == "" {
		worker = "a worker"
	}
	c, err := h.d.Picture.Claim(reqCtx(r), worker, body.Limit, body.Method)
	if pictureError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// pictureError answers err, true when there was one.
func pictureError(w http.ResponseWriter, err error) bool {
	var bad *picture.BadReport
	switch {
	case err == nil:
		return false
	case errors.Is(err, picture.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, picture.ErrNoTitle):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	default:
		writeError(w, http.StatusInternalServerError, "the picture area could not be read or kept: "+err.Error())
	}
	return true
}
