package captions

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// replaced is fullRead with the window at the fraction at (its JSON as
// fullRead writes it, "0.5") written as window.
func replaced(at, window string) string {
	i := strings.Index(fullRead, `{"at": `+at+`,`)
	j := strings.Index(fullRead[i:], "}")
	return fullRead[:i] + window + fullRead[i+j+1:]
}

// record keeps the report body of the claim token of the original source,
// and answers what the catalog keeps of it now.
func record(t *testing.T, ctx context.Context, svc *Service, source, token, body string) *Read {
	t.Helper()
	r, err := Record(ctx, svc.st, source, report(t, `{"token": "`+token+`", "original": `+body+`}`), svc.now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The worst a read says decides: captions in a window of the file handed
// out, or frames counted in fields, are kept as found - from a read one of
// whose windows lies far from its place, from a window otherwise malformed,
// from a claim whose lease ended - and no later clean read of the same file
// replaces them. A read kept so is done: not handed out again.
func TestTheWorstReadDecides(t *testing.T) {
	ctx := context.Background()
	clock := at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC))
	captionsAt50 := `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true, "interlacedFrames": 0, "repeatFrames": 0}`
	for _, c := range []struct {
		name, body string
		captions   bool
	}{
		{"captions at 50 %, the window at 90 % 100 s off its place", strings.Replace(replaced("0.5", captionsAt50),
			`{"at": 0.9, "start": 540, "end": 541.88`, `{"at": 0.9, "start": 640, "end": 641.88`, 1), true},
		{"captions in a window whose count can't be read", replaced("0.5", `{"at": 0.5, "start": 300, "end": 301.88,
			"frames": 48, "captions": true, "interlacedFrames": 49, "repeatFrames": 0}`), true},
		{"captions in a window whose keyframeBefore is no boolean", replaced("0.5", `{"at": 0.5, "start": 300,
			"end": 301.88, "frames": 48, "captions": true, "interlacedFrames": 0, "repeatFrames": 0, "keyframeBefore": "yes"}`),
			true},
		{"captions true then false in one window", replaced("0.5", `{"at": 0.5, "start": 300, "end": 301.88, "frames": 48,
			"captions": true, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}`), true},
		{"interlaced frames at 30 %, the window at 90 % failed", strings.Replace(replaced("0.3", `{"at": 0.3, "start": 180,
			"end": 181.88, "frames": 48, "captions": false, "interlacedFrames": 12, "repeatFrames": 0}`),
			`{"at": 0.9, "start": 540, "end": 541.88, "frames": 48`, `{"at": 0.9, "start": 540, "end": 541.88, "frames": 0`, 1),
			false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, source, _, token := claimed(t)
			svc := New(st).WithClock(clock)
			r := record(t, ctx, svc, source, token, c.body)
			if r.State != StateRead || r.Captions == nil || *r.Captions != c.captions || !r.found() {
				t.Fatalf("the read: %+v; want it kept as found (captions %v)", r, c.captions)
			}
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Fatalf("a read kept as found was handed out again: %+v, %v", cl, err)
			}
			// a later clean read of the same file replaces it not
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET claimedat = now(), claimtoken = 'again'
				WHERE source_id = $1`, source)
			if r := record(t, ctx, svc, source, "again", fullRead); !r.found() {
				t.Errorf("a clean read of the same file replaced what the worst one found: %+v", r)
			}
		})
	}
}

// A late report of a claim whose lease ended replaces no read made since,
// unless it found what keeps the original for good: then the worst read
// decides, and the late one is kept.
func TestALateReportReplacesNoNewerReadButTheWorstDecides(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, newer, late string
		captions          bool
	}{
		{"a late clean read, a read with a failed window since", replaced("0.9", `{"at": 0.9, "frames": 0}`), fullRead,
			false},
		{"a late read with captions, a clean read since", fullRead, withCaptions, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, source, _, late := claimed(t)
			svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 30, 0, 0, time.UTC)))
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Fatalf("the claim that ends the lease: %+v, %v", cl, err)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = NULL WHERE source_id = $1`,
				source)
			cl, err := svc.Claim(ctx, "analyzer-1", 1)
			if err != nil || len(cl.Items) != 1 {
				t.Fatalf("the claim after the lease: %+v, %v", cl, err)
			}
			record(t, ctx, svc, source, cl.Items[0].Token, c.newer)
			r := record(t, ctx, svc, source, late, c.late)
			if r.State != StateRead || r.Captions == nil || *r.Captions != c.captions {
				t.Errorf("after the late report: %+v; want captions %v", r, c.captions)
			}
			if !c.captions && (r.Reason == nil || !strings.Contains(*r.Reason, "90% decoded no frame")) {
				t.Errorf("the late clean report replaced the read made since: %+v", r)
			}
		})
	}
}

