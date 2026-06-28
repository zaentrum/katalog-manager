package rest

import (
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// getPlay streams the primary playback asset file for an item with HTTP
// byte-range support (200/206/416, Accept-Ranges: bytes). Ports PlayController:
// resolve the primary asset (isprimary=true) else fall back ORDER BY path; 404
// when no asset row or the file is missing/unreadable. http.ServeContent does
// the range parsing + Content-Range/Content-Length emission (RFC 7233), which
// subsumes the hand-rolled Java parser.
func (h *Handlers) getPlay(w http.ResponseWriter, r *http.Request) {
	itemID := chi.URLParam(r, "itemId")
	ctx := reqCtx(r)

	var path string
	err := h.d.Store.Pool().QueryRow(ctx,
		`SELECT path FROM com_nalet_katalog_playbackassets
		 WHERE item_id = $1 AND isprimary = true LIMIT 1`, itemID).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		err = h.d.Store.Pool().QueryRow(ctx,
			`SELECT path FROM com_nalet_katalog_playbackassets
			 WHERE item_id = $1 ORDER BY path LIMIT 1`, itemID).Scan(&path)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "no playback asset for item", http.StatusNotFound)
			return
		}
		http.Error(w, "playback asset lookup failed", http.StatusInternalServerError)
		return
	}

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file not found on filesystem", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "file not found on filesystem", http.StatusNotFound)
		return
	}

	name := filepath.Base(path)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	w.Header().Set("Content-Type", contentTypeForFile(name))

	// http.ServeContent honours the Content-Type we already set, emits
	// Accept-Ranges/Content-Range/Content-Length, and handles If-Range etc.
	// modtime drives Last-Modified + If-Range; the source file's mtime is the
	// right value here.
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// contentTypeForFile mirrors Spring's MediaTypeFactory: derive from extension,
// default application/octet-stream. mime.TypeByExtension covers the common
// container types; matroska is added explicitly because the stdlib table omits
// it (and it's the reason PlayController bypasses Spring's converter).
func contentTypeForFile(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mkv":
		return "video/x-matroska"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".ts":
		return "video/mp2t"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
