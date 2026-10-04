package retry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// bus stands in for the event bus: it records what it is asked to send, and
// refuses the events of the items in refuse.
type bus struct {
	mu     sync.Mutex
	off    bool
	refuse map[string]bool
	sent   []events.Message
}

func (b *bus) Enabled() bool { return !b.off }

func (b *bus) Publish(_ context.Context, msgs []events.Message) []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		if b.refuse[m.Event.ItemID] {
			errs[i] = errors.New("broker: not the leader")
			continue
		}
		b.sent = append(b.sent, m)
	}
	return errs
}

// take returns what was sent since the last take, as "topic item step
// status source type", sorted.
func (b *bus) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, m := range b.sent {
		e := m.Event
		out = append(out, strings.Join([]string{m.Topic, e.ItemID, e.Step, e.Status, e.Source, e.Type}, " "))
	}
	b.sent = nil
	sort.Strings(out)
	return out
}

var testPolicy = processing.Policy{MaxAttempts: 3, Backoff: time.Minute, BackoffMax: time.Hour,
	Timeouts: map[string]time.Duration{"transcode": time.Hour, "package": 2 * time.Hour}, DefaultTimeout: 2 * time.Hour}

func newService(t *testing.T, st *store.Store, b *bus) *Service {
	t.Helper()
	return New(st, testPolicy, b, 30*time.Second)
}

// put gives item a step in status, with the columns set says ("failures = 1,
// nextretryat = now() - interval '1 second'").
func put(t *testing.T, st *store.Store, item, step, status, set string) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts)
		VALUES (gen_random_uuid()::varchar, localtimestamp, localtimestamp, $1, $2, $3, 1)`, item, step, status)
	if set != "" {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET `+set+` WHERE item_id = $1 AND step = $2`, item, step)
	}
}

// state is a step's status, failures, error, last error and whether it has a
// retry scheduled and a dispatch outstanding.
func state(t *testing.T, st *store.Store, item, step string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT status || ' ' || failures || ' error=' || COALESCE(error, '-') ||
		' last=' || COALESCE(lasterror, '-') || ' retry=' || (nextretryat IS NOT NULL) || ' sent=' || (dispatchedat IS NOT NULL)
		FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = $2`, item, step).Scan(&out); err != nil {
		t.Fatalf("the %s step of %s: %v", step, item, err)
	}
	return out
}

// retryIn is the seconds until a step's retry, from now.
func retryIn(t *testing.T, st *store.Store, item, step string) float64 {
	t.Helper()
	var secs float64
	if err := st.Pool().QueryRow(context.Background(), `SELECT extract(epoch FROM nextretryat - now())::float8
		FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = $2`, item, step).Scan(&secs); err != nil {
		t.Fatalf("the retry of %s %s: %v", item, step, err)
	}
	return secs
}

func catalog(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "Sintel", "")
	storetest.AddItem(t, st, "m2", "movie", "Tears of Steel", "")
	storetest.AddItem(t, st, "s1", "series", "Pioneer One", "")
	storetest.AddItem(t, st, "e1", "episode", "Earthfall", "s1")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2 WHERE id = 'e1'`)
	return st
}

const due = `failures = 1, lasterror = 'it failed', error = 'it failed', nextretryat = now() - interval '1 second'`

