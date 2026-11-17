package captions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// fullRead is a read of every planned window of a ten-minute video, none
// with captions, as the analyzer reports it.
var fullRead = `{"captionWindows": [{"at": 0, "start": 0, "end": 1.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
	{"at": 0.1, "start": 60, "end": 61.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
	{"at": 0.3, "start": 180, "end": 181.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
	{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
	{"at": 0.7, "start": 420, "end": 421.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0},
	{"at": 0.9, "start": 540, "end": 541.88, "frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}], "duration": 600, "sizeBytes": 6,
	"ffprobe": "7.1.5"}`

// claimed is an original on disk the backfill handed out: the store, the
// source, its title and the claim's token.
func claimed(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := writeRecord(t, dir, tenMinuteRecord)
	storetest.AddItem(t, st, "r1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state,
		recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, record)
	c, err := New(st).WithClock(func() time.Time { return time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC) }).
		Claim(context.Background(), "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	return st, "r1-src", "r1", c.Items[0].Token
}

// tenMinuteRecord is the source record of a ten-minute film: its container
// and its length, its one picture.
const tenMinuteRecord = `{"schema": "zaentrum.library.source/2", "container": {"format": "matroska,webm",
	"durationMs": 600000}, "streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"}]}`

// writeRecord writes the source record record into dir/sources/r1-src, and
// answers that folder.
func writeRecord(t *testing.T, dir, record string) string {
	t.Helper()
	d := filepath.Join(dir, "sources", "r1-src")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "source.json"), []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func report(t *testing.T, body string) Report {
	t.Helper()
	var r Report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A report names its claim: one with another token, or none, is refused and
// changes nothing; the claim's is taken. A report's package part, of an
// older analyzer, is taken no notice of.
func TestAReportOfClosedCaptionsNamesItsClaim(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	for _, tok := range []string{"", "another-claim"} {
		_, err := Record(ctx, st, source, report(t, `{"token": "`+tok+`", "original": `+fullRead+`}`), time.Now())
		if !errors.Is(err, ErrNotTheClaim) {
			t.Errorf("a report with token %q: %v; want it refused", tok, err)
		}
	}
	if r, _ := Of(ctx, st.Pool(), source); r == nil || r.State != StatePending || r.ClaimedAt == nil {
		t.Fatalf("a refused report changed what is kept: %+v", r)
	}
	r, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+fullRead+`,
		"package": {"versionId": "v1", "captionWindows": [{"at": 0, "start": 0, "end": 1.88, "frames": 48,
		"captions": true}], "ffprobe": "7.1.5"}}`), time.Now())
	if err != nil || r.State != StateRead || r.Captions == nil || *r.Captions || r.ClaimedAt != nil ||
		r.Duration == nil || *r.Duration != 600 {
		t.Fatalf("the claim's report: %+v, %v", r, err)
	}
	if _, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+fullRead+`}`), time.Now()); !errors.Is(err,
		ErrNotTheClaim) {
		t.Errorf("a second report of the claim it ended: %v; want it refused", err)
	}
}

