package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/itemactions"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// replacing wires an instance as main does: the real retries, sending
// through pub, and the item actions that give a title another file,
// encoding it again through those retries.
func replacing(pub retry.Publisher) wiring {
	return wiring{
		pipeline: func(st *store.Store) graph.Pipeline {
			return retry.New(st, processing.DefaultPolicy(), pub, time.Minute)
		},
		sources: func(st *store.Store, cfg config.Config, pipe graph.Pipeline) graph.SourceReplacer {
			return itemactions.New(st, cfg, processing.New(st.Pool()), nil).WithReencoder(pipe)
		},
	}
}

// file writes n bytes at rel under the instance's media root, and answers its
// path.
func (in *instance) file(t *testing.T, rel string, n int) string {
	t.Helper()
	p := filepath.Join(in.cfg.NFSRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A film given its original through the service as main wires it, with the
// real retries, the bearer tokens of a realm and a bus that takes every
// event, as the demo's seed Job asks it: by the path of its small copy, the
// small copy deleted. A viewer is refused, and so is the service account,
// which may not delete the file, before anything changes. For an admin the film keeps its id, its trailer
// and the language an admin set for a track; the transcoder's trigger goes,
// not as a retry; the record the workers read names the new file and the
// language; and a scan then finds the new file the film's, and takes nothing
// in. A film whose transcode runs is given its file all the same and left
// alone, saying that run is the old file's.
func TestReplaceSourceThroughTheService(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWired(t, replacing(b))
	st := in.st
	old, cur := in.file(t, "BigBuckBunny_320x180.mp4", 1000), in.file(t, "Big Buck Bunny (2008).mov", 5000)
	running, itsNew := in.file(t, "A Film Being Encoded.mkv", 100), in.file(t, "A Film Being Encoded (2020).mkv", 200)
	storetest.AddItem(t, st, "bbb", "movie", "Big Buck Bunny", "")
	storetest.AddItem(t, st, "f2", "movie", "A Film Being Encoded", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes) VALUES
		('a-bbb', 'bbb', $1, true, 'primary', 'h264', 1000),
		('a-bbb-p', 'bbb', $2, false, 'packaged', 'hvc1.1.6.L93.B0', 900),
		('a-f2', 'f2', $3, true, 'primary', 'h264', 100)`, old, filepath.Join(in.cfg.PackagesRoot, "movies", "bb", "bbb", "manifest.json"), running)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts, failures) VALUES
		('st-bbb-t', now(), now(), 'bbb', 'transcode', 'done', 1, 0), ('st-bbb-p', now(), now(), 'bbb', 'package', 'done', 1, 0),
		('st-f2-t', now(), now(), 'f2', 'transcode', 'in_progress', 1, 0)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		VALUES ('x-bbb', 'bbb', 'trailer', 'Trailer', 'api', $1)`, filepath.Join(in.cfg.ExtrasRoot, "big-buck-bunny", "trailer.mov"))
	viewer, service, admin := in.iss.Viewer(t), in.iss.Service(t, "zaentrum-manager"), in.iss.Admin(t)
	gql := func(doc string) string {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) > 0 {
			t.Fatalf("%s: %v", doc, a.Errors)
		}
		return string(a.Data)
	}
	gql(`mutation { setTrackLanguage(itemId: "bbb", kind: "audio", ordinal: 0, language: "zxx") { id } }`)

	doc := fmt.Sprintf(`mutation { replaceSource(itemPath: %s, path: %s, deleteOldFile: true) { itemId oldPath path replaced
		oldFileDeleted oldSidecars reencode { reencoded busy notSent } message } }`, jsonString(old), jsonString(cur))
	before := fingerprint(t, st)
	for token, refusal := range map[string]string{
		viewer: "forbidden: replaceSource requires the zaentrum-admin or the zaentrum-addon role, or the platform's service account",
		// The service account may give a title another file, not delete one.
		service: "forbidden: replaceSource with deleteOldFile requires the zaentrum-admin or the zaentrum-addon role",
	} {
		_, a := in.gql(t, "/api/manage/query", token, doc)
		if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" || a.Errors[0].Message != refusal {
			t.Errorf("%v, want it refused: %s", a.Errors, refusal)
		}
	}
	if sent := b.take(); sent != "" {
		t.Errorf("refused callers sent %s", sent)
	}
	if after := fingerprint(t, st); after != before || storetest.Count(t, st,
		`SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bbb' AND path = $1`, old) != 1 {
		t.Errorf("refused callers changed the catalog:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("refused callers deleted the small copy: %v", err)
	}

	want := `{"replaceSource":{"itemId":"bbb","oldPath":` + jsonString(old) + `,"path":` + jsonString(cur) +
		`,"replaced":true,"oldFileDeleted":true,"oldSidecars":0,"reencode":{"reencoded":1,"busy":0,"notSent":0},"message":` +
		jsonString("the title's file is "+cur+" now, in place of "+old+"; the old file is deleted; encoding it again: "+
			"its transcode and package wait for their workers ("+events.TopicAnalyzed+" sent); "+
			"the current package plays until the packager starts on the new one") + `}}`
	if got := gql(doc); got != want {
		t.Errorf("an admin's replace:\n %s\nwant\n %s", got, want)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" bbb transcode reencode reencode movie" {
		t.Errorf("the replace sent %q, want the transcoder's trigger, not marked as a retry", sent)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the small copy is still there: %v", err)
	}
	if code, body := in.get(t, "/api/analyze/items/bbb", service); code != http.StatusOK || !strings.Contains(body, `"path":`+jsonString(cur)) ||
		!strings.Contains(body, `"trackLanguages":[{"kind":"audio","ordinal":0,"language":"zxx"}]`) {
		t.Errorf("the record the workers read: %d %s", code, body)
	}
	if got := gql(`{ item(id: "bbb") { id title extras { id } tracks { kind ordinal languageOverride reported } } }`); got !=
		`{"item":{"id":"bbb","title":"Big Buck Bunny","extras":[{"id":"x-bbb"}],"tracks":[{"kind":"audio","ordinal":0,"languageOverride":"zxx","reported":false}]}}` {
		t.Errorf("the film after its replace: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bbb-p' AND kind = 'packaged'`); n != 1 {
		t.Error("the film's package is gone before the packager replaced it")
	}

	got := gql(fmt.Sprintf(`mutation { replaceSource(itemId: "f2", path: %s, deleteOldFile: true) { replaced oldFileDeleted
		reencode { reencoded busy } message } }`, jsonString(itsNew)))
	if !strings.HasPrefix(got, `{"replaceSource":{"replaced":true,"oldFileDeleted":true,"reencode":{"reencoded":0,"busy":1},"message":`) ||
		!strings.Contains(got, "; transcode is running: its worker last reported at ") ||
		!strings.HasSuffix(got, `; that run is the old file's: encode the title again (reencodeItem) once it is done"}}`) {
		t.Errorf("a replace of a film being encoded: %s", got)
	}
	if sent := b.take(); sent != "" {
		t.Errorf("a film being encoded was sent %s", sent)
	}

	job, err := scanner.New(st, in.cfg, processing.New(st.Pool()), nil).Trigger(context.Background(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); storetest.Count(t, st,
		`SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'running'`, job) == 1; {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'done'
		AND itemsinserted = 0 AND itemsupdated = 2`, job); n != 1 {
		t.Error("the scan took a new file in, or did not find both its title's")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE (path = $1 AND item_id = 'bbb')
		OR (path = $2 AND item_id = 'f2')`, cur, itsNew); n != 2 || storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items`) != 3 {
		t.Error("after the scan the new files are not their titles' alone, or the catalog holds another title")
	}
}

// A service without an event bus gives a title its file all the same, and
// says that nothing encodes it again, as reencodeItem does.
func TestReplaceSourceWithoutAnEventBus(t *testing.T) {
	in := newInstanceWired(t, replacing(nil))
	old, cur := in.file(t, "Sintel.2010.720p.mkv", 100), in.file(t, "Sintel (2010).mkv", 300)
	storetest.AddItem(t, in.st, "s1", "movie", "Sintel", "")
	storetest.Exec(t, in.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a-s1', 's1', $1, true)`, old)
	_, a := in.gql(t, "/api/manage/query", in.iss.Admin(t), fmt.Sprintf(`mutation { replaceSource(itemId: "s1", path: %s) {
		replaced oldFileDeleted reencode { titles reencoded message } } }`, jsonString(cur)))
	if want := `{"replaceSource":{"replaced":true,"oldFileDeleted":false,"reencode":{"titles":0,"reencoded":0,"message":` +
		`"cannot re-encode: no event bus: a retry sends the step's trigger event again, and KAFKA_BROKERS is not set"}}}`; len(a.Errors) > 0 ||
		string(a.Data) != want {
		t.Errorf("a replace without an event bus: %s %v\nwant %s", a.Data, a.Errors, want)
	}
	if n := storetest.Count(t, in.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-s1' AND path = $1`, cur); n != 1 {
		t.Error("the film does not have its new file")
	}
}

// A seed Job hands a title a better file with the platform's service account,
// as the demo's does: the title takes the new file and its encode is sent, and
// the old file stays on disk, for the Job to delete — deleting one is an
// admin's.
func TestTheServiceAccountGivesATitleAnotherFileAndDeletesNone(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWired(t, replacing(b))
	st := in.st
	old, cur := in.file(t, "Sintel.2010.720p.mkv", 1000), in.file(t, "Sintel (2010).mkv", 5000)
	storetest.AddItem(t, st, "sintel", "movie", "Sintel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes) VALUES
		('a-sintel', 'sintel', $1, true, 'primary', 'h264', 1000)`, old)

	_, a := in.gql(t, "/api/manage/query", in.iss.Service(t, "zaentrum-manager"), fmt.Sprintf(
		`mutation { replaceSource(itemPath: %s, path: %s) { itemId replaced oldFileDeleted } }`, jsonString(old), jsonString(cur)))
	if len(a.Errors) > 0 || string(a.Data) != `{"replaceSource":{"itemId":"sintel","replaced":true,"oldFileDeleted":false}}` {
		t.Fatalf("the service account's replace: %s %v", a.Data, a.Errors)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-sintel' AND path = $1`, cur); n != 1 {
		t.Errorf("the title's file is not the new one (%d rows)", n)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the old file went, which only an admin may have done: %v", err)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" sintel transcode reencode reencode movie" {
		t.Errorf("the replace sent %q, want the transcoder's trigger", sent)
	}
}

// An addon that grabbed a better file of a title the library holds, for an
// admin who asked it, hands it over as acquire does with the library's v2
// layout: from .work/replace, with its own service account, the old file
// deleted, the title encoded again, asked only while the title is idle.
// While the title's transcode runs it is refused with SOURCE_BUSY and nothing
// changes: the title keeps its old file, the new one waits in .work/replace,
// nothing is sent. Once the transcode is done the title takes the new file
// into the arrivals, the old file stays where it lies, superseded by the new
// one, and the title's encode is sent; a scan then takes nothing in, the old
// file no title of its own — even one recorded without its quick hash.
func TestAnAddonReplacesAHeldTitlesFileWhileItIsIdle(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWired(t, replacing(b))
	st := in.st
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('set-layout', 'library.layout', 'v2')`)
	roots := in.cfg.Roots(true)
	write := func(p string, n int) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := write(filepath.Join(roots.Arrivals, "A Film (1999).mkv"), 100)
	handed := write(filepath.Join(roots.Work, "replace", "acquire", "tg_1", "A.Film.1999.1080p.BluRay-G.mkv"), 500)
	taken := filepath.Join(roots.Arrivals, "A.Film.1999.1080p.BluRay-G.mkv")
	storetest.AddItem(t, st, "film", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('s-old', 'film', 'A Film (1999).mkv', $1, 100, 'present')`, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes, sourceid)
		VALUES ('a-film', 'film', $1, true, 'primary', 100, 's-old')`, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts, failures) VALUES
		('st-film-t', now(), now(), 'film', 'transcode', 'in_progress', 1, 0), ('st-film-p', now(), now(), 'film', 'package', 'pending', 0, 0)`)
	doc := fmt.Sprintf(`mutation { replaceSource(itemId: "film", path: %s, deleteOldFile: true, reencode: true, ifIdle: true) {
		itemId oldPath path replaced oldFileDeleted oldFileSuperseded reencode { reencoded busy } message } }`, jsonString(handed))
	addon := in.iss.Addon(t)

	// An addon the catalog does not name is refused, whatever its role.
	before := fingerprint(t, st)
	_, a := in.gql(t, "/api/manage/query", addon, doc)
	if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" ||
		!strings.Contains(a.Errors[0].Message, "library.replace.clients names (it names none, and not example-addon)") {
		t.Fatalf("a replace by an addon the catalog does not name: %s %v", a.Data, a.Errors)
	}
	if after := fingerprint(t, st); after != before {
		t.Errorf("a forbidden replace changed the catalog:\nbefore %s\nafter  %s", before, after)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('set-clients', 'library.replace.clients',
		'example-addon')`)

	before = fingerprint(t, st)
	_, a = in.gql(t, "/api/manage/query", addon, doc)
	if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "SOURCE_BUSY" ||
		!strings.HasPrefix(a.Errors[0].Message, "the title is given no other file while it is encoded (ifIdle): transcode is running") {
		t.Fatalf("a replace while the title's transcode runs: %s %v", a.Data, a.Errors)
	}
	if after := fingerprint(t, st); after != before || storetest.Count(t, st,
		`SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-film' AND path = $1 AND sourceid = 's-old'`, old) != 1 {
		t.Errorf("a busy title's replace changed the catalog:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("a busy title's old file went: %v", err)
	}
	if _, err := os.Stat(handed); err != nil {
		t.Errorf("the file handed over left .work/replace: %v", err)
	}
	if _, err := os.Stat(taken); !os.IsNotExist(err) {
		t.Errorf("the file handed over is in the arrivals: %v", err)
	}
	if sent := b.take(); sent != "" {
		t.Errorf("a busy title's replace sent %s", sent)
	}

	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done' WHERE item_id = 'film'`)
	_, a = in.gql(t, "/api/manage/query", addon, doc)
	if len(a.Errors) > 0 {
		t.Fatalf("the replace once the title is idle: %v", a.Errors)
	}
	want := `{"replaceSource":{"itemId":"film","oldPath":` + jsonString(old) + `,"path":` + jsonString(taken) +
		`,"replaced":true,"oldFileDeleted":false,"oldFileSuperseded":true,"reencode":{"reencoded":1,"busy":0},"message":`
	if !strings.HasPrefix(string(a.Data), want) {
		t.Errorf("the replace once the title is idle:\n %s\nwant\n %s…", a.Data, want)
	}
	if sent := b.take(); sent != events.TopicAnalyzed+" film transcode reencode reencode movie" {
		t.Errorf("the replace sent %q, want the transcoder's trigger", sent)
	}
	if _, err := os.Stat(handed); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", handed, err)
	}
	for _, p := range []string{old, taken} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is not there: %v", p, err)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = 's-old' AND state = 'superseded'
		AND arrivalpath = $1`, old); n != 1 {
		t.Error("the old original is not superseded where it lies")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_itemsources s
		ON s.id = p.sourceid WHERE p.id = 'a-film' AND p.path = $1 AND s.state = 'present'`, taken); n != 1 {
		t.Error("the title's asset is not the new file, a source of its own")
	}

	job, err := scanner.New(st, in.cfg, processing.New(st.Pool()), nil).Trigger(context.Background(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); storetest.Count(t, st,
		`SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'running'`, job) == 1; {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'done'
		AND itemsinserted = 0`, job); n != 1 || storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id <> 'm1'`) != 1 {
		t.Error("the scan took a title in after the replace")
	}
}
