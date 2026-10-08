package rest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ingestRequest registers a file that appeared in the library (e.g. staged by an
// external importer) as a catalog item. NEUTRAL: it is the scanner's create path
// exposed as a machine contract — it knows nothing of how the file got there.
type ingestRequest struct {
	Path        string  `json:"path"`  // absolute source path (must exist under a known root)
	Type        string  `json:"type"`  // movie|episode|track
	Title       string  `json:"title"` // required
	Year        *int32  `json:"year,omitempty"`
	Description *string `json:"description,omitempty"`
	SortTitle   *string `json:"sortTitle,omitempty"`
	SizeBytes   int64   `json:"sizeBytes,omitempty"`
	// MetadataLocked pins the provided metadata against the enricher (default
	// false: let the pipeline enrich/fill artwork from TMDB).
	MetadataLocked *bool `json:"metadataLocked,omitempty"`

	// Episode coordinates. store.ItemWrite has always accepted these and
	// IngestExternalFile has always bound them — only this DTO dropped them, so
	// every externally-ingested episode arrived with a NULL parent.
	//
	// The consequence is not cosmetic: enrichEpisode returns "episode has no
	// series parent" -> not_found -> skipped, and the worker advances on
	// done|not_found alike, so the item becomes a PLAYABLE, metadata-less
	// orphan named after the show, with no error anywhere. Production holds 64
	// such rows, all with a primary asset.
	ParentID      *string `json:"parentId,omitempty"`
	SeasonNumber  *int32  `json:"seasonNumber,omitempty"`
	EpisodeNumber *int32  `json:"episodeNumber,omitempty"`
}

// ingest handles POST /api/ingest: create item + primary asset at the path, seed
// the scan step done, and emit catalog.item.discovered so the enrich→analyze→
// transcode→package pipeline runs. Idempotent on the path (re-ingest returns the
// existing item, no re-fire). Returns {itemId, created}.
func (h *Handlers) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	req.Type = strings.TrimSpace(req.Type)
	req.Title = strings.TrimSpace(req.Title)
	if req.Path == "" || req.Type == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, "path, type and title are required")
		return
	}
	// A disc image is no file the library holds, as the scanner passes over
	// one: refused here, it is no title whose steps would all fail.
	if processing.IsDiscImage(req.Path) {
		writeError(w, http.StatusBadRequest, filepath.Base(req.Path)+" is a "+processing.DiscImageReason)
		return
	}
	// An episode without coordinates cannot be linked to its series, and the
	// pipeline will happily publish it anyway as an orphan. Reject it at the
	// boundary: a 400 here is the only thing that turns a silent, permanent
	// data defect into a visible failure the caller can fix.
	if req.Type == "episode" && (req.ParentID == nil || *req.ParentID == "" ||
		req.SeasonNumber == nil || req.EpisodeNumber == nil) {
		writeError(w, http.StatusBadRequest,
			"an episode requires parentId, seasonNumber and episodeNumber; "+
				"without them it would be ingested as an unlinked orphan")
		return
	}

	// Guard: the path must live under the media OR packages root — an ingest must
	// never point the catalog at an arbitrary host path. With the library's v2
	// layout it lives under the arrivals' root alone, and is told by its size
	// and quick hash among the originals the library knows (ingestKnown).
	set, err := h.settings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the library's settings could not be read: "+err.Error())
		return
	}
	var fix *fixity
	if set.V2() {
		if !library.Within(h.paths().Arrivals, filepath.Clean(req.Path)) || !filepath.IsAbs(req.Path) {
			writeError(w, http.StatusBadRequest, "path must be under the arrivals' root (ARRIVALS_ROOT)")
			return
		}
		req.Path = filepath.Clean(req.Path)
		if id, done := h.ingestKnown(w, r, req.Path, &fix); done {
			if id != "" {
				writeJSON(w, http.StatusOK, map[string]any{"itemId": id, "created": false})
			}
			return
		}
	} else if !underRoot(h.d.Cfg.NFSRoot, req.Path) && !underRoot(h.d.Cfg.PackagesRoot, req.Path) {
		writeError(w, http.StatusBadRequest, "path must be under the media or packages root")
		return
	}

	iw := store.ItemWrite{
		Type: &req.Type, Title: &req.Title, SortTitle: req.SortTitle,
		Year: req.Year, Description: req.Description, MetadataLocked: req.MetadataLocked,
		ParentID: req.ParentID, SeasonNumber: req.SeasonNumber, EpisodeNumber: req.EpisodeNumber,
	}
	itemID, created, err := h.d.Store.IngestExternalFile(r.Context(), iw, req.Path, req.SizeBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
		return
	}
	if created && fix != nil {
		src, err := h.paths().AddSource(r.Context(), h.d.Store.Pool(), itemID, req.Path, fix.size, fix.qh1)
		if err == nil {
			_, err = h.d.Store.Pool().Exec(r.Context(), `UPDATE com_nalet_katalog_playbackassets SET sourceid = $3
				WHERE item_id = $1 AND path = $2`, itemID, req.Path, src.ID)
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "ingest failed: the source could not be kept: "+err.Error())
			return
		}
	}

	if created {
		// Seed the scan step done + emit discovered, exactly as the scanner does
		// on a fresh insert. Best-effort on the step (never abort the ingest).
		_ = h.d.Steps.Upsert(r.Context(), itemID, "scan", processing.StatusDone, nil, nil)
		if h.d.Events != nil {
			ev := events.NewItemEvent(itemID)
			ev.Type = req.Type
			ev.Step = "tmdb"
			ev.Source = "ingest"
			h.d.Events.EmitItem(r.Context(), events.TopicDiscovered, ev)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"itemId": itemID, "created": created})
}

