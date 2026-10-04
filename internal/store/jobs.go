package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/zaentrum/katalog-manager/internal/model"
)

const scanJobCols = `id, source, status, startedat, finishedat, errormessage, filesseen, itemsinserted, itemsupdated`

func scanScanJob(row pgx.Row, x *model.ScanJob) error {
	return row.Scan(&x.ID, &x.Source, &x.Status, &x.StartedAt, &x.FinishedAt, &x.ErrorMessage,
		&x.FilesSeen, &x.ItemsInserted, &x.ItemsUpdated)
}

// InsertScanJob creates a scan job (triggerScan sets status 'running').
func (s *Store) InsertScanJob(ctx context.Context, source, status string) (*model.ScanJob, error) {
	var x model.ScanJob
	err := scanScanJob(s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_scanjobs
		(id, source, status, startedat, filesseen, itemsinserted, itemsupdated)
		VALUES (gen_random_uuid()::varchar, $1, $2, now(), 0, 0, 0)
		RETURNING `+scanJobCols, source, status), &x)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// ScanJobResult carries the worker's completion counters.
type ScanJobResult struct {
	Status        string
	ErrorMessage  *string
	FilesSeen     int32
	ItemsInserted int32
	ItemsUpdated  int32
}

// FinishScanJob stamps a scan job with its final status + counters.
func (s *Store) FinishScanJob(ctx context.Context, id string, r ScanJobResult) error {
	_, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs SET
		status = $2, finishedat = now(), errormessage = $3,
		filesseen = $4, itemsinserted = $5, itemsupdated = $6 WHERE id = $1`,
		id, r.Status, r.ErrorMessage, r.FilesSeen, r.ItemsInserted, r.ItemsUpdated)
	return err
}

func (s *Store) GetScanJob(ctx context.Context, id string) (*model.ScanJob, error) {
	var x model.ScanJob
	err := scanScanJob(s.pool.QueryRow(ctx, `SELECT `+scanJobCols+` FROM com_nalet_katalog_scanjobs WHERE id = $1`, id), &x)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) ListScanJobs(ctx context.Context, limit int32) ([]*model.ScanJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+scanJobCols+` FROM com_nalet_katalog_scanjobs
		ORDER BY startedat DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ScanJob
	for rows.Next() {
		var x model.ScanJob
		if err := scanScanJob(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}
