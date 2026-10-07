package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/retry"
)

// Reencoder is the re-encode queue (retry.Service).
type Reencoder interface {
	EnqueueReencode(ctx context.Context, req graph.ReencodeRequest) (graph.ReencodeEnqueued, error)
	ReencodeQueue(ctx context.Context) (graph.ReencodeQueue, error)
}

// reencodeBody is the body of POST /api/library/reencode.
type reencodeBody struct {
	Items []string `json:"items"`
	Held  bool     `json:"held"`
	All   bool     `json:"all"`
}

// enqueued is the answer of POST /api/library/reencode.
type enqueued struct {
	Queued        int32     `json:"queued"`
	AlreadyQueued int32     `json:"alreadyQueued"`
	Skipped       []skipped `json:"skipped"`
}

type skipped struct {
	ItemID string `json:"itemId"`
	Reason string `json:"reason"`
}

// queueCounts is the answer of GET /api/library/reencode.
type queueCounts struct {
	Queued int32        `json:"queued"`
	Sent   int32        `json:"sent"`
	Done   int32        `json:"done"`
	Failed int32        `json:"failed"`
	Oldest *queuedTitle `json:"oldest"`
	Newest *queuedTitle `json:"newest"`
	Idle   *string      `json:"idle,omitempty"`
}

type queuedTitle struct {
	ItemID     string    `json:"itemId"`
	State      string    `json:"state"`
	EnqueuedAt time.Time `json:"enqueuedAt"`
}

// postReencode serves POST /api/library/reencode, for the workers' service
// account and admins: it queues titles to be encoded again under the
// pipeline's current settings, which the sweep sends a few at a time (the
// settings library.reencode.rate, .inflight and .window). The body names
// them: {"items": ["<itemId>", …]} (movies and episodes; a series is its
// episodes), {"held": true} (every title whose original's retire is held for
// its surround), {"all": true} (every packaged movie and episode), or any of
// them together, each title once. It answers {queued, alreadyQueued,
// skipped: [{itemId, reason}]}: a title queued already, waiting or sent, is
// not queued again, and one with nothing to encode (unknown, no file, its
// original deleted after packaging, no movie, episode or series) is skipped.
// 400 for a body that names none; 503 without the queue (migration 043).
func (h *Handlers) postReencode(w http.ResponseWriter, r *http.Request) {
	if h.d.Reencode == nil {
		writeError(w, http.StatusServiceUnavailable, "the re-encode queue is not configured")
		return
	}
	var body reencodeBody
	const names = `name the titles: {"items": ["<itemId>", …]}, {"held": true} or {"all": true}`
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "the body is no request to the queue: "+err.Error()+"; "+names)
			return
		}
	}
	if len(body.Items) == 0 && !body.Held && !body.All {
		writeError(w, http.StatusBadRequest, names)
		return
	}
	res, err := h.d.Reencode.EnqueueReencode(reqCtx(r), graph.ReencodeRequest{Items: body.Items, Held: body.Held, All: body.All})
	switch {
	case errors.Is(err, retry.ErrNoQueue):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the re-encode queue: "+err.Error())
		return
	}
	out := enqueued{Queued: res.Queued, AlreadyQueued: res.AlreadyQueued, Skipped: []skipped{}}
	for _, s := range res.Skipped {
		out.Skipped = append(out.Skipped, skipped{ItemID: s.ItemID, Reason: s.Reason})
	}
	writeJSON(w, http.StatusOK, out)
}

// getReencode serves GET /api/library/reencode: the queue's titles by state
// ({queued, sent, done, failed}), of those waiting or sent the one queued
// first and the one queued last ({oldest, newest}: {itemId, state,
// enqueuedAt}, null when none waits), and why the sweep sends none now
// (idle, left out when it may). 503 without the queue (migration 043).
func (h *Handlers) getReencode(w http.ResponseWriter, r *http.Request) {
	if h.d.Reencode == nil {
		writeError(w, http.StatusServiceUnavailable, "the re-encode queue is not configured")
		return
	}
	q, err := h.d.Reencode.ReencodeQueue(reqCtx(r))
	switch {
	case errors.Is(err, retry.ErrNoQueue):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the re-encode queue: "+err.Error())
		return
	}
	title := func(t *graph.QueuedTitle) *queuedTitle {
		if t == nil {
			return nil
		}
		return &queuedTitle{ItemID: t.ItemID, State: t.State, EnqueuedAt: t.EnqueuedAt.UTC()}
	}
	writeJSON(w, http.StatusOK, queueCounts{Queued: q.Queued, Sent: q.Sent, Done: q.Done, Failed: q.Failed,
		Oldest: title(q.Oldest), Newest: title(q.Newest), Idle: q.Idle})
}
