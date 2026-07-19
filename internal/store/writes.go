package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// ItemWrite carries item fields for create/update. Nil = unset (left unchanged
// on update; defaulted on create).
type ItemWrite struct {
	Type           *string
	Title          *string
	SortTitle      *string
	Year           *int32
	Description    *string
	Rating         *float64
	DurationMs     *int64
	ParentID       *string
	SeasonNumber   *int32
	EpisodeNumber  *int32
	Tagline        *string
	MetadataLocked *bool // true = manual edit; the enricher leaves metadata alone
}

// CreateItem inserts a new item. type and title are required.
func (s *Store) CreateItem(ctx context.Context, w ItemWrite) (*model.Item, error) {
	if w.Type == nil || *w.Type == "" || w.Title == nil || *w.Title == "" {
		return nil, errors.New("type and title are required")
	}
	var i model.Item
	err := scanItemBase(s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_items
		(id, createdat, modifiedat, type, title, sorttitle, year, description, rating, durationms,
		 parent_id, seasonnumber, episodenumber, tagline)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+itemBaseCols,
		*w.Type, *w.Title, w.SortTitle, w.Year, w.Description, w.Rating, w.DurationMs,
		w.ParentID, w.SeasonNumber, w.EpisodeNumber, w.Tagline), &i)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// UpdateItem updates the provided (non-nil) fields and bumps modifiedat.
func (s *Store) UpdateItem(ctx context.Context, id string, w ItemWrite) (*model.Item, error) {
	var i model.Item
	err := scanItemBase(s.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_items SET
		type          = COALESCE($2, type),
		title         = COALESCE($3, title),
		sorttitle     = COALESCE($4, sorttitle),
		year          = COALESCE($5, year),
		description   = COALESCE($6, description),
		rating        = COALESCE($7, rating),
		durationms    = COALESCE($8, durationms),
		parent_id     = COALESCE($9, parent_id),
		seasonnumber  = COALESCE($10, seasonnumber),
		episodenumber = COALESCE($11, episodenumber),
		tagline       = COALESCE($12, tagline),
		metadatalocked = COALESCE($13, metadatalocked),
		modifiedat    = now()
		WHERE id = $1 RETURNING `+itemBaseCols,
		id, w.Type, w.Title, w.SortTitle, w.Year, w.Description, w.Rating, w.DurationMs,
		w.ParentID, w.SeasonNumber, w.EpisodeNumber, w.Tagline, w.MetadataLocked), &i)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// itemChildTables hold rows keyed by item_id that must be removed with the item
// (the CAP compositions). Ordered children-first.
var itemChildTables = []string{
	"com_nalet_katalog_itemgenres",
	"com_nalet_katalog_itempeople",
	"com_nalet_katalog_itemtags",
	"com_nalet_katalog_itemexternalids",
	"com_nalet_katalog_itemartwork",
	"com_nalet_katalog_itemartworkdata",
	"com_nalet_katalog_playbackassets",
	"com_nalet_katalog_subtitleassets",
	"com_nalet_katalog_mediasegments",
	"com_nalet_katalog_itemchapters",
	"com_nalet_katalog_itemtrailerlinks",
	"com_nalet_katalog_itemdiagnostics",
	"com_nalet_katalog_itemprocessingsteps",
}

// DeleteItem removes an item and all its composition rows in one transaction.
func (s *Store) DeleteItem(ctx context.Context, id string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	for _, t := range itemChildTables {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE item_id = $1`, id); err != nil {
			return false, err
		}
	}
	ct, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_items WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// SetItemGenres replaces the item's genres, find-or-creating each by name.
func (s *Store) SetItemGenres(ctx context.Context, id string, names []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemgenres WHERE item_id = $1`, id); err != nil {
		return err
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		var genreID string
		err := tx.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_genres WHERE name = $1`, name).Scan(&genreID)
		if err == pgx.ErrNoRows {
			if err := tx.QueryRow(ctx, `INSERT INTO com_nalet_katalog_genres (id, name)
				VALUES (gen_random_uuid()::varchar, $1) RETURNING id`, name).Scan(&genreID); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id)
			VALUES (gen_random_uuid()::varchar, $1, $2)`, id, genreID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// SetItemTags replaces the item's tags.
func (s *Store) SetItemTags(ctx context.Context, id string, tags []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemtags WHERE item_id = $1`, id); err != nil {
		return err
	}
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemtags (id, item_id, tag)
			VALUES (gen_random_uuid()::varchar, $1, $2)`, id, tag); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
