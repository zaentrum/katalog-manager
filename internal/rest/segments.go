package rest

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var segmentKinds = map[string]bool{"intro": true, "recap": true, "credits": true, "preview": true}

var segmentSources = map[string]bool{
	"tidb": true, "chapter": true, "subtitle": true, "silence": true, "blackframe": true,
	"chromaprint": true, "whisper": true, "transnet": true, "manual": true,
}

const segmentKindsMsg = "kind must be one of [intro, recap, credits, preview]"
const segmentSourcesMsg = "source must be one of [tidb, chapter, subtitle, silence, blackframe, chromaprint, whisper, transnet, manual]"

// putSegments ports SegmentsController#replaceForItem: atomic DELETE + batch
// INSERT of TIDB-aligned segments. Validates 0 <= start < end; does NOT touch
// processing steps. 404 unknown item, 400 on validation failure.
func (h *Handlers) putSegments(w http.ResponseWriter, r *http.Request) {
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

	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "missing 'segments' array")
		return
	}
	raw, present := body["segments"]
	list, isList := raw.([]any)
	if !present || !isList {
		writeError(w, http.StatusBadRequest, "missing 'segments' array")
		return
	}

	type segRow struct {
		kind, source string
		start, end   int64
		conf         *float64
		label        *string
	}
	rows := make([]segRow, 0, len(list))
	for _, o := range list {
		seg, isMap := o.(map[string]any)
		if !isMap {
			writeError(w, http.StatusBadRequest, "segment entries must be objects")
			return
		}
		kindP := asString(seg["kind"])
		sourceP := asString(seg["source"])
		start, startOK := asLong(seg["startMs"])
		end, endOK := asLong(seg["endMs"])
		var conf *float64
		if c, ok := asDouble(seg["confidence"]); ok {
			conf = &c
		}
		label := asString(seg["label"])

		if kindP == nil || !segmentKinds[*kindP] {
			writeError(w, http.StatusBadRequest, segmentKindsMsg)
			return
		}
		if sourceP == nil || !segmentSources[*sourceP] {
			writeError(w, http.StatusBadRequest, segmentSourcesMsg)
			return
		}
		if !startOK || !endOK || end <= start || start < 0 {
			writeError(w, http.StatusBadRequest, "startMs/endMs invalid (need 0 <= start < end)")
			return
		}
		rows = append(rows, segRow{kind: *kindP, source: *sourceP, start: start, end: end, conf: conf, label: label})
	}

	pool := h.d.Store.Pool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		http.Error(w, "segments tx failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM com_nalet_katalog_mediasegments WHERE item_id = $1`, itemID); err != nil {
		http.Error(w, "segments delete failed", http.StatusInternalServerError)
		return
	}
	if len(rows) > 0 {
		batch := &pgx.Batch{}
		for _, s := range rows {
			batch.Queue(`INSERT INTO com_nalet_katalog_mediasegments
				(id, createdat, modifiedat, item_id, kind, startms, endms, source, confidence, label)
				VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5, $6, $7)`,
				itemID, s.kind, s.start, s.end, s.source, s.conf, s.label)
		}
		br := tx.SendBatch(ctx, batch)
		for range rows {
			if _, err := br.Exec(); err != nil {
				br.Close()
				http.Error(w, "segments insert failed", http.StatusInternalServerError)
				return
			}
		}
		if err := br.Close(); err != nil {
			http.Error(w, "segments insert failed", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "segments commit failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "written": len(rows)})
}

// deleteSegments ports SegmentsController#deleteForItem (no 404 check).
func (h *Handlers) deleteSegments(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	tag, err := h.d.Store.Pool().Exec(reqCtx(r),
		`DELETE FROM com_nalet_katalog_mediasegments WHERE item_id = $1`, itemID)
	if err != nil {
		http.Error(w, "segments delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "removed": tag.RowsAffected()})
}

// itemExists reproduces the COUNT(*) existence check the controllers do.
func itemExists(ctx context.Context, pool *pgxpool.Pool, itemID string) (bool, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM com_nalet_katalog_items WHERE id = $1`, itemID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