// A read's report takes the title's lock the retire job's hold takes: one
// that lands while a hold is being written waits for it, and then lets it
// go, as it would a hold written before.
func TestAReadThatLandsWhileAHoldIsWrittenLetsItGo(t *testing.T) {
	ctx := context.Background()
	st, source, item, token := claimed(t)
	held := library.CaptionsUnknown + ": no read of its original's frames found whether it has them"
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := library.LockItem(ctx, tx, item); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+fullRead+`}`), time.Now())
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the read did not wait for the title's lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// the hold, as the retire job writes it, under the lock
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET error = $2 WHERE id = $1`, source, held); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step,
		status, attempts, error, details) VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'retire', 'failed', 1, $2, $3)`,
		item, held, library.HeldDetails("v1")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, item) +
		storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND error IS NOT NULL`,
			source); n != 0 {
		t.Errorf("the hold written while the read landed stays (%d)", n)
	}
}

// The analyzer's own reads of made clips (the library's
// testdata/captions/reads.json, its PUT bodies) are reports the catalog
// takes: each original's part checks, a read or a failed one (a file it
// could not read, {error, ffprobe}, as a transport stream whose clock starts
// again or a raw stream).
func TestTheAnalyzersReadsAreReportsTheCatalogTakes(t *testing.T) {
	b, err := os.ReadFile("../library/testdata/captions/reads.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases map[string]struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		var r Report
		if err := json.Unmarshal(c.Body, &r); err != nil || r.Original == nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if r.Original.FFprobe == "" {
			r.Original.FFprobe = "7.1.5" // the oldest reads named none
		}
		if r.Original.SizeBytes == nil {
			r.Original.SizeBytes = &aSize // reads before the analyzer said the size it found
		}
		o, err := r.Original.check()
		if err != nil || (!o.read && o.why == "") {
			t.Errorf("%s: %+v, %v; want a read, or a failed one", name, o, err)
		}
	}
	// and its reads of the made clips, as the analyzer that keeps to the
	// closed lists sends them: a failure keeps what it found of the file
	b, err = os.ReadFile("../library/testdata/captions/clips.json")
	if err != nil {
		t.Fatal(err)
	}
	cases = nil
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for name, c := range cases {
		var r Report
		if err := json.Unmarshal(c.Body, &r); err != nil || r.Original == nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		o, err := r.Original.check()
		switch {
		case err != nil || (!o.read && o.why == ""):
			t.Errorf("%s: %+v, %v; want a read, or a failed one", name, o, err)
		case o.size == nil || o.streams == nil:
			t.Errorf("%s: kept no size or streams of captions of what it found: %+v", name, o)
		case strings.HasPrefix(r.Original.Error, "off the closed lists") && o.offList == nil:
			t.Errorf("%s: a failure off the closed lists kept no offList", name)
		}
		if o.read {
			reads++
		}
	}
	if reads < 15 {
		t.Errorf("%d of the clips are reads", reads)
	}
}

// A failure that found what keeps the original anyway, of the file handed
// out - something off the closed lists, a stream of captions of its own -
// is kept as found: the gate's facts have it (CaptionsFinds: what keeps it
// not known for good, or carried), no read again of the same file changes
// it, so it is not handed out again and counts as read. One of another size
// than the claim's is no find of this file: handed out again after its
// backoff.
func TestAFailureThatFoundWhatKeepsTheOriginalIsKept(t *testing.T) {
	ctx := context.Background()
	none, one := 0, 1
	for _, c := range []struct {
		name, body string
		final      bool
		finds      *library.CaptionsRead // what the gate's facts keep of it; nil nothing
	}{
		{"off the closed lists", `{"error": "off the closed lists: codec:vc1", "ffprobe": "7.1.5", "sizeBytes": 6,
			"captionStreams": 0, "offList": ["codec:vc1"]}`, true,
			&library.CaptionsRead{CaptionStreams: &none, OffList: []string{"codec:vc1"}}},
		{"a track of captions, the file off the lists", `{"error": "off the closed lists: video-streams:2", "ffprobe": "7.1.5",
			"sizeBytes": 6, "captionStreams": 1, "offList": ["video-streams:2"]}`, true,
			&library.CaptionsRead{CaptionStreams: &one, OffList: []string{"video-streams:2"}}},
		{"off the closed lists, another size", `{"error": "off the closed lists: codec:vc1", "ffprobe": "7.1.5",
			"sizeBytes": 26, "captionStreams": 0, "offList": ["codec:vc1"]}`, false, nil},
		{"a failure that found nothing", `{"error": "its file gives no length", "ffprobe": "7.1.5", "sizeBytes": 6,
			"captionStreams": 0, "offList": []}`, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, source, _, token := claimed(t)
			r, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+c.body+`}`), time.Now())
			if err != nil || r.State != StateFailed {
				t.Fatalf("the report: %+v, %v", r, err)
			}
			s, err := library.SourceByID(ctx, st.Pool(), source)
			if err != nil || s == nil {
				t.Fatal(err)
			}
			f, err := library.FactsOf(ctx, st.Pool(), s)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(f.CaptionsFinds)
			want, _ := json.Marshal(c.finds)
			if f.Captions != nil || f.CaptionsFailed == "" || string(got) != string(want) {
				t.Errorf("the gate's facts: read %v, failed %q, finds %s; want finds %s", f.Captions, f.CaptionsFailed,
					got, want)
			}
			svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)))
			p, err := svc.Progress(ctx)
			if err != nil || (p.ToRead == 0) != c.final {
				t.Errorf("the progress: %+v, %v; want it read %v", p, err, c.final)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = '2026-11-16T21:00:00Z'
				WHERE source_id = $1`, source)
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || (len(cl.Items) == 0) != c.final {
				t.Errorf("the claim after its backoff: %+v, %v; want it handed out %v", cl, err, !c.final)
			}
		})
	}
}

