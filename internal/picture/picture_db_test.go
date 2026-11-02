package picture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// night is inside the backfill's default window (22:00-17:00 UTC).
var night = time.Date(2026, 11, 3, 3, 0, 0, 0, time.UTC)

// lib is a library on disk: files the tests write, under one root.
type lib struct {
	t    *testing.T
	root string
}

func (l lib) file(rel, content string) string {
	l.t.Helper()
	p := filepath.Join(l.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
	return p
}

// version gives the item a version in state of the original source, its
// folder holding a package whose record names a 1920x1080 video rendition
// with its playlist on disk (none when bare); it returns the folder.
func (l lib) version(st *store.Store, item, id, state, source string, bare bool) string {
	l.t.Helper()
	dir := filepath.Join(l.root, "movies", item, "versions", id)
	if !bare {
		l.file(filepath.Join("movies", item, "versions", id, "package.json"),
			`{"renditions": {"video": [{"id": "v1", "dir": "hls/v1", "width": 1280, "height": 720},
			{"id": "v0", "dir": "hls/v0", "width": 1920, "height": 1080}]}}`)
		l.file(filepath.Join("movies", item, "versions", id, "hls", "v0", "playlist.m3u8"), "#EXTM3U\n")
		l.file(filepath.Join("movies", item, "versions", id, "hls", "v1", "playlist.m3u8"), "#EXTM3U\n")
	}
	storetest.Exec(l.t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir, sourceids)
		VALUES ($1, $2, $3, $4, ARRAY[$5]::varchar[])`, id, item, state, dir, source)
	return dir
}

// original gives the item its original on disk, of the source, as its
// primary asset.
func (l lib) original(st *store.Store, item, source string) string {
	l.t.Helper()
	p := l.file(filepath.Join("incoming", item+".mkv"), "a film")
	storetest.Exec(l.t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', $3)`, item, p, source)
	return p
}

func setting(t *testing.T, st *store.Store, key, value string) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE key = $1`, key)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, $1, $2)`,
		key, value)
}

func letterbox(path string) Report {
	return Report{Path: path, Method: "picture/1", Frame: &Size{1920, 1080}, Area: &Rect{0, 138, 1920, 804},
		Samples: i32(300), Informative: i32(290)}
}

func service(t *testing.T, st *store.Store, root string, now *time.Time) *Service {
	return New(st, config.Config{LibraryRoot: root, PackagesRoot: filepath.Join(root, "packages")}).
		WithClock(func() time.Time { return *now })
}

// A measurement of an item's original is of its source: of what the title
// plays now while it plays that original, and once a version made of it is
// complete; not once the title's file is replaced (a new original, a version
// of it). One of a version's package is of that version, and of the original
// it was made of.
func TestAnAreaIsOfTheFileItWasMeasuredIn(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	orig := l.original(st, "m1", "s1")

	a, err := s.Record(ctx, KindItem, "m1", letterbox(orig))
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateMeasured || !a.Current || a.SourceID == nil || *a.SourceID != "s1" || a.VersionID != nil ||
		*a.Y != 138 || *a.Height != 804 || *a.FrameWidth != 1920 {
		t.Fatalf("measured of the original: %+v", a)
	}
	l.version(st, "m1", "v1", "complete", "s1", false)
	if a, _ := s.Of(ctx, KindItem, "m1"); !a.Current {
		t.Error("not current once the version of its original is complete")
	}
	// A new original, and a version of it: the area is of another file.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = 'v1'`)
	l.version(st, "m1", "v2", "complete", "s2", false)
	if a, _ := s.Of(ctx, KindItem, "m1"); a.Current {
		t.Error("current after the title's file was replaced")
	}
	// Measured again, of the package of v2.
	pkg := filepath.Join(l.root, "movies", "m1", "versions", "v2", "hls", "v0", "playlist.m3u8")
	a, err = s.Record(ctx, KindItem, "m1", letterbox(pkg))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Current || *a.VersionID != "v2" || *a.SourceID != "s2" || *a.Path != pkg {
		t.Errorf("measured of the package: %+v", a)
	}
}

