package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// itemRatingColumns are the columns db/migrations/036_item_ratings.sql adds to
// com_nalet_katalog_items.
var itemRatingColumns = []string{"certification", "certification_country", "min_age", "min_age_override",
	"certification_fetched_at"}

// ItemRatingsReady reports whether migration 036 is in place: every column it
// adds to the items, and its index. Without it no title is rated: enrichment
// keeps no certification, and a capped viewer is served nothing.
func (s *Store) ItemRatingsReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_items') AND attnum > 0 AND NOT attisdropped
		  AND attname = ANY($1::text[])) = cardinality($1::text[])
		AND to_regclass('idx_items_rated_age') IS NOT NULL`, itemRatingColumns).Scan(&ok)
	return ok, err
}

// EnsureItemRatings applies db/migrations/036_item_ratings.sql when any of its
// objects is missing. The check comes first for the reason EnsurePeople gives.
func (s *Store) EnsureItemRatings(ctx context.Context) error {
	ready, err := s.ItemRatingsReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.ItemRatings)
	return err
}
