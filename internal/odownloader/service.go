package odownloader

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Service implements graph.TrailerFetcher and runs the oDownloader ingestion
// poller. It owns the com_nalet_katalog_trailerjobs table (created best-effort
// at startup since the live schema is missing it — SPEC §1.3 / migration 028).
type Service struct {
	st     *store.Store
	cfg    config.Config
	steps  *processing.Steps
	client *client
}

// New constructs the service and best-effort ensures the trailerjobs table
// exists. The poller is started separately by main via RunPoller. steps is
// accepted for signature symmetry with the other integration services; the
// trailer flow does not write processing steps.
func New(st *store.Store, cfg config.Config, steps *processing.Steps) *Service {
	s := &Service{st: st, cfg: cfg, steps: steps, client: newClient(cfg)}
	s.ensureSchema(context.Background())
	return s
}

// ensureSchema creates com_nalet_katalog_trailerjobs if it is absent. The DDL
// mirrors db/migrations/028_go_rewrite.sql (lowercase, Postgres-folded). Failures
// are logged and ignored — a deploy that already ran the migration is unaffected,
// and a transiently-unavailable DB at boot must not crash the app.
func (s *Service) ensureSchema(ctx context.Context) {
	const ddl = `
CREATE TABLE IF NOT EXISTS com_nalet_katalog_trailerjobs (
  id              VARCHAR(36) PRIMARY KEY,
  createdat       TIMESTAMP,
  modifiedat      TIMESTAMP,
  item_id         VARCHAR(36) NOT NULL,
  trailer_link_id VARCHAR(36),
  source_url      VARCHAR(2048) NOT NULL,
  package_id      VARCHAR(255),
  download_id     VARCHAR(255),
  state           VARCHAR(20) NOT NULL DEFAULT 'queued',
  attempts        INTEGER DEFAULT 0,
  started_at      TIMESTAMP,
  finished_at     TIMESTAMP,
  bytes_done      BIGINT,
  bytes_total     BIGINT,
  message         VARCHAR(500),
  final_path      VARCHAR(2048)
);
CREATE INDEX IF NOT EXISTS idx_trailerjobs_item  ON com_nalet_katalog_trailerjobs (item_id);
CREATE INDEX IF NOT EXISTS idx_trailerjobs_state ON com_nalet_katalog_trailerjobs (state);`
	if _, err := s.st.Pool().Exec(ctx, ddl); err != nil {
		log.Printf("odownloader: ensure trailerjobs schema (best-effort) failed: %v", err)
	}
}

// FetchTrailers reads the item's undownloaded trailer links, de-dups by URL,
// enqueues each URL as its own oDownloader package, and records one trailerjobs
// row per accepted URL. It mirrors TrailerActionsController.fetchTrailers +
// TrailerIngestionService.enqueue.
//
// Disabled (URL or TOKEN unset) → {Enqueued:0, Message:"odownloader disabled"},
// no error. Unknown item → {Enqueued:0, Message:"unknown item: <id>"}, no error
// (the GraphQL surface has no 404; the Java controller returned 404 — here we
// return a benign result so the resolver does not surface an internal error).
func (s *Service) FetchTrailers(ctx context.Context, id string) (graph.FetchTrailersResult, error) {
	res := graph.FetchTrailersResult{ItemID: id, JobIDs: []string{}}

	if !s.cfg.ODownloaderEnabled() {
		res.Message = ptr("odownloader disabled")
		return res, nil
	}

	// Load item title (also confirms the item exists).
	var title string
	err := s.st.Pool().QueryRow(ctx,
		`SELECT title FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&title)
	if err == pgx.ErrNoRows {
		res.Message = ptr("unknown item: " + id)
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("load item: %w", err)
	}
	res.Title = &title

	// Undownloaded trailer links (localpath NULL or empty), in publishedat order
	// to mirror the default ItemTrailerLinks sort; de-dup by URL keeping the
	// first link id seen for each URL.
	rows, err := s.st.Pool().Query(ctx,
		`SELECT id, url FROM com_nalet_katalog_itemtrailerlinks
		 WHERE item_id = $1 AND (localpath IS NULL OR localpath = '')`, id)
	if err != nil {
		return res, fmt.Errorf("load trailer links: %w", err)
	}
	defer rows.Close()

	var uniqueURLs []string
	linkIDByURL := map[string]string{}
	for rows.Next() {
		var linkID, u string
		if err := rows.Scan(&linkID, &u); err != nil {
			return res, fmt.Errorf("scan trailer link: %w", err)
		}
		if u == "" {
			continue
		}
		if _, seen := linkIDByURL[u]; !seen {
			linkIDByURL[u] = linkID
			uniqueURLs = append(uniqueURLs, u)
		}
	}
	if err := rows.Err(); err != nil {
		return res, fmt.Errorf("iterate trailer links: %w", err)
	}

	if len(uniqueURLs) == 0 {
		res.Message = ptr("No trailers to fetch. Either none are known yet " +
			"(click Refresh from TMDB first) or every known trailer is already downloaded.")
		return res, nil
	}

	jobIDs, packageID, msg, err := s.enqueue(ctx, id, title, uniqueURLs, linkIDByURL)
	if err != nil {
		return res, err
	}
	res.JobIDs = jobIDs
	res.Enqueued = int32(len(jobIDs))
	if packageID != "" {
		res.PackageID = &packageID
	}
	res.Message = &msg
	return res, nil
}

// enqueue posts each URL as its own oDownloader package (one URL = one package so
// the poller can map the variant fan-out back to a single trailerjob) and inserts
// a trailerjobs row per accepted URL. Mirrors TrailerIngestionService.enqueue.
func (s *Service) enqueue(ctx context.Context, itemID, packageName string, urls []string,
	linkIDByURL map[string]string) (jobIDs []string, firstPackageID, message string, err error) {

	jobIDs = []string{}
	for _, u := range urls {
		result := s.client.addLinks(ctx, []string{u}, packageName, "katalog item="+itemID)
		if result == nil {
			log.Printf("odownloader: enqueue skip url=%s (oDownloader rejected)", u)
			continue
		}
		if firstPackageID == "" {
			firstPackageID = result.PackageID
		}
		var jobID string
		row := s.st.Pool().QueryRow(ctx,
			`INSERT INTO com_nalet_katalog_trailerjobs
			 (id, createdat, modifiedat, item_id, trailer_link_id, source_url,
			  package_id, download_id, state, attempts)
			 VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, NULL, 'queued', 0)
			 RETURNING id`,
			itemID, nullable(linkIDByURL[u]), u, result.PackageID)
		if err = row.Scan(&jobID); err != nil {
			return jobIDs, firstPackageID, "", fmt.Errorf("insert trailerjob: %w", err)
		}
		jobIDs = append(jobIDs, jobID)
	}

	if len(jobIDs) == 0 {
		return jobIDs, "", "oDownloader rejected every URL. See katalog logs.", nil
	}
	return jobIDs, firstPackageID, fmt.Sprintf("Queued %d download(s).", len(jobIDs)), nil
}

func ptr(s string) *string { return &s }

// nullable returns nil for an empty string so the column is written NULL.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
