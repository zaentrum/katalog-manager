package extras

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	kafka "github.com/segmentio/kafka-go"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The catalog the tests take extras in for: a film with its file and its TMDB
// id, a series with episodes in seasons 1 and 2, one of them, and an album.
const (
	film    = "ea886f9b-0d06-4f0f-babb-d2a1162f9b01"
	series  = "5e5e5e5e-0000-4000-8000-000000000001"
	episode = "e1e1e1e1-0000-4000-8000-000000000002"
	album   = "a1a1a1a1-0000-4000-8000-000000000003"
)

var policy = processing.Policy{MaxAttempts: 3, Backoff: time.Minute, BackoffMax: time.Hour,
	Timeouts: map[string]time.Duration{}, DefaultTimeout: 2 * time.Hour}

// writer stands in for the Kafka writer: it records what it is asked to
// write.
type writer struct {
	mu   sync.Mutex
	msgs []kafka.Message
}

func (w *writer) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, msgs...)
	return nil
}

func (w *writer) Close() error { return nil }

// refusing is a bus that refuses every trigger at once (the producer's own
// retries of a refused write take seconds).
type refusing struct{}

func (refusing) Enabled() bool { return true }

func (refusing) PublishExtras(_ context.Context, msgs []events.ExtraMessage) []error {
	errs := make([]error, len(msgs))
	for i := range errs {
		errs[i] = errors.New("dial tcp: connection refused")
	}
	return errs
}

// take returns what was written since the last take.
func (w *writer) take() []kafka.Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	m := w.msgs
	w.msgs = nil
	return m
}

type fixture struct {
	st  *store.Store
	cfg config.Config
	w   *writer
	svc *Service
	dir string
}

// write makes a file of n bytes under the fixture's folder.
func (f *fixture) write(t *testing.T, rel string, n int) string {
	t.Helper()
	path := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(data)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	events.Configure("stube.")
	st := storetest.Open(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LegacyLibraryRoot: dir + "/library",
		ExtrasRoot: dir + "/extras"}
	w := &writer{}
	f := &fixture{st: st, cfg: cfg, w: w, svc: New(st, cfg, policy, events.ProducerOn(w)), dir: dir}
	storetest.AddItem(t, st, film, "movie", "Big Buck Bunny", "")
	storetest.AddItem(t, st, series, "series", "Pioneer One", "")
	storetest.AddItem(t, st, episode, "episode", "Earthfall", series)
	storetest.AddItem(t, st, "e2e2e2e2-0000-4000-8000-000000000004", "episode", "The Man From Mars", series)
	storetest.AddItem(t, st, album, "album", "An Album", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1 WHERE id = $1`, episode)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 2 WHERE id = 'e2e2e2e2-0000-4000-8000-000000000004'`)
	main := f.write(t, "media/BigBuckBunny_320x180.mp4", 1000)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a-film', $1, $2, true)`, film, main)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES ('x-film', $1, 'tmdb', '10378')`, film)
	return f
}

var admin = &auth.Principal{Subject: "admin-1"}

func asAdmin() context.Context { return auth.WithPrincipal(context.Background(), admin) }

// add takes in the file at rel as a trailer of the film.
func (f *fixture) add(t *testing.T, rel string) *model.Extra {
	t.Helper()
	path := f.write(t, rel, 2000)
	res, err := f.svc.AddExtra(asAdmin(), graph.AddExtraRequest{ItemID: film, Path: path, Kind: "trailer"})
	if err != nil || !res.Created {
		t.Fatalf("AddExtra %s: %+v, %v", rel, res, err)
	}
	return res.Extra
}

// triggers are the triggers written since the last take, by extra.
func (f *fixture) triggers(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, m := range f.w.take() {
		if m.Topic != "stube.catalog.extra.queued" {
			t.Errorf("a message to %s", m.Topic)
		}
		var v map[string]any
		if err := json.Unmarshal(m.Value, &v); err != nil {
			t.Fatal(err)
		}
		if v["extraId"] != string(m.Key) {
			t.Errorf("a trigger keyed %s for %v", m.Key, v["extraId"])
		}
		out[string(m.Key)] = v
	}
	return out
}

func refusal(t *testing.T, err error) *graph.ExtraRefused {
	t.Helper()
	var r *graph.ExtraRefused
	if !errors.As(err, &r) {
		t.Fatalf("%v, want a refusal", err)
	}
	return r
}

