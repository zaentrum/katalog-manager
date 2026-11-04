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

// The analyzer writes a title's chapters as it always did, and with what its
// read of the original was: the catalog keeps the read, answers it, and says
// whether the title misses its chapters. A read that found none settles the
// title; one that failed leaves its chapters as they are. A title longer than
// its column is cut and a NUL dropped, not refused; chapters or a read that
// can't be taken are 400, a title of none 404. The backfill says why it hands
// out nothing while it is off.
func TestTheAnalyzerReportsAReadOfAnOriginalsChapters(t *testing.T) {
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
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ('src1', 'm1', 'm1.mkv', 6, 'present')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('a1', 'm1', $1, true, 'primary', 'src1')`, media)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, ruleshash, records)
		VALUES ('m1', 'movie', 'h', '{"srcEssence": {"chapters": true}}')`)

	type answer struct {
		Written  int
		Chapters []struct {
			StartMs, EndMs int64
			Title          *string
			Ordinal        int
		}
		Read *struct {
			State, SourceID, Probe, Reason string
			Found, Written, Failures       int
		}
		Missing bool
	}
	get := func(t *testing.T) answer {
		t.Helper()
		w := do(h, http.MethodGet, "/api/chapters/items/m1", "", service)
		var a answer
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &a) != nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body.String())
		}
		return a
	}
	if a := get(t); !a.Missing || a.Read != nil || len(a.Chapters) != 0 {
		t.Fatalf("before a read: %+v", a)
	}

	// as the analyzer always wrote them: the chapters, no read; a title
	// longer than its column cut, a NUL dropped
	long := strings.Repeat("x", 130)
	w := do(h, http.MethodPut, "/api/chapters/items/m1",
		`{"chapters": [{"startMs": 0, "endMs": 1000, "title": "`+long+`"}, {"startMs": 1000, "endMs": 2000, "title": "Act\u00001"}]}`,
		service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"written":2`) {
		t.Fatalf("a PUT without a read: %d %s", w.Code, w.Body.String())
	}
	if a := get(t); a.Missing || a.Read != nil || len(a.Chapters) != 2 || len(*a.Chapters[0].Title) != 120 ||
		*a.Chapters[1].Title != "Act1" || a.Chapters[1].Ordinal != 2 {
		t.Fatalf("after a PUT without a read: %+v", a)
	}

	// a read that found none: the chapters go, the title is settled
	w = do(h, http.MethodPut, "/api/chapters/items/m1",
		`{"chapters": [], "read": {"path": "`+media+`", "found": 0, "probe": "ffprobe version 4.4.2", "seconds": 0.3}}`, service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"none"`) {
		t.Fatalf("a read that found none: %d %s", w.Code, w.Body.String())
	}
	if a := get(t); a.Missing || a.Read == nil || a.Read.State != "none" || a.Read.SourceID != "src1" ||
		a.Read.Probe != "ffprobe version 4.4.2" || len(a.Chapters) != 0 {
		t.Fatalf("after a read that found none: %+v", a)
	}

	// a read that found three
	w = do(h, http.MethodPut, "/api/chapters/items/m1", `{"chapters": [{"startMs": 0, "endMs": 5000, "title": "Intro", "ordinal": 1},
		{"startMs": 5000, "endMs": 9000, "ordinal": 2}, {"startMs": 9000, "endMs": 12000, "title": " ", "ordinal": 3}],
		"read": {"path": "`+media+`", "found": 4, "note": "1 mark without a length left out"}}`, service)
	if w.Code != http.StatusOK {
		t.Fatalf("a read that found three: %d %s", w.Code, w.Body.String())
	}
	a := get(t)
	if a.Missing || a.Read.State != "read" || a.Read.Found != 4 || a.Read.Written != 3 ||
		a.Read.Reason != "1 mark without a length left out" || len(a.Chapters) != 3 || a.Chapters[2].Title != nil {
		t.Fatalf("after a read that found three: %+v", a)
	}

	// one that failed: the chapters stay
	w = do(h, http.MethodPut, "/api/chapters/items/m1",
		`{"read": {"path": "`+media+`", "error": "ffprobe did not answer within 120 s"}}`, service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state":"read"`) {
		t.Fatalf("a read that failed: %d %s", w.Code, w.Body.String())
	}
	if a := get(t); len(a.Chapters) != 3 || a.Read.Failures != 0 {
		t.Errorf("after a read that failed: %+v", a)
	}

	for _, tc := range []struct {
		path, body string
		code       int
		says       string
	}{
		{"/api/chapters/items/m1", `{"chapters": [{"startMs": 5, "endMs": 5}]}`, http.StatusBadRequest, "startMs/endMs invalid"},
		{"/api/chapters/items/m1", `{"chapters": [{"startMs": 5, "endMs": 5}], "read": {"path": "/m.mkv"}}`,
			http.StatusBadRequest, "startMs/endMs invalid"},
		{"/api/chapters/items/m1", `{"read": {"path": "/m.mkv"}}`, http.StatusBadRequest, "chapters, [] for none"},
		{"/api/chapters/items/m1", `{"chapters": [], "read": {"path": "/m.mkv", "error": "x"}}`, http.StatusBadRequest,
			"found no chapters"},
		{"/api/chapters/items/m1", `{"chapters": []`, http.StatusBadRequest, "missing 'chapters' array"},
		{"/api/chapters/items/m1", `{"read": "yes"}`, http.StatusBadRequest, "no report of a read"},
		{"/api/chapters/items/m1", `{}`, http.StatusBadRequest, "missing 'chapters' array"},
		{"/api/chapters/items/none", `{"chapters": []}`, http.StatusNotFound, "unknown item"},
	} {
		if w := do(h, http.MethodPut, tc.path, tc.body, service); w.Code != tc.code || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("PUT %s %s: %d %s, want %d saying %q", tc.path, tc.body, w.Code, w.Body.String(), tc.code, tc.says)
		}
	}
	if w := do(h, http.MethodGet, "/api/chapters/items/none", "", service); w.Code != http.StatusNotFound {
		t.Errorf("GET of no such title: %d %s", w.Code, w.Body.String())
	}
	w = do(h, http.MethodPost, "/api/chapters/claim", `{"worker": "analyzer-0", "limit": 1}`, service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"items":[]`) ||
		!strings.Contains(w.Body.String(), `"idle":"chapters.backfill is off"`) ||
		!strings.Contains(w.Body.String(), `"retryAfterSeconds":900`) {
		t.Errorf("a claim while it is off: %d %s", w.Code, w.Body.String())
	}
}
