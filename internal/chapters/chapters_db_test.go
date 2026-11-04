package chapters

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
var night = time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)

// lib is a catalog's titles with their originals on disk, under one root.
type lib struct {
	t    *testing.T
	st   *store.Store
	root string
}

func newLib(t *testing.T) lib {
	return lib{t, storetest.Open(t), t.TempDir()}
}

// title makes the movie or episode id with its original, the source
// sourceID, as its primary asset on disk (none when gone is set), and the
// compliance check's record of it that says whether the source has chapters;
// it returns the original's path.
func (l lib) title(id, typ, sourceID string, hasChapters, gone bool) string {
	l.t.Helper()
	storetest.AddItem(l.t, l.st, id, typ, "Title "+id, "")
	path := filepath.Join(l.root, "media", id, "original.mkv")
	if !gone {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("a film"), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ($1, $2, 'original.mkv', 6, 'present')`, sourceID, id)
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', $3)`, id, path, sourceID)
	kind := typ
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, ruleshash, records)
		VALUES ($1, $2, 'h', jsonb_build_object('srcEssence', jsonb_build_object('chapters', $3::boolean)))`,
		id, kind, hasChapters)
	return path
}

// packaged gives the item a complete version made of its source.
func (l lib) packaged(id, sourceID string) {
	l.t.Helper()
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir, sourceids)
		VALUES ($1, $2, 'complete', $3, ARRAY[$4]::varchar[])`, "v-"+id, id, filepath.Join(l.root, "versions", id), sourceID)
}

func setting(t *testing.T, st *store.Store, key, value string) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE key = $1`, key)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, $1, $2)`,
		key, value)
}

func three() []Chapter {
	return []Chapter{{StartMs: 0, EndMs: 60_000, Title: str("Opening"), Ordinal: 1},
		{StartMs: 60_000, EndMs: 1_200_000, Title: str("Act 1"), Ordinal: 2},
		{StartMs: 1_200_000, EndMs: 1_300_000, Title: nil, Ordinal: 3}}
}

func found(path string, chs []Chapter, marks int32) Report {
	return Report{Read: ReadReport{Path: path, Found: &marks, Probe: "ffprobe version 4.4.2", Seconds: f64(0.4)},
		Chapters: chs}
}

func failed(path, why string) Report {
	return Report{Read: ReadReport{Path: path, Error: why}}
}

func f64(v float64) *float64 { return &v }