// The quick hash is the library's: sha256 of the first 64 KiB, the last 64
// KiB of a file larger than that, and the size as a big-endian uint64.
func TestQH1IsTheLibrarysQuickHash(t *testing.T) {
	f := &fixture{dir: t.TempDir()}
	for _, n := range []int{0, 10, 65536, 65537, 100000, 131072, 300000} {
		path := f.write(t, "f.bin", n)
		data, _ := os.ReadFile(path)
		h := sha256.New()
		h.Write(data[:min(n, 65536)])
		if n > 65536 {
			h.Write(data[n-65536:])
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(n))
		h.Write(size[:])
		want := "sha256:" + hex.EncodeToString(h.Sum(nil))
		gotSize, got, err := QH1(path)
		if err != nil || gotSize != int64(n) || got != want {
			t.Errorf("%d bytes: %d %s %v, want %s", n, gotSize, got, err, want)
		}
	}
	if _, _, err := QH1(filepath.Join(f.dir, "none")); err == nil {
		t.Error("the quick hash of no file")
	}
}

// A file is taken in as an extra of the film: waiting to be sent, then queued
// as its trigger goes at once, keyed by the extra, naming the film, the
// kind and the transcode and no item, with its size and quick hash stored
// and the admin who took it in. The same file again is that extra, and
// nothing more is sent.
func TestAddExtraTakesAFileInAndSendsItsTrigger(t *testing.T) {
	f := newFixture(t)
	path := f.write(t, "extras/big-buck-bunny/trailer.mov", 70000)
	lang := "en"
	res, err := f.svc.AddExtra(asAdmin(), graph.AddExtraRequest{ItemID: film, Path: path, Kind: " Trailer ", Language: &lang})
	if err != nil || !res.Created {
		t.Fatalf("AddExtra: %+v, %v", res, err)
	}
	x := res.Extra
	_, qh1, _ := QH1(path)
	if x.ItemID != film || x.Kind != "trailer" || x.Title != "Trailer" || *x.Language != "en" || *x.SourcePath != path ||
		*x.SourceSize != 70000 || *x.SourceQH1 != qh1 || x.RegisteredBy != "api" || x.State != "queued" ||
		x.DispatchedAt == nil || *x.CreatedBy != "admin-1" {
		t.Errorf("the extra: %+v", *x)
	}
	msgs := f.w.take()
	if len(msgs) != 1 || msgs[0].Topic != "stube.catalog.extra.queued" || string(msgs[0].Key) != x.ID {
		t.Fatalf("written: %v", msgs)
	}
	var v map[string]any
	if err := json.Unmarshal(msgs[0].Value, &v); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, " ") != "eventId extraId kind occurredAt parentId source status step type" || v["extraId"] != x.ID ||
		v["parentId"] != film || v["type"] != "extra" || v["kind"] != "trailer" || v["step"] != "transcode" ||
		v["status"] != "queued" || v["source"] != "api" {
		t.Errorf("the trigger: %s", msgs[0].Value)
	}
	again, err := f.svc.AddExtra(asAdmin(), graph.AddExtraRequest{ItemID: film, Path: path + "/../trailer.mov", Kind: "teaser"})
	if err != nil || again.Created || again.Extra.ID != x.ID || again.Extra.Kind != "trailer" {
		t.Errorf("the same file again: %+v, %v", again, err)
	}
	if msgs := f.w.take(); len(msgs) != 0 {
		t.Errorf("the same file again sent %d triggers", len(msgs))
	}
}

