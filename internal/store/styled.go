package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// FontAssetsTable is where migration 053 keeps the fonts a title's package
// carries.
const FontAssetsTable = "com_nalet_katalog_fontassets"

// StyledSubtitlesReady reports whether migration 053 is in place: a
// subtitle's styled original (styledpath, styledformat) and the fonts table.
func (s *Store) StyledSubtitlesReady(ctx context.Context) (bool, error) {
	return styledSubtitlesReady(ctx, s.pool)
}

func styledSubtitlesReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass('`+FontAssetsTable+`') IS NOT NULL
		AND (SELECT count(*) = 2 FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_subtitleassets')
			AND attname IN ('styledpath', 'styledformat') AND NOT attisdropped)`).Scan(&ok)
	return ok, err
}

// EnsureStyledSubtitles applies db/migrations/053_styled_subtitles.sql when
// any of its objects is missing, under the migrations' lock (applyLocked).
// Without it a package's styled originals and fonts are kept by no row, and
// no client is offered them.
func (s *Store) EnsureStyledSubtitles(ctx context.Context) error {
	return s.applyLocked(ctx, styledSubtitlesReady, migrations.StyledSubtitles)
}

// deleteFontsOf removes, in tx, the fonts of the items' packages, on a
// catalog that has the table (migration 053).
func deleteFontsOf(ctx context.Context, tx pgx.Tx, ids []string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, FontAssetsTable).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM `+FontAssetsTable+` WHERE item_id = ANY($1)`, ids)
	return err
}
