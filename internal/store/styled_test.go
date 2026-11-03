package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Migration 053 waits for the subtitles' table a short while at a time and
// gives way between: a read of the table that queues behind its ALTER while
// another session holds the table goes on before that session lets go; the
// migration is applied once it has, and gives up, its lock refused, when it
// never does within its attempts.
func TestTheStyledSubtitlesMigrationGivesWayToTheTablesReaders(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	// hold holds the table in a transaction of its own until what it answers
	// is called, or the test ends
	hold := func() func() {
		t.Helper()
		tx, err := st.Pool().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		release := sync.OnceFunc(func() { _ = tx.Commit(ctx) })
		t.Cleanup(release)
		if _, err := tx.Exec(ctx, `LOCK TABLE com_nalet_katalog_subtitleassets IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		return release
	}

	// held for every attempt: refused, and nothing applied
	restore := store.SetStyledLockWaits("100ms", 3, 50*time.Millisecond)
	release := hold()
	if err := st.EnsureStyledSubtitles(ctx); err == nil {
		t.Error("the migration applied while the table was held throughout")
	}
	release()
	restore()
	if ready, err := st.StyledSubtitlesReady(ctx); err != nil || ready {
		t.Fatalf("ready after a refused migration: %v %v", ready, err)
	}

	defer store.SetStyledLockWaits("200ms", 300, 100*time.Millisecond)()
	release = hold()
	applied := make(chan error, 1)
	go func() { applied <- st.EnsureStyledSubtitles(ctx) }()
	for deadline := time.Now().Add(time.Minute); storetest.Count(t, st, `SELECT count(*) FROM pg_locks
		WHERE NOT granted AND relation = to_regclass('com_nalet_katalog_subtitleassets')`) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the migration's ALTER never waited for the table")
		}
	}
	// a reader queued behind the waiting ALTER goes on once it gives way,
	// while the table is held still
	read, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	var n int
	if err := st.Pool().QueryRow(read, `SELECT count(*) FROM com_nalet_katalog_subtitleassets`).Scan(&n); err != nil {
		t.Fatalf("the read behind the migration: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the read waited %s behind the migration", took)
	}
	release()
	if err := <-applied; err != nil {
		t.Fatalf("the migration once the table is let go: %v", err)
	}
	if ready, err := st.StyledSubtitlesReady(ctx); err != nil || !ready {
		t.Errorf("ready: %v %v", ready, err)
	}
}
