// Package processing ports the CAP ProcessingStepService — the upsert/reset/
// promote logic over com_nalet_katalog_itemprocessingsteps (SPEC §3) — and
// keeps what the service needs to retry a step: its failures in a row, its
// last error and when it is retried next (migration 033).
package processing

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Step status + name vocab.
const (
	StatusPending       = "pending"
	StatusInProgress    = "in_progress"
	StatusDone          = "done"
	StatusFailed        = "failed"
	StatusSkipped       = "skipped"
	StatusNotApplicable = "not_applicable"
)

// StepOrder is every step, in the order the pipeline runs them.
var StepOrder = []string{"scan", "tmdb", "tidb", "chapter", "chromaprint", "blackframe", "silence", "subtitle", "transcode", "package"}

var validSteps = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range StepOrder {
		m[s] = true
	}
	return m
}()

var validStatuses = map[string]bool{
	StatusPending: true, StatusInProgress: true, StatusDone: true,
	StatusFailed: true, StatusSkipped: true, StatusNotApplicable: true,
}

// ErrBadStep / ErrBadStatus map to HTTP 400 in the REST layer.
var (
	ErrBadStep   = errors.New("unknown step")
	ErrBadStatus = errors.New("unknown status")
)

// Steps owns the processing-step audit table.
type Steps struct {
	pool   *pgxpool.Pool
	policy Policy
	// legacy is set once the table turned out to lack migration 033's
	// columns: then a step's status, error and details are written as
	// before the migration, and nothing of its retries is kept.
	legacy *atomic.Bool
}

// New is the step table's writer, retrying by DefaultPolicy.
func New(pool *pgxpool.Pool) *Steps {
	return &Steps{pool: pool, policy: DefaultPolicy(), legacy: &atomic.Bool{}}
}

// WithPolicy is s retrying by p.
func (s *Steps) WithPolicy(p Policy) *Steps {
	c := *s
	c.policy = p
	return &c
}

// Policy is how s retries a step.
func (s *Steps) Policy() Policy { return s.policy }

func ValidStep(s string) bool   { return validSteps[s] }
func ValidStatus(s string) bool { return validStatuses[s] }

// Upsert performs INSERT ... ON CONFLICT (item_id, step):
//   - insert: startedat=now when status=in_progress; finishedat=now when
//     terminal (done|failed|skipped).
//   - conflict: status overwritten; startedat sticky (set to now only on the
//     first transition into in_progress while null); finishedat set on a
//     terminal status (NOT not_applicable).
//
// attempts counts the step's runs, not the reports of them: a run starts when
// the step turns in_progress from any other status (+1), and a worker's first
// report of a step is its first run (1 on insert), unless the step is only
// enqueued (pending: 0, nothing has run). A worker saying in_progress again
// (the analyzer's heartbeat), a run's end, a failure reported again and a
// promotion to pending count nothing.
//
// The error is kept as CleanError makes it: credentials redacted, at most 500
// characters. With migration 033 it also keeps the step's retries:
//   - a step that turns failed counts a failure (failures+1; 1 on insert),
//     keeps the error as its last error, and is scheduled for a retry by
//     itself (nextretryat) when the policy retries that many failures in a
//     row; a failure reported again (failed while failed) counts nothing and
//     keeps its schedule, and its last error unless it gives a new one;
//   - done, skipped and not_applicable end the failures in a row (0); no
//     status but failed has a retry scheduled; skipped is terminal and never
//     retried;
//   - any report of a worker answers a trigger the service sent again
//     (dispatchedat is cleared).
func (s *Steps) Upsert(ctx context.Context, itemID, step, status string, errMsg, details *string) error {
	if !validSteps[step] {
		return ErrBadStep
	}
	if !validStatuses[status] {
		return ErrBadStatus
	}
	em := cleanError(errMsg)
	if !s.legacy.Load() {
		err := s.exec(ctx, upsertSQL, itemID, step, status, em, details, s.policy.delays())
		if !missingRetryColumns(err) {
			return err
		}
		s.legacy.Store(true)
	}
	return s.exec(ctx, legacyUpsertSQL, itemID, step, status, em, details)
}

func (s *Steps) exec(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("processing step upsert affected 0 rows")
	}
	return nil
}

// missingRetryColumns reports whether err is Postgres saying a column does
// not exist (42703): the table lacks migration 033.
func missingRetryColumns(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "42703"
}

const tbl = "com_nalet_katalog_itemprocessingsteps"

// $3 (status) is cast to text at every use. Under pgx's extended protocol
// Postgres deduces one type for a parameter at parse time; using bare $3 both
// as the varchar `status` value AND inside `= 'in_progress'` / `IN (...)`
// comparisons yields "inconsistent types deduced for parameter $3" (42P08).
// psql never hit this (simple protocol inlines the literal). The explicit
// ::text casts pin $3 to a single type. See analyzer step-upsert regression.
const legacyUpsertSQL = `INSERT INTO ` + tbl + `
		(id, createdat, modifiedat, item_id, step, status, startedat, finishedat, attempts, error, details)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3::text,
			CASE WHEN $3::text = 'in_progress' THEN now() ELSE NULL END,
			CASE WHEN $3::text IN ('done','failed','skipped') THEN now() ELSE NULL END,
			` + insertAttempts + `, $4, $5)
		ON CONFLICT (item_id, step) DO UPDATE SET
			modifiedat = now(),
			status     = EXCLUDED.status,
			attempts   = ` + runAttempts + `,
			startedat  = CASE WHEN EXCLUDED.status = 'in_progress' AND ` + tbl + `.startedat IS NULL
			                  THEN now() ELSE ` + tbl + `.startedat END,
			finishedat = CASE WHEN EXCLUDED.status IN ('done','failed','skipped') THEN now()
			                  ELSE ` + tbl + `.finishedat END,
			error      = EXCLUDED.error,
			details    = EXCLUDED.details`