// A report the catalog takes that leaves the original not read in full - a
// read whose windows say nothing of interlaced or repeated frames (an older
// analyzer's), a read with a window that decoded no frame, a refused one -
// counts as a failure and waits its backoff; it is not handed out again a
// second later. A length mismatch reported while a partial read is kept is
// kept as its finding, and is not handed out again either.
func TestAReportThatLeavesItNotReadWaitsItsBackoff(t *testing.T) {
	ctx := context.Background()
	silent := strings.NewReplacer(`, "interlacedFrames": 0, "repeatFrames": 0`, "").Replace(fullRead)
	for _, c := range []struct{ name, body string }{
		{"a read that says nothing of its fields", silent},
		{"a read with a window that decoded no frame", replaced("0.9", `{"at": 0.9, "frames": 0}`)},
		{"five windows and none at the start", strings.Replace(fullRead, `{"at": 0, "start": 0, "end": 1.88, "frames": 48, `+
			`"captions": false, "interlacedFrames": 0, "repeatFrames": 0},`, "", 1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, source, _, token := claimed(t)
			svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 1, 0, time.UTC)))
			r := record(t, ctx, svc, source, token, c.body)
			if r.Failures != 1 || r.NextAttemptAt == nil || !r.NextAttemptAt.After(svc.now()) {
				t.Fatalf("the report: %+v; want a failure counted and its backoff", r)
			}
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Errorf("handed out again a second later: %+v, %v", cl, err)
			}
		})
	}
	t.Run("a length mismatch after a partial read", func(t *testing.T) {
		st, source, _, token := claimed(t)
		svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 1, 0, time.UTC)))
		record(t, ctx, svc, source, token, replaced("0.9", `{"at": 0.9, "frames": 0}`))
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET claimedat = now(), claimtoken = 'again'
			WHERE source_id = $1`, source)
		r := record(t, ctx, svc, source, "again", strings.Replace(fullRead, `"duration": 600`, `"duration": 640`, 1))
		if r.Finding == nil || *r.Finding != library.FindingLengthMismatch {
			t.Fatalf("the mismatch after a partial read: %+v; want its finding kept", r)
		}
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = NULL WHERE source_id = $1`, source)
		if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
			t.Errorf("a finding was handed out again: %+v, %v", cl, err)
		}
	})
}

// The hourly rate counts hand-outs: an original handed out, given back and
// handed out again counts twice, so that the rate bounds the reads.
func TestTheHourlyRateCountsHandOuts(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('cr', 'captions.backfill.rate', '2')`)
	t0 := time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)
	if _, err := Record(ctx, st, source, Report{Token: token, Released: true}, t0); err != nil {
		t.Fatal(err)
	}
	svc := New(st).WithClock(at(t0.Add(20 * time.Minute)))
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 1 {
		t.Fatalf("handed out again once its release waited: %+v, %v", cl, err)
	} else if _, err := Record(ctx, st, source, Report{Token: cl.Items[0].Token, Released: true}, t0.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	svc = New(st).WithClock(at(t0.Add(40 * time.Minute)))
	cl, err := svc.Claim(ctx, "analyzer-0", 1)
	if err != nil || len(cl.Items) != 0 || !strings.Contains(cl.Idle, "2 hand-outs were made in the last hour") {
		t.Errorf("a third hand-out within the hour of a rate of 2: %+v, %v", cl, err)
	}
}

// A record that said no length when its original was handed out leaves
// where the read's windows lie not known: the report is kept as its finding
// (noRecordLength), final for that file and that record, not handed out
// again until the record is written again.
func TestARecordWithoutALengthIsAFindingAtThePut(t *testing.T) {
	ctx := context.Background()
	st, source, _, _ := claimed(t)
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_sourcecaptions`)
	var dir string
	if err := st.Pool().QueryRow(ctx, `SELECT recorddir FROM com_nalet_katalog_itemsources WHERE id = $1`, source).
		Scan(&dir); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, strings.TrimSuffix(dir, "/sources/r1-src"), strings.Replace(tenMinuteRecord, `,
	"durationMs": 600000`, "", 1))
	svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
	cl, err := svc.Claim(ctx, "analyzer-0", 1)
	if err != nil || len(cl.Items) != 1 || cl.Items[0].DurationMs != nil {
		t.Fatalf("the claim: %+v, %v; want it handed out with no length", cl, err)
	}
	r := record(t, ctx, svc, source, cl.Items[0].Token, fullRead)
	if r.State == StateRead || r.Finding == nil || *r.Finding != library.FindingNoLength {
		t.Fatalf("a read of an original whose record says no length: %+v; want its finding", r)
	}
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
		t.Errorf("handed out again with its record as it was: %+v, %v", cl, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET recordedat = now() + interval '1 second' WHERE id = $1`,
		source)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET nextattemptat = NULL WHERE source_id = $1`, source)
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 1 {
		t.Errorf("not handed out again once its record was written again: %+v, %v", cl, err)
	}
}