// The title is named one way: by its id, by the path of its file, or by its
// TMDB id and type, a movie or a series. One there is not is not found; two
// ways, or none, are refused; a TMDB id two titles share names none.
func TestAddExtraNamesItsTitleOneWay(t *testing.T) {
	f := newFixture(t)
	ctx := asAdmin()
	tmdb := int64(10378)
	for i, req := range []graph.AddExtraRequest{
		{ItemPath: f.cfg.NFSRoot + "/BigBuckBunny_320x180.mp4"},
		{TmdbID: &tmdb, ItemType: "movie"},
		{TmdbID: &tmdb, ItemType: " Movie "},
	} {
		req.Path, req.Kind = f.write(t, "extras/bbb/"+string(rune('a'+i))+".mkv", 100), "teaser"
		res, err := f.svc.AddExtra(ctx, req)
		if err != nil || res.Extra.ItemID != film {
			t.Errorf("%+v: %+v, %v", req, res, err)
		}
	}
	path := f.write(t, "extras/bbb/z.mkv", 100)
	unknown := int64(1)
	for _, c := range []struct {
		req    graph.AddExtraRequest
		status int
		says   string
	}{
		{graph.AddExtraRequest{}, 400, "name the title one way"},
		{graph.AddExtraRequest{ItemID: film, ItemPath: f.cfg.NFSRoot + "/BigBuckBunny_320x180.mp4"}, 400, "name the title one way"},
		{graph.AddExtraRequest{ItemID: "dddddddd-0000-4000-8000-000000000009"}, 404, "unknown item: dddddddd-0000-4000-8000-000000000009"},
		{graph.AddExtraRequest{ItemPath: f.cfg.NFSRoot + "/Other.mp4"}, 404, "no item has the file"},
		{graph.AddExtraRequest{TmdbID: &unknown, ItemType: "movie"}, 404, "no movie has the TMDB id 1"},
		{graph.AddExtraRequest{TmdbID: &tmdb, ItemType: "series"}, 404, "no series has the TMDB id 10378"},
		{graph.AddExtraRequest{TmdbID: &tmdb, ItemType: "episode"}, 400, "itemType is movie or series"},
		{graph.AddExtraRequest{TmdbID: &tmdb}, 400, "itemType is movie or series"},
	} {
		c.req.Path, c.req.Kind = path, "trailer"
		_, err := f.svc.AddExtra(ctx, c.req)
		if r := refusal(t, err); r.Status != c.status || !strings.Contains(r.Message, c.says) {
			t.Errorf("%+v: %d %q, want %d saying %q", c.req, r.Status, r.Message, c.status, c.says)
		}
	}
	storetest.AddItem(t, f.st, "ffffffff-0000-4000-8000-000000000005", "movie", "Big Buck Bunny", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid)
		VALUES ('x-twin', 'ffffffff-0000-4000-8000-000000000005', 'tmdb', '10378')`)
	_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{TmdbID: &tmdb, ItemType: "movie", Path: path, Kind: "trailer"})
	if r := refusal(t, err); r.Status != http.StatusConflict || !strings.Contains(r.Message, "the title is not one") {
		t.Errorf("a TMDB id two films share: %d %s", r.Status, r.Message)
	}
}

// An extra is a movie's or a series' (an episode has none, its series has),
// of a kind of the list, in a language as the library writes one; a season
// is named only for a series, one it has episodes of. Its file is an
// existing video file under the media root, the library or the extras' root,
// never under the package store, never one that leads out of the roots, and
// no title's own file. Nothing refused is taken in.
func TestAddExtraRefusesWhatIsNoExtra(t *testing.T) {
	f := newFixture(t)
	ctx := asAdmin()
	video := f.write(t, "extras/x/trailer.mkv", 100)
	outside := filepath.Join(f.dir, "elsewhere", "secret.mkv")
	f.write(t, "elsewhere/secret.mkv", 100)
	if err := os.Symlink(outside, filepath.Join(f.cfg.ExtrasRoot, "x", "link.mkv")); err != nil {
		t.Fatal(err)
	}
	f.write(t, "packages/extras/aa/staged.mkv", 100)
	f.write(t, "extras/x/notes.txt", 10)
	if err := os.MkdirAll(filepath.Join(f.cfg.ExtrasRoot, "x", "folder.mkv"), 0o755); err != nil {
		t.Fatal(err)
	}
	season := func(n int32) *int32 { return &n }
	lang := func(l string) *string { return &l }
	for _, c := range []struct {
		req  graph.AddExtraRequest
		says string
	}{
		{graph.AddExtraRequest{ItemID: episode, Path: video, Kind: "trailer"}, "is an episode, and an episode has no extras: its series has"},
		{graph.AddExtraRequest{ItemID: album, Path: video, Kind: "trailer"}, "is a album: only a movie or a series has extras"},
		{graph.AddExtraRequest{ItemID: film, Path: video, Kind: "trailer", SeasonNumber: season(1)}, "a movie's extra names no season"},
		{graph.AddExtraRequest{ItemID: series, Path: video, Kind: "trailer", SeasonNumber: season(3)}, "has no episode in season 3"},
		{graph.AddExtraRequest{ItemID: series, Path: video, Kind: "trailer", SeasonNumber: season(-1)}, "a season is 0 or more"},
		{graph.AddExtraRequest{ItemID: film, Path: video, Kind: "clip"}, `an extra's kind is one of featurette, behind-the-scenes`},
		{graph.AddExtraRequest{ItemID: film, Path: video, Kind: "trailer", Language: lang("English")}, `not "English"`},
		{graph.AddExtraRequest{ItemID: film, Path: "extras/x/trailer.mkv", Kind: "trailer"}, "is no absolute path"},
		{graph.AddExtraRequest{ItemID: film, Path: "", Kind: "trailer"}, "path is required"},
		{graph.AddExtraRequest{ItemID: film, Path: outside, Kind: "trailer"}, "is not under the media root, the library's or the extras' folder or EXTRAS_ROOT"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot + "/../elsewhere/secret.mkv", Kind: "trailer"}, "is not under"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot, Kind: "trailer"}, "is not under"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.PackagesRoot + "/extras/aa/staged.mkv", Kind: "trailer"}, "is not under"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot + "/x/link.mkv", Kind: "trailer"}, "leads out of the media root"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot + "/x/gone.mkv", Kind: "trailer"}, "there is no file at"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot + "/x/folder.mkv", Kind: "trailer"}, "is no file"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.ExtrasRoot + "/x/notes.txt", Kind: "trailer"}, "is no video file (.avi, .m2ts"},
		{graph.AddExtraRequest{ItemID: film, Path: f.cfg.NFSRoot + "/BigBuckBunny_320x180.mp4", Kind: "trailer"}, "is the file of item " + film},
	} {
		_, err := f.svc.AddExtra(ctx, c.req)
		if r := refusal(t, err); r.Status != http.StatusBadRequest || r.Code != "EXTRA_REFUSED" || !strings.Contains(r.Message, c.says) {
			t.Errorf("%+v: %d %s %q, want 400 saying %q", c.req, r.Status, r.Code, r.Message, c.says)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemextras`); n != 0 {
		t.Errorf("%d extras taken in by refusals", n)
	}
	// What is: a series' season it has, the library's and the media root's
	// files, a language as BCP 47 or an ISO 639-2 code.
	for _, req := range []graph.AddExtraRequest{
		{ItemID: series, Path: f.write(t, "extras/p1/s1.mkv", 10), Kind: "featurette", SeasonNumber: season(1), Language: lang("pt-BR")},
		{ItemID: series, Path: f.write(t, "library/series/5e/p1/extras/x/teaser.webm", 10), Kind: "teaser", Language: lang("eng")},
		{ItemID: film, Path: f.write(t, "media/Big Buck Bunny-making-of.mkv", 10), Kind: "making-of", Title: "How it was made"},
	} {
		if res, err := f.svc.AddExtra(ctx, req); err != nil || !res.Created {
			t.Errorf("%+v: %+v, %v", req, res, err)
		}
	}
}

// The same file for another title is refused with the extra it is.
func TestAddExtraOfAnotherTitlesFileIsAConflict(t *testing.T) {
	f := newFixture(t)
	x := f.add(t, "extras/bbb/trailer.mov")
	_, err := f.svc.AddExtra(asAdmin(), graph.AddExtraRequest{ItemID: series, Path: *x.SourcePath, Kind: "trailer"})
	r := refusal(t, err)
	if r.Status != http.StatusConflict || r.Code != "EXTRA_CONFLICT" || r.Extra == nil || r.Extra.ID != x.ID ||
		r.Extensions()["extraId"] != x.ID || r.Extensions()["itemId"] != film {
		t.Errorf("the file for another title: %+v, %v", r, r.Extensions())
	}
}

// Without an event bus an extra is taken in and waits, pending; the sweep
// sends it once there is one. A catalog without migration 039 takes none in.
func TestAnExtraWaitsWithoutABus(t *testing.T) {
	f := newFixture(t)
	f.svc.pub = nil
	x := f.add(t, "extras/bbb/trailer.mov")
	if x.State != "pending" || x.DispatchedAt != nil {
		t.Errorf("without a bus: %s", x.State)
	}
	if n, err := f.svc.SendDue(context.Background()); err != nil || n != 0 {
		t.Errorf("SendDue without a bus: %d, %v", n, err)
	}
	f.svc.pub = events.ProducerOn(f.w)
	if n, err := f.svc.SendDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("SendDue: %d, %v", n, err)
	}
	if tr := f.triggers(t)[x.ID]; tr == nil || tr["status"] != "queued" || tr["source"] != "api" {
		t.Errorf("the trigger the sweep sent: %v", tr)
	}
	storetest.Exec(t, f.st, `DROP TABLE com_nalet_katalog_itemextras`)
	_, err := f.svc.AddExtra(asAdmin(), graph.AddExtraRequest{ItemID: film, Path: f.write(t, "extras/y.mkv", 10), Kind: "trailer"})
	if r := refusal(t, err); r.Status != http.StatusServiceUnavailable || !strings.Contains(r.Message, "039_item_extras.sql") {
		t.Errorf("without the table: %d %s", r.Status, r.Message)
	}
	if n, err := f.svc.SendDue(context.Background()); err != nil || n != 0 {
		t.Errorf("SendDue without the table: %d, %v", n, err)
	}
}

// line is an extra's packaging: "state failures error retry sent".
func line(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT state || ' ' || failures || ' ' || COALESCE(error, '-') ||
		' retry=' || (nextretryat IS NOT NULL) || ' sent=' || (dispatchedat IS NOT NULL)
		FROM com_nalet_katalog_itemextras WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A trigger that could not be sent puts its extra back, pending, sent again
// a backoff later, no failure counted; a retry after a failed run says it is
// one.
func TestSendPutsBackWhatCouldNotBeSentAndMarksRetries(t *testing.T) {
	f := newFixture(t)
	pub := f.svc.pub
	f.svc.pub = refusing{}
	x := f.add(t, "extras/bbb/trailer.mov")
	if got := line(t, f.st, x.ID); !strings.HasPrefix(got, "pending 0 its trigger could not be sent: dial tcp: connection refused retry=true sent=false") {
		t.Errorf("not sent: %s", got)
	}
	f.svc.pub = pub
	if n, _ := f.svc.SendDue(context.Background()); n != 0 {
		t.Error("an extra put back was sent before its backoff")
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET failures = 1, nextretryat = now() WHERE id = $1`, x.ID)
	if n, err := f.svc.SendDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("SendDue: %d, %v", n, err)
	}
	if tr := f.triggers(t)[x.ID]; tr["status"] != "retry" || tr["source"] != "retry" {
		t.Errorf("a retry: %v", tr)
	}
}

