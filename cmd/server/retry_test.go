package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// recordingBus stands in for a deployment's event bus: it takes every event,
// and records it as "topic item".
type recordingBus struct {
	mu   sync.Mutex
	sent []string
}

func (b *recordingBus) Enabled() bool { return true }

func (b *recordingBus) Publish(_ context.Context, msgs []events.Message) []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range msgs {
		b.sent = append(b.sent, m.Topic+" "+m.Event.ItemID)
	}
	return make([]error, len(msgs))
}

func (b *recordingBus) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := strings.Join(b.sent, "; ")
	b.sent = nil
	return s
}

// put sends a worker's report of a step to the REST route, as the workers do.
func (in *instance) put(t *testing.T, path, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, in.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	in.record(string(raw))
	return resp.StatusCode
}

// The retries through the service as main wires it, with the real retries,
// the bearer tokens of a realm and a bus that takes every event: a viewer and
// the service account are refused the overview and both retries before
// anything is read, changed or sent; an admin reads the overview, retries a
// failed step (its trigger sent, the step waiting for its worker again) and
// is told why a running one is left alone, and retries every failed step;
// the workers' reports through the REST route answer the retry, and a
// failure's credential never reaches an answer.
func TestRetriesThroughTheService(t *testing.T) {
	b := &recordingBus{}
	in := newInstanceWith(t, func(st *store.Store) graph.Pipeline {
		return retry.New(st, processing.DefaultPolicy(), b, time.Minute)
	})
	st := in.st
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, failures, error, lasterror) VALUES
		('st-m2-p', now(), now(), 'm2', 'package', 'in_progress', 0, NULL, NULL),
		('st-m2-s', now(), now(), 'm2', 'subtitle', 'failed', 3, 'whisper crashed', 'whisper crashed')`)
	viewer, service, admin := in.iss.Viewer(t), in.iss.Service(t, "zaentrum-manager"), in.iss.Admin(t)

	docs := []string{
		`{ processingOverview { failedTotal } }`,
		`mutation { retryStep(itemId: "m1", step: "transcode") { retried } }`,
		`mutation { retryFailed { retried } }`,
	}
	before := fingerprint(t, st)
	for _, token := range []string{viewer, service} {
		for _, doc := range docs {
			_, a := in.gql(t, "/api/manage/query", token, doc)
			if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
				t.Errorf("%s: %v, want it refused", doc, a.Errors)
			}
		}
	}
	if sent := b.take(); sent != "" {
		t.Errorf("refused callers sent %s", sent)
	}
	if after := fingerprint(t, st); after != before {
		t.Errorf("refused callers changed the steps:\nbefore %s\nafter  %s", before, after)
	}

	gql := func(doc string) string {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) > 0 {
			t.Fatalf("%s: %v", doc, a.Errors)
		}
		return string(a.Data)
	}
	if got := gql(`{ processingOverview(step: "transcode") { failedTotal failed { itemId itemTitle step failures lastError }
		retry { available automatic maxAttempts } } }`); got != `{"processingOverview":{"failedTotal":1,"failed":[{"itemId":"m1",`+
		`"itemTitle":"A Film","step":"transcode","failures":1,"lastError":"ffmpeg exited 1"}],"retry":{"available":true,"automatic":true,"maxAttempts":3}}}` {
		t.Errorf("the overview: %s", got)
	}
	if got := gql(`mutation { retryStep(itemId: "m1", step: "transcode") { itemId step retried status } }`); got !=
		`{"retryStep":{"itemId":"m1","step":"transcode","retried":true,"status":"pending"}}` {
		t.Errorf("an admin's retry: %s", got)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" m1" {
		t.Errorf("the retry sent %q, want the transcoder's trigger", sent)
	}
	var steps struct {
		Item struct {
			ProcessingSteps []struct {
				Step, Status              string
				Failures                  int
				LastError                 *string
				DispatchedAt, NextRetryAt *string
			}
		}
	}
	read := func() {
		t.Helper()
		if err := json.Unmarshal([]byte(gql(`{ item(id: "m1") { processingSteps { step status failures lastError dispatchedAt nextRetryAt } } }`)), &steps); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if s := steps.Item.ProcessingSteps; len(s) != 1 || s[0].Status != "pending" || s[0].Failures != 0 || s[0].LastError == nil ||
		*s[0].LastError != "ffmpeg exited 1" || s[0].DispatchedAt == nil || s[0].NextRetryAt != nil {
		t.Errorf("the step after the retry: %+v", s)
	}
	if got := gql(`mutation { retryStep(itemId: "m2", step: "package") { retried status message } }`); !strings.Contains(got,
		`"retried":false,"status":"in_progress","message":"package is running`) {
		t.Errorf("a retry of a running step: %s", got)
	}
	if got := gql(`mutation { retryFailed { retried items notSent message } }`); got !=
		`{"retryFailed":{"retried":1,"items":1,"notSent":0,"message":"retried 1 failed steps of 1 items"}}` {
		t.Errorf("retryFailed: %s", got)
	}
	if sent := b.take(); sent != events.TopicEnriched+" m2" {
		t.Errorf("retryFailed sent %q, want the analyzer's trigger for m2", sent)
	}

	// The transcoder starts the retried step and fails it, quoting a URL
	// with credentials: the answer redacts them and schedules a retry.
	if code := in.put(t, "/api/analyze/items/m1/steps/transcode", service, `{"status": "in_progress"}`); code != http.StatusOK {
		t.Fatalf("the worker's start: %d", code)
	}
	read()
	if s := steps.Item.ProcessingSteps[0]; s.Status != "in_progress" || s.DispatchedAt != nil {
		t.Errorf("the step its worker started: %+v", s)
	}
	if code := in.put(t, "/api/analyze/items/m1/steps/transcode", service,
		`{"status": "failed", "error": "GET http://km:hunter2@nas/media?token=abcdefghijklmnop: 403"}`); code != http.StatusOK {
		t.Fatalf("the worker's failure: %d", code)
	}
	read()
	if s := steps.Item.ProcessingSteps[0]; s.Status != "failed" || s.Failures != 1 || s.NextRetryAt == nil || s.LastError == nil ||
		*s.LastError != "GET http://REDACTED@nas/media?token=REDACTED 403" {
		t.Errorf("the step its worker failed: %+v, last error %q", s, deref(s.LastError))
	}
	for _, a := range in.answers {
		if strings.Contains(a, "hunter2") || strings.Contains(a, "abcdefghijklmnop") {
			t.Errorf("an answer carries the failure's credential: %s", a)
		}
	}
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
