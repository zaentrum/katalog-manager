package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// queueOf is the queue in its order, a title a line: "<item> <state>
// <selection> by=<who> sent=<whether> done=<whether> reason=<why>".
func queueOf(t *testing.T, st *store.Store) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT coalesce(string_agg(item_id || ' ' || state || ' ' || selection ||
			' by=' || COALESCE(enqueuedby, '-') || ' sent=' || (sentat IS NOT NULL) || ' done=' || (doneat IS NOT NULL) ||
			' reason=' || COALESCE(reason, '-'), E'\n' ORDER BY seq), '')
		FROM com_nalet_katalog_reencodequeue`).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// setting sets a setting of the catalog.
func setting(t *testing.T, st *store.Store, key, value string) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE key = $1`, key)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, $1, $2)`, key, value)
}

// packaged gives titles a package: an HEVC rendition among their playback
// assets.
func packaged(t *testing.T, st *store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, codec)
			VALUES (gen_random_uuid()::varchar, $1::varchar, '/packages/' || $1::varchar || '/master.m3u8', false, 'hvc1.2.4.L120.90')`, id)
	}
}

// held holds a title's retire for its surround, as the retire job does: its
// current version's package lacked a 5.1 of it.
func held(t *testing.T, st *store.Store, item, version string) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, completedat) VALUES ($1, $2, 'complete', now())`,
		version, item)
	quoted := strings.ReplaceAll(library.SurroundHeld, "'", "''")
	put(t, st, item, "retire", "failed", `failures = 1, error = '`+quoted+`', lasterror = '`+quoted+`',
		details = '`+library.HeldDetails(version)+`', nextretryat = NULL`)
}

// retiredOriginal makes a title one whose original was deleted after
// packaging: no file, a source deleted by the retire job.
func retiredOriginal(t *testing.T, st *store.Store, item string) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true`, item)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, retireeventid, retireeventat, deletedat)
		VALUES (gen_random_uuid()::varchar, $1, 'original.mkv', 1, 'deleted', 'ev-retired', '2026-10-06T10:00:00Z', '2026-10-06T10:00:00Z')`, item)
}

// queueCatalog is the catalog of the queue's tests: two films with files,
// a series whose first season is a season of its own (Earthfall its second
// episode, The Pilot its first) and whose second has an episode with no
// file, a series with no episode, and a film whose original was retired.
func queueCatalog(t *testing.T) *store.Store {
	t.Helper()
	st := catalog(t)
	storetest.AddItem(t, st, "sn1", "season", "Season 1", "s1")
	storetest.AddItem(t, st, "e2", "episode", "The Pilot", "sn1")
	storetest.AddItem(t, st, "e3", "episode", "Second Season", "s1")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = 'e2'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 2, episodenumber = 1 WHERE id = 'e3'`)
	storetest.AddItem(t, st, "s2", "series", "Nothing Yet", "")
	storetest.AddItem(t, st, "m3", "movie", "Cosmos Laundromat", "")
	file(t, st, "m1", "m2", "e1", "e2")
	retiredOriginal(t, st, "m3")
	return st
}

func skippedOf(r graph.ReencodeEnqueued) string {
	var out []string
	for _, s := range r.Skipped {
		out = append(out, s.ItemID+": "+s.Reason)
	}
	return strings.Join(out, "\n")
}