// insertAttempts is a step's attempts when a report inserts it: its worker's
// first report is its first run, a step enqueued (pending) has run none.
const insertAttempts = `CASE WHEN $3::text = 'pending' THEN 0 ELSE 1 END`

// runAttempts is a step's attempts after a report: one more when the step
// starts a run (it turns in_progress from any other status), as they were for
// any other report. A row without a count (NULL) counts from 0.
const runAttempts = `CASE WHEN EXCLUDED.status = 'in_progress' AND ` + tbl + `.status <> 'in_progress'
				THEN COALESCE(` + tbl + `.attempts, 0) + 1 ELSE ` + tbl + `.attempts END`

// upsertSQL is legacyUpsertSQL keeping the step's retries; $4 (the error) is
// cast to text at both its uses for the reason $3 is. $6 are the
// seconds to wait after a step's 1st, 2nd, ... failure in a row
// (Policy.delays): a failure beyond them schedules no retry, as the array's
// element is NULL. Every SET expression reads the row as it was.
const upsertSQL = `INSERT INTO ` + tbl + `
		(id, createdat, modifiedat, item_id, step, status, startedat, finishedat, attempts, error, details,
		 failures, lasterror, nextretryat, dispatchedat)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3::text,
			CASE WHEN $3::text = 'in_progress' THEN now() ELSE NULL END,
			CASE WHEN $3::text IN ('done','failed','skipped') THEN now() ELSE NULL END,
			` + insertAttempts + `, $4::text, $5,
			CASE WHEN $3::text = 'failed' THEN 1 ELSE 0 END,
			CASE WHEN $3::text = 'failed' THEN $4::text ELSE NULL END,
			CASE WHEN $3::text = 'failed' THEN now() + make_interval(secs => ($6::float8[])[1]) END,
			NULL)
		ON CONFLICT (item_id, step) DO UPDATE SET
			modifiedat = now(),
			status     = EXCLUDED.status,
			attempts   = ` + runAttempts + `,
			startedat  = CASE WHEN EXCLUDED.status = 'in_progress' AND ` + tbl + `.startedat IS NULL
			                  THEN now() ELSE ` + tbl + `.startedat END,
			finishedat = CASE WHEN EXCLUDED.status IN ('done','failed','skipped') THEN now()
			                  ELSE ` + tbl + `.finishedat END,
			error      = EXCLUDED.error,
			details    = EXCLUDED.details,
			failures   = CASE
				WHEN EXCLUDED.status = 'failed' AND ` + tbl + `.status <> 'failed' THEN ` + tbl + `.failures + 1
				WHEN EXCLUDED.status IN ('done','skipped','not_applicable') THEN 0
				ELSE ` + tbl + `.failures END,
			lasterror  = CASE
				WHEN EXCLUDED.status <> 'failed' THEN ` + tbl + `.lasterror
				WHEN ` + tbl + `.status = 'failed' THEN COALESCE(EXCLUDED.error, ` + tbl + `.lasterror)
				ELSE EXCLUDED.error END,
			nextretryat = CASE
				WHEN EXCLUDED.status <> 'failed' THEN NULL
				WHEN ` + tbl + `.status = 'failed' THEN ` + tbl + `.nextretryat
				ELSE now() + make_interval(secs => ($6::float8[])[` + tbl + `.failures + 1]) END,
			dispatchedat = NULL`

// ResetForItems sets the given steps back to pending for the given items
// (startedat/finishedat/error = NULL, modifiedat = now; attempts preserved).
// A step reset is waiting for its worker afresh: no failures in a row, no
// retry scheduled (its last error stays, as the record of what it met).
func (s *Steps) ResetForItems(ctx context.Context, itemIDs, steps []string) (int64, error) {
	if len(itemIDs) == 0 || len(steps) == 0 {
		return 0, nil
	}
	if !s.legacy.Load() {
		tag, err := s.pool.Exec(ctx, `UPDATE `+tbl+` SET
			status = 'pending', startedat = NULL, finishedat = NULL, error = NULL, modifiedat = now(),
			failures = 0, nextretryat = NULL, dispatchedat = NULL
			WHERE item_id = ANY($1) AND step = ANY($2)`, itemIDs, steps)
		if !missingRetryColumns(err) {
			if err != nil {
				return 0, err
			}
			return tag.RowsAffected(), nil
		}
		s.legacy.Store(true)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE `+tbl+` SET
		status = 'pending', startedat = NULL, finishedat = NULL, error = NULL, modifiedat = now()
		WHERE item_id = ANY($1) AND step = ANY($2)`, itemIDs, steps)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PromoteTranscodeToPackage promotes package=pending when transcode reaches a
// non-failed terminal state (done|not_applicable|skipped). Failed transcode does
// NOT promote. Routed through Upsert so the ON CONFLICT DO UPDATE semantics match
// the Java reference (a re-run transcode regresses an existing package row back to
// pending, details rewritten) — re-transcode implies re-package. The promotion
// is no run: the package's attempts count the packager's runs.
func (s *Steps) PromoteTranscodeToPackage(ctx context.Context, itemID, transcodeStatus string) error {
	switch transcodeStatus {
	case StatusDone, StatusNotApplicable, StatusSkipped:
	default:
		return nil
	}
	details := "auto-promoted after transcode=" + transcodeStatus
	return s.Upsert(ctx, itemID, "package", StatusPending, nil, &details)
}
