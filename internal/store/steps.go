package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// stepRetryColumns are the columns db/migrations/033_step_retries.sql adds to
// com_nalet_katalog_itemprocessingsteps.
var stepRetryColumns = []string{"failures", "lasterror", "nextretryat", "dispatchedat"}

// StepRetriesReady reports whether migration 033 is in place: every column it
// adds to the step table, and its indexes. Without it a step keeps its status,
// error and details, and nothing retries it.
func (s *Store) StepRetriesReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_itemprocessingsteps') AND attnum > 0 AND NOT attisdropped
		  AND attname = ANY($1::text[])) = cardinality($1::text[])
		AND to_regclass('idx_processingsteps_retry') IS NOT NULL
		AND to_regclass('idx_processingsteps_failed') IS NOT NULL
		AND to_regclass('idx_processingsteps_running') IS NOT NULL
		AND to_regclass('idx_processingsteps_dispatched') IS NOT NULL`, stepRetryColumns).Scan(&ok)
	return ok, err
}

// EnsureStepRetries applies db/migrations/033_step_retries.sql when any of its
// objects is missing. Without it the pipeline runs as before: a step that
// fails stays failed and one whose worker died stays in progress, and the
// retries an admin asks for are refused. The check comes first for the reason
// EnsurePeople gives.
func (s *Store) EnsureStepRetries(ctx context.Context) error {
	ready, err := s.StepRetriesReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.StepRetries)
	return err
}
