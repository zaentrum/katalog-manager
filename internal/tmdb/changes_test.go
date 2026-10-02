package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// syncNow is when the change lists run in these tests: the 2nd of October,
// mid-morning UTC.
var syncNow = time.Date(2026, 10, 2, 10, 30, 0, 0, time.UTC)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// A run reads at most 14 days per call, and a window starts on the day the one
// before it ended, so none falls between two.
func TestChangeWindows(t *testing.T) {
	for _, tc := range []struct{ from, to, want string }{
		{"2026-10-02", "2026-10-02", "2026-10-02..2026-10-02"},
		{"2026-10-01", "2026-10-02", "2026-10-01..2026-10-02"},
		{"2026-09-19", "2026-10-02", "2026-09-19..2026-10-02"}, // 14 days: one call
		{"2026-09-18", "2026-10-02", "2026-09-18..2026-10-01 2026-10-01..2026-10-02"},
		{"2026-09-12", "2026-10-02", "2026-09-12..2026-09-25 2026-09-25..2026-10-02"},
		{"2026-08-01", "2026-09-10", "2026-08-01..2026-08-14 2026-08-14..2026-08-27 2026-08-27..2026-09-09 2026-09-09..2026-09-10"},
		{"2026-10-03", "2026-10-02", ""},
	} {
		var got []string
		for _, w := range changeWindows(day(tc.from), day(tc.to)) {
			if w.days() > maxChangeDays {
				t.Errorf("%s..%s: a window of %d days", tc.from, tc.to, w.days())
			}
			got = append(got, w.from.Format(time.DateOnly)+".."+w.to.Format(time.DateOnly))
		}
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%s..%s: %v, want %s", tc.from, tc.to, got, tc.want)
		}
	}
}

// The lists run an interval after the earliest of their last runs, and at
// once when one never ran.
func TestDueIn(t *testing.T) {
	now := syncNow
	all := func(ago time.Duration) map[string]time.Time {
		return map[string]time.Time{"person": now.Add(-ago), "movie": now.Add(-ago), "tv": now.Add(-ago)}
	}
	if d := dueIn(nil, 24*time.Hour, now); d != 0 {
		t.Errorf("never ran: due in %s", d)
	}
	if d := dueIn(all(time.Hour), 24*time.Hour, now); d != 23*time.Hour {
		t.Errorf("ran an hour ago: due in %s, want 23h", d)
	}
	if d := dueIn(all(25*time.Hour), 24*time.Hour, now); d != 0 {
		t.Errorf("ran 25 hours ago: due in %s, want now", d)
	}
	last := all(time.Hour)
	last["movie"] = now.Add(-30 * time.Hour)
	if d := dueIn(last, 24*time.Hour, now); d != 0 {
		t.Errorf("one list ran 30 hours ago: due in %s, want now", d)
	}
	delete(last, "tv")
	if d := dueIn(all(time.Hour), 24*time.Hour, now); d == 0 {
		t.Error("all ran an hour ago, yet due")
	}
	if d := dueIn(last, 24*time.Hour, now); d != 0 {
		t.Errorf("a list that never ran: due in %s, want now", d)
	}
}

// cursor is a change list's row: "cursor changes/matched/refreshed/skipped/failed error".
func cursor(t *testing.T, st *store.Store, kind string) string {
	t.Helper()
	var c, e string
	var counts [5]*int
	var ran bool
	err := st.Pool().QueryRow(context.Background(), `SELECT to_char(cursor, 'YYYY-MM-DD'), COALESCE(lastrunerror, '-'),
			lastrunchanges, lastrunmatched, lastrunrefreshed, lastrunskipped, lastrunfailed, lastrunat > now() - interval '1 minute'
		FROM com_nalet_katalog_referencesync WHERE kind = $1`, kind).
		Scan(&c, &e, &counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &ran)
	if err != nil {
		return "none: " + err.Error()
	}
	if !ran {
		t.Errorf("%s: lastrunat is not now", kind)
	}
	n := make([]string, 5)
	for i, p := range counts {
		n[i] = "-"
		if p != nil {
			n[i] = strconv.Itoa(*p)
		}
	}
	return c + " " + strings.Join(n, "/") + " " + e
}

func runSync(t *testing.T, s *Service, now time.Time) {
	t.Helper()
	if _, err := s.SyncChanges(context.Background(), now); err != nil {
		t.Fatalf("SyncChanges: %v", err)
	}
}

