// Package rest serves the endpoints that stay REST (SPEC §7 KEEP-REST):
// binary/byte-range (artwork, play, subtitles) and the analyzer/packager
// machine contracts (claim, putStep, segments, chapters, packaging-complete).
package rest

import (
	"github.com/go-chi/chi/v5"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Deps are the dependencies the REST handlers need.
type Deps struct {
	Store  *store.Store
	Cfg    config.Config
	Steps  *processing.Steps
	Events *events.Producer // nil-safe: packaged-event emit no-ops without a bus
}

// Handlers groups the REST handlers.
type Handlers struct {
	d Deps
}

func New(d Deps) *Handlers { return &Handlers{d: d} }

// Register mounts the KEEP-REST routes onto r. Bodies are implemented in the
// per-area files (artwork.go, play.go, subtitles.go, analyzer.go, segments.go,
// chapters.go, packaging.go); the skeleton registers them so routing + auth are
// wired from day one.
func (h *Handlers) Register(r chi.Router) {
	// Binary / byte-range
	r.Get("/api/artwork/{itemId}/{kind}", h.getArtwork)
	r.Put("/api/artwork/{itemId}/{kind}", h.putArtwork) // analyzer-extracted keyframe upload
	r.Get("/api/play/{itemId}", h.getPlay)
	r.Get("/api/subtitles/items/{itemId}", h.listSubtitles)
	r.Get("/api/subtitles/{subId}", h.getSubtitle)

	// Analyzer worker protocol. The batch POST /api/analyze/claim poll endpoint
	// was removed: the pipeline is now Kafka-triggered (pure event-driven), so
	// workers consume item events and fetch detail via GET /api/analyze/items/{id}.
	r.Get("/api/analyze/items/{id}", h.getAnalyzeItem)
	r.Get("/api/analyze/items/{id}/steps", h.getSteps)
	r.Post("/api/analyze/items/{id}/steps/skip", h.skipSteps)
	r.Put("/api/analyze/items/{id}/steps/{step}", h.putStep)
	r.Post("/api/analyze/items/{id}/fail", h.failItem)
	r.Get("/api/analyze/items/{id}/siblings", h.getSiblings)
	r.Post("/api/analyze/series/{id}/reset", h.resetSeries)

	// Fused analyzer output
	r.Put("/api/segments/items/{itemId}", h.putSegments)
	r.Delete("/api/segments/items/{itemId}", h.deleteSegments)
	r.Put("/api/chapters/items/{itemId}", h.putChapters)
	r.Delete("/api/chapters/items/{itemId}", h.deleteChapters)

	// Packager machine sink
	r.Post("/api/items/{id}/packaging-complete", h.packagingComplete)

	// External-file ingest: register a staged file (item + primary asset) and
	// emit discovered so it flows the pipeline. Neutral machine contract used by
	// importers/addons; the scanner's create path exposed as an API.
	r.Post("/api/ingest", h.ingest)
}
