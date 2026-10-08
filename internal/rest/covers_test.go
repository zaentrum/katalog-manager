package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A disc image is no file to take in: POST /api/ingest refuses one, saying
// what to do, before anything else, letter case aside.
func TestADiscImageIsNoFileToIngest(t *testing.T) {
	h := &Handlers{}
	for _, path := range []string{"/var/lib/katalog/media/Film (2001).iso", "/var/lib/katalog/media/Film (2001).IMG"} {
		raw, _ := json.Marshal(map[string]any{"path": path, "type": "movie", "title": "Film"})
		w := httptest.NewRecorder()
		h.ingest(w, httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(raw)))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), processing.DiscImageReason) {
			t.Errorf("%s: %d %s, want 400 saying it is a disc image", path, w.Code, w.Body.String())
		}
	}
}

// POST /api/items/{id}/package of an episode another's file covers packages
// its holder, saying so; of a title whose file is a disc image it is 409,
// saying what to do. The analyzer's reset of a series leaves a covered
// episode alone: it runs no pass.
func TestTheRoutesOfACoveredEpisodeAndADiscImage(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "e2", "episode", "Pilot, Part Two", "s1")
	storetest.AddItem(t, st, "m1", "movie", "On A Disc", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', 'e1', '/m/Pilot.S01E01E02.mkv', true), ('a2', 'm1', '/m/On A Disc.iso', true)`)
	if _, err := library.Link(ctx, st.Pool(), "e1", "e2"); err != nil {
		t.Fatal(err)
	}
	h, iss := server(t, st, testConfig(t.TempDir()))
	admin := iss.Admin(t)
	answer := func(method, path, token string) (int, map[string]any) {
		t.Helper()
		w := do(h, method, path, "", token)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %d %s is no JSON", path, w.Code, w.Body.String())
		}
		return w.Code, body
	}
	if code, body := answer(http.MethodPost, "/api/items/e2/package", admin); code != http.StatusOK || body["status"] != "pending" ||
		!strings.HasPrefix(body["message"].(string), library.CoveredNote("e2", "e1")) {
		t.Errorf("package the covered episode: %d %v", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'e1'
		AND step = 'transcode' AND status = 'pending'`); n != 1 {
		t.Error("the holder's transcode is not enqueued")
	}
	if code, body := answer(http.MethodPost, "/api/items/m1/package", admin); code != http.StatusConflict ||
		!strings.Contains(body["error"].(string), processing.DiscImageReason) {
		t.Errorf("package a disc image: %d %v", code, body)
	}
	worker := iss.Service(t, "zaentrum-manager")
	if code, body := answer(http.MethodPost, "/api/analyze/series/s1/reset", worker); code != http.StatusOK || body["episodes"] != 1.0 {
		t.Errorf("reset the series: %d %v, want its one episode with a file of its own", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'e2'
		AND step = ANY($1) AND status = 'not_applicable'`, []string{"chapter", "chromaprint", "blackframe", "silence", "subtitle", "tidb"}); n != 6 {
		t.Errorf("the reset touched the covered episode: %d of its analyzer's steps still do not apply", n)
	}
}
