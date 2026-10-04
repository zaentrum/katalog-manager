package retry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// file gives each item a primary asset: the file the transcoder encodes.
func file(t *testing.T, st *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
			VALUES (gen_random_uuid()::varchar, $1::varchar, '/media/' || $1::varchar, true)`, id)
	}
}

// chainState is a step as a re-encode leaves it: its status, failures in a
// row, attempts, error, last error, whether a retry is scheduled, whether its
// trigger is noted as sent, and whether it holds a run's times.
func chainState(t *testing.T, st *store.Store, item, step string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT status || ' ' || failures || ' attempts=' || COALESCE(attempts::text, '-') ||
		' error=' || COALESCE(error, '-') || ' last=' || COALESCE(lasterror, '-') || ' retry=' || (nextretryat IS NOT NULL) ||
		' sent=' || (dispatchedat IS NOT NULL) || ' times=' || (startedat IS NOT NULL OR finishedat IS NOT NULL)
		FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = $2`, item, step).Scan(&out); err != nil {
		return "none"
	}
	return out
}

// rowsOf is every column of an item's steps, to tell that nothing changed.
func rowsOf(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT coalesce(string_agg(s::text, '|' ORDER BY s.step), '')
		FROM com_nalet_katalog_itemprocessingsteps s WHERE item_id = $1`, item).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A finished title is encoded again: its transcode and package wait for their
// workers afresh (no failures, no retry, no run's times or error; attempts
// and last error kept), the transcode noted as sent, and the transcoder told
// with the event it consumes, not marked as a retry. While the transcoder
// runs it, a re-encode is refused, and the package, not sent, is not reaped
// however long the transcode takes.
func TestAReencodeRunsAFinishedTitleAgain(t *testing.T) {
	st := catalog(t)
	file(t, st, "m1")
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "done", `lasterror = 'out of memory', details = 'profile=hevc-1080p', startedat = localtimestamp - interval '3 hours',
		finishedat = localtimestamp - interval '2 hours'`)
	put(t, st, "m1", "package", "done", `startedat = localtimestamp - interval '2 hours', finishedat = localtimestamp - interval '1 hour'`)
	put(t, st, "m1", "subtitle", "done", "")

	res, err := s.ReencodeItem(context.Background(), "m1")
	if err != nil || res.ItemID != "m1" || res.Titles != 1 || res.Reencoded != 1 || res.Busy != 0 || res.NotSent != 0 ||
		!strings.HasPrefix(res.Message, "encoding it again: its transcode and package wait for their workers ("+events.TopicAnalyzed+" sent)") {
		t.Fatalf("ReencodeItem(m1): %+v, %v", res, err)
	}
	if got := strings.Join(b.take(), "\n"); got != events.TopicAnalyzed+" m1 transcode reencode reencode movie" {
		t.Errorf("sent %q, want the transcoder's trigger, not marked as a retry", got)
	}
	if got := chainState(t, st, "m1", "transcode"); got != "pending 0 attempts=1 error=- last=out of memory retry=false sent=true times=false" {
		t.Errorf("the transcode: %s", got)
	}
	if got := chainState(t, st, "m1", "package"); got != "pending 0 attempts=1 error=- last=- retry=false sent=false times=false" {
		t.Errorf("the package: %s, want it waiting, not noted as sent", got)
	}
	if got := chainState(t, st, "m1", "subtitle"); !strings.HasPrefix(got, "done ") {
		t.Errorf("a step not of the chain was touched: %s", got)
	}

	// the transcoder starts it: a second run; a re-encode now is refused
	steps := processing.New(st.Pool()).WithPolicy(testPolicy)
	if err := steps.Upsert(context.Background(), "m1", "transcode", processing.StatusInProgress, nil, nil); err != nil {
		t.Fatal(err)
	}
	before := rowsOf(t, st, "m1")
	res, err = s.ReencodeItem(context.Background(), "m1")
	if err != nil || res.Reencoded != 0 || res.Busy != 1 ||
		!strings.HasPrefix(res.Message, "transcode is running: its worker last reported at ") ||
		!strings.HasSuffix(res.Message, "encoding it again would run it twice; wait for it, or for its timeout of 1h") {
		t.Errorf("a re-encode while the transcode runs: %+v, %v", res, err)
	}
	if sent := b.take(); len(sent) != 0 || rowsOf(t, st, "m1") != before {
		t.Errorf("a refused re-encode sent %v or changed the steps", sent)
	}
	if got := chainState(t, st, "m1", "transcode"); got != "in_progress 0 attempts=2 error=- last=out of memory retry=false sent=false times=true" {
		t.Errorf("the transcode its worker started: %s", got)
	}

	// a transcode longer than the package's timeout: the package waits on
	// it, and the reaper leaves it alone
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET modifiedat = localtimestamp - interval '3 hours'
		WHERE item_id = 'm1' AND step = 'package'`)
	if reaped, sent, err := s.Sweep(context.Background()); err != nil || reaped != 0 || sent != 0 {
		t.Errorf("the sweep during a long transcode: reaped %d, sent %d, %v", reaped, sent, err)
	}
	if got := chainState(t, st, "m1", "package"); got != "pending 0 attempts=1 error=- last=- retry=false sent=false times=false" {
		t.Errorf("the package after the sweep: %s", got)
	}
}

