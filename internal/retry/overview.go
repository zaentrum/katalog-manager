package retry

import (
	"context"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// Overview says what the pipeline holds: every step, in the order the
// pipeline runs them (a step it does not know after them), with how many
// items are in each state of it, how many of the failed the service retries
// by itself, and how many are silent past their timeout; the failed steps (of
// step when given), the latest failure first, at most limit (default 50, max
// 200) from offset, and how many failed in all; and how the service retries.
func (s *Service) Overview(ctx context.Context, step string, limit, offset int32) (graph.ProcessingOverview, error) {
	var o graph.ProcessingOverview
	if step != "" && !processing.ValidStep(step) {
		return o, errUnknownStep(step)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset = max(offset, 0)
	why := s.unavailable(ctx)
	o.Retry = graph.RetryPolicyInfo{
		Automatic:         why == "" && s.automatic(),
		Available:         why == "",
		MaxAttempts:       int32(s.pol.MaxAttempts),
		BackoffSeconds:    int32(s.pol.Backoff.Seconds()),
		BackoffMaxSeconds: int32(s.pol.BackoffMax.Seconds()),
		IntervalSeconds:   int32(s.interval.Seconds()),
	}
	if why != "" {
		o.Retry.Reason = &why
	}
	// Without migration 033 the steps have no retries to read: the counts
	// read what there is.
	retries := s.migrated.Load()

	counts := map[string]*graph.StepCounts{}
	var extra []string
	for _, st := range processing.StepOrder {
		counts[st] = &graph.StepCounts{Step: st, TimeoutSeconds: int32(s.pol.Timeout(st).Seconds())}
	}
	steps, secs := s.timeouts()
	q := `SELECT s.step,
			count(*) FILTER (WHERE s.status = 'pending'),
			count(*) FILTER (WHERE s.status = 'in_progress'),
			count(*) FILTER (WHERE s.status = 'done'),
			count(*) FILTER (WHERE s.status = 'failed'),
			count(*) FILTER (WHERE s.status = 'skipped'),
			count(*) FILTER (WHERE s.status = 'not_applicable'),
			0::bigint, count(*) FILTER (WHERE s.status = 'in_progress'
				AND ` + heard + ` < localtimestamp - make_interval(secs => COALESCE(t.secs, $3::float8)))
		FROM ` + tbl + ` s LEFT JOIN unnest($1::text[], $2::float8[]) AS t(step, secs) ON t.step = s.step
		GROUP BY s.step`
	if retries {
		q = `SELECT s.step,
			count(*) FILTER (WHERE s.status = 'pending'),
			count(*) FILTER (WHERE s.status = 'in_progress'),
			count(*) FILTER (WHERE s.status = 'done'),
			count(*) FILTER (WHERE s.status = 'failed'),
			count(*) FILTER (WHERE s.status = 'skipped'),
			count(*) FILTER (WHERE s.status = 'not_applicable'),
			count(*) FILTER (WHERE s.status = 'failed' AND s.nextretryat IS NOT NULL),
			count(*) FILTER (WHERE (s.status = 'in_progress'
				AND ` + heard + ` < localtimestamp - make_interval(secs => COALESCE(t.secs, $3::float8)))
				OR (s.status = 'pending' AND s.dispatchedat < now() - make_interval(secs => COALESCE(t.secs, $3::float8))))
		FROM ` + tbl + ` s LEFT JOIN unnest($1::text[], $2::float8[]) AS t(step, secs) ON t.step = s.step
		GROUP BY s.step`
	}
	rows, err := s.st.Pool().Query(ctx, q, steps, secs, s.pol.DefaultTimeout.Seconds())
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var name string
		var n [8]int64
		if err := rows.Scan(&name, &n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6], &n[7]); err != nil {
			rows.Close()
			return o, err
		}
		c, ok := counts[name]
		if !ok {
			c = &graph.StepCounts{Step: name, TimeoutSeconds: int32(s.pol.Timeout(name).Seconds())}
			counts[name] = c
			extra = append(extra, name)
		}
		c.Pending, c.InProgress, c.Done, c.Failed = int32(n[0]), int32(n[1]), int32(n[2]), int32(n[3])
		c.Skipped, c.NotApplicable, c.Retrying, c.Stalled = int32(n[4]), int32(n[5]), int32(n[6]), int32(n[7])
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return o, err
	}
	for _, st := range append(append([]string(nil), processing.StepOrder...), extra...) {
		o.Steps = append(o.Steps, *counts[st])
	}

	if err := s.st.Pool().QueryRow(ctx, `SELECT count(*) FROM `+tbl+` s
		WHERE s.status = 'failed' AND ($1::text = '' OR s.step = $1::text)`, step).Scan(&o.FailedTotal); err != nil {
		return o, err
	}
	failures, last, next := `0`, `s.error`, `NULL::timestamptz`
	if retries {
		failures, last, next = `s.failures`, `COALESCE(s.lasterror, s.error)`, `s.nextretryat`
	}
	rows, err = s.st.Pool().Query(ctx, `SELECT s.item_id, i.title, i.type, p.id, p.title, i.seasonnumber, i.episodenumber,
			s.step, `+failures+`, `+last+`, COALESCE(s.finishedat, s.modifiedat), `+next+`
		FROM `+tbl+` s
		JOIN com_nalet_katalog_items i ON i.id = s.item_id
		LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
		WHERE s.status = 'failed' AND ($1::text = '' OR s.step = $1::text)
		ORDER BY COALESCE(s.finishedat, s.modifiedat) DESC NULLS LAST, s.item_id, s.step
		LIMIT $2 OFFSET $3`, step, limit, offset)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	o.Failed = []graph.FailedStep{}
	for rows.Next() {
		var f graph.FailedStep
		if err := rows.Scan(&f.ItemID, &f.ItemTitle, &f.ItemType, &f.SeriesID, &f.SeriesTitle, &f.SeasonNumber,
			&f.EpisodeNumber, &f.Step, &f.Failures, &f.LastError, &f.FailedAt, &f.NextRetryAt); err != nil {
			return o, err
		}
		f.NextRetryAt = utc(f.NextRetryAt)
		o.Failed = append(o.Failed, f)
	}
	return o, rows.Err()
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
