package rest

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// sent records the events the retries send.
type sent struct {
	mu   sync.Mutex
	msgs []events.Message
}

func (s *sent) Enabled() bool { return true }

func (s *sent) Publish(_ context.Context, msgs []events.Message) []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msgs...)
	return make([]error, len(msgs))
}

func (s *sent) take() []events.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.msgs
	s.msgs = nil
	return out
}

// A transcode the transcoder refused, reported as its step's end, has the
// title taken in at once with the v2 layout: its take-in sent to the packager
// (transcoded, its step takein), whose worker record then takes the title
// in, its original named original.mkv. So has a package that failed with no
// attempt left; one with an attempt left is retried as before. With the
// legacy layout nothing is taken in.
func TestATitleThatGetsNoPackageIsTakenIn(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := v2Config(dir)
	bus := &sent{}
	h, iss := serverWith(t, st, cfg, bus)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/A Film (2024).mkv": 100, ".work/incoming/Another Film (2024).m2ts": 100})
	const other = "f2f2f2f2-0000-4000-8000-000000000002"
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	storetest.AddItem(t, st, other, "movie", "Another Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('src-f1', $1, $2, true), ('src-f2', $3, $4, true)`, filmItem, cfg.ArrivalsRoot+"/A Film (2024).mkv", other,
		cfg.ArrivalsRoot+"/Another Film (2024).m2ts")
	report := func(item, step, body string) {
		t.Helper()
		if w := do(h, http.MethodPut, "/api/analyze/items/"+item+"/steps/"+step, body, svc); w.Code != http.StatusOK {
			t.Fatalf("PUT %s %s: %d %s", item, step, w.Code, w.Body.String())
		}
	}
	refused := `{"status": "failed", "error": "Dolby Vision profile 5 needs a tone-mapping encode; kept the original"}`
	report(filmItem, "transcode", refused)
	if got := bus.take(); len(got) != 0 {
		t.Errorf("taken in with the legacy layout: %v", got)
	}
	v2Layout(t, st)
	// The workers read the record first, which gives each title its source.
	recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	recordOf(t, h, svc, "/api/analyze/items/"+other)
	report(filmItem, "transcode", refused)
	got := bus.take()
	if len(got) != 1 || got[0].Topic != events.TopicTranscoded || got[0].Event.ItemID != filmItem || got[0].Event.Step != "takein" {
		t.Fatalf("the refused title's take-in: %+v", got)
	}
	lib, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
	if b, _ := lib["build"].(map[string]any); b["mode"] != "takein" || b["originalName"] != "original.mkv" {
		t.Errorf("the worker record of a title taken in: %v", b)
	}

	report(other, "transcode", `{"status": "done"}`)
	report(other, "package", `{"status": "failed", "error": "no space left"}`)
	if got := bus.take(); len(got) != 0 {
		t.Errorf("a package with attempts left taken in: %v", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'in_progress', failures = 2
		WHERE item_id = $1 AND step = 'package'`, other)
	report(other, "package", `{"status": "failed", "error": "no space left"}`)
	if got := bus.take(); len(got) != 1 || got[0].Event.ItemID != other || got[0].Event.Step != "takein" {
		t.Errorf("the exhausted package's take-in: %+v", got)
	}
	lib, _ = recordOf(t, h, svc, "/api/analyze/items/"+other)["library"].(map[string]any)
	if b, _ := lib["build"].(map[string]any); b["mode"] != "takein" || b["originalName"] != "original.m2ts" {
		t.Errorf("the worker record of the exhausted title: %v", b)
	}
}
