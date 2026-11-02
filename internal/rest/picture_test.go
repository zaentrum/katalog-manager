package rest

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The analyzer reports what it measured of a title, and reads it back with
// whether it is of what the title plays now; a report that can't be taken is
// 400 and says why, a title of none 404. Its backfill asks for titles to
// measure and is told why there are none while the backfill is off.
func TestTheAnalyzerReportsAndReadsAPictureArea(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	h, iss := server(t, st, testConfig(dir))
	service := iss.Service(t, "zaentrum-manager")
	media := filepath.Join(dir, "media", "m1.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('a1', 'm1', $1, true, 'primary', 'src1')`, media)

	if w := do(h, http.MethodGet, "/api/picture/items/m1", "", service); w.Code != http.StatusNotFound {
		t.Errorf("before a measurement: %d %s, want 404", w.Code, w.Body.String())
	}
	report := `{"path": "` + media + `", "method": "picture/1", "frame": {"width": 1920, "height": 1080},
		"area": {"x": 0, "y": 138, "width": 1920, "height": 804}, "samples": 300, "informative": 288,
		"durationMs": 6000000, "seconds": 41.5, "cpuSeconds": 60.2,
		"sides": {"top": {"bar": 138, "support": 280, "note": "bar"}, "black": 16}}`
	var got struct {
		State, SourceID string
		Current         bool
		Frame           struct{ Width, Height int }
		Area            struct{ X, Y, Width, Height int }
		Evidence        map[string]any
	}
	for _, method := range []string{http.MethodPut, http.MethodGet} {
		body := ""
		if method == http.MethodPut {
			body = report
		}
		w := do(h, method, "/api/picture/items/m1", body, service)
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
		if got.State != "measured" || !got.Current || got.SourceID != "src1" || got.Frame.Width != 1920 ||
			got.Area.Y != 138 || got.Area.Height != 804 || got.Evidence["black"] != 16.0 {
			t.Errorf("%s: %s", method, w.Body.String())
		}
	}
	for _, tc := range []struct {
		path, body string
		code       int
		says       string
	}{
		{"/api/picture/items/m1", `{"path": "/m.mkv", "frame": {"width": 1920, "height": 1080}, "area": {"x": 0, "y": 900, "width": 1920, "height": 804}}`,
			http.StatusBadRequest, "does not lie within the frame"},
		{"/api/picture/items/m1", `not json`, http.StatusBadRequest, "no report"},
		{"/api/picture/items/none", report, http.StatusNotFound, "no such title"},
	} {
		if w := do(h, http.MethodPut, tc.path, tc.body, service); w.Code != tc.code || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("PUT %s %s: %d %s, want %d saying %q", tc.path, tc.body, w.Code, w.Body.String(), tc.code, tc.says)
		}
	}
	w := do(h, http.MethodPost, "/api/picture/claim", `{"worker": "analyzer-0", "limit": 1}`, service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"items":[]`) ||
		!strings.Contains(w.Body.String(), `"idle":"picture.backfill is off"`) ||
		!strings.Contains(w.Body.String(), `"retryAfterSeconds":900`) {
		t.Errorf("a claim while it is off: %d %s", w.Code, w.Body.String())
	}
}