// A list that never ran starts today: no historical sweep.
func TestSyncChangesFirstRunStartsToday(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('ada', 'Ada', '101'), ('ben', 'Ben', '102')`)
	f.person(101, &fakePerson{Name: "Ada Example"})
	f.person(102, &fakePerson{Name: "Ben Example"})
	f.changed("person", "2026-10-02", 101)
	f.changed("person", "2026-09-30", 102) // before the first run: never read

	runSync(t, s, syncNow)

	for _, kind := range changeKinds {
		if got := f.changeReads(kind); len(got) != 1 || got[0] != "2026-10-02..2026-10-02 p1" {
			t.Errorf("%s list read %q, want today only", kind, got)
		}
	}
	if got, want := cursor(t, st, "person"), "2026-10-02 1/1/1/0/0 -"; got != want {
		t.Errorf("person cursor: %s, want %s", got, want)
	}
	if got, want := cursor(t, st, "movie"), "2026-10-02 0/0/0/0/0 -"; got != want {
		t.Errorf("movie cursor: %s, want %s", got, want)
	}
	samePerson(t, st, "ben", `{"name": "Ben", "sortName": null, "aka": null, "birth": null, "death": null, "place": null,
		"bio": null, "tmdb": "102", "imdb": null, "dept": null, "locked": false, "lockedFields": null, "origins": null,
		"changed": null, "fetched": false}`)
}

// A run pages through each list from its cursor to today in windows of at most
// 14 days, and refreshes only what the catalog holds: people by their TMDB id,
// movies and series by theirs, each in its own list. Locks are honoured, and a
// skip (a locked record, an id TMDB no longer knows) does not hold the cursor.
func TestSyncChangesPagesThroughWindowsAndRefreshesWhatTheCatalogHolds(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	f.pageSize = 2
	s := newTestService(t, st, f, "en-US")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_referencesync (kind, cursor) VALUES
		('person', '2026-09-12'), ('movie', '2026-09-12'), ('tv', '2026-10-01')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid, metadatalocked) VALUES
		('ada', 'Ada', '101', false), ('ben', 'Ben', '102', true), ('cy', 'Cy', '103', false),
		('dot', 'Dot', '104', false), ('eve', 'Eve', '905', false)`)
	for id, name := range map[int64]string{101: "Ada Example", 102: "Ben Example", 103: "Cy Example", 104: "Dot Example"} {
		f.person(id, &fakePerson{Name: name})
	}
	f.changed("person", "2026-09-11", 104) // the day before the cursor
	f.changed("person", "2026-09-13", 101, 900, 901)
	f.changed("person", "2026-09-25", 902)
	f.changed("person", "2026-10-01", 103, 903, 905)
	f.changed("person", "2026-10-02", 102)
	addTitle(t, st, film1, "movie", "Old Title One", 10)
	addTitle(t, st, film2, "movie", "Old Title Two", 11)
	addTitle(t, st, show1, "series", "Old Show", 20)
	f.movie(10, "First Film")
	f.movie(11, "Second Film")
	f.tv(20, "A Show")
	f.changed("movie", "2026-09-20", 10, 500, 20) // 20 is the show's id, but this is the movie list
	f.changed("movie", "2026-09-30", 11)
	f.changed("tv", "2026-10-02", 20, 11) // and 11 a movie's

	runSync(t, s, syncNow)

	if got, want := strings.Join(f.changeReads("person"), ", "),
		"2026-09-12..2026-09-25 p1, 2026-09-12..2026-09-25 p2, 2026-09-25..2026-10-02 p1, 2026-09-25..2026-10-02 p2, "+
			"2026-09-25..2026-10-02 p3"; got != want {
		t.Errorf("person list read\n %s\nwant\n %s", got, want)
	}
	if got, want := strings.Join(f.changeReads("tv"), ", "), "2026-10-01..2026-10-02 p1"; got != want {
		t.Errorf("tv list read %s, want %s", got, want)
	}
	for _, kind := range changeKinds {
		for _, r := range f.calls("/3/" + kind + "/changes?") {
			q, _ := url.ParseQuery(r[strings.Index(r, "?")+1:])
			if days := day(q.Get("end_date")).Sub(day(q.Get("start_date"))).Hours()/24 + 1; days > 14 {
				t.Errorf("%s: one call over %v days", r, days)
			}
		}
	}

	// 8 ids listed, 5 held: Ada and Cy refreshed, Ben locked, Eve unknown to
	// TMDB, Dot changed before the cursor and left alone.
	if got, want := cursor(t, st, "person"), "2026-10-02 8/4/2/2/0 -"; got != want {
		t.Errorf("person cursor: %s, want %s", got, want)
	}
	for id, want := range map[string]string{"ada": "2026-09-25", "cy": "2026-10-02"} {
		var changed string
		storetestScan(t, st, `SELECT to_char(tmdbchangedat, 'YYYY-MM-DD') FROM com_nalet_katalog_people WHERE id = '`+id+`'`, &changed)
		if changed != want {
			t.Errorf("%s changed on %s, want the end of the window that listed them, %s", id, changed, want)
		}
	}
	samePerson(t, st, "ben", `{"name": "Ben", "sortName": null, "aka": null, "birth": null, "death": null, "place": null,
		"bio": null, "tmdb": "102", "imdb": null, "dept": null, "locked": true, "lockedFields": null, "origins": null,
		"changed": null, "fetched": false}`)
	if got := f.calls("/3/person/104"); len(got) != 0 {
		t.Errorf("a person changed before the cursor was read: %q", got)
	}
	for _, id := range []string{"900", "901", "902", "903"} {
		if got := f.calls("/3/person/" + id); len(got) != 0 {
			t.Errorf("a person the catalog does not hold was read: %q", got)
		}
	}

	if got, want := cursor(t, st, "movie"), "2026-10-02 4/2/2/0/0 -"; got != want {
		t.Errorf("movie cursor: %s, want %s", got, want)
	}
	if got, want := cursor(t, st, "tv"), "2026-10-02 2/1/1/0/0 -"; got != want {
		t.Errorf("tv cursor: %s, want %s", got, want)
	}
	for id, want := range map[string]string{film1: "First Film", film2: "Second Film", show1: "A Show"} {
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND title = $2`, id, want); n != 1 {
			t.Errorf("%s was not enriched again to %q", id, want)
		}
	}
	if got := f.calls("/3/tv/20?"); len(got) != 1 {
		t.Errorf("the show was read %d times, want once, from the tv list: %q", len(got), got)
	}
	if got := f.calls("/3/movie/20?"); len(got) != 0 {
		t.Errorf("a tv id in the movie list was taken for a movie: %q", got)
	}

	// The next day re-reads the cursor's day, which may have changed since.
	f.forget()
	runSync(t, s, syncNow.Add(24*time.Hour))
	if got := f.changeReads("person"); len(got) != 1 || got[0] != "2026-10-02..2026-10-03 p1" {
		t.Errorf("the next day read %q, want 2026-10-02..2026-10-03", got)
	}
}

// The cursor moves only after a run that read the whole list and refreshed
// everything it held; otherwise the next run reads the same days again.
func TestSyncChangesCursorAdvancesOnlyAfterSuccess(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	f.pageSize = 1
	s := newTestService(t, st, f, "en-US")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_referencesync (kind, cursor) VALUES ('person', '2026-09-28')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('ada', 'Ada', '101')`)
	f.person(101, &fakePerson{Name: "Ada Example"})
	f.changed("person", "2026-09-29", 101, 900)

	// Page 2 fails: nothing is refreshed, not even what page 1 named.
	f.failWhen(func(path string, q url.Values) int {
		if path == "/3/person/changes" && q.Get("page") == "2" {
			return http.StatusInternalServerError
		}
		return 0
	})
	runSync(t, s, syncNow)
	if got := cursor(t, st, "person"); !strings.HasPrefix(got, "2026-09-28 0/0/0/0/0 read 2026-09-28..2026-10-02 page 2: ") {
		t.Errorf("after a page failed: %s", got)
	}
	if got := f.calls("/3/person/101"); len(got) != 0 {
		t.Errorf("a list read halfway refreshed what it named: %q", got)
	}
	if got := cursor(t, st, "movie"); got != "2026-10-02 0/0/0/0/0 -" {
		t.Errorf("a failure in the person list held the movie list: %s", got)
	}

	// The list reads, the refresh fails.
	f.failWhen(nil)
	f.failing("/3/person/101", http.StatusBadGateway)
	runSync(t, s, syncNow)
	if got, want := cursor(t, st, "person"), "2026-09-28 2/1/0/0/1 1 of 1 refreshes failed"; got != want {
		t.Errorf("after a refresh failed: %s, want %s", got, want)
	}

	// TMDB is back: the same days again, and the cursor moves.
	f.failing("/3/person/101", 0)
	f.forget()
	runSync(t, s, syncNow)
	if got := f.changeReads("person"); len(got) != 2 || got[0] != "2026-09-28..2026-10-02 p1" {
		t.Errorf("the run after the failures read %q, want 2026-09-28..2026-10-02 again", got)
	}
	if got, want := cursor(t, st, "person"), "2026-10-02 2/1/1/0/0 -"; got != want {
		t.Errorf("after TMDB came back: %s, want %s", got, want)
	}
}

