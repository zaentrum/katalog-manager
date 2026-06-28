package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// SearchHit is a lightweight search result row.
type SearchHit struct {
	ID     string
	Type   string
	Title  string
	Year   *int32
	Rating *float64
	Score  *float64
}

// SearchFilter narrows a search.
type SearchFilter struct {
	Q      *string
	Type   *string
	Genre  *string
	Year   *int32
	Limit  int32
	Offset int32
}

// SearchItems runs a relevance-ranked title search with optional type/genre/year
// filters. Score is exact=1.0 / prefix=0.8 / contains=0.5 (0 when no query), which
// avoids depending on a tsvector/pg_trgm column not present in every deployment.
// Returns the page and its size (the CAP's `total` is the page size, not a full count).
func (s *Store) SearchItems(ctx context.Context, f SearchFilter) ([]model.Item, []*float64, error) {
	var where []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}

	scoreExpr := "0::float8"
	q := ""
	if f.Q != nil {
		q = strings.TrimSpace(*f.Q)
	}
	if q != "" {
		args = append(args, q)
		n := len(args)
		scoreExpr = fmt.Sprintf(`CASE
			WHEN lower(title) = lower($%d) THEN 1.0
			WHEN lower(title) LIKE lower($%d) || '%%' THEN 0.8
			ELSE 0.5 END`, n, n)
		where = append(where, fmt.Sprintf("title ILIKE '%%' || $%d || '%%'", n))
	}
	if f.Type != nil && *f.Type != "" {
		add("type = $%d", *f.Type)
	}
	if f.Year != nil {
		add("year = $%d", *f.Year)
	}
	if f.Genre != nil && *f.Genre != "" {
		add(`id IN (SELECT ig.item_id FROM com_nalet_katalog_itemgenres ig
			JOIN com_nalet_katalog_genres g ON g.id = ig.genre_id WHERE g.name = $%d)`, *f.Genre)
	}

	query := `SELECT id, type, title, year, rating, ` + scoreExpr + ` AS score
		FROM com_nalet_katalog_items`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY score DESC, title"

	limit := clampLimit(f.Limit)
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		args = append(args, offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []model.Item
	var scores []*float64
	for rows.Next() {
		var it model.Item
		var score *float64
		if err := rows.Scan(&it.ID, &it.Type, &it.Title, &it.Year, &it.Rating, &score); err != nil {
			return nil, nil, err
		}
		items = append(items, it)
		scores = append(scores, score)
	}
	return items, scores, rows.Err()
}
