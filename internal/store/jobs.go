package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
)

const scanJobCols = `id, source, status, startedat, finishedat, errormessage, filesseen, itemsinserted, itemsupdated`

func scanScanJob(row pgx.Row, x *model.ScanJob) error {
	return row.Scan(&x.ID, &x.Source, &x.Status, &x.StartedAt, &x.FinishedAt, &x.ErrorMessage,
		&x.FilesSeen, &x.ItemsInserted, &x.ItemsUpdated)
}

// scanJobRunnerColumns are the columns db/migrations/034_scan_job_runner.sql
// adds to com_nalet_katalog_scanjobs.
var scanJobRunnerColumns = []string{"runner", "heartbeatat"}

// ScanJobRunnerReady reports whether migration 034 is in place: every column
// it adds to the scan jobs. Without it a scan job names no runner and keeps no
// last word, and one a restart cut short says running.
func (s *Store) ScanJobRunnerReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_scanjobs') AND attnum > 0 AND NOT attisdropped
		  AND attname = ANY($1::text[])) = cardinality($1::text[])`, scanJobRunnerColumns).Scan(&ok)
	return ok, err
}

// EnsureScanJobRunner applies db/migrations/034_scan_job_runner.sql when any
// column it adds is missing. The check comes first for the reason EnsurePeople
// gives.
func (s *Store) EnsureScanJobRunner(ctx context.Context) error {
	ready, err := s.ScanJobRunnerReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.ScanJobRunner)
	return err
}

// undefinedColumn reports whether err is Postgres saying a column does not
// exist (42703): here, the scan jobs lack migration 034.
func undefinedColumn(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42703"
}

// StartScanJob records a scan of source that runner, the process that runs it,
// starts now: running, its last word now. On a catalog without migration 034
// the job is recorded without either, as before.
func (s *Store) StartScanJob(ctx context.Context, source, runner string) (*model.ScanJob, error) {
	var x model.ScanJob
	err := scanScanJob(s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_scanjobs
		(id, source, status, startedat, filesseen, itemsinserted, itemsupdated, runner, heartbeatat)
		VALUES (gen_random_uuid()::varchar, $1, 'running', now(), 0, 0, 0, $2, now())
		RETURNING `+scanJobCols, source, runner), &x)
	if undefinedColumn(err) {
		err = scanScanJob(s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_scanjobs
			(id, source, status, startedat, filesseen, itemsinserted, itemsupdated)
			VALUES (gen_random_uuid()::varchar, $1, 'running', now(), 0, 0, 0)
			RETURNING `+scanJobCols, source), &x)
	}
	if err != nil {
		return nil, err
	}
	return &x, nil
}

// BeatScanJob is a word of the scan job id's scanner: it is alive, now. A job
// no longer running is left as it is (one the reaper failed stays failed until
// its scan ends), as is every job on a catalog without migration 034.
func (s *Store) BeatScanJob(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs SET heartbeatat = now()
		WHERE id = $1 AND status = 'running'`, id)
	if undefinedColumn(err) {
		return nil
	}
	return err
}

// FailInterruptedScanJobs fails, saying reason, the scan jobs a previous
// process left running, and returns how many. A scan runs in the process that
// started it, so one whose process is gone never ends. Such a job is one a
// process of runner's host ran with another tag (a process here before this
// one), or one that names no runner (started before migration 034). A job of
// another host is left alone: its process may be alive, and if it is not, the
// reaper fails the job once it has said nothing for longer than the scan's
// timeout (FailSilentScanJobs). Nothing is failed on a catalog without 034.
func (s *Store) FailInterruptedScanJobs(ctx context.Context, runner, reason string) (int, error) {
	host := runner
	if i := strings.LastIndexByte(runner, '/'); i >= 0 {
		host = runner[:i]
	}
	tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs
		SET status = 'failed', finishedat = now(), errormessage = $3
		WHERE status = 'running' AND (runner IS NULL OR (starts_with(runner, $2) AND runner <> $1))`,
		runner, host+"/", reason)
	if undefinedColumn(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// FailSilentScanJobs fails, saying reason, the scan jobs running without a
// word from their scanner for longer than timeout (since they started, for one
// that has said nothing), and returns how many. Nothing is failed on a catalog
// without migration 034: without a last word, a scan that walks a large library
// would be taken for one that is gone.
func (s *Store) FailSilentScanJobs(ctx context.Context, timeout time.Duration, reason string) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs
		SET status = 'failed', finishedat = now(), errormessage = $2
		WHERE status = 'running'
		  AND COALESCE(heartbeatat, startedat::timestamptz, '-infinity'::timestamptz) < now() - make_interval(secs => $1::float8)`,
		timeout.Seconds(), reason)
	if undefinedColumn(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ScanJobResult carries the worker's completion counters, and its report:
// what it passed over and left alone.
type ScanJobResult struct {
	Status        string
	ErrorMessage  *string
	FilesSeen     int32
	ItemsInserted int32
	ItemsUpdated  int32
	Report        []model.ScanNote
}

// FinishScanJob stamps a scan job with its final status + counters, and its
// report (null when it says nothing). It is the scan's own word on how it
// ended, so it stands over a failure the service wrote for a scan it took for
// lost. On a catalog without migration 045 the job keeps no report.
func (s *Store) FinishScanJob(ctx context.Context, id string, r ScanJobResult) error {
	var report *string
	if len(r.Report) > 0 {
		b, err := json.Marshal(r.Report)
		if err != nil {
			return err
		}
		s := string(b)
		report = &s
	}
	_, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs SET
		status = $2, finishedat = now(), errormessage = $3,
		filesseen = $4, itemsinserted = $5, itemsupdated = $6, report = $7::jsonb WHERE id = $1`,
		id, r.Status, r.ErrorMessage, r.FilesSeen, r.ItemsInserted, r.ItemsUpdated, report)
	if undefinedColumn(err) {
		_, err = s.pool.Exec(ctx, `UPDATE com_nalet_katalog_scanjobs SET
			status = $2, finishedat = now(), errormessage = $3,
			filesseen = $4, itemsinserted = $5, itemsupdated = $6 WHERE id = $1`,
			id, r.Status, r.ErrorMessage, r.FilesSeen, r.ItemsInserted, r.ItemsUpdated)
	}
	return err
}

// scanJobRead are a scan job's columns with its report, read by name from the
// row j, so that a catalog without migration 045 reads none.
const scanJobRead = scanJobCols + `, to_jsonb(j)->'report'`

// scanReportedJob reads a row of scanJobRead.
func scanReportedJob(row pgx.Row, x *model.ScanJob) error {
	var report []byte
	if err := row.Scan(&x.ID, &x.Source, &x.Status, &x.StartedAt, &x.FinishedAt, &x.ErrorMessage,
		&x.FilesSeen, &x.ItemsInserted, &x.ItemsUpdated, &report); err != nil {
		return err
	}
	if len(report) == 0 || string(report) == "null" {
		return nil
	}
	return json.Unmarshal(report, &x.Report)
}

func (s *Store) GetScanJob(ctx context.Context, id string) (*model.ScanJob, error) {
	var x model.ScanJob
	err := scanReportedJob(s.pool.QueryRow(ctx, `SELECT `+scanJobRead+` FROM com_nalet_katalog_scanjobs j WHERE id = $1`, id), &x)
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
	rows, err := s.pool.Query(ctx, `SELECT `+scanJobRead+` FROM com_nalet_katalog_scanjobs j
		ORDER BY startedat DESC NULLS LAST LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ScanJob
	for rows.Next() {
		var x model.ScanJob
		if err := scanReportedJob(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}