func needs(t *testing.T, st *store.Store, id, source string) bool {
	t.Helper()
	n, err := NeedsOriginal(context.Background(), st.Pool(), id, source)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A read that found the chapters of a title's original writes them and keeps
// what it read: the original (by its primary asset's path), its size, the
// marks the header held and those written. The title misses no chapters
// then. A read of the same original that finds none writes none, and keeps
// the title settled; a new original is read again.
func TestAReadWritesTheChaptersAndSettlesTheTitle(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	orig := l.title("m1", "movie", "s1", true, false)
	if !needs(t, l.st, "m1", "s1") || !needs(t, l.st, "m1", "") {
		t.Fatal("a title whose original has chapters and none in the catalog: it needs its original")
	}
	r, err := Record(ctx, l.st, "m1", found(orig, three(), 4), night)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateRead || deref(r.SourceID) != "s1" || *r.Found != 4 || *r.Written != 3 || *r.FileSize != 6 ||
		deref(r.Probe) != "ffprobe version 4.4.2" || !r.ReadAt.Equal(night) || r.Failures != 0 {
		t.Fatalf("the read: %+v", r)
	}
	chs, err := Of(ctx, l.st.Pool(), "m1")
	if err != nil || len(chs) != 3 || deref(chs[1].Title) != "Act 1" || chs[2].Title != nil || chs[2].EndMs != 1_300_000 {
		t.Fatalf("the chapters written: %+v, %v", chs, err)
	}
	if needs(t, l.st, "m1", "s1") {
		t.Error("needs its original once its chapters are in the catalog")
	}

	// read again, and the header holds two marks of no length
	if r, err = Record(ctx, l.st, "m1", found(orig, []Chapter{}, 2), night.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r.State != StateNone || *r.Found != 2 || *r.Written != 0 ||
		storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_itemchapters WHERE item_id = 'm1'`) != 0 {
		t.Fatalf("a read that found none: %+v", r)
	}
	if needs(t, l.st, "m1", "s1") {
		t.Error("a title whose original's header holds no chapter needs it all the same")
	}
	// a new original, its record says chapters too: the read was of another
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted' WHERE id = 's1'`)
	storetest.Exec(t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ('s2', 'm1', 'original.mkv', 9, 'present')`)
	if !needs(t, l.st, "m1", "s2") || needs(t, l.st, "m1", "s1") {
		t.Error("a new original is to be read: the read was of the old one")
	}
}

// A read that failed leaves the title's chapters as they are and counts a
// failure, which waits its backoff; the fifth in a row settles the title as
// unreadable, and a read that finds chapters ends the count. A failure of a
// title whose chapters are in the catalog is none of it.
func TestAFailedReadCountsUntilTheTitleIsUnreadable(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	orig := l.title("m1", "movie", "s1", true, false)
	at := night
	waits := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}
	for i, wait := range waits {
		r, err := Record(ctx, l.st, "m1", failed(orig, "ffprobe did not answer within 120 s"), at)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != StateFailed || r.Failures != int32(i+1) || !r.NextAttemptAt.Equal(at.Add(wait)) ||
			deref(r.Reason) != "ffprobe did not answer within 120 s" || deref(r.SourceID) != "s1" {
			t.Fatalf("failure %d: %+v", i+1, r)
		}
		// the step's retries of the pass, a minute and three later: what
		// failed is said, the failure counted once
		for _, retry := range []time.Duration{time.Minute, 3 * time.Minute} {
			r, err := Record(ctx, l.st, "m1", failed(orig, "the storage is slow"), at.Add(retry))
			if err != nil || r.Failures != int32(i+1) || !r.NextAttemptAt.Equal(at.Add(wait)) ||
				deref(r.Reason) != "the storage is slow" || !r.ReadAt.Equal(at.Add(retry)) {
				t.Fatalf("a retry within failure %d's backoff: %+v, %v", i+1, r, err)
			}
		}
		if !needs(t, l.st, "m1", "s1") {
			t.Fatalf("failure %d settled the title", i+1)
		}
		at = at.Add(wait)
	}
	r, err := Record(ctx, l.st, "m1", failed(orig, "exit 1: Invalid data found when processing input"), at)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateUnreadable || r.Failures != MaxFailures || r.NextAttemptAt != nil ||
		deref(r.Reason) != "5 reads in a row failed, the last: exit 1: Invalid data found when processing input" {
		t.Fatalf("the fifth failure: %+v", r)
	}
	if needs(t, l.st, "m1", "s1") {
		t.Error("an unreadable original is needed for good")
	}
	// it can be read after all: chapters, and the count ends
	if r, err = Record(ctx, l.st, "m1", found(orig, three(), 3), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r.State != StateRead || r.Failures != 0 || r.NextAttemptAt != nil || r.Reason != nil {
		t.Fatalf("read after the failures: %+v", r)
	}
	// a failure now leaves what was read
	if r, err = Record(ctx, l.st, "m1", failed(orig, "the storage is gone"), at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r.State != StateRead || r.Failures != 0 ||
		storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_itemchapters WHERE item_id = 'm1'`) != 3 {
		t.Errorf("a failure after a read: %+v", r)
	}
	// a title whose record says no chapters keeps its failures all the same
	orig2 := l.title("m2", "movie", "s2", false, false)
	if r, err = Record(ctx, l.st, "m2", failed(orig2, "no such file"), night); err != nil || r.State != StateFailed ||
		r.Failures != 1 {
		t.Errorf("a failure of a title without chapters: %+v, %v", r, err)
	}
}

// A report that can't be taken says why; one of no such title, or of a
// series, is refused.
func TestAReportThatCantBeTaken(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	orig := l.title("m1", "movie", "s1", true, false)
	storetest.AddItem(t, l.st, "sh1", "series", "A Show", "")
	var bad *BadReport
	if _, err := Record(ctx, l.st, "sh1", found("/a.mkv", three(), 3), night); !errors.As(err, &bad) {
		t.Errorf("a series: %v", err)
	}
	if _, err := Record(ctx, l.st, "nope", found(orig, three(), 3), night); !errors.Is(err, ErrNoTitle) {
		t.Errorf("no such title: %v", err)
	}
	if _, err := Record(ctx, l.st, "m1", Report{Read: ReadReport{Path: orig, Error: "x"}, Chapters: three()}, night); !errors.As(err, &bad) {
		t.Errorf("a failed read with chapters: %v", err)
	}
	if n := storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_chapterreads`); n != 0 {
		t.Errorf("reports refused left %d reads", n)
	}
}

func service(l lib, now *time.Time) *Service {
	return New(l.st, config.Config{LibraryRoot: l.root, PackagesRoot: filepath.Join(l.root, "packages")}).
		WithClock(func() time.Time { return *now })
}

// The backfill hands out nothing while it is off, nor outside its window,
// saying why and when to ask again; then the titles that miss their chapters,
// the packaged ones first and movies before episodes, never one whose
// chapters are in the catalog, whose record says none, whose original is
// gone, or whose original's read settled it. A title whose original is not on
// disk counts a failure, and the next goes.
func TestTheBackfillHandsOutTheTitlesThatMissTheirChapters(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)

	l.title("e1", "episode", "se1", true, false) // misses them
	l.title("m1", "movie", "sm1", true, false)   // misses them, packaged: first
	l.packaged("m1", "sm1")
	l.title("m2", "movie", "sm2", true, false)  // misses them
	l.title("m3", "movie", "sm3", false, false) // its record says no chapters
	l.title("m4", "movie", "sm4", true, false)  // its chapters are in the catalog
	if _, err := Record(ctx, l.st, "m4", found(filepath.Join(l.root, "media", "m4", "original.mkv"), three(), 3), now); err != nil {
		t.Fatal(err)
	}
	orig5 := l.title("m5", "movie", "sm5", true, false) // read: none
	if _, err := Record(ctx, l.st, "m5", found(orig5, []Chapter{}, 0), now); err != nil {
		t.Fatal(err)
	}
	l.title("m6", "movie", "sm6", true, true) // its original is not on disk
	l.title("m7", "movie", "sm7", true, false)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted' WHERE id = 'sm7'`) // retired

	c, err := s.Claim(ctx, "analyzer-0", 5)
	if err != nil || len(c.Items) != 0 || c.Idle != "chapters.backfill is off" || c.RetryAfterSeconds != 900 {
		t.Fatalf("off: %+v, %v", c, err)
	}
	setting(t, l.st, SettingBackfill, "on")
	now = time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	if c, err = s.Claim(ctx, "analyzer-0", 5); err != nil || len(c.Items) != 0 ||
		c.Idle != "outside chapters.backfill.window (22:00-17:00 UTC)" || c.RetryAfterSeconds != 4*3600 {
		t.Fatalf("in the quiet hours: %+v, %v", c, err)
	}
	now = night
	setting(t, l.st, SettingInflight, "5")
	if c, err = s.Claim(ctx, "analyzer-0", 5); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range c.Items {
		got = append(got, j.ID+"/"+j.SourceID+"/"+j.Type)
		if j.Token == "" || !strings.HasSuffix(j.Path, "/"+j.ID+"/original.mkv") {
			t.Errorf("job %+v", j)
		}
	}
	if strings.Join(got, " ") != "m1/sm1/movie m2/sm2/movie e1/se1/episode" {
		t.Errorf("handed out %v", got)
	}
	if c.Deadline == nil || !c.Deadline.Equal(now.Add(DefaultLease)) {
		t.Errorf("deadline %v, want the lease's end", c.Deadline)
	}
	m6, err := ReadOf(ctx, l.st.Pool(), "m6")
	if err != nil || m6 == nil || m6.State != StateFailed || m6.Failures != 1 ||
		!strings.HasPrefix(deref(m6.Reason), "nothing to read: its original is not at ") ||
		!m6.NextAttemptAt.Equal(now.Add(time.Hour)) || deref(m6.SourceID) != "sm6" {
		t.Errorf("the title whose original is not on disk: %+v, %v", m6, err)
	}
	// all three are being read: none more until they report
	if c2, err := s.Claim(ctx, "analyzer-0", 5); err != nil || len(c2.Items) != 0 ||
		c2.Idle != "no title misses its chapters" {
		t.Errorf("while they are read: %+v, %v", c2, err)
	}
	// m1 reports with its claim's token: its claim ends
	m1 := c.Items[0]
	r, err := s.Record(ctx, "m1", Report{Read: ReadReport{Path: m1.Path, Found: i32(3)}, Chapters: three(), Token: m1.Token})
	if err != nil || r.State != StateRead || r.ClaimedAt != nil || r.Attempts != 1 {
		t.Errorf("m1's report: %+v, %v", r, err)
	}
	// a report of another claim's token leaves m2's claim
	m2 := c.Items[1]
	if r, err = s.Record(ctx, "m2", Report{Released: true, Token: "not-its-token"}); err != nil || r.ClaimedAt == nil {
		t.Errorf("a release of another claim: %+v, %v", r, err)
	}
	// m2 is given back: it waits a quarter of an hour
	if r, err = s.Record(ctx, "m2", Report{Released: true, Token: m2.Token}); err != nil || r.ClaimedAt != nil ||
		!r.NextAttemptAt.Equal(now.Add(ReleaseWait)) || r.State != StatePending || r.Failures != 0 {
		t.Errorf("m2 given back: %+v, %v", r, err)
	}
}

