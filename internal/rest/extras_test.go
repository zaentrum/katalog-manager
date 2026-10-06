package rest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	kafka "github.com/segmentio/kafka-go"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/extras"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// kafkaRecorder stands in for the Kafka writer the service's producer writes
// through: it records every message.
type kafkaRecorder struct {
	mu   sync.Mutex
	msgs []kafka.Message
}

func (k *kafkaRecorder) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.msgs = append(k.msgs, msgs...)
	return nil
}

func (k *kafkaRecorder) Close() error { return nil }

// take returns the messages written since the last take, by topic.
func (k *kafkaRecorder) take() map[string][]kafka.Message {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string][]kafka.Message{}
	for _, m := range k.msgs {
		out[m.Topic] = append(out[m.Topic], m)
	}
	k.msgs = nil
	return out
}

// extrasServer is server with an event bus that writes into k, for the
// extras' triggers and their packaged announcements.
func extrasServer(t *testing.T, st *store.Store, cfg config.Config, k *kafkaRecorder) (http.Handler, *authtest.Issuer) {
	t.Helper()
	events.Configure("stube.")
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
	prod := events.ProducerOn(k)
	steps := processing.New(st.Pool())
	r := chi.NewRouter()
	r.Group(func(pr chi.Router) {
		pr.Use(auth.NewMiddleware(jwt, stream).Handler)
		New(Deps{Store: st, Cfg: cfg, Steps: steps, Events: prod, Extras: extras.New(st, cfg, steps.Policy(), prod)}).Register(pr)
	})
	return r, iss
}

// aFilmWithATrailer is the film, and beside it in the extras' root a trailer
// file.
func aFilmWithATrailer(t *testing.T, st *store.Store, cfg config.Config) string {
	t.Helper()
	aFilm(t, st, cfg.NFSRoot)
	trailer := cfg.ExtrasRoot + "/a-film/trailer.mov"
	files(t, cfg.ExtrasRoot, map[string]int64{"a-film/trailer.mov": 100000})
	return trailer
}

func extrasConfig(dir string) config.Config {
	cfg := testConfig(dir)
	cfg.ExtrasRoot, cfg.LegacyLibraryRoot = dir+"/extras", dir+"/library"
	return cfg
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("%s is no JSON object: %v", body, err)
	}
	return m
}

