package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// ingester is a catalog an addon hands files to through POST /api/ingest,
// with its service account, in either layout, and an event bus that records
// what the service sends.
type ingester struct {
	st  *store.Store
	cfg config.Config
	h   http.Handler
	tok string // the addon's
	svc string // the platform's service account's
	k   *kafkaRecorder
	v2  bool
}

func newIngester(t *testing.T, v2 bool) *ingester {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := testConfig(dir)
	if v2 {
		v2Layout(t, st)
		cfg = v2Config(dir)
	}
	k := &kafkaRecorder{}
	h, iss := extrasServer(t, st, cfg, k)
	return &ingester{st: st, cfg: cfg, h: h, tok: iss.Addon(t), svc: iss.Service(t, "zaentrum-manager"), k: k, v2: v2}
}

// file writes the file rel under the root an ingest takes files from (the
// media root, the arrivals with the v2 layout), its bytes its own, and
// answers its path.
func (f *ingester) file(t *testing.T, rel string) string {
	t.Helper()
	root := f.cfg.NFSRoot
	if f.v2 {
		root = f.cfg.ArrivalsRoot
	}
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// ingest posts body and answers the status and the answer.
func (f *ingester) ingest(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := do(f.h, http.MethodPost, "/api/ingest", string(raw), f.tok)
	var answer map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatalf("POST /api/ingest: %d %s is no JSON object", w.Code, w.Body.String())
	}
	return w.Code, answer
}

// sent are the events sent since the last look, on topic, in the order
// they were sent.
func (f *ingester) sent(t *testing.T, topic string) []events.ItemEvent {
	t.Helper()
	var out []events.ItemEvent
	for _, m := range f.k.take()[topic] {
		var ev events.ItemEvent
		if err := json.Unmarshal(m.Value, &ev); err != nil {
			t.Fatalf("an event on %s: %v", topic, err)
		}
		if string(m.Key) != ev.ItemID {
			t.Errorf("an event of %s keyed by %s", ev.ItemID, m.Key)
		}
		out = append(out, ev)
	}
	return out
}

