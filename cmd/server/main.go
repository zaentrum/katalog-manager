// Command server is the katalog-manager service: a GraphQL API (graph-gophers)
// for the catalog-management surface plus the retained REST endpoints for
// binary/byte-range and analyzer/packager machine contracts (SPEC §7).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/graph-gophers/graphql-go/relay"
	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/chaptersdb"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/downloads"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
	"github.com/zaentrum/katalog-manager/internal/odownloader"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/rest"
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
	cfg := config.Load()
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

	steps := processing.New(st.Pool())

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
	gateway := downloads.NewGateway(cfg)
	actions := itemactions.New(st, cfg, steps, eventProducer)
	trailers := odownloader.New(st, cfg, steps)

	// Background workers (lifetime = server) share bgCtx, cancelled on shutdown.
	if cfg.DownloadEventsEnabled {
		consumer := downloads.NewConsumer(st, cfg)
		go func() {
			if err := consumer.Run(bgCtx); err != nil {
				log.Printf("download-events consumer stopped: %v", err)
			}
		}()
	}
	if cfg.ODownloaderEnabled() {
		go trailers.RunPoller(bgCtx)
	}
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
		Packager:  actions,
		Validator: actions,
		Remover:   actions,
		Trailers:  trailers,
		DLGateway: gateway,
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
	r.Group(func(pr chi.Router) {
		pr.Use(authMW.Handler)
		gqlHandler := &relay.Handler{Schema: schema}
		pr.Handle("/query", gqlHandler)
		pr.Handle("/graphql", gqlHandler)
		// Also serve GraphQL under the /api/manage prefix so it resolves behind
		// the demo's path-routing (the portal's katalog console posts there).
		pr.Handle("/api/manage/query", gqlHandler)
		pr.Handle("/api/manage/graphql", gqlHandler)
		pr.Get("/api/manage/stream", broker.Handler)
		rest.New(rest.Deps{Store: st, Cfg: cfg, Steps: steps, Events: eventProducer}).Register(pr)
	})

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
