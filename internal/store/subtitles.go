package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// SubtitleForcedReady reports whether migration 038 is in place: a subtitle's
// isforced. Without it no subtitle is kept as forced.
func (s *Store) SubtitleForcedReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_subtitleassets') AND attname = 'isforced' AND NOT attisdropped)`).Scan(&ok)
	return ok, err
}

// EnsureSubtitleForced applies db/migrations/038_subtitle_forced.sql when its
// column is missing. The check comes first for the reason EnsurePeople gives.
func (s *Store) EnsureSubtitleForced(ctx context.Context) error {
	ready, err := s.SubtitleForcedReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.SubtitleForced)
	return err
}

// SubtitlesByItem lists the item's subtitles, the default first, then by
// language; on a catalog without migration 038 none is forced.
func (s *Store) SubtitlesByItem(ctx context.Context, id string) ([]*model.SubtitleAsset, error) {
	out, err := s.subtitlesByItem(ctx, id, "isforced")
	if undefinedColumn(err) {
		out, err = s.subtitlesByItem(ctx, id, "false")
	}
	return out, err
}

// subtitlesByItem lists the item's subtitles, reading forced as it says.
func (s *Store) subtitlesByItem(ctx context.Context, id, forced string) ([]*model.SubtitleAsset, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, item_id, path, format, lang, label, isdefault, `+forced+`
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY isdefault DESC NULLS LAST, lang`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.SubtitleAsset
	for rows.Next() {
		var x model.SubtitleAsset
		if err := rows.Scan(&x.ID, &x.ItemID, &x.Path, &x.Format, &x.Lang, &x.Label, &x.IsDefault, &x.IsForced); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}
