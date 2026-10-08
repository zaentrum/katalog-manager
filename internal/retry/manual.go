package retry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// RetryStep retries an item's step now, as an admin asks: a failed step, with
// or without attempts left, or one in progress with no word from its worker,
// or waiting for one to start it, for longer than its timeout. Its failures
// in a row start afresh. A step running or waiting within its timeout is left
// alone, as is one done, not applicable or skipped; the result says why. A
// step that reads the title's original is refused once the original was
// retired, and one that reads its file while that is a disc image.
func (s *Service) RetryStep(ctx context.Context, itemID, step string) (graph.RetryStepResult, error) {
	res := graph.RetryStepResult{ItemID: itemID, Step: step}
	if !processing.ValidStep(step) {
		return res, errUnknownStep(step)
	}
	if step == processing.StepRetire {
		return s.retryRetire(ctx, res)
	}
	if _, _, ok := Trigger(step); !ok {
		return res, fmt.Errorf("%s is katalog-manager's own step, which no event triggers: its job runs it", step)
	}
	if why := s.unavailable(ctx); why != "" {
		return res, errors.New("cannot retry: " + why)
	}
	if slices.Contains(processing.OriginalSteps, step) && s.withOriginal(ctx) != "" {
		o, err := library.OriginalOf(ctx, s.st.Pool(), itemID)
		if err != nil {
			return res, err
		}
		if o.Gone() {
			return res, fmt.Errorf("cannot retry %s: it reads the title's original, and %s", step, o.RetiredAt())
		}
	}
	if slices.Contains(processing.FileSteps, step) {
		if disc, err := library.IsDiscImageTitle(ctx, s.st.Pool(), itemID); err != nil {
			return res, err
		} else if disc {
			return res, fmt.Errorf("cannot retry %s: it reads the title's file, which is a %s", step, processing.DiscImageReason)
		}
	}
	timeout := s.pol.Timeout(step)
	rows, err := s.claim(ctx, `WITH cur AS (
			SELECT s.id, s.status FROM `+tbl+` s
			WHERE s.item_id = $1 AND s.step = $2
			FOR UPDATE)
		UPDATE `+tbl+` s SET `+claimSet(true)+`
		FROM cur
		WHERE s.id = cur.id AND (cur.status = 'failed'
			OR (cur.status = 'in_progress' AND `+heard+` < localtimestamp - make_interval(secs => $3::float8))
			OR (cur.status = 'pending'
			    AND COALESCE(s.dispatchedat, (`+heard+`)::timestamptz) < now() - make_interval(secs => $3::float8)))
		RETURNING s.id, s.item_id, s.step, cur.status,
			COALESCE((SELECT i.type FROM com_nalet_katalog_items i WHERE i.id = s.item_id), '')`,
		itemID, step, timeout.Seconds())
	if err != nil {
		return res, err
	}
	if len(rows) == 0 {
		return s.whyNot(ctx, res, timeout)
	}
	_, _, notSent, first := s.dispatch(ctx, rows, false, asRetry)
	if len(notSent) > 0 {
		failed := processing.StatusFailed
		res.Status = &failed
		res.Message = fmt.Sprintf("the retry could not be sent (%v); %s is failed as it was", first, step)
		return res, nil
	}
	topic, _, _ := Trigger(step)
	status := processing.StatusPending
	res.Message = fmt.Sprintf("retried: %s is waiting for its worker again (%s sent)", step, topic)
	if step == "scan" {
		status = processing.StatusDone
		res.Message = fmt.Sprintf("retried: the item's pipeline runs again from its start (%s sent)", topic)
	}
	res.Retried, res.Status = true, &status
	return res, nil
}

// retryRetire has an item's failed retire step wait for the retire job's
// next pass, its failures in a row afresh: the job runs it, as no event
// does, so it needs no event bus. A retire step waiting or running, or done,
// is left alone, as RetryStep leaves any other.
func (s *Service) retryRetire(ctx context.Context, res graph.RetryStepResult) (graph.RetryStepResult, error) {
	if why := s.stepTable(ctx); why != "" {
		return res, errors.New("cannot retry: " + why)
	}
	tag, err := s.st.Pool().Exec(ctx, resetRetire+` AND item_id = $1`, res.ItemID)
	if err != nil {
		return res, err
	}
	if tag.RowsAffected() == 0 {
		res, err = s.whyNot(ctx, res, s.pol.Timeout(res.Step))
		if err == nil && res.Status != nil {
			switch *res.Status {
			case processing.StatusPending:
				res.Message = "retire is waiting for the retire job's next pass, which comes once a minute: nothing to retry"
			case processing.StatusInProgress:
				res.Message = "retire is running: the retire job is deleting the title's original"
			}
		}
		return res, err
	}
	status := processing.StatusPending
	res.Retried, res.Status = true, &status
	res.Message = "retried: retire waits for the retire job's next pass, which comes once a minute"
	return res, nil
}

// resetRetire sets the failed retire steps waiting for the retire job, their
// failures in a row afresh.
const resetRetire = `UPDATE ` + tbl + ` SET status = 'pending', startedat = NULL, finishedat = NULL, error = NULL,
		nextretryat = NULL, dispatchedat = NULL, failures = 0, modifiedat = now()
	WHERE step = 'retire' AND status = 'failed'`