// externalIDs are the reference ids the catalog holds of the item id, as
// "source id" pairs, by source.
func externalIDs(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT source || ' ' || externalid FROM com_nalet_katalog_itemexternalids
		WHERE item_id = $1 ORDER BY source`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", ")
}

// keysOf are the keys of an answer, sorted.
func keysOf(m map[string]any) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// episodeBody is the body of an episode handed over by its series' identity.
func episodeBody(path, title string, series map[string]any, season, episode int) map[string]any {
	return map[string]any{"path": path, "type": "episode", "title": title, "series": series, "seasonNumber": season,
		"episodeNumber": episode}
}

// An episode handed over by its series' identity: a series the catalog does
// not hold is made from the identity and the title and year given, as the
// scanner makes one it finds (its title, sort title and the name it is found
// by, its year, a row for each of its ids, its scan done, its discovered
// event), and the episode is made under it, its own ids where the catalog
// keeps an episode's (its TMDB id under tmdb-episode). An episode of a
// series the catalog holds is made under that one, found by any of its ids,
// a series the scanner made and enrichment matched too, whose ids stay as
// they are. The answer names the series; the events name the identities.
func TestAnEpisodeIsHandedOverByItsSeriesIdentity(t *testing.T) {
	f := newIngester(t, false)
	body := episodeBody(f.file(t, "Show (2020)/Show.S04E07.mkv"), "Show", map[string]any{"tmdb": "97546", "tvdb": "371572"}, 4, 7)
	body["year"] = 2020
	body["identity"] = map[string]any{"tmdb": "937631", "imdb": "tt1234567"}
	code, answer := f.ingest(t, body)
	item, _ := answer["itemId"].(string)
	series, _ := answer["seriesId"].(string)
	if code != http.StatusOK || answer["created"] != true || len(item) != 36 || len(series) != 36 ||
		keysOf(answer) != "created itemId seriesId" {
		t.Fatalf("an episode of a series the catalog does not hold: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND type = 'series'
		AND title = 'Show' AND sorttitle = 'show' AND scantitle = 'show' AND year = 2020 AND parent_id IS NULL`, series); n != 1 {
		t.Error("the series is not made as the scanner makes one, from the title and year given")
	}
	if got := externalIDs(t, f.st, series); got != "tmdb 97546, tvdb 371572" {
		t.Errorf("the series' ids: %s", got)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items e JOIN com_nalet_katalog_playbackassets a
		ON a.item_id = e.id AND a.isprimary AND a.path = $3 WHERE e.id = $1 AND e.type = 'episode' AND e.parent_id = $2
		AND e.seasonnumber = 4 AND e.episodenumber = 7`, item, series, body["path"]); n != 1 {
		t.Error("the episode is not made under its series, with its file")
	}
	if got := externalIDs(t, f.st, item); got != "imdb tt1234567, tmdb-episode 937631" {
		t.Errorf("the episode's own ids: %s", got)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id IN ($1, $2)
		AND step = 'scan' AND status = 'done'`, series, item); n != 2 {
		t.Errorf("%d of the series and the episode are scanned, want both", n)
	}
	sent := f.sent(t, events.TopicDiscovered)
	if len(sent) != 2 {
		t.Fatalf("%d discovered events, want the series' and the episode's", len(sent))
	}
	if s := sent[0]; s.ItemID != series || s.Type != "series" || s.Step != "tmdb" || s.Source != "ingest" ||
		fmt.Sprint(s.ExternalIDs) != "map[tmdb:97546 tvdb:371572]" || s.SeriesExternalIDs != nil || s.SeasonNumber != nil {
		t.Errorf("the series' discovered event: %+v", s)
	}
	if e := sent[1]; e.ItemID != item || e.Type != "episode" || e.Source != "ingest" ||
		fmt.Sprint(e.ExternalIDs) != "map[imdb:tt1234567 tmdb-episode:937631]" ||
		fmt.Sprint(e.SeriesExternalIDs) != "map[tmdb:97546 tvdb:371572]" || *e.SeasonNumber != 4 || *e.EpisodeNumber != 7 {
		t.Errorf("the episode's discovered event: %+v", e)
	}

	// The next episode, by one of the series' ids: the same series.
	code, answer = f.ingest(t, episodeBody(f.file(t, "Show (2020)/Show.S04E08.mkv"), "Show", map[string]any{"tvdb": "371572"}, 4, 8))
	if code != http.StatusOK || answer["created"] != true || answer["seriesId"] != series {
		t.Errorf("an episode of the series the catalog holds: %d %v, want it under %s", code, answer, series)
	}
	if sent := f.sent(t, events.TopicDiscovered); len(sent) != 1 || sent[0].ItemID != answer["itemId"] {
		t.Errorf("discovered %+v, want the episode's alone", sent)
	}

	// A series the scanner made, which enrichment matched.
	const scanned = "5ca00000-0000-4000-8000-000000000001"
	storetest.AddItem(t, f.st, scanned, "series", "Other Show", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES ('x', $1, 'tmdb', '1399')`,
		scanned)
	code, answer = f.ingest(t, episodeBody(f.file(t, "Other Show/S01E01.mkv"), "Other Show",
		map[string]any{"tmdb": "1399", "imdb": "tt0944947"}, 1, 1))
	if code != http.StatusOK || answer["seriesId"] != scanned {
		t.Errorf("an episode of a series the scanner made: %d %v, want it under %s", code, answer, scanned)
	}
	if got := externalIDs(t, f.st, scanned); got != "tmdb 1399" {
		t.Errorf("the scanner's series' ids: %s, want them as they were", got)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE type = 'series'`); n != 2 {
		t.Errorf("%d series, want the one made and the scanner's", n)
	}
}

// Of a series the library holds twice, an episode handed over by its
// identity goes to the one made first, as the holdings name it.
func TestAnEpisodeGoesToTheOldestSeriesOfItsIdentity(t *testing.T) {
	f := newIngester(t, false)
	const older, newer = "01d00000-0000-4000-8000-000000000001", "0e000000-0000-4000-8000-000000000002"
	for id, created := range map[string]string{older: "2026-01-01", newer: "2026-06-01"} {
		storetest.AddItem(t, f.st, id, "series", "Show", "")
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET createdat = $2 WHERE id = $1`, id, created)
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid)
			VALUES (gen_random_uuid()::varchar, $1, 'tmdb', '97546')`, id)
	}
	code, answer := f.ingest(t, episodeBody(f.file(t, "Show/S01E01.mkv"), "Show", map[string]any{"tmdb": "97546"}, 1, 1))
	if code != http.StatusOK || answer["seriesId"] != older {
		t.Errorf("%d %v, want the episode under %s, the series made first", code, answer, older)
	}
}

