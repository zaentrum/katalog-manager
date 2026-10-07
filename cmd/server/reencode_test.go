package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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

// markingBus takes every event, and records it as "topic item step status
// source type": whether it is marked as a retry shows.
type markingBus struct {
	mu   sync.Mutex
	sent []string
}

func (b *markingBus) Enabled() bool { return true }

func (b *markingBus) Publish(_ context.Context, msgs []events.Message) []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range msgs {
		e := m.Event
		b.sent = append(b.sent, strings.Join([]string{m.Topic, e.ItemID, e.Step, e.Status, e.Source, e.Type}, " "))
	}
	return make([]error, len(msgs))
}

func (b *markingBus) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	sort.Strings(b.sent)
	s := strings.Join(b.sent, "; ")
	b.sent = nil
	return s
}

// get reads a route with token, as a worker does.
func (in *instance) get(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, in.url+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	in.record(string(raw))
	return resp.StatusCode, string(raw)
}

// Encoding a title again through the service as main wires it, with the real
// retries, the bearer tokens of a realm and a bus that takes every event: a
// viewer, an addon and the service account are refused before anything is
// read, changed or sent. An admin encodes a packaged film again: its
// transcode and package wait for their workers, the transcoder's trigger is
// sent (not as a retry), the steps the workers' guards read say pending, and
// the packaged asset stays until the packager replaces it; the workers'
// reports through the REST route then count one run of each, a heartbeat
// none. A film whose transcode runs is left alone, saying why; a series'
// episodes are encoded again; an unknown item is an error.
func TestReencodeThroughTheService(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWith(t, func(st *store.Store) graph.Pipeline {
		return retry.New(st, processing.DefaultPolicy(), b, time.Minute)
	})
	st := in.st
	storetest.AddItem(t, st, "f1", "movie", "A Packaged Film", "")
	storetest.AddItem(t, st, "f2", "movie", "A Film Being Encoded", "")
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "e2", "episode", "Second", "s1")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec) VALUES
		('a-f1', 'f1', '/media/f1.mkv', true, 'primary', 'h264'),
		('a-f1-p', 'f1', '/packages/movies/f1/f1/manifest.json', false, 'packaged', 'hvc1.1.6.L120.B0'),
		('a-f2', 'f2', '/media/f2.mkv', true, 'primary', 'h264'),
		('a-e1', 'e1', '/media/e1.mkv', true, 'primary', 'hevc'),
		('a-e2', 'e2', '/media/e2.mkv', true, 'primary', 'hevc')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts, failures, lasterror) VALUES
		('st-f1-t', now(), now(), 'f1', 'transcode', 'done', 1, 0, 'out of memory'),
		('st-f1-p', now(), now(), 'f1', 'package', 'done', 1, 0, NULL),
		('st-f2-t', now(), now(), 'f2', 'transcode', 'in_progress', 1, 0, NULL),
		('st-e1-t', now(), now(), 'e1', 'transcode', 'not_applicable', 1, 0, NULL),
		('st-e1-p', now(), now(), 'e1', 'package', 'done', 1, 0, NULL)`)
	viewer, addon, service, admin := in.iss.Viewer(t), in.iss.Addon(t), in.iss.Service(t, "zaentrum-manager"), in.iss.Admin(t)

	const doc = `mutation { reencodeItem(id: "f1") { itemId titles reencoded busy notSent message } }`
	before := fingerprint(t, st)
	for _, token := range []string{viewer, addon, service} {
		_, a := in.gql(t, "/api/manage/query", token, doc)
		if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" ||
			a.Errors[0].Message != "forbidden: reencodeItem requires the zaentrum-admin role" {
			t.Errorf("%s: %v, want it refused", doc, a.Errors)
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
	if got := gql(doc); got != `{"reencodeItem":{"itemId":"f1","titles":1,"reencoded":1,"busy":0,"notSent":0,"message":`+
		`"encoding it again: its transcode and package wait for their workers (`+events.TopicAnalyzed+` sent); `+
		`the current package plays until the packager starts on the new one"}}` {
		t.Errorf("an admin's re-encode: %s", got)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" f1 transcode reencode reencode movie" {
		t.Errorf("the re-encode sent %q, want the transcoder's trigger, not marked as a retry", sent)
	}
	var item struct {
		Item struct {
			ProcessingSteps []struct {
				Step, Status string
				Attempts     int
				Failures     int
				LastError    *string
				DispatchedAt *string
			}
		}
	}
	read := func() string {
		t.Helper()
		if err := json.Unmarshal([]byte(gql(`{ item(id: "f1") { processingSteps { step status attempts failures lastError dispatchedAt } } }`)), &item); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range item.Item.ProcessingSteps {
			out = append(out, fmt.Sprintf("%s %s attempts=%d failures=%d last=%s sent=%v",
				s.Step, s.Status, s.Attempts, s.Failures, deref(s.LastError), s.DispatchedAt != nil))
		}
		return strings.Join(out, "; ")
	}
	if got := read(); got != "transcode pending attempts=1 failures=0 last=out of memory sent=true; package pending attempts=1 failures=0 last= sent=false" {
		t.Errorf("the film's steps after its re-encode: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE item_id = 'f1' AND kind = 'packaged'`); n != 1 {
		t.Errorf("the film has %d packaged assets, want its package kept until the packager replaces it", n)
	}
	// what the transcoder's and the packager's guards read: both waiting,
	// so each runs its step
	if code, body := in.get(t, "/api/analyze/items/f1/steps", service); code != http.StatusOK ||
		body != `{"itemId":"f1","steps":{"package":"pending","transcode":"pending"}}`+"\n" {
		t.Errorf("the steps a worker reads: %d %q", code, body)
	}

	// The transcoder runs it, saying so twice, and passes it on; the
	// packager runs the package: one run each.
	for _, r := range []struct{ path, body string }{
		{"/api/analyze/items/f1/steps/transcode", `{"status": "in_progress"}`},
		{"/api/analyze/items/f1/steps/transcode", `{"status": "in_progress"}`},
		{"/api/analyze/items/f1/steps/transcode", `{"status": "done", "details": "profile=hevc-1080p ladder=v0:hevc_nvenc:1920x1080,v1:h264_nvenc:1280x720"}`},
		{"/api/analyze/items/f1/steps/package", `{"status": "in_progress"}`},
		{"/api/analyze/items/f1/steps/package", `{"status": "done"}`},
	} {
		if code := in.put(t, r.path, service, r.body); code != http.StatusOK {
			t.Fatalf("PUT %s %s: %d", r.path, r.body, code)
		}
	}
	if got := read(); got != "transcode done attempts=2 failures=0 last=out of memory sent=false; package done attempts=2 failures=0 last= sent=false" {
		t.Errorf("the film's steps once the workers ran them: %s", got)
	}

	if got := gql(`mutation { reencodeItem(id: "f2") { titles reencoded busy message } }`); !strings.HasPrefix(got,
		`{"reencodeItem":{"titles":1,"reencoded":0,"busy":1,"message":"transcode is running: its worker last reported at `) {
		t.Errorf("a re-encode of a film whose transcode runs: %s", got)
	}
	if got := gql(`mutation { reencodeItem(id: "s1") { titles reencoded busy notSent } }`); got !=
		`{"reencodeItem":{"titles":2,"reencoded":2,"busy":0,"notSent":0}}` {
		t.Errorf("a series' re-encode: %s", got)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" e1 transcode reencode reencode episode; "+
		events.TopicAnalyzed+" e2 transcode reencode reencode episode" {
		t.Errorf("the series' re-encode sent %q", sent)
	}
	if _, a := in.gql(t, "/api/manage/query", admin, `mutation { reencodeItem(id: "nope") { titles } }`); len(a.Errors) != 1 ||
		a.Errors[0].Message != "unknown item: nope" {
		t.Errorf("an unknown item: %v", a.Errors)
	}
}

// A title encoded again is packaged with what the catalog knows of its
// tracks: once reencodeItem has sent the transcoder's trigger, the record the
// packager reads for the title (its worker record, read by the id on the
// transcoder's event) names the languages an admin set and the subtitle file
// beside its source.
func TestATitleEncodedAgainIsPackagedWithItsLanguagesAndFiles(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWith(t, func(st *store.Store) graph.Pipeline {
		return retry.New(st, processing.DefaultPolicy(), b, time.Minute)
	})
	st := in.st
	dir := t.TempDir()
	source, sidecar := filepath.Join(dir, "A Film.mkv"), filepath.Join(dir, "A Film.en.srt")
	for _, f := range []string{source, sidecar} {
		if err := os.WriteFile(f, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, "f9", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec)
		VALUES ('a-f9', 'f9', $1, true, 'primary', 'h264')`, source)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label)
		VALUES ('s-f9', 'f9', $1, 'srt', 'en', 'English')`, sidecar)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts, failures) VALUES
		('st-f9-t', now(), now(), 'f9', 'transcode', 'done', 1, 0), ('st-f9-p', now(), now(), 'f9', 'package', 'done', 1, 0)`)
	admin, service := in.iss.Admin(t), in.iss.Service(t, "zaentrum-manager")
	if _, a := in.gql(t, "/api/manage/query", admin,
		`mutation { setTrackLanguage(itemId: "f9", kind: "audio", ordinal: 0, language: "zxx") { id } }`); len(a.Errors) > 0 {
		t.Fatal(a.Errors)
	}
	if _, a := in.gql(t, "/api/manage/query", admin, `mutation { reencodeItem(id: "f9") { reencoded } }`); len(a.Errors) > 0 ||
		string(a.Data) != `{"reencodeItem":{"reencoded":1}}` {
		t.Fatalf("reencodeItem: %s %v", a.Data, a.Errors)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" f9 transcode reencode reencode movie" {
		t.Errorf("the re-encode sent %q", sent)
	}
	code, body := in.get(t, "/api/analyze/items/f9", service)
	if code != http.StatusOK ||
		!strings.Contains(body, `"trackLanguages":[{"kind":"audio","ordinal":0,"language":"zxx"}]`) ||
		!strings.Contains(body, `"subtitleFiles":[{"id":"s-f9","path":"`+sidecar+`","language":"eng","label":"English","forced":false}]`) {
		t.Errorf("the record the packager reads: %d %s", code, body)
	}
}

// The re-encode queue through the service as main wires it: the service
// account queues a film (POST /api/library/reencode) and reads the queue
// (GET), the console's overview counts it, the sweep sends it to the
// transcoder, and an admin, and only an admin, clears it.
func TestTheReencodeQueueThroughTheService(t *testing.T) {
	b := &markingBus{}
	var svc *retry.Service
	in := newInstanceWith(t, func(st *store.Store) graph.Pipeline {
		svc = retry.New(st, processing.DefaultPolicy(), b, time.Minute)
		return svc
	})
	service, admin := in.iss.Service(t, "zaentrum-manager"), in.iss.Admin(t)
	if code, body := in.post(t, "/api/library/reencode", service, `{"items": ["m1", "p1"]}`); code != http.StatusOK ||
		strings.TrimSpace(body) != `{"queued":1,"alreadyQueued":0,"skipped":[{"itemId":"p1","reason":"unknown item"}]}` {
		t.Fatalf("POST: %d %s", code, body)
	}
	const overview = `{ processingOverview { reencodeQueue { queued sent done failed idle oldest { itemId state } newest { itemId } } } }`
	_, a := in.gql(t, "/api/manage/query", admin, overview)
	if len(a.Errors) > 0 || string(a.Data) != `{"processingOverview":{"reencodeQueue":{"queued":1,"sent":0,"done":0,"failed":0,"idle":null,`+
		`"oldest":{"itemId":"m1","state":"queued"},"newest":{"itemId":"m1"}}}}` {
		t.Errorf("the overview: %s %v", a.Data, a.Errors)
	}
	p, err := svc.DrainQueue(context.Background())
	if err != nil || p.Sent != 1 {
		t.Fatalf("the sweep's pass: %+v, %v", p, err)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" m1 transcode reencode reencode movie" {
		t.Errorf("sent %q, want the transcoder's trigger", sent)
	}
	if code, body := in.get(t, "/api/library/reencode", service); code != http.StatusOK ||
		!strings.HasPrefix(body, `{"queued":0,"sent":1,"done":0,"failed":0,"oldest":{"itemId":"m1","state":"sent","enqueuedAt":"`) {
		t.Errorf("GET: %d %s", code, body)
	}
	const clear = `mutation { clearReencodeQueue(states: ["sent"]) }`
	if _, a := in.gql(t, "/api/manage/query", service, clear); len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
		t.Errorf("the service account's clear: %v, want it refused", a.Errors)
	}
	if _, a := in.gql(t, "/api/manage/query", admin, clear); len(a.Errors) > 0 || string(a.Data) != `{"clearReencodeQueue":1}` {
		t.Errorf("an admin's clear: %s %v", a.Data, a.Errors)
	}
	if n := storetest.Count(t, in.st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`); n != 0 {
		t.Errorf("%d titles queued after the clear", n)
	}
}
