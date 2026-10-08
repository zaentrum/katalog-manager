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

// A version taken marks the episodes its title's file covers changed with the
// title: their projections play it.
func TestPackagingCompleteMarksTheEpisodesItsFileCovers(t *testing.T) {
	f := newV2Film(t)
	complete := f.version(t, filmVersion, filmPackage)
	storetest.AddItem(t, f.st, "covered", "episode", "Part Two", "")
	storetest.AddItem(t, f.st, "other", "episode", "Another", "")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $1 WHERE id = 'covered'`, filmItem)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	if code, answer := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE modifiedat > '2001-01-01'
		AND id IN ($1, 'covered')`, filmItem); n != 2 {
		t.Errorf("%d of the title and the episode its file covers are marked changed, want both", n)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 'other' AND modifiedat > '2001-01-01'`); n != 0 {
		t.Error("an episode the file does not cover is marked changed")
	}
}

// The worker record of an episode whose file covers others names them in its
// library's source (covers): the episode first, then those it covers, in
// episode order, as its source record lists them; the record of a file of one
// episode names none. A covered episode has no file, and no worker record.
func TestTheWorkerRecordNamesTheEpisodesTheFileCovers(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	const show, e1, e2, e3, e4 = "5e5e0000-0000-4000-8000-000000000001", "e1e10000-0000-4000-8000-000000000001",
		"e1e10000-0000-4000-8000-000000000002", "e1e10000-0000-4000-8000-000000000003", "e1e10000-0000-4000-8000-000000000004"
	files(t, dir, map[string]int64{".work/incoming/Show/Show.S01E01E03.mkv": 3000, ".work/incoming/Show/Show.S01E04.mkv": 2000})
	storetest.AddItem(t, st, show, "series", "Show", "")
	for i, id := range []string{e1, e2, e3, e4} {
		storetest.AddItem(t, st, id, "episode", "Show", show)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, id, i+1)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes) VALUES
		('a1', $1, $3, true, 3000), ('a4', $2, $4, true, 2000)`, e1, e4,
		cfg.ArrivalsRoot+"/Show/Show.S01E01E03.mkv", cfg.ArrivalsRoot+"/Show/Show.S01E04.mkv")
	ctx := context.Background()
	for _, id := range []string{e3, e2} {
		if _, err := library.Link(ctx, st.Pool(), e1, id); err != nil {
			t.Fatal(err)
		}
	}
	src := func(id string) map[string]any {
		t.Helper()
		lib, _ := recordOf(t, h, svc, "/api/analyze/items/"+id)["library"].(map[string]any)
		s, _ := lib["source"].(map[string]any)
		return s
	}
	if got, _ := json.Marshal(src(e1)["covers"]); string(got) != `["`+e1+`","`+e2+`","`+e3+`"]` {
		t.Errorf("the holder's source covers %s, want it, then S01E02, then S01E03", got)
	}
	if covers, ok := src(e4)["covers"]; ok {
		t.Errorf("a file of one episode covers %v", covers)
	}
	if w := do(h, http.MethodGet, "/api/analyze/items/"+e2, "", svc); w.Code != http.StatusNotFound {
		t.Errorf("a covered episode's worker record: %d, want 404 (no file)", w.Code)
	}
}
