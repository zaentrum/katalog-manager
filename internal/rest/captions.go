package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/captions"
)

// ClosedCaptions are the originals' closed captions and their backfill
// (captions.Service): the analyzer asks the backfill for the originals to
// read, and reports what it read of each. katalog-manager reads no media.
type ClosedCaptions interface {
	Record(ctx context.Context, sourceID string, r captions.Report) (*captions.Read, error)
	Claim(ctx context.Context, worker string, limit int) (captions.Claim, error)
}

// captionsReadJSON is what the catalog keeps of an original's closed
// captions, on the wire.
type captionsReadJSON struct {
	SourceID      string           `json:"sourceId"`
	ItemID        string           `json:"itemId"`
	Original      captionsFileJSON `json:"original"`
	Attempts      int32            `json:"attempts"`
	Failures      int32            `json:"failures"`
	NextAttemptAt *time.Time       `json:"nextAttemptAt"`
	ClaimedAt     *time.Time       `json:"claimedAt"`
}

// captionsFileJSON is what the catalog keeps of a read of the original.
type captionsFileJSON struct {
	State          string          `json:"state"`
	CaptionWindows json.RawMessage `json:"captionWindows"`
	Captions       *bool           `json:"captions"`
	FFprobe        *string         `json:"ffprobe"`
	FramesDecoded  *int32          `json:"framesDecoded"`
	Duration       *float64        `json:"duration"`
	Reason         *string         `json:"reason"`
	ReadAt         *time.Time      `json:"readAt"`
}

func captionsReadOut(c *captions.Read) captionsReadJSON {
	return captionsReadJSON{SourceID: c.SourceID, ItemID: c.ItemID, Attempts: c.Attempts, Failures: c.Failures,
		NextAttemptAt: utc(c.NextAttemptAt), ClaimedAt: utc(c.ClaimedAt),
		Original: captionsFileJSON{State: c.State, CaptionWindows: rawOrNull(c.Windows), Captions: c.Captions,
			FFprobe: c.FFprobeVersion, FramesDecoded: c.FramesDecoded, Duration: c.Duration, Reason: c.Reason,
			ReadAt: utc(c.ReadAt)}}
}

// rawOrNull is the JSON b, null when there is none.
func rawOrNull(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

// postCaptionsClaim serves POST /api/captions/claim, for the workers'
// service account and admins: the analyzer's captions backfill asks for the
// originals to read, {"worker": "<its name>", "limit": n}, and is handed out
// at most what the backfill's pace allows (captions.Claim): {"items":
// [{"sourceId", "itemId", "path", "packets", "token"}], "deadline"}, or no
// items, "idle" (why) and "retryAfterSeconds". 503 without migration 058.
func (h *Handlers) postCaptionsClaim(w http.ResponseWriter, r *http.Request) {
	if h.d.Captions == nil {
		writeError(w, http.StatusServiceUnavailable, "the closed captions are not configured")
		return
	}
	var body struct {
		Worker string `json:"worker"`
		Limit  int    `json:"limit"`
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
	c, err := h.d.Captions.Claim(reqCtx(r), worker, body.Limit)
	if captionsError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// putCaptions serves PUT /api/captions/sources/{sourceId}, for the workers'
// service account and admins: the analyzer reports what it read of an
// original a claim handed out (captions.Report: {"token", "original":
// {"captionWindows": [{"at", "start", "end", "frames", "captions"}],
// "duration", "ffprobe"} or {"error", "ffprobe"}}, or {"token", "released":
// true} to give an original handed out back unread), and is answered what
// the catalog keeps of it now. A report's "package" part, of an older
// analyzer, is taken no notice of. A read none of whose windows decoded a
// frame is kept as a failed one, and so is a window that is no window (a
// value of another type, one out of its range, a key given twice): it is
// kept as a failed window and said, never a 400. 400 for a report that
// can't be taken (no read, no ffprobe or one that is not 7.1, a read
// without windows, a key of the report given twice), 404 for no such
// original, 409 for a token that is not the claim's, 503 without migration
// 058.
func (h *Handlers) putCaptions(w http.ResponseWriter, r *http.Request) {
	if h.d.Captions == nil {
		writeError(w, http.StatusServiceUnavailable, "the closed captions are not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body is no report of a read: "+err.Error())
		return
	}
	if k := captions.RepeatedKey(body); k != "" {
		writeError(w, http.StatusBadRequest, "the body is no report of a read: its key "+strconv.Quote(k)+
			" is given twice in one object")
		return
	}
	var rep captions.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		writeError(w, http.StatusBadRequest, "the body is no report of a read: "+err.Error())
		return
	}
	c, err := h.d.Captions.Record(reqCtx(r), chi.URLParam(r, "sourceId"), rep)
	if captionsError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, captionsReadOut(c))
}

// captionsError answers err, true when there was one.
func captionsError(w http.ResponseWriter, err error) bool {
	var bad *captions.BadReport
	switch {
	case err == nil:
		return false
	case errors.Is(err, captions.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, captions.ErrNoSource):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, captions.ErrNotTheClaim):
		writeError(w, http.StatusConflict, err.Error())
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	default:
		writeError(w, http.StatusInternalServerError, "the closed captions could not be read or kept: "+err.Error())
	}
	return true
}
