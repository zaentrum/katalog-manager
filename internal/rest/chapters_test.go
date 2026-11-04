package rest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The movie and its original the chapter tests read, ids as the library
// names folders by.
const (
	chapterFilm   = "a0000001-0000-4000-8000-000000000001"
	chapterSource = "b0000001-0000-4000-8000-000000000001"
)

type chapterAnswer struct {
	Written  int
	Chapters []struct {
		StartMs, EndMs int64
		Title          *string
		Ordinal        int
	}
	Read *struct {
		State, ReadFrom, SourceID, Probe, Reason string
		RowsSourceID                             *string
		Replaced                                 json.RawMessage
		Confirmed                                bool
		Found, Written, Failures, Transients     int
	}
	Missing bool
}

// chapterFilmOf makes the movie with its original on disk, the compliance
// check's record that says it has chapters, and returns the original's path.
func chapterFilmOf(t *testing.T, st *store.Store, cfg config.Config) string {
	t.Helper()
	media := filepath.Join(cfg.NFSRoot, "film.mkv")
	if err := os.MkdirAll(filepath.Dir(media), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, st, chapterFilm, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ($1, $2, 'film.mkv', 6, 'present')`, chapterSource, chapterFilm)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('a1', $1, $2, true, 'primary', $3)`, chapterFilm, media, chapterSource)
	return media
}

func getChapters(t *testing.T, h http.Handler, token string) chapterAnswer {
	t.Helper()
	w := do(h, http.MethodGet, "/api/chapters/items/"+chapterFilm, "", token)
	var a chapterAnswer
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &a) != nil {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	return a
}

