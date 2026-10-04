package store

import "context"

// CatalogCounts is how much the catalog holds, each counted in full.
type CatalogCounts struct {
	Movies, Series, Episodes int32
	// People are the people the catalog holds: everyone a title credits, as
	// a person no title credits any more is deleted.
	People int32
}

// CatalogCounts counts the catalog's movies, series and episodes, and its
// people, in one statement.
func (s *Store) CatalogCounts(ctx context.Context) (CatalogCounts, error) {
	var c CatalogCounts
	err := s.pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE type = 'movie')::int,
			count(*) FILTER (WHERE type = 'series')::int,
			count(*) FILTER (WHERE type = 'episode')::int,
			(SELECT count(*) FROM com_nalet_katalog_people)::int
		FROM com_nalet_katalog_items`).Scan(&c.Movies, &c.Series, &c.Episodes, &c.People)
	return c, err
}
