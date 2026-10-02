package store

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// Deletion says who removes items from the catalog, and why. Every delete
// records each item it removes in the deletion log
// (com_nalet_katalog_deleteditems) inside its own transaction: an item never
// leaves the catalog without a row there, and a delete that fails leaves none.
type Deletion struct {
	// By is the authenticated principal's subject, or the service that deletes,
	// e.g. "katalog-manager/scanner". Required.
	By string
	// Reason is optional free text for whoever reads the log.
	Reason string
}

// values returns what the log stores for d: who, and why (nil when no reason
// is given), each cut to its column so that a long text never fails a delete.
func (d Deletion) values() (by string, reason *string, err error) {
	by = strings.TrimSpace(d.By)
	if by == "" {
		return "", nil, errors.New("delete items: the deletion log must say who deletes")
	}
	by = clip(by, 255)
	if r := strings.TrimSpace(d.Reason); r != "" {
		r = clip(r, 500)
		reason = &r
	}
	return by, reason, nil
}

// clip cuts s to at most n characters, which is what VARCHAR(n) counts.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

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
