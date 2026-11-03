package picture

import (
	"context"
	"encoding/json"
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
	if state == "complete" {
		// its packaged asset, as the handover writes it (its first video
		// rendition's size)
		storetest.Exec(l.t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged'`, item)
		storetest.Exec(l.t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind,
			versionid, resolution) VALUES (gen_random_uuid()::varchar, $1, $2, false, 'packaged', $3, '1280x720')`,
			item, filepath.Join(dir, "package.json"), id)
	}
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
	return Report{Path: path, Method: "picture/2", Frame: &Size{1920, 1080}, Area: &Rect{0, 138, 1920, 804},
		Samples: i32(300), Informative: i32(290)}
}

// measured is the report of a job the backfill handed out: a letterbox, with
// the job's token.
func measured(j Job) Report {
	r := letterbox(j.Path)
	r.Token = j.Token
	return r
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
		c, err := s.Claim(ctx, "analyzer-0", 1, "")
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
	// a lease of 30 minutes, so the lost claim lapses within the night
	lease := 30 * time.Minute
	setting(t, st, SettingLease, "30m")
	now = time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC)
	if c := claim(); !strings.HasPrefix(c.Idle, "outside picture.backfill.window") || c.RetryAfterSeconds != 4*3600 {
		t.Errorf("in the quiet hours: %+v", c)
	}
	now = night
	// m2's original is nowhere and it has no package: it fails, and m3's
	// original goes instead, the titles with their original first.
	c := claim("m3 " + strings.TrimPrefix(takenAt, l.root))
	if c.Deadline == nil || !c.Deadline.Equal(night.Add(lease)) || c.Items[0].Type != "movie" {
		t.Errorf("the claim: %+v", c)
	}
	if a, _ := s.Of(ctx, KindItem, "m2"); a == nil || a.State != StateFailed || !strings.HasPrefix(*a.Reason, "nothing to measure") {
		t.Errorf("the title with nothing to measure: %+v", a)
	}
	if c := claim(); !strings.Contains(c.Idle, "being measured") {
		t.Errorf("one in flight: %+v", c)
	}
	if _, err := s.Record(ctx, KindItem, "m3", measured(c.Items[0])); err != nil {
		t.Fatal(err)
	}
	claim("e1 " + strings.TrimPrefix(epAt, l.root))
	// e1's run never reports back: after the lease it fails, and waits; the
	// package of m1 goes.
	now = night.Add(lease + time.Minute)
	c1 := claim("m1 /movies/m1/versions/v1/hls/v0/playlist.m3u8")
	if a, _ := s.Of(ctx, KindItem, "e1"); a.State != StateFailed || a.Failures != 1 || a.ClaimedAt != nil ||
		!strings.Contains(*a.Reason, "did not report back within 30m") {
		t.Errorf("the lost claim: %+v", a)
	}
	if _, err := s.Record(ctx, KindItem, "m1", Report{Path: c1.Items[0].Path, Frame: &Size{1920, 1080},
		Reason: "too few frames", Method: "picture/2", Token: c1.Items[0].Token}); err != nil {
		t.Fatal(err)
	}
	cx := claim("x1 /movies/m1/extras/x1/hls/v0/playlist.m3u8")
	if _, err := s.Record(ctx, KindExtra, "x1", measured(cx.Items[0])); err != nil {
		t.Fatal(err)
	}
	// Every title has an area or waits out a failure.
	if c := claim(); c.Idle != "every title has a picture area of what it plays now" {
		t.Errorf("done: %+v", c)
	}
	// After its backoff e1 goes again; the rate stops the hour's next.
	now = night.Add(3 * time.Hour)
	setting(t, st, SettingRate, "1")
	ce := claim("e1 " + strings.TrimPrefix(epAt, l.root))
	if _, err := s.Record(ctx, KindItem, "e1", measured(ce.Items[0])); err != nil {
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

// claimOne hands out a title to the worker, the analyzer's method named; it
// fails the test unless it is want.
func claimOne(t *testing.T, s *Service, worker, want string) Job {
	t.Helper()
	c, err := s.Claim(context.Background(), worker, 1, "picture/2")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 1 || c.Items[0].ID != want {
		t.Fatalf("handed out %+v (idle %q), want %s", c.Items, c.Idle, want)
	}
	if c.Items[0].Token == "" {
		t.Fatalf("a job without a token: %+v", c.Items[0])
	}
	return c.Items[0]
}

// A title given back unmeasured (its run's deadline came first) waits its
// release backoff, twice as long after each release in a row, and the titles
// after it go meanwhile; a release of another claim changes nothing, and a
// measurement ends the waits.
func TestAReleasedTitleWaitsAndTheNextGoes(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Slow Film", "")
	storetest.AddItem(t, st, "m2", "movie", "The Next Film", "")
	l.version(st, "m1", "v1", "complete", "s1", false)
	l.version(st, "m2", "v2", "complete", "s2", false)
	setting(t, st, SettingBackfill, "on")

	j := claimOne(t, s, "analyzer-0", "m1")
	now = now.Add(DefaultLease - time.Second)
	a, err := s.Record(ctx, KindItem, "m1", Report{Released: true, Token: j.Token})
	if err != nil {
		t.Fatal(err)
	}
	if a.ClaimedAt != nil || a.Releases != 1 || a.Failures != 0 || a.NextAttemptAt == nil ||
		!a.NextAttemptAt.Equal(now.Add(FirstRelease)) {
		t.Fatalf("given back: %+v", a)
	}
	j2 := claimOne(t, s, "analyzer-0", "m2")
	if _, err := s.Record(ctx, KindItem, "m2", measured(j2)); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Claim(ctx, "analyzer-0", 1, "picture/2"); err != nil || len(c.Items) != 0 {
		t.Fatalf("while m1 waits: %+v, %v", c, err)
	}
	now = now.Add(FirstRelease)
	j = claimOne(t, s, "analyzer-0", "m1")
	now = now.Add(time.Minute)
	if a, err = s.Record(ctx, KindItem, "m1", Report{Released: true, Token: j.Token}); err != nil {
		t.Fatal(err)
	}
	if a.Releases != 2 || !a.NextAttemptAt.Equal(now.Add(2*FirstRelease)) {
		t.Errorf("given back again: %+v", a)
	}
	now = now.Add(2 * FirstRelease)
	j = claimOne(t, s, "analyzer-0", "m1")
	if a, err = s.Record(ctx, KindItem, "m1", Report{Released: true, Token: "another claim's"}); err != nil ||
		a.ClaimedAt == nil || a.Releases != 2 {
		t.Errorf("a release of another claim: %+v, %v", a, err)
	}
	if a, err = s.Record(ctx, KindItem, "m1", measured(j)); err != nil {
		t.Fatal(err)
	}
	if a.State != StateMeasured || a.Releases != 0 || a.NextAttemptAt != nil || a.ClaimedAt != nil {
		t.Errorf("measured: %+v", a)
	}
}

// A report ends the claim it is of and no other. A claim whose lease ended
// counts a failure then: its run's own failure, reported late, is not
// counted again, and its measurement, reported late, is kept and leaves the
// claim another run holds now as it is. The analyzer's pass of a new title
// reports without a token, and leaves a claim as it is too.
func TestAReportEndsOnlyItsOwnClaim(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another", "")
	l.version(st, "m1", "v1", "complete", "s1", false)
	l.version(st, "m2", "v2", "complete", "s2", false)
	setting(t, st, SettingBackfill, "on")
	setting(t, st, SettingInflight, "2")
	inflight := func() int {
		return storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_pictureareas WHERE claimedat IS NOT NULL`)
	}

	j1 := claimOne(t, s, "w1", "m1")
	now = night.Add(DefaultLease + time.Minute)
	j2 := claimOne(t, s, "w2", "m2") // w1's lease ended: m1 failed, and waits
	if _, err := s.Record(ctx, KindItem, "m2", measured(j2)); err != nil {
		t.Fatal(err)
	}
	a, err := s.Record(ctx, KindItem, "m1", Report{Path: j1.Path, Error: "2 of 30 samples failed", Token: j1.Token})
	if err != nil {
		t.Fatal(err)
	}
	if a.Failures != 1 || a.State != StateFailed || *a.Reason != "2 of 30 samples failed" || a.LapsedToken != nil {
		t.Errorf("the late failure: %+v", a)
	}
	now = now.Add(backoff(1))
	j3 := claimOne(t, s, "w2", "m1")
	if a, err = s.Record(ctx, KindItem, "m1", measured(j1)); err != nil {
		t.Fatal(err)
	}
	if a.State != StateMeasured || a.ClaimedAt == nil || *a.ClaimedBy != "w2" || inflight() != 1 {
		t.Errorf("a late measurement: %+v, %d in flight", a, inflight())
	}
	if a, err = s.Record(ctx, KindItem, "m1", letterbox(j1.Path)); err != nil || a.ClaimedAt == nil {
		t.Errorf("a report without a token: %+v, %v", a, err)
	}
	if a, err = s.Record(ctx, KindItem, "m1", measured(j3)); err != nil || a.ClaimedAt != nil || inflight() != 0 {
		t.Errorf("the claim's own report: %+v, %v, %d in flight", a, err, inflight())
	}
}

