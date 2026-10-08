package retry

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// refusal is the error of a transcode the transcoder refused.
const refusal = "Dolby Vision profile 5 needs a tone-mapping encode; kept the original"

// withOriginal gives the catalog's titles their originals where they arrived,
// each a source present, and the v2 layout.
func withOriginal(t *testing.T, st *store.Store, items ...string) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('layout', 'library.layout', 'v2')`)
	for _, item := range items {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
			VALUES ('src-' || $1::text, $1::text, $1::text || '.mkv', '/arrivals/' || $1::text || '.mkv', 10, 'present')`, item)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
			VALUES ('a-' || $1::text, $1::text, '/arrivals/' || $1::text || '.mkv', true, 'primary', 'src-' || $1::text)`, item)
	}
}

// A title whose transcode was refused is taken in at once: its step takein
// waits for the packager, sent (transcoded, its step takein, marked takein),
// its details saying why, and its transcode is retried by itself no more. So
// is one whose transcode or package failed with no attempt left. None is
// taken in twice, and the next take-in sends nothing.
func TestARefusedOrExhaustedTitleIsTakenIn(t *testing.T) {
	st := catalog(t)
	storetest.AddItem(t, st, "m3", "movie", "Big Buck Bunny", "")
	withOriginal(t, st, "m1", "m2", "m3", "e1")
	b := &bus{}
	s := newService(t, st, b)
	put(t, st, "m1", "transcode", "failed", `failures = 1, error = '`+refusal+`', lasterror = '`+refusal+`',
		nextretryat = now() + interval '1 minute'`)
	put(t, st, "m2", "transcode", "failed", `failures = 3, error = 'ffmpeg exited 1', lasterror = 'ffmpeg exited 1'`)
	put(t, st, "m3", "transcode", "done", "")
	put(t, st, "m3", "package", "failed", `failures = 3, error = 'no space left', lasterror = 'no space left'`)
	put(t, st, "e1", "transcode", "failed", `failures = 1, error = 'ffmpeg exited 1', lasterror = 'ffmpeg exited 1',
		nextretryat = now() + interval '1 minute'`)

	n, err := s.TakeIn(context.Background(), nil)
	if err != nil || n != 3 {
		t.Fatalf("TakeIn: %d, %v; want 3 sent", n, err)
	}
	want := []string{"stube.catalog.item.transcoded m1 takein takein takein movie",
		"stube.catalog.item.transcoded m2 takein takein takein movie",
		"stube.catalog.item.transcoded m3 takein takein takein movie"}
	if got := b.take(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for item, why := range map[string]string{"m1": "the transcode was refused: " + refusal,
		"m2": "the transcode failed with no attempt left: ffmpeg exited 1",
		"m3": "the package failed with no attempt left: no space left"} {
		if got := state(t, st, item, "takein"); got != "pending 0 error=- last=- retry=false sent=true" {
			t.Errorf("%s's take-in: %s", item, got)
		}
		var details string
		if err := st.Pool().QueryRow(context.Background(), `SELECT details FROM com_nalet_katalog_itemprocessingsteps
			WHERE item_id = $1 AND step = 'takein'`, item).Scan(&details); err != nil || details != why {
			t.Errorf("%s's take-in says %q, want %q (%v)", item, details, why, err)
		}
	}
	if got := state(t, st, "m1", "transcode"); !strings.HasSuffix(got, "retry=false sent=false") {
		t.Errorf("the refused transcode is retried still: %s", got)
	}
	if got := state(t, st, "e1", "transcode"); !strings.Contains(got, "retry=true") {
		t.Errorf("a transcode with an attempt left is not retried any more: %s", got)
	}
	if n, err := s.TakeIn(context.Background(), nil); err != nil || n != 0 || len(b.take()) != 0 {
		t.Errorf("TakeIn again: %d, %v", n, err)
	}
	// The packager's failed take-in is the retries': the sweep sends it again.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', failures = 1, dispatchedat = NULL,
		nextretryat = now() - interval '1 second', error = 'no room', lasterror = 'no room' WHERE item_id = 'm1' AND step = 'takein'`)
	if _, sent, err := s.Sweep(context.Background()); err != nil || sent != 1 {
		t.Fatalf("Sweep: %d sent, %v", sent, err)
	}
	if got := b.take(); len(got) != 1 || got[0] != "stube.catalog.item.transcoded m1 takein retry retry movie" {
		t.Errorf("the take-in's retry: %v", got)
	}
}

