// Package processing ports the CAP ProcessingStepService — the upsert/reset/
// promote logic over com_nalet_katalog_itemprocessingsteps (SPEC §3).
package processing

import (
	"context"
	"errors"

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

var validSteps = map[string]bool{
	"scan": true, "tmdb": true, "tidb": true, "chapter": true, "chromaprint": true,
	"blackframe": true, "silence": true, "subtitle": true, "transcode": true, "package": true,
}

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
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Steps { return &Steps{pool: pool} }

func ValidStep(s string) bool   { return validSteps[s] }
func ValidStatus(s string) bool { return validStatuses[s] }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Upsert performs INSERT ... ON CONFLICT (item_id, step):
//   - insert: attempts=1; startedat=now when status=in_progress; finishedat=now
//     when terminal (done|failed|skipped).
//   - conflict: attempts++, status overwritten; startedat sticky (set to now only
//     on the first transition into in_progress while null); finishedat set on a
//     terminal status (NOT not_applicable); error truncated to 500.
func (s *Steps) Upsert(ctx context.Context, itemID, step, status string, errMsg, details *string) error {
	if !validSteps[step] {
		return ErrBadStep
	}
	if !validStatuses[status] {
		return ErrBadStatus
	}
	var em *string
	if errMsg != nil {
		t := truncate(*errMsg, 500)
		em = &t
	}
	const tbl = "com_nalet_katalog_itemprocessingsteps"
	tag, err := s.pool.Exec(ctx, `INSERT INTO `+tbl+`
		(id, createdat, modifiedat, item_id, step, status, startedat, finishedat, attempts, error, details)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3,
			CASE WHEN $3 = 'in_progress' THEN now() ELSE NULL END,
			CASE WHEN $3 IN ('done','failed','skipped') THEN now() ELSE NULL END,
			1, $4, $5)
		ON CONFLICT (item_id, step) DO UPDATE SET
			modifiedat = now(),
			status     = EXCLUDED.status,
			attempts   = `+tbl+`.attempts + 1,
			startedat  = CASE WHEN EXCLUDED.status = 'in_progress' AND `+tbl+`.startedat IS NULL
			                  THEN now() ELSE `+tbl+`.startedat END,
			finishedat = CASE WHEN EXCLUDED.status IN ('done','failed','skipped') THEN now()
			                  ELSE `+tbl+`.finishedat END,
			error      = EXCLUDED.error,
			details    = EXCLUDED.details`,
		itemID, step, status, em, details)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("processing step upsert affected 0 rows")
	}
	return nil
}

// ResetForItems sets the given steps back to pending for the given items
// (startedat/finishedat/error = NULL, modifiedat = now; attempts preserved).
func (s *Steps) ResetForItems(ctx context.Context, itemIDs, steps []string) (int64, error) {
	if len(itemIDs) == 0 || len(steps) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemprocessingsteps SET
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
// pending, attempts++, details rewritten) — re-transcode implies re-package.
func (s *Steps) PromoteTranscodeToPackage(ctx context.Context, itemID, transcodeStatus string) error {
	switch transcodeStatus {
	case StatusDone, StatusNotApplicable, StatusSkipped:
	default:
		return nil
	}
	details := "auto-promoted after transcode=" + transcodeStatus
	return s.Upsert(ctx, itemID, "package", StatusPending, nil, &details)
}
