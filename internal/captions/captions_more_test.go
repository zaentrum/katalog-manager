package captions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A record that can't be read is not handed out: what it says of the
// original's length can't be told, which is no finding of it; the original
// waits its backoff, a failure, and is handed out once its record reads.
func TestAnOriginalWhoseRecordCantBeReadIsNotHandedOut(t *testing.T) {
	ctx := context.Background()
	for name, record := range map[string]string{"no JSON": `{"container": `, "a key twice": `{"container": {}, "container": {}}`,
		"no essence object":     `{"essence": [], "container": {"format": "matroska,webm", "durationMs": 600000}}`,
		"a number past a float": `{"essence": {}, "container": {"format": "matroska,webm", "durationMs": 1e999}}`} {
		st := storetest.Open(t)
		dir := t.TempDir()
		media := filepath.Join(dir, "film.mkv")
		if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec := writeRecord(t, dir, record)
		storetest.AddItem(t, st, "r1", "movie", "A Film", "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
			recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, rec)
		svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
		cl, err := svc.Claim(ctx, "analyzer-0", 1)
		if err != nil || len(cl.Items) != 0 {
			t.Errorf("%s: %+v, %v; want it not handed out", name, cl, err)
			continue
		}
		r, err := Of(ctx, st.Pool(), "r1-src")
		if err != nil || r == nil || r.Finding != nil || r.Failures != 1 || r.Reason == nil ||
			!strings.Contains(*r.Reason, "its source record can't be read") {
			t.Errorf("%s: %+v, %v; want a failure waiting its backoff, no finding", name, r, err)
		}
	}
	// one whose record is not yet written is handed out: its read is a
	// finding that its record says no length, and waits for its record
	st := storetest.Open(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, st, "r1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
		recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, t.TempDir())
	cl, err := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC))).Claim(ctx, "analyzer-0", 1)
	if err != nil || len(cl.Items) != 1 || cl.Items[0].DurationMs != nil {
		t.Errorf("a record not yet written: %+v, %v; want it handed out with no length", cl, err)
	}
}

// A report that says a thing of itself twice, in another case too, says it
// neither way: refused; a window that carries the catalog's own key of a
// window it kept as failed (unreadable) is no window a read says: refused,
// never a failure of the catalog's.
func TestAReportInAnyCaseOrWithTheCatalogsKeyIsRefused(t *testing.T) {
	if k := RepeatedKey([]byte(`{"token": "a", "Token": "b", "original": {}}`)); k == "" {
		t.Error("a token in two cases: not refused")
	}
	if k := RepeatedKey([]byte(`{"token": "a", "original": {"sizeBytes": 6, "SizeBytes": 7}}`)); k == "" {
		t.Error("a size in two cases: not refused")
	}
	ctx := context.Background()
	st, source, _, token := claimed(t)
	_, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": {"captionWindows": [{"unreadable": "x",
		"captions": true}], "duration": 600, "span": {"start": 0, "end": 600}, "sizeBytes": 6, "ffprobe": "7.1.5"}}`), time.Now())
	var bad *BadReport
	if !errors.As(err, &bad) || !strings.Contains(bad.Why, "the catalog's own key") {
		t.Errorf("a window with the catalog's own key: %v; want it refused", err)
	}
}

// What a window whose frames can't be read found is kept as found, a read
// that decoded a frame at least.
func TestAWindowOfFramesThatCantBeReadIsKeptAsFound(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	body := strings.Replace(fullRead, `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false`,
		`{"at": 0.5, "start": 300, "end": 301.88, "frames": "48", "captions": true`, 1)
	r, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+body+`}`), time.Now())
	if err != nil || r.State != StateRead || r.Captions == nil || !*r.Captions || r.FramesDecoded == nil ||
		*r.FramesDecoded < 1 {
		t.Errorf("captions in a window of frames that can't be read: %+v, %v; want them kept as found", r, err)
	}
}

// An original whose record leaves its captions not known (it says no
// length, or its picture needs its packets' signature) awaits its record:
// counted on its own, none of read nor of to read, so that what waits for
// nothing to read passes.
func TestAnOriginalAwaitingItsRecordIsNoneToRead(t *testing.T) {
	ctx := context.Background()
	svc, source, token := claimedOf(t, tenMinuteRecord)
	record(t, ctx, svc, source, token, `{"captionsKnown": false, "reason": "`+library.UnscannedReason+`", "duration": 600,
		"span": {"start": 0, "end": 600}, "sizeBytes": 6, "ffprobe": "7.1.5"}`)
	p, err := svc.Progress(ctx)
	if err != nil || p.Originals != 1 || p.AwaitingRecord != 1 || p.ToRead != 0 || p.Read != 0 {
		t.Errorf("the progress: %+v, %v; want it awaiting its record, nothing to read", p, err)
	}
}