// A request names titles: a movie, an episode, a series (its episodes under
// it or a season of it, by season and episode), each once; a name that is
// no title, or no movie, episode or series, a series with no episode, an
// episode with no file and a film whose original was retired are skipped,
// saying why. A title queued already, waiting or sent, is not queued again;
// one done is. Who queued it is the caller's subject.
func TestTheQueueTakesTheTitlesARequestNames(t *testing.T) {
	st := queueCatalog(t)
	s := newService(t, st, &bus{})
	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "service-account-zaentrum-manager"})

	req := graph.ReencodeRequest{Items: []string{"m1", "s1", "m1", "nobody", "sn1", "s2", "m3", " ", " e1 "}}
	res, err := s.EnqueueReencode(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	wantSkipped := `nobody: unknown item
sn1: only a movie, an episode or a series is encoded again, and it is of type season
s2: the series has no episode to encode
e3: the episode has no file to encode
m3: the original was deleted after packaging (event ev-retired, 2026-10-06T10:00:00Z); ` + newArrival
	if res.Queued != 3 || res.AlreadyQueued != 0 || skippedOf(res) != wantSkipped {
		t.Errorf("queued %d, already %d, skipped:\n%s\nwant 3, 0 and:\n%s", res.Queued, res.AlreadyQueued, skippedOf(res), wantSkipped)
	}
	const by = " items by=service-account-zaentrum-manager sent=false done=false reason=-"
	if got := queueOf(t, st); got != "m1 queued"+by+"\ne2 queued"+by+"\ne1 queued"+by {
		t.Errorf("the queue:\n%s", got)
	}

	// again: nothing new, nothing doubled
	res, err = s.EnqueueReencode(ctx, req)
	if err != nil || res.Queued != 0 || res.AlreadyQueued != 3 || len(res.Skipped) != 5 {
		t.Errorf("the same request again: %+v, %v", res, err)
	}
	// sent or done: a title sent stays as it is, one done is queued anew
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'sent', sentat = now() WHERE item_id = 'e2'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'done', sentat = now(), doneat = now() WHERE item_id = 'm1'`)
	res, err = s.EnqueueReencode(context.Background(), graph.ReencodeRequest{Items: []string{"e2", "m1"}})
	if err != nil || res.Queued != 1 || res.AlreadyQueued != 1 || len(res.Skipped) != 0 {
		t.Errorf("a title sent and one done: %+v, %v", res, err)
	}
	if got := queueOf(t, st); !strings.HasSuffix(got, "\nm1 queued items by=katalog-manager sent=false done=false reason=-") ||
		!strings.HasPrefix(got, "m1 done items by=service-account-zaentrum-manager sent=true done=true") {
		t.Errorf("the queue, the done title queued anew by the service itself:\n%s", got)
	}

	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{}); err == nil {
		t.Error("a request that names nothing: no error")
	}
}

// {"held": true} takes the titles whose retire the retire job holds for
// their surround (a hold for a version since superseded is none), {"all":
// true} every packaged movie and episode; with titles named too, each once,
// as the first that named it.
func TestTheQueueTakesTheHeldAndEveryPackagedTitle(t *testing.T) {
	st := queueCatalog(t)
	s := newService(t, st, &bus{})
	packaged(t, st, "m1", "m2", "e1", "m3")
	held(t, st, "m2", "v-m2")
	// e2 was held for a version a newer one superseded: the job looks at it
	// again, it is held no more
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, completedat, supersededby)
		VALUES ('v-old', 'e2', 'superseded', now() - interval '1 day', 'v-new')`)
	held(t, st, "e2", "v-new")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET details = $1 WHERE item_id = 'e2' AND step = 'retire'`,
		library.HeldDetails("v-old"))

	res, err := s.EnqueueReencode(context.Background(), graph.ReencodeRequest{Held: true})
	if err != nil || res.Queued != 1 || res.AlreadyQueued != 0 || len(res.Skipped) != 0 {
		t.Fatalf("held: %+v, %v", res, err)
	}
	if got := queueOf(t, st); got != "m2 queued held by=katalog-manager sent=false done=false reason=-" {
		t.Errorf("the queue after held:\n%s", got)
	}
	res, err = s.EnqueueReencode(context.Background(), graph.ReencodeRequest{All: true})
	if err != nil || res.Queued != 2 || res.AlreadyQueued != 1 ||
		skippedOf(res) != "m3: the original was deleted after packaging (event ev-retired, 2026-10-06T10:00:00Z); "+newArrival {
		t.Fatalf("all: %+v, %v", res, err)
	}
	if got := queueOf(t, st); got != `m2 queued held by=katalog-manager sent=false done=false reason=-
e1 queued all by=katalog-manager sent=false done=false reason=-
m1 queued all by=katalog-manager sent=false done=false reason=-` {
		t.Errorf("the queue after all:\n%s", got)
	}

	// together: each title once, as the first that named it
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_reencodequeue`)
	res, err = s.EnqueueReencode(context.Background(), graph.ReencodeRequest{Items: []string{"m2"}, Held: true, All: true})
	if err != nil || res.Queued != 3 || res.AlreadyQueued != 0 || len(res.Skipped) != 1 {
		t.Fatalf("items, held and all: %+v, %v", res, err)
	}
	if got := queueOf(t, st); !strings.HasPrefix(got, "m2 queued items ") || strings.Count(got, "\n") != 2 {
		t.Errorf("the queue after items, held and all:\n%s", got)
	}
}

