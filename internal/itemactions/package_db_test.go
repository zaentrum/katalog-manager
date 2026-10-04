package itemactions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// fakeBus records the events it is asked to send, and refuses those of the
// items in refuse; off is no bus at all.
type fakeBus struct {
	mu     sync.Mutex
	off    bool
	refuse map[string]bool
	sent   []string
}

func (b *fakeBus) EmitItem(context.Context, string, events.ItemEvent) {}
func (b *fakeBus) Enabled() bool                                      { return !b.off }
func (b *fakeBus) Publish(_ context.Context, msgs []events.Message) []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		if b.off {
			errs[i] = events.ErrNoBus
			continue
		}
		if b.refuse[m.Event.ItemID] {
			errs[i] = errors.New("broker: not the leader")
			continue
		}
		b.sent = append(b.sent, m.Topic+" "+m.Event.ItemID+" "+m.Event.Type+" "+m.Event.Step+" "+m.Event.Source)
	}
	return errs
}

func (b *fakeBus) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	sort.Strings(b.sent)
	s := strings.Join(b.sent, "; ")
	b.sent = nil
	return s
}

func transcode(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT status || ' sent=' || (dispatchedat IS NOT NULL) ||
		' retry=' || (nextretryat IS NOT NULL) || ' ' || COALESCE(error, '-')
		FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = 'transcode'`, item).Scan(&out); err != nil {
		return "none"
	}
	return out
}

// packageItem tells the transcoder of the item it enqueues: the event the
// transcoder consumes (analyzed), the dispatch noted. An item whose chain is
// active is left alone and nothing is sent; a series sends one event per
// episode it enqueues.
func TestPackageItemTellsTheTranscoder(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "Sintel", "")
	storetest.AddItem(t, st, "m2", "movie", "Tears of Steel", "")
	storetest.AddItem(t, st, series, "series", "A Series", "")
	storetest.AddItem(t, st, episode1, "episode", "Pilot", series)
	storetest.AddItem(t, st, episode2, "episode", "Second", series)
	for _, id := range []string{"m1", "m2", episode1, episode2} {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES (gen_random_uuid()::varchar, $1::varchar, '/media/' || $1::varchar, true)`, id)
	}
	steps := processing.New(st.Pool())
	if err := steps.Upsert(context.Background(), "m2", "package", processing.StatusDone, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := steps.Upsert(context.Background(), episode2, "transcode", processing.StatusInProgress, nil, nil); err != nil {
		t.Fatal(err)
	}
	b := &fakeBus{}
	svc := New(st, config.Config{}, steps, nil)
	svc.events = b
	ctx := context.Background()

	res, err := svc.PackageItem(ctx, "m1")
	if err != nil || *res.Status != "pending" || *res.AlreadyActive || strings.Contains(*res.Message, "could not") {
		t.Fatalf("package m1: %+v, %v", res, err)
	}
	if got := b.take(); got != events.TopicAnalyzed+" m1 movie transcode package" {
		t.Errorf("sent %q, want the transcoder's trigger for m1", got)
	}
	if got := transcode(t, st, "m1"); got != "pending sent=true retry=false -" {
		t.Errorf("m1's transcode: %s", got)
	}
	// active: nothing again
	if res, err := svc.PackageItem(ctx, "m1"); err != nil || !*res.AlreadyActive || b.take() != "" {
		t.Errorf("package m1 again: %+v, %v", res, err)
	}
	if res, err := svc.PackageItem(ctx, "m2"); err != nil || !*res.AlreadyActive || b.take() != "" {
		t.Errorf("package m2, packaged: %+v, %v", res, err)
	}
	// a series: the episode not active
	res, err = svc.PackageItem(ctx, series)
	if err != nil || *res.EpisodesEnqueued != 1 || *res.EpisodesTotal != 2 {
		t.Fatalf("package the series: %+v, %v", res, err)
	}
	if got := b.take(); got != events.TopicAnalyzed+" "+episode1+" episode transcode package" {
		t.Errorf("the series sent %q", got)
	}
	if _, err := svc.PackageItem(ctx, "nope"); !errors.Is(err, ErrUnknownItem) {
		t.Errorf("an unknown item: %v", err)
	}
}

// A transcode whose event could not be sent is failed with a retry
// scheduled, and the result says so; without a bus the transcode waits, and
// the result says nothing tells the transcoder.
func TestPackageItemWhoseEventCouldNotBeSent(t *testing.T) {
	st := storetest.Open(t)
	for _, id := range []string{"m1", "m2"} {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
	}
	steps := processing.New(st.Pool())
	b := &fakeBus{refuse: map[string]bool{"m1": true}}
	svc := New(st, config.Config{}, steps, nil)
	svc.events = b
	res, err := svc.PackageItem(context.Background(), "m1")
	if err != nil || *res.Status != "failed" || !strings.Contains(*res.Message, "could not be sent (broker: not the leader): failed, and retried later") {
		t.Errorf("package m1, its event refused: %+v, %v", res, err)
	}
	if got := transcode(t, st, "m1"); got != "failed sent=false retry=true packaging could not start: the transcoder's event could not be sent: broker: not the leader" {
		t.Errorf("m1's transcode: %s", got)
	}

	svc.events = &fakeBus{off: true}
	res, err = svc.PackageItem(context.Background(), "m2")
	if err != nil || *res.Status != "pending" || !strings.Contains(*res.Message, "no event bus") {
		t.Errorf("package m2 without a bus: %+v, %v", res, err)
	}
	if got := transcode(t, st, "m2"); got != "pending sent=false retry=false -" {
		t.Errorf("m2's transcode: %s", got)
	}
}
