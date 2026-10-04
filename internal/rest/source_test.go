package rest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

func sourceOf(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT COALESCE(codec, '-') || ' ' || COALESCE(resolution, '-') || ' ' ||
		COALESCE(durationms::text, '-') || ' ' || COALESCE(bitratekbps::text, '-')
		FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary AND kind = 'primary'`, item).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A title's source gets what the workers probed it as: the transcoder's
// report of a transcode done or skipped says its codec and resolution, the
// packager's v2 manifest its duration (the bit rate follows from the size), a
// manifest's source block all of it, in the v1 names or the transcoder's.
func TestTheSourceKeepsWhatTheWorkersProbed(t *testing.T) {
	st := storetest.Open(t)
	for _, id := range []string{"m1", "m2", "m3"} {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes)
			VALUES ('src-'||$1::varchar, $1::varchar, '/media/'||$1::varchar, true, 62988700)`, id)
	}
	h, iss := server(t, st, testConfig(t.TempDir()))
	svc := iss.Service(t, "zaentrum-manager")
	call := func(method, path, body string) {
		t.Helper()
		if w := do(h, method, path, body, svc); w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
	}

	call(http.MethodPut, "/api/analyze/items/m1/steps/transcode", `{"status": "in_progress"}`)
	if got := sourceOf(t, st, "m1"); got != "- - - -" {
		t.Errorf("a transcode started: %s, want nothing yet", got)
	}
	call(http.MethodPut, "/api/analyze/items/m1/steps/transcode",
		`{"status": "done", "details": "profile=x265-804p src_codec=h264 res=1920x804 ladder=v0:libx265:1920x804 kf=105/6s out_mb=90 dur_s=99"}`)
	if got := sourceOf(t, st, "m1"); got != "h264 1920x804 - -" {
		t.Errorf("after the transcoder's report: %s", got)
	}
	call(http.MethodPost, "/api/items/m1/packaging-complete", `{"version": 2, "durationMs": 629887,
		"renditions": {"video": [{"codec": "hev1.1.6.L120.90", "width": 1920, "height": 804, "bitrateBps": 2000000}], "audio": []}}`)
	if got := sourceOf(t, st, "m1"); got != "h264 1920x804 629887 800" {
		t.Errorf("after a v2 manifest: %s, want its duration and the bit rate from the size", got)
	}

	// skipped: the copy's report
	call(http.MethodPut, "/api/analyze/items/m2/steps/transcode",
		`{"status": "not_applicable", "details": "skip codec=hevc res=3840x1608 reason=source_already_hevc:hevc"}`)
	if got := sourceOf(t, st, "m2"); got != "hevc 3840x1608 - -" {
		t.Errorf("after a copy's report: %s", got)
	}
	// a failed run says nothing of the source
	call(http.MethodPut, "/api/analyze/items/m3/steps/transcode", `{"status": "failed", "error": "ffprobe: no such file", "details": "src_codec=vp9 res=1x1"}`)
	if got := sourceOf(t, st, "m3"); got != "- - - -" {
		t.Errorf("after a failure: %s", got)
	}
	// a source block, in the v1 names, then the transcoder's
	call(http.MethodPost, "/api/items/m3/packaging-complete", `{"source": {"videoCodec": "h264", "resolution": "1280x720",
		"bitrateBps": 3000000, "durationMs": 600000}, "durationMs": 1}`)
	if got := sourceOf(t, st, "m3"); got != "h264 1280x720 600000 3000" {
		t.Errorf("after a v1 source block: %s", got)
	}
	call(http.MethodPost, "/api/items/m3/packaging-complete", `{"source": {"codec": "hevc", "width": 1920, "height": 1080, "bitRate": 4000000}}`)
	if got := sourceOf(t, st, "m3"); got != "hevc 1920x1080 600000 4000" {
		t.Errorf("after the transcoder's source block: %s", got)
	}
	w := do(h, http.MethodPost, "/api/items/m3/packaging-complete", `{"source": {"codec": "hevc"}}`, svc)
	if !strings.Contains(w.Body.String(), `"sourceEnriched":true`) {
		t.Errorf("packaging-complete with a source block: %s", w.Body.String())
	}
}
