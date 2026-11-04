package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/chapters"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ChapterReads are the titles' chapters, what reading them from the
// originals found, and the backfill that reads them again (chapters.Service):
// the analyzer reports each read with the chapters it found, reads a
// title's, and asks the backfill for the titles to read.
type ChapterReads interface {
	Of(ctx context.Context, itemID string) (*chapters.Title, error)
	Record(ctx context.Context, itemID string, r chapters.Report) (*chapters.Read, error)
	Claim(ctx context.Context, worker string, limit int) (chapters.Claim, error)
}

// chapterReadJSON is a title's last read on the wire.
type chapterReadJSON struct {
	State          string          `json:"state"`
	ReadFrom       *string         `json:"readFrom"`
	Confirmed      bool            `json:"confirmed"`
	SourceID       *string         `json:"sourceId"`
	RowsSourceID   *string         `json:"rowsSourceId"`
	Replaced       json.RawMessage `json:"replaced"`
	Path           *string         `json:"path"`
	FileSize       *int64          `json:"fileSize"`
	Found          *int32          `json:"found"`
	Written        *int32          `json:"written"`
	Probe          *string         `json:"probe"`
	Seconds        *float64        `json:"seconds"`
	Reason         *string         `json:"reason"`
	ReadAt         *time.Time      `json:"readAt"`
	RecordSourceID *string         `json:"recordSourceId"`
	RecordAt       *time.Time      `json:"recordAt"`
	RecordNote     *string         `json:"recordNote"`
	Attempts       int32           `json:"attempts"`
	Failures       int32           `json:"failures"`
	Transients     int32           `json:"transients"`
	NextAttemptAt  *time.Time      `json:"nextAttemptAt"`
	ClaimedAt      *time.Time      `json:"claimedAt"`
	AcceptedAt     *time.Time      `json:"acceptedAt"`
	AcceptedBy     *string         `json:"acceptedBy"`
}

func chapterReadOut(r *chapters.Read) *chapterReadJSON {
	if r == nil {
		return nil
	}
	return &chapterReadJSON{State: r.State, ReadFrom: r.ReadFrom, Confirmed: r.Confirmed, SourceID: r.SourceID,
		RowsSourceID: r.RowsSourceID, Replaced: r.Replaced, Path: r.Path, FileSize: r.FileSize, Found: r.Found, Written: r.Written, Probe: r.Probe, Seconds: r.Seconds,
		Reason: r.Reason, ReadAt: utc(r.ReadAt), RecordSourceID: r.RecordSourceID, RecordAt: utc(r.RecordAt),
		RecordNote: r.RecordNote, Attempts: r.Attempts, Failures: r.Failures, Transients: r.Transients,
		NextAttemptAt: utc(r.NextAttemptAt), ClaimedAt: utc(r.ClaimedAt), AcceptedAt: utc(r.AcceptedAt),
		AcceptedBy: r.AcceptedBy}
}