// underRoot reports whether path lies inside root (both cleaned). Local copy so
// this file stands alone; mirrors itemactions.underRoot.
func underRoot(root, path string) bool {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	path = strings.TrimSpace(path)
	if root == "" || root == "/" || path == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// fixity is what tells a file: its size and its quick hash.
type fixity struct {
	size int64
	qh1  string
}

// ingestKnown tells the file at path among the originals the library knows,
// as the scanner does: a file the catalog has at that path already is that
// title (re-ingested: nothing happens); one whose original moved here takes
// its title along; a copy of one still in its place, and one deleted after
// packaging, are refused (409), saying whose; one that cannot be read is
// refused (400). It answers the title the file is when done says the answer
// is given (the id empty when a refusal was written), and fix the file's size
// and quick hash otherwise.
func (h *Handlers) ingestKnown(w http.ResponseWriter, r *http.Request, path string, fix **fixity) (string, bool) {
	ctx := r.Context()
	pool := h.d.Store.Pool()
	var existing string
	if err := pool.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`, path).
		Scan(&existing); err == nil {
		return existing, true
	}
	size, qh1, err := library.QH1(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "there is no file to take in at "+path+": "+err.Error())
		return "", true
	}
	*fix = &fixity{size: size, qh1: qh1}
	known, err := library.SourcesByFixity(ctx, pool, size, qh1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
		return "", true
	}
	for _, k := range known {
		switch k.State {
		case library.SourcePresent:
			if k.ArrivalPath != nil {
				if fi, err := os.Stat(*k.ArrivalPath); err == nil && fi.Mode().IsRegular() {
					writeJSON(w, http.StatusConflict, map[string]any{"error": path + " is a copy of the original of item " +
						k.ItemID + " at " + *k.ArrivalPath, "itemId": k.ItemID})
					return "", true
				}
			}
			old := ""
			if k.ArrivalPath != nil {
				old = *k.ArrivalPath
			}
			if _, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, sourceid = $2
				WHERE item_id = $1 AND COALESCE(kind, 'primary') = 'primary' AND (sourceid = $2 OR (sourceid IS NULL AND path = $4))`,
				k.ItemID, k.ID, path, old); err != nil {
				writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
				return "", true
			}
			if err := h.paths().MoveSource(ctx, pool, k, path); err != nil {
				writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
				return "", true
			}
			return k.ItemID, true
		case library.SourceRetiring, library.SourceDeleted:
			writeJSON(w, http.StatusConflict, map[string]any{"error": path + " is already in the library as item " + k.ItemID +
				" (retired); use replaceSource to make it a new version", "itemId": k.ItemID})
			return "", true
		}
	}
	return "", false
}
