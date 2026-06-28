package rest

import (
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// listSubtitles ports SubtitlesController#listForItem: JSON list of available
// subtitle tracks for an item, ORDER BY isdefault DESC, label ASC. A query
// failure (table may not exist) is swallowed -> {"subtitles": []} so the player
// hides captions. Each entry carries id/lang/label, optional format (omitted
// when null), url "/api/subtitles/<id>", and default:true only when isdefault.
func (h *Handlers) listSubtitles(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")

	rows, err := h.d.Store.Pool().Query(reqCtx(r),
		`SELECT id, format, lang, label, isdefault
		 FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
		 ORDER BY isdefault DESC, label ASC`, itemID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"subtitles": []any{}})
		return
	}
	defer rows.Close()

	out := make([]map[string]any, 0)
	for rows.Next() {
		var id string
		var format, lang, label *string
		var isDefault *bool
		if err := rows.Scan(&id, &format, &lang, &label, &isDefault); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"subtitles": []any{}})
			return
		}
		sub := map[string]any{
			"id":    id,
			"lang":  lang,
			"label": label,
			"url":   "/api/subtitles/" + id,
		}
		if format != nil {
			sub["format"] = *format
		}
		if isDefault != nil && *isDefault {
			sub["default"] = true
		}
		out = append(out, sub)
	}
	if rows.Err() != nil {
		writeJSON(w, http.StatusOK, map[string]any{"subtitles": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subtitles": out})
}

var srtTimeRe = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})`)

// getSubtitle ports SubtitlesController#serve: serve a track. pgs/vobsub/dvb
// pass through with their octet MIME; everything else is served as text/vtt
// (SRT converted live; non-WEBVTT text gets a WEBVTT header prepended).
// Cache-Control private, max-age=300. 404 on unknown id or missing file.
func (h *Handlers) getSubtitle(w http.ResponseWriter, r *http.Request) {
	subID := chi.URLParam(r, "subId")

	var path string
	var format *string
	err := h.d.Store.Pool().QueryRow(reqCtx(r),
		`SELECT path, format FROM com_nalet_katalog_subtitleassets WHERE id = $1 LIMIT 1`,
		subID).Scan(&path, &format)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "unknown subtitle", http.StatusNotFound)
			return
		}
		http.Error(w, "subtitle lookup failed", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	if info, serr := f.Stat(); serr != nil || info.IsDir() {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=300")
	fmt := ""
	if format != nil {
		fmt = strings.ToLower(*format)
	}

	switch fmt {
	case "pgs":
		w.Header().Set("Content-Type", "application/pgs")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		return
	case "vobsub":
		w.Header().Set("Content-Type", "application/x-vobsub")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		return
	case "dvb":
		w.Header().Set("Content-Type", "application/dvb-subtitles")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, f)
		return
	}

	body, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	text := string(body)
	if fmt == "srt" {
		text = srtToVtt(text)
	} else if !strings.HasPrefix(text, "WEBVTT") {
		text = "WEBVTT\n\n" + text
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, text)
}

// srtToVtt ports SubtitlesController#srtToVtt: normalise CRLF->LF, comma->dot on
// the millisecond separator, prepend the WEBVTT header.
func srtToVtt(srt string) string {
	body := strings.ReplaceAll(srt, "\r\n", "\n")
	body = srtTimeRe.ReplaceAllString(body, "$1.$2")
	return "WEBVTT\n\n" + body
}
