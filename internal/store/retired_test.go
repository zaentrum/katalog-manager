package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// retiredSchema is what a catalog may hold of the retired integration: the
// base schema's read model of its job events and the view it is read through,
// as the cds-generated DDL declares them, and what 028 once added, a job table
// with its indexes and a unique index on the read model.
const retiredSchema = `
CREATE TABLE IF NOT EXISTS com_nalet_katalog_DownloadJobs (
  ID VARCHAR(36) NOT NULL, createdAt TIMESTAMP, createdBy VARCHAR(255), modifiedAt TIMESTAMP, modifiedBy VARCHAR(255),
  adapter VARCHAR(40) NOT NULL, clientJobId VARCHAR(255) NOT NULL, title VARCHAR(500), wantedItemId VARCHAR(80),
  state VARCHAR(20) NOT NULL DEFAULT 'queued', progressPct DECIMAL(5, 2) DEFAULT 0, downloadedBytes BIGINT DEFAULT 0,
  sizeBytes BIGINT, speedBps BIGINT, etaSec INTEGER, files TEXT, errorMessage TEXT,
  startedAt TIMESTAMP, completedAt TIMESTAMP, lastEventAt TIMESTAMP,
  PRIMARY KEY(ID)
);
CREATE OR REPLACE VIEW KatalogService_DownloadJobs AS SELECT
  DownloadJobs_0.ID, DownloadJobs_0.adapter, DownloadJobs_0.clientJobId, DownloadJobs_0.state,
  CASE DownloadJobs_0.state WHEN 'failed' THEN 1 WHEN 'queued' THEN 2 WHEN 'completed' THEN 3 ELSE 0 END AS stateCriticality
FROM com_nalet_katalog_DownloadJobs AS DownloadJobs_0;
CREATE TABLE IF NOT EXISTS com_nalet_katalog_trailerjobs (
  id VARCHAR(36) PRIMARY KEY, createdat TIMESTAMP, modifiedat TIMESTAMP, item_id VARCHAR(36) NOT NULL,
  trailer_link_id VARCHAR(36), source_url VARCHAR(2048) NOT NULL, package_id VARCHAR(255), download_id VARCHAR(255),
  state VARCHAR(20) NOT NULL DEFAULT 'queued', attempts INTEGER DEFAULT 0, started_at TIMESTAMP, finished_at TIMESTAMP,
  bytes_done BIGINT, bytes_total BIGINT, message VARCHAR(500), final_path VARCHAR(2048)
);
CREATE INDEX IF NOT EXISTS idx_trailerjobs_item  ON com_nalet_katalog_trailerjobs (item_id);
CREATE INDEX IF NOT EXISTS idx_trailerjobs_state ON com_nalet_katalog_trailerjobs (state);
CREATE UNIQUE INDEX IF NOT EXISTS idx_downloadjobs_client
  ON com_nalet_katalog_downloadjobs (adapter, clientjobid);
`

// retiredObjects are the objects of the retired integration the catalog
// holds, by name.
func retiredObjects(t *testing.T, st *store.Store) string {
	t.Helper()
	var out []string
	for _, name := range []string{"com_nalet_katalog_trailerjobs", "idx_trailerjobs_item", "idx_trailerjobs_state",
		"com_nalet_katalog_downloadjobs", "katalogservice_downloadjobs", "idx_downloadjobs_client"} {
		if storetest.Count(t, st, `SELECT count(*) FROM (SELECT to_regclass($1) AS c) x WHERE c IS NOT NULL`, name) == 1 {
			out = append(out, name)
		}
	}
	return strings.Join(out, " ")
}

const allRetired = "com_nalet_katalog_trailerjobs idx_trailerjobs_item idx_trailerjobs_state " +
	"com_nalet_katalog_downloadjobs katalogservice_downloadjobs idx_downloadjobs_client"

// 035 drops the retired job tables while they are empty, with their view and
// indexes, and says which; then there is nothing to do, and running the file
// again changes nothing. A base schema that creates the read model again gets
// it dropped again at the next start.
func TestRetiredJobTablesGoWhileEmpty(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.Exec(t, st, retiredSchema)
	if got := retiredObjects(t, st); got != allRetired {
		t.Fatalf("the fixture holds %q", got)
	}
	dropped, kept, err := st.DropRetiredJobTables(ctx)
	if err != nil || !slices.Equal(dropped, []string{"com_nalet_katalog_trailerjobs", "com_nalet_katalog_downloadjobs"}) || kept != nil {
		t.Fatalf("DropRetiredJobTables: dropped %q, kept %v, %v; want both dropped", dropped, kept, err)
	}
	if got := retiredObjects(t, st); got != "" {
		t.Errorf("left after 035: %q", got)
	}
	if dropped, kept, err := st.DropRetiredJobTables(ctx); err != nil || dropped != nil || kept != nil {
		t.Errorf("a second time: dropped %q, kept %v, %v; want nothing to do", dropped, kept, err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.RetiredJobTables); err != nil {
			t.Fatalf("applying 035 to a catalog without the tables, %d. time: %v", run, err)
		}
	}

	// The base schema of a deployment that applies it at every start.
	storetest.Exec(t, st, `CREATE TABLE IF NOT EXISTS com_nalet_katalog_DownloadJobs (ID VARCHAR(36) NOT NULL PRIMARY KEY,
		adapter VARCHAR(40) NOT NULL, clientJobId VARCHAR(255) NOT NULL, state VARCHAR(20) NOT NULL DEFAULT 'queued')`)
	storetest.Exec(t, st, `CREATE OR REPLACE VIEW KatalogService_DownloadJobs AS SELECT ID, state FROM com_nalet_katalog_DownloadJobs`)
	if dropped, kept, err := st.DropRetiredJobTables(ctx); err != nil || !slices.Equal(dropped, []string{"com_nalet_katalog_downloadjobs"}) || kept != nil {
		t.Errorf("the read model created again: dropped %q, kept %v, %v", dropped, kept, err)
	}
	if got := retiredObjects(t, st); got != "" {
		t.Errorf("left after 035 the second time: %q", got)
	}
}

