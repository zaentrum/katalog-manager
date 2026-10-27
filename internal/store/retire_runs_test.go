package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// 052 keeps an admin's lists of titles whose originals are retired, and how
// each title fared: running it again changes nothing, the startup check adds
// it where any of it is missing, and one run runs at a time.
func TestRetireRunsMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if ready, err := st.RetireRunsReady(ctx); err != nil || ready {
		t.Fatalf("RetireRunsReady on a catalog without 052: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureRetireRuns(ctx); err != nil {
			t.Fatalf("EnsureRetireRuns, %d. time: %v", run, err)
		}
	}
	if _, err := st.Pool().Exec(ctx, migrations.RetireRuns); err != nil {
		t.Fatalf("applying 052 again: %v", err)
	}
	if ready, err := st.RetireRunsReady(ctx); err != nil || !ready {
		t.Fatalf("RetireRunsReady after EnsureRetireRuns: %v, %v", ready, err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_retireruns (id, dryrun, requestedby) VALUES ('r1', true, 'an-admin')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_retirerunitems (run_id, position, item_id, title)
		VALUES ('r1', 1, 'm1', 'A Film (1991)'), ('r1', 2, 'gone', NULL)`)
	if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_retireruns (id, dryrun, requestedby)
		VALUES ('r2', false, 'an-admin')`); err == nil {
		t.Error("a second run runs while the first does")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_retireruns SET state = 'done', finishedat = now() WHERE id = 'r1'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_retireruns (id, dryrun, requestedby) VALUES ('r2', false, 'an-admin')`)
	if _, err := st.Pool().Exec(ctx, `UPDATE com_nalet_katalog_retirerunitems SET result = 'gone' WHERE run_id = 'r1'`); err == nil {
		t.Error("a title of a run took a result no run gives")
	}

	// A table missing brings the migration back; the other keeps its rows.
	storetest.Exec(t, st, `DROP INDEX idx_retireruns_running`)
	if ready, err := st.RetireRunsReady(ctx); err != nil || ready {
		t.Fatalf("RetireRunsReady with its index missing: %v, %v", ready, err)
	}
	if err := st.EnsureRetireRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_retirerunitems WHERE run_id = 'r1'`); n != 2 {
		t.Errorf("the titles of the first run after the startup check: %d, want 2", n)
	}
}

// Instances that start at once on a catalog without 051 and 052 apply each
// once, under the migrations' lock: none fails, and both are in place.
func TestMigrationsAppliedByInstancesStartingAtOnce(t *testing.T) {
	for round := 0; round < 5; round++ {
		st := storetest.OpenBase(t)
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					errs[i] = st.EnsureRetireRuns(context.Background())
				} else {
					errs[i] = st.EnsureCompliance(context.Background())
				}
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d, instance %d: %v", round, i, err)
			}
		}
		for name, ready := range map[string]func(context.Context) (bool, error){"052": st.RetireRunsReady, "051": st.ComplianceReady} {
			if ok, err := ready(context.Background()); !ok || err != nil {
				t.Errorf("round %d: %s not in place: %v %v", round, name, ok, err)
			}
		}
	}
}
