// Package retry heals the processing pipeline. A step that failed is retried
// when its retry is due (processing.Steps schedules it by the policy): the
// event that triggers the step's worker is sent again. A step in progress
// whose worker has been silent for longer than the step's timeout, or one
// sent again that no worker started within it, is reaped: taken for a failed
// run, and so retried in turn while it has attempts left. An admin retries a
// step, or every failed one, by hand, and reads the pipeline's overview.
//
// A retry claims its step in the database before it sends anything. The
// claim is one UPDATE (failed → pending, the step's trigger sent again
// noted in dispatchedat) whose rows only one caller gets (FOR UPDATE SKIP
// LOCKED), so two instances, or an admin and the sweep, never send a step's
// retry twice; a worker's report answers it. Nothing retries a step in
// progress or waiting for its worker within the step's timeout (its worker
// may be alive, and a retry would run it twice), and nothing retries a step
// done, not applicable or skipped: skipped is terminal. An event that could
// not be sent puts its steps back: failed, retried later by the sweep, or
// left for the admin who asked.
//
// The trigger of a step is the event its worker consumes: discovered for tmdb
// (the enricher) and for the scan step (the item's pipeline from its start:
// the scan step is the scanner's, which never reports on it again, so its
// retry leaves it done), enriched for the analyzer's passes, analyzed for the
// transcode and transcoded for the package, and for the take-in (its step
// takein), which katalog-manager sends itself (takein.go). Each worker passes
// the chain on, and its own idempotency guard skips the work that is done.
//
// An admin also encodes a title again whose transcode and package are done
// (reencode.go): both wait for their workers afresh, claimed as a retry is,
// and the transcoder's trigger goes, not marked as a retry, so its guard runs
// the step.
//
// The sweep runs the extras' jobs too (extras.go): an extra of a title is
// packaged on a chain of its own, and retried by the same policy. It runs
// the library's jobs as well, once a minute: the retire job, which deletes a
// title's original after packaging (library.originals) and is the step
// retire of the title, which no event triggers. An admin's retry of retire
// has it wait for the job's next pass. And it sends the re-encode queue
// (queue.go): the titles queued to be encoded again, a few a pass.
//
// The reaper takes a scan job silent for longer than the scan's timeout (the
// scan step's) for lost too: its scanner, which gives it a word every so
// often while it walks, is stuck or gone. The job is failed, saying so; a scan
// is not retried by itself (an admin, or a deployment's scan Job, starts
// another), so this needs no event bus and runs whatever the steps' retries
// can do.
package retry

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Publisher sends item events: events.Producer.
type Publisher interface {
	Enabled() bool
	Publish(ctx context.Context, msgs []events.Message) []error
}

// batch is the most steps a sweep reaps, or claims, at once, and the size of
// a retryFailed batch.
const batch = 200

// Service retries the processing steps.
type Service struct {
	st       *store.Store
	pol      processing.Policy
	pub      Publisher
	interval time.Duration
	// migrated is set once migration 033 is seen in place (it is not undone
	// under a running service).
	migrated atomic.Bool
	// sources is set once migration 040 is seen in place, which tells the
	// titles whose originals were retired.
	sources atomic.Bool
	// retryingFailed is set while a retryFailed runs in this instance.
	retryingFailed atomic.Bool
	// extras are the sweep's jobs for the titles' extras (extras.go).
	extras ExtraJobs
	// library are the library's jobs (extras.go): the retire job.
	library LibraryJobs
	// queue is set once migration 043 is seen in place: the re-encode queue
	// (queue.go).
	queue atomic.Bool
	// takeInMigrated is set once migration 044 is seen in place: a title
	// is taken in (takein.go).
	takeInMigrated atomic.Bool
	// queueSaid is why the sweep last said the queue waits (Run's alone).
	queueSaid string
	// now is the clock the queue's window is read by.
	now func() time.Time
}

// New is the retries of st's steps by pol, sending events through pub (nil:
// no bus), the sweep every interval (0: no sweep).
func New(st *store.Store, pol processing.Policy, pub Publisher, interval time.Duration) *Service {
	return &Service{st: st, pol: pol, pub: pub, interval: interval, now: time.Now}
}

const tbl = "com_nalet_katalog_itemprocessingsteps"

// heard is when step s was last written: for a step in progress, its
// worker's last word. A row written without a time (by hand, or by a seed
// older than this service) has been silent for ever.
const heard = `COALESCE(s.modifiedat, s.startedat, s.createdat, '-infinity'::timestamp)`

