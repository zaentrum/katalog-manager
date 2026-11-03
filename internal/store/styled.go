package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
			AND attname IN ('styledpath', 'styledformat') AND NOT attisdropped)
		AND EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_fontassets_item'
			AND conrelid = to_regclass('`+FontAssetsTable+`'))`).Scan(&ok)
	return ok, err
}

// How migration 053 waits for its locks — the migrations', and the
// subtitles' table's, which its ALTER takes: a few seconds at a time
// (styledLockTimeout), for every read of the table queues behind an ALTER
// that waits, then giving way to them for styledPause, styledAttempts times
// in all. Variables only so that a test can make them short.
var (
	styledLockTimeout = "3s"
	styledAttempts    = 6
	styledPause       = 2 * time.Second
)

// EnsureStyledSubtitles applies db/migrations/053_styled_subtitles.sql when
// any of its objects is missing, under the migrations' lock (applyLocked),
// waiting for each lock no longer than styledLockTimeout and trying again
// while one is not to be had, up to styledAttempts times. Without it a
// package's styled originals and fonts are kept by no row, and no client is
// offered them.
func (s *Store) EnsureStyledSubtitles(ctx context.Context) error {
	var err error
	for i := 0; i < styledAttempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(styledPause):
			}
		}
		err = s.applyLockedWithin(ctx, styledSubtitlesReady, migrations.StyledSubtitles, styledLockTimeout)
		if !lockNotAvailable(err) {
			return err
		}
	}
	return err
}

// lockNotAvailable reports whether err is PostgreSQL giving up on a lock it
// waited for longer than lock_timeout (55P03).
func lockNotAvailable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "55P03"
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
