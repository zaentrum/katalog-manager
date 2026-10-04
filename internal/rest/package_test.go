package rest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// POST /api/items/{id}/package answers as the CAP service's enqueuePackaging
// did, which chino-api's admin route passes on: a movie's status, whether its
// chain was active and a message; a series' episodes enqueued of all; 404
// for an unknown item and 400 for one that cannot be packaged.
func TestThePackagingActionAnswersAsChinoAPIExpects(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "Sintel", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "a1", "album", "An Album", "")
	for _, id := range []string{"m1", "e1"} {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES (gen_random_uuid()::varchar, $1::varchar, '/m/' || $1::varchar, true)`, id)
	}
	h, iss := server(t, st, testConfig(t.TempDir()))
	admin := iss.Admin(t)
	answer := func(path string) (int, map[string]any) {
		t.Helper()
		w := do(h, http.MethodPost, path, "", admin)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %d %s is no JSON", path, w.Code, w.Body.String())
		}
		return w.Code, body
	}
	code, body := answer("/api/items/m1/package")
	if code != http.StatusOK || body["status"] != "pending" || body["alreadyActive"] != false ||
		!strings.HasPrefix(body["message"].(string), "Queued for transcoding.") {
		t.Errorf("a movie: %d %v", code, body)
	}
	if code, body := answer("/api/items/m1/package"); code != http.StatusOK || body["status"] != "transcode pending" || body["alreadyActive"] != true {
		t.Errorf("the movie again: %d %v", code, body)
	}
	if code, body := answer("/api/items/s1/package"); code != http.StatusOK || body["episodesEnqueued"] != 1.0 || body["episodesTotal"] != 1.0 {
		t.Errorf("a series: %d %v", code, body)
	}
	if code, body := answer("/api/items/nope/package"); code != http.StatusNotFound || body["error"] != "unknown item: nope" {
		t.Errorf("an unknown item: %d %v", code, body)
	}
	if code, body := answer("/api/items/a1/package"); code != http.StatusBadRequest || body["error"] != "Packaging is only available for movies and episodes." {
		t.Errorf("an album: %d %v", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE step = 'transcode' AND status = 'pending'`); n != 2 {
		t.Errorf("%d transcodes enqueued, want the movie's and the episode's", n)
	}
}
