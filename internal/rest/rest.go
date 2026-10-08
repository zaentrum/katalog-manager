// Package rest serves the endpoints that stay REST (SPEC §7 KEEP-REST):
// binary/byte-range (artwork, play, subtitles) and the analyzer/packager
// machine contracts (claim, putStep, segments, chapters, packaging-complete).
package rest

import (
	"github.com/go-chi/chi/v5"
	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Deps are the dependencies the REST handlers need.
type Deps struct {
	Store    *store.Store
	Cfg      config.Config
	Steps    *processing.Steps
	Events   *events.Producer // nil-safe: packaged-event emit no-ops without a bus
	Packager Packager         // nil: POST /api/items/{id}/package answers 503
	Extras   ExtraTaker       // nil: POST /api/extras answers 503
	Reencode Reencoder        // nil: /api/library/reencode answers 503
	TakeIn   TakeIn           // nil: a title that gets no package is taken in by the sweep alone; POST /api/items/{id}/takein answers 503
}

// Handlers groups the REST handlers.
type Handlers struct {
	d Deps
	// unrated is the setting that says whether a capped viewer is served
	// unrated titles (ratings.go).
	unrated unratedPolicy
}

func New(d Deps) *Handlers { return &Handlers{d: d} }

// Register mounts the KEEP-REST routes onto r, behind the authentication
// middleware, each for those who may call it (auth.Policy):
//
//   - viewers: the reads a player makes, artwork (chino-api proxies posters
//     and portraits here with the viewer's token or stream token; the console
//     reads them at /api/manage/artwork), playback and subtitles; a viewer
//     capped at an age (max_rating) is answered for a title rated above the
//     cap as for a title there is not (ratings.go);
//   - workers (an admin, or the platform's service account the analyzer,
//     transcoder and packager mint tokens with): the worker protocol, which
//     hands out paths on disk and writes the pipeline's results, and the
//     settings that are no secret;
//   - ingest (a worker, or an addon's service account): POST /api/ingest, and
//     POST /api/extras, which takes a file in as a title's extra;
//   - admins: POST /api/items/{id}/package, the packaging action an admin's
//     client forwards (chino-api's admin route), and POST
//     /api/items/{id}/takein, a title's take-in without a package.
//
// The workers' protocol of an extra (its record, its steps, its
// packaging-complete) is in the worker group too. Bodies are implemented in
// the per-area files (artwork.go, play.go, subtitles.go, analyzer.go,
// segments.go, chapters.go, packaging.go, package.go, settings.go,
// ingest.go, extras.go, migrations.go, reencode.go, takein.go).
func (h *Handlers) Register(r chi.Router) {
	pol := h.d.Cfg.Policy()

	// Binary / byte-range reads, for any signed-in caller.
	r.Group(func(r chi.Router) {
		r.Use(pol.Require(auth.Viewer))
		r.Get("/api/artwork/{itemId}/{kind}", h.getArtwork)
		// Also serve artwork READS under /api/manage — the same reason GraphQL is
		// mounted twice in cmd/server/routes.go. Only /api/manage is published by a
		// Route, so the bare /api/artwork path is unreachable from a browser and
		// every image in the catalog console 404'd, in every environment.
		//
		// The PUT is deliberately NOT mirrored: it is the analyzer uploading an
		// extracted keyframe from inside the cluster, and it has no reason to be
		// reachable from outside.
		r.Get("/api/manage/artwork/{itemId}/{kind}", h.getArtwork)
		// A person's portrait, read like a title's artwork, at both mount points.
		r.Get("/api/artwork/person/{personId}/profile", h.getPersonProfile)
		r.Get("/api/manage/artwork/person/{personId}/profile", h.getPersonProfile)
		r.Get("/api/play/{itemId}", h.getPlay)
		r.Get("/api/subtitles/items/{itemId}", h.listSubtitles)
		r.Get("/api/subtitles/{subId}", h.getSubtitle)
	})

	// The worker protocol, for the workers' service account and admins.
	r.Group(func(r chi.Router) {
		r.Use(pol.Require(auth.Worker))
		r.Put("/api/artwork/{itemId}/{kind}", h.putArtwork) // analyzer-extracted keyframe upload

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

		// An extra's worker protocol: its record, its steps' reports, and the
		// packager's sink for its package.
		r.Get("/api/analyze/extras/{id}", h.getAnalyzeExtra)
		r.Put("/api/analyze/extras/{id}/steps/{step}", h.putExtraStep)
		r.Post("/api/extras/{id}/packaging-complete", h.extraPackagingComplete)

		// The settings the workers read (the packager's language whitelist),
		// secrets left out.
		r.Get("/api/settings", h.getSettings)

		// The library's migration: the flip of a staged run, and its
		// reversal; the catalog's side of a run of the neutral names.
		r.Post("/api/library/migrations/{run}/adopt", h.adoptRun)
		r.Post("/api/library/migrations/{run}/revert", h.revertRun)
		r.Post("/api/library/migrations/{run}/names", h.namesRun)
		// The items' projections written now, as the migration's verify
		// needs them.
		r.Post("/api/library/projections", h.refreshProjections)
		// The re-encode queue: titles queued to be encoded again, and what
		// it holds.
		r.Post("/api/library/reencode", h.postReencode)
		r.Get("/api/library/reencode", h.getReencode)
	})

	// An admin's packaging action, as chino-api's admin route forwards it with
	// the admin's bearer token: what GraphQL's packageItem does.
	r.With(pol.Require(auth.Admin)).Post("/api/items/{id}/package", h.postPackage)
	// An admin's take-in of a title: its original into a version of its own,
	// with no package (the library's v2 layout).
	r.With(pol.Require(auth.Admin)).Post("/api/items/{id}/takein", h.postTakeIn)

	// External-file ingest: register a staged file (item + primary asset) and
	// emit discovered so it flows the pipeline. Neutral machine contract used by
	// importers/addons; the scanner's create path exposed as an API. An addon
	// calls it with its own service account, which carries the addon role.
	r.With(pol.Require(auth.Ingest)).Post("/api/ingest", h.ingest)

	// A file taken in as a title's extra (what GraphQL's addExtra does), for
	// an operator's tool, a deployment's Job or an addon.
	r.With(pol.Require(auth.Ingest)).Post("/api/extras", h.postExtra)
}