// One file of several episodes handed over by its series' identity covers
// those after its first up to episodeEnd, as the catalog's covering model
// links the episodes a file covers: the series' episode of that number that
// has no file, one made for it when the series has none (its scan done, its
// discovered event, no file), and not one with a file of its own, which
// wins. Each covered one names the first, whose file it is, and runs none of
// the steps of a file; the answer lists them in episode order.
func TestAFileOfSeveralEpisodesCoversThem(t *testing.T) {
	f := newIngester(t, false)
	const series, e08, e10 = "5e500000-0000-4000-8000-000000000001", "e0800000-0000-4000-8000-000000000008",
		"e1000000-0000-4000-8000-000000000010"
	storetest.AddItem(t, f.st, series, "series", "Show", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES ('x', $1, 'tmdb', '1')`,
		series)
	for id, n := range map[string]int{e08: 8, e10: 10} {
		storetest.AddItem(t, f.st, id, "episode", "Show", series)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, id, n)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a10', $1, '/m/E10.mkv', true)`, e10)
	body := episodeBody(f.file(t, "Show/Show.S01E07-E10.mkv"), "Show", map[string]any{"tmdb": "1"}, 1, 7)
	body["episodeEnd"] = 10
	code, answer := f.ingest(t, body)
	holder, _ := answer["itemId"].(string)
	covered, _ := answer["covered"].([]any)
	if code != http.StatusOK || answer["created"] != true || answer["seriesId"] != series || len(covered) != 2 || covered[0] != e08 {
		t.Fatalf("a file of S01E07 to S01E10: %d %v", code, answer)
	}
	e09, _ := covered[1].(string)
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items e WHERE e.id = $1 AND e.type = 'episode'
		AND e.parent_id = $2 AND e.seasonnumber = 1 AND e.episodenumber = 9 AND e.title = 'Show'
		AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a WHERE a.item_id = e.id)`, e09, series); n != 1 {
		t.Error("S01E09 is not made under the series, with no file")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE coveredby = $1 AND id IN ($2, $3)`,
		holder, e08, e09); n != 2 {
		t.Errorf("%d of S01E08 and S01E09 name the holder, want both", n)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND coveredby IS NULL`, e10); n != 1 {
		t.Error("S01E10, with a file of its own, is covered")
	}
	for _, id := range []string{e08, e09} {
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
			AND step = ANY($2) AND status = 'not_applicable' AND error = $3`, id, processing.CoveredSteps,
			processing.CoveredReason(holder)); n != len(processing.CoveredSteps) {
			t.Errorf("%d of the steps of a file of %s do not apply, want all %d", n, id, len(processing.CoveredSteps))
		}
	}
	if covers, err := library.CoversOf(context.Background(), f.st.Pool(), holder); err != nil ||
		fmt.Sprint(covers) != fmt.Sprint([]string{holder, e08, e09}) {
		t.Errorf("the holder's file covers %v, %v; want it, S01E08 and S01E09", covers, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND step = 'scan' AND status = 'done'`, e09); n != 1 {
		t.Error("S01E09, made for the file, is not scanned")
	}
	sent := f.sent(t, events.TopicDiscovered)
	if len(sent) != 2 || sent[0].ItemID != holder || sent[1].ItemID != e09 {
		t.Fatalf("discovered %+v, want the holder's and S01E09's", sent)
	}
	if e := sent[1]; e.Type != "episode" || *e.SeasonNumber != 1 || *e.EpisodeNumber != 9 ||
		fmt.Sprint(e.SeriesExternalIDs) != "map[tmdb:1]" {
		t.Errorf("S01E09's discovered event: %+v", e)
	}
}

// An ingest by identity is idempotent on the path, in either layout: the
// same body again answers the title it made, created false, with its series
// and the episodes its file covers, and makes nothing and sends nothing.
// With the v2 layout the file is the holder's source, and no covered
// episode has one.
func TestIngestByIdentityIsIdempotent(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		f := newIngester(t, v2)
		episode := episodeBody(f.file(t, "Show/Show.S04E07E08.mkv"), "Show", map[string]any{"tmdb": "97546"}, 4, 7)
		episode["episodeEnd"] = 8
		movie := map[string]any{"path": f.file(t, "Film (2024)/Film (2024).mkv"), "type": "movie", "title": "Film", "year": 2024,
			"identity": map[string]any{"tmdb": "12345", "imdb": "tt0111161"}}
		_, first := f.ingest(t, episode)
		_, film := f.ingest(t, movie)
		f.k.take()
		items := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`)
		ids := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemexternalids`)
		code, again := f.ingest(t, episode)
		if code != http.StatusOK || again["created"] != false || again["itemId"] != first["itemId"] ||
			again["seriesId"] != first["seriesId"] || fmt.Sprint(again["covered"]) != fmt.Sprint(first["covered"]) ||
			len(first["covered"].([]any)) != 1 {
			t.Errorf("v2 %v: the episode again: %d %v, first %v", v2, code, again, first)
		}
		code, filmAgain := f.ingest(t, movie)
		if code != http.StatusOK || filmAgain["created"] != false || filmAgain["itemId"] != film["itemId"] ||
			keysOf(filmAgain) != "created itemId" {
			t.Errorf("v2 %v: the movie again: %d %v, first %v", v2, code, filmAgain, film)
		}
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`); n != items {
			t.Errorf("v2 %v: %d items, want the %d there were", v2, n, items)
		}
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemexternalids`); n != ids {
			t.Errorf("v2 %v: %d ids, want the %d there were", v2, n, ids)
		}
		if msgs := f.k.take(); len(msgs) != 0 {
			t.Errorf("v2 %v: sent %v again", v2, msgs)
		}
		if v2 {
			holder, covered := first["itemId"], first["covered"].([]any)[0]
			if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets a
				ON a.sourceid = s.id WHERE s.item_id = $1 AND a.item_id = $1`, holder); n != 1 {
				t.Error("the holder has no source its file's asset names")
			}
			if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE item_id = $1`, covered); n != 0 {
				t.Error("a covered episode has a source")
			}
		}
	}
}

// An ingest by identity is refused (409), naming the item that holds it, and
// writes nothing: an identity that contradicts what the catalog holds of the
// path (another series than the episode's, other numbers, another type, a
// movie another id or another movie's identity, an episode's own id another
// one), and a title the library holds already as another item (an episode of
// that series, season and number with a file of its own, or covered by
// another's file; a movie of that identity with a file). An episode of those
// numbers with no file holds none, and is made anew, as the scanner makes it.
func TestIngestRefusesWhatContradictsTheCatalog(t *testing.T) {
	f := newIngester(t, false)
	ep := f.file(t, "A/A.S01E01.mkv")
	epBody := episodeBody(ep, "A", map[string]any{"tmdb": "100"}, 1, 1)
	epBody["identity"] = map[string]any{"tmdb": "7001"}
	_, a := f.ingest(t, epBody)
	epID, series := a["itemId"].(string), a["seriesId"].(string)
	mv := f.file(t, "M (2001).mkv")
	_, m := f.ingest(t, map[string]any{"path": mv, "type": "movie", "title": "M", "identity": map[string]any{"tmdb": "500"}})
	mvID := m["itemId"].(string)
	const otherSeries, otherMovie = "b0000000-0000-4000-8000-000000000001", "b0000000-0000-4000-8000-000000000002"
	storetest.AddItem(t, f.st, otherSeries, "series", "B", "")
	storetest.AddItem(t, f.st, otherMovie, "movie", "N", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', $1, 'tmdb', '200'), ('x2', $2, 'tmdb', '600')`, otherSeries, otherMovie)
	f.k.take()
	items := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`)
	ids := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemexternalids`)
	withIdentity := func(body map[string]any, identity map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range body {
			out[k] = v
		}
		out["identity"] = identity
		return out
	}
	for name, c := range map[string]struct {
		body         map[string]any
		itemID, says string
	}{
		"another series' identity": {episodeBody(ep, "A", map[string]any{"tmdb": "200"}, 1, 1), epID,
			"an episode of series " + series + ", which the identity does not name: it names series " + otherSeries},
		"an identity its series is not": {episodeBody(ep, "A", map[string]any{"tmdb": "300"}, 1, 1), epID,
			"which is tmdb 100, not 300"},
		"other numbers": {episodeBody(ep, "A", map[string]any{"tmdb": "100"}, 1, 2), epID, "S01E01 of its series, and not S01E02"},
		"another type": {map[string]any{"path": ep, "type": "movie", "title": "A", "identity": map[string]any{"tmdb": "100"}}, epID,
			"an episode, and not a movie"},
		"another id of the episode": {withIdentity(episodeBody(ep, "A", map[string]any{"tmdb": "100"}, 1, 1),
			map[string]any{"tmdb": "7002"}), epID, "which is tmdb-episode 7001, not 7002"},
		"another id of the movie": {map[string]any{"path": mv, "type": "movie", "title": "M", "identity": map[string]any{"tmdb": "501"}},
			mvID, "which is tmdb 500, not 501"},
		"another movie's identity": {map[string]any{"path": mv, "type": "movie", "title": "M", "identity": map[string]any{"tmdb": "600"}},
			mvID, "it names movie " + otherMovie},
	} {
		code, answer := f.ingest(t, c.body)
		if code != http.StatusConflict || answer["itemId"] != c.itemID || !strings.Contains(fmt.Sprint(answer["error"]), c.says) {
			t.Errorf("%s: %d %v, want 409 naming %s, saying %q", name, code, answer, c.itemID, c.says)
		}
	}

	// Titles the library holds already.
	const e02, e03, e04 = "e0200000-0000-4000-8000-000000000002", "e0300000-0000-4000-8000-000000000003",
		"e0400000-0000-4000-8000-000000000004"
	for id, n := range map[string]int{e02: 2, e03: 3, e04: 4} {
		storetest.AddItem(t, f.st, id, "episode", "A", series)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, id, n)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a2', $1, '/m/A.S01E02E03.mkv', true)`, e02)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $1 WHERE id = $2`, e02, e03)
	items += 3
	for name, c := range map[string]struct {
		body         map[string]any
		itemID, says string
	}{
		"an episode with a file of its own": {episodeBody(f.file(t, "A/other.S01E02.mkv"), "A", map[string]any{"tmdb": "100"}, 1, 2),
			e02, "S01E02 of series " + series + " is in the library already: item " + e02 + ", with a file of its own"},
		"an episode another's file covers": {episodeBody(f.file(t, "A/other.S01E03.mkv"), "A", map[string]any{"tmdb": "100"}, 1, 3),
			e03, "which the file of episode " + e02 + " covers"},
		"a movie of the identity": {map[string]any{"path": f.file(t, "M again (2001).mkv"), "type": "movie", "title": "M",
			"identity": map[string]any{"imdb": "tt9", "tmdb": "500"}}, mvID, "the movie of tmdb 500, imdb tt9 is in the library already: item " + mvID},
	} {
		code, answer := f.ingest(t, c.body)
		if code != http.StatusConflict || answer["itemId"] != c.itemID || !strings.Contains(fmt.Sprint(answer["error"]), c.says) {
			t.Errorf("%s: %d %v, want 409 naming %s, saying %q", name, code, answer, c.itemID, c.says)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`); n != items {
		t.Errorf("%d items after the refusals, want the %d there were", n, items)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemexternalids`); n != ids {
		t.Errorf("%d ids after the refusals, want the %d there were", n, ids)
	}
	if msgs := f.k.take(); len(msgs) != 0 {
		t.Errorf("a refusal sent %v", msgs)
	}
	code, answer := f.ingest(t, episodeBody(f.file(t, "A/A.S01E04.mkv"), "A", map[string]any{"tmdb": "100"}, 1, 4))
	if code != http.StatusOK || answer["created"] != true || answer["itemId"] == e04 {
		t.Errorf("an episode whose number the series holds with no file: %d %v, want it made", code, answer)
	}
}