// A title whose transcode or package is running, or waiting for its worker,
// within the step's timeout is left alone, nothing sent or changed (not even
// a step it lacked); one silent or waiting past it, failed, or done is
// encoded again. A package waiting while its transcode has not finished
// waits for that transcode, unless it was sent again itself.
func TestAReencodeLeavesABusyTitleAlone(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	const ago5m, ago3h = `modifiedat = localtimestamp - interval '5 minutes'`, `modifiedat = localtimestamp - interval '3 hours'`
	for i, tc := range []struct {
		name            string
		transcode, tSet string // "" for no step
		pkg, pSet       string
		busy            bool
		says            string
		transcodeAfter  string // a prefix of its chainState after, "" to skip
		packageAfter    string
	}{
		{"a transcode running", "in_progress", ago5m, "done", "", true, "transcode is running: its worker last reported at", "", ""},
		{"a transcode silent past its timeout", "in_progress", `modifiedat = localtimestamp - interval '61 minutes'`, "done", "", false, "", "pending 0 ", "pending 0 "},
		{"a package running", "done", "", "in_progress", ago5m, true, "package is running: its worker last reported at", "", ""},
		{"a package silent past its timeout", "done", "", "in_progress", ago3h, false, "", "pending 0 ", "pending 0 "},
		{"a transcode sent and not started", "pending", `dispatchedat = now() - interval '1 minute'`, "done", "", true, "transcode is waiting for its worker since", "", ""},
		{"a transcode sent long ago", "pending", `dispatchedat = now() - interval '2 hours', ` + ago3h, "done", "", false, "", "pending 0 ", "pending 0 "},
		{"a package retry on its way", "done", "", "pending", `dispatchedat = now() - interval '1 minute'`, true, "package is waiting for its worker since", "", ""},
		{"a package promoted, its event on its way", "done", "", "pending", ago5m, true, "package is waiting for its worker since", "", ""},
		{"a package promoted long ago", "done", "", "pending", ago3h, false, "", "pending 0 ", "pending 0 "},
		{"a package waiting for a failed transcode", "failed", `failures = 1, nextretryat = now() + interval '1 minute'`, "pending", ago5m, false, "",
			"pending 0 attempts=1 error=- last=- retry=false sent=true", "pending 0 attempts=1 error=- last=- retry=false sent=false"},
		{"a package retry on its way, its transcode failed", "failed", "", "pending", `dispatchedat = now() - interval '1 minute'`, true, "package is waiting", "", ""},
		{"a package failed, its retry due", "done", "", "failed", `failures = 2, error = 'shaka exited 1', lasterror = 'shaka exited 1', nextretryat = now() - interval '1 second'`, false, "",
			"pending 0 ", "pending 0 attempts=1 error=- last=shaka exited 1 retry=false sent=false times=false"},
		{"a source that needed no encode", "not_applicable", "", "done", "", false, "", "pending 0 ", "pending 0 "},
		{"a transcode skipped", "skipped", "", "done", "", false, "", "pending 0 ", "pending 0 "},
		{"no steps yet", "", "", "", "", false, "",
			"pending 0 attempts=0 error=- last=- retry=false sent=true times=false", "pending 0 attempts=0 error=- last=- retry=false sent=false times=false"},
		{"a transcode running, no package yet", "in_progress", ago5m, "", "", true, "transcode is running", "", "none"},
		{"a package running, no transcode", "", "", "in_progress", ago5m, true, "package is running", "none", ""},
		{"a package waiting, no transcode", "", "", "pending", ago5m, true, "package is waiting for its worker", "none", ""},
	} {
		item := fmt.Sprintf("c%02d", i)
		storetest.AddItem(t, st, item, "movie", tc.name, "")
		file(t, st, item)
		if tc.transcode != "" {
			put(t, st, item, "transcode", tc.transcode, tc.tSet)
		}
		if tc.pkg != "" {
			put(t, st, item, "package", tc.pkg, tc.pSet)
		}
		before := rowsOf(t, st, item)
		res, err := s.ReencodeItem(context.Background(), item)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		sent := b.take()
		if tc.busy {
			if res.Busy != 1 || res.Reencoded != 0 || !strings.Contains(res.Message, tc.says) || len(sent) != 0 {
				t.Errorf("%s: %+v, sent %v; want it left alone, saying %q", tc.name, res, sent, tc.says)
			}
			if after := rowsOf(t, st, item); after != before {
				t.Errorf("%s: a title left alone changed:\nbefore %s\nafter  %s", tc.name, before, after)
			}
		} else if res.Reencoded != 1 || res.Busy != 0 || len(sent) != 1 || sent[0] != events.TopicAnalyzed+" "+item+" transcode reencode reencode movie" {
			t.Errorf("%s: %+v, sent %v; want it encoded again", tc.name, res, sent)
		}
		if tc.transcodeAfter != "" && !strings.HasPrefix(chainState(t, st, item, "transcode"), tc.transcodeAfter) {
			t.Errorf("%s: the transcode after: %s, want %s…", tc.name, chainState(t, st, item, "transcode"), tc.transcodeAfter)
		}
		if tc.packageAfter != "" && !strings.HasPrefix(chainState(t, st, item, "package"), tc.packageAfter) {
			t.Errorf("%s: the package after: %s, want %s…", tc.name, chainState(t, st, item, "package"), tc.packageAfter)
		}
	}
}

