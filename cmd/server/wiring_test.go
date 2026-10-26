package main

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The service as run wires it (wire, of the configuration config.Load reads),
// served as run serves it, and not a router a test puts together: the
// workers' reports of their rules are kept, and a package that completes, or
// a transcode that ends, marks its title for the library's compliance check
// to look at again - what the REST routes lost while run gave them no check
// (the report answered 503, and no title was marked). The check reaches
// GraphQL and the report's CSV, an admin's, and the re-encode queue asks it
// which titles it may not queue.
func TestTheServiceAsRunWiresIt(t *testing.T) {
	st := storetest.Open(t)
	iss := authtest.NewIssuer(t)
	dir := t.TempDir()
	for k, v := range map[string]string{
		"OIDC_ISSUER": iss.URL, "AUTH_DISABLED": "false", "STREAM_SIGNING_KEY": base64.StdEncoding.EncodeToString(streamKey),
		"SCANNER_NFS_ROOT": dir + "/media", "PACKAGES_ROOT": dir + "/packages", "LIBRARY_ROOT": dir + "/library",
		"EXTRAS_ROOT": dir + "/extras", "KAFKA_BROKERS": "", "CATALOG_EVENTS_ENABLED": "false",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	svc, err := wire(ctx, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.close)
	srv := httptest.NewServer(svc.handler)
	t.Cleanup(srv.Close)
	service, admin := iss.Service(t, "zaentrum-manager"), iss.Admin(t)
	call := func(method, path, token, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := call(http.MethodGet, "/healthz", "", ""); code != http.StatusOK {
		t.Errorf("/healthz: %d", code)
	}
	if code, _ := call(http.MethodPut, "/api/analyze/rules/packager", "", `{"rules": {}}`); code != http.StatusUnauthorized {
		t.Errorf("a report of rules without a token: %d, want 401", code)
	}
	for worker, body := range map[string]string{
		"transcoder": `{"version": "1.4.0", "rules": {"caps": {"movie1080Mbps": 8, "movie2160Mbps": 14, "movieMaxGiB": 15,
			"episode1080Mbps": 6, "episode2160Mbps": 8}, "bucket2160": {"minHeight": 2000, "minWidth": 3200},
			"encoder": {"codec": "hevc_nvenc"}, "ladder": "source:hevc"}}`,
		"packager": `{"version": "1.4.0", "rules": {"surroundAudio": "eac3", "stereoAudio": "aac", "imageSubtitles": ["pgs"]}}`,
	} {
		if code, b := call(http.MethodPut, "/api/analyze/rules/"+worker, service, body); code != http.StatusOK ||
			!strings.Contains(b, `"worker":"`+worker+`"`) {
			t.Errorf("the %s's report of its rules: %d %s, want it kept", worker, code, b)
		}
	}

	storetest.AddItem(t, st, "m1", "movie", "A Film Packaged Again", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('a-m1', 'm1', '/media/a-film-packaged-again.mkv', true)`)
	marked := func() int {
		return storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE item_id = 'm1' AND staleat IS NOT NULL`)
	}
	if n := marked(); n != 0 {
		t.Fatalf("the title is marked before anything happened to it")
	}
	if code, b := call(http.MethodPost, "/api/items/m1/packaging-complete", service, `{"durationMs": 600000,
		"renditions": {"video": [{"id": "v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080, "bitrateBps": 4000000}],
			"audio": [{"id": "a0", "idx": 0, "language": "eng", "codec": "mp4a.40.2", "channels": 2}]}}`); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %s", code, b)
	}
	if n := marked(); n != 1 {
		t.Error("a package that completed did not mark its title to be checked again")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET staleat = NULL`)
	if code, b := call(http.MethodPut, "/api/analyze/items/m1/steps/transcode", service,
		`{"status": "failed", "error": "ffmpeg exited 1"}`); code != http.StatusOK {
		t.Fatalf("a transcode that failed: %d %s", code, b)
	}
	if n := marked(); n != 1 {
		t.Error("a transcode that ended did not mark its title to be checked again")
	}

	if code, b := call(http.MethodPost, "/api/manage/query", admin,
		`{"query": "{ pipelineRules { transcoder { worker version } packager { worker } } libraryCompliance { total pending } }"}`); code != http.StatusOK ||
		strings.Contains(b, `"errors"`) || !strings.Contains(b, `"transcoder":{"worker":"transcoder","version":"1.4.0"}`) {
		t.Errorf("the check through GraphQL: %d %s", code, b)
	}
	if code, b := call(http.MethodGet, "/api/manage/compliance.csv", admin, ""); code != http.StatusOK ||
		!strings.HasPrefix(b, "item_id,kind,title,") {
		t.Errorf("the report's CSV, for an admin: %d %.80s", code, b)
	}
	if code, _ := call(http.MethodGet, "/api/manage/compliance.csv", service, ""); code != http.StatusForbidden {
		t.Errorf("the report's CSV, for the service account: %d, want 403", code)
	}

	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET blocked = 'novideo' WHERE item_id = 'm1'`)
	if code, b := call(http.MethodPost, "/api/library/reencode", service, `{"items": ["m1"]}`); code != http.StatusOK ||
		!strings.Contains(b, `"queued":0`) || !strings.Contains(b, "its original has no video") {
		t.Errorf("a title the check found without video, queued by its id: %d %s, want it skipped", code, b)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`); n != 0 {
		t.Errorf("%d titles queued, want none", n)
	}
}
