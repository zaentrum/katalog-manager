package rest

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// putChapters ports ChaptersController#replaceForItem: atomic DELETE + batch
// INSERT of file-internal chapter atoms. Validates 0 <= start < end; blank
// title -> NULL; ordinal fallback = 1-based array position. 404 unknown item.
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

	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "missing 'chapters' array")
		return
	}
	raw, present := body["chapters"]
	list, isList := raw.([]any)
	if !present || !isList {
		writeError(w, http.StatusBadRequest, "missing 'chapters' array")
		return
	}

	type chapRow struct {
		start, end int64
		title      *string
		ordinal    int
	}
	rows := make([]chapRow, 0, len(list))
	pos := 0
	for _, o := range list {
		ch, isMap := o.(map[string]any)
		if !isMap {
			writeError(w, http.StatusBadRequest, "chapter entries must be objects")
			return
		}
		start, startOK := asLong(ch["startMs"])
		end, endOK := asLong(ch["endMs"])
		title := asString(ch["title"])
		ord, ordOK := asInt(ch["ordinal"])

		if !startOK || !endOK || end <= start || start < 0 {
			writeError(w, http.StatusBadRequest, "startMs/endMs invalid (need 0 <= start < end)")
			return
		}
		pos++
		var titleVal *string
		if title != nil && trimBlank(*title) != "" {
			titleVal = title
		}
		ordinal := pos
		if ordOK {
			ordinal = ord
		}
		rows = append(rows, chapRow{start: start, end: end, title: titleVal, ordinal: ordinal})
	}

	pool := h.d.Store.Pool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		http.Error(w, "chapters tx failed", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, itemID); err != nil {
		http.Error(w, "chapters delete failed", http.StatusInternalServerError)
		return
	}
	if len(rows) > 0 {
		batch := &pgx.Batch{}
		for _, c := range rows {
			batch.Queue(`INSERT INTO com_nalet_katalog_itemchapters
				(id, createdat, modifiedat, item_id, startms, endms, title, ordinal)
				VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5)`,
				itemID, c.start, c.end, c.title, c.ordinal)
		}
		br := tx.SendBatch(ctx, batch)
		for range rows {
			if _, err := br.Exec(); err != nil {
				br.Close()
				http.Error(w, "chapters insert failed", http.StatusInternalServerError)
				return
			}
		}
		if err := br.Close(); err != nil {
			http.Error(w, "chapters insert failed", http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "chapters commit failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "written": len(rows)})
}

// deleteChapters ports ChaptersController#deleteForItem.
func (h *Handlers) deleteChapters(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	tag, err := h.d.Store.Pool().Exec(reqCtx(r),
		`DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, itemID)
	if err != nil {
		http.Error(w, "chapters delete failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "removed": tag.RowsAffected()})
}
