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

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/chaptersdb"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/downloads"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
	"github.com/zaentrum/katalog-manager/internal/odownloader"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/rest"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/tmdb"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg := config.Load()
	ctx := context.Background()

	st, err := store.New(ctx, cfg.DatabaseURL, cfg.DatabaseUser, cfg.DatabasePassword)
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
	jwtVerifier, err := auth.NewJWTVerifier(ctx, cfg.OIDCIssuer, cfg.Audience, cfg.AudienceRequired, cfg.AuthDisabled)
	if err != nil {
		return err
	}
	authMW := auth.NewMiddleware(jwtVerifier, streamVerifier)

	// Integration services. Each no-ops cleanly when its feature is unconfigured.
	chapters := chaptersdb.New(cfg)
	enricher := tmdb.New(st, cfg, steps, chapters)
	scan := scanner.New(st, cfg, steps)
	gateway := downloads.NewGateway(cfg)
	actions := itemactions.New(st, cfg, steps)
	trailers := odownloader.New(st, cfg, steps)

	// Background workers (lifetime = server). Cancelled on shutdown.
	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

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

	// GraphQL.
	resolver := graph.NewResolver(st, cfg, graph.Services{
		Scanner:   scan,
		Enricher:  enricher,
		Packager:  actions,
		Validator: actions,
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
		rest.New(rest.Deps{Store: st, Cfg: cfg, Steps: steps}).Register(pr)
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

func writeText(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}