// An area is of the file it was measured in: of an original by its source
// where the title's asset and the area both name one, else by its path, and
// by its size where both say one; of a legacy extra (no package id) by its
// package's folder, measured since the extra was packaged.
func TestAnotherFileAtTheSamePathHasNoCurrentArea(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	current := func(kind, id string) bool {
		t.Helper()
		a, err := s.Of(ctx, kind, id)
		if err != nil || a == nil {
			t.Fatalf("%s: %+v, %v", id, a, err)
		}
		return a.Current
	}
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	orig := l.original(st, "m1", "s1")
	if _, err := s.Record(ctx, KindItem, "m1", letterbox(orig)); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = 's2' WHERE item_id = 'm1'`)
	if current(KindItem, "m1") {
		t.Error("current after a new source at the same path")
	}

	storetest.AddItem(t, st, "m2", "movie", "An Old Film", "")
	p2 := l.file("incoming/m2.mkv", "a film")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes)
		VALUES ('a2', 'm2', $1, true, 'primary', 6)`, p2)
	r := letterbox(p2)
	six := int64(6)
	r.MeasuredOn = &MeasuredOn{Kind: "original", Size: &six}
	a, err := s.Record(ctx, KindItem, "m2", r)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Current || a.FileSize == nil || *a.FileSize != 6 || *a.MeasuredOn != "original" {
		t.Errorf("a legacy original: %+v", a)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sizebytes = 7 WHERE id = 'a2'`)
	if current(KindItem, "m2") {
		t.Error("current after the file at its path changed its size")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sizebytes = NULL WHERE id = 'a2'`)
	if !current(KindItem, "m2") {
		t.Error("not current while the asset says no size")
	}

	dir := filepath.Join(l.root, "packages", "extras", "x1")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state,
		packagepath, packagedat) VALUES ('x1', 'm1', 'trailer', 'Trailer', 'api', 'ready', $1, $2)`, dir, night.Add(-time.Hour))
	if a, err := s.Record(ctx, KindExtra, "x1", letterbox(dir+"/hls/v0/playlist.m3u8")); err != nil || !a.Current ||
		*a.MeasuredOn != "package" {
		t.Fatalf("a legacy extra: %+v, %v", a, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET packagedat = $1 WHERE id = 'x1'`, night.Add(time.Hour))
	if current(KindExtra, "x1") {
		t.Error("current after the legacy extra was packaged anew in its folder")
	}
}

