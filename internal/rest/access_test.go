package rest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

func do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(path, "/api/artwork/") && method == http.MethodPut {
		req.Header.Set("Content-Type", "image/png")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// route is one REST route, called as its caller would, and who may call it.
type route struct {
	method, path, body string
	who                string // viewer, worker or ingest
}

// extraX1 is the film's extra the worker routes are called for.
const extraX1 = "1b5c2a8e-0000-4000-8000-000000000001"

// routes is every route Register mounts.
var routes = []route{
	{http.MethodGet, "/api/artwork/m1/poster", "", "viewer"},
	{http.MethodGet, "/api/manage/artwork/m1/poster", "", "viewer"},
	{http.MethodGet, "/api/artwork/person/p1/profile", "", "viewer"},
	{http.MethodGet, "/api/manage/artwork/person/p1/profile", "", "viewer"},
	{http.MethodGet, "/api/play/m1", "", "viewer"},
	{http.MethodGet, "/api/subtitles/items/m1", "", "viewer"},
	{http.MethodGet, "/api/subtitles/sub1", "", "viewer"},

	{http.MethodPut, "/api/artwork/m1/backdrop", "a keyframe", "worker"},
	{http.MethodGet, "/api/analyze/items/m1", "", "worker"},
	{http.MethodGet, "/api/analyze/items/m1/steps", "", "worker"},
	{http.MethodPost, "/api/analyze/items/m1/steps/skip", `{"steps": ["blackframe"]}`, "worker"},
	{http.MethodPut, "/api/analyze/items/m1/steps/chapter", `{"status": "done"}`, "worker"},
	{http.MethodPost, "/api/analyze/items/m1/fail", `{"reason": "a test"}`, "worker"},
	{http.MethodGet, "/api/analyze/items/m1/siblings", "", "worker"},
	{http.MethodPost, "/api/analyze/series/s1/reset", "", "worker"},
	{http.MethodPut, "/api/segments/items/m1", `{"segments": []}`, "worker"},
	{http.MethodDelete, "/api/segments/items/m1", "", "worker"},
	{http.MethodPut, "/api/chapters/items/m1", `{"chapters": []}`, "worker"},
	{http.MethodDelete, "/api/chapters/items/m1", "", "worker"},
	{http.MethodPost, "/api/items/m1/packaging-complete", `{}`, "worker"},
	{http.MethodGet, "/api/settings", "", "worker"},
	{http.MethodGet, "/api/analyze/extras/" + extraX1, "", "worker"},
	{http.MethodPut, "/api/analyze/extras/" + extraX1 + "/steps/transcode", `{"status": "in_progress"}`, "worker"},
	{http.MethodPost, "/api/extras/" + extraX1 + "/packaging-complete",
		`{"type": "extra", "renditions": {"video": [{"id": "v0", "codec": "avc1.64001f", "width": 1280, "height": 720}]}}`, "worker"},
	{http.MethodPost, "/api/library/migrations/r1/adopt", "", "worker"},
	{http.MethodPost, "/api/library/migrations/r1/revert", `{"items": []}`, "worker"},
	{http.MethodPost, "/api/library/projections", `{}`, "worker"},
	{http.MethodPost, "/api/library/reencode", `{"items": ["m1"]}`, "worker"},
	{http.MethodGet, "/api/library/reencode", "", "worker"},

	{http.MethodPost, "/api/ingest", `{"path": "MEDIA/new.mkv", "type": "movie", "title": "New"}`, "ingest"},
	{http.MethodPost, "/api/extras", `{"itemId": "m1", "path": "MEDIA/m1-trailer.mkv", "kind": "trailer"}`, "ingest"},

	{http.MethodPost, "/api/items/m1/package", "", "admin"},
}

// Every route answers whom it is for, and refuses everyone else before it
// does anything: a viewer reads artwork, playback and subtitles; the worker
// protocol and the library's migration are the service account's and the
// admins'; an addon only ingests; the packaging action is an admin's alone.
// A stream token reads artwork and nothing else, and no token gets nowhere.
func TestEveryRouteIsForWhomItIsFor(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.LibraryRoot = dir // the migration run r1, staged with nothing in it
	for _, d := range []string{cfg.NFSRoot, cfg.PackagesRoot, dir + "/.work/migration/r1"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	media, sub := cfg.NFSRoot+"/m1.mkv", cfg.NFSRoot+"/m1.en.vtt"
	for f, content := range map[string]string{media: "a film", sub: "WEBVTT\n", cfg.NFSRoot + "/m1-trailer.mkv": "a trailer"} {
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a1', 'm1', $1, true)`, media)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format) VALUES ('sub1', 'm1', $1, 'vtt')`, sub)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemartworkdata (id, item_id, kind, contenttype, bytes)
		VALUES ('d1', 'm1', 'poster', 'image/jpeg', '\xffd8')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, kind, contenttype, bytes, sha256, isprimary)
		VALUES ('pa1', 'p1', 'profile', 'image/jpeg', '\xffd8', repeat('a', 64), true)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, state)
		VALUES ($1, 'm1', 'teaser', 'Teaser', 'api', '/extras/m1/teaser.mov', 'queued')`, extraX1)
	h, iss := server(t, st, cfg)

	stream := streamToken("viewer-1", time.Now().Add(time.Hour))
	callers := []struct {
		name, token string
		may         map[string]bool // the routes' who it may call
	}{
		{"a viewer", iss.Viewer(t), map[string]bool{"viewer": true}},
		{"another confidential client", iss.Service(t, "zaentrum-other"), map[string]bool{"viewer": true}},
		{"an addon", iss.Addon(t), map[string]bool{"viewer": true, "ingest": true}},
		{"the service account", iss.Service(t, "zaentrum-manager"), map[string]bool{"viewer": true, "worker": true, "ingest": true}},
		{"an admin", iss.Admin(t), map[string]bool{"viewer": true, "worker": true, "ingest": true, "admin": true}},
		{"an admin through the CLI", iss.CLIAdmin(t), map[string]bool{"viewer": true, "worker": true, "ingest": true, "admin": true}},
	}

	// Those who may not call a route first: nothing they ask for may happen.
	for _, c := range callers {
		for _, rt := range routes {
			if c.may[rt.who] {
				continue
			}
			w := do(h, rt.method, rt.path, strings.ReplaceAll(rt.body, "MEDIA", cfg.NFSRoot), c.token)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "forbidden: requires the zaentrum-admin") {
				t.Errorf("%s: %s %s: %d %s, want 403 naming who may", c.name, rt.method, rt.path, w.Code, w.Body.String())
			}
		}
	}
	for _, rt := range routes {
		if w := do(h, rt.method, rt.path, strings.ReplaceAll(rt.body, "MEDIA", cfg.NFSRoot), ""); w.Code != http.StatusUnauthorized {
			t.Errorf("no token: %s %s: %d, want 401", rt.method, rt.path, w.Code)
		}
		sep := "?"
		if strings.Contains(rt.path, "?") {
			sep = "&"
		}
		w := do(h, rt.method, rt.path+sep+"stream="+stream, strings.ReplaceAll(rt.body, "MEDIA", cfg.NFSRoot), "")
		artworkRead := rt.method == http.MethodGet && strings.Contains(rt.path, "/artwork/")
		if artworkRead && w.Code != http.StatusOK || !artworkRead && w.Code != http.StatusUnauthorized {
			t.Errorf("a stream token: %s %s: %d, want %s", rt.method, rt.path, w.Code,
				map[bool]string{true: "200", false: "401"}[artworkRead])
		}
	}
	for what, n := range map[string]int{
		"the uploaded backdrop": storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemartworkdata WHERE kind = 'backdrop'`),
		"a step":                storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps`),
		"the ingested item":     storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE title = 'New'`),
		"the extra taken in":    storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE kind = 'trailer'`),
		"the extra's report": storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras
			WHERE state <> 'queued' OR heartbeatat IS NOT NULL`),
		"a title queued to be encoded again": storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`),
	} {
		if n != 0 {
			t.Errorf("a refused caller left %s behind (%d rows)", what, n)
		}
	}

	// Then those who may: each route does what it does for them (route by
	// route, as a later route changes what an earlier one reads: the
	// packager's manifest replaces the package's subtitles).
	for _, rt := range routes {
		for _, c := range callers {
			if !c.may[rt.who] {
				continue
			}
			w := do(h, rt.method, rt.path, strings.ReplaceAll(rt.body, "MEDIA", cfg.NFSRoot), c.token)
			if w.Code < 200 || w.Code > 299 {
				t.Errorf("%s: %s %s: %d %s, want it done", c.name, rt.method, rt.path, w.Code, w.Body.String())
			}
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE title = 'New'`); n != 1 {
		t.Errorf("ingest by the addon, the service account and the admins: %d items, want the one", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemartworkdata WHERE kind = 'backdrop'`); n != 1 {
		t.Errorf("the backdrop the workers uploaded: %d rows, want 1", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE kind = 'trailer' AND item_id = 'm1'`); n != 1 {
		t.Errorf("the extras taken in by the addon, the service account and the admins: %d, want the one", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE id = $1 AND state = 'ready'`, extraX1); n != 1 {
		t.Error("the workers' packaging-complete did not make the extra ready")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = 'm1'`); n != 1 {
		t.Errorf("the film queued by the service account and the admins: %d rows, want the one", n)
	}
}

// A worker route refused names who may call it; ingest names the addon role
// too.
func TestRefusalsNameWhoMay(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, testConfig(t.TempDir()))
	for path, want := range map[string]string{
		"/api/analyze/items/m1/fail": `{"error":"forbidden: requires the zaentrum-admin role or the platform's service account"}`,
		"/api/ingest":                `{"error":"forbidden: requires the zaentrum-admin or the zaentrum-addon role, or the platform's service account"}`,
	} {
		w := do(h, http.MethodPost, path, `{}`, iss.Viewer(t))
		if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != want {
			t.Errorf("POST %s as a viewer: %d %s, want 403 %s", path, w.Code, w.Body.String(), want)
		}
	}
}

// routes names every route Register mounts, so a route added without a place
// in the table above is caught here.
func TestRoutesNameEveryRoute(t *testing.T) {
	r := chi.NewRouter()
	New(Deps{}).Register(r)
	mounted := map[string]bool{}
	if err := chi.Walk(r, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		mounted[method+" "+pattern] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, rt := range routes {
		pattern := r.Find(chi.NewRouteContext(), rt.method, rt.path)
		if pattern == "" {
			t.Errorf("%s %s is not mounted", rt.method, rt.path)
			continue
		}
		named[rt.method+" "+pattern] = true
	}
	for route := range mounted {
		if !named[route] {
			t.Errorf("%s is mounted, and the access table does not name it", route)
		}
	}
	if len(named) != len(routes) {
		t.Errorf("the table names %d routes, %d of them distinct", len(routes), len(named))
	}
}
