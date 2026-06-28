package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// itemViewCols is the column projection of katalogservice_items (the 16 base
// columns + 4 computed). Typed views (movies/series/episodes) append their own
// boolean columns and are read by the dedicated methods.
const itemViewCols = `id, createdat, createdby, modifiedat, modifiedby, type, title, sorttitle,
	year, description, rating, durationms, parent_id, seasonnumber, episodenumber, tagline,
	posterurl, backdropurl, runtimemin, yeartext`

// scanItemRow scans the 20 itemViewCols columns into an ItemRow.
func scanItemRow(row pgx.Row, r *model.ItemRow) error {
	return row.Scan(
		&r.ID, &r.CreatedAt, &r.CreatedBy, &r.ModifiedAt, &r.ModifiedBy, &r.Type, &r.Title, &r.SortTitle,
		&r.Year, &r.Description, &r.Rating, &r.DurationMs, &r.ParentID, &r.SeasonNumber, &r.EpisodeNumber, &r.Tagline,
		&r.PosterURL, &r.BackdropURL, &r.RuntimeMin, &r.YearText,
	)
}

// ItemFilter narrows an item listing (UI filter bar / GraphQL args).
type ItemFilter struct {
	Type     *string
	Genre    *string // genre name; resolved via itemgenres -> genres
	Year     *int32
	Search   *string // matches title (ILIKE)
	ParentID *string // direct parent (season for an episode)
	SeriesID *string // episodes anywhere under a series (parent or grandparent)
	Limit    int32
	Offset   int32
}

func clampLimit(n int32) int32 {
	if n <= 0 || n > 200 {
		return 50
	}
	return n
}

// GetItem reads one item (any type) from the katalogservice_items view.
func (s *Store) GetItem(ctx context.Context, id string) (*model.ItemRow, error) {
	var r model.ItemRow
	err := scanItemRow(s.pool.QueryRow(ctx, `SELECT `+itemViewCols+` FROM katalogservice_items WHERE id = $1`, id), &r)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListItems lists items from katalogservice_items with optional filters,
// ordered by createdat DESC (the CAP default sort).
func (s *Store) ListItems(ctx context.Context, f ItemFilter) ([]*model.ItemRow, error) {
	var where []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Type != nil {
		add("type = $%d", *f.Type)
	}
	if f.Year != nil {
		add("year = $%d", *f.Year)
	}
	if f.Search != nil && *f.Search != "" {
		add("title ILIKE '%%' || $%d || '%%'", *f.Search)
	}
	if f.Genre != nil && *f.Genre != "" {
		add(`id IN (SELECT ig.item_id FROM com_nalet_katalog_itemgenres ig
			JOIN com_nalet_katalog_genres g ON g.id = ig.genre_id WHERE g.name = $%d)`, *f.Genre)
	}
	if f.ParentID != nil && *f.ParentID != "" {
		add("parent_id = $%d", *f.ParentID)
	}
	if f.SeriesID != nil && *f.SeriesID != "" {
		args = append(args, *f.SeriesID)
		n := len(args)
		where = append(where, fmt.Sprintf(`(parent_id = $%d OR parent_id IN
			(SELECT id FROM com_nalet_katalog_items WHERE parent_id = $%d))`, n, n))
	}

	q := `SELECT ` + itemViewCols + ` FROM katalogservice_items`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY createdat DESC"
	limit := clampLimit(f.Limit)
	args = append(args, limit)
	q += fmt.Sprintf(" LIMIT $%d", len(args))
	if f.Offset > 0 {
		args = append(args, f.Offset)
		q += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ItemRow
	for rows.Next() {
		var r model.ItemRow
		if err := scanItemRow(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetItemBase reads one item from the base table (no computed columns) — used
// by mutations that need the raw row.
func (s *Store) GetItemBase(ctx context.Context, id string) (*model.Item, error) {
	var i model.Item
	err := s.pool.QueryRow(ctx, `SELECT id, createdat, createdby, modifiedat, modifiedby, type, title,
		sorttitle, year, description, rating, durationms, parent_id, seasonnumber, episodenumber, tagline
		FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(
		&i.ID, &i.CreatedAt, &i.CreatedBy, &i.ModifiedAt, &i.ModifiedBy, &i.Type, &i.Title,
		&i.SortTitle, &i.Year, &i.Description, &i.Rating, &i.DurationMs, &i.ParentID, &i.SeasonNumber, &i.EpisodeNumber, &i.Tagline)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}