// A window with more pages than TMDB serves is read again a day at a time; a
// day with more is read as far as TMDB serves it, which the run reports, and
// the cursor moves on.
func TestSyncChangesSplitsAWindowTooBigToPage(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	f.pageSize, f.maxPage = 1, 2
	s := newTestService(t, st, f, "en-US")
	s.maxChangePages = 2
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_referencesync (kind, cursor) VALUES ('person', '2026-09-28')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES
		('ada', 'Ada', '101'), ('ben', 'Ben', '102'), ('cy', 'Cy', '103')`)
	for _, id := range []int64{101, 102, 103} {
		f.person(id, &fakePerson{Name: "Someone"})
	}
	f.changed("person", "2026-09-29", 101)
	f.changed("person", "2026-09-30", 102)
	f.changed("person", "2026-10-01", 103)

	runSync(t, s, syncNow)
	if got, want := strings.Join(f.changeReads("person"), ", "), "2026-09-28..2026-10-02 p1, "+
		"2026-09-28..2026-09-28 p1, 2026-09-29..2026-09-29 p1, 2026-09-30..2026-09-30 p1, "+
		"2026-10-01..2026-10-01 p1, 2026-10-02..2026-10-02 p1"; got != want {
		t.Errorf("read\n %s\nwant\n %s", got, want)
	}
	if got, want := cursor(t, st, "person"), "2026-10-02 3/3/3/0/0 -"; got != want {
		t.Errorf("cursor %s, want %s", got, want)
	}

	f.forget()
	f.changed("person", "2026-10-03", 101, 102, 103)
	runSync(t, s, syncNow.Add(24*time.Hour))
	if got, want := strings.Join(f.changeReads("person"), ", "), "2026-10-02..2026-10-03 p1, "+
		"2026-10-02..2026-10-02 p1, 2026-10-03..2026-10-03 p1, 2026-10-03..2026-10-03 p2"; got != want {
		t.Errorf("read\n %s\nwant\n %s", got, want)
	}
	if got, want := cursor(t, st, "person"), "2026-10-03 2/2/2/0/0 2026-10-03 lists more than 2 pages; read the first 2"; got != want {
		t.Errorf("cursor %s, want %s", got, want)
	}
}

// The lock is per catalog: a catalog in another schema of the same database
// runs its lists while one runs.
func TestSyncChangesOfTwoCatalogsDoNotWaitForEachOther(t *testing.T) {
	busy, free := newFakeTMDB(t), newFakeTMDB(t)
	first := newTestService(t, storetest.Open(t), busy, "en-US")
	second := newTestService(t, storetest.Open(t), free, "en-US")
	arrived, release := busy.hold("/3/person/changes")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := first.SyncChanges(context.Background(), syncNow)
		done <- err
	}()
	<-arrived
	if _, err := second.SyncChanges(context.Background(), syncNow); err != nil {
		t.Errorf("another catalog's lists while the first runs: %v", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// One instance runs the lists at a time; the lists need TMDB and migration 030.
func TestSyncChangesRunsOnceAtATime(t *testing.T) {
	st := storetest.Open(t)
	f := newFakeTMDB(t)
	s := newTestService(t, st, f, "en-US")
	arrived, release := f.hold("/3/person/changes")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := s.SyncChanges(context.Background(), syncNow)
		done <- err
	}()
	<-arrived
	other := newTestService(t, st, f, "en-US") // another instance, same database
	if _, err := other.SyncChanges(context.Background(), syncNow); !errors.Is(err, errSyncBusy) {
		t.Errorf("a second instance while one runs: %v, want busy", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := other.SyncChanges(context.Background(), syncNow); err != nil {
		t.Errorf("after the first finished: %v", err)
	}

	off := newTestService(t, st, f, "en-US")
	off.tmdb.key = func() string { return "" }
	if _, err := off.SyncChanges(context.Background(), syncNow); err == nil {
		t.Error("the lists ran without a TMDB key")
	}
	if _, err := newTestService(t, storetest.OpenBase(t), f, "en-US").SyncChanges(context.Background(), syncNow); err == nil {
		t.Error("the lists ran on a catalog without migration 030")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	finished := make(chan struct{})
	go func() { s.RunChangeSync(ctx, 24*time.Hour); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Error("RunChangeSync did not stop with its context")
	}
	s.RunChangeSync(context.Background(), 0) // disabled: returns at once
}