// A clean read is of its record as it was: once the record is written again
// (a record carrying a caption signature, a length), the original is handed
// out to be read again, which lets its hold go; a read that found captions
// is of the file, whichever record.
func TestARewrittenRecordHasItsOriginalReadAgain(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, body string
		again      bool
	}{
		{"a clean read", fullRead, true},
		{"a read that found captions", withCaptions, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, source, _, token := claimed(t)
			svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 0, 0, time.UTC)))
			record(t, ctx, svc, source, token, c.body)
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
				t.Fatalf("handed out again with its record as it was: %+v, %v", cl, err)
			}
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET recordedat = now() + interval '1 second'
				WHERE id = $1`, source)
			if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || (len(cl.Items) == 1) != c.again {
				t.Errorf("once its record was written again: %+v, %v; want it handed out %v", cl, err, c.again)
			}
		})
	}
}

// A report that says a thing of itself twice says it neither way: it is
// refused (RepeatedKey names the key; the PUT answers 400), whereas a key
// repeated inside a window is the window's own, which makes it a failed
// window.
func TestAReportThatSaysAThingTwiceIsRefused(t *testing.T) {
	for body, want := range map[string]string{
		`{"token": "a", "token": "b", "original": {}}`:                                            "token",
		`{"token": "a", "original": {"sizeBytes": 6, "sizeBytes": 7}}`:                            "sizeBytes",
		`{"token": "a", "original": {"captionWindows": [], "captionWindows": []}}`:                "captionWindows",
		`{"token": "a", "original": {"captionWindows": [{"captions": true, "captions": false}]}}`: "",
		`{"token": "a", "original": ` + fullRead + `}`:                                            "",
	} {
		if got := RepeatedKey([]byte(body)); got != want {
			t.Errorf("%s: %q; want %q", body, got, want)
		}
	}
}

// A clean read keeps its report as the analyzer sent it (readreport), the one
// the contract's deletion gate reads; a clean read kept without one (taken
// before the catalog kept reports) is not done: it is handed out again.
func TestACleanReadKeepsItsReport(t *testing.T) {
	ctx := context.Background()
	st, source, _, token := claimed(t)
	svc := New(st).WithClock(at(time.Date(2026, 11, 16, 23, 0, 1, 0, time.UTC)))
	if r := record(t, ctx, svc, source, token, fullRead); r.State != StateRead || r.found() {
		t.Fatalf("the read: %+v; want a clean one", r)
	}
	var kept *string
	if err := st.Pool().QueryRow(ctx, `SELECT readreport FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`,
		source).Scan(&kept); err != nil || kept == nil || *kept != fullRead {
		t.Fatalf("the report kept: %v, %v; want it as sent", kept, err)
	}
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 0 {
		t.Fatalf("a clean read with its report was handed out again: %+v, %v", cl, err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_sourcecaptions SET readreport = NULL WHERE source_id = $1`, source)
	if cl, err := svc.Claim(ctx, "analyzer-0", 1); err != nil || len(cl.Items) != 1 {
		t.Errorf("a clean read without its report was not handed out again: %+v, %v", cl, err)
	}
}
