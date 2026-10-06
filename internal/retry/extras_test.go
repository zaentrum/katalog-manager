package retry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/extras"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// jobs stand in for the extras' jobs: they record each call, and fail the
// one named in failing.
type jobs struct {
	mu      sync.Mutex
	calls   []string
	failing string
}

func (j *jobs) call(name string) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls = append(j.calls, name)
	if j.failing == name {
		return 0, errors.New(name + " failed")
	}
	return 1, nil
}

func (j *jobs) Reap(context.Context) (int, error)                  { return j.call("reap") }
func (j *jobs) SendDue(context.Context) (int, error)               { return j.call("send") }
func (j *jobs) DeleteRemovedPackages(context.Context) (int, error) { return j.call("delete") }

func (j *jobs) taken() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return strings.Join(j.calls, " ")
}

// A sweep runs the extras' jobs, reaping first, then sending what is due,
// then deleting the removed packages; one that fails is said, and the others
// run. Without the jobs wired it does nothing.
func TestASweepRunsTheExtrasJobs(t *testing.T) {
	j := &jobs{failing: "send"}
	s := New(nil, testPolicy, &bus{}, time.Minute).WithExtras(j)
	reaped, sent, deleted, err := s.SweepExtras(context.Background())
	if err == nil || !strings.Contains(err.Error(), "send failed") || reaped != 1 || sent != 0 || deleted != 1 {
		t.Errorf("SweepExtras: %d %d %d %v", reaped, sent, deleted, err)
	}
	if got := j.taken(); got != "reap send delete" {
		t.Errorf("the jobs ran %q", got)
	}
	if r, se, d, err := New(nil, testPolicy, &bus{}, time.Minute).SweepExtras(context.Background()); r+se+d != 0 || err != nil {
		t.Errorf("without the jobs: %d %d %d %v", r, se, d, err)
	}
}

// The sweep runs the extras' jobs every interval also while the steps'
// retries idle (no migration 033 here, no event bus).
func TestRunSweepsTheExtrasWhileTheStepsIdle(t *testing.T) {
	st := storetest.OpenBase(t)
	j := &jobs{}
	s := New(st, testPolicy, &bus{off: true}, 10*time.Millisecond).WithExtras(j)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(j.taken(), "delete") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if n := strings.Count(j.taken(), "reap send delete"); n < 2 {
		t.Errorf("the extras' jobs ran %q, want them each sweep", j.taken())
	}
}

// writer records the Kafka messages the extras' producer writes.
type writer struct {
	mu   sync.Mutex
	msgs []string
}

func (w *writer) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range msgs {
		w.msgs = append(w.msgs, m.Topic+" "+string(m.Key)+" "+string(m.Value))
	}
	return nil
}

func (w *writer) Close() error { return nil }

// An extra stuck in its packaging, through the sweep as main wires it: the
// reaper puts it back, and once its backoff has passed the sweep sends its
// trigger again, a retry.
func TestTheSweepHealsAStuckExtra(t *testing.T) {
	events.Configure("stube.")
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, state,
		dispatchedat) VALUES ('1b5c2a8e-0000-4000-8000-000000000001', 'm1', 'trailer', 'Trailer', 'api', '/e/t.mov', 'queued',
		now() - interval '25 hours')`)
	w := &writer{}
	prod := events.ProducerOn(w)
	s := New(st, testPolicy, prod, time.Minute).WithExtras(extras.New(st, config.Config{PackagesRoot: t.TempDir()}, testPolicy, prod))
	if reaped, sent, _, err := s.SweepExtras(context.Background()); err != nil || reaped != 1 || sent != 0 {
		t.Fatalf("the first sweep: %d reaped, %d sent, %v", reaped, sent, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET nextretryat = now() - interval '1 second'`)
	if reaped, sent, _, err := s.SweepExtras(context.Background()); err != nil || reaped != 0 || sent != 1 {
		t.Fatalf("the second sweep: %d reaped, %d sent, %v", reaped, sent, err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.msgs) != 1 || !strings.HasPrefix(w.msgs[0], "stube.catalog.extra.queued 1b5c2a8e-0000-4000-8000-000000000001 ") ||
		!strings.Contains(w.msgs[0], `"status":"retry"`) || !strings.Contains(w.msgs[0], `"parentId":"m1"`) {
		t.Errorf("sent: %v", w.msgs)
	}
}

// libraryJobs stand in for the library's jobs: they count their sweeps.
type libraryJobs struct {
	mu   sync.Mutex
	runs int
	ran  chan struct{}
}

func (l *libraryJobs) Sweep(context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs++
	if l.runs == 1 {
		close(l.ran)
	}
	return "", nil
}

// With no sweep (KATALOG_RETRY_INTERVAL off), the library's jobs still run,
// once a minute by themselves: the first at once. They need no event bus and
// no step table.
func TestTheLibrarysJobsRunWithoutASweep(t *testing.T) {
	l := &libraryJobs{ran: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(nil, testPolicy, nil, 0).WithLibrary(l).Run(ctx)
	}()
	select {
	case <-l.ran:
	case <-time.After(10 * time.Second):
		t.Error("the library's jobs did not run")
	}
	cancel()
	<-done
}

// A sweep's round runs the library's jobs, with no event bus too.
func TestASweepRunsTheLibrarysJobs(t *testing.T) {
	st := storetest.Open(t)
	l := &libraryJobs{ran: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(st, testPolicy, nil, time.Hour).WithLibrary(l).Run(ctx)
	}()
	select {
	case <-l.ran:
	case <-time.After(10 * time.Second):
		t.Error("the sweep did not run the library's jobs")
	}
	cancel()
	<-done
}