// A claim whose lease ended counts as a failure of its title: it waits its
// backoff, and its run's failure reported late is not counted again, nor
// does it end the claim made since. The hour's rate, and the titles in
// flight, hold the next back; a claim made near the window's end must be
// read by it.
func TestTheBackfillsLeaseRateAndWindow(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	l.title("m1", "movie", "s1", true, false)
	l.title("m2", "movie", "s2", true, false)
	l.title("m3", "movie", "s3", true, false)

	c, err := s.Claim(ctx, "analyzer-0", 5)
	if err != nil || len(c.Items) != 1 || c.Items[0].ID != "m1" {
		t.Fatalf("one at a time: %+v, %v", c, err)
	}
	lost := c.Items[0]
	now = now.Add(time.Minute)
	if c, err = s.Claim(ctx, "analyzer-0", 5); err != nil || len(c.Items) != 0 ||
		c.Idle != "1 titles are being read (chapters.backfill.inflight 1)" || c.RetryAfterSeconds != 60 {
		t.Fatalf("one in flight: %+v, %v", c, err)
	}
	// its run never reports: its lease ends, a failure of m1, and m2 goes
	now = night.Add(DefaultLease + time.Second)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 || c.Items[0].ID != "m2" {
		t.Fatalf("after the lease: %+v, %v", c, err)
	}
	r, err := ReadOf(ctx, l.st.Pool(), "m1")
	if err != nil || r.State != StateFailed || r.Failures != 1 || r.ClaimedAt != nil ||
		deref(r.LapsedToken) != lost.Token || !r.NextAttemptAt.Equal(now.Add(time.Hour)) ||
		deref(r.Reason) != "its run did not report back within 30m" {
		t.Fatalf("m1 after its lease: %+v, %v", r, err)
	}
	// the lost run reports its failure late: counted already
	if r, err = s.Record(ctx, "m1", Report{Read: ReadReport{Path: lost.Path, Error: "killed"}, Token: lost.Token}); err != nil ||
		r.Failures != 1 || deref(r.Reason) != "killed" || r.LapsedToken != nil {
		t.Errorf("a late failure of a lapsed claim: %+v, %v", r, err)
	}
	// m2 reports; the rate holds m3 back
	setting(t, l.st, SettingRate, "2")
	m2 := c.Items[0]
	if _, err := s.Record(ctx, "m2", found(m2.Path, three(), 3)); err != nil {
		t.Fatal(err) // without its token: the analyzer's pass; the claim stays
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), "m2"); r.ClaimedAt == nil {
		t.Error("a pass's report ended the backfill's claim")
	}
	if _, err := s.Record(ctx, "m2", Report{Read: ReadReport{Path: m2.Path, Found: i32(3)}, Chapters: three(), Token: m2.Token}); err != nil {
		t.Fatal(err)
	}
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 0 ||
		c.Idle != "2 titles were handed out in the last hour (chapters.backfill.rate 2)" ||
		c.RetryAfterSeconds != int((time.Hour-DefaultLease-time.Second).Seconds()) {
		t.Fatalf("the hour's rate: %+v, %v", c, err)
	}
	// later m1's backoff has passed, and it goes again before m3; at 16:50
	// its deadline is the window's end
	setting(t, l.st, SettingRate, "120")
	now = time.Date(2026, 11, 4, 16, 50, 0, 0, time.UTC)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 || c.Items[0].ID != "m1" ||
		!c.Deadline.Equal(time.Date(2026, 11, 4, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("near the window's end: %+v, %v", c, err)
	}
	// a window that is none hands out nothing, and says so
	setting(t, l.st, SettingWindow, "after dinner")
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || c.Idle != "chapters.backfill.window is no window" {
		t.Errorf("no window: %+v, %v", c, err)
	}
}

