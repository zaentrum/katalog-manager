package rest

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// sent records the events the retries send.
type sent struct {
	mu   sync.Mutex
	msgs []events.Message
}

func (s *sent) Enabled() bool { return true }

func (s *sent) Publish(_ context.Context, msgs []events.Message) []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, msgs...)
	return make([]error, len(msgs))
}

func (s *sent) take() []events.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.msgs
	s.msgs = nil
	return out
}

// A transcode the transcoder refused, reported as its step's end, has the
// title taken in at once with the v2 layout: its take-in sent to the packager
// (transcoded, its step takein), whose worker record then takes the title
// in, its original named original.mkv. So has a package that failed with no
// attempt left; one with an attempt left is retried as before. With the
// legacy layout nothing is taken in.
func TestATitleThatGetsNoPackageIsTakenIn(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	cfg := v2Config(dir)
	bus := &sent{}
	h, iss := serverWith(t, st, cfg, bus)
	svc := iss.Service(t, "zaentrum-manager")
	files(t, dir, map[string]int64{".work/incoming/A Film (2024).mkv": 100, ".work/incoming/Another Film (2024).m2ts": 100})
	const other = "f2f2f2f2-0000-4000-8000-000000000002"
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	storetest.AddItem(t, st, other, "movie", "Another Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('src-f1', $1, $2, true), ('src-f2', $3, $4, true)`, filmItem, cfg.ArrivalsRoot+"/A Film (2024).mkv", other,
		cfg.ArrivalsRoot+"/Another Film (2024).m2ts")
	report := func(item, step, body string) {
		t.Helper()
		if w := do(h, http.MethodPut, "/api/analyze/items/"+item+"/steps/"+step, body, svc); w.Code != http.StatusOK {
			t.Fatalf("PUT %s %s: %d %s", item, step, w.Code, w.Body.String())
		}
	}
	refused := `{"status": "failed", "error": "Dolby Vision profile 5 needs a tone-mapping encode; kept the original"}`
	report(filmItem, "transcode", refused)
	if got := bus.take(); len(got) != 0 {
		t.Errorf("taken in with the legacy layout: %v", got)
	}
	v2Layout(t, st)
	// The workers read the record first, which gives each title its source.
	recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	recordOf(t, h, svc, "/api/analyze/items/"+other)
	report(filmItem, "transcode", refused)
	got := bus.take()
	if len(got) != 1 || got[0].Topic != events.TopicTranscoded || got[0].Event.ItemID != filmItem || got[0].Event.Step != "takein" {
		t.Fatalf("the refused title's take-in: %+v", got)
	}
	lib, _ := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)["library"].(map[string]any)
	if b, _ := lib["build"].(map[string]any); b["mode"] != "takein" || b["originalName"] != "original.mkv" {
		t.Errorf("the worker record of a title taken in: %v", b)
	}

	report(other, "transcode", `{"status": "done"}`)
	report(other, "package", `{"status": "failed", "error": "no space left"}`)
	if got := bus.take(); len(got) != 0 {
		t.Errorf("a package with attempts left taken in: %v", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'in_progress', failures = 2
		WHERE item_id = $1 AND step = 'package'`, other)
	report(other, "package", `{"status": "failed", "error": "no space left"}`)
	if got := bus.take(); len(got) != 1 || got[0].Event.ItemID != other || got[0].Event.Step != "takein" {
		t.Errorf("the exhausted package's take-in: %+v", got)
	}
	lib, _ = recordOf(t, h, svc, "/api/analyze/items/"+other)["library"].(map[string]any)
	if b, _ := lib["build"].(map[string]any); b["mode"] != "takein" || b["originalName"] != "original.m2ts" {
		t.Errorf("the worker record of the exhausted title: %v", b)
	}
}

// A title encoded again (reencodeItem; the re-encode queue sends a title
// the same way) whose original lies in the folder of its packaged version is
// packaged anew: its transcode's end makes a new version, and the worker
// record's run is repackage, its original read where it lies, in the older
// version's folder, and renamed nowhere.
func TestAReencodeRepackagesFromTheOriginalWhereItLies(t *testing.T) {
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	itemDir := dir + "/movies/f1/" + filmItem
	const older = "0a0a0a0a-0000-4000-8000-000000000001"
	original := library.VersionDir(itemDir, older) + "/original.mkv"
	files(t, dir, map[string]int64{"movies/f1/" + filmItem + "/versions/" + older + "/original.mkv": 500})
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state, recordedat)
		VALUES ('0b0b0b0b-0000-4000-8000-000000000002', $1, 'original.mkv', $2, 500, 'present', now())`, filmItem, original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sourceid)
		VALUES ('src-f1', $1, $2, true, '0b0b0b0b-0000-4000-8000-000000000002')`, filmItem, original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY['0b0b0b0b-0000-4000-8000-000000000002'], 'complete', $1, $3, now())`, older, filmItem,
		library.VersionDir(itemDir, older))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status) VALUES
		('t', $1, 'transcode', 'done'), ('p', $1, 'package', 'done')`, filmItem)

	bus := &sent{}
	r := retry.New(st, processing.DefaultPolicy(), bus, 0)
	res, err := r.ReencodeItem(context.Background(), filmItem)
	if err != nil || res.Reencoded != 1 {
		t.Fatalf("ReencodeItem: %+v, %v", res, err)
	}
	if got := bus.take(); len(got) != 1 || got[0].Topic != events.TopicAnalyzed {
		t.Errorf("the transcoder's trigger: %+v", got)
	}
	rec := recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	if rec["path"] != original {
		t.Errorf("the transcoder reads %v, want the original in the older version's folder %s", rec["path"], original)
	}
	if w := do(h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/transcode", `{"status": "done"}`, svc); w.Code != http.StatusOK {
		t.Fatalf("the transcode's end: %d %s", w.Code, w.Body.String())
	}
	rec = recordOf(t, h, svc, "/api/analyze/items/"+filmItem)
	lib, _ := rec["library"].(map[string]any)
	b, _ := lib["build"].(map[string]any)
	vid, _ := b["versionId"].(string)
	cur, _ := lib["current"].(map[string]any)
	if rec["path"] != original || b["mode"] != "repackage" || b["originalName"] != nil || vid == older ||
		b["versionDir"] != library.VersionDir(itemDir, vid) || cur["versionId"] != older {
		t.Errorf("the packager's record: path %v, build %v, current %v", rec["path"], b, cur)
	}
}

// The packager reports its take-in at PUT /api/analyze/items/{id}/steps/takein,
// as any worker reports its step: the step runs, then is done.
func TestThePackagerReportsItsTakeIn(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, v2Config(t.TempDir()))
	svc := iss.Service(t, "zaentrum-manager")
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	for _, status := range []string{"in_progress", "done"} {
		if w := do(h, http.MethodPut, "/api/analyze/items/"+filmItem+"/steps/takein", `{"status": "`+status+`"}`, svc); w.Code != http.StatusOK {
			t.Fatalf("PUT the take-in %s: %d %s", status, w.Code, w.Body.String())
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = 'takein'
		AND status = 'done' AND attempts = 1`, filmItem); n != 1 {
		t.Error("the take-in's report is not recorded as its step's")
	}
}