// An extra through its whole chain on the REST routes, as an operator's Job
// and the workers call them: taken in (201, its trigger sent), its record
// read, its transcode and package reported, its package recorded (ready,
// with what its master playlist and its manifest say, announced as an event
// of its title and never as the title packaged), the packager's late end
// harmless; removed, it is not found.
func TestAnExtrasChainThroughTheWorkerProtocol(t *testing.T) {
	st := storetest.Open(t)
	cfg := extrasConfig(t.TempDir())
	trailer := aFilmWithATrailer(t, st, cfg)
	k := &kafkaRecorder{}
	h, iss := extrasServer(t, st, cfg, k)
	svc := iss.Service(t, "zaentrum-manager")

	w := do(h, http.MethodPost, "/api/extras", `{"itemPath": "`+cfg.NFSRoot+`/A Film (2024)/A Film (2024).mkv", "path": "`+
		trailer+`", "kind": "trailer", "language": "en"}`, svc)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /api/extras: %d %s", w.Code, w.Body.String())
	}
	body := decode(t, w.Body.String())
	id, _ := body["extraId"].(string)
	if body["itemId"] != filmItem || body["created"] != true || body["kind"] != "trailer" || body["title"] != "Trailer" ||
		body["state"] != "queued" || len(id) != 36 {
		t.Errorf("POST /api/extras: %s", w.Body.String())
	}
	sent := k.take()
	if q := sent["stube.catalog.extra.queued"]; len(q) != 1 || string(q[0].Key) != id || strings.Contains(string(q[0].Value), "itemId") {
		t.Errorf("the trigger: %v", sent)
	}
	if w := do(h, http.MethodPost, "/api/extras", `{"itemId": "`+filmItem+`", "path": "`+trailer+`", "kind": "trailer"}`, svc); w.Code != http.StatusOK ||
		decode(t, w.Body.String())["created"] != false || decode(t, w.Body.String())["extraId"] != id {
		t.Errorf("the same file again: %d %s", w.Code, w.Body.String())
	}

	w = do(h, http.MethodGet, "/api/analyze/extras/"+id, "", svc)
	if want := `{"id":"` + id + `","type":"extra","parentId":"` + filmItem + `","parentType":"movie","parentTitle":"A Film",` +
		`"kind":"trailer","title":"Trailer","language":"en","seasonNumber":null,"path":"` + trailer + `","state":"queued"}`; w.Code != http.StatusOK ||
		strings.TrimSpace(w.Body.String()) != want {
		t.Errorf("the record:\n got  %d %s\n want %s", w.Code, w.Body.String(), want)
	}

	put := func(step, body string) string {
		t.Helper()
		w := do(h, http.MethodPut, "/api/analyze/extras/"+id+"/steps/"+step, body, svc)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s %s: %d %s", step, body, w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	for _, c := range []struct{ step, body, state string }{
		{"transcode", `{"status": "in_progress"}`, "transcoding"},
		{"transcode", `{"status": "done", "details": "profile=h264-720p src_codec=vp9 res=1920x1080"}`, "transcoded"},
		{"package", `{"status": "in_progress"}`, "packaging"},
	} {
		var status string
		_ = json.Unmarshal([]byte(c.body), &struct{ Status *string }{&status})
		if got, want := put(c.step, c.body), `{"extraId":"`+id+`","state":"`+c.state+`","status":"`+status+`","step":"`+c.step+`"}`; got != want {
			t.Errorf("%s %s: %s, want %s", c.step, c.body, got, want)
		}
	}

	// The package, as the packager leaves it in the store.
	root := filepath.Join(cfg.PackagesRoot, "extras", id[:2], id)
	files(t, root, map[string]int64{"manifest.json": 900, ".complete": 71, "hls/v0/seg-1.m4s": 3000000, "hls/v1/seg-1.m4s": 1000000,
		".next/stale.m4s": 50000})
	if err := os.WriteFile(filepath.Join(root, "hls", "master.m3u8"), []byte("#EXTM3U\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=2396000,CODECS=\"avc1.64001f,mp4a.40.2\"\nv0/playlist.m3u8\n"+
		"#EXT-X-STREAM-INF:BANDWIDTH=1100000,CODECS=\"avc1.64001e,mp4a.40.2\"\nv1/playlist.m3u8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"version": 2, "itemId": "` + id + `", "type": "extra", "parentId": "` + filmItem + `", "extraKind": "trailer",
		"title": "Trailer", "year": null, "tmdbId": null, "durationMs": 33000,
		"renditions": {"video": [{"id": "v1", "codec": "avc1.64001e", "width": 854, "height": 480},
		                         {"id": "v0", "codec": "avc1.64001f", "width": 1280, "height": 720}],
		               "audio": [{"id": "a0", "codec": "mp4a.40.2", "language": "eng", "default": true, "channels": 2}],
		               "audioSurround": []},
		"subtitles": [], "hls": {"master": "hls/master.m3u8", "segmentSeconds": 6}}`
	w = do(h, http.MethodPost, "/api/extras/"+id+"/packaging-complete", manifest, svc)
	if want := `{"durationMs":33000,"extraId":"` + id + `","itemId":"` + filmItem + `","packaged":true}`; w.Code != http.StatusOK ||
		strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("packaging-complete:\n got  %d %s\n want %s", w.Code, w.Body.String(), want)
	}
	x, err := st.GetExtra(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if x.State != "ready" || !x.Playable() || *x.PackagePath != root || *x.DurationMs != 33000 || *x.VideoCodec != "avc1.64001f" ||
		*x.Width != 1280 || *x.Height != 720 || *x.PeakBandwidthBps != 2396000 || x.Failures != 0 ||
		*x.PackageSizeBytes != 900+71+3000000+1000000+int64(len("#EXTM3U\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=2396000,CODECS=\"avc1.64001f,mp4a.40.2\"\nv0/playlist.m3u8\n"+
			"#EXT-X-STREAM-INF:BANDWIDTH=1100000,CODECS=\"avc1.64001e,mp4a.40.2\"\nv1/playlist.m3u8\n")) {
		t.Errorf("the extra packaged: %+v (size %v)", *x, *x.PackageSizeBytes)
	}
	sent = k.take()
	if len(sent["stube.catalog.item.packaged"]) != 0 {
		t.Error("an extra's package announced the title packaged")
	}
	p := sent["stube.catalog.extra.packaged"]
	if len(p) != 1 || string(p[0].Key) != id {
		t.Fatalf("the announcement: %v", sent)
	}
	ev := decode(t, string(p[0].Value))
	var keys []string
	for k := range ev {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, " ") != "eventId extraId itemId kind occurredAt source status step type" || ev["itemId"] != filmItem ||
		ev["type"] != "movie" || ev["step"] != "extra" || ev["status"] != "done" || ev["extraId"] != id || ev["kind"] != "trailer" ||
		ev["source"] != "katalog-manager" {
		t.Errorf("the announcement: %s", p[0].Value)
	}

	// The packager's end comes after; a transcoder's late report of a run
	// before changes nothing.
	if got := put("package", `{"status": "done"}`); !strings.Contains(got, `"state":"ready"`) {
		t.Errorf("the package's end after ready: %s", got)
	}
	if got := put("transcode", `{"status": "failed", "error": "a late run"}`); !strings.Contains(got, `"state":"ready"`) {
		t.Errorf("a late failure after ready: %s", got)
	}

	if _, err := st.RemoveExtra(context.Background(), id, "admin-1", "", 0); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/analyze/extras/" + id, ""},
		{http.MethodPut, "/api/analyze/extras/" + id + "/steps/transcode", `{"status": "in_progress"}`},
		{http.MethodPost, "/api/extras/" + id + "/packaging-complete", manifest},
	} {
		if w := do(h, c.method, c.path, c.body, svc); w.Code != http.StatusNotFound {
			t.Errorf("%s %s of a removed extra: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

// A failed run, reported, is retried by the policy: pending, a backoff
// later, then failed when no attempt is left; a package failing before the
// packager says it started counts too. An extra there is not, removed or of
// a step it has not answers 404, 400; one whose file is missing 409.
func TestAnExtrasReportsThroughREST(t *testing.T) {
	st := storetest.Open(t)
	cfg := extrasConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	h, iss := extrasServer(t, st, cfg, &kafkaRecorder{})
	svc := iss.Service(t, "zaentrum-manager")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, state) VALUES
		('x-run', $1, 'trailer', 'Trailer', 'api', '/e/1.mov', 'queued'),
		('x-missing', $1, 'teaser', 'Teaser', 'scanner', '/e/2.mov', 'missing')`, filmItem)
	state := func() string {
		var s string
		var failures int
		if err := st.Pool().QueryRow(context.Background(), `SELECT state, failures FROM com_nalet_katalog_itemextras WHERE id = 'x-run'`).
			Scan(&s, &failures); err != nil {
			t.Fatal(err)
		}
		return s + " " + string(rune('0'+failures))
	}
	requeue := func() {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued' WHERE id = 'x-run'`)
	}
	fail := `{"status": "failed", "error": "ffmpeg exited 1"}`
	if w := do(h, http.MethodPut, "/api/analyze/extras/x-run/steps/transcode", fail, svc); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"state":"pending"`) || state() != "pending 1" {
		t.Errorf("a failed transcode: %d %s, %s", w.Code, w.Body.String(), state())
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'transcoded' WHERE id = 'x-run'`)
	if w := do(h, http.MethodPut, "/api/analyze/extras/x-run/steps/package", `{"status": "failed", "error": "transcoder handoff: no renditions.json"}`, svc); w.Code != http.StatusOK ||
		state() != "pending 2" {
		t.Errorf("a package failing before it started: %d %s, %s", w.Code, w.Body.String(), state())
	}
	requeue()
	if w := do(h, http.MethodPut, "/api/analyze/extras/x-run/steps/transcode", fail, svc); w.Code != http.StatusOK || state() != "failed 3" {
		t.Errorf("the third failure: %d %s, %s", w.Code, w.Body.String(), state())
	}
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/api/analyze/extras/unknown/steps/transcode", `{"status": "in_progress"}`, http.StatusNotFound},
		{"/api/analyze/extras/x-missing/steps/transcode", `{"status": "in_progress"}`, http.StatusConflict},
		{"/api/analyze/extras/x-run/steps/analyze", `{"status": "done"}`, http.StatusBadRequest},
		{"/api/analyze/extras/x-run/steps/transcode", `{"status": "skipped"}`, http.StatusBadRequest},
		{"/api/analyze/extras/x-run/steps/transcode", `{}`, http.StatusBadRequest},
		{"/api/analyze/extras/x-run/steps/transcode", `not json`, http.StatusBadRequest},
	} {
		if w := do(h, http.MethodPut, c.path, c.body, svc); w.Code != c.code {
			t.Errorf("PUT %s %s: %d %s, want %d", c.path, c.body, w.Code, w.Body.String(), c.code)
		}
	}
	if w := do(h, http.MethodGet, "/api/analyze/extras/unknown", "", svc); w.Code != http.StatusNotFound {
		t.Errorf("the record of an extra there is not: %d", w.Code)
	}
	if w := do(h, http.MethodGet, "/api/analyze/extras/x-missing", "", svc); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), `"state":"missing"`) {
		t.Errorf("the record of a missing extra: %d %s", w.Code, w.Body.String())
	}
}

