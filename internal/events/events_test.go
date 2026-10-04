package events

import (
	"context"
	"encoding/json"
	"errors"
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
