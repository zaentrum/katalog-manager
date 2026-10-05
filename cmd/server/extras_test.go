package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	kafka "github.com/segmentio/kafka-go"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/extras"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// kafkaLog stands in for the Kafka writer of the service's producer: it
// keeps every message as "topic key value".
type kafkaLog struct {
	mu   sync.Mutex
	msgs []string
}

func (k *kafkaLog) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, m := range msgs {
		k.msgs = append(k.msgs, m.Topic+" "+string(m.Key)+" "+string(m.Value))
	}
	return nil
}

func (k *kafkaLog) Close() error { return nil }

func (k *kafkaLog) take() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.msgs
	k.msgs = nil
	return out
}

// post sends a body to a route with token, as a Job or a worker does.
func (in *instance) post(t *testing.T, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, in.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

// An extra through the service as main wires it, the extras' service and
// the producer real, with the bearer tokens of a realm: an admin takes a
// trailer in by the film's file (its trigger goes, keyed by the extra, no
// item named); the workers read its record and report its transcode and
// package, and packaging-complete makes it ready, announced as an event of
// the film and never as the film packaged; the admin reads it playable,
// packages it again (it leaves ready, waits, and its trigger goes again,
// its package playing meanwhile), and removes it: the workers no longer
// find it. A Job takes another file in with the service account on the
// REST route.
func TestAnExtraThroughTheService(t *testing.T) {
	events.Configure("stube.")
	k := &kafkaLog{}
	prod := events.ProducerOn(k)
	in := newInstanceWired(t, wiring{events: prod, extras: func(st *store.Store, cfg config.Config) *extras.Service {
		return extras.New(st, cfg, processing.DefaultPolicy(), prod)
	}})
	admin, svc := in.iss.Admin(t), in.iss.Service(t, "zaentrum-manager")
	trailer := filepath.Join(in.cfg.ExtrasRoot, "a-film", "trailer.mov")
	if err := os.MkdirAll(filepath.Dir(trailer), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trailer, []byte("a trailer"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, a := in.gql(t, "/api/manage/query", admin, `mutation { addExtra(itemPath: "/media/a-film.mkv", path: "`+trailer+
		`", kind: "trailer", language: "en") { created extra { id itemId kind title state sourceSize sourceQh1 playable } } }`)
	var added struct {
		AddExtra struct {
			Created bool
			Extra   struct {
				ID, ItemID, Kind, Title, State, SourceQh1 string
				SourceSize                                float64
				Playable                                  bool
			}
		}
	}
	if err := json.Unmarshal(a.Data, &added); err != nil || len(a.Errors) > 0 {
		t.Fatalf("addExtra: %s %v", a.Data, a.Errors)
	}
	x := added.AddExtra.Extra
	if !added.AddExtra.Created || x.ItemID != "m1" || x.Kind != "trailer" || x.Title != "Trailer" || x.State != "queued" ||
		x.SourceSize != 9 || !strings.HasPrefix(x.SourceQh1, "sha256:") || x.Playable {
		t.Fatalf("addExtra: %s", a.Data)
	}
	sent := k.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "stube.catalog.extra.queued "+x.ID+" ") ||
		!strings.Contains(sent[0], `"parentId":"m1"`) || strings.Contains(sent[0], "itemId") {
		t.Fatalf("the trigger: %v", sent)
	}

	if code, body := in.get(t, "/api/analyze/extras/"+x.ID, svc); code != http.StatusOK ||
		!strings.Contains(body, `"parentType":"movie","parentTitle":"A Film"`) || !strings.Contains(body, `"path":"`+trailer+`"`) {
		t.Fatalf("the record: %d %s", code, body)
	}
	for _, step := range [][2]string{{"transcode", "in_progress"}, {"transcode", "done"}, {"package", "in_progress"}} {
		if code := in.put(t, "/api/analyze/extras/"+x.ID+"/steps/"+step[0], svc, `{"status": "`+step[1]+`"}`); code != http.StatusOK {
			t.Fatalf("%s %s: %d", step[0], step[1], code)
		}
	}
	manifest := `{"version": 2, "itemId": "` + x.ID + `", "type": "extra", "parentId": "m1", "extraKind": "trailer", "durationMs": 33000,
		"renditions": {"video": [{"id": "v0", "codec": "avc1.64001f", "width": 1280, "height": 720, "bitrateBps": 2000000}]}}`
	if code, body := in.post(t, "/api/extras/"+x.ID+"/packaging-complete", svc, manifest); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %s", code, body)
	}
	if code := in.put(t, "/api/analyze/extras/"+x.ID+"/steps/package", svc, `{"status": "done"}`); code != http.StatusOK {
		t.Fatalf("the package's end: %d", code)
	}
	sent = k.take()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "stube.catalog.extra.packaged "+x.ID+" ") ||
		!strings.Contains(sent[0], `"itemId":"m1","type":"movie","step":"extra","status":"done"`) {
		t.Fatalf("the announcement: %v", sent)
	}

	extrasOfTheFilm := `{ item(id: "m1") { extras { id state playable durationMs videoCodec height packagePath } } }`
	_, a = in.gql(t, "/api/manage/query", admin, extrasOfTheFilm)
	if want := `{"item":{"extras":[{"id":"` + x.ID + `","state":"ready","playable":true,"durationMs":33000,` +
		`"videoCodec":"avc1.64001f","height":720,"packagePath":"` + filepath.Join(in.cfg.PackagesRoot, "extras", x.ID[:2], x.ID) +
		`"}]}}`; string(a.Data) != want || len(a.Errors) > 0 {
		t.Errorf("the film's extras:\n got  %s %v\n want %s", a.Data, a.Errors, want)
	}

	_, a = in.gql(t, "/api/manage/query", admin, `mutation { packageExtra(id: "`+x.ID+`") { queued busy message extras { state playable } } }`)
	if want := `{"packageExtra":{"queued":1,"busy":0,"message":"packaging it again (stube.catalog.extra.queued sent); ` +
		`the package it has plays until the new one is in place","extras":[{"state":"queued","playable":true}]}}`; string(a.Data) != want {
		t.Errorf("packageExtra:\n got  %s %v\n want %s", a.Data, a.Errors, want)
	}
	if sent := k.take(); len(sent) != 1 || !strings.Contains(sent[0], `"status":"queued"`) || !strings.Contains(sent[0], `"source":"reencode"`) {
		t.Errorf("the re-encode's trigger: %v", sent)
	}

	_, a = in.gql(t, "/api/manage/query", admin, `mutation { removeExtra(id: "`+x.ID+`", reason: "a duplicate") { removedBy removalReason playable } }`)
	if !strings.Contains(string(a.Data), `"removalReason":"a duplicate","playable":false`) || len(a.Errors) > 0 {
		t.Errorf("removeExtra: %s %v", a.Data, a.Errors)
	}
	if code, _ := in.get(t, "/api/analyze/extras/"+x.ID, svc); code != http.StatusNotFound {
		t.Errorf("a removed extra's record: %d", code)
	}

	teaser := filepath.Join(in.cfg.ExtrasRoot, "a-film", "teaser.webm")
	if err := os.WriteFile(teaser, []byte("a teaser"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := in.post(t, "/api/extras", svc, `{"itemPath": "/media/a-film.mkv", "path": "`+teaser+`", "kind": "teaser"}`); code != http.StatusCreated ||
		!strings.Contains(body, `"created":true`) {
		t.Errorf("the Job's POST /api/extras: %d %s", code, body)
	}
	if code, _ := in.post(t, "/api/extras", in.iss.Viewer(t), `{"itemPath": "/media/a-film.mkv", "path": "`+teaser+`", "kind": "teaser"}`); code != http.StatusForbidden {
		t.Errorf("a viewer's POST /api/extras: %d", code)
	}
}
