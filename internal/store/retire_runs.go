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
// objects is missing. Without it no list of originals is retired.
func (s *Store) EnsureRetireRuns(ctx context.Context) error {
	ready, err := s.RetireRunsReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.RetireRuns)
	return err
}