// A title whose reads keep failing in the backfill is settled as unreadable
// after the fifth, and handed out no more; how far the chapters are says so,
// and a deleted title's read goes with it.
func TestTheBackfillEndsWithEveryTitleSettled(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	l.title("m1", "movie", "s1", true, false)
	l.packaged("m1", "s1")
	l.title("e1", "episode", "s2", true, false)

	p, err := s.Progress(ctx)
	if err != nil || p.Missing != 2 || p.MissingPackaged != 1 || p.Idle != "" || !p.Settings.On {
		t.Fatalf("progress before: %+v, %v", p, err)
	}
	for i := 1; i <= MaxFailures; i++ {
		c, err := s.Claim(ctx, "analyzer-0", 1)
		if err != nil || len(c.Items) != 1 || c.Items[0].ID != "m1" {
			t.Fatalf("claim %d: %+v, %v", i, c, err)
		}
		j := c.Items[0]
		r, err := s.Record(ctx, "m1", Report{Read: ReadReport{Path: j.Path, Error: "exit 1: moov atom not found"}, Token: j.Token})
		if err != nil || r.Failures != int32(i) {
			t.Fatalf("failure %d: %+v, %v", i, r, err)
		}
		if r.NextAttemptAt != nil {
			now = *r.NextAttemptAt
			if h := now.Hour(); h >= 17 && h < 22 { // the quiet hours: the window opens at 22:00
				now = time.Date(now.Year(), now.Month(), now.Day(), 22, 0, 0, 0, time.UTC)
			}
		}
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), "m1"); r.State != StateUnreadable || r.Attempts != MaxFailures {
		t.Fatalf("m1 after its failures: %+v", r)
	}
	// e1 is next, and reads none: every title is settled
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 || c.Items[0].ID != "e1" {
		t.Fatalf("e1: %+v, %v", c, err)
	}
	if _, err := s.Record(ctx, "e1", Report{Read: ReadReport{Path: c.Items[0].Path, Found: i32(0)}, Chapters: []Chapter{},
		Token: c.Items[0].Token}); err != nil {
		t.Fatal(err)
	}
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 0 || c.Idle != "no title misses its chapters" ||
		c.RetryAfterSeconds != 3600 {
		t.Errorf("all settled: %+v, %v", c, err)
	}
	p, err = s.Progress(ctx)
	if err != nil || p.Missing != 0 || p.None != 1 || p.Unreadable != 1 || p.Read != 0 || p.Failed != 0 ||
		p.InFlight != 0 || p.Idle != "no title misses its chapters" || p.LastReadAt == nil {
		t.Errorf("progress after: %+v, %v", p, err)
	}
	title, err := s.Of(ctx, "e1")
	if err != nil || title.Missing || title.Read == nil || title.Read.State != StateNone || len(title.Chapters) != 0 {
		t.Errorf("e1: %+v, %v", title, err)
	}
	if _, err := s.Of(ctx, "nope"); !errors.Is(err, ErrNoTitle) {
		t.Errorf("no such title: %v", err)
	}
	if _, err := l.st.DeleteItems(ctx, []string{"m1"}, store.Deletion{By: "a test"}); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_chapterreads WHERE item_id = 'm1'`); n != 0 {
		t.Errorf("a deleted title left %d reads", n)
	}
}

// Without migration 055 the backfill and the reads say so; NeedsOriginal
// keeps the original.
func TestWithoutTheMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	now := night
	s := New(st, config.Config{}).WithClock(func() time.Time { return now })
	if _, err := s.Claim(ctx, "analyzer-0", 1); !errors.Is(err, ErrNotReady) {
		t.Errorf("a claim: %v", err)
	}
	if _, err := s.Record(ctx, "m1", found("/m.mkv", three(), 3)); !errors.Is(err, ErrNotReady) {
		t.Errorf("a report: %v", err)
	}
	if n, err := NeedsOriginal(ctx, st.Pool(), "m1", "s1"); !n || !errors.Is(err, ErrNotReady) {
		t.Errorf("NeedsOriginal: %v, %v", n, err)
	}
}
