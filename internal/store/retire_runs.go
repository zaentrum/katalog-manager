package store

import (
	"context"

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
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM unnest($1::text[]) AS r(name)
		WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])`, retireRunRelations).Scan(&ok)
	return ok, err
}

// EnsureRetireRuns applies db/migrations/052_retire_runs.sql when any of its
// objects is missing, under the migrations' lock (applyLocked). Without it no
// list of originals is retired.
func (s *Store) EnsureRetireRuns(ctx context.Context) error {
	return s.applyLocked(ctx, s.RetireRunsReady, migrations.RetireRuns)
}

// migrationLock is the advisory lock a startup migration is applied under,
// the catalog's (its schema's): of instances that start at once, one applies
// it, and the others find it in place.
const migrationLock = `hashtext('katalog-migrations/' || current_schema())`

// applyLocked applies the migration sql when ready says it is not in place,
// in one transaction under migrationLock, asking ready again once it holds
// the lock: another instance may have applied it meanwhile.
func (s *Store) applyLocked(ctx context.Context, ready func(context.Context) (bool, error), sql string) error {
	if ok, err := ready(ctx); err != nil || ok {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+migrationLock+`)`); err != nil {
		return err
	}
	if ok, err := ready(ctx); err != nil || ok {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