// A failure never replaces an area of what the title plays now: it counts,
// and the area stays. Of a title without one it is the title's state, and it
// waits its backoff. A release ends a claim and nothing else. Who can't have
// an area of their own is told why.
func TestFailuresReleasesAndRefusals(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "e2", "episode", "The Second Half", "s1")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET coveredby = 'e1' WHERE id = 'e2'`)
	orig := l.original(st, "m1", "s1")
	if _, err := s.Record(ctx, KindItem, "m1", letterbox(orig)); err != nil {
		t.Fatal(err)
	}
	a, err := s.Record(ctx, KindItem, "m1", Report{Path: orig, Error: "ffprobe could not read the file"})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateMeasured || a.Failures != 1 || *a.Height != 804 {
		t.Errorf("a failure over a current area: %+v", a)
	}

	orig2 := l.original(st, "m2", "s3")
	a, err = s.Record(ctx, KindItem, "m2", Report{Path: orig2, Error: "none of 300 samples could be decoded"})
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateFailed || a.Failures != 1 || a.NextAttemptAt == nil || !a.NextAttemptAt.Equal(now.Add(time.Hour)) ||
		*a.Reason != "none of 300 samples could be decoded" || a.Current != true {
		t.Errorf("a failure: %+v", a)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET claimedat = $1, claimedby = 'w' WHERE id = 'm2'`, now)
	if a, err = s.Record(ctx, KindItem, "m2", Report{Released: true}); err != nil || a.ClaimedAt != nil ||
		a.State != StateFailed || a.Failures != 1 {
		t.Errorf("released: %+v, %v", a, err)
	}

	var bad *BadReport
	for id, says := range map[string]string{"e2": "plays the file of e1", "s1": "a series has no frame"} {
		if _, err := s.Record(ctx, KindItem, id, letterbox(orig)); !errors.As(err, &bad) || !strings.Contains(err.Error(), says) {
			t.Errorf("%s: %v, want a refusal saying %q", id, err, says)
		}
	}
	if _, err := s.Record(ctx, KindItem, "nothing", letterbox(orig)); !errors.Is(err, ErrNoTitle) {
		t.Errorf("an unknown title: %v", err)
	}
	if _, err := s.Record(ctx, KindExtra, "nothing", letterbox(orig)); !errors.Is(err, ErrNoTitle) {
		t.Errorf("an unknown extra: %v", err)
	}
	// A covered episode is answered its holder's area.
	ep := l.original(st, "e1", "s5")
	if _, err := s.Record(ctx, KindItem, "e1", letterbox(ep)); err != nil {
		t.Fatal(err)
	}
	if a, err := s.Of(ctx, KindItem, "e2"); err != nil || a == nil || a.ID != "e1" {
		t.Errorf("the covered episode: %+v, %v", a, err)
	}
}

// An extra's area is of its package: current while it is the one it has,
// not once the extra is packaged anew or removed.
func TestAnExtrasAreaIsOfItsPackage(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	dir := filepath.Join(l.root, "movies", "m1", "extras", "x1")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state,
		packagepath, packagedat, packageid) VALUES ('x1', 'm1', 'trailer', 'Trailer', 'api', 'ready', $1, now(), 'p1')`, dir)
	a, err := s.Record(ctx, KindExtra, "x1", letterbox(dir+"/hls/v0/playlist.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Current || a.ItemID != "m1" || *a.PackageID != "p1" {
		t.Errorf("measured: %+v", a)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET packageid = 'p2' WHERE id = 'x1'`)
	if a, _ := s.Of(ctx, KindExtra, "x1"); a.Current {
		t.Error("current after the extra was packaged anew")
	}
}