// The claim hands out the length the original's record says (durationMs),
// which the analyzer holds its read to; the catalog holds the read to it
// too. A read within DurationSlack of it is a read. One of another length is
// a finding about the record or the file (lengthMismatch): kept for good,
// counted on its own (not read, not to read, its source id listed), not
// handed out again, and the gate keeps the original on it - final for that
// file and that record: once the file is another (its size) or the record
// is (written again), it is to read again, once.
func TestAReadIsOfTheLengthItsRecordSays(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		duration string
		read     bool
	}{
		{"its record's length", "600", true},
		{"within the slack", "605.5", true},
		{"another length", "610", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := storetest.Open(t)
			dir := t.TempDir()
			media := filepath.Join(dir, "film.mkv")
			if err := os.WriteFile(media, []byte("a film"), 0o644); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(dir, "sources", "r1-src")
			if err := os.MkdirAll(record, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(record, "source.json"), []byte(`{"schema": "zaentrum.library.source/2",
				"container": {"format": "matroska,webm", "durationMs": 600000}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			storetest.AddItem(t, st, "r1", "movie", "A Film", "")
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes,
				state, recordedat, recorddir) VALUES ('r1-src', 'r1', 'film.mkv', $1, 6, 'present', now(), $2)`, media, record)
			svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
			cl, err := svc.Claim(ctx, "analyzer-0", 1)
			if err != nil || len(cl.Items) != 1 || cl.Items[0].DurationMs == nil || *cl.Items[0].DurationMs != 600000 {
				t.Fatalf("the claim: %+v, %v; want it to hand out its record's length", cl, err)
			}
			body := strings.Replace(fullRead, `"duration": 600`, `"duration": `+c.duration, 1)
			r, err := Record(ctx, st, "r1-src", report(t, `{"token": "`+cl.Items[0].Token+`", "original": `+body+`}`),
				time.Now())
			if err != nil || (r.State == StateRead) != c.read {
				t.Fatalf("the report: %+v, %v; want read %v", r, err, c.read)
			}
			p, err := svc.Progress(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c.read {
				if p.Read != 1 || p.ToRead != 0 || p.LengthMismatch != 0 {
					t.Errorf("the progress: %+v", p)
				}
				return
			}
			if r.Finding == nil || *r.Finding != library.FindingLengthMismatch || r.Reason == nil ||
				!strings.Contains(*r.Reason, "says it lasts 610 s, and its record 600 s") {
				t.Errorf("the finding: %s, why: %s", deref(r.Finding), deref(r.Reason))
			}
			if p.Read != 0 || p.ToRead != 0 || p.LengthMismatch != 1 || strings.Join(p.LengthMismatches, ",") != "r1-src" {
				t.Errorf("the progress: %+v; want a length mismatch of its own", p)
			}
			s, err := library.SourceByID(ctx, st.Pool(), "r1-src")
			if err != nil || s == nil {
				t.Fatal(err)
			}
			if f, err := library.FactsOf(ctx, st.Pool(), s); err != nil || f.CaptionsFinds == nil ||
				f.CaptionsFinds.Finding != library.FindingLengthMismatch {
				t.Errorf("the gate's facts: %+v, %v; want the finding", f, err)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = '2026-11-16T21:00:00Z'`)
			if again, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(again.Items) != 0 {
				t.Errorf("a length mismatch was handed out again: %+v, %v", again, err)
			}
			// the record written again: to read again, once
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET recordedat = recordedat + interval '1 hour'`)
			if p, err := svc.Progress(ctx); err != nil || p.ToRead != 1 || p.LengthMismatch != 0 {
				t.Errorf("the progress once the record is another: %+v, %v", p, err)
			}
			again, err := svc.Claim(ctx, "analyzer-0", 1)
			if err != nil || len(again.Items) != 1 {
				t.Fatalf("the claim once the record is another: %+v, %v", again, err)
			}
			if _, err := Record(ctx, st, "r1-src", report(t, `{"token": "`+again.Items[0].Token+`", "original": `+body+`}`),
				time.Now()); err != nil {
				t.Fatal(err)
			}
			if p, err := svc.Progress(ctx); err != nil || p.LengthMismatch != 1 || p.ToRead != 0 {
				t.Errorf("the progress after the read of the record written again: %+v, %v", p, err)
			}
			// the file another (its size): to read again
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET sizebytes = 26`)
			if p, err := svc.Progress(ctx); err != nil || p.ToRead != 1 || p.LengthMismatch != 0 {
				t.Errorf("the progress once the file is another: %+v, %v", p, err)
			}
		})
	}
}

// withCaptions is fullRead with captions in its window at 50 %.
var withCaptions = strings.Replace(fullRead, `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": false`,
	`{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true`, 1)

// at is a clock that says t.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// A read is of the file handed out, not of the source id: once another file
// is at the original's place (of another size), the read of the one before
// is no read the gate takes, and the claim hands the source out to be read
// again; a report of a file of another size than the claim's is no read;
// the scanner's Refix of a source not recorded yet forgets the read.
func TestAReadOfAnotherFileIsNoReadOfTheSource(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	if _, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+fullRead+`}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	facts := func() library.GateFacts {
		t.Helper()
		s, err := library.SourceByID(ctx, st.Pool(), source)
		if err != nil || s == nil {
			t.Fatalf("the source: %+v, %v", s, err)
		}
		f, err := library.FactsOf(ctx, st.Pool(), s)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	claim := func() Claim {
		t.Helper()
		c, err := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC))).Claim(ctx, "analyzer-0", 1)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if facts().Captions == nil {
		t.Fatal("the read of the file handed out is not the source's")
	}
	if c := claim(); len(c.Items) != 0 {
		t.Fatalf("a source read in full was handed out again: %+v", c)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET sizebytes = 26 WHERE id = $1`, source)
	if f := facts(); f.Captions != nil {
		t.Errorf("the gate's facts of another file at the original's place are the read of the one before: %d windows",
			len(f.Captions.Windows))
	}
	c := claim()
	if len(c.Items) != 1 {
		t.Fatalf("the changed file is not handed out to be read: %+v", c)
	}
	// a report of a file of the size before is no read of this one
	if _, err := Record(ctx, st, source, report(t, `{"token": "`+c.Items[0].Token+`", "original": `+fullRead+`}`),
		time.Now()); err != nil {
		t.Fatal(err)
	}
	if f := facts(); f.Captions != nil {
		t.Error("a read of a file of the size before was taken of the file now")
	}
	// the scanner's Refix of a source not recorded yet
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET recordedat = NULL WHERE id = $1`, source)
	other := filepath.Join(t.TempDir(), "another.mkv")
	if err := os.WriteFile(other, []byte("another film, a longer one"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, qh1, err := library.QH1(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := library.Refix(ctx, st.Pool(), source, size, qh1); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`,
		source); n != 0 {
		t.Errorf("the Refix kept what a read found of the file before (%d rows)", n)
	}
	if c := claim(); len(c.Items) != 0 {
		t.Errorf("a source not recorded was handed out: %+v", c)
	}
}

// A read of the file handed out that found captions is kept for good: a
// later report of the same file that found none replaces it not, and the
// claim hands it out no more; a read of another file (another size) does.
func TestAReadThatFoundCaptionsIsKeptForGood(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	r, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+withCaptions+`}`), time.Now())
	if err != nil || r.Captions == nil || !*r.Captions {
		t.Fatalf("the read with captions: %+v, %v", r, err)
	}
	c, err := New(st).WithClock(at(time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC))).Claim(ctx, "analyzer-0", 1)
	if err != nil || len(c.Items) != 0 {
		t.Fatalf("a read that found captions was handed out again: %+v, %v", c, err)
	}
	again := func(tok string, size int, body string) *Read {
		t.Helper()
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET claimedat = now(), claimtoken = $2,
			claimsize = $3 WHERE source_id = $1`, source, tok, size)
		r, err := Record(ctx, st, source, report(t, `{"token": "`+tok+`", "original": `+body+`}`), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := again("same-file", 6, fullRead); r.Captions == nil || !*r.Captions {
		t.Errorf("a read of the same file that found none replaced the one that found captions: %+v", r)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET sizebytes = 26 WHERE id = $1`, source)
	other := strings.Replace(fullRead, `"sizeBytes": 6`, `"sizeBytes": 26`, 1)
	if r := again("another-file", 26, other); r.Captions == nil || *r.Captions {
		t.Errorf("the read of another file kept the captions the one before had: %+v", r)
	}
}