// The reaper has the transcoder announce again a transcode no packager
// started (a trigger that is no retry, its state kept), and puts back an
// extra whose worker went silent, to run its chain again; with no bus it
// takes nothing. A transcode it could not have announced is reaped again,
// its failure uncounted.
func TestReapAnnouncesATranscodeAgainAndPutsBackTheRest(t *testing.T) {
	f := newFixture(t)
	waiting := f.add(t, "extras/bbb/a.mov")
	silent := f.add(t, "extras/bbb/b.mov")
	f.w.take()
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET state = 'transcoded', dispatchedat = NULL,
		heartbeatat = now() - interval '25 hours' WHERE id = $1`, waiting.ID)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET state = 'packaging', dispatchedat = NULL,
		heartbeatat = now() - interval '3 hours' WHERE id = $1`, silent.ID)
	pub := f.svc.pub
	f.svc.pub = nil
	if n, err := f.svc.Reap(context.Background()); err != nil || n != 0 {
		t.Errorf("Reap without a bus: %d, %v", n, err)
	}
	f.svc.pub = refusing{}
	if n, err := f.svc.Reap(context.Background()); err != nil || n != 2 {
		t.Fatalf("Reap: %d, %v", n, err)
	}
	if got := line(t, f.st, waiting.ID); !strings.HasPrefix(got, "transcoded 0 its transcode could not be announced again") {
		t.Errorf("a transcode not announced: %s", got)
	}
	if got := line(t, f.st, silent.ID); got != "pending 1 timed out: no word from its packager for 2h retry=true sent=false" {
		t.Errorf("a silent packager: %s", got)
	}
	f.svc.pub = pub
	if n, err := f.svc.Reap(context.Background()); err != nil || n != 1 {
		t.Fatalf("Reap again: %d, %v", n, err)
	}
	tr := f.triggers(t)
	if len(tr) != 1 || tr[waiting.ID]["status"] != "queued" || tr[waiting.ID]["source"] != "reaper" {
		t.Errorf("the triggers the reaper sent: %v", tr)
	}
	if got := line(t, f.st, waiting.ID); !strings.HasPrefix(got, "transcoded 1 timed out: no packager started") || !strings.HasSuffix(got, "sent=true") {
		t.Errorf("a transcode announced again: %s", got)
	}
}