// The sweep sends the queue as reencodeItem sends a title (its transcode
// and package reset, the transcoder's trigger not marked as a retry), the
// titles queued first first: at most library.reencode.rate a pass, and no
// more while library.reencode.inflight are sent and not done.
func TestTheSweepSendsTheQueueAtItsRate(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	films := []string{"m1", "m2", "m4", "m5", "m6", "m7"}
	for _, id := range films[2:] {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
	}
	file(t, st, films...)
	for _, id := range films {
		put(t, st, id, "transcode", "done", "")
		put(t, st, id, "package", "done", "")
	}
	if res, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: films}); err != nil || res.Queued != 6 {
		t.Fatalf("queue the films: %+v, %v", res, err)
	}

	// the defaults: 4 a pass, 4 in flight
	p, err := s.DrainQueue(ctx)
	if err != nil || p.Sent != 4 || p.Idle != "" {
		t.Fatalf("the first pass: %+v, %v", p, err)
	}
	if got := strings.Join(b.take(), "\n"); got != strings.Join([]string{
		events.TopicAnalyzed + " m1 transcode reencode reencode movie", events.TopicAnalyzed + " m2 transcode reencode reencode movie",
		events.TopicAnalyzed + " m4 transcode reencode reencode movie", events.TopicAnalyzed + " m5 transcode reencode reencode movie"}, "\n") {
		t.Errorf("sent:\n%s", got)
	}
	if got := chainState(t, st, "m1", "transcode"); !strings.HasPrefix(got, "pending 0 ") || !strings.Contains(got, " sent=true ") {
		t.Errorf("the transcode of a title sent: %s", got)
	}
	if got := chainState(t, st, "m6", "transcode"); !strings.HasPrefix(got, "done ") {
		t.Errorf("the transcode of a title waiting: %s", got)
	}
	if got := queueOf(t, st); strings.Count(got, " sent items ") != 4 || !strings.Contains(got, "m6 queued ") || !strings.Contains(got, "m7 queued ") {
		t.Errorf("the queue:\n%s", got)
	}
	// four in flight: none goes
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 0 || len(b.take()) != 0 {
		t.Errorf("a pass with four in flight: %+v, %v", p, err)
	}
	// one done: one goes, the first queued
	steps := processing.New(st.Pool()).WithPolicy(testPolicy)
	if err := steps.Upsert(ctx, "m1", "package", processing.StatusDone, nil, nil); err != nil {
		t.Fatal(err)
	}
	if p, err := s.DrainQueue(ctx); err != nil || p.Done != 1 || p.Sent != 1 {
		t.Errorf("a pass with one done: %+v, %v", p, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m6 ") {
		t.Errorf("sent %v, want m6", got)
	}

	// a rate of 1 with room for more: one a pass; a rate of 0: none
	setting(t, st, library.SettingReencodeInflight, "10")
	setting(t, st, library.SettingReencodeRate, "0")
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 0 || p.Idle != "library.reencode.rate is 0" || p.Waiting != 1 {
		t.Errorf("a rate of 0: %+v, %v", p, err)
	}
	setting(t, st, library.SettingReencodeRate, "1")
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 1 {
		t.Errorf("a rate of 1: %+v, %v", p, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m7 ") {
		t.Errorf("sent %v, want m7", got)
	}
	if got := queueOf(t, st); strings.Count(got, " sent ") != 5 || !strings.HasPrefix(got, "m1 done ") {
		t.Errorf("the queue:\n%s", got)
	}
}

// The queue is sent only within library.reencode.window, by the service's
// clock: outside of it, with a window it cannot read, or with no event bus,
// the titles wait, and the pass says why.
func TestTheQueueIsSentInItsWindow(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	file(t, st, "m1", "m2")
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return clock }
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	setting(t, st, library.SettingReencodeWindow, "23:00-07:00")
	p, err := s.DrainQueue(ctx)
	if err != nil || p.Sent != 0 || p.Idle != "outside library.reencode.window (23:00-07:00)" || p.Waiting != 2 || len(b.take()) != 0 {
		t.Errorf("at noon: %+v, %v", p, err)
	}
	if q, err := s.ReencodeQueue(ctx); err != nil || q.Queued != 2 || q.Idle == nil || *q.Idle != p.Idle {
		t.Errorf("the queue at noon: %+v, %v", q, err)
	}
	clock = time.Date(2026, 10, 7, 23, 30, 0, 0, time.Local)
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 2 || p.Idle != "" || len(b.take()) != 2 {
		t.Errorf("at half past eleven: %+v, %v", p, err)
	}
	if q, err := s.ReencodeQueue(ctx); err != nil || q.Sent != 2 || q.Idle != nil {
		t.Errorf("the queue at half past eleven: %+v, %v", q, err)
	}

	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_reencodequeue`)
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	// m1 was reset a moment ago: done again, so it is not busy
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done', dispatchedat = NULL WHERE item_id = 'm1'`)
	setting(t, st, library.SettingReencodeWindow, "nights")
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 0 ||
		p.Idle != `library.reencode.window: "nights" is no window: HH:MM-HH:MM, as 23:00-07:00; nothing is sent until it reads` {
		t.Errorf("a window that is none: %+v, %v", p, err)
	}
	setting(t, st, library.SettingReencodeWindow, "")
	b.off = true
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 0 || !strings.HasPrefix(p.Idle, "no event bus: ") {
		t.Errorf("no event bus: %+v, %v", p, err)
	}
	b.off = false
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 1 {
		t.Errorf("any time, with a bus: %+v, %v", p, err)
	}
}

