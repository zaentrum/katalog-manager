package store

import (
	"context"
	"fmt"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// The tables db/migrations/052_retire_runs.sql creates: an admin's lists of
// titles whose originals are retired (retireOriginals), and how each title of
// a list fared. A run keeps its titles' rows when a title is deleted: they
// are the record of what was retired, on whose word.
const (
	RetireRunsTable     = "com_nalet_katalog_retireruns"
	RetireRunItemsTable = "com_nalet_katalog_retirerunitems"
)

// retireRunRelations are the tables and indexes 052 creates.
var retireRunRelations = []string{RetireRunsTable, RetireRunItemsTable, "idx_retireruns_running"}

// RetireRunsReady reports whether migration 052 is in place.
func (s *Store) RetireRunsReady(ctx context.Context) (bool, error) {
	return retireRunsReady(ctx, s.pool)
}

// retireRunsReady is RetireRunsReady read by q: the pool, or the transaction
// that holds the migrations' lock.
func retireRunsReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM unnest($1::text[]) AS r(name)
		WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])`, retireRunRelations).Scan(&ok)
	return ok, err
}

// EnsureRetireRuns applies db/migrations/052_retire_runs.sql when any of its
// objects is missing, under the migrations' lock (applyLocked). Without it no
// list of originals is retired.
func (s *Store) EnsureRetireRuns(ctx context.Context) error {
	return s.applyLocked(ctx, retireRunsReady, migrations.RetireRuns)
}

// migrationLock is the advisory lock a startup migration is applied under,
// the catalog's (its schema's): of instances that start at once, one applies
// it, and the others find it in place.
const migrationLock = `hashtext('katalog-migrations/' || current_schema())`

// migrationLockTimeout is how long a startup migration waits for a lock, the
// migrations' or one its statements take of a table another session reads:
// longer, and the migration fails (its feature waits for the next start)
// rather than holding the table's readers behind it.
const migrationLockTimeout = "1min"

// applyLocked applies the migration sql when ready says it is not in place,
// in one transaction under migrationLock, asking ready again by that
// transaction once it holds the lock: another instance may have applied it
// meanwhile. It holds one connection of the pool while it waits, never a
// second: of more callers than the pool has connections, the one that holds
// the lock goes on.
func (s *Store) applyLocked(ctx context.Context, ready func(context.Context, querier) (bool, error), sql string) error {
	if ok, err := ready(ctx, s.pool); err != nil || ok {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+migrationLockTimeout+`'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+migrationLock+`)`); err != nil {
		return fmt.Errorf("the migrations' lock: %w", err)
	}
	if ok, err := ready(ctx, tx); err != nil || ok {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