// A day after an extra's removal its package goes, with the packages it
// replaced and its handoff left in the inbox; a removed extra within its
// grace, and one not removed, keep theirs, and nothing outside the extras'
// part of the store is touched.
func TestTheRemovedExtrasPackagesAreDeletedADayLater(t *testing.T) {
	f := newFixture(t)
	old := f.add(t, "extras/bbb/a.mov")
	recent := f.add(t, "extras/bbb/b.mov")
	live := f.add(t, "extras/bbb/c.mov")
	pkg := func(x *model.Extra) string {
		dir := PackageDir(f.cfg.PackagesRoot, x.ID)
		f.write(t, strings.TrimPrefix(dir, f.dir+"/")+"/hls/master.m3u8", 10)
		f.write(t, strings.TrimPrefix(dir, f.dir+"/")+"/.complete", 10)
		return dir
	}
	oldDir, recentDir, liveDir := pkg(old), pkg(recent), pkg(live)
	f.write(t, strings.TrimPrefix(oldDir, f.dir+"/")+".old-20261005T120000Z/manifest.json", 10)
	inbox := filepath.Join(f.cfg.PackagesRoot, "_inbox", "extra-"+old.ID)
	f.write(t, strings.TrimPrefix(inbox, f.dir+"/")+"/renditions.json", 10)
	elsewhere := f.write(t, "media/keep.mkv", 10)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET packagepath = $2 WHERE id = $1`, old.ID, filepath.Dir(elsewhere))
	for id, removed := range map[string]string{old.ID: "25 hours", recent.ID: "1 hour"} {
		if _, err := f.svc.RemoveExtra(asAdmin(), id, "a duplicate"); err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET removedat = now() - $2::interval,
			nextretryat = now() - $2::interval + interval '24 hours' WHERE id = $1`, id, removed)
	}
	if n, err := f.svc.DeleteRemovedPackages(context.Background()); err != nil || n != 1 {
		t.Fatalf("DeleteRemovedPackages: %d, %v", n, err)
	}
	for _, gone := range []string{oldDir, oldDir + ".old-20261005T120000Z", inbox} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is left: %v", gone, err)
		}
	}
	for _, kept := range []string{recentDir, liveDir, elsewhere} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s went: %v", kept, err)
		}
	}
	if x, _ := f.st.GetExtra(context.Background(), old.ID); x.PackagePath != nil || x.NextRetryAt != nil {
		t.Errorf("the removed extra still names a package or a sweep: %+v", x)
	}
	if n, err := f.svc.DeleteRemovedPackages(context.Background()); err != nil || n != 0 {
		t.Errorf("a second sweep: %d, %v", n, err)
	}
}