// A series fans out to its episodes that have a file, under it or under a
// season of it, each as a title of its own: those busy are left alone and
// counted, an episode without a file is not looked at. A series without an
// episode with a file, and a movie without one, encode nothing.
func TestAReencodeOfASeries(t *testing.T) {
	st := catalog(t)
	storetest.AddItem(t, st, "e2", "episode", "Second", "s1")
	storetest.AddItem(t, st, "e3", "episode", "No File", "s1")
	storetest.AddItem(t, st, "season2", "season", "Season 2", "s1")
	storetest.AddItem(t, st, "e4", "episode", "Under A Season", "season2")
	storetest.AddItem(t, st, "e5", "episode", "Busy", "s1")
	storetest.AddItem(t, st, "s2", "series", "Nothing On Disk", "")
	storetest.AddItem(t, st, "e6", "episode", "Elsewhere", "s2")
	file(t, st, "e1", "e2", "e4", "e5")
	put(t, st, "e1", "transcode", "done", "")
	put(t, st, "e1", "package", "done", "")
	put(t, st, "e2", "transcode", "not_applicable", "")
	put(t, st, "e2", "package", "failed", `failures = 3`)
	put(t, st, "e5", "transcode", "done", "")
	put(t, st, "e5", "package", "in_progress", `modifiedat = localtimestamp - interval '5 minutes'`)
	b := &bus{}
	s := newService(t, st, b)

	res, err := s.ReencodeItem(context.Background(), "s1")
	if err != nil || res.ItemID != "s1" || res.Titles != 4 || res.Reencoded != 3 || res.Busy != 1 || res.NotSent != 0 {
		t.Fatalf("ReencodeItem(s1): %+v, %v", res, err)
	}
	if want := "encoding 3 of its 4 episodes with a file again (" + events.TopicAnalyzed + " sent for each); 1 left alone, " +
		"a transcode or package of them running or waiting for its worker (the first: package is running: its worker last reported at "; !strings.HasPrefix(res.Message, want) {
		t.Errorf("the series' message: %q\nwant it to start %q", res.Message, want)
	}
	want := []string{
		events.TopicAnalyzed + " e1 transcode reencode reencode episode",
		events.TopicAnalyzed + " e2 transcode reencode reencode episode",
		events.TopicAnalyzed + " e4 transcode reencode reencode episode",
	}
	if got := b.take(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, e := range []string{"e1", "e2", "e4"} {
		if got := chainState(t, st, e, "transcode"); !strings.HasPrefix(got, "pending 0 ") || !strings.Contains(got, "sent=true") {
			t.Errorf("%s's transcode: %s", e, got)
		}
	}
	if got := chainState(t, st, "e5", "package"); !strings.HasPrefix(got, "in_progress ") {
		t.Errorf("the busy episode's package: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id IN ('e3', 's1', 'season2')`); n != 0 {
		t.Errorf("%d steps given to the series, its season or the episode without a file", n)
	}

	for _, tc := range []struct{ id, says string }{
		{"s2", "the series has no episode with a file to encode"},
		{"m2", "the movie has no file to encode"},
	} {
		res, err := s.ReencodeItem(context.Background(), tc.id)
		if err != nil || res.Titles != 0 || res.Reencoded != 0 || res.Message != tc.says || len(b.take()) != 0 {
			t.Errorf("ReencodeItem(%s): %+v, %v; want %q", tc.id, res, err, tc.says)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id IN ('m2', 's2', 'e6')`); n != 0 {
		t.Errorf("%d steps given to titles without a file", n)
	}
}

// An event that could not be sent puts the title's transcode back: failed,
// saying so, sent again by the sweep a backoff later as a retry (which the
// transcoder runs: the step waits for it); without automatic retries it is
// left for an admin. The title can be encoded again while its transcode is
// failed: its package waits for that transcode.
func TestAReencodeWhoseEventCouldNotBeSent(t *testing.T) {
	st := catalog(t)
	file(t, st, "m1", "m2")
	for _, id := range []string{"m1", "m2"} {
		put(t, st, id, "transcode", "done", "")
		put(t, st, id, "package", "done", "")
	}
	b := &bus{refuse: map[string]bool{"m1": true, "m2": true}}
	s := newService(t, st, b)
	res, err := s.ReencodeItem(context.Background(), "m1")
	if err != nil || res.Reencoded != 0 || res.NotSent != 1 || res.Message !=
		"the re-encode could not be sent (broker: not the leader); its transcode is put back: failed, and the sweep sends it again a backoff later" {
		t.Fatalf("a re-encode whose event was refused: %+v, %v", res, err)
	}
	if got := chainState(t, st, "m1", "transcode"); got != "failed 0 attempts=1 error=the re-encode could not be sent: broker: not the leader last=- retry=true sent=false times=true" {
		t.Errorf("the transcode put back: %s", got)
	}
	if in := retryIn(t, st, "m1", "transcode"); in < 55 || in > 61 {
		t.Errorf("it is sent again in %vs, want the backoff (60s)", in)
	}
	if got := chainState(t, st, "m1", "package"); got != "pending 0 attempts=1 error=- last=- retry=false sent=false times=false" {
		t.Errorf("the package: %s", got)
	}

	// the sweep sends it when due, as a retry the transcoder runs
	b.refuse = nil
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET nextretryat = now() - interval '1 second'
		WHERE item_id = 'm1' AND step = 'transcode'`)
	if _, sent, err := s.Sweep(context.Background()); err != nil || sent != 1 {
		t.Fatalf("the sweep: sent %d, %v", sent, err)
	}
	if got := strings.Join(b.take(), "\n"); got != events.TopicAnalyzed+" m1 transcode retry retry movie" {
		t.Errorf("the sweep sent %q", got)
	}
	if got := chainState(t, st, "m1", "transcode"); !strings.HasPrefix(got, "pending 0 ") || !strings.Contains(got, "sent=true") {
		t.Errorf("the transcode the sweep sent: %s", got)
	}

	// without automatic retries, the admin's
	b.refuse = map[string]bool{"m2": true}
	manual := New(st, testPolicy, b, 0)
	res, err = manual.ReencodeItem(context.Background(), "m2")
	if err != nil || res.NotSent != 1 || !strings.HasSuffix(res.Message, "its transcode is put back: failed; retry its transcode") {
		t.Errorf("without automatic retries: %+v, %v", res, err)
	}
	if got := chainState(t, st, "m2", "transcode"); !strings.HasPrefix(got, "failed 0 ") || !strings.Contains(got, "retry=false sent=false") {
		t.Errorf("the transcode put back without automatic retries: %s", got)
	}
	// encoded again, its transcode failed and its package waiting for it
	b.refuse = nil
	if res, err := manual.ReencodeItem(context.Background(), "m2"); err != nil || res.Reencoded != 1 {
		t.Errorf("a re-encode after one that could not be sent: %+v, %v", res, err)
	}
	if got := strings.Join(b.take(), "\n"); got != events.TopicAnalyzed+" m2 transcode reencode reencode movie" {
		t.Errorf("sent %q", got)
	}
}

// Nothing is encoded again without an event bus or migration 033, an unknown
// item and one that is no movie, episode or series are errors, and none of it
// changes a step.
func TestWhatIsNotReencoded(t *testing.T) {
	st := catalog(t)
	file(t, st, "m1")
	put(t, st, "m1", "transcode", "done", "")
	storetest.AddItem(t, st, "t1", "track", "A Song", "")
	file(t, st, "t1")
	before := fingerprintSteps(t, st)
	for name, s := range map[string]*Service{"no bus": newService(t, st, &bus{off: true}), "a nil bus": New(st, testPolicy, nil, time.Second)} {
		if _, err := s.ReencodeItem(context.Background(), "m1"); err == nil || err.Error() != "cannot re-encode: "+
			"no event bus: a retry sends the step's trigger event again, and KAFKA_BROKERS is not set" {
			t.Errorf("%s: %v", name, err)
		}
	}
	s := newService(t, st, &bus{})
	if _, err := s.ReencodeItem(context.Background(), "nope"); err == nil || err.Error() != "unknown item: nope" {
		t.Errorf("an unknown item: %v", err)
	}
	if _, err := s.ReencodeItem(context.Background(), "t1"); err == nil ||
		err.Error() != "only a movie, an episode or a series is encoded again, and t1 is of type track" {
		t.Errorf("a track: %v", err)
	}
	if after := fingerprintSteps(t, st); after != before {
		t.Errorf("the steps changed:\nbefore %s\nafter  %s", before, after)
	}

	base := storetest.OpenBase(t)
	storetest.AddItem(t, base, "m1", "movie", "Sintel", "")
	file(t, base, "m1")
	if _, err := newService(t, base, &bus{}).ReencodeItem(context.Background(), "m1"); err == nil ||
		!strings.Contains(err.Error(), "cannot re-encode: migration 033") {
		t.Errorf("without 033: %v", err)
	}
}

func fingerprintSteps(t *testing.T, st *store.Store) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT coalesce(string_agg(s::text, '|' ORDER BY s.item_id, s.step), '')
		FROM com_nalet_katalog_itemprocessingsteps s`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A re-encode reads a title's steps under their lock: a worker that holds
// them, and starts the transcode meanwhile, is waited for, and the re-encode
// then finds the transcode running and leaves it alone (read without the
// lock, the steps looked done, and the reset overwrote the run).
func TestAReencodeWaitsForAWorkerThatHoldsTheSteps(t *testing.T) {
	st := catalog(t)
	file(t, st, "m1")
	put(t, st, "m1", "transcode", "done", "")
	put(t, st, "m1", "package", "done", "")
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var holder int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		res graph.ReencodeResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := s.ReencodeItem(ctx, "m1")
		done <- answer{res, err}
	}()
	for deadline := time.Now().Add(10 * time.Second); storetest.Count(t, st,
		`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holder) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the re-encode never waited for the steps' lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'in_progress', modifiedat = localtimestamp,
		startedat = localtimestamp, attempts = attempts + 1 WHERE item_id = 'm1' AND step = 'transcode'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	a := <-done
	if a.err != nil || a.res.Busy != 1 || a.res.Reencoded != 0 || !strings.HasPrefix(a.res.Message, "transcode is running") {
		t.Errorf("a re-encode that waited for a worker's lock: %+v, %v; want the transcode it started left alone", a.res, a.err)
	}
	if sent := b.take(); len(sent) != 0 {
		t.Errorf("sent %v", sent)
	}
	if got := chainState(t, st, "m1", "transcode"); !strings.HasPrefix(got, "in_progress 0 attempts=2 ") {
		t.Errorf("the transcode its worker started: %s", got)
	}
}

// Re-encodes of a title at once, as two admins or two instances ask them,
// encode it once: one resets it and sends its event, the others find it
// waiting for its worker. So too for a title without steps, whose steps one
// of them adds.
func TestReencodesAtOnceEncodeATitleOnce(t *testing.T) {
	st := catalog(t)
	file(t, st, "m1", "m2")
	put(t, st, "m1", "transcode", "done", "")
	put(t, st, "m1", "package", "done", "")
	b := &bus{}
	for _, item := range []string{"m1", "m2"} {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var reencoded, busy int32
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := newService(t, st, b).ReencodeItem(context.Background(), item)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				reencoded += res.Reencoded
				busy += res.Busy
				mu.Unlock()
			}()
		}
		wg.Wait()
		if sent := b.take(); reencoded != 1 || busy != 5 || len(sent) != 1 {
			t.Errorf("%s: %d encoded again, %d left alone, sent %v; want one of six, one event", item, reencoded, busy, sent)
		}
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, item); n != 2 {
			t.Errorf("%s has %d steps, want its transcode and package", item, n)
		}
	}
}