// An original measured before its version was packaged is the title's area
// while the package's picture has its shape: the frame's display aspect (with
// the pixel aspect the measurement read) and the package's within 1 %. An
// anamorphic original's (720x576, its pixels 64:45) holds for its package of
// square pixels (1024x576); one of another shape is measured again, of the
// original while it is on disk, of the package once it is gone.
func TestAnOriginalsAreaHoldsForAPackageOfItsShape(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film on a Disc", "")
	orig := l.original(st, "m1", "s1")
	dvd := Report{Path: orig, Method: "picture/2", Frame: &Size{720, 576}, Area: &Rect{0, 70, 720, 436},
		Video: json.RawMessage(`{"codec": "mpeg2video", "sar": "64:45"}`)}
	if a, err := s.Record(ctx, KindItem, "m1", dvd); err != nil || !a.Current {
		t.Fatalf("measured of the original: %+v, %v", a, err)
	}
	l.version(st, "m1", "v1", "complete", "s1", false)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET resolution = '1024x576'
		WHERE item_id = 'm1' AND kind = 'packaged'`)
	if a, _ := s.Of(ctx, KindItem, "m1"); !a.Current {
		t.Fatal("not current for a package of the same display shape")
	}
	// a package of another shape (cropped to 1024x436)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET resolution = '1024x436'
		WHERE item_id = 'm1' AND kind = 'packaged'`)
	if a, _ := s.Of(ctx, KindItem, "m1"); a.Current {
		t.Fatal("current while the package's picture has another shape")
	}
	setting(t, st, SettingBackfill, "on")
	if c, err := s.Claim(ctx, "analyzer-0", 1, "picture/2"); err != nil || len(c.Items) != 0 {
		t.Fatalf("within a day of its measurement: %+v, %v", c, err)
	}
	now = now.Add(MeasureAgainAfter)
	if j := claimOne(t, s, "analyzer-0", "m1"); j.Path != orig {
		t.Errorf("handed out %s while the original is there, not the original", j.Path)
	} else if _, err := s.Record(ctx, KindItem, "m1", Report{Released: true, Token: j.Token}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(orig); err != nil {
		t.Fatal(err)
	}
	now = now.Add(MaxRelease)
	j := claimOne(t, s, "analyzer-0", "m1")
	if !strings.HasSuffix(j.Path, "/versions/v1/hls/v0/playlist.m3u8") {
		t.Errorf("handed out %s, not its package, once the original is gone", j.Path)
	}
	r := Report{Path: j.Path, Method: "picture/2", Token: j.Token, Frame: &Size{1024, 436}, Area: &Rect{0, 0, 1024, 436}}
	if a, err := s.Record(ctx, KindItem, "m1", r); err != nil || !a.Current || *a.VersionID != "v1" ||
		*a.MeasuredOn != "package" {
		t.Errorf("measured of the package: %+v, %v", a, err)
	}
}

