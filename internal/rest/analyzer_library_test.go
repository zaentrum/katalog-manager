package rest

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A season of three episodes: e1 with its original, e2 whose original was
// deleted after packaging (its version complete), e3 with neither.
const (
	sibSeries = "5e5e5e5e-1111-4000-8000-000000000001"
	sibE1     = "e1e1e1e1-1111-4000-8000-000000000002"
	sibE2     = "e2e2e2e2-1111-4000-8000-000000000003"
	sibE3     = "e3e3e3e3-1111-4000-8000-000000000004"
)

func aSeasonWithARetiredEpisode(t *testing.T, st *store.Store, root string) string {
	t.Helper()
	storetest.AddItem(t, st, sibSeries, "series", "A Series", "")
	for i, e := range []string{sibE1, sibE2, sibE3} {
		storetest.AddItem(t, st, e, "episode", "Episode", sibSeries)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, e, i+1)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
			VALUES (gen_random_uuid()::varchar, $1, 'chromaprint', 'done')`, e)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_mediasegments (id, item_id, kind, startms, endms, source)
			VALUES (gen_random_uuid()::varchar, $1, 'intro', 0, 1000, 'chromaprint')`, e)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('p1', $1, '/media/e1.mkv', true),
		('p2', $2, '/lib/e2/sources/s2', false)`, sibE1, sibE2)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET kind = 'original' WHERE id = 'p2'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, retireeventid, retireeventat)
		VALUES ('s2', $1, 'e2.mkv', 1, 'deleted', 'ev-2', '2026-10-06 11:00:00+00')`, sibE2)
	dir := filepath.Join(root, "series", sibSeries[:2], sibSeries, "episodes", sibE2, "versions", "v2")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "v2", PackageID: library.NewID(), SourceIDs: []string{"s2"},
		CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"renditions": map[string]any{"video": []any{},
			"audio": []any{map[string]any{"id": "a0", "dir": "hls/a0"}}}}})
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir, completedat)
		VALUES ('v2', $1, ARRAY['s2']::varchar[], 'complete', $2, now())`, sibE2, dir)
	return dir
}

// With the v2 layout a sibling whose original was deleted after packaging
// is one still, its path the playlist of its package's first audio
// rendition; one with neither an original nor a version is none. With the
// legacy layout only a sibling with its original is.
func TestTheSiblingsOfAnEpisodeWithARetiredOne(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	version := aSeasonWithARetiredEpisode(t, st, dir)
	h, iss := server(t, st, v2Config(dir))
	worker := iss.Service(t, "zaentrum-manager")
	siblings := func() []analyzeItemView {
		t.Helper()
		w := do(h, http.MethodGet, "/api/analyze/items/"+sibE1+"/siblings", "", worker)
		var body struct{ Items []analyzeItemView }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("siblings: %d %s", w.Code, w.Body.String())
		}
		return body.Items
	}
	if got := siblings(); len(got) != 0 {
		t.Errorf("the legacy layout's siblings: %+v", got)
	}
	v2Layout(t, st)
	got := siblings()
	if len(got) != 1 || got[0].ID != sibE2 || got[0].Path == nil || *got[0].Path != filepath.Join(version, "hls/a0/playlist.m3u8") {
		t.Errorf("the v2 layout's siblings: %+v", got)
	}
}

// A series' reset leaves an episode whose original was deleted after
// packaging as it is, its steps and its marks, and says so; the others are
// reset as before.
func TestAResetOfASeriesLeavesARetiredEpisode(t *testing.T) {
	st := storetest.Open(t)
	aSeasonWithARetiredEpisode(t, st, t.TempDir())
	h, iss := server(t, st, testConfig(t.TempDir()))
	w := do(h, http.MethodPost, "/api/analyze/series/"+sibSeries+"/reset", "", iss.Service(t, "zaentrum-manager"))
	var body map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("reset: %d %s", w.Code, w.Body.String())
	}
	if body["episodes"] != 2.0 || body["stepsReset"] != 2.0 || body["segmentsPurged"] != 2.0 || body["originalsDeleted"] != 1.0 ||
		body["message"] != "1 episode is left as it is: the original was deleted after packaging (event ev-2, 2026-10-06T11:00:00Z), "+
			"and the analyzer reads the original" {
		t.Errorf("the reset: %v", body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND status = 'done'`, sibE2); n != 1 {
		t.Error("the retired episode's steps were reset")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_mediasegments WHERE item_id = $1`, sibE2); n != 1 {
		t.Error("the retired episode's marks were purged")
	}

	// A series none of whose episodes was retired: as before, no word of it.
	st2 := storetest.Open(t)
	storetest.AddItem(t, st2, sibSeries, "series", "A Series", "")
	storetest.AddItem(t, st2, sibE1, "episode", "Episode", sibSeries)
	h2, iss2 := server(t, st2, testConfig(t.TempDir()))
	w = do(h2, http.MethodPost, "/api/analyze/series/"+sibSeries+"/reset", "", iss2.Service(t, "zaentrum-manager"))
	body = map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 4 || body["episodes"] != 1.0 {
		t.Errorf("a reset of a series with its originals: %v", body)
	}
}