// Trigger is the topic of the event that starts step's worker, and the step
// that event names as the one it unblocks.
func Trigger(step string) (topic, next string, ok bool) {
	switch step {
	case "scan", "tmdb":
		return events.TopicDiscovered, "tmdb", true
	case "tidb", "chapter", "chromaprint", "blackframe", "silence", "subtitle":
		return events.TopicEnriched, "analyze", true
	case "transcode":
		return events.TopicAnalyzed, "transcode", true
	case "package":
		return events.TopicTranscoded, "package", true
	case "takein":
		return events.TopicTranscoded, "takein", true
	}
	return "", "", false
}

// triggered are the steps an event starts (Trigger), the only ones a retry
// sends again: retire is katalog-manager's own, run by its retire job.
var triggered = func() []string {
	var out []string
	for _, s := range processing.StepOrder {
		if _, _, ok := Trigger(s); ok {
			out = append(out, s)
		}
	}
	return out
}()

// unavailable says why no step can be retried, "" when one can: the step
// table lacks migration 033, or the service has no event bus.
func (s *Service) unavailable(ctx context.Context) string {
	if why := s.stepTable(ctx); why != "" {
		return why
	}
	if s.pub == nil || !s.pub.Enabled() {
		return "no event bus: a retry sends the step's trigger event again, and KAFKA_BROKERS is not set"
	}
	return ""
}

// stepTable says why the step table keeps no retry, "" when it does: it
// lacks migration 033.
func (s *Service) stepTable(ctx context.Context) string {
	if !s.migrated.Load() {
		ok, err := s.st.StepRetriesReady(ctx)
		if err != nil {
			return "the step table could not be read: " + err.Error()
		}
		if !ok {
			return "migration 033 (db/migrations/033_step_retries.sql) is not applied"
		}
		s.migrated.Store(true)
	}
	return ""
}

// withOriginal is the condition on a claimed step s that one reading the
// title's original is claimed only while the title has it: once the original
// was deleted after packaging, or while it is being deleted, nothing can read
// it, and the step is left as it is. It needs migration 040: without it every
// original is there.
func (s *Service) withOriginal(ctx context.Context) string {
	if !s.sources.Load() {
		ok, err := s.st.LibraryReady(ctx)
		if err != nil || !ok {
			return ""
		}
		s.sources.Store(true)
	}
	return ` AND NOT (s.step = ANY('{` + strings.Join(processing.OriginalSteps, ",") + `}'::text[]) AND ` +
		library.OriginalGone("s.item_id") + `)`
}

// automatic reports whether the sweep sends due retries and reaps by itself.
func (s *Service) automatic() bool { return s.interval > 0 && s.pol.MaxAttempts > 1 }