// A sweep sends the retries that are due, one event per item and worker, on
// the topic the step's worker consumes, and leaves every other step alone: a
// retry not due yet, a failure with no retry scheduled, a step whose item is
// gone, and any step that is not failed.
func TestASweepSendsTheRetriesThatAreDue(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "failed", due)
	put(t, st, "m1", "subtitle", "failed", due)
	put(t, st, "m1", "blackframe", "failed", due)
	put(t, st, "m2", "package", "failed", due)
	put(t, st, "s1", "tmdb", "failed", due)
	put(t, st, "e1", "scan", "failed", due)
	put(t, st, "e1", "chapter", "failed", `failures = 1, nextretryat = now() + interval '5 minutes'`)
	put(t, st, "e1", "silence", "failed", `failures = 3`)
	put(t, st, "gone", "transcode", "failed", due)
	for _, status := range []string{"done", "skipped", "not_applicable", "pending", "in_progress"} {
		put(t, st, "m2", map[string]string{"done": "tidb", "skipped": "chapter", "not_applicable": "silence",
			"pending": "subtitle", "in_progress": "blackframe"}[status], status, `nextretryat = now() - interval '1 second'`)
	}

	reaped, sent, err := s.Sweep(context.Background())
	if err != nil || reaped != 0 || sent != 6 {
		t.Fatalf("Sweep: reaped %d, sent %d, %v; want 6 sent", reaped, sent, err)
	}
	want := []string{
		events.TopicAnalyzed + " m1 transcode retry retry movie",
		events.TopicDiscovered + " e1 tmdb retry retry episode",
		events.TopicDiscovered + " s1 tmdb retry retry series",
		events.TopicEnriched + " m1 analyze retry retry movie",
		events.TopicTranscoded + " m2 package retry retry movie",
	}
	sort.Strings(want)
	if got := b.take(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range [][2]string{{"m1", "transcode"}, {"m1", "subtitle"}, {"m1", "blackframe"}, {"m2", "package"}, {"s1", "tmdb"}} {
		if got := state(t, st, c[0], c[1]); got != "pending 1 error=- last=it failed retry=false sent=true" {
			t.Errorf("%s %s after its retry: %s", c[0], c[1], got)
		}
	}
	if got := state(t, st, "e1", "scan"); got != "done 1 error=- last=it failed retry=false sent=true" {
		t.Errorf("the scan step after its retry: %s, want done (the pipeline runs again from its start), its failure kept", got)
	}
	for _, c := range [][2]string{{"e1", "chapter"}, {"e1", "silence"}, {"gone", "transcode"}} {
		if got := state(t, st, c[0], c[1]); !strings.HasPrefix(got, "failed") || strings.Contains(got, "sent=true") {
			t.Errorf("%s %s was touched: %s", c[0], c[1], got)
		}
	}
	for _, step := range []string{"tidb", "chapter", "silence", "subtitle", "blackframe"} {
		if got := state(t, st, "m2", step); strings.Contains(got, "sent=true") {
			t.Errorf("m2 %s, not failed, was retried: %s", step, got)
		}
	}
	// nothing is due any more
	if _, sent, err := s.Sweep(context.Background()); err != nil || sent != 0 || len(b.take()) != 0 {
		t.Errorf("a second sweep sent %d: %v", sent, err)
	}
}

