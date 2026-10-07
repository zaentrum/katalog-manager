package rest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// POST /api/library/reencode queues the titles its body names and answers
// what it did, {queued, alreadyQueued, skipped}; GET answers the queue's
// counts and its oldest and newest title, and why the sweep sends none (here
// no event bus). A body that names nothing, or is no request, is refused;
// without migration 043 both answer 503.
func TestTheReencodeRoutes(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', 'm1', '/media/m1.mkv', true), ('a2', 'm2', '/media/m2.mkv', true)`)
	h, iss := server(t, st, testConfig(t.TempDir()))
	service := iss.Service(t, "zaentrum-manager")

	w := do(h, http.MethodPost, "/api/library/reencode", `{"items": ["m1", "nobody", "s1", "m1"]}`, service)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"queued":1,"alreadyQueued":0,"skipped":[`+
		`{"itemId":"nobody","reason":"unknown item"},{"itemId":"s1","reason":"the series has no episode to encode"}]}` {
		t.Fatalf("POST items: %d %s", w.Code, w.Body.String())
	}
	w = do(h, http.MethodPost, "/api/library/reencode", `{"items": ["m2", "m1"]}`, service)
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"queued":1,"alreadyQueued":1,"skipped":[]}` {
		t.Errorf("POST again: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue
		WHERE enqueuedby = 'service-1' AND state = 'queued'`); n != 2 {
		t.Errorf("%d titles queued by the service account's subject, want 2", n)
	}

	w = do(h, http.MethodGet, "/api/library/reencode", "", service)
	var q struct {
		Queued, Sent, Done, Failed int
		Oldest, Newest             *struct{ ItemID, State, EnqueuedAt string }
		Idle                       *string
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &q) != nil {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	if q.Queued != 2 || q.Sent != 0 || q.Done != 0 || q.Failed != 0 || q.Oldest == nil || q.Oldest.ItemID != "m1" ||
		q.Oldest.State != "queued" || !strings.HasSuffix(q.Oldest.EnqueuedAt, "Z") || q.Newest == nil || q.Newest.ItemID != "m2" ||
		q.Idle == nil || !strings.HasPrefix(*q.Idle, "no event bus: ") {
		t.Errorf("GET: %s", w.Body.String())
	}

	for _, body := range []string{"", `{}`, `{"items": [], "held": false}`, `["m1"]`, `{"all": "yes"}`} {
		if w := do(h, http.MethodPost, "/api/library/reencode", body, service); w.Code != http.StatusBadRequest ||
			!strings.Contains(w.Body.String(), `name the titles: {\"items\": [`) ||
			!strings.Contains(w.Body.String(), `]}, {\"held\": true} or {\"all\": true}`) {
			t.Errorf("POST %q: %d %s, want 400 naming what it takes", body, w.Code, w.Body.String())
		}
	}

	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_reencodequeue`)
	h, iss = server(t, st, testConfig(t.TempDir()))
	service = iss.Service(t, "zaentrum-manager")
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		w := do(h, method, "/api/library/reencode", `{"all": true}`, service)
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "migration 043") {
			t.Errorf("%s without the queue: %d %s, want 503", method, w.Code, w.Body.String())
		}
	}
}