// Run sweeps every interval until ctx ends; with no interval it returns.
func (s *Service) Run(ctx context.Context) {
	if s.interval <= 0 {
		log.Printf("retry: no sweep (KATALOG_RETRY_INTERVAL off): failed steps are retried by an admin only, no silent step or scan is reaped, " +
			"an extra is sent only when it is taken in or packaged again, and the re-encode queue is not sent")
		if s.library != nil {
			s.runLibrary(ctx)
		}
		return
	}
	log.Printf("retry: sweeping every %s (up to %d runs in a row, a backoff from %s to %s)",
		s.interval, s.pol.MaxAttempts, s.pol.Backoff, s.pol.BackoffMax)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	said := ""
	for {
		if n, err := s.ReapScans(ctx); err != nil {
			log.Printf("retry: %v", err)
		} else if n > 0 {
			log.Printf("retry: %d scans silent past the scan's timeout failed", n)
		}
		s.sweepExtras(ctx)
		s.sweepLibrary(ctx)
		if why := s.unavailable(ctx); why != "" {
			if why != said {
				log.Printf("retry: the sweep idles: %s", why)
				said = why
			}
		} else {
			said = ""
			if reaped, sent, err := s.Sweep(ctx); err != nil {
				log.Printf("retry: sweep: %v", err)
			} else if reaped+sent > 0 {
				log.Printf("retry: sweep: %d steps reaped, %d retries sent", reaped, sent)
			}
			s.sweepTakeIns(ctx)
		}
		s.sweepQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// runLibrary runs the library's jobs once a minute until ctx ends, when
// there is no sweep to run them.
func (s *Service) runLibrary(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.sweepLibrary(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep reaps the silent steps and sends the retries that are due, once. It
// does nothing when no step can be retried (unavailable).
func (s *Service) Sweep(ctx context.Context) (reaped, sent int, err error) {
	if s.unavailable(ctx) != "" {
		return 0, 0, nil
	}
	if reaped, err = s.reap(ctx); err != nil {
		return reaped, 0, err
	}
	for {
		rows, err := s.claimDue(ctx)
		if err != nil {
			return reaped, sent, err
		}
		n, _, notSent, _ := s.dispatch(ctx, rows, true, asRetry)
		sent += n
		if len(rows) < batch || len(notSent) > 0 || ctx.Err() != nil {
			return reaped, sent, ctx.Err()
		}
	}
}

// reap takes the steps silent for longer than their timeout for failed runs:
// a step in progress whose worker has not reported for that long, and one
// sent again that no worker has started within it. Each counts a failure
// and is scheduled for a retry by the policy, like any failure.
func (s *Service) reap(ctx context.Context) (int, error) {
	steps, secs := s.timeouts()
	labels := make([]string, len(secs))
	for i, v := range secs {
		labels[i] = Label(time.Duration(v * float64(time.Second)))
	}
	tag, err := s.st.Pool().Exec(ctx, `WITH t(step, secs, label) AS (SELECT * FROM unnest($1::text[], $2::float8[], $3::text[])),
		stale AS (
			SELECT s.id, s.status, COALESCE(t.label, $5::text) AS label
			FROM `+tbl+` s LEFT JOIN t ON t.step = s.step
			WHERE (s.status = 'in_progress'
			       AND `+heard+` < localtimestamp - make_interval(secs => COALESCE(t.secs, $4::float8)))
			   OR (s.status = 'pending' AND s.dispatchedat IS NOT NULL
			       AND s.dispatchedat < now() - make_interval(secs => COALESCE(t.secs, $4::float8)))
			ORDER BY s.modifiedat
			LIMIT $6
			FOR UPDATE OF s SKIP LOCKED)
		UPDATE `+tbl+` s SET
			status = 'failed', finishedat = now(), modifiedat = now(),
			error = m.msg, lasterror = m.msg,
			failures = s.failures + 1,
			nextretryat = now() + make_interval(secs => ($7::float8[])[s.failures + 1]),
			dispatchedat = NULL
		FROM stale, LATERAL (SELECT CASE stale.status
			WHEN 'in_progress' THEN 'timed out: no word from its worker for ' || stale.label || ' (the step''s timeout)'
			ELSE 'timed out: its trigger was sent again and no worker started it within ' || stale.label || ' (the step''s timeout)'
			END AS msg) m
		WHERE s.id = stale.id`,
		steps, secs, labels, s.pol.DefaultTimeout.Seconds(), Label(s.pol.DefaultTimeout), batch, s.delays())
	if err != nil {
		return 0, fmt.Errorf("reap the silent steps: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ReapScans fails the scan jobs whose scanner has said nothing for longer
// than the scan's timeout, as reap takes a silent step for a failed run, and
// returns how many. Run calls it on every sweep, an event bus or not.
func (s *Service) ReapScans(ctx context.Context) (int, error) {
	timeout := s.pol.Timeout("scan")
	n, err := s.st.FailSilentScanJobs(ctx, timeout,
		"timed out: no word from its scanner for "+Label(timeout)+" (the scan's timeout)")
	if err != nil {
		return 0, fmt.Errorf("reap the silent scans: %w", err)
	}
	return n, nil
}

// claimed is a step claimed for a retry.
type claimed struct {
	id, itemID, step, itemType string
	was                        string // its status before the claim
}

// claimSet is what a claim does to a step: it waits for its worker again
// (pending; the scan step done, see the package doc), the trigger sent again
// noted. An admin's claim starts its failures in a row afresh.
func claimSet(manual bool) string {
	set := `status = CASE WHEN s.step = 'scan' THEN 'done' ELSE 'pending' END,
		startedat = NULL, finishedat = CASE WHEN s.step = 'scan' THEN now() ELSE NULL END,
		error = NULL, nextretryat = NULL, dispatchedat = now(), modifiedat = now()`
	if manual {
		set += `, failures = 0`
	}
	return set
}

// claimDue claims the failed steps whose retry is due, oldest first, at most
// a batch of them; a step whose item is gone is left alone, and so is one no
// event triggers (retire), and one that reads the original of a title whose
// original was retired (withOriginal).
func (s *Service) claimDue(ctx context.Context) ([]claimed, error) {
	return s.claim(ctx, `WITH due AS (
			SELECT s.id, s.status FROM `+tbl+` s
			WHERE s.status = 'failed' AND s.nextretryat <= now() AND s.step = ANY($2::text[])
			  AND EXISTS (SELECT 1 FROM com_nalet_katalog_items i WHERE i.id = s.item_id)`+s.withOriginal(ctx)+`
			ORDER BY s.nextretryat, s.id
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		UPDATE `+tbl+` s SET `+claimSet(false)+`
		FROM due WHERE s.id = due.id
		RETURNING s.id, s.item_id, s.step, due.status,
			COALESCE((SELECT i.type FROM com_nalet_katalog_items i WHERE i.id = s.item_id), '')`, batch, triggered)
}

func (s *Service) claim(ctx context.Context, sql string, args ...any) ([]claimed, error) {
	rows, err := s.st.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("claim the steps to retry: %w", err)
	}
	defer rows.Close()
	var out []claimed
	for rows.Next() {
		var c claimed
		if err := rows.Scan(&c.id, &c.itemID, &c.step, &c.was, &c.itemType); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// send is how dispatch marks the events it sends, and what a step whose event
// could not be sent says of it. A retry's event is marked as one (status and
// source "retry"): a worker whose step has finished since it was sent does
// nothing. A re-encode's is not, so a worker's guard finds its step waiting
// and runs it.
type send struct{ mark, what string }

var (
	asRetry    = send{mark: "retry", what: "the retry"}
	asReencode = send{mark: "reencode", what: "the re-encode"}
)

// dispatch sends the trigger event of each claimed step, one per item and
// topic, marked as how says, and puts back the steps whose event could not be
// sent: failed, and retried a backoff later when automatic (the sweep sends
// it), left for the admin otherwise. It returns the steps sent, the items
// their events went to, the steps not sent and the first error.
func (s *Service) dispatch(ctx context.Context, rows []claimed, automatic bool, how send) (sent, items int, notSent []claimed, first error) {
	if len(rows) == 0 {
		return 0, 0, nil, nil
	}
	type key struct{ item, topic string }
	at := map[key]int{}
	var msgs []events.Message
	var of [][]claimed
	for _, c := range rows {
		topic, next, _ := Trigger(c.step)
		k := key{c.itemID, topic}
		i, ok := at[k]
		if !ok {
			ev := events.NewItemEvent(c.itemID)
			ev.Type, ev.Step, ev.Status, ev.Source = c.itemType, next, how.mark, how.mark
			i = len(msgs)
			at[k] = i
			msgs = append(msgs, events.Message{Topic: topic, Event: ev})
			of = append(of, nil)
		}
		of[i] = append(of[i], c)
	}
	errs := s.pub.Publish(ctx, msgs)
	seen := map[string]bool{}
	var back []string
	for i, err := range errs {
		if err != nil {
			if first == nil {
				first = err
			}
			notSent = append(notSent, of[i]...)
			for _, c := range of[i] {
				back = append(back, c.id)
			}
			continue
		}
		sent += len(of[i])
		if !seen[msgs[i].Event.ItemID] {
			seen[msgs[i].Event.ItemID] = true
			items++
		}
	}
	if len(back) > 0 {
		again := 0.0
		if automatic {
			again = s.pol.Backoff.Seconds()
		}
		// Only the steps still waiting on this dispatch: a worker's report
		// clears dispatchedat.
		if _, err := s.st.Pool().Exec(context.WithoutCancel(ctx), `UPDATE `+tbl+` SET
				status = 'failed', finishedat = now(), modifiedat = now(), error = $2,
				nextretryat = CASE WHEN $3::float8 > 0 THEN now() + make_interval(secs => $3::float8) END,
				dispatchedat = NULL
			WHERE id = ANY($1) AND dispatchedat IS NOT NULL`,
			back, *processing.CleanError(how.what + " could not be sent: " + first.Error()), again); err != nil {
			log.Printf("retry: %d steps could not be put back either after %s could not be sent: %v", len(back), how.what, err)
		}
	}
	return sent, items, notSent, first
}

// delays are the policy's delays after a step's 1st, 2nd, ... failure in a
// row, as processing.Steps schedules them.
func (s *Service) delays() []float64 {
	out := []float64{}
	for n := 1; s.pol.Retries(n); n++ {
		out = append(out, s.pol.Delay(n).Seconds())
	}
	return out
}

// timeouts are every step's timeout, in seconds, as parallel arrays.
func (s *Service) timeouts() ([]string, []float64) {
	steps := append([]string(nil), processing.StepOrder...)
	secs := make([]float64, len(steps))
	for i, st := range steps {
		secs[i] = s.pol.Timeout(st).Seconds()
	}
	return steps, secs
}

// Label writes d as short as it reads: 6h, 1h30m, 15m, 90s, 1m30s.
func Label(d time.Duration) string {
	out := d.Round(time.Second).String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}

// errUnknownStep refuses a step the pipeline does not have.
func errUnknownStep(step string) error {
	return fmt.Errorf("unknown step %q: one of %s", step, strings.Join(processing.StepOrder, ", "))
}

var _ graph.Pipeline = (*Service)(nil)

// errBusy says a retryFailed is running in this instance already.
var errBusy = errors.New("a retry of the failed steps is running already")
