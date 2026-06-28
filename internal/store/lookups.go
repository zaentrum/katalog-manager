package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/model"
)

func (s *Store) ListGenres(ctx context.Context) ([]*model.Genre, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM com_nalet_katalog_genres ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Genre
	for rows.Next() {
		var g model.Genre
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}

func (s *Store) ListPeople(ctx context.Context) ([]*model.Person, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name FROM com_nalet_katalog_people ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Person
	for rows.Next() {
		var p model.Person
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *Store) ListEnrichmentStatusCodes(ctx context.Context) ([]*model.EnrichmentStatusCode, error) {
	rows, err := s.pool.Query(ctx, `SELECT code, name FROM com_nalet_katalog_enrichmentstatuscodes ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.EnrichmentStatusCode
	for rows.Next() {
		var c model.EnrichmentStatusCode
		if err := rows.Scan(&c.Code, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

const settingCols = `id, createdat, modifiedat, key, valuetext, valuetype, description`

func scanSetting(row pgx.Row, x *model.Setting) error {
	return row.Scan(&x.ID, &x.CreatedAt, &x.ModifiedAt, &x.Key, &x.ValueText, &x.ValueType, &x.Description)
}

func (s *Store) ListSettings(ctx context.Context) ([]*model.Setting, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+settingCols+` FROM com_nalet_katalog_settings ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Setting
	for rows.Next() {
		var x model.Setting
		if err := scanSetting(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

func (s *Store) GetSetting(ctx context.Context, id string) (*model.Setting, error) {
	var x model.Setting
	err := scanSetting(s.pool.QueryRow(ctx, `SELECT `+settingCols+` FROM com_nalet_katalog_settings WHERE id = $1`, id), &x)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// GetSettingByKey returns the setting with the given key, or nil.
func (s *Store) GetSettingByKey(ctx context.Context, key string) (*model.Setting, error) {
	var x model.Setting
	err := scanSetting(s.pool.QueryRow(ctx, `SELECT `+settingCols+` FROM com_nalet_katalog_settings WHERE key = $1`, key), &x)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// CreateSetting inserts a new setting (id via gen_random_uuid()).
func (s *Store) CreateSetting(ctx context.Context, key, valueText, valueType string, description *string) (*model.Setting, error) {
	var x model.Setting
	err := scanSetting(s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_settings
		(id, createdat, modifiedat, key, valuetext, valuetype, description)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4)
		RETURNING `+settingCols, key, valueText, valueType, description), &x)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// UpdateSetting updates value/type/description (key is read-only after create).
func (s *Store) UpdateSetting(ctx context.Context, id string, valueText, valueType, description *string) (*model.Setting, error) {
	var x model.Setting
	err := scanSetting(s.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_settings SET
		valuetext = COALESCE($2, valuetext),
		valuetype = COALESCE($3, valuetype),
		description = COALESCE($4, description),
		modifiedat = now()
		WHERE id = $1 RETURNING `+settingCols, id, valueText, valueType, description), &x)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) DeleteSetting(ctx context.Context, id string) (bool, error) {
	ct, err := s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_settings WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}