// The reaper takes a step for a failed run when its worker has been silent
// for longer than the step's timeout, and one sent again that no worker
// started within it, and schedules its retry like any failure's; a step
// within its timeout, one waiting that the service did not send, and one
// with no attempt left after it, are dealt with as they are.
func TestTheReaperTakesSilentStepsForFailedRuns(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "in_progress", `modifiedat = localtimestamp - interval '61 minutes'`)
	put(t, st, "m2", "transcode", "in_progress", `modifiedat = localtimestamp - interval '59 minutes'`)
	put(t, st, "m1", "package", "pending", `dispatchedat = now() - interval '121 minutes', modifiedat = localtimestamp - interval '121 minutes'`)
	put(t, st, "m2", "package", "pending", `modifiedat = localtimestamp - interval '30 days'`)
	put(t, st, "e1", "subtitle", "in_progress", `modifiedat = NULL, startedat = NULL, createdat = NULL`)
	put(t, st, "e1", "chapter", "in_progress", `failures = 2, modifiedat = localtimestamp - interval '3 hours'`)
	put(t, st, "s1", "tmdb", "done", `modifiedat = localtimestamp - interval '30 days'`)

	reaped, sent, err := s.Sweep(context.Background())
	if err != nil || reaped != 4 || sent != 0 {
		t.Fatalf("Sweep: reaped %d, sent %d, %v; want 4 reaped, nothing sent yet", reaped, sent, err)
	}
	if got := state(t, st, "m1", "transcode"); got != "failed 1 error=timed out: no word from its worker for 1h (the step's timeout) "+
		"last=timed out: no word from its worker for 1h (the step's timeout) retry=true sent=false" {
		t.Errorf("a silent transcode: %s", got)
	}
	if in := retryIn(t, st, "m1", "transcode"); in < 55 || in > 61 {
		t.Errorf("a silent transcode is retried in %vs, want the first backoff (60s)", in)
	}
	if got := state(t, st, "m1", "package"); !strings.HasPrefix(got,
		"failed 1 error=timed out: its trigger was sent again and no worker started it within 2h (the step's timeout)") {
		t.Errorf("a package sent again and never started: %s", got)
	}
	if got := state(t, st, "e1", "subtitle"); !strings.HasPrefix(got, "failed 1 error=timed out: no word from its worker for 2h") {
		t.Errorf("a step in progress written without a time: %s", got)
	}
	if got := state(t, st, "e1", "chapter"); !strings.HasPrefix(got, "failed 3 ") || !strings.HasSuffix(got, "retry=false sent=false") {
		t.Errorf("a step reaped with no attempt left: %s, want three failures and no retry", got)
	}
	for _, c := range [][3]string{{"m2", "transcode", "in_progress"}, {"m2", "package", "pending"}, {"s1", "tmdb", "done"}} {
		if got := state(t, st, c[0], c[1]); !strings.HasPrefix(got, c[2]+" 0 ") {
			t.Errorf("%s %s was reaped: %s", c[0], c[1], got)
		}
	}

	// The silent worker was alive after all: its late report ends it, and
	// no retry is sent; the reaped package's retry is sent when due.
	steps := processing.New(st.Pool()).WithPolicy(testPolicy)
	if err := steps.Upsert(context.Background(), "m1", "transcode", processing.StatusDone, nil, nil); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET nextretryat = now() - interval '1 second' WHERE nextretryat IS NOT NULL`)
	if _, sent, err := s.Sweep(context.Background()); err != nil || sent != 2 {
		t.Fatalf("the sweep after the late report: sent %d, %v; want the package and the subtitle", sent, err)
	}
	if got := strings.Join(b.take(), "\n"); got != events.TopicEnriched+" e1 analyze retry retry episode\n"+
		events.TopicTranscoded+" m1 package retry retry movie" {
		t.Errorf("sent:\n%s", got)
	}
}

// A step the policy retries is retried until its attempts are used up: a
// worker's failure schedules it, the sweep sends it, the worker fails it
// again, and after the last attempt it is left for an admin.
func TestAFailingStepIsRetriedUntilItsAttemptsAreUsedUp(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	pol := testPolicy
	pol.Backoff, pol.BackoffMax = time.Millisecond, time.Millisecond
	s := New(st, pol, b, time.Second)
	steps := processing.New(st.Pool()).WithPolicy(pol)
	ctx := context.Background()
	for run := 1; run <= 3; run++ {
		if err := steps.Upsert(ctx, "m1", "transcode", processing.StatusInProgress, nil, nil); err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("ffmpeg exited %d", run)
		if err := steps.Upsert(ctx, "m1", "transcode", processing.StatusFailed, &msg, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		_, sent, err := s.Sweep(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[bool]int{true: 1, false: 0}[run < 3]; sent != want {
			t.Errorf("run %d failed: the sweep sent %d, want %d", run, sent, want)
		}
	}
	if got := state(t, st, "m1", "transcode"); got != "failed 3 error=ffmpeg exited 3 last=ffmpeg exited 3 retry=false sent=false" {
		t.Errorf("after three failed runs: %s", got)
	}
	if got := len(b.take()); got != 2 {
		t.Errorf("%d retries sent, want 2", got)
	}
}

// An event that could not be sent puts its steps back: failed, and retried a
// backoff later, their failures as they were; the other events go.
func TestAnEventThatCouldNotBeSentPutsItsStepsBack(t *testing.T) {
	st := catalog(t)
	b := &bus{refuse: map[string]bool{"m1": true}}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "failed", due)
	put(t, st, "m2", "transcode", "failed", due)
	if _, sent, err := s.Sweep(context.Background()); err != nil || sent != 1 {
		t.Fatalf("Sweep: sent %d, %v", sent, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m2 ") {
		t.Errorf("sent %v, want m2's", got)
	}
	if got := state(t, st, "m1", "transcode"); got != "failed 1 error=the retry could not be sent: broker: not the leader last=it failed retry=true sent=false" {
		t.Errorf("a step whose retry could not be sent: %s", got)
	}
	if in := retryIn(t, st, "m1", "transcode"); in < 55 || in > 61 {
		t.Errorf("it is retried again in %vs, want the backoff (60s)", in)
	}
}

// Without an event bus, or without migration 033, nothing is retried: the
// sweep does nothing, an admin's retry is refused saying why, and the
// overview says so.
func TestNothingIsRetriedWithoutABusOrTheMigration(t *testing.T) {
	st := catalog(t)
	put(t, st, "m1", "transcode", "failed", due)
	put(t, st, "m2", "transcode", "in_progress", `modifiedat = localtimestamp - interval '3 hours'`)
	for name, s := range map[string]*Service{"no bus": newService(t, st, &bus{off: true}), "a nil bus": New(st, testPolicy, nil, time.Second)} {
		if reaped, sent, err := s.Sweep(context.Background()); err != nil || reaped+sent != 0 {
			t.Errorf("%s: the sweep reaped %d and sent %d: %v", name, reaped, sent, err)
		}
		if _, err := s.RetryStep(context.Background(), "m1", "transcode"); err == nil || !strings.Contains(err.Error(), "cannot retry: no event bus") {
			t.Errorf("%s: an admin's retry: %v", name, err)
		}
		if _, err := s.RetryFailed(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no event bus") {
			t.Errorf("%s: retryFailed: %v", name, err)
		}
		o, err := s.Overview(context.Background(), "", 0, 0)
		if err != nil || o.Retry.Available || o.Retry.Automatic || o.Retry.Reason == nil || !strings.Contains(*o.Retry.Reason, "KAFKA_BROKERS") {
			t.Errorf("%s: the overview's retry: %+v, %v", name, o.Retry, err)
		}
	}
	if got := state(t, st, "m1", "transcode"); !strings.HasPrefix(got, "failed 1") || strings.Contains(got, "sent=true") {
		t.Errorf("a step changed without a bus: %s", got)
	}

	base := storetest.OpenBase(t)
	storetest.AddItem(t, base, "m1", "movie", "Sintel", "")
	storetest.Exec(t, base, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, error) VALUES ('x', 'm1', 'transcode', 'failed', 'boom')`)
	s := newService(t, base, &bus{})
	if reaped, sent, err := s.Sweep(context.Background()); err != nil || reaped+sent != 0 {
		t.Errorf("without 033: the sweep reaped %d and sent %d: %v", reaped, sent, err)
	}
	if _, err := s.RetryStep(context.Background(), "m1", "transcode"); err == nil || !strings.Contains(err.Error(), "migration 033") {
		t.Errorf("without 033: an admin's retry: %v", err)
	}
	o, err := s.Overview(context.Background(), "", 0, 0)
	if err != nil || o.Retry.Available || o.FailedTotal != 1 || len(o.Failed) != 1 || o.Failed[0].Failures != 0 ||
		o.Failed[0].LastError == nil || *o.Failed[0].LastError != "boom" || o.Steps[8].Failed != 1 {
		t.Errorf("without 033, the overview: %+v, %v", o, err)
	}
}

