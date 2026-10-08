package retry

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// covered gives the catalog's series an episode e1's file covers (e1x,
// S01E03), with no file of its own, as the scanner links it.
func covered(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.AddItem(t, st, "e1x", "episode", "Earthfall, Part Two", "s1")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 3 WHERE id = 'e1x'`)
	if _, err := library.Link(context.Background(), st.Pool(), "e1", "e1x"); err != nil {
		t.Fatal(err)
	}
}

// An episode another's file covers is encoded with that file: reencodeItem of
// it encodes its holder again, the result the holder's and saying so, and the
// re-encode queue queues the holder, once; the covered episode's own steps
// stay as they are, none of them run.
func TestACoveredEpisodeIsEncodedWithItsHolder(t *testing.T) {
	st := catalog(t)
	file(t, st, "e1")
	covered(t, st)
	put(t, st, "e1", "transcode", "done", "")
	put(t, st, "e1", "package", "done", "")
	before := rowsOf(t, st, "e1x")
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	res, err := s.ReencodeItem(ctx, "e1x")
	if err != nil || res.ItemID != "e1" || res.Reencoded != 1 || !strings.HasPrefix(res.Message, library.CoveredNote("e1x", "e1")) {
		t.Fatalf("reencodeItem of the covered episode: %+v, %v", res, err)
	}
	if got := strings.Join(b.take(), "; "); got != events.TopicAnalyzed+" e1 transcode reencode reencode episode" {
		t.Errorf("sent %q, want the holder's re-encode", got)
	}
	if got := rowsOf(t, st, "e1x"); got != before {
		t.Errorf("the covered episode's steps changed:\n%s\nwant:\n%s", got, before)
	}
	if !strings.Contains(chainState(t, st, "e1", "transcode"), "pending") {
		t.Errorf("the holder's transcode: %s", chainState(t, st, "e1", "transcode"))
	}

	q, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"e1x", "e1"}})
	if err != nil || q.Queued != 1 || q.AlreadyQueued != 0 || len(q.Skipped) != 0 {
		t.Fatalf("queue the covered episode and its holder: %+v, %v", q, err)
	}
	if got := queueOf(t, st); !strings.HasPrefix(got, "e1 queued items") || strings.Contains(got, "e1x") {
		t.Errorf("the queue:\n%s\nwant the holder once", got)
	}
}

// A covered episode is taken in with its holder's file: the take-in is the
// holder's, saying so.
func TestACoveredEpisodeIsTakenInWithItsHolder(t *testing.T) {
	st := catalog(t)
	withOriginal(t, st, "e1")
	covered(t, st)
	b := &bus{}
	s := newService(t, st, b)
	res, err := s.TakeInItem(context.Background(), "e1x")
	if err != nil || res.ItemID != "e1" || !res.Sent || !strings.HasPrefix(res.Message, library.CoveredNote("e1x", "e1")) {
		t.Fatalf("the take-in of the covered episode: %+v, %v", res, err)
	}
	if got := strings.Join(b.take(), "; "); got != events.TopicTranscoded+" e1 takein takein takein episode" {
		t.Errorf("sent %q, want the holder's take-in", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'e1x' AND step = 'takein'`); n != 0 {
		t.Error("the covered episode has a take-in of its own")
	}
}

// The pipeline runs nothing of a disc image: a title whose file is one is not
// encoded again, nor queued to be, nor taken in; its steps that read the
// file are retried neither by the sweep nor by an admin, and say why.
func TestNothingRunsOfADiscImage(t *testing.T) {
	st := catalog(t)
	arrivals := withOriginal(t, st, "m1")
	disc := filepath.Join(arrivals, "m1.iso")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET path = $1 WHERE item_id = 'm1'`, disc)
	b := &bus{}
	s := newService(t, st, b)
	ctx := context.Background()
	res, err := s.ReencodeItem(ctx, "m1")
	if err != nil || res.Reencoded != 0 || res.Message != "its file is a "+processing.DiscImageReason {
		t.Errorf("reencodeItem of a disc image: %+v, %v", res, err)
	}
	q, err := s.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{"m1"}})
	if err != nil || q.Queued != 0 || skippedOf(q) != "m1: its file is a "+processing.DiscImageReason {
		t.Errorf("queue a disc image: %+v, %v", q, err)
	}
	put(t, st, "m1", "transcode", "failed", due)
	put(t, st, "m1", "chapter", "failed", due)
	if _, sent, err := s.Sweep(ctx); err != nil || sent != 0 {
		t.Errorf("the sweep sent %d retries of a disc image, %v", sent, err)
	}
	if r, err := s.RetryFailed(ctx, ""); err != nil || r.Retried != 0 {
		t.Errorf("retryFailed retried a disc image: %+v, %v", r, err)
	}
	if _, err := s.RetryStep(ctx, "m1", "transcode"); err == nil || !strings.Contains(err.Error(), processing.DiscImageReason) {
		t.Errorf("an admin's retry of a disc image's transcode: %v", err)
	}
	// Its transcode failed with no attempt left: it is no title to take in.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET nextretryat = NULL WHERE item_id = 'm1'`)
	if n, err := s.TakeIn(ctx, nil); err != nil || n != 0 {
		t.Errorf("a disc image taken in: %d, %v", n, err)
	}
	if r, err := s.TakeInItem(ctx, "m1"); err != nil || r.Sent || !strings.Contains(r.Message, processing.DiscImageReason) {
		t.Errorf("an admin's take-in of a disc image: %+v, %v", r, err)
	}
	if got := b.take(); len(got) != 0 {
		t.Errorf("sent %v for a disc image", got)
	}
}
