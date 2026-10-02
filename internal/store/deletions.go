package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
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

// ListDeletedItems reads the deletion log, newest first: the items deleted at
// or after since (every one when since is nil), at most limit of them (default
// 100, at most 500). An id that the catalog holds again was re-created after
// it was deleted; the item that exists wins.
func (s *Store) ListDeletedItems(ctx context.Context, since *time.Time, limit int32) ([]*model.DeletedItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var from any // NULL: no lower bound
	if since != nil {
		// deletedat holds UTC wall time, and pgx sends a timestamp parameter as
		// the wall time of the value's own zone: compare UTC with UTC.
		from = since.UTC()
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, type, title, deletedat, deletedby, reason
		FROM com_nalet_katalog_deleteditems
		WHERE $1::timestamp IS NULL OR deletedat >= $1::timestamp
		ORDER BY deletedat DESC, id
		LIMIT $2`, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DeletedItem
	for rows.Next() {
		var d model.DeletedItem
		if err := rows.Scan(&d.ID, &d.Type, &d.Title, &d.DeletedAt, &d.DeletedBy, &d.Reason); err != nil {
			return nil, err
		}
		out = append(out, &d)
	}
	return out, rows.Err()
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