// packaging-complete takes the manifest of the extra's package and nothing
// else: one of another package, of an item, or without video is refused,
// and the extra is not ready.
func TestExtraPackagingCompleteTakesItsOwnPackage(t *testing.T) {
	st := storetest.Open(t)
	cfg := extrasConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	k := &kafkaRecorder{}
	h, iss := extrasServer(t, st, cfg, k)
	svc := iss.Service(t, "zaentrum-manager")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, state)
		VALUES ('x1', $1, 'trailer', 'Trailer', 'api', '/e/1.mov', 'packaging')`, filmItem)
	video := `"renditions": {"video": [{"id": "v0", "codec": "avc1.64001f", "width": 1280, "height": 720, "bitrateBps": 2000000}]}`
	for _, body := range []string{
		`{"itemId": "x2", "type": "extra", ` + video + `}`,
		`{"itemId": "x1", "type": "extra", "parentId": "another", ` + video + `}`,
		`{"itemId": "x1", "type": "movie", ` + video + `}`,
		`{"itemId": "x1", "type": "extra", "renditions": {"video": []}}`,
		`{"itemId": "x1", "type": "extra"}`,
		`not json`,
	} {
		if w := do(h, http.MethodPost, "/api/extras/x1/packaging-complete", body, svc); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE state = 'ready'`); n != 0 {
		t.Error("a refused manifest made the extra ready")
	}
	if w := do(h, http.MethodPost, "/api/extras/unknown/packaging-complete", `{`+video+`}`, svc); w.Code != http.StatusNotFound {
		t.Errorf("an extra there is not: %d", w.Code)
	}
	// Without a master playlist to read, the peak is the top rendition's bit
	// rate; a manifest without a type or ids is taken as the extra's.
	if w := do(h, http.MethodPost, "/api/extras/x1/packaging-complete", `{`+video+`}`, svc); w.Code != http.StatusOK {
		t.Fatalf("a manifest of the extra's: %d %s", w.Code, w.Body.String())
	}
	x, _ := st.GetExtra(context.Background(), "x1")
	if x.State != "ready" || *x.PeakBandwidthBps != 2000000 || x.DurationMs != nil || x.PackageSizeBytes != nil {
		t.Errorf("the extra: %+v", *x)
	}
	if len(k.take()["stube.catalog.extra.packaged"]) != 1 {
		t.Error("the package was not announced")
	}
	// A missing extra's package is kept, and not announced: it does not play.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'missing', hidden = true WHERE id = 'x1'`)
	if w := do(h, http.MethodPost, "/api/extras/x1/packaging-complete", `{`+video+`}`, svc); w.Code != http.StatusOK {
		t.Fatalf("a missing extra's package: %d %s", w.Code, w.Body.String())
	}
	if len(k.take()) != 0 {
		t.Error("a missing extra's package was announced")
	}
}