// A read the catalog refuses (its windows say nothing of the video: here
// all of them in the first 12 s of a ten-minute video, its length its
// record's) is no read: it counts as to read, and is handed out again after
// its backoff.
func TestARefusedReadIsToRead(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	var ws []string
	for i, w := range []string{"0", "0.1", "0.3", "0.5", "0.7", "0.9"} {
		ws = append(ws, fmt.Sprintf(`{"at": %s, "start": %d, "end": %d.88, "frames": 48, "captions": false,
			"interlacedFrames": 0, "repeatFrames": 0}`, w, 2*i, 2*i))
	}
	far := `{"captionWindows": [` + strings.Join(ws, ", ") + `], "duration": 600, "sizeBytes": 6, "ffprobe": "7.1.5"}`
	r, err := Record(ctx, st, source, report(t, `{"token": "`+token+`", "original": `+far+`}`), time.Now())
	if err != nil || r.State == StateRead || r.Reason == nil || !strings.Contains(*r.Reason, "from their places") {
		t.Fatalf("a read whose windows lie far from their places: %+v, %v", r, err)
	}
	svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)))
	p, err := svc.Progress(ctx)
	if err != nil || p.Originals != 1 || p.Read != 0 || p.ToRead != 1 || p.Failed != 1 {
		t.Errorf("the progress: %+v, %v; want it to read, failed", p, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = '2026-11-16T21:00:00Z'
		WHERE source_id = $1`, source)
	if c, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 {
		t.Errorf("a refused read was not handed out again after its backoff: %+v, %v", c, err)
	}
}

// deref is *s, "<nil>" for none.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
