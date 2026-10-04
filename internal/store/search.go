package store

import (
	"context"
	"fmt"
	"strings"
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
//
// It returns the page asked for (limit, default 50 and at most 200, from offset)
// and how many items match in all, counted with the same filters. A page that
// holds fewer items than the limit ends the matches, so it tells the total
// itself; any other page has it counted.
func (s *Store) SearchItems(ctx context.Context, f SearchFilter) ([]SearchHit, int32, error) {
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
	from := ` FROM com_nalet_katalog_items`
	if len(where) > 0 {
		from += " WHERE " + strings.Join(where, " AND ")
	}
	filters := len(args)

	query := `SELECT id, type, title, year, rating, ` + scoreExpr + ` AS score` + from + ` ORDER BY score DESC, title`
	limit := clampLimit(f.Limit)
	args = append(args, limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	offset := max(f.Offset, 0)
	if offset > 0 {
		args = append(args, offset)
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ID, &h.Type, &h.Title, &h.Year, &h.Rating, &h.Score); err != nil {
			return nil, 0, err
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if n := int32(len(hits)); n > 0 && n < limit || n == 0 && offset == 0 {
		return hits, offset + n, nil
	}
	var total int32
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::int`+from, args[:filters]...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return hits, total, nil
}