// An extra packaged again waits to be sent afresh and goes, its failures
// cleared, its package playing meanwhile; one in its packaging within its
// timeout is left alone, and so is one whose file is missing until the file
// is back. Every extra of a title goes the same way. An extra or a title
// there is not is not found.
func TestPackageExtraAgain(t *testing.T) {
	f := newFixture(t)
	ctx := asAdmin()
	ready := f.add(t, "extras/bbb/a.mov")
	busy := f.add(t, "extras/bbb/b.mov")
	missing := f.add(t, "extras/bbb/c.mov")
	f.w.take()
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET state = 'ready', packagedat = now(), failures = 3,
		error = 'it failed', dispatchedat = NULL WHERE id = $1`, ready.ID)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET state = 'transcoding', heartbeatat = now(),
		dispatchedat = NULL WHERE id = $1`, busy.ID)
	if _, err := f.st.MarkExtrasMissing(context.Background(), []string{missing.ID}, "katalog-manager/scanner"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(*missing.SourcePath, *missing.SourcePath+".away"); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.PackageExtra(ctx, ready.ID)
	if err != nil || res.Queued != 1 || res.Busy != 0 || len(res.Extras) != 1 || res.Extras[0].State != "queued" ||
		res.Extras[0].Failures != 0 || !res.Extras[0].Playable() ||
		res.Message != "packaging it again (stube.catalog.extra.queued sent); the package it has plays until the new one is in place" {
		t.Fatalf("PackageExtra: %+v, %v", res, err)
	}
	if tr := f.triggers(t)[ready.ID]; tr["status"] != "queued" || tr["source"] != "reencode" {
		t.Errorf("the re-encode's trigger: %v", tr)
	}
	res, err = f.svc.PackageExtra(ctx, busy.ID)
	if err != nil || res.Queued != 0 || res.Busy != 1 || !strings.HasPrefix(res.Message, "left alone: it is transcoding: its worker last reported at") {
		t.Errorf("a running extra: %+v, %v", res, err)
	}
	res, _ = f.svc.PackageExtra(ctx, missing.ID)
	if res.Busy != 1 || !strings.HasPrefix(res.Message, "left alone: its file is missing (") {
		t.Errorf("a missing extra: %+v", res)
	}
	if err := os.Rename(*missing.SourcePath+".away", *missing.SourcePath); err != nil {
		t.Fatal(err)
	}
	// The extra sent a moment ago waits for its transcoder, within its
	// timeout: left alone, as the running one; the one whose file is back
	// goes.
	res, err = f.svc.PackageExtras(ctx, film)
	if err != nil || res.Queued != 1 || res.Busy != 2 || len(res.Extras) != 3 ||
		!strings.HasPrefix(res.Message, "packaging 1 of its 3 extras again (stube.catalog.extra.queued sent)") ||
		!strings.Contains(res.Message, "; 2 left alone (the first: it waits for the transcoder since ") {
		t.Errorf("PackageExtras: %+v, %v", res, err)
	}
	if x, _ := f.st.GetExtra(context.Background(), missing.ID); x.State != "queued" || x.Hidden {
		t.Errorf("a missing extra whose file is back: %s hidden %v", x.State, x.Hidden)
	}
	for _, call := range []func() error{
		func() error { _, err := f.svc.PackageExtra(ctx, "unknown"); return err },
		func() error { _, err := f.svc.PackageExtras(ctx, "unknown"); return err },
	} {
		if r := refusal(t, call()); r.Status != http.StatusNotFound || r.Code != "NOT_FOUND" {
			t.Errorf("unknown: %+v", r)
		}
	}
	if res, err := f.svc.PackageExtras(ctx, series); err != nil || res.Message != "the title has no extras" || res.Extras == nil {
		t.Errorf("a title without extras: %+v, %v", res, err)
	}
	if _, err := f.svc.RemoveExtra(ctx, ready.ID, ""); err != nil {
		t.Fatal(err)
	}
	if r := refusal(t, func() error { _, err := f.svc.PackageExtra(ctx, ready.ID); return err }()); r.Status != http.StatusNotFound {
		t.Errorf("a removed extra: %+v", r)
	}
}

