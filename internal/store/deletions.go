package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// Deletion says who removes items from the catalog, and why. Every delete
// records each item it removes in the deletion log
// (com_nalet_katalog_deleteditems) inside its own transaction: an item never
// leaves the catalog without a row there, and a delete that fails leaves none.
// The same goes for a person, whom the catalog deletes once no title credits
// them any more (type person).
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

// The people a delete of credits leaves credited by no title: deleted with
// their images, and recorded in the deletion log as type person under their
// name, in the statement that deletes them. A person deleted before (and
// re-created since) has their row replaced by this, their latest, deletion.
const uncreditedPeople = `
	gone AS (
		DELETE FROM com_nalet_katalog_people p
		WHERE p.id = ANY($1::text[])
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itempeople ip WHERE ip.person_id = p.id)
		RETURNING p.id, p.name
	),
	logged AS (
		INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedat, deletedby, reason)
		SELECT id, 'person', left(COALESCE(name, ''), 255), now() AT TIME ZONE 'utc', $2, $3
		FROM gone
		ON CONFLICT (id) DO UPDATE SET
			type = EXCLUDED.type, title = EXCLUDED.title, deletedat = EXCLUDED.deletedat,
			deletedby = EXCLUDED.deletedby, reason = EXCLUDED.reason
		RETURNING id
	)`

const (
	deleteUncreditedPeople          = `WITH` + uncreditedPeople + ` SELECT count(*)::int FROM logged`
	deleteUncreditedPeopleAndImages = `WITH` + uncreditedPeople + `,
	images AS (
		DELETE FROM com_nalet_katalog_personartwork a USING gone WHERE a.person_id = gone.id
	)
	SELECT count(*)::int FROM logged`
)

// DeleteUncreditedPeople deletes, in tx, each of the given people whom no
// title credits any more, with their images, and records each in the deletion
// log (type person) in the same transaction, attributed to d: a person never
// leaves the catalog without a row there, and if the log cannot be written,
// nothing is deleted. A person a title still credits stays. It returns how many
// it deleted.
//
// The people are locked first. A transaction that gives one of them a credit
// meanwhile either commits before, and they stay, or finds them gone after.
func DeleteUncreditedPeople(ctx context.Context, tx pgx.Tx, ids []string, d Deletion) (int, error) {
	ids = uniqueSorted(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	by, reason, err := d.values()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM com_nalet_katalog_people WHERE id = ANY($1::text[])
		ORDER BY id FOR UPDATE`, ids); err != nil {
		return 0, err
	}
	var withImages bool // a catalog older than 030 keeps no person images
	if err := tx.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_personartwork') IS NOT NULL`).
		Scan(&withImages); err != nil {
		return 0, err
	}
	sql := deleteUncreditedPeople
	if withImages {
		sql = deleteUncreditedPeopleAndImages
	}
	var n int
	if err := tx.QueryRow(ctx, sql, ids, by, reason).Scan(&n); err != nil {
		return 0, fmt.Errorf("delete the people no title credits and record them in the deletion log: %w", err)
	}
	return n, nil
}

func uniqueSorted(ids []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
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
