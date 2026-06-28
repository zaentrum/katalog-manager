-- 028_go_rewrite.sql — schema gaps the Go rewrite needs (idempotent).
-- The Go service reuses the existing com_nalet_katalog_* tables unchanged; this
-- only fills two gaps found against the live schema (SPEC §1.3).

-- (1) Trailer crawler jobs. The table is defined in 020_trailerjobs.sql but was
-- absent from the live database; recreate it so fetch-trailers + the oDownloader
-- poller have a write target. Lowercase identifiers (Postgres-folded).
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
CREATE INDEX IF NOT EXISTS idx_trailerjobs_state ON com_nalet_katalog_trailerjobs (state);

-- (2) DownloadJobs upsert key. The Kafka read-model consumer upserts on
-- (adapter, clientjobid); the live table only had a PK on id. A unique index
-- lets the consumer ON CONFLICT (adapter, clientjobid) cleanly. (The Go consumer
-- also derives a deterministic id, so this is belt-and-suspenders.)
CREATE UNIQUE INDEX IF NOT EXISTS idx_downloadjobs_client
  ON com_nalet_katalog_downloadjobs (adapter, clientjobid);