// A busy title stays queued, saying why, and takes no place of the pass: the
// next one goes. It goes once it is no longer busy.
func TestABusyTitleWaitsInTheQueue(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	file(t, st, "m1", "m2")
	setting(t, st, library.SettingReencodeRate, "1")
	put(t, st, "m1", "transcode", "in_progress", "startedat = localtimestamp")
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	before := rowsOf(t, st, "m1")
	p, err := s.DrainQueue(ctx)
	if err != nil || p.Sent != 1 || p.Busy != 1 {
		t.Fatalf("the pass: %+v, %v", p, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m2 ") {
		t.Errorf("sent %v, want m2", got)
	}
	if rowsOf(t, st, "m1") != before {
		t.Error("the busy title's steps were changed")
	}
	got := queueOf(t, st)
	if !strings.HasPrefix(got, "m1 queued items by=katalog-manager sent=false done=false reason=transcode is running: its worker last reported at ") ||
		!strings.HasSuffix(got, "\nm2 sent items by=katalog-manager sent=true done=false reason=-") {
		t.Errorf("the queue:\n%s", got)
	}
	// its transcode done: it goes
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done', finishedat = localtimestamp
		WHERE item_id = 'm1' AND step = 'transcode'`)
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 1 || p.Busy != 0 {
		t.Errorf("the pass once it is free: %+v, %v", p, err)
	}
	if got := queueOf(t, st); !strings.HasPrefix(got, "m1 sent items by=katalog-manager sent=true done=false reason=-") {
		t.Errorf("the queue:\n%s", got)
	}
}

// A title sent is done once its package is; failed when its transcode or its
// package fails with no attempt left (one with a retry scheduled is still
// sent), or its package is skipped, saying why. One whose original was
// retired while it waited fails before it is sent; one whose event could not
// be sent stays queued, its transcode failed with no retry of its own, and
// goes on a later pass.
func TestASentTitleIsDoneOrFailed(t *testing.T) {
	st := queueCatalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	setting(t, st, library.SettingReencodeRate, "10")
	setting(t, st, library.SettingReencodeInflight, "10")
	storetest.AddItem(t, st, "m4", "movie", "Agent 327", "")
	file(t, st, "m4")
	if res, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1", "m2", "e1", "e2", "m4"}}); err != nil || res.Queued != 5 {
		t.Fatalf("queue them: %+v, %v", res, err)
	}
	// e2's original is retired while it waits; m4's event cannot be sent
	retiredOriginal(t, st, "e2")
	b.refuse = map[string]bool{"m4": true}
	p, err := s.DrainQueue(ctx)
	if err != nil || p.Sent != 3 || p.Refused != 1 || p.NotSent != 1 {
		t.Fatalf("the pass: %+v, %v", p, err)
	}
	b.take()
	got := queueOf(t, st)
	for _, want := range []string{
		"e2 failed items by=katalog-manager sent=false done=true reason=the original was deleted after packaging (event ev-retired, ",
		"m4 queued items by=katalog-manager sent=false done=false reason=the re-encode could not be sent: broker: not the leader",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the queue:\n%s\nlacks: %s", got, want)
		}
	}
	if got := chainState(t, st, "m4", "transcode"); got != "failed 0 attempts=0 error=the re-encode could not be sent: broker: not the leader last=- retry=false sent=false times=true" {
		t.Errorf("the transcode of the title not sent: %s", got)
	}

	// m1's package is done; m2's transcode failed with a retry, e1's package
	// for good
	steps := processing.New(st.Pool()).WithPolicy(testPolicy)
	if err := steps.Upsert(ctx, "m1", "package", processing.StatusDone, nil, nil); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', failures = 1, error = 'out of memory',
		lasterror = 'out of memory', nextretryat = now() + interval '1 minute' WHERE item_id = 'm2' AND step = 'transcode'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', failures = 3, error = 'the manifest is invalid',
		lasterror = 'the manifest is invalid', nextretryat = NULL WHERE item_id = 'e1' AND step = 'package'`)
	b.refuse = nil
	p, err = s.DrainQueue(ctx)
	if err != nil || p.Done != 1 || p.Failed != 1 || p.Sent != 1 {
		t.Fatalf("the pass after: %+v, %v", p, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m4 ") {
		t.Errorf("sent %v, want m4 again", got)
	}
	got = queueOf(t, st)
	for _, want := range []string{
		"m1 done items by=katalog-manager sent=true done=true reason=-",
		"m2 sent items by=katalog-manager sent=true done=false reason=-",
		"e1 failed items by=katalog-manager sent=true done=true reason=package failed: the manifest is invalid",
		"m4 sent items by=katalog-manager sent=true done=false reason=-",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the queue:\n%s\nlacks: %s", got, want)
		}
	}
	// m2's transcode out of attempts; m4's package skipped
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET failures = 3, nextretryat = NULL WHERE item_id = 'm2' AND step = 'transcode'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'skipped', error = 'no rendition to package'
		WHERE item_id = 'm4' AND step = 'package'`)
	if p, err := s.DrainQueue(ctx); err != nil || p.Failed != 2 || p.Sent != 0 {
		t.Errorf("the last pass: %+v, %v", p, err)
	}
	got = queueOf(t, st)
	for _, want := range []string{
		"m2 failed items by=katalog-manager sent=true done=true reason=transcode failed: out of memory",
		"m4 failed items by=katalog-manager sent=true done=true reason=package skipped: no rendition to package",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the queue:\n%s\nlacks: %s", got, want)
		}
	}
	q, err := s.ReencodeQueue(ctx)
	if err != nil || q.Queued != 0 || q.Sent != 0 || q.Done != 1 || q.Failed != 4 || q.Oldest != nil || q.Newest != nil {
		t.Errorf("the queue's counts: %+v, %v", q, err)
	}
}

// The overview counts the queue, its oldest and newest title waiting or
// sent; an admin clears it, by default what waits and what ended, not what
// is sent, and refuses a state it does not have. Without migration 043
// there is no queue: a request to it is refused, the overview has none, and
// the sweep passes it by.
func TestTheOverviewCountsTheQueueAndAnAdminClearsIt(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	file(t, st, "m1", "m2", "e1")
	setting(t, st, library.SettingReencodeRate, "1")
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1", "m2", "e1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainQueue(ctx); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state, doneat, reason)
		VALUES ('old-1', 'm2', 'failed', now(), 'transcode failed: out of memory'), ('old-2', 'e1', 'done', now(), NULL)`)
	o, err := s.Overview(ctx, "", 0, 0)
	if err != nil || o.Reencode == nil {
		t.Fatalf("the overview: %+v, %v", o.Reencode, err)
	}
	q := *o.Reencode
	if q.Queued != 2 || q.Sent != 1 || q.Done != 1 || q.Failed != 1 || q.Oldest == nil || q.Oldest.ItemID != "m1" ||
		q.Oldest.State != QueueSent || q.Newest == nil || q.Newest.ItemID != "e1" || q.Newest.State != QueueQueued ||
		q.Newest.EnqueuedAt.Before(q.Oldest.EnqueuedAt) || q.Idle != nil {
		t.Errorf("the overview's queue: %+v, oldest %+v, newest %+v", q, q.Oldest, q.Newest)
	}

	if n, err := s.ClearReencodeQueue(ctx, nil); err != nil || n != 4 {
		t.Errorf("clear: %d, %v", n, err)
	}
	if got := queueOf(t, st); got != "m1 sent items by=katalog-manager sent=true done=false reason=-" {
		t.Errorf("the queue after a clear:\n%s", got)
	}
	if n, err := s.ClearReencodeQueue(ctx, []string{"sent"}); err != nil || n != 1 || queueOf(t, st) != "" {
		t.Errorf("clear what is sent: %d, %v", n, err)
	}
	if _, err := s.ClearReencodeQueue(ctx, []string{"waiting"}); err == nil ||
		err.Error() != `unknown state "waiting": one of queued, sent, done, failed` {
		t.Errorf("a state that is none: %v", err)
	}

	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_reencodequeue`)
	s = newService(t, st, b)
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{All: true}); !errors.Is(err, ErrNoQueue) ||
		!strings.Contains(err.Error(), "migration 043 (db/migrations/043_reencode_queue.sql) is not applied") {
		t.Errorf("a request without the queue: %v", err)
	}
	if _, err := s.ClearReencodeQueue(ctx, nil); !errors.Is(err, ErrNoQueue) {
		t.Errorf("a clear without the queue: %v", err)
	}
	if o, err := s.Overview(ctx, "", 0, 0); err != nil || o.Reencode != nil {
		t.Errorf("the overview without the queue: %+v, %v", o.Reencode, err)
	}
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 0 || !strings.HasPrefix(p.Idle, "migration 043 ") {
		t.Errorf("a pass without the queue: %+v, %v", p, err)
	}
}

// One instance sends the queue at a time: while another holds the queue's
// lock, a pass sends nothing, and says nothing of it.
func TestOneInstanceSendsTheQueueAtATime(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	file(t, st, "m1")
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	other, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock(`+queueLock+`)`); err != nil {
		t.Fatal(err)
	}
	if p, err := s.DrainQueue(ctx); err != nil || p != (QueuePass{}) || len(b.take()) != 0 {
		t.Errorf("a pass while another instance sends: %+v, %v", p, err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(`+queueLock+`)`); err != nil {
		t.Fatal(err)
	}
	if p, err := s.DrainQueue(ctx); err != nil || p.Sent != 1 || len(b.take()) != 1 {
		t.Errorf("a pass once it is free: %+v, %v", p, err)
	}
}

// A pass that cannot note why a title waits still sends the titles it reset
// before, and notes them sent: no title is left reset without its event.
func TestAPassThatCannotNoteATitleStillSendsWhatItReset(t *testing.T) {
	st := catalog(t)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	file(t, st, "m1", "m2")
	put(t, st, "m2", "transcode", "in_progress", "startedat = localtimestamp")
	if _, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1", "m2"}}); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `CREATE FUNCTION refuse_busy() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN IF NEW.reason LIKE 'transcode is running%' THEN RAISE EXCEPTION 'the disk is full'; END IF; RETURN NEW; END $$`)
	storetest.Exec(t, st, `CREATE TRIGGER refuse_busy BEFORE UPDATE ON com_nalet_katalog_reencodequeue
		FOR EACH ROW EXECUTE FUNCTION refuse_busy()`)
	p, err := s.DrainQueue(ctx)
	if err == nil || !strings.Contains(err.Error(), "the disk is full") || p.Sent != 1 {
		t.Fatalf("the pass: %+v, %v", p, err)
	}
	if got := b.take(); len(got) != 1 || !strings.Contains(got[0], " m1 ") {
		t.Errorf("sent %v, want m1", got)
	}
	if got := queueOf(t, st); got != "m1 sent items by=katalog-manager sent=true done=false reason=-\n"+
		"m2 queued items by=katalog-manager sent=false done=false reason=-" {
		t.Errorf("the queue:\n%s", got)
	}
}
