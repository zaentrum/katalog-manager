package rest

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ExtraTaker takes a file in as a title's extra (extras.Service).
type ExtraTaker interface {
	AddExtra(ctx context.Context, in graph.AddExtraRequest) (graph.AddExtraResult, error)
}

// extraRequest is POST /api/extras: a file taken in as an extra of a movie
// or a series, the title named one way (itemId, itemPath or tmdbId with
// itemType).
type extraRequest struct {
	ItemID       string  `json:"itemId"`
	ItemPath     string  `json:"itemPath"`
	TmdbID       *int64  `json:"tmdbId"`
	ItemType     string  `json:"itemType"`
	Path         string  `json:"path"`
	Kind         string  `json:"kind"`
	Title        string  `json:"title"`
	Language     *string `json:"language"`
	SeasonNumber *int32  `json:"seasonNumber"`
}

// postExtra serves POST /api/extras: what GraphQL's addExtra does, for an
// operator's tool, a deployment's Job or an addon. It answers 201 with the
// extra taken in, 200 with the one the file was already
// ({extraId, itemId, created, kind, title, state}); a refusal with its
// status and {"error", "code"}, a conflict naming the extra in the way.
func (h *Handlers) postExtra(w http.ResponseWriter, r *http.Request) {
	if h.d.Extras == nil {
		writeError(w, http.StatusServiceUnavailable, "extras are not configured")
		return
	}
	var req extraRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	res, err := h.d.Extras.AddExtra(reqCtx(r), graph.AddExtraRequest{ItemID: req.ItemID, ItemPath: req.ItemPath,
		TmdbID: req.TmdbID, ItemType: req.ItemType, Path: req.Path, Kind: req.Kind, Title: req.Title,
		Language: req.Language, SeasonNumber: req.SeasonNumber})
	if err != nil {
		writeExtraError(w, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	x := res.Extra
	writeJSON(w, status, map[string]any{"extraId": x.ID, "itemId": x.ItemID, "created": res.Created, "kind": x.Kind,
		"title": x.Title, "state": x.State})
}

// writeExtraError answers a refusal with its status, its message and its
// code, and the extra in the way when there is one; any other error 500.
func writeExtraError(w http.ResponseWriter, err error) {
	var refused *graph.ExtraRefused
	if !errors.As(err, &refused) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	body := map[string]any{"error": refused.Message, "code": refused.Code}
	if refused.Extra != nil {
		body["extraId"], body["itemId"] = refused.Extra.ID, refused.Extra.ItemID
	}
	writeJSON(w, refused.Status, body)
}

// extraRecord is an extra's worker record: what the transcoder encodes and
// the packager packages, and the title it belongs to.
type extraRecord struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"` // always "extra"
	ParentID     string     `json:"parentId"`
	ParentType   string     `json:"parentType"`
	ParentTitle  string     `json:"parentTitle"`
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Language     *string    `json:"language"`
	SeasonNumber *int32     `json:"seasonNumber"`
	Path         *string    `json:"path"`
	State        string     `json:"state"`
	RemovedAt    *time.Time `json:"removedAt,omitempty"`
	// With the setting library.layout=v2, where its folder goes and what it
	// records; left out otherwise.
	Library *extraLibrary `json:"library,omitempty"`
}

// liveExtra is the extra id and its title, nil when there is no such extra,
// or it is removed, or its title is gone.
func (h *Handlers) liveExtra(ctx context.Context, id string) (*model.Extra, *model.Item, error) {
	x, err := h.d.Store.GetExtra(ctx, id)
	if err != nil || x == nil || x.RemovedAt != nil {
		return nil, nil, err
	}
	it, err := h.d.Store.GetItemBase(ctx, x.ItemID)
	if err != nil || it == nil {
		return nil, nil, err
	}
	return x, it, nil
}

// getAnalyzeExtra serves GET /api/analyze/extras/{id}, the extra's worker
// record: {id, type "extra", parentId, parentType, parentTitle, kind,
// title, language, seasonNumber, path, state}. 404 for an extra there is
// not, and for a removed one: a worker skips it.
func (h *Handlers) getAnalyzeExtra(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	x, it, err := h.liveExtra(reqCtx(r), id)
	if err != nil {
		log.Printf("getAnalyzeExtra: %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "extra lookup failed")
		return
	}
	if x == nil {
		writeError(w, http.StatusNotFound, "no such extra: "+id)
		return
	}
	rec := extraRecord{ID: x.ID, Type: "extra", ParentID: x.ItemID, ParentType: it.Type,
		ParentTitle: it.Title, Kind: x.Kind, Title: x.Title, Language: x.Language, SeasonNumber: x.SeasonNumber,
		Path: x.SourcePath, State: x.State, RemovedAt: x.RemovedAt}
	set, err := h.settings(reqCtx(r))
	if err != nil {
		log.Printf("getAnalyzeExtra: the library's settings: %v", err)
		writeError(w, http.StatusInternalServerError, "extra lookup failed")
		return
	}
	if set.V2() {
		if rec.Library, err = h.extraLibraryOf(reqCtx(r), x.ID, x.ItemID); err != nil {
			log.Printf("getAnalyzeExtra: the library of %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "extra lookup failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, rec)
}

// putExtraStep serves PUT /api/analyze/extras/{id}/steps/{step}: a worker's
// report of the extra's transcode or package, with the body an item's step
// takes ({"status": "in_progress|done|not_applicable|failed", "error",
// "details"}). It moves the extra along (store.ReportExtraStep): the
// transcode's start to transcoding, its end (done, or not_applicable: the
// packager packages the source as it is) to transcoded, the package's start
// to packaging; a failed run is retried a backoff later by the retry policy,
// or failed when it has no attempt left. It answers {extraId, step, status,
// state}: 404 for an extra there is not or a removed one, 409 for one whose
// file is missing, 400 for a step or a status an extra does not have.
func (h *Handlers) putExtraStep(w http.ResponseWriter, r *http.Request) {
	id, step := chi.URLParam(r, "id"), chi.URLParam(r, "step")
	var body struct {
		Status  *string         `json:"status"`
		Error   *string         `json:"error"`
		Details json.RawMessage `json:"details"`
	}
	if err := decodeJSON(r, &body); err != nil || body.Status == nil || trimBlank(*body.Status) == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}
	pol := processing.DefaultPolicy()
	if h.d.Steps != nil {
		pol = h.d.Steps.Policy()
	}
	x, err := h.d.Store.ReportExtraStep(reqCtx(r), id, store.ExtraReport{Step: step, Status: trimBlank(*body.Status),
		Error: body.Error}, pol)
	switch {
	case errors.Is(err, store.ErrExtraGone), errors.Is(err, store.ErrNoExtras):
		writeError(w, http.StatusNotFound, "no such extra: "+id)
		return
	case errors.Is(err, store.ErrExtraMissing):
		writeError(w, http.StatusConflict, "the file of extra "+id+" is missing; the scanner keeps its state")
		return
	case errors.Is(err, processing.ErrBadStep), errors.Is(err, processing.ErrBadStatus):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		log.Printf("putExtraStep: extra=%s step=%s: %v", id, step, err)
		writeError(w, http.StatusInternalServerError, "step report failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"extraId": id, "step": step, "status": trimBlank(*body.Status), "state": x.State})
}

// extraPackageRoot is the folder of the extra id's package in the package
// store, as the packager writes it: packages/extras/<the id's first two
// characters>/<id>. An item's (packageRootFor) is by its type; an extra's is
// not.
func (h *Handlers) extraPackageRoot(id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(h.d.Cfg.PackagesRoot, "extras", shard, id)
}

// extraPackagingComplete serves POST /api/extras/{id}/packaging-complete, the
// packager's sink for an extra's package: it takes the package's manifest
// (the manifest of an item's package with type "extra", the extra's id as
// itemId, parentId and extraKind; no trickplay). The extra is ready (one
// whose file is missing stays missing, its package kept), its failures
// over, and it keeps its package's folder, how long it plays, its top
// rendition's codec and size, the highest BANDWIDTH its master playlist
// names (the top rendition's bit rate when the playlist cannot be read) and
// its size. A ready one is announced on catalog.extra.packaged, in the
// shape of an item event of its title; never on catalog.item.packaged,
// which says the title itself became watchable. It answers {extraId,
// itemId, packaged, durationMs}: 404 for an extra there is not or a removed
// one, 400 for a manifest of another package or one without video.
func (h *Handlers) extraPackagingComplete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	ctx := reqCtx(r)
	x, it, err := h.liveExtra(ctx, id)
	if err != nil {
		log.Printf("extraPackagingComplete: %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "extra lookup failed")
		return
	}
	if x == nil {
		writeError(w, http.StatusNotFound, "no such extra: "+id)
		return
	}
	var manifest map[string]any
	if err := decodeJSON(r, &manifest); err != nil || manifest == nil {
		writeError(w, http.StatusBadRequest, "the body is no manifest")
		return
	}
	for key, want := range map[string]string{"itemId": id, "parentId": x.ItemID, "type": "extra"} {
		if v := asString(manifest[key]); v != nil && *v != want {
			writeError(w, http.StatusBadRequest, "the manifest is another package's: its "+key+" is "+*v+", not "+want)
			return
		}
	}
	video := asListOfMap(asMap(manifest["renditions"])["video"])
	if len(video) == 0 {
		writeError(w, http.StatusBadRequest, "the manifest names no video rendition")
		return
	}
	top := video[0]
	for _, v := range video[1:] {
		h1, _ := asInt(v["height"])
		h0, _ := asInt(top["height"])
		if h1 > h0 {
			top = v
		}
	}
	root := h.extraPackageRoot(id)
	pkg := store.ExtraPackage{Path: root, VideoCodec: asString(top["codec"]), SizeBytes: sumPackageBytes(root)}
	if d, ok := asLong(manifest["durationMs"]); ok && d >= 0 {
		pkg.DurationMs = &d
	}
	if wd, ok := asInt(top["width"]); ok && wd > 0 {
		v := int32(wd)
		pkg.Width = &v
	}
	if ht, ok := asInt(top["height"]); ok && ht > 0 {
		v := int32(ht)
		pkg.Height = &v
	}
	if peak := peakBandwidth(root); peak != nil {
		pkg.PeakBandwidthBps = peak
	} else if bps, ok := asLong(top["bitrateBps"]); ok && bps > 0 {
		pkg.PeakBandwidthBps = &bps
	}
	done, err := h.d.Store.CompleteExtraPackage(ctx, id, pkg)
	switch {
	case errors.Is(err, store.ErrExtraGone):
		writeError(w, http.StatusNotFound, "no such extra: "+id)
		return
	case err != nil:
		log.Printf("extraPackagingComplete: %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "the package could not be recorded: "+err.Error())
		return
	}
	if done.State == model.ExtraReady {
		ev := events.ExtraPackagedEvent{ItemEvent: events.NewItemEvent(done.ItemID), ExtraID: done.ID, Kind: done.Kind}
		ev.Type, ev.Step, ev.Status, ev.Source = it.Type, "extra", "done", "katalog-manager"
		h.d.Events.Emit(ctx, events.TopicExtraPackaged, done.ID, ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"extraId": id, "itemId": done.ItemID, "packaged": true,
		"durationMs": pkg.DurationMs})
}

// peakBandwidth is the highest BANDWIDTH the package's master playlist
// names, in bits per second; nil when it cannot be read or names none.
func peakBandwidth(root string) *int64 {
	content, err := os.ReadFile(filepath.Join(root, "hls", "master.m3u8"))
	if err != nil {
		return nil
	}
	var peak int64
	for _, m := range bandwidthRe.FindAllSubmatch(content, -1) {
		if v, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil && v > peak {
			peak = v
		}
	}
	if peak <= 0 {
		return nil
	}
	return &peak
}