// What an ingest cannot read is refused (400) before anything is read or
// written: an episode with neither its series' catalog id nor its identity,
// or with both; an id that is no string, or not of its source's form, or of
// a source the catalog does not find titles by; series and episodeEnd of
// anything but an episode, identity of anything but a movie or an episode;
// numbers below 0; and a file of more than ten episodes, or one that ends
// before it begins.
func TestAnIngestByIdentityItCannotReadIsRefused(t *testing.T) {
	h := &Handlers{}
	ep := func(more map[string]any) map[string]any {
		body := map[string]any{"path": "/var/lib/katalog/media/x.mkv", "type": "episode", "title": "Show",
			"seasonNumber": 1, "episodeNumber": 2}
		for k, v := range more {
			body[k] = v
		}
		return body
	}
	movie := func(more map[string]any) map[string]any {
		body := map[string]any{"path": "/var/lib/katalog/media/x.mkv", "type": "movie", "title": "Film"}
		for k, v := range more {
			body[k] = v
		}
		return body
	}
	tmdb := map[string]any{"tmdb": "97546"}
	for _, c := range []struct {
		body map[string]any
		says string
	}{
		{ep(nil), "orphan"},
		{ep(map[string]any{"series": map[string]any{}}), "orphan"},
		{ep(map[string]any{"series": map[string]any{"tmdb": ""}}), "orphan"},
		{ep(map[string]any{"series": tmdb, "parentId": "p"}), "names its series one way"},
		{ep(map[string]any{"series": map[string]any{"tmdb": 97546}}), "series.tmdb is an id, given as a string"},
		{ep(map[string]any{"series": map[string]any{"tmdb": "97546a"}}), `series.tmdb is digits, not "97546a"`},
		{ep(map[string]any{"series": map[string]any{"imdb": "0944947"}}), `series.imdb is tt and digits, not "0944947"`},
		{ep(map[string]any{"series": map[string]any{"wikidata": "Q1"}}), `series takes the ids of tmdb, tvdb and imdb, not "wikidata"`},
		{ep(map[string]any{"series": tmdb, "identity": map[string]any{"tvdb": "x"}}), `identity.tvdb is digits, not "x"`},
		{ep(map[string]any{"series": tmdb, "seasonNumber": -1}), "0 or more"},
		{ep(map[string]any{"series": tmdb, "episodeEnd": 1}), "episodeEnd is the last episode the file covers"},
		{ep(map[string]any{"series": tmdb, "episodeEnd": 12}), "2 to 11, as one file covers 10 episodes at most"},
		{movie(map[string]any{"series": tmdb}), "series names the series of an episode"},
		{movie(map[string]any{"episodeEnd": 3}), "episodeEnd is the last episode one file of several covers"},
		{map[string]any{"path": "/var/lib/katalog/media/x.flac", "type": "track", "title": "T", "identity": tmdb},
			"identity names a movie or an episode"},
	} {
		raw, _ := json.Marshal(c.body)
		w := httptest.NewRecorder()
		h.ingest(w, httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(raw)))
		var answer struct{ Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &answer)
		if w.Code != http.StatusBadRequest || !strings.Contains(answer.Error, c.says) {
			t.Errorf("%v: %d %s, want 400 saying %q", c.body, w.Code, w.Body.String(), c.says)
		}
	}
}

