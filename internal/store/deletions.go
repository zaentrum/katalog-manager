package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// EnsureDeletionLog applies db/migrations/029_deleted_items.sql when the
// deletion log is missing. Every item delete writes to that table inside its
// own transaction, so without it deletes fail rather than leave no trace.
//
// The check comes first so that a role allowed to write the table, but not to
// create tables in its schema, starts cleanly once the migration has been
// applied by hand: CREATE TABLE IF NOT EXISTS checks that privilege even when
// the table already exists.
func (s *Store) EnsureDeletionLog(ctx context.Context) error {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('com_nalet_katalog_deleteditems') IS NOT NULL`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	// No arguments, so pgx sends the file over the simple protocol: all of its
	// statements run, as one implicit transaction.
	_, err := s.pool.Exec(ctx, migrations.DeletedItems)
	return err
}