// whyNot says why a step was left alone.
func (s *Service) whyNot(ctx context.Context, res graph.RetryStepResult, timeout time.Duration) (graph.RetryStepResult, error) {
	var status string
	var at time.Time
	err := s.st.Pool().QueryRow(ctx, `SELECT status, COALESCE(dispatchedat, modifiedat::timestamptz, now())
		FROM `+tbl+` WHERE item_id = $1 AND step = $2`, res.ItemID, res.Step).Scan(&status, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Message = fmt.Sprintf("the item has no %s step: nothing to retry", res.Step)
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.Status = &status
	since := at.UTC().Format(time.RFC3339)
	switch status {
	case processing.StatusDone:
		res.Message = res.Step + " is done: nothing to retry"
	case processing.StatusNotApplicable:
		res.Message = res.Step + " does not apply to the item: nothing to retry"
	case processing.StatusSkipped:
		res.Message = res.Step + " was skipped, which is final: nothing to retry"
	case processing.StatusInProgress:
		res.Message = fmt.Sprintf("%s is running: its worker last reported at %s, and a retry would run it twice; "+
			"wait for it, or for its timeout of %s", res.Step, since, Label(timeout))
	case processing.StatusPending:
		res.Message = fmt.Sprintf("%s is waiting for its worker since %s, and a retry would run it twice; "+
			"wait for it, or for its timeout of %s", res.Step, since, Label(timeout))
	default:
		res.Message = fmt.Sprintf("%s is %s: nothing to retry", res.Step, status)
	}
	return res, nil
}

// RetryFailed retries every failed step (of one step when given) as
// RetryStep does, a batch at a time, and stops at a batch whose events could
// not all be sent. It runs on when the caller stops waiting, and one runs in
// an instance at a time; instances share the work, each step once. The
// failed retire steps, which delete originals, are retried only when the
// step is named, and the steps that read an original that was retired, or a
// file that is a disc image, are left as they are.
func (s *Service) RetryFailed(ctx context.Context, step string) (graph.RetryFailedResult, error) {
	var res graph.RetryFailedResult
	if step != "" {
		if !processing.ValidStep(step) {
			return res, errUnknownStep(step)
		}
		res.Step = &step
	}
	if step == processing.StepRetire {
		return s.retryFailedRetire(ctx, res)
	}
	if why := s.unavailable(ctx); why != "" {
		return res, errors.New("cannot retry: " + why)
	}
	if !s.retryingFailed.CompareAndSwap(false, true) {
		return res, errBusy
	}
	defer s.retryingFailed.Store(false)
	ctx = context.WithoutCancel(ctx)

	var first error
	for {
		rows, err := s.claim(ctx, `WITH failed AS (
				SELECT s.id, s.status FROM `+tbl+` s
				WHERE s.status = 'failed' AND ($1::text = '' OR s.step = $1::text) AND s.step = ANY($3::text[])
				  AND EXISTS (SELECT 1 FROM com_nalet_katalog_items i WHERE i.id = s.item_id)`+s.withOriginal(ctx)+notDiscImage+`
				ORDER BY s.id
				LIMIT $2
				FOR UPDATE SKIP LOCKED)
			UPDATE `+tbl+` s SET `+claimSet(true)+`
			FROM failed WHERE s.id = failed.id
			RETURNING s.id, s.item_id, s.step, failed.status,
				COALESCE((SELECT i.type FROM com_nalet_katalog_items i WHERE i.id = s.item_id), '')`, step, batch, triggered)
		if err != nil {
			return res, err
		}
		sent, items, notSent, err := s.dispatch(ctx, rows, false, asRetry)
		res.Retried += int32(sent)
		res.Items += int32(items)
		res.NotSent += int32(len(notSent))
		if first == nil {
			first = err
		}
		if len(rows) < batch || len(notSent) > 0 {
			break
		}
	}
	what := "failed steps"
	if step != "" {
		what = "failed " + step + " steps"
	}
	switch {
	case res.NotSent > 0:
		res.Message = fmt.Sprintf("retried %d %s; %d could not be sent (%v) and are failed as they were", res.Retried, what, res.NotSent, first)
	case res.Retried == 0:
		res.Message = "no " + what + " to retry"
	default:
		res.Message = fmt.Sprintf("retried %d %s of %d items", res.Retried, what, res.Items)
	}
	return res, nil
}

// retryFailedRetire has every failed retire step wait for the retire job's
// next pass, as retryRetire has one.
func (s *Service) retryFailedRetire(ctx context.Context, res graph.RetryFailedResult) (graph.RetryFailedResult, error) {
	if why := s.stepTable(ctx); why != "" {
		return res, errors.New("cannot retry: " + why)
	}
	tag, err := s.st.Pool().Exec(ctx, resetRetire+`
		AND EXISTS (SELECT 1 FROM com_nalet_katalog_items i WHERE i.id = item_id)`)
	if err != nil {
		return res, err
	}
	n := int32(tag.RowsAffected())
	res.Retried, res.Items = n, n
	if n == 0 {
		res.Message = "no failed retire steps to retry"
	} else {
		res.Message = fmt.Sprintf("retried %d failed retire steps of %d items: they wait for the retire job's next pass", n, n)
	}
	return res, nil
}