// The 400 through the service: an episode handed over with neither its
// series' catalog id nor its identity makes nothing and sends nothing.
func TestAnEpisodeWithoutItsSeriesMakesNothing(t *testing.T) {
	f := newIngester(t, false)
	code, answer := f.ingest(t, map[string]any{"path": f.file(t, "Show/S01E01.mkv"), "type": "episode", "title": "Show",
		"seasonNumber": 1, "episodeNumber": 1})
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(answer["error"]), "orphan") {
		t.Errorf("%d %v, want 400", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 0 {
		t.Errorf("%d items made", n)
	}
	if msgs := f.k.take(); len(msgs) != 0 {
		t.Errorf("sent %v", msgs)
	}
}

// A movie handed over with its identity arrives matched: a row for each of
// its ids (an IMDb id's letters in lower case), which its discovered event
// names and its worker record reads its TMDB id from; the answer is the
// movie's alone.
func TestAMovieArrivesWithItsIdentity(t *testing.T) {
	f := newIngester(t, false)
	code, answer := f.ingest(t, map[string]any{"path": f.file(t, "Film (2024)/Film (2024).mkv"), "type": "movie", "title": "Film",
		"year": 2024, "identity": map[string]any{"tmdb": "12345", "imdb": "TT0111161"}})
	id, _ := answer["itemId"].(string)
	if code != http.StatusOK || answer["created"] != true || keysOf(answer) != "created itemId" {
		t.Fatalf("a movie with its identity: %d %v", code, answer)
	}
	if got := externalIDs(t, f.st, id); got != "imdb tt0111161, tmdb 12345" {
		t.Errorf("the movie's ids: %s", got)
	}
	sent := f.sent(t, events.TopicDiscovered)
	if len(sent) != 1 || sent[0].ItemID != id || sent[0].Type != "movie" ||
		fmt.Sprint(sent[0].ExternalIDs) != "map[imdb:tt0111161 tmdb:12345]" || sent[0].SeasonNumber != nil {
		t.Errorf("discovered %+v", sent)
	}
	if rec := recordOf(t, f.h, f.svc, "/api/analyze/items/"+id); rec["movieTmdbId"] != "12345" {
		t.Errorf("the worker record reads TMDB id %v", rec["movieTmdbId"])
	}
}

// The catalog id path is as it was: an episode handed over with its parent
// and numbers is made under that parent, answered {itemId, created} and
// nothing more, and again created false; a second file of the same numbers
// is another episode, refused nothing; a movie without an identity gets no
// ids. Their discovered events name what the catalog holds of them: the
// episode's numbers and its series' ids.
func TestTheCatalogIDPathIsUnchanged(t *testing.T) {
	f := newIngester(t, false)
	const series = "5e500000-0000-4000-8000-000000000001"
	storetest.AddItem(t, f.st, series, "series", "Show", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES ('x', $1, 'tmdb', '1')`,
		series)
	body := map[string]any{"path": f.file(t, "Show/Show.S01E01.mkv"), "type": "episode", "title": "Show", "parentId": series,
		"seasonNumber": 1, "episodeNumber": 1}
	code, answer := f.ingest(t, body)
	id, _ := answer["itemId"].(string)
	if code != http.StatusOK || answer["created"] != true || keysOf(answer) != "created itemId" {
		t.Fatalf("an episode by its parent: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND parent_id = $2
		AND seasonnumber = 1 AND episodenumber = 1 AND title = 'Show' AND sorttitle = 'Show'`, id, series); n != 1 {
		t.Error("the episode is not made under its parent as before")
	}
	sent := f.sent(t, events.TopicDiscovered)
	if len(sent) != 1 || sent[0].ItemID != id || sent[0].Source != "ingest" || sent[0].ExternalIDs != nil ||
		fmt.Sprint(sent[0].SeriesExternalIDs) != "map[tmdb:1]" || *sent[0].SeasonNumber != 1 || *sent[0].EpisodeNumber != 1 {
		t.Errorf("discovered %+v", sent)
	}
	code, answer = f.ingest(t, body)
	if code != http.StatusOK || answer["created"] != false || answer["itemId"] != id || keysOf(answer) != "created itemId" {
		t.Errorf("the episode again: %d %v", code, answer)
	}
	body["path"] = f.file(t, "Show/Show.S01E01.other.mkv")
	if code, answer = f.ingest(t, body); code != http.StatusOK || answer["created"] != true || answer["itemId"] == id {
		t.Errorf("a second file of the numbers: %d %v, want another episode, as before", code, answer)
	}
	code, answer = f.ingest(t, map[string]any{"path": f.file(t, "Film.mkv"), "type": "movie", "title": "Film"})
	if code != http.StatusOK || answer["created"] != true || keysOf(answer) != "created itemId" {
		t.Errorf("a movie: %d %v", code, answer)
	}
	if got := externalIDs(t, f.st, answer["itemId"].(string)); got != "" {
		t.Errorf("a movie without an identity has ids %s", got)
	}
}

// One file of several episodes handed over by its parent's catalog id covers
// the episodes after its first under its parent's series, as one by its
// series' identity does; a parent there is not is refused (400).
func TestAFileOfSeveralEpisodesByItsParent(t *testing.T) {
	f := newIngester(t, false)
	const series, season = "5e500000-0000-4000-8000-000000000001", "5ea50000-0000-4000-8000-000000000001"
	storetest.AddItem(t, f.st, series, "series", "Show", "")
	storetest.AddItem(t, f.st, season, "season", "Season 2", series)
	body := map[string]any{"path": f.file(t, "Show/Show.S02E01E02.mkv"), "type": "episode", "title": "Show", "parentId": season,
		"seasonNumber": 2, "episodeNumber": 1, "episodeEnd": 2}
	code, answer := f.ingest(t, body)
	covered, _ := answer["covered"].([]any)
	if code != http.StatusOK || answer["created"] != true || len(covered) != 1 || keysOf(answer) != "covered created itemId" {
		t.Fatalf("a file of two episodes under a season: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND parent_id = $2
		AND seasonnumber = 2 AND episodenumber = 2 AND coveredby = $3`, covered[0], season, answer["itemId"]); n != 1 {
		t.Error("S02E02 is not made under the season, covered by the holder")
	}
	body["path"], body["parentId"] = f.file(t, "Show/Show.S02E03E04.mkv"), "no-such-item"
	if code, answer = f.ingest(t, body); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(answer["error"]), "no item") {
		t.Errorf("a parent there is not: %d %v", code, answer)
	}
}

// The packaged event of a title names its identities, in either layout: an
// episode's own ids, its series' and its numbers once its package is
// recorded, and a movie's ids once its version is taken.
func TestThePackagedEventNamesTheIdentities(t *testing.T) {
	f := newIngester(t, false)
	body := episodeBody(f.file(t, "Show/Show.S04E07.mkv"), "Show", map[string]any{"tmdb": "97546"}, 4, 7)
	body["identity"] = map[string]any{"tmdb": "937631"}
	_, answer := f.ingest(t, body)
	id := answer["itemId"].(string)
	f.k.take()
	if w := do(f.h, http.MethodPost, "/api/items/"+id+"/packaging-complete", packagedManifest, f.svc); w.Code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %s", w.Code, w.Body.String())
	}
	sent := f.sent(t, events.TopicPackaged)
	if len(sent) != 1 || sent[0].ItemID != id || sent[0].Type != "episode" || sent[0].Status != "done" ||
		fmt.Sprint(sent[0].ExternalIDs) != "map[tmdb-episode:937631]" || fmt.Sprint(sent[0].SeriesExternalIDs) != "map[tmdb:97546]" ||
		*sent[0].SeasonNumber != 4 || *sent[0].EpisodeNumber != 7 {
		t.Errorf("packaged %+v", sent)
	}

	film := newV2Film(t)
	k := &kafkaRecorder{}
	h, iss := extrasServer(t, film.st, film.cfg, k)
	film.h, film.svc = h, iss.Service(t, "zaentrum-manager")
	storetest.Exec(t, film.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', $1, 'tmdb', '45745'), ('x2', $1, 'imdb', 'tt1727587')`, filmItem)
	complete := film.version(t, filmVersion, filmPackage)
	if code, answer := film.complete(t, film.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("the v2 packaging-complete: %d %v", code, answer)
	}
	msgs := k.take()[events.TopicPackaged]
	if len(msgs) != 1 {
		t.Fatalf("%d packaged events, want the film's", len(msgs))
	}
	var ev events.ItemEvent
	if err := json.Unmarshal(msgs[0].Value, &ev); err != nil || ev.ItemID != filmItem ||
		fmt.Sprint(ev.ExternalIDs) != "map[imdb:tt1727587 tmdb:45745]" || ev.SeriesExternalIDs != nil || ev.SeasonNumber != nil {
		t.Errorf("the film's packaged event: %s, %v", msgs[0].Value, err)
	}
}
