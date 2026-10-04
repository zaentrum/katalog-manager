package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// retiredJobTables are the tables db/migrations/035_retired_job_tables.sql
// drops while they are empty, in its order.
var retiredJobTables = []string{"com_nalet_katalog_trailerjobs", "com_nalet_katalog_downloadjobs"}

// RetiredTable is a table of an integration the core no longer carries that
// migration 035 kept because it holds rows: its name and how many.
type RetiredTable struct {
	Name string
	Rows int64
}

// DropRetiredJobTables applies db/migrations/035_retired_job_tables.sql when a
// table it drops is there. It returns the tables it dropped, each empty, and
// those it kept because they hold rows; neither when there is no such table.
func (s *Store) DropRetiredJobTables(ctx context.Context) (dropped []string, kept []RetiredTable, err error) {
	before, err := s.presentTables(ctx, retiredJobTables)
	if err != nil || len(before) == 0 {
		return nil, nil, err
	}
	// No arguments, so pgx sends the file over the simple protocol: its
	// statement runs in a transaction of its own.
	if _, err := s.pool.Exec(ctx, migrations.RetiredJobTables); err != nil {
		return nil, nil, err
	}
	after, err := s.presentTables(ctx, before)
	if err != nil {
		return nil, nil, err
	}
	left := map[string]bool{}
	for _, name := range after {
		left[name] = true
		var n int64
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+name).Scan(&n); err != nil {
			return nil, nil, err
		}
		// A table there again and empty is one a base schema created since,
		// not one kept for its rows.
		if n > 0 {
			kept = append(kept, RetiredTable{Name: name, Rows: n})
		}
	}
	for _, name := range before {
		if !left[name] {
			dropped = append(dropped, name)
		}
	}
	return dropped, kept, nil
}

// presentTables are the tables of names that the catalog holds, in the order
// of names.
func (s *Store) presentTables(ctx context.Context, names []string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM unnest($1::text[]) WITH ORDINALITY AS t(name, i)
		WHERE to_regclass(name) IS NOT NULL ORDER BY i`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