// The backfill hands out the titles that still have their original first, and
// measures each of its original; a package that could not tell an area for
// its keyframes' gaps is measured again of the original while there is one,
// and not again of itself after a month.
func TestTitlesWithTheirOriginalGoFirst(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Packaged Film", "")
	storetest.AddItem(t, st, "m2", "movie", "A Film With Its Original", "")
	l.version(st, "m1", "v1", "complete", "s1", false)
	l.version(st, "m2", "v2", "complete", "s2", false)
	orig := l.original(st, "m2", "s2")
	setting(t, st, SettingBackfill, "on")
	setting(t, st, SettingInflight, "2")
	c, err := s.Claim(ctx, "analyzer-0", 2, "picture/2")
	if err != nil || len(c.Items) != 2 {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	if c.Items[0].ID != "m2" || c.Items[0].Path != orig || c.Items[1].ID != "m1" ||
		!strings.HasSuffix(c.Items[1].Path, "/versions/v1/hls/v0/playlist.m3u8") {
		t.Fatalf("handed out %+v", c.Items)
	}
	gap := func(j Job) Report {
		return Report{Path: j.Path, Method: "picture/2", Token: j.Token, Frame: &Size{1920, 1080},
			Reason:   "can't tell: its samples are up to 6.0 s apart (from 1002.0 s), more than 3 s",
			Sampling: json.RawMessage(`{"mode": "package", "doubt": ["gap"]}`)}
	}
	for _, j := range c.Items {
		if _, err := s.Record(ctx, KindItem, j.ID, gap(j)); err != nil {
			t.Fatal(err)
		}
	}
	// m2 was told of its original: it needs it no more; m1 was told of a
	// package only
	if needs, err := NeedsOriginal(ctx, st.Pool(), "m2", "s2"); err != nil || needs {
		t.Errorf("m2 needs its original: %v, %v", needs, err)
	}
	if needs, err := NeedsOriginal(ctx, st.Pool(), "m1", "s1"); err != nil || !needs {
		t.Errorf("m1 needs no original: %v, %v", needs, err)
	}
	// a month on: m1's package answer stays (it would say the same), m2's
	// original answer is retried as any unknown is
	now = now.Add(UnknownAgain + time.Hour)
	c, err = s.Claim(ctx, "analyzer-0", 2, "picture/2")
	if err != nil || len(c.Items) != 1 || c.Items[0].ID != "m2" {
		t.Fatalf("a month on: %+v, %v", c.Items, err)
	}
}

// NeedsOriginal: an item needs its original until it has an area of what it
// plays now that says the picture's area, or one of that original that says
// it can't tell.
func TestAnItemNeedsItsOriginalUntilItIsMeasured(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	orig := l.original(st, "m1", "s1")
	needs := func() bool {
		t.Helper()
		n, err := NeedsOriginal(ctx, st.Pool(), "m1", "s1")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if !needs() {
		t.Error("needs no original before any measurement")
	}
	if _, err := s.Record(ctx, KindItem, "m1", Report{Path: orig, Error: "the file can't be opened"}); err != nil {
		t.Fatal(err)
	}
	if !needs() {
		t.Error("needs no original after a failure")
	}
	r := Report{Path: orig, Method: "picture/2", Frame: &Size{1920, 1080}, Reason: "can't tell: a gap",
		MeasuredOn: &MeasuredOn{Kind: "original"}}
	if _, err := s.Record(ctx, KindItem, "m1", r); err != nil {
		t.Fatal(err)
	}
	if needs() {
		t.Error("needs its original after it could not tell of it")
	}
	if _, err := s.Record(ctx, KindItem, "m1", letterbox(orig)); err != nil {
		t.Fatal(err)
	}
	if needs() {
		t.Error("needs its original after its area was measured")
	}
}

// The backfill measures again what an older method measured and what the
// analyzer could not tell of a month ago, after the titles never measured.
func TestOlderMethodsAndOldUnknownsAreMeasuredAgain(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		l.version(st, id, "v"+id, "complete", "s"+id, false)
	}
	pkg := func(id string) string {
		return filepath.Join(l.root, "movies", id, "versions", "v"+id, "hls", "v0", "playlist.m3u8")
	}
	old := letterbox(pkg("m1"))
	old.Method = "picture/1"
	for id, r := range map[string]Report{
		"m1": old,
		"m2": {Path: pkg("m2"), Method: "picture/2", Frame: &Size{1920, 1080}, Reason: "too few samples show a picture"},
		"m3": {Path: pkg("m3"), Method: "picture/2", Frame: &Size{1920, 1080}, Reason: "too few samples show a picture"},
		"m5": letterbox(pkg("m5")),
	} {
		if _, err := s.Record(ctx, KindItem, id, r); err != nil {
			t.Fatal(err)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET measuredat = $1 WHERE id IN ('m1', 'm3', 'm5')`,
		night.Add(-2*24*time.Hour))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET measuredat = $1 WHERE id = 'm2'`,
		night.Add(-UnknownAgain-time.Hour))
	setting(t, st, SettingBackfill, "on")
	setting(t, st, SettingInflight, "10")
	c, err := s.Claim(ctx, "analyzer-0", 10, "picture/2")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range c.Items {
		got = append(got, j.ID)
	}
	if strings.Join(got, ",") != "m4,m1,m2" {
		t.Errorf("handed out %v, want m4 (never measured), m1 (an older method), m2 (unknown a month ago)", got)
	}
}

// What the analyzer could not tell of a title's original by an older method
// than its own is measured again the day after, before the titles never
// measured, but not a title too costly to measure, nor one it could not tell
// of a package.
func TestAnUnknownOfAnOlderMethodOfItsOriginalGoesFirst(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
		l.original(st, id, "s"+id)
	}
	unknown := func(id, doubt string, on MeasuredOn) Report {
		return Report{Path: filepath.Join(l.root, "incoming", id+".mkv"), Method: "picture/4",
			Frame: &Size{1920, 1080}, Reason: "can't tell: " + doubt, MeasuredOn: &on,
			Sampling: json.RawMessage(`{"mode": "keyframes", "doubt": ["` + doubt + `"]}`)}
	}
	for id, r := range map[string]Report{
		"m4": unknown("m4", "gap", MeasuredOn{Kind: "original"}),
		"m5": unknown("m5", "cost", MeasuredOn{Kind: "original"}),
	} {
		if _, err := s.Record(ctx, KindItem, id, r); err != nil {
			t.Fatal(err)
		}
	}
	setting(t, st, SettingBackfill, "on")
	setting(t, st, SettingInflight, "10")
	claim := func(method string) string {
		t.Helper()
		c, err := s.Claim(ctx, "analyzer-0", 10, method)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, j := range c.Items {
			got = append(got, j.ID)
		}
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET claimedat = NULL, claimedby = NULL`)
		return strings.Join(got, ",")
	}
	now = night.Add(time.Hour)
	if got := claim("picture/5"); got != "m1,m2,m3" {
		t.Errorf("within a day: handed out %v, want m1,m2,m3 (never measured)", got)
	}
	now = night.Add(MeasureAgainAfter + time.Hour)
	if got := claim("picture/5"); got != "m4,m1,m2,m3,m5" {
		t.Errorf("a day on: handed out %v, want m4 (an older method's unknown), m1,m2,m3 (never "+
			"measured), m5 (too costly, of an older method)", got)
	}
	// of the analyzer's own method, the unknowns stand
	if got := claim("picture/4"); got != "m1,m2,m3" {
		t.Errorf("of the same method: handed out %v, want m1,m2,m3", got)
	}
}

// A run that has its whole lease and does not finish in it counts a costly
// run; one given back before its lease's end (its worker stopped), or one the
// window's end cuts short, does not. The run after CostlyRuns-1 of them is
// the last (Job.Last), unless the window cuts its lease: its report of the
// title as too costly to measure, an unknown of the original, ends the count,
// settles what a deletion wave asks, and stands a month later.
func TestATitleTooCostlyToMeasureIsSaidSo(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	now := night
	l := lib{t, t.TempDir()}
	s := service(t, st, l.root, &now)
	storetest.AddItem(t, st, "m1", "movie", "A Long Film", "")
	orig := l.original(st, "m1", "s1")
	setting(t, st, SettingBackfill, "on")
	setting(t, st, SettingLease, "6h")
	release := func(j Job, after time.Duration) *Area {
		t.Helper()
		now = now.Add(after)
		a, err := s.Record(ctx, KindItem, "m1", Report{Released: true, Path: j.Path, Token: j.Token})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	next := func() Job {
		t.Helper()
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_pictureareas SET nextattemptat = NULL WHERE id = 'm1'`)
		return claimOne(t, s, "analyzer-0", "m1")
	}

	j := claimOne(t, s, "analyzer-0", "m1")
	if j.CostlyRuns != 0 || j.Last {
		t.Fatalf("the first run: %+v", j)
	}
	if a := release(j, time.Hour); a.CostlyRuns != 0 || a.Releases != 1 {
		t.Errorf("given back an hour in: %+v", a)
	}
	for i := int32(1); i < CostlyRuns; i++ {
		if j = next(); j.Last || j.CostlyRuns != i-1 {
			t.Fatalf("run %d: %+v", i, j)
		}
		if a := release(j, 6*time.Hour); a.CostlyRuns != i {
			t.Errorf("given back at its lease's end: %+v", a)
		}
	}
	// 16:00 UTC: the window closes at 17:00, before the lease ends
	if j = next(); j.Last || j.CostlyRuns != CostlyRuns-1 {
		t.Fatalf("a run the window cuts short: %+v", j)
	}
	if a := release(j, time.Hour); a.CostlyRuns != CostlyRuns-1 {
		t.Errorf("given back at the window's end: %+v", a)
	}
	now = night.Add(24 * time.Hour)
	if j = next(); !j.Last || j.CostlyRuns != CostlyRuns-1 {
		t.Fatalf("the last run: %+v", j)
	}
	r := Report{Path: orig, Token: j.Token, Method: "picture/2", Frame: &Size{1920, 1080},
		Reason:     "can't tell: too costly to measure",
		MeasuredOn: &MeasuredOn{Kind: "original"}, Sampling: json.RawMessage(`{"doubt": ["cost"]}`)}
	now = now.Add(6 * time.Hour)
	a, err := s.Record(ctx, KindItem, "m1", r)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateUnknown || a.CostlyRuns != 0 || a.Releases != 0 || !a.Current {
		t.Errorf("too costly: %+v", a)
	}
	if n, err := NeedsOriginal(ctx, st.Pool(), "m1", "s1"); err != nil || n {
		t.Errorf("needs its original after it was too costly to measure: %v, %v", n, err)
	}
	now = now.Add(UnknownAgain + 2*24*time.Hour)
	if c, err := s.Claim(ctx, "analyzer-0", 1, "picture/2"); err != nil || len(c.Items) != 0 {
		t.Errorf("handed out again a month later: %+v, %v", c, err)
	}
}