// A retired job table that holds a row is kept as it is, its view and its
// index with it, and said at every start; the empty one goes all the same.
func TestARetiredJobTableThatHoldsRowsIsKept(t *testing.T) {
	ctx := context.Background()

	st := storetest.Open(t)
	storetest.Exec(t, st, retiredSchema)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_downloadjobs (id, adapter, clientjobid, state, title)
		VALUES ('d1', 'x', 'job-1', 'completed', 'kept')`)
	for run := 1; run <= 2; run++ {
		dropped, kept, err := st.DropRetiredJobTables(ctx)
		wantDropped := []string{"com_nalet_katalog_trailerjobs"}
		if run == 2 {
			wantDropped = nil
		}
		if err != nil || !slices.Equal(dropped, wantDropped) ||
			!slices.Equal(kept, []store.RetiredTable{{Name: "com_nalet_katalog_downloadjobs", Rows: 1}}) {
			t.Fatalf("%d. start: dropped %q, kept %v, %v; want %q dropped, the read model kept with its row", run, dropped, kept, err, wantDropped)
		}
	}
	if got := retiredObjects(t, st); got != "com_nalet_katalog_downloadjobs katalogservice_downloadjobs idx_downloadjobs_client" {
		t.Errorf("left: %q, want the read model with its view and index", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM katalogservice_downloadjobs WHERE id = 'd1'`); n != 1 {
		t.Error("the kept row is gone")
	}

	other := storetest.Open(t)
	storetest.Exec(t, other, retiredSchema)
	storetest.Exec(t, other, `INSERT INTO com_nalet_katalog_trailerjobs (id, item_id, source_url) VALUES ('t1', 'm1', 'u1'), ('t2', 'm2', 'u2')`)
	dropped, kept, err := other.DropRetiredJobTables(ctx)
	if err != nil || !slices.Equal(dropped, []string{"com_nalet_katalog_downloadjobs"}) ||
		!slices.Equal(kept, []store.RetiredTable{{Name: "com_nalet_katalog_trailerjobs", Rows: 2}}) {
		t.Fatalf("the job table holding rows: dropped %q, kept %v, %v", dropped, kept, err)
	}
	if got := retiredObjects(t, other); got != "com_nalet_katalog_trailerjobs idx_trailerjobs_item idx_trailerjobs_state" {
		t.Errorf("left: %q, want the job table with its indexes", got)
	}
}

// Instances that start together drop the retired tables once between them,
// and none fails for it: one waits for the other to commit. A reader holds the
// job table, so that both are under way, waiting, before either drops.
func TestRetiredJobTablesGoOnceWhenInstancesStartTogether(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, retiredSchema)
	ctx := context.Background()
	reader, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback(ctx)
	if _, err := reader.Exec(ctx, `LOCK TABLE com_nalet_katalog_trailerjobs IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, _, err := st.DropRetiredJobTables(ctx)
			errs <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for storetest.Count(t, st, `SELECT count(*) FROM pg_stat_activity
		WHERE wait_event_type = 'Lock' AND query LIKE '%035_retired_job_tables%'`) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the two starts never both waited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := reader.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("a start failed: %v", err)
		}
	}
	if got := retiredObjects(t, st); got != "" {
		t.Errorf("left: %q", got)
	}
}

// 035 drops nothing it does not know of: an object of someone else's that
// hangs off a retired table fails the migration, and every table stays.
func TestRetiredJobTablesGoWithNothingElse(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, retiredSchema)
	storetest.Exec(t, st, `CREATE VIEW someones_report AS SELECT count(*) AS jobs FROM com_nalet_katalog_downloadjobs`)
	if dropped, kept, err := st.DropRetiredJobTables(context.Background()); err == nil || dropped != nil || kept != nil {
		t.Fatalf("DropRetiredJobTables with a view of someone else's on the read model: dropped %q, kept %v, %v; want an error", dropped, kept, err)
	}
	if got := retiredObjects(t, st); got != allRetired {
		t.Errorf("left: %q, want everything as it was", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM someones_report`); n != 1 {
		t.Error("someone else's view is gone")
	}

	// A catalog without the retired tables has nothing to drop.
	none := storetest.Open(t)
	if dropped, kept, err := none.DropRetiredJobTables(context.Background()); err != nil || dropped != nil || kept != nil {
		t.Errorf("a catalog without them: dropped %q, kept %v, %v", dropped, kept, err)
	}
}
