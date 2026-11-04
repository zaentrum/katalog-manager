package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// supersededColumns are the columns db/migrations/056_superseded_sources.sql
// adds to the sources.
var supersededColumns = []string{"supersededby", "supersededat", "replacekey"}

// SupersededSourcesReady reports whether migration 056 is in place: a
// source's state may be superseded, and its columns and indexes are there.
func (s *Store) SupersededSourcesReady(ctx context.Context) (bool, error) {
	return supersededReady(ctx, s.pool)
}

// supersededReady is SupersededSourcesReady read by q: the pool, or the
// transaction that holds the migrations' lock.
func supersededReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass($1::text) AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%''superseded''%')
		AND to_regclass('idx_itemsources_replacekey') IS NOT NULL AND to_regclass('idx_itemsources_superseded') IS NOT NULL
		AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass($1::text) AND attname = ANY($2::text[])
			AND attnum > 0 AND NOT attisdropped) = cardinality($2::text[])`, SourcesTable, supersededColumns).Scan(&ok)
	return ok, err
}

// EnsureSupersededSources applies db/migrations/056_superseded_sources.sql
// when any of it is missing, on a catalog that keeps sources (040), under the
// migrations' lock (applyLocked). Without it a replace keeps no old original
// superseded: one that deletes the old file is refused, and the retire job
// retires no superseded original.
func (s *Store) EnsureSupersededSources(ctx context.Context) error {
	var sources bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, SourcesTable).Scan(&sources); err != nil ||
		!sources {
		return err
	}
	return s.applyLocked(ctx, supersededReady, migrations.SupersededSources)
}
