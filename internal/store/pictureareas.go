package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// The table and the view db/migrations/054_picture_areas.sql creates: each
// title's picture area as the analyzer measured it, and those of what each
// title plays now.
const (
	PictureAreasTable       = "com_nalet_katalog_pictureareas"
	CurrentPictureAreasView = "com_nalet_katalog_currentpictureareas"
)

// pictureAreaRelations are the table, the view and the indexes 054 creates.
var pictureAreaRelations = []string{PictureAreasTable, CurrentPictureAreasView, "idx_pictureareas_item",
	"idx_pictureareas_claimed", "idx_pictureareas_lastclaimed"}

// PictureAreasReady reports whether migration 054 is in place.
func (s *Store) PictureAreasReady(ctx context.Context) (bool, error) {
	return pictureAreasReady(ctx, s.pool)
}

// pictureAreasReady is PictureAreasReady read by q: the pool, or the
// transaction that holds the migrations' lock.
func pictureAreasReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM unnest($1::text[]) AS r(name)
		WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])`, pictureAreaRelations).Scan(&ok)
	return ok, err
}

// EnsurePictureAreas applies db/migrations/054_picture_areas.sql when any of
// its objects is missing, under the migrations' lock (applyLocked). Without
// it no picture area is kept, and the players fit the frame.
func (s *Store) EnsurePictureAreas(ctx context.Context) error {
	return s.applyLocked(ctx, pictureAreasReady, migrations.PictureAreas)
}
