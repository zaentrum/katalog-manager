package store

import (
	"context"
	"time"

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

// ---- DownloadJobs (read model) ----

const downloadJobViewCols = `id, adapter, clientjobid, title, wanteditemid, state, progresspct,
	downloadedbytes, sizebytes, speedbps, etasec, files, errormessage, startedat, completedat,
	lasteventat, statecriticality`

func scanDownloadJob(row pgx.Row, x *model.DownloadJob) error {
	return row.Scan(&x.ID, &x.Adapter, &x.ClientJobID, &x.Title, &x.WantedItemID, &x.State, &x.ProgressPct,
		&x.DownloadedBytes, &x.SizeBytes, &x.SpeedBps, &x.EtaSec, &x.Files, &x.ErrorMessage, &x.StartedAt,
		&x.CompletedAt, &x.LastEventAt, &x.StateCriticality)
}

func (s *Store) ListDownloadJobs(ctx context.Context, limit int32) ([]*model.DownloadJob, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+downloadJobViewCols+` FROM katalogservice_downloadjobs
		ORDER BY lasteventat DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.DownloadJob
	for rows.Next() {
		var x model.DownloadJob
		if err := scanDownloadJob(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}

// DownloadUpsert is the projection input from the Kafka consumer. Nil pointer
// fields are left unchanged on conflict. State precedence is enforced in SQL:
// a 'downloading' update never overwrites a terminal completed/failed row.
type DownloadUpsert struct {
	ID              string // deterministic (UUIDv3 of "adapter:clientjobid")
	Adapter         string
	ClientJobID     string
	Title           *string
	WantedItemID    *string
	State           string
	ProgressPct     *float64
	DownloadedBytes *int64
	SizeBytes       *int64
	SpeedBps        *int64
	EtaSec          *int32
	Files           *string
	ErrorMessage    *string
	StartedAt       *time.Time
	CompletedAt     *time.Time
	LastEventAt     *time.Time
}

// UpsertDownloadJob projects a download event into the read model. Upserts on
// the deterministic primary key id (= UUIDv3 of "adapter:clientjobid"), so it
// needs no secondary unique index. Nullable fields use COALESCE(new, existing);
// state is guarded so an in-flight 'downloading' event cannot regress a terminal row.
func (s *Store) UpsertDownloadJob(ctx context.Context, ev DownloadUpsert) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_downloadjobs
		(id, createdat, modifiedat, adapter, clientjobid, title, wanteditemid, state, progresspct,
		 downloadedbytes, sizebytes, speedbps, etasec, files, errormessage, startedat, completedat, lasteventat)
		VALUES ($1, now(), now(), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET
		  modifiedat      = now(),
		  title           = COALESCE(EXCLUDED.title, com_nalet_katalog_downloadjobs.title),
		  wanteditemid    = COALESCE(EXCLUDED.wanteditemid, com_nalet_katalog_downloadjobs.wanteditemid),
		  -- a terminal (completed/failed) row is never regressed by a later/redelivered
		  -- non-terminal event (e.g. an at-least-once 'started' or 'progress' replay).
		  state           = CASE
		      WHEN com_nalet_katalog_downloadjobs.state IN ('completed','failed') AND EXCLUDED.state NOT IN ('completed','failed')
		      THEN com_nalet_katalog_downloadjobs.state ELSE EXCLUDED.state END,
		  progresspct     = COALESCE(EXCLUDED.progresspct, com_nalet_katalog_downloadjobs.progresspct),
		  downloadedbytes = COALESCE(EXCLUDED.downloadedbytes, com_nalet_katalog_downloadjobs.downloadedbytes),
		  sizebytes       = COALESCE(EXCLUDED.sizebytes, com_nalet_katalog_downloadjobs.sizebytes),
		  speedbps        = COALESCE(EXCLUDED.speedbps, com_nalet_katalog_downloadjobs.speedbps),
		  etasec          = COALESCE(EXCLUDED.etasec, com_nalet_katalog_downloadjobs.etasec),
		  files           = COALESCE(EXCLUDED.files, com_nalet_katalog_downloadjobs.files),
		  errormessage    = COALESCE(EXCLUDED.errormessage, com_nalet_katalog_downloadjobs.errormessage),
		  startedat       = COALESCE(com_nalet_katalog_downloadjobs.startedat, EXCLUDED.startedat),
		  completedat     = COALESCE(EXCLUDED.completedat, com_nalet_katalog_downloadjobs.completedat),
		  lasteventat     = EXCLUDED.lasteventat`,
		ev.ID, ev.Adapter, ev.ClientJobID, ev.Title, ev.WantedItemID, ev.State, ev.ProgressPct,
		ev.DownloadedBytes, ev.SizeBytes, ev.SpeedBps, ev.EtaSec, ev.Files, ev.ErrorMessage,
		ev.StartedAt, ev.CompletedAt, ev.LastEventAt)
	return err
}