// POST /api/extras answers a refusal with its status, its code and what is
// in the way; without the extras configured, 503.
func TestPostExtraRefusals(t *testing.T) {
	st := storetest.Open(t)
	cfg := extrasConfig(t.TempDir())
	trailer := aFilmWithATrailer(t, st, cfg)
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	h, iss := extrasServer(t, st, cfg, &kafkaRecorder{})
	admin := iss.Admin(t)
	if w := do(h, http.MethodPost, "/api/extras", `{"itemId": "`+filmItem+`", "path": "`+trailer+`", "kind": "trailer"}`, admin); w.Code != http.StatusCreated {
		t.Fatalf("POST /api/extras: %d %s", w.Code, w.Body.String())
	}
	x, err := st.ExtraAt(context.Background(), trailer)
	if err != nil || x == nil {
		t.Fatalf("the extra: %v, %v", x, err)
	}
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{`{"itemId": "s1", "path": "` + trailer + `", "kind": "trailer"}`, http.StatusConflict,
			`{"code":"EXTRA_CONFLICT","error":"` + trailer + ` is extra ` + x.ID + ` of item ` + filmItem + ` already","extraId":"` + x.ID + `","itemId":"` + filmItem + `"}`},
		{`{"itemId": "unknown", "path": "` + trailer + `", "kind": "trailer"}`, http.StatusNotFound,
			`{"code":"NOT_FOUND","error":"unknown item: unknown"}`},
		{`{"itemId": "s1", "path": "/etc/hosts", "kind": "trailer"}`, http.StatusBadRequest,
			`{"code":"EXTRA_REFUSED","error":"/etc/hosts is not under the media root, the library's or the extras' folder or EXTRAS_ROOT (or is under the package store)"}`},
		{`{"itemId": 7}`, http.StatusBadRequest, `{"error":"invalid JSON body"}`},
	} {
		w := do(h, http.MethodPost, "/api/extras", c.body, admin)
		if w.Code != c.code || strings.TrimSpace(w.Body.String()) != c.want {
			t.Errorf("%s:\n got  %d %s\n want %d %s", c.body, w.Code, w.Body.String(), c.code, c.want)
		}
	}
	w := httptest.NewRecorder()
	New(Deps{Store: st, Cfg: cfg}).postExtra(w, httptest.NewRequest(http.MethodPost, "/api/extras", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("without the extras configured: %d", w.Code)
	}
}
