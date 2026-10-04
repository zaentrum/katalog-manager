package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 033 a step has these columns: the four it adds, failures counting
// from 0 for every step older than it.
const stepShape = `id character varying(36) NOT NULL
createdat timestamp without time zone NULL
createdby character varying(255) NULL
modifiedat timestamp without time zone NULL
modifiedby character varying(255) NULL
item_id character varying(36) NOT NULL
step character varying(20) NOT NULL
status character varying(20) NOT NULL
startedat timestamp without time zone NULL
finishedat timestamp without time zone NULL
attempts integer NULL
error character varying(500) NULL
details text NULL
failures integer NOT NULL
lasterror character varying(500) NULL
nextretryat timestamp with time zone NULL
dispatchedat timestamp with time zone NULL`

// 033 gives a step its failures in a row, last error, next retry and the time
// its trigger was sent again, none for a step older than it (no retry
// scheduled: what failed before is an admin's to retry); running it again
// changes nothing, and the startup check applies it where any of it is
// missing, and only then.
func TestStepRetriesMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, attempts, error)
		VALUES ('s1', 'm1', 'transcode', 'failed', 3, 'ffmpeg exited 1')`)
	if ready, err := st.StepRetriesReady(ctx); err != nil || ready {
		t.Fatalf("StepRetriesReady on a catalog without 033: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureStepRetries(ctx); err != nil {
			t.Fatalf("EnsureStepRetries, %d. time: %v", run, err)
		}
	}
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.StepRetries); err != nil {
			t.Fatalf("applying 033 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.StepRetriesReady(ctx); err != nil || !ready {
		t.Fatalf("StepRetriesReady after EnsureStepRetries: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_itemprocessingsteps"); got != stepShape {
		t.Errorf("steps after 033:\n%s\nwant:\n%s", got, stepShape)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE id = 's1'
		AND status = 'failed' AND attempts = 3 AND error = 'ffmpeg exited 1'
		AND failures = 0 AND lasterror IS NULL AND nextretryat IS NULL AND dispatchedat IS NULL`); n != 1 {
		t.Error("a step older than 033 must come through as it was, with nothing scheduled")
	}

	// An index missing brings the migration back, and the columns keep what
	// they hold.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET failures = 2, lasterror = 'x' WHERE id = 's1'`)
	storetest.Exec(t, st, `DROP INDEX idx_processingsteps_running`)
	if ready, err := st.StepRetriesReady(ctx); err != nil || ready {
		t.Fatalf("StepRetriesReady with an index of 033 missing: %v, %v", ready, err)
	}
	if err := st.EnsureStepRetries(ctx); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.StepRetriesReady(ctx); err != nil || !ready {
		t.Fatalf("StepRetriesReady after the startup check: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps
		WHERE id = 's1' AND failures = 2 AND lasterror = 'x'`); n != 1 {
		t.Error("applying 033 again changed what a step holds")
	}
}

// storetest.Open gives a test every migration the service applies at startup,
// 033 among them.
func TestTheTestSchemaHasTheStepRetries(t *testing.T) {
	if ready, err := storetest.Open(t).StepRetriesReady(context.Background()); err != nil || !ready {
		t.Fatalf("StepRetriesReady on the test schema: %v, %v", ready, err)
	}
}