// The backfill hands out the titles without an area of what they play now,
// movies first, then episodes, then extras, a title's package where it has
// one (its largest video rendition's playlist), else its original; at most
// picture.backfill.inflight at once and .rate an hour, inside the window and
// only while it is on. A title whose file can't be found fails and the next
// one goes; a claim whose lease ended counts as a failure.
func TestTheBackfillHandsOutTitlesAtItsPace(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m3", "movie", "A Taken Film", "")
	storetest.AddItem(t, st, "m2", "movie", "A Lost Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	l.version(st, "m1", "v1", "complete", "s1", false)
	takenAt := l.original(st, "m3", "s2")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('gone', 'm2', '/nowhere/m2.mkv', true, 'primary', 's3')`)
	epAt := l.original(st, "e1", "s4")
	xdir := filepath.Join(l.root, "movies", "m1", "extras", "x1")
	l.file("movies/m1/extras/x1/package.json", `{"renditions": {"video": [{"id": "v0", "dir": "hls/v0", "width": 1920, "height": 800}]}}`)
	l.file("movies/m1/extras/x1/hls/v0/playlist.m3u8", "#EXTM3U\n")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state,
		packagepath, packagedat, packageid) VALUES ('x1', 'm1', 'trailer', 'Trailer', 'api', 'ready', $1, now(), 'p1')`, xdir)

	claim := func(want ...string) Claim {
		t.Helper()
		c, err := s.Claim(ctx, "analyzer-0", 1)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, j := range c.Items {
			got = append(got, j.ID+" "+strings.TrimPrefix(j.Path, l.root))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("handed out %v (idle %q), want %v", got, c.Idle, want)
		}
		return c
	}

	if c := claim(); c.Idle != "picture.backfill is off" {
		t.Errorf("off: %+v", c)
	}
	setting(t, st, SettingBackfill, "on")
	now = time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	if c := claim(); !strings.HasPrefix(c.Idle, "outside picture.backfill.window") || c.RetryAfterSeconds != 4*3600 {
		t.Errorf("in the quiet hours: %+v", c)
	}
	now = night
	c := claim("m1 /movies/m1/versions/v1/hls/v0/playlist.m3u8")
	if c.Deadline == nil || !c.Deadline.Equal(night.Add(DefaultLease)) || c.Items[0].Type != "movie" {
		t.Errorf("the claim: %+v", c)
	}
	if c := claim(); !strings.Contains(c.Idle, "being measured") {
		t.Errorf("one in flight: %+v", c)
	}
	if _, err := s.Record(ctx, KindItem, "m1", letterbox(c.Items[0].Path)); err != nil {
		t.Fatal(err)
	}
	// m2's original is nowhere: it fails, and m3's original goes instead.
	claim("m3 " + strings.TrimPrefix(takenAt, l.root))
	if a, _ := s.Of(ctx, KindItem, "m2"); a == nil || a.State != StateFailed || !strings.HasPrefix(*a.Reason, "nothing to measure") {
		t.Errorf("the title with nothing to measure: %+v", a)
	}
	// m3's run never reports back: after the lease it fails, and waits.
	now = night.Add(DefaultLease + time.Minute)
	claim("e1 " + strings.TrimPrefix(epAt, l.root))
	if a, _ := s.Of(ctx, KindItem, "m3"); a.State != StateFailed || a.Failures != 1 || a.ClaimedAt != nil ||
		!strings.Contains(*a.Reason, "did not report back within 30m") {
		t.Errorf("the lost claim: %+v", a)
	}
	if _, err := s.Record(ctx, KindItem, "e1", Report{Path: epAt, Frame: &Size{1920, 1080}, Reason: "too few frames"}); err != nil {
		t.Fatal(err)
	}
	claim("x1 /movies/m1/extras/x1/hls/v0/playlist.m3u8")
	if _, err := s.Record(ctx, KindExtra, "x1", letterbox(xdir+"/hls/v0/playlist.m3u8")); err != nil {
		t.Fatal(err)
	}
	// Every title has an area or waits out a failure.
	if c := claim(); c.Idle != "every title has a picture area of what it plays now" {
		t.Errorf("done: %+v", c)
	}
	// After its backoff m3 goes again; the rate stops the hour's next.
	now = night.Add(3 * time.Hour)
	setting(t, st, SettingRate, "1")
	claim("m3 " + strings.TrimPrefix(takenAt, l.root))
	if _, err := s.Record(ctx, KindItem, "m3", letterbox(takenAt)); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET nextattemptat = NULL WHERE id = 'm2'`)
	if c := claim(); !strings.Contains(c.Idle, "in the last hour") || c.RetryAfterSeconds != 3600 {
		t.Errorf("the rate: %+v", c)
	}

	p, err := s.Progress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Titles != 5 || p.Letterboxed != 3 || p.Unknown != 1 || p.ToMeasure != 1 || p.Failed != 1 || p.InFlight != 0 ||
		!p.Settings.On || p.Settings.Rate != 1 || !strings.Contains(p.Idle, "picture.backfill.rate") {
		t.Errorf("progress: %+v", p)
	}
}