// putChapters serves PUT /api/chapters/items/{itemId}, for the workers'
// service account and admins: the chapters of a title, read from its
// original, in place of its own ({"chapters": [{"startMs", "endMs", "title",
// "ordinal"}]}: 0 <= start < end; a blank title is none, one longer than 120
// characters is cut; an ordinal left out is the chapter's place in the
// list). With "read", what the read of the original's header was ({"path",
// "sourceId", "fileSize", "found", "error", "transient", "note", "probe",
// "seconds"}), and the token of the backfill's claim it read for ("token"),
// the catalog keeps the read with the chapters, in the same transaction
// (chapters.Record): a read that found none ("chapters": []) never deletes
// chapters, and settles the title only as far as the original's source record
// agrees; one that failed ("read": {"error"}, no "chapters") leaves the
// title's chapters as they are, and counts a failure of the title only when
// it ran on the file ("transient" false). {"released": true, "token"} gives a
// claim back unread. 400 for chapters or a read that can't be taken, 404 for
// no such item; a read is answered with the title's read as it is now. A
// catalog without migration 055 writes chapters a read found, keeps no read,
// and answers 503 for a read without chapters (a failed one), whose title's
// chapters stay as they are.
func (h *Handlers) putChapters(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	ctx := reqCtx(r)

	ok, err := itemExists(ctx, h.d.Store.Pool(), itemID)
	if err != nil {
		http.Error(w, "item lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown item: "+itemID)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body could not be read: "+err.Error())
		return
	}
	var body map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&body); err != nil || body == nil {
		writeError(w, http.StatusBadRequest, "missing 'chapters' array")
		return
	}
	var meta struct {
		Read     *chapters.ReadReport `json:"read"`
		Token    string               `json:"token"`
		Released bool                 `json:"released"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		writeError(w, http.StatusBadRequest, "the body is no report of a read: "+err.Error())
		return
	}
	rows, present, why := chapterRows(body)
	if why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	if meta.Read == nil && !meta.Released {
		if !present {
			writeError(w, http.StatusBadRequest, "missing 'chapters' array")
			return
		}
		h.replaceChapters(w, r, itemID, rows)
		return
	}
	// Where no read is kept, only chapters a read found are written: a read
	// that found none, or failed, changes nothing.
	fallback := func(why string) {
		if present && len(rows) > 0 && !meta.Released && (meta.Read == nil || strings.TrimSpace(meta.Read.Error) == "") {
			h.replaceChapters(w, r, itemID, rows)
			return
		}
		writeError(w, http.StatusServiceUnavailable, why+"; a read that found no chapter, or failed, changes nothing")
	}
	if h.d.Chapters == nil {
		fallback("the chapters' reads are not configured")
		return
	}
	rep := chapters.Report{Token: meta.Token, Released: meta.Released}
	if meta.Read != nil {
		rep.Read = *meta.Read
	}
	if present {
		rep.Chapters = rows
	}
	read, err := h.d.Chapters.Record(ctx, itemID, rep)
	if errors.Is(err, chapters.ErrNotReady) {
		fallback(err.Error())
		return
	}
	if chaptersError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "written": len(rows), "read": chapterReadOut(read)})
}

// chapterRows are the chapters of a body's "chapters" list, each as the
// catalog keeps it, and whether the body has the list; why the list can't be
// taken, when it can't.
func chapterRows(body map[string]any) ([]chapters.Chapter, bool, string) {
	raw, present := body["chapters"]
	if !present {
		return nil, false, ""
	}
	list, isList := raw.([]any)
	if !isList {
		return nil, false, "missing 'chapters' array"
	}
	rows := make([]chapters.Chapter, 0, len(list))
	for i, o := range list {
		ch, isMap := o.(map[string]any)
		if !isMap {
			return nil, false, "chapter entries must be objects"
		}
		start, startOK := asLong(ch["startMs"])
		end, endOK := asLong(ch["endMs"])
		if !startOK || !endOK || end <= start || start < 0 {
			return nil, false, "startMs/endMs invalid (need 0 <= start < end)"
		}
		ordinal := i + 1
		if ord, ok := asInt(ch["ordinal"]); ok {
			ordinal = ord
		}
		rows = append(rows, chapters.Chapter{StartMs: start, EndMs: end, Title: chapters.CleanTitle(asString(ch["title"])),
			Ordinal: ordinal})
	}
	return rows, true, ""
}

// replaceChapters writes rows as the chapters of the item itemID, in place
// of its own, and answers how many it wrote: what a PUT without a read does.
// They are of no original known (store.ChaptersOfNoRead): the title misses
// them until a read of its original agrees.
func (h *Handlers) replaceChapters(w http.ResponseWriter, r *http.Request, itemID string, rows []chapters.Chapter) {
	ctx := reqCtx(r)
	tx, err := h.d.Store.Pool().Begin(ctx)
	if err != nil {
		http.Error(w, "chapters tx failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := chapters.Replace(ctx, tx, itemID, rows); err != nil {
		http.Error(w, "chapters write failed", http.StatusInternalServerError)
		return
	}
	if err := store.ChaptersOfNoRead(ctx, tx, itemID); err != nil {
		http.Error(w, "chapters write failed", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "chapters commit failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "written": len(rows)})
}

// getChapters serves GET /api/chapters/items/{itemId}, for the workers'
// service account and admins: the title's chapters, its last read
// (chapterReadJSON; null when it has none) and whether it misses its chapters
// ("missing": its original's record says it has chapters, the catalog has
// none of the title, and no read of that original settled it), which a
// deletion wave keeps its original for. 404 for no such item, 503 without
// migration 055.
func (h *Handlers) getChapters(w http.ResponseWriter, r *http.Request) {
	if h.d.Chapters == nil {
		writeError(w, http.StatusServiceUnavailable, "the chapters' reads are not configured")
		return
	}
	itemID := chi.URLParam(r, "itemId")
	t, err := h.d.Chapters.Of(reqCtx(r), itemID)
	if chaptersError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "chapters": t.Chapters, "read": chapterReadOut(t.Read),
		"missing": t.Missing})
}

// postChaptersClaim serves POST /api/chapters/claim, for the workers' service
// account and admins: the analyzer's backfill asks for the titles to read,
// {"worker": "<its name>", "limit": n}, and is handed out at most what the
// backfill's pace allows (chapters.Claim): {"items": [{"id", "type", "path",
// "sourceId", "durationMs", "token"}], "deadline"}, or no items, "idle" (why)
// and "retryAfterSeconds". 503 without migration 055.
func (h *Handlers) postChaptersClaim(w http.ResponseWriter, r *http.Request) {
	if h.d.Chapters == nil {
		writeError(w, http.StatusServiceUnavailable, "the chapters' reads are not configured")
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
	c, err := h.d.Chapters.Claim(reqCtx(r), worker, body.Limit)
	if chaptersError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// chaptersError answers err, true when there was one.
func chaptersError(w http.ResponseWriter, err error) bool {
	var bad *chapters.BadReport
	switch {
	case err == nil:
		return false
	case errors.Is(err, chapters.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, chapters.ErrNoTitle):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, bad.Error())
	default:
		writeError(w, http.StatusInternalServerError, "the chapters could not be read or kept: "+err.Error())
	}
	return true
}

// deleteChapters ports ChaptersController#deleteForItem; the title's read
// keeps no original of its chapters then (store.ChaptersOfNoRead).
func (h *Handlers) deleteChapters(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	ctx := reqCtx(r)
	tx, err := h.d.Store.Pool().Begin(ctx)
	if err != nil {
		http.Error(w, "chapters tx failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, itemID)
	if err == nil {
		err = store.ChaptersOfNoRead(ctx, tx, itemID)
	}
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		http.Error(w, "chapters delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "removed": tag.RowsAffected()})
}