// Sweeps at once, as two instances run them, send each due retry once.
func TestSweepsAtOnceSendEachRetryOnce(t *testing.T) {
	st := storetest.Open(t)
	for i := 0; i < 450; i++ {
		id := fmt.Sprintf("m%03d", i)
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		put(t, st, id, "transcode", "failed", due)
	}
	b := &bus{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := newService(t, st, b).Sweep(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	sent := b.take()
	seen := map[string]int{}
	for _, m := range sent {
		seen[strings.Fields(m)[1]]++
	}
	if len(sent) != 450 || len(seen) != 450 {
		t.Errorf("%d events for %d items, want one for each of 450", len(sent), len(seen))
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE status = 'pending' AND dispatchedat IS NOT NULL`); n != 450 {
		t.Errorf("%d steps waiting on their retry, want 450", n)
	}
}

// An admin retries a step that failed, with attempts left or none, and one
// in progress or waiting for longer than its timeout; its failures start
// afresh. A step running or waiting within its timeout is left alone (its
// worker may be alive), as is one done, not applicable or skipped, and the
// result says why; an unknown step is an error.
func TestAnAdminRetriesAStep(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "failed", `failures = 3, lasterror = 'out of memory'`)
	put(t, st, "m1", "package", "in_progress", `modifiedat = localtimestamp - interval '5 minutes'`)
	put(t, st, "m1", "subtitle", "in_progress", `modifiedat = localtimestamp - interval '3 hours'`)
	put(t, st, "m1", "blackframe", "pending", `dispatchedat = now() - interval '1 minute'`)
	put(t, st, "m1", "silence", "pending", `dispatchedat = now() - interval '3 hours', modifiedat = localtimestamp - interval '3 hours'`)
	put(t, st, "m1", "chapter", "pending", `modifiedat = NULL, createdat = NULL`)
	put(t, st, "m2", "transcode", "done", "")
	put(t, st, "m2", "package", "not_applicable", "")
	put(t, st, "m2", "subtitle", "skipped", "")
	put(t, st, "e1", "scan", "failed", `failures = 1, lasterror = 'file missing'`)

	for _, tc := range []struct {
		item, step string
		retried    bool
		status     string // "" for none
		says       string
		sent       string
	}{
		{"m1", "transcode", true, "pending", "retried: transcode is waiting for its worker again", events.TopicAnalyzed + " m1 transcode retry retry movie"},
		{"m1", "package", false, "in_progress", "package is running: its worker last reported at", ""},
		{"m1", "subtitle", true, "pending", "retried", events.TopicEnriched + " m1 analyze retry retry movie"},
		{"m1", "blackframe", false, "pending", "blackframe is waiting for its worker since", ""},
		{"m1", "silence", true, "pending", "retried", events.TopicEnriched + " m1 analyze retry retry movie"},
		{"m1", "chapter", true, "pending", "retried", events.TopicEnriched + " m1 analyze retry retry movie"},
		{"m2", "transcode", false, "done", "transcode is done: nothing to retry", ""},
		{"m2", "package", false, "not_applicable", "package does not apply to the item: nothing to retry", ""},
		{"m2", "subtitle", false, "skipped", "subtitle was skipped, which is final: nothing to retry", ""},
		{"m2", "tidb", false, "", "the item has no tidb step: nothing to retry", ""},
		{"gone", "transcode", false, "", "the item has no transcode step", ""},
		{"e1", "scan", true, "done", "retried: the item's pipeline runs again from its start", events.TopicDiscovered + " e1 tmdb retry retry episode"},
	} {
		res, err := s.RetryStep(context.Background(), tc.item, tc.step)
		if err != nil {
			t.Errorf("%s %s: %v", tc.item, tc.step, err)
			continue
		}
		status := ""
		if res.Status != nil {
			status = *res.Status
		}
		if res.Retried != tc.retried || status != tc.status || !strings.Contains(res.Message, tc.says) || res.ItemID != tc.item || res.Step != tc.step {
			t.Errorf("%s %s: %+v (status %q), want retried %v, status %q, saying %q", tc.item, tc.step, res, status, tc.retried, tc.status, tc.says)
		}
		if got := strings.Join(b.take(), "\n"); got != tc.sent {
			t.Errorf("%s %s sent %q, want %q", tc.item, tc.step, got, tc.sent)
		}
	}
	if got := state(t, st, "m1", "transcode"); got != "pending 0 error=- last=out of memory retry=false sent=true" {
		t.Errorf("an exhausted step retried by an admin: %s, want its failures afresh, its last error kept", got)
	}
	if got := state(t, st, "m1", "package"); !strings.HasPrefix(got, "in_progress 0") || strings.Contains(got, "sent=true") {
		t.Errorf("a running step was touched: %s", got)
	}
	// A retry asked again while it waits is refused: no step is sent twice.
	if res, err := s.RetryStep(context.Background(), "m1", "transcode"); err != nil || res.Retried || !strings.Contains(res.Message, "waiting for its worker") {
		t.Errorf("a retry asked twice: %+v, %v", res, err)
	}
	if _, err := s.RetryStep(context.Background(), "m1", "encode"); err == nil || !strings.Contains(err.Error(), `unknown step "encode"`) {
		t.Errorf("an unknown step: %v", err)
	}
	// An event that could not be sent leaves the step failed, for the admin.
	b.refuse = map[string]bool{"m2": true}
	put(t, st, "m2", "chapter", "failed", `failures = 1, nextretryat = now() + interval '1 minute'`)
	res, err := s.RetryStep(context.Background(), "m2", "chapter")
	if err != nil || res.Retried || res.Status == nil || *res.Status != "failed" || !strings.Contains(res.Message, "could not be sent") {
		t.Errorf("a retry that could not be sent: %+v, %v", res, err)
	}
	if got := state(t, st, "m2", "chapter"); got != "failed 0 error=the retry could not be sent: broker: not the leader last=- retry=false sent=false" {
		t.Errorf("after a retry that could not be sent: %s", got)
	}
}

// retryFailed retries every failed step, of one step when given, a batch at
// a time, as retryStep does; it says how many of how many items, and when
// there were none; one runs at a time; an unknown step is an error.
func TestRetryFailedRetriesEveryFailedStep(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "failed", `failures = 3`)
	put(t, st, "m2", "transcode", "failed", due)
	put(t, st, "e1", "transcode", "failed", "")
	put(t, st, "e1", "package", "failed", due)
	put(t, st, "m1", "package", "in_progress", "")
	put(t, st, "gone", "transcode", "failed", "")

	res, err := s.RetryFailed(context.Background(), "transcode")
	if err != nil || res.Retried != 3 || res.Items != 3 || res.NotSent != 0 || res.Step == nil || *res.Step != "transcode" ||
		res.Message != "retried 3 failed transcode steps of 3 items" {
		t.Fatalf("retryFailed(transcode): %+v, %v", res, err)
	}
	if got := len(b.take()); got != 3 {
		t.Errorf("%d events, want 3", got)
	}
	if got := state(t, st, "e1", "package"); !strings.HasPrefix(got, "failed 1") {
		t.Errorf("a failed package was retried with the transcodes: %s", got)
	}
	if res, err := s.RetryFailed(context.Background(), ""); err != nil || res.Retried != 1 || res.Message != "retried 1 failed steps of 1 items" {
		t.Errorf("retryFailed(): %+v, %v", res, err)
	}
	if res, err := s.RetryFailed(context.Background(), ""); err != nil || res.Retried != 0 || res.Message != "no failed steps to retry" {
		t.Errorf("retryFailed() with none left: %+v, %v", res, err)
	}
	if _, err := s.RetryFailed(context.Background(), "encode"); err == nil || !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("retryFailed(encode): %v", err)
	}
	s.retryingFailed.Store(true)
	if _, err := s.RetryFailed(context.Background(), ""); !errors.Is(err, errBusy) {
		t.Errorf("a second retryFailed at once: %v", err)
	}
	s.retryingFailed.Store(false)

	// more than a batch; one item's events cannot be sent, and the run stops
	// there, the rest failed as they were
	many := storetest.Open(t)
	for i := 0; i < 2*batch+50; i++ {
		id := fmt.Sprintf("m%03d", i)
		storetest.AddItem(t, many, id, "movie", "Film "+id, "")
		put(t, many, id, "package", "failed", "")
	}
	b2 := &bus{}
	if res, err := newService(t, many, b2).RetryFailed(context.Background(), ""); err != nil || res.Retried != 2*batch+50 || len(b2.take()) != 2*batch+50 {
		t.Errorf("retryFailed of %d: %+v, %v", 2*batch+50, res, err)
	}
	// m000's step comes first, so the first batch holds it
	storetest.Exec(t, many, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed'`)
	storetest.Exec(t, many, `UPDATE com_nalet_katalog_itemprocessingsteps SET id = '00000000-0000-0000-0000-000000000000' WHERE item_id = 'm000'`)
	b3 := &bus{refuse: map[string]bool{"m000": true}}
	res, err = newService(t, many, b3).RetryFailed(context.Background(), "package")
	if err != nil || res.NotSent != 1 || res.Retried != batch-1 || !strings.Contains(res.Message, "1 could not be sent") {
		t.Errorf("retryFailed with an event refused: %+v, %v", res, err)
	}
	if n := storetest.Count(t, many, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE status = 'failed'`); n != 2*batch+50-(batch-1) {
		t.Errorf("%d steps failed after the run stopped, want the refused one and the batches not run", n)
	}
}

// The overview counts every step's items by state, the failed the service
// retries and the silent ones past their timeout, lists the failed steps,
// the latest failure first, with their items, and says how it retries.
func TestTheOverview(t *testing.T) {
	st := catalog(t)
	s := newService(t, st, &bus{})
	put(t, st, "m1", "transcode", "failed", `failures = 2, lasterror = 'out of memory', error = 'out of memory',
		finishedat = localtimestamp - interval '1 hour', nextretryat = now() + interval '2 minutes'`)
	put(t, st, "e1", "transcode", "failed", `failures = 3, error = 'no such file', finishedat = localtimestamp - interval '5 minutes'`)
	put(t, st, "m2", "transcode", "in_progress", `modifiedat = localtimestamp - interval '2 hours'`)
	put(t, st, "s1", "tmdb", "done", "")
	put(t, st, "m1", "package", "pending", `dispatchedat = now() - interval '3 hours'`)
	put(t, st, "m2", "package", "pending", "")
	put(t, st, "e1", "subtitle", "skipped", "")
	put(t, st, "e1", "chapter", "not_applicable", "")
	put(t, st, "e1", "rescan", "failed", `finishedat = localtimestamp - interval '1 day'`)

	o, err := s.Overview(context.Background(), "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, c := range o.Steps {
		steps = append(steps, fmt.Sprintf("%s %d/%d/%d/%d/%d/%d r%d s%d %d", c.Step, c.Pending, c.InProgress, c.Done, c.Failed,
			c.Skipped, c.NotApplicable, c.Retrying, c.Stalled, c.TimeoutSeconds))
	}
	want := []string{"scan 0/0/0/0/0/0 r0 s0 7200", "tmdb 0/0/1/0/0/0 r0 s0 7200", "tidb 0/0/0/0/0/0 r0 s0 7200",
		"chapter 0/0/0/0/0/1 r0 s0 7200", "chromaprint 0/0/0/0/0/0 r0 s0 7200", "blackframe 0/0/0/0/0/0 r0 s0 7200",
		"silence 0/0/0/0/0/0 r0 s0 7200", "subtitle 0/0/0/0/1/0 r0 s0 7200", "transcode 0/1/0/2/0/0 r1 s1 3600",
		"package 2/0/0/0/0/0 r0 s1 7200", "rescan 0/0/0/1/0/0 r0 s0 7200"}
	if strings.Join(steps, "\n") != strings.Join(want, "\n") {
		t.Errorf("the counts:\n%s\nwant:\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	if o.FailedTotal != 3 || len(o.Failed) != 3 {
		t.Fatalf("failed: %d of %d, want 3", len(o.Failed), o.FailedTotal)
	}
	f := o.Failed[0]
	if f.ItemID != "e1" || f.ItemTitle != "Earthfall" || f.ItemType != "episode" || f.SeriesID == nil || *f.SeriesID != "s1" ||
		*f.SeriesTitle != "Pioneer One" || *f.SeasonNumber != 1 || *f.EpisodeNumber != 2 || f.Step != "transcode" ||
		f.Failures != 3 || *f.LastError != "no such file" || f.NextRetryAt != nil || f.FailedAt == nil {
		t.Errorf("the latest failure: %+v", f)
	}
	if f := o.Failed[1]; f.ItemID != "m1" || f.SeriesID != nil || f.Failures != 2 || *f.LastError != "out of memory" || f.NextRetryAt == nil {
		t.Errorf("the next: %+v", f)
	}
	if o.Failed[2].Step != "rescan" {
		t.Errorf("the oldest: %+v", o.Failed[2])
	}
	r := o.Retry
	if !r.Available || !r.Automatic || r.Reason != nil || r.MaxAttempts != 3 || r.BackoffSeconds != 60 || r.BackoffMaxSeconds != 3600 || r.IntervalSeconds != 30 {
		t.Errorf("the retry policy: %+v", r)
	}

	// one step, a page of it
	o, err = s.Overview(context.Background(), "transcode", 1, 1)
	if err != nil || o.FailedTotal != 2 || len(o.Failed) != 1 || o.Failed[0].ItemID != "m1" {
		t.Errorf("the failed transcodes, the second: %+v, %v", o.Failed, err)
	}
	if _, err := s.Overview(context.Background(), "encode", 0, 0); err == nil {
		t.Error("the overview of an unknown step")
	}
	if o, _ := New(st, testPolicy, &bus{}, 0).Overview(context.Background(), "", 0, 0); o.Retry.Automatic || !o.Retry.Available {
		t.Errorf("no sweep: %+v", o.Retry)
	}
	if p, err := s.Policy(context.Background()); err != nil || p != r {
		t.Errorf("the policy alone: %+v, %v; want the overview's %+v", p, err, r)
	}
	one := testPolicy
	one.MaxAttempts = 1
	if p, _ := New(st, one, &bus{}, time.Second).Policy(context.Background()); p.Automatic || !p.Available || p.MaxAttempts != 1 {
		t.Errorf("one attempt: %+v, want no automatic retries", p)
	}
}

func TestLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{6 * time.Hour: "6h", 90 * time.Minute: "1h30m", 15 * time.Minute: "15m",
		90 * time.Second: "1m30s", 45 * time.Second: "45s", 2*time.Hour + 30*time.Second: "2h0m30s"} {
		if got := Label(d); got != want {
			t.Errorf("Label(%v) = %q, want %q", d, got, want)
		}
	}
}