// A removal says who removed the extra, from the caller's token.
func TestRemoveExtraSaysWho(t *testing.T) {
	f := newFixture(t)
	x := f.add(t, "extras/bbb/a.mov")
	removed, err := f.svc.RemoveExtra(asAdmin(), x.ID, "the wrong film's")
	if err != nil || removed.RemovedAt == nil || *removed.RemovedBy != "admin-1" || *removed.RemovalReason != "the wrong film's" {
		t.Errorf("RemoveExtra: %+v, %v", removed, err)
	}
	if gone, err := f.svc.RemoveExtra(asAdmin(), "unknown", ""); err != nil || gone != nil {
		t.Errorf("an extra there is not: %v, %v", gone, err)
	}
	// An extra recorded in the library is removed by its record's event.
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, recordpath)
		VALUES ('x-recorded', $1, 'featurette', 'Featurette', 'library', 'library/movies/ea/`+film+`/extras/x-recorded/')`, film)
	_, err = f.svc.RemoveExtra(asAdmin(), "x-recorded", "")
	if r := refusal(t, err); r.Status != http.StatusConflict || !strings.Contains(r.Message, "is recorded in the library") {
		t.Errorf("a recorded extra: %+v", r)
	}
	if x, _ := f.st.GetExtra(context.Background(), "x-recorded"); x.RemovedAt != nil {
		t.Error("a recorded extra was removed")
	}
}
