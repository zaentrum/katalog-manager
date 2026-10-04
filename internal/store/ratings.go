package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
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

// RatedAgeSQL is, in SQL, the age a viewer must be to be served the item i,
// whose parent is joined as par (ParentJoinSQL): its own override, else its
// parent's override, else its parent's rating, else its own rating; NULL when
// nothing rates it. An episode is so rated as its series, as katalog-api
// rates it.
const (
	RatedAgeSQL   = `COALESCE(i.min_age_override, par.min_age_override, par.min_age, i.min_age)`
	ParentJoinSQL = `LEFT JOIN com_nalet_katalog_items par ON par.id = i.parent_id`
)

// ItemRating is the age rating of the item id, with the age a capped viewer
// is held to (an episode's from its series); nil when there is no such item.
// On a catalog without migration 036 every item is unrated: an empty rating.
func (s *Store) ItemRating(ctx context.Context, id string) (*model.ItemRating, error) {
	var r model.ItemRating
	err := s.pool.QueryRow(ctx, `SELECT i.certification, i.certification_country, i.min_age, i.min_age_override,
			`+RatedAgeSQL+`, i.certification_fetched_at
		FROM com_nalet_katalog_items i `+ParentJoinSQL+` WHERE i.id = $1`, id).
		Scan(&r.Certification, &r.Country, &r.MinAge, &r.MinAgeOverride, &r.Effective, &r.FetchedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case undefinedColumn(err):
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items WHERE id = $1)`, id).
			Scan(&exists); err != nil || !exists {
			return nil, err
		}
		return &model.ItemRating{}, nil
	case err != nil:
		return nil, err
	}
	r.FetchedAt = utc(r.FetchedAt)
	return &r, nil
}

// SetMinAgeOverride rates the item id by hand, as by says who: age (0 to 21)
// wins over the rating TMDB's certification gives it, and over its series'
// for an episode; nil clears it. It reports whether there is such an item. A
// catalog without migration 036 has nowhere to keep it: an error.
func (s *Store) SetMinAgeOverride(ctx context.Context, id string, age *int32, by string) (bool, error) {
	if age != nil && (*age < 0 || *age > 21) {
		return false, errors.New("an age rating is 0 to 21 years")
	}
	if len(by) > 255 {
		by = by[:255]
	}
	tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_items SET min_age_override = $2::smallint,
		modifiedat = now(), modifiedby = $3 WHERE id = $1`, id, age, by)
	if undefinedColumn(err) {
		return false, errors.New("the ratings migration (db/migrations/036_item_ratings.sql) is not applied")
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