// The analyzer writes a title's chapters as it always did, and with what its
// read of the header was: the catalog keeps the read, answers it, and says
// whether the title misses its chapters. Chapters written without a read are
// of no known original, and settle nothing. A read that found chapters
// settles the title; one that found none never deletes them, and where the
// original's source record holds chapters they are read from it; one that
// failed leaves them as they are. A title longer than its column is cut and a
// NUL dropped, not refused; chapters or a read that can't be taken are 400, a
// title of none 404. The backfill says why it hands out nothing while it is
// off.
func TestTheAnalyzerReportsAReadOfAnOriginalsChapters(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.LibraryRoot = dir
	h, iss := server(t, st, cfg)
	service := iss.Service(t, "zaentrum-manager")
	media := chapterFilmOf(t, st, cfg)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, ruleshash, recordkey, records)
		VALUES ($1, 'movie', 'h', $2, '{"srcEssence": {"chapters": true}}')`, chapterFilm, "records/2 s:"+chapterSource+"@1 ")
	put := func(body string) string {
		t.Helper()
		w := do(h, http.MethodPut, "/api/chapters/items/"+chapterFilm, body, service)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", body, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if a := getChapters(t, h, service); !a.Missing || a.Read != nil || len(a.Chapters) != 0 {
		t.Fatalf("before a read: %+v", a)
	}

	// as the analyzer always wrote them: the chapters, no read; a title
	// longer than its column cut, a NUL dropped. Of no known original: the
	// title misses its chapters still
	long := strings.Repeat("x", 130)
	if b := put(`{"chapters": [{"startMs": 0, "endMs": 1000, "title": "` + long + `"},
		{"startMs": 1000, "endMs": 2000, "title": "Act\u00001"}]}`); !strings.Contains(b, `"written":2`) {
		t.Fatalf("a PUT without a read: %s", b)
	}
	if a := getChapters(t, h, service); !a.Missing || a.Read != nil || len(a.Chapters) != 2 || len(*a.Chapters[0].Title) != 120 ||
		*a.Chapters[1].Title != "Act1" || a.Chapters[1].Ordinal != 2 {
		t.Fatalf("after a PUT without a read: %+v", a)
	}

	// a read of the header that found three: settled
	put(`{"chapters": [{"startMs": 0, "endMs": 5000, "title": "Intro", "ordinal": 1},
		{"startMs": 5000, "endMs": 9000, "ordinal": 2}, {"startMs": 9000, "endMs": 12000, "title": " ", "ordinal": 3}],
		"read": {"path": "` + media + `", "found": 4, "note": "1 mark without a length left out", "probe": "ffprobe version 4.4.2"}}`)
	a := getChapters(t, h, service)
	if a.Missing || a.Read.State != "read" || a.Read.ReadFrom != "header" || a.Read.SourceID != chapterSource ||
		a.Read.Found != 4 || a.Read.Written != 3 || len(a.Chapters) != 3 || a.Chapters[2].Title != nil {
		t.Fatalf("after a read that found three: %+v", a)
	}
	// one that found none, and one that failed: the chapters stay
	put(`{"chapters": [], "read": {"path": "` + media + `", "found": 0}}`)
	put(`{"read": {"path": "` + media + `", "error": "ffprobe did not answer within 120 s", "transient": true}}`)
	if a := getChapters(t, h, service); a.Missing || len(a.Chapters) != 3 || a.Read.State != "read" || a.Read.Failures != 0 {
		t.Errorf("after a read that found none, and one that failed: %+v", a)
	}

	// the chapters gone, a read that finds none: its record holds two
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, chapterFilm)
	rec := filepath.Join(library.SourceDir(library.PathsOf(cfg).MovieDir(chapterFilm), chapterSource), "ffprobe.json")
	if err := os.MkdirAll(filepath.Dir(rec), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := []byte(`{"chapters": [{"start_time": "0.000000", "end_time": "60.000000", "tags": {"title": "One"}},
		{"start_time": "60.000000", "end_time": "120.000000", "tags": {"title": "Two"}}], "format": {"duration": "120.0"}}`)
	sum := sha256.Sum256(probe)
	if err := os.WriteFile(rec, probe, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(rec), "source.json"), []byte(`{"file": {"sizeBytes": 6}, `+
		`"probe": {"sha256": "sha256:`+hex.EncodeToString(sum[:])+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	put(`{"chapters": [], "read": {"path": "` + media + `", "found": 0}}`)
	if a := getChapters(t, h, service); a.Missing || len(a.Chapters) != 2 || a.Read.ReadFrom != "record" ||
		!strings.Contains(a.Read.Reason, "its source record holds 2") {
		t.Errorf("an empty read whose record holds chapters: %+v", a)
	}

	for _, tc := range []struct {
		path, body string
		code       int
		says       string
	}{
		{"/api/chapters/items/" + chapterFilm, `{"chapters": [{"startMs": 5, "endMs": 5}]}`, http.StatusBadRequest,
			"startMs/endMs invalid"},
		{"/api/chapters/items/" + chapterFilm, `{"chapters": [{"startMs": 5, "endMs": 5}], "read": {"path": "/m.mkv"}}`,
			http.StatusBadRequest, "startMs/endMs invalid"},
		{"/api/chapters/items/" + chapterFilm, `{"read": {"path": "/m.mkv"}}`, http.StatusBadRequest, "chapters, [] for none"},
		{"/api/chapters/items/" + chapterFilm, `{"chapters": [], "read": {"path": "/m.mkv", "error": "x"}}`,
			http.StatusBadRequest, "found no chapters"},
		{"/api/chapters/items/" + chapterFilm, `{"chapters": []`, http.StatusBadRequest, "missing 'chapters' array"},
		{"/api/chapters/items/" + chapterFilm, `{"read": "yes"}`, http.StatusBadRequest, "no report of a read"},
		{"/api/chapters/items/" + chapterFilm, `{}`, http.StatusBadRequest, "missing 'chapters' array"},
		{"/api/chapters/items/none", `{"chapters": []}`, http.StatusNotFound, "unknown item"},
	} {
		if w := do(h, http.MethodPut, tc.path, tc.body, service); w.Code != tc.code || !strings.Contains(w.Body.String(), tc.says) {
			t.Errorf("PUT %s %s: %d %s, want %d saying %q", tc.path, tc.body, w.Code, w.Body.String(), tc.code, tc.says)
		}
	}
	if w := do(h, http.MethodGet, "/api/chapters/items/none", "", service); w.Code != http.StatusNotFound {
		t.Errorf("GET of no such title: %d %s", w.Code, w.Body.String())
	}
	w := do(h, http.MethodPost, "/api/chapters/claim", `{"worker": "analyzer-0", "limit": 1}`, service)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"items":[]`) ||
		!strings.Contains(w.Body.String(), `"idle":"chapters.backfill is off"`) ||
		!strings.Contains(w.Body.String(), `"retryAfterSeconds":900`) {
		t.Errorf("a claim while it is off: %d %s", w.Code, w.Body.String())
	}
}

// Chapters written by a hand that keeps no read of the original — a PUT
// without a read, a DELETE — are of no original known: a title a read
// settled misses its chapters again, until a read of its original agrees; the
// read's chapters then replace them, kept to undo, and the read says which
// original the chapters in the catalog were read of.
func TestChaptersWrittenWithoutAReadAreOfNoOriginal(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.LibraryRoot = dir
	h, iss := server(t, st, cfg)
	service := iss.Service(t, "zaentrum-manager")
	media := chapterFilmOf(t, st, cfg)
	read := `{"chapters": [{"startMs": 0, "endMs": 5000}, {"startMs": 5000, "endMs": 9000}],
		"read": {"path": "` + media + `", "found": 2}}`
	for i, tc := range []struct {
		method, body string
		missing      bool
		rows         string
	}{
		{http.MethodPut, read, false, chapterSource},
		{http.MethodPut, `{"chapters": [{"startMs": 0, "endMs": 7000, "title": "Edited"}]}`, true, ""},
		{http.MethodPut, read, false, chapterSource},
		{http.MethodDelete, "", true, ""},
	} {
		if w := do(h, tc.method, "/api/chapters/items/"+chapterFilm, tc.body, service); w.Code != http.StatusOK {
			t.Fatalf("%d: %s: %d %s", i+1, tc.method, w.Code, w.Body.String())
		}
		a := getChapters(t, h, service)
		if a.Missing != tc.missing || a.Read == nil || (a.Read.RowsSourceID == nil) != (tc.rows == "") ||
			(tc.rows != "" && *a.Read.RowsSourceID != tc.rows) {
			t.Errorf("%d: after the %s: %+v", i+1, tc.method, a)
		}
		if i == 2 && !strings.Contains(string(a.Read.Replaced), `"Edited"`) {
			t.Errorf("the edited chapter was not kept to undo: %s", a.Read.Replaced)
		}
	}
}

// Where no read is kept — the catalog's chapters are not configured, or its
// migration 055 is missing — only chapters a read found are written: a read
// that failed, or found none, changes nothing (503), so it never deletes the
// title's chapters; a PUT without a read writes its chapters as it always did.
func TestWhereNoReadIsKeptAnEmptyOrFailedReadChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(testing.TB) *store.Store
		with bool // the chapters service configured
	}{
		{"not configured", storetest.Open, false},
		{"no migration 055", openWithout055, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.open(t)
			dir := t.TempDir()
			cfg := testConfig(dir)
			h, iss := chapterServer(t, st, cfg, tc.with)
			service := iss.Service(t, "zaentrum-manager")
			media := chapterFilmOf(t, st, cfg)
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemchapters (id, item_id, startms, endms, title, ordinal)
				VALUES ('c1', $1, 0, 1000, 'One', 1), ('c2', $1, 1000, 2000, 'Two', 2)`, chapterFilm)
			count := func() int {
				return storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, chapterFilm)
			}
			for _, body := range []string{
				`{"read": {"path": "` + media + `", "error": "ffprobe exited 1: Read error"}}`,
				`{"chapters": [], "read": {"path": "` + media + `", "found": 0}}`,
				`{"chapters": [{"startMs": 0, "endMs": 5000}], "read": {"path": "` + media + `", "error": "a read that failed"}}`,
				`{"released": true, "token": "t"}`,
			} {
				if w := do(h, http.MethodPut, "/api/chapters/items/"+chapterFilm, body, service); w.Code != http.StatusServiceUnavailable ||
					!strings.Contains(w.Body.String(), "changes nothing") || count() != 2 {
					t.Errorf("%s: %d %s, %d chapters; want 503 and the 2 chapters kept", body, w.Code, w.Body.String(), count())
				}
			}
			if w := do(h, http.MethodPut, "/api/chapters/items/"+chapterFilm,
				`{"chapters": [{"startMs": 0, "endMs": 5000}], "read": {"path": "`+media+`", "found": 1}}`, service); w.Code != http.StatusOK ||
				count() != 1 {
				t.Errorf("a read that found one: %d %s, %d chapters", w.Code, w.Body.String(), count())
			}
			if w := do(h, http.MethodPut, "/api/chapters/items/"+chapterFilm, `{"chapters": []}`, service); w.Code != http.StatusOK ||
				count() != 0 {
				t.Errorf("a PUT without a read: %d %s, %d chapters", w.Code, w.Body.String(), count())
			}
			if w := do(h, http.MethodGet, "/api/chapters/items/"+chapterFilm, "", service); w.Code != http.StatusServiceUnavailable {
				t.Errorf("GET: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

// openWithout055 is a catalog with every migration the service applies at
// startup but 055.
func openWithout055(t testing.TB) *store.Store {
	st := storetest.Open(t)
	storetest.Exec(t, st, `DROP VIEW com_nalet_katalog_chaptersmissing;
		DROP TABLE com_nalet_katalog_chapterreads, com_nalet_katalog_chapterclaims`)
	return st
}

// chapterServer serves the REST routes as server does, with the chapters
// service configured (with) or not.
func chapterServer(t *testing.T, st *store.Store, cfg config.Config, with bool) (http.Handler, *authtest.Issuer) {
	t.Helper()
	if with {
		return server(t, st, cfg)
	}
	iss := authtest.NewIssuer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	jwt, err := auth.NewJWTVerifier(ctx, iss.URL, "chino", false, false)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := auth.NewStreamVerifier(base64.StdEncoding.EncodeToString(streamKey))
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Group(func(pr chi.Router) {
		pr.Use(auth.NewMiddleware(jwt, stream).Handler)
		New(Deps{Store: st, Cfg: cfg, Steps: processing.New(st.Pool())}).Register(pr)
	})
	return r, iss
}
