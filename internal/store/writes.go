package store

import (
	"context"
	"errors"
	"fmt"

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

// IngestExternalFile registers a file that appeared in the library at absPath
// as a new item with a primary playback asset — the same shape the scanner
// creates (com_nalet_katalog_items + a primary playbackasset), so the file
// flows the normal enrich→analyze→transcode→package pipeline. Idempotent on the
// asset path: if the path already maps to an item, that item id is returned and
// created=false (a re-ingest is a no-op). The caller seeds the scan step +
// emits discovered exactly as the scanner does on a fresh insert.
//
// Neutral: this is "register an external file into the catalog", identical to
// what the scanner does for the media root — it knows nothing of where the file
// came from.
func (s *Store) IngestExternalFile(ctx context.Context, w ItemWrite, absPath string, sizeBytes int64) (itemID string, created bool, err error) {
	if w.Type == nil || *w.Type == "" || w.Title == nil || *w.Title == "" {
		return "", false, errors.New("type and title are required")
	}
	if absPath == "" {
		return "", false, errors.New("path is required")
	}
	// Already ingested at this path? Return the existing item (idempotent).
	var existing *string
	if err := s.pool.QueryRow(ctx,
		`SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`, absPath).
		Scan(&existing); err != nil && err != pgx.ErrNoRows {
		return "", false, err
	}
	if existing != nil {
		return *existing, false, nil
	}

	sort := ""
	if w.SortTitle != nil {
		sort = *w.SortTitle
	} else {
		sort = *w.Title
	}
	// metadatalocked is NOT NULL — default a nil pointer to false (unlocked: let
	// the enricher fill artwork/metadata from TMDB).
	locked := false
	if w.MetadataLocked != nil {
		locked = *w.MetadataLocked
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_items
		(id, createdat, modifiedat, type, title, sorttitle, year, description, rating, durationms,
		 parent_id, seasonnumber, episodenumber, tagline, metadatalocked)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id`,
		*w.Type, *w.Title, sort, w.Year, w.Description, w.Rating, w.DurationMs,
		w.ParentID, w.SeasonNumber, w.EpisodeNumber, w.Tagline, locked).Scan(&itemID)
	if err != nil {
		return "", false, err
	}
	if _, err = s.pool.Exec(ctx,
		`INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary)
		 VALUES (gen_random_uuid()::varchar, $1, $2, $3, true)`,
		itemID, absPath, sizeBytes); err != nil {
		return "", false, err
	}
	return itemID, true, nil
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
// (the CAP compositions). Ordered children-first. The tables of a migration
// that may not be applied go with them where they exist (trackTables).
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

// deleteAndRecordItems removes items and writes one deletion-log row for each
// item it removed — one statement, so the log holds exactly the rows that went.
// now() is the transaction's start: everything one delete removes shares one
// deletedat. An id deleted before (and re-created since) has its row replaced
// by this, its latest, deletion.
const deleteAndRecordItems = `
	WITH gone AS (
		DELETE FROM com_nalet_katalog_items WHERE id = ANY($1)
		RETURNING id, type, title
	)
	INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedat, deletedby, reason)
	SELECT id, left(COALESCE(type, ''), 20), left(COALESCE(title, ''), 255),
	       now() AT TIME ZONE 'utc', $2, $3
	FROM gone
	ON CONFLICT (id) DO UPDATE SET
		type = EXCLUDED.type, title = EXCLUDED.title, deletedat = EXCLUDED.deletedat,
		deletedby = EXCLUDED.deletedby, reason = EXCLUDED.reason`

// DeleteItems removes the given items and all their facet rows in ONE
// transaction (the remover takes a series and its episodes together), and in
// that same transaction records every item it removes in the deletion log,
// attributed to d. A person the items credited whom no other title credits
// goes with them, recorded in the log too (DeleteUncreditedPeople). If the log
// cannot be written, nothing is deleted. Returns the number of items removed;
// an id that does not exist is skipped and leaves no row.
func (s *Store) DeleteItems(ctx context.Context, ids []string, d Deletion) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	by, reason, err := d.values()
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	credited, err := personIDs(ctx, tx, `SELECT DISTINCT person_id FROM com_nalet_katalog_itempeople
		WHERE item_id = ANY($1)`, ids)
	if err != nil {
		return 0, err
	}
	for _, t := range itemChildTables {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE item_id = ANY($1)`, ids); err != nil {
			return 0, err
		}
	}
	if err := deleteTracksOf(ctx, tx, ids); err != nil {
		return 0, err
	}
	ct, err := tx.Exec(ctx, deleteAndRecordItems, ids, by, reason)
	if err != nil {
		return 0, fmt.Errorf("delete items and record them in the deletion log: %w", err)
	}
	if _, err := DeleteUncreditedPeople(ctx, tx, credited, Deletion{By: by,
		Reason: "no title credits them any more: the title that did was deleted"}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// personIDs runs a query that selects person ids.
func personIDs(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteItem removes one item and its facet rows, and records it in the
// deletion log, all in one transaction (see DeleteItems). It reports whether
// the item existed.
func (s *Store) DeleteItem(ctx context.Context, id string, d Deletion) (bool, error) {
	n, err := s.DeleteItems(ctx, []string{id}, d)
	return n > 0, err
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