// A title is not taken in while its source has a version (taken in already,
// or packaged), while something reads its original, while its take-in failed
// (the retries' to send), when its original was retired, nor without the v2
// layout or an event bus; a take-in done of a source before is no take-in of
// the next one.
func TestATitleIsTakenInOnlyWhenItGetsNoPackage(t *testing.T) {
	st := catalog(t)
	for _, id := range []string{"m3", "m4", "m5", "m6"} {
		storetest.AddItem(t, st, id, "movie", "Film "+id, "")
	}
	withOriginal(t, st, "m1", "m2", "m3", "m4", "m5", "m6")
	for _, item := range []string{"m1", "m2", "m3", "m4", "m5", "m6"} {
		put(t, st, item, "transcode", "failed", `failures = 1, error = '`+refusal+`', lasterror = '`+refusal+`'`)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES
		('v1', 'm1', ARRAY['src-m1'], 'taken'), ('v2', 'm2', ARRAY['src-m2'], 'complete'),
		('v6', 'm6', ARRAY['src-m6'], 'building')`)
	put(t, st, "m3", "subtitle", "in_progress", "")
	put(t, st, "m4", "takein", "failed", `failures = 3, error = 'no room', lasterror = 'no room'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL WHERE item_id = 'm5'`)

	noBus := New(st, testPolicy, &bus{off: true}, 0)
	if n, err := noBus.TakeIn(context.Background(), nil); err != nil || n != 0 {
		t.Errorf("TakeIn without a bus: %d, %v", n, err)
	}
	b := &bus{}
	s := newService(t, st, b)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'legacy' WHERE key = 'library.layout'`)
	if n, err := s.TakeIn(context.Background(), nil); err != nil || n != 0 {
		t.Errorf("TakeIn with the legacy layout: %d, %v", n, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'v2' WHERE key = 'library.layout'`)
	if n, err := s.TakeIn(context.Background(), nil); err != nil || n != 1 {
		t.Fatalf("TakeIn: %d, %v; want the one with a version being built and nothing else", n, err)
	}
	if got := b.take(); len(got) != 1 || !strings.HasPrefix(got[0], "stube.catalog.item.transcoded m6 takein ") {
		t.Errorf("sent: %v", got)
	}
	// Taken in, then given another file: the new source has no version.
	put(t, st, "m1", "takein", "done", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('src-m1b', 'm1', 'better.mkv', '/arrivals/better.mkv', 20, 'present')`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = 'src-m1b', path = '/arrivals/better.mkv' WHERE id = 'a-m1'`)
	if n, err := s.TakeIn(context.Background(), []string{"m1", "m2"}); err != nil || n != 1 {
		t.Errorf("TakeIn of a title given another file: %d, %v", n, err)
	}
	if got := b.take(); len(got) != 1 || !strings.HasPrefix(got[0], "stube.catalog.item.transcoded m1 takein ") {
		t.Errorf("sent: %v", got)
	}
}

// An admin takes a title in by hand, whatever its steps say, unless its
// source has a version or something reads its original; the result says
// what it did.
func TestAnAdminTakesATitleIn(t *testing.T) {
	st := catalog(t)
	withOriginal(t, st, "m1", "m2")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES
		('v2', 'm2', ARRAY['src-m2'], 'complete')`)
	b := &bus{}
	s := newService(t, st, b)
	res, err := s.TakeInItem(context.Background(), "m1")
	if err != nil || !res.Sent || !strings.HasPrefix(res.Message, "taken in") {
		t.Fatalf("TakeInItem: %+v, %v", res, err)
	}
	if got := b.take(); len(got) != 1 || got[0] != events.TopicTranscoded+" m1 takein takein takein movie" {
		t.Errorf("sent: %v", got)
	}
	for id, says := range map[string]string{"m1": "its take-in is pending already", "m2": "its original has a version already",
		"s1": "only a movie or an episode is taken in", "nope": "unknown item"} {
		res, err := s.TakeInItem(context.Background(), id)
		if err != nil || res.Sent || !strings.Contains(res.Message, says) {
			t.Errorf("TakeInItem(%s): %+v, %v; want it to say %q", id, res, err, says)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'legacy' WHERE key = 'library.layout'`)
	if res, err := s.TakeInItem(context.Background(), "m1"); err != nil || res.Sent || !strings.Contains(res.Message, "legacy") {
		t.Errorf("TakeInItem with the legacy layout: %+v, %v", res, err)
	}
}
