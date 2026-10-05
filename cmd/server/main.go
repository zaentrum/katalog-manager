// Command server is the katalog-manager service: a GraphQL API (graph-gophers)
// for the catalog-management surface plus the retained REST endpoints for
// binary/byte-range and analyzer/packager machine contracts (SPEC §7).
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/chaptersdb"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/extras"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/rest"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/stream"
	"github.com/zaentrum/katalog-manager/internal/tmdb"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// Tenant topic prefix — must be set before any Kafka producer/consumer starts.
	events.Configure(cfg.KafkaTopicPrefix)

	// Server-lifetime context. Cancelled on shutdown so background workers —
	// including the OIDC discovery retry goroutine — stop with the server
	// instead of lingering until process exit.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	st, err := store.New(bgCtx, cfg.DatabaseURL, cfg.DatabaseUser, cfg.DatabasePassword)
	if err != nil {
		return err
	}
	defer st.Close()

	// The deletion log (migration 029). Item deletes record themselves there in
	// their own transaction and fail without it, so a missing table must be loud;
	// reads keep working, so it does not stop the service. Where this role may
	// not create tables, apply db/migrations/029_deleted_items.sql by hand.
	if err := st.EnsureDeletionLog(bgCtx); err != nil {
		log.Printf("catalog: the deletion log (db/migrations/029_deleted_items.sql) is missing and could not be created: %v; item deletes fail until it exists", err)
	}
	// People with their TMDB identity and details (migration 030). Without it
	// the catalog works as before: credits link people by name, and nothing
	// about a person is fetched or kept. Where this role may not alter the
	// people table, apply db/migrations/030_people.sql by hand.
	if err := st.EnsurePeople(bgCtx); err != nil {
		log.Printf("catalog: the people migration (db/migrations/030_people.sql) is missing and could not be applied: %v; people keep only their names until it is", err)
	}
	// A title's locked fields (migration 031): with "credits" among them, TMDB
	// neither adds nor drops a credit of the title. Without the column only
	// metadatalocked keeps credits as they are.
	if err := st.EnsureItemLockedFields(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/031_item_locked_fields.sql is missing and could not be applied: %v; only metadataLocked keeps a title's credits until it is", err)
	}
	// What a credit says besides its role (migration 032): the job, character,
	// order and episodes TMDB gives it. Without it credits keep their roles
	// alone, as before.
	if err := st.EnsureCreditDetails(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/032_credit_details.sql is missing and could not be applied: %v; credits keep only their roles until it is", err)
	}
	// What the service keeps to retry a processing step (migration 033): its
	// failures in a row, last error and next retry. Without it the pipeline
	// runs as before, and nothing retries a step.
	if err := st.EnsureStepRetries(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/033_step_retries.sql is missing and could not be applied: %v; no step is retried until it is", err)
	}
	// Who runs a scan job, and its scanner's last word (migration 034). Without
	// it a scan a restart cuts short says running for ever, as before.
	if err := st.EnsureScanJobRunner(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/034_scan_job_runner.sql is missing and could not be applied: %v; a scan a restart cuts short says running until it is", err)
	}
	// A title's age rating (migration 036): the certification TMDB gives it,
	// its country, the minimum age it means and an admin's override. Without
	// it no title is rated, and a viewer with a rating cap is served nothing.
	if err := st.EnsureItemRatings(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/036_item_ratings.sql is missing and could not be applied: %v; no title is rated, and a capped viewer is served nothing, until it is", err)
	}
	// The languages of a title's tracks (migration 037): its source's audio
	// and subtitle tracks as the packager read them, and an admin's language
	// of a track, which the packager labels it with. Without it the packager
	// labels every track as its source tags it, as before.
	if err := st.EnsureTrackLanguages(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/037_track_languages.sql is missing and could not be applied: %v; no track's language can be set until it is", err)
	}
	// Whether a subtitle is forced (migration 038), as the packager says of
	// each subtitle of a package. Without it no subtitle is kept as forced.
	if err := st.EnsureSubtitleForced(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/038_subtitle_forced.sql is missing and could not be applied: %v; no subtitle is kept as forced until it is", err)
	}
	// A title's extras (migration 039): its trailers and other bonus
	// material, each packaged on its own. Without it no extra is taken in.
	if err := st.EnsureItemExtras(bgCtx); err != nil {
		log.Printf("catalog: migration db/migrations/039_item_extras.sql is missing and could not be applied: %v; no extra is taken in until it is", err)
	}
	dropRetiredJobTables(bgCtx, st)

	steps := processing.New(st.Pool()).WithPolicy(cfg.RetryPolicy())

	// Auth: bearer JWT (issuer-only MVP) + stream-token (artwork only).
	streamVerifier, err := auth.NewStreamVerifier(cfg.StreamSigningKey)
	if err != nil {
		return err
	}
	jwtVerifier, err := auth.NewJWTVerifier(bgCtx, cfg.OIDCIssuer, cfg.Audience, cfg.AudienceRequired, cfg.AuthDisabled)
	if err != nil {
		return err
	}
	authMW := auth.NewMiddleware(jwtVerifier, streamVerifier)
	if cfg.AuthDisabled || cfg.OIDCIssuer == "" {
		log.Printf("auth: OFF (AUTH_DISABLED or no issuer) — every caller may do anything")
	} else {
		log.Printf("auth: admins carry the %s role and addons the %s role (at %s); the service account is %v",
			cfg.AdminRole, cfg.AddonRole, cfg.RolesClaim, cfg.ServiceClients)
	}

	// Catalog pipeline event producer (nil-safe no-op when no brokers). The
	// scanner emits discovered through it; the enricher emits enriched.
	var eventProducer *events.Producer
	if cfg.CatalogEventsEnabled {
		brokers := events.SplitBrokers(cfg.KafkaBrokers)
		tlsCfg, err := events.MaybeTLS(cfg.KafkaCertDir)
		if err != nil {
			log.Printf("catalog events: certs present but unreadable (%v); producing over PLAINTEXT", err)
			tlsCfg = nil
		}
		eventProducer = events.NewProducer(brokers, tlsCfg)
		defer eventProducer.Close()
	}

	// Integration services. Each no-ops cleanly when its feature is unconfigured.
	chapters := chaptersdb.New(cfg)
	// Resolve enrichment API keys from the settings table at runtime (the
	// `tmdb.api_key` / `omdb.api_key` / `fanart.api_key` / `fanart.client_key`
	// settings override the env/build defaults, so the settings editor can change
	// them without a restart).
	settingLookup := func(ctx context.Context, key string) (string, bool) {
		row, err := st.GetSettingByKey(ctx, key)
		if err != nil || row == nil {
			return "", false
		}
		return row.ValueText, true
	}
	enricher := tmdb.New(st, cfg, steps, chapters, settingLookup)
	scan := scanner.New(st, cfg, steps, eventProducer)
	// A scan runs in the process that started it, so one a previous process
	// left running never ends: it is failed now, saying so, before this
	// process starts a scan of its own. A scan another host runs is left to
	// the reaper, which fails it once it is silent past the scan's timeout.
	if n, err := scan.FailInterrupted(bgCtx); err != nil {
		log.Printf("catalog: the scans a previous process left running could not be failed: %v", err)
	} else if n > 0 {
		log.Printf("catalog: %d scans a previous process left running are failed (%s)", n, scanner.InterruptedReason)
	}
	actions := itemactions.New(st, cfg, steps, eventProducer)
	// A title's extras: taken in by an operator or the scanner, and packaged
	// on a chain of their own (catalog.extra.*), retried by the same policy.
	extrasSvc := extras.New(st, cfg, cfg.RetryPolicy(), eventProducer)
	// The pipeline heals itself: a failed step is retried by sending its
	// trigger event again, after a backoff, a bounded number of times, and a
	// step whose worker went silent past its timeout is reaped into a failure.
	retries := retry.New(st, cfg.RetryPolicy(), eventProducer, cfg.RetryInterval)

	// Background workers (lifetime = server) share bgCtx, cancelled on shutdown.
	// Keep the people and titles the catalog holds fresh from TMDB's change
	// lists, without crawling TMDB: every TMDB_REFRESH_INTERVAL (default 24h),
	// idle while there is no TMDB key.
	go enricher.RunChangeSync(bgCtx, cfg.TMDBRefreshInterval)
	go retries.Run(bgCtx)
	// Event-driven enrichment: consume stube.catalog.item.discovered, enrich the
	// item synchronously, then emit stube.catalog.item.enriched to trigger analyze.
	// This replaces the old 60s enrichment poll ticker (pure-Kafka triggers).
	if cfg.CatalogEventsEnabled && cfg.TMDBEnabled() {
		go events.Consume(bgCtx, events.SplitBrokers(cfg.KafkaBrokers), cfg.KafkaCertDir,
			"katalog-enricher", []string{events.TopicDiscovered},
			func(ctx context.Context, _ string, ev events.ItemEvent) error {
				status, _, err := enricher.EnrichOne(ctx, ev.ItemID)
				if err != nil {
					return err
				}
				// done|not_found both mean "enrichment finished, proceed to analyze".
				// failed|skipped do not advance the pipeline. A series parent is
				// metadata-only (no primary playback asset) — it enriches but must
				// NOT enter analyze/transcode/package, so gate on a playable file.
				switch status {
				case "done", "not_found":
					if hasPrimaryAsset(ctx, st, ev.ItemID) {
						out := events.NewItemEvent(ev.ItemID)
						out.Type = ev.Type
						out.Step = "analyze"
						out.Status = status
						eventProducer.EmitItem(ctx, events.TopicEnriched, out)
					}
				}
				return nil
			})
	}

	// GraphQL.
	resolver := graph.NewResolver(st, cfg, graph.Services{
		Scanner:   scan,
		Enricher:  enricher,
		People:    enricher,
		Ratings:   enricher,
		Packager:  actions,
		Validator: actions,
		Remover:   actions,
		Pipeline:  retries,
		// setSecretSetting checks a TMDB token with TMDB before it stores it.
		Secrets: enricher,
		Extras:  extrasSvc,
	})
	schema := graph.MustSchema(resolver)

	// Router.
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)

	// Health (public — also whitelisted in the auth middleware).
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "ok\n") })
	r.Get("/actuator/health/liveness", func(w http.ResponseWriter, _ *http.Request) { writeText(w, "{\"status\":\"UP\"}") })
	r.Get("/actuator/health/readiness", func(w http.ResponseWriter, req *http.Request) {
		if err := st.Ping(req.Context()); err != nil {
			http.Error(w, "{\"status\":\"DOWN\"}", http.StatusServiceUnavailable)
			return
		}
		writeText(w, "{\"status\":\"UP\"}")
	})

	// Live catalog stream: a per-pod Kafka tail (latest offset) fans thin
	// catalog.updated notifications out to the console over SSE, so it refreshes
	// the moment the pipeline moves instead of polling. No brokers => inert.
	broker := stream.NewBroker()
	if cfg.CatalogEventsEnabled {
		host, _ := os.Hostname()
		if host == "" {
			host = "unknown"
		}
		go events.ConsumeLatest(bgCtx, events.SplitBrokers(cfg.KafkaBrokers), cfg.KafkaCertDir,
			"katalog-stream-"+host,
			[]string{events.TopicDiscovered, events.TopicEnriched, events.TopicAnalyzed, events.TopicTranscoded, events.TopicPackaged, events.TopicRemoved},
			func(_ context.Context, topic string, ev events.ItemEvent) error {
				broker.Publish(stream.Note{ItemID: ev.ItemID, ItemType: ev.Type, Phase: stream.PhaseOf(topic)})
				return nil
			})
	}

	// Authenticated surface.
	routes(r, authMW.Handler, cfg.Policy(), schema, broker.Handler,
		rest.New(rest.Deps{Store: st, Cfg: cfg, Steps: steps, Events: eventProducer, Packager: actions}))

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("katalog-manager listening on :%s (graphql /query, rest /api/*)", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// dropRetiredJobTables applies migration 035 at startup: the job tables of an
// integration the core no longer carries go while they are empty. One that
// holds rows is kept, and the log says so, once, with the tables it dropped.
func dropRetiredJobTables(ctx context.Context, st *store.Store) {
	dropped, kept, err := st.DropRetiredJobTables(ctx)
	if err != nil {
		log.Printf("catalog: migration db/migrations/035_retired_job_tables.sql could not be applied: %v; the retired job tables stay until it is", err)
		return
	}
	if len(dropped) > 0 {
		log.Printf("catalog: dropped the empty job tables of a retired integration (migration 035): %s", strings.Join(dropped, ", "))
	}
	if len(kept) > 0 {
		held := make([]string, 0, len(kept))
		for _, t := range kept {
			held = append(held, fmt.Sprintf("%s (%d rows)", t.Name, t.Rows))
		}
		log.Printf("catalog: kept the job tables of a retired integration that hold rows: %s; nothing reads or writes them, "+
			"and migration 035 drops a table only once it is empty", strings.Join(held, ", "))
	}
}

// hasPrimaryAsset reports whether an item has a primary playback asset (a
// playable file). Series parents are metadata-only and have none, so this gates
// them out of the analyze/transcode/package pipeline.
//
// Error handling is deliberately fail-OPEN: only a clean ErrNoRows (the genuine
// metadata-only series-parent case) blocks advancement. Any OTHER DB fault
// (pool exhaustion, deadline, reset) must NOT silently strand a playable item —
// enrichment is one-shot per discovered event (no retry/poll), so a movie that
// missed its analyze emission would never become playable. A spurious analyze on
// a series parent is far cheaper, so on an unexpected error we log and advance.
func hasPrimaryAsset(ctx context.Context, st *store.Store, itemID string) bool {
	var one int
	err := st.Pool().QueryRow(ctx,
		`SELECT 1 FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true LIMIT 1`,
		itemID).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, pgx.ErrNoRows):
		return false
	default:
		log.Printf("catalog: hasPrimaryAsset(%s) errored (%v); advancing to analyze (fail-open)", itemID, err)
		return true
	}
}

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}
