package events

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// fakeWriter answers each write with the next of its answers (nil when they
// run out) and records what it was asked to write.
type fakeWriter struct {
	answers []func(n int) error
	writes  [][]kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.writes = append(f.writes, msgs)
	if len(f.answers) == 0 {
		return nil
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a(len(msgs))
}

func (f *fakeWriter) Close() error { return nil }

func init() { emitBackoff = time.Millisecond }

func msg(topic, item string) Message {
	ev := NewItemEvent(item)
	ev.Status = "retry"
	return Message{Topic: topic, Event: ev}
}

// Publish writes the events in one batch, each keyed by its item, and says
// nil of each written.
func TestPublishWritesTheEventsInOneBatch(t *testing.T) {
	w := &fakeWriter{}
	errs := (&Producer{w: w}).Publish(context.Background(), []Message{msg("t.enriched", "m1"), msg("t.analyzed", "m2")})
	if len(errs) != 2 || errs[0] != nil || errs[1] != nil {
		t.Fatalf("errs %v", errs)
	}
	if len(w.writes) != 1 || len(w.writes[0]) != 2 {
		t.Fatalf("writes %v, want one batch of two", w.writes)
	}
	m := w.writes[0][1]
	var ev ItemEvent
	if err := json.Unmarshal(m.Value, &ev); err != nil {
		t.Fatal(err)
	}
	if m.Topic != "t.analyzed" || string(m.Key) != "m2" || ev.ItemID != "m2" || ev.Status != "retry" || ev.EventID == "" {
		t.Errorf("the second message: %s %s %+v", m.Topic, m.Key, ev)
	}
}

// A message the batch fails is written again, alone; one that keeps
// failing is reported, the rest nil.
func TestPublishWritesAgainWhatFailed(t *testing.T) {
	refused := errors.New("not the leader")
	w := &fakeWriter{answers: []func(int) error{
		func(n int) error { we := make(kafka.WriteErrors, n); we[1] = refused; return we },
		func(n int) error { return kafka.WriteErrors{nil} },
	}}
	errs := (&Producer{w: w}).Publish(context.Background(), []Message{msg("t", "a"), msg("t", "b"), msg("t", "c")})
	if errs[0] != nil || errs[1] != nil || errs[2] != nil {
		t.Errorf("errs %v, want every message written", errs)
	}
	if len(w.writes) != 2 || len(w.writes[1]) != 1 || string(w.writes[1][0].Key) != "b" {
		t.Errorf("writes %v, want the failed message written again alone", w.writes)
	}

	down := errors.New("dial tcp: connection refused")
	always := func(int) error { return down }
	w = &fakeWriter{answers: []func(int) error{always, always, always, always, always, always, always}}
	errs = (&Producer{w: w}).Publish(context.Background(), []Message{msg("t", "a"), msg("t", "b")})
	if !errors.Is(errs[0], down) || !errors.Is(errs[1], down) || len(w.writes) != emitAttempts {
		t.Errorf("a broker down: errs %v after %d writes, want each failed after %d", errs, len(w.writes), emitAttempts)
	}
}

// Without brokers every event fails with ErrNoBus, and nothing is written.
func TestPublishWithoutABus(t *testing.T) {
	var p *Producer
	if p.Enabled() {
		t.Error("a nil producer is enabled")
	}
	errs := p.Publish(context.Background(), []Message{msg("t", "a")})
	if len(errs) != 1 || !errors.Is(errs[0], ErrNoBus) {
		t.Errorf("errs %v", errs)
	}
	if NewProducer(nil, nil).Enabled() {
		t.Error("a producer without brokers is enabled")
	}
}

// The extras' topics are the tenant's, beside the items'.
func TestConfigureNamesTheExtrasTopics(t *testing.T) {
	t.Cleanup(func() { Configure("stube.") })
	Configure("zaentrum-beta")
	if TopicExtraQueued != "zaentrum-beta.catalog.extra.queued" || TopicExtraTranscoded != "zaentrum-beta.catalog.extra.transcoded" ||
		TopicExtraPackaged != "zaentrum-beta.catalog.extra.packaged" || TopicPackaged != "zaentrum-beta.catalog.item.packaged" {
		t.Errorf("topics: %s %s %s %s", TopicExtraQueued, TopicExtraTranscoded, TopicExtraPackaged, TopicPackaged)
	}
}

// keys are the keys of a JSON object, sorted.
func keys(t *testing.T, raw []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// An extra's trigger is keyed by its extraId and names the extra, its title,
// its kind and the transcode, and no itemId, so that an item worker skips
// it; a batch of them is written as one.
func TestAnExtrasTriggerIsKeyedByItsExtraAndNamesNoItem(t *testing.T) {
	w := &fakeWriter{}
	ev := NewExtraEvent("1b5c2a8e-0000-4000-8000-000000000001", "ea886f9b-0d06-4f0f-babb-d2a1162f9b01", "trailer")
	ev.Source = "api"
	retry := NewExtraEvent("2b5c2a8e-0000-4000-8000-000000000002", "ea886f9b-0d06-4f0f-babb-d2a1162f9b01", "teaser")
	retry.Status, retry.Source = "retry", "retry"
	errs := ProducerOn(w).PublishExtras(context.Background(), []ExtraMessage{{Topic: "t.extra.queued", Event: ev},
		{Topic: "t.extra.queued", Event: retry}})
	if len(errs) != 2 || errs[0] != nil || errs[1] != nil || len(w.writes) != 1 || len(w.writes[0]) != 2 {
		t.Fatalf("errs %v, writes %v", errs, w.writes)
	}
	m := w.writes[0][0]
	if m.Topic != "t.extra.queued" || string(m.Key) != ev.ExtraID {
		t.Errorf("the message: %s %s", m.Topic, m.Key)
	}
	if got := keys(t, m.Value); got != "eventId extraId kind occurredAt parentId source status step type" {
		t.Errorf("the trigger's keys: %s", got)
	}
	var got ExtraEvent
	if err := json.Unmarshal(m.Value, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "extra" || got.Step != "transcode" || got.Status != "queued" || got.Source != "api" || got.Kind != "trailer" ||
		got.ParentID != "ea886f9b-0d06-4f0f-babb-d2a1162f9b01" || got.EventID == "" || got.OccurredAt == "" {
		t.Errorf("the trigger: %s", m.Value)
	}
	if !strings.Contains(string(w.writes[0][1].Value), `"status":"retry"`) || string(w.writes[0][1].Key) != retry.ExtraID {
		t.Errorf("the retry: %s %s", w.writes[0][1].Key, w.writes[0][1].Value)
	}
	if errs := (*Producer)(nil).PublishExtras(context.Background(), []ExtraMessage{{Topic: "t", Event: ev}}); !errors.Is(errs[0], ErrNoBus) {
		t.Errorf("without a bus: %v", errs)
	}
}

// An extra's package recorded is announced as an item event of its title,
// with the extra named beside it.
func TestTheExtraPackagedEventIsAnEventOfItsTitle(t *testing.T) {
	ev := ExtraPackagedEvent{ItemEvent: NewItemEvent("ea886f9b-0d06-4f0f-babb-d2a1162f9b01"), ExtraID: "1b5c", Kind: "trailer"}
	ev.Type, ev.Step, ev.Status, ev.Source = "movie", "extra", "done", "katalog-manager"
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(t, raw); got != "eventId extraId itemId kind occurredAt source status step type" {
		t.Errorf("keys: %s", got)
	}
	var item ItemEvent
	if err := json.Unmarshal(raw, &item); err != nil || item.ItemID != "ea886f9b-0d06-4f0f-babb-d2a1162f9b01" || item.Type != "movie" ||
		item.Step != "extra" || item.Status != "done" {
		t.Errorf("read as an item event: %+v, %v", item, err)
	}
}
