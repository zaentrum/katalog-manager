package rest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A worker reports the rules it runs with: kept, answered with when, and
// whether they changed since it reported last; a report of another worker,
// or one whose cap is no number, is 400 and keeps nothing.
func TestAWorkerReportsItsRules(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, testConfig(t.TempDir()))
	service := iss.Service(t, "zaentrum-manager")
	body := `{"version": "3f2a9c1", "rules": {"surroundAudio": "eac3", "surroundBitrate": "448k", "stereoBitrate": "192k",
		"imageSubtitles": ["pgs"]}}`
	var got struct {
		Worker, Version, ReportedAt string
		Changed                     bool
	}
	for i, changed := range []bool{true, false} {
		w := do(h, http.MethodPut, "/api/analyze/rules/packager", body, service)
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("report %d: %d %s", i, w.Code, w.Body.String())
		}
		if _, err := time.Parse(time.RFC3339Nano, got.ReportedAt); err != nil || got.Worker != "packager" ||
			got.Version != "3f2a9c1" || got.Changed != changed {
			t.Errorf("report %d: %+v, want changed %v", i, got, changed)
		}
	}
	for _, tc := range []struct{ path, body, says string }{
		{"/api/analyze/rules/analyzer", `{"rules": {}}`, "no such worker"},
		{"/api/analyze/rules/transcoder", `{"rules": {"caps": {"movie2160Mbps": "fourteen"}}}`,
			"caps.movie2160Mbps is a JSON string, and it is a number"},
	} {
		if w := do(h, http.MethodPut, tc.path, tc.body, service); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("PUT %s %s: %d %s, want 400 saying %q", tc.path, tc.body, w.Code, w.Body.String(), tc.says)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_workerrules`); n != 1 {
		t.Errorf("%d workers' rules kept, want the packager's", n)
	}
}

// A transcode, a package or a take-in that ends has the compliance check look
// at its title again on its next pass; a step that runs does not.
func TestAStepThatEndsHasItsTitleCheckedAgain(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, testConfig(t.TempDir()))
	service := iss.Service(t, "zaentrum-manager")
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a1', 'm1', '/media/m1.mkv', true)`)
	marked := func() int {
		return storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE item_id = 'm1' AND staleat IS NOT NULL`)
	}
	if w := do(h, http.MethodPut, "/api/analyze/items/m1/steps/transcode", `{"status": "in_progress"}`, service); w.Code != http.StatusOK {
		t.Fatalf("in progress: %d %s", w.Code, w.Body.String())
	}
	if n := marked(); n != 0 {
		t.Errorf("a transcode that runs marked its title")
	}
	if w := do(h, http.MethodPut, "/api/analyze/items/m1/steps/transcode", `{"status": "failed", "error": "ffmpeg exited 1"}`, service); w.Code != http.StatusOK {
		t.Fatalf("failed: %d %s", w.Code, w.Body.String())
	}
	if n := marked(); n != 1 {
		t.Errorf("a transcode that failed did not mark its title")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET staleat = NULL`)
	if w := do(h, http.MethodPut, "/api/analyze/items/m1/steps/package", `{"status": "done"}`, service); w.Code != http.StatusOK {
		t.Fatalf("done: %d %s", w.Code, w.Body.String())
	}
	if n := marked(); n != 1 {
		t.Errorf("a package that is done did not mark its title")
	}
}
