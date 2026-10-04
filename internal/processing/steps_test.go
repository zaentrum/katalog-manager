package processing_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// stepRow is what a step holds of its retries.
type stepRow struct {
	status       string
	failures     int
	err, last    string // "" for NULL
	retryInSecs  float64
	retryAt      bool
	dispatchedAt bool
	finished     bool
}

func row(t *testing.T, st *store.Store, item, step string) stepRow {
	t.Helper()
	var r stepRow
	var errText, last *string
	var in *float64
	if err := st.Pool().QueryRow(context.Background(), `SELECT status, failures, error, lasterror,
		extract(epoch FROM nextretryat - modifiedat::timestamptz)::float8, dispatchedat IS NOT NULL, finishedat IS NOT NULL
		FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = $2`, item, step).
		Scan(&r.status, &r.failures, &errText, &last, &in, &r.dispatchedAt, &r.finished); err != nil {
		t.Fatalf("read %s %s: %v", item, step, err)
	}
	if errText != nil {
		r.err = *errText
	}
	if last != nil {
		r.last = *last
	}
	if in != nil {
		r.retryAt, r.retryInSecs = true, *in
	}
	return r
}

func s(v string) *string { return &v }

func upsert(t *testing.T, steps *processing.Steps, item, step, status string, errMsg *string) {
	t.Helper()
	if err := steps.Upsert(context.Background(), item, step, status, errMsg, nil); err != nil {
		t.Fatalf("upsert %s %s %s: %v", item, step, status, err)
	}
}

// What a worker's reports make of a step's retries: a failure counts and is
// scheduled for a retry a backoff later, as long as the policy retries that
// many failures in a row; a failure reported again counts nothing; done,
// skipped and not applicable end the failures; only a failed step has a
// retry scheduled, and any report answers a trigger sent again.
func TestAStepsReportsKeepItsRetries(t *testing.T) {
	st := storetest.Open(t)
	steps := processing.New(st.Pool()).WithPolicy(processing.Policy{MaxAttempts: 3, Backoff: time.Minute, BackoffMax: time.Hour})
	ctx := context.Background()

	// a first failure, on a step that did not exist
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, s("ffmpeg exited 1"))
	if r := row(t, st, "m1", "transcode"); r.status != "failed" || r.failures != 1 || r.err != "ffmpeg exited 1" ||
		r.last != "ffmpeg exited 1" || !r.retryAt || r.retryInSecs != 60 || !r.finished {
		t.Errorf("a first failure: %+v, want one failure, retried a minute later", r)
	}
	// reported again: nothing more counted, the schedule kept; a new
	// message replaces the error, none keeps it
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET nextretryat = now() + interval '30 seconds' WHERE item_id = 'm1'`)
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, nil)
	if r := row(t, st, "m1", "transcode"); r.failures != 1 || r.last != "ffmpeg exited 1" || r.retryInSecs > 31 {
		t.Errorf("a failure reported again without a message: %+v", r)
	}
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, s("ffmpeg exited 137"))
	if r := row(t, st, "m1", "transcode"); r.failures != 1 || r.last != "ffmpeg exited 137" || r.err != "ffmpeg exited 137" {
		t.Errorf("a failure reported again with a message: %+v", r)
	}

	// a retry: sent again, started, failed again — two minutes this time
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'pending', nextretryat = NULL,
		dispatchedat = now() WHERE item_id = 'm1'`)
	upsert(t, steps, "m1", "transcode", processing.StatusInProgress, nil)
	if r := row(t, st, "m1", "transcode"); r.status != "in_progress" || r.failures != 1 || r.err != "" ||
		r.last != "ffmpeg exited 137" || r.retryAt || r.dispatchedAt {
		t.Errorf("a retry started: %+v, want its last error kept, nothing scheduled, the dispatch answered", r)
	}
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, s("out of memory"))
	if r := row(t, st, "m1", "transcode"); r.failures != 2 || r.retryInSecs != 120 || r.last != "out of memory" {
		t.Errorf("a second failure: %+v, want two, retried two minutes later", r)
	}
	// the third run fails too: no attempt left
	upsert(t, steps, "m1", "transcode", processing.StatusInProgress, nil)
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, s("out of memory"))
	if r := row(t, st, "m1", "transcode"); r.failures != 3 || r.retryAt {
		t.Errorf("a third failure: %+v, want three and no retry by itself", r)
	}

	// done ends the failures, and keeps the last error as the record
	upsert(t, steps, "m1", "transcode", processing.StatusInProgress, nil)
	upsert(t, steps, "m1", "transcode", processing.StatusDone, nil)
	if r := row(t, st, "m1", "transcode"); r.status != "done" || r.failures != 0 || r.retryAt || r.last != "out of memory" {
		t.Errorf("done after failures: %+v", r)
	}
	// and a later failure starts counting afresh
	upsert(t, steps, "m1", "transcode", processing.StatusFailed, s("disk full"))
	if r := row(t, st, "m1", "transcode"); r.failures != 1 || r.retryInSecs != 60 {
		t.Errorf("a failure after done: %+v, want the first of a new series", r)
	}

	// skipped and not applicable end the failures, and are never scheduled
	for _, status := range []string{processing.StatusSkipped, processing.StatusNotApplicable} {
		upsert(t, steps, "m2", "subtitle", processing.StatusFailed, s("whisper crashed"))
		upsert(t, steps, "m2", "subtitle", status, nil)
		if r := row(t, st, "m2", "subtitle"); r.status != status || r.failures != 0 || r.retryAt {
			t.Errorf("%s after a failure: %+v", status, r)
		}
	}
	// pending (a transcode promoting the package) schedules nothing
	upsert(t, steps, "m2", "package", processing.StatusFailed, s("shaka exited 1"))
	if err := steps.PromoteTranscodeToPackage(ctx, "m2", processing.StatusDone); err != nil {
		t.Fatal(err)
	}
	if r := row(t, st, "m2", "package"); r.status != "pending" || r.failures != 1 || r.retryAt {
		t.Errorf("a package promoted after its failure: %+v, want pending, its failure counted, nothing scheduled", r)
	}

	// a credential in an error never reaches the table
	upsert(t, steps, "m3", "package", processing.StatusFailed, s("POST http://km:hunter2@katalog/x?token="+strings.Repeat("t", 40)+" failed"))
	if r := row(t, st, "m3", "package"); strings.Contains(r.err+r.last, "hunter2") || strings.Contains(r.err+r.last, "ttt") {
		t.Errorf("the error kept a credential: %+v", r)
	}
}

// A policy of one attempt schedules no retry; the step still counts its
// failure.
func TestOneAttemptRetriesNothingByItself(t *testing.T) {
	st := storetest.Open(t)
	steps := processing.New(st.Pool()).WithPolicy(processing.Policy{MaxAttempts: 1, Backoff: time.Minute, BackoffMax: time.Hour})
	upsert(t, steps, "m1", "package", processing.StatusFailed, s("x"))
	if r := row(t, st, "m1", "package"); r.failures != 1 || r.retryAt {
		t.Errorf("one attempt: %+v, want a failure and no retry", r)
	}
}

// A reset waits for the worker afresh: no failures, nothing scheduled or
// sent; its last error stays.
func TestAResetStartsAfresh(t *testing.T) {
	st := storetest.Open(t)
	steps := processing.New(st.Pool())
	upsert(t, steps, "e1", "blackframe", processing.StatusFailed, s("ffmpeg exited 1"))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET dispatchedat = now()`)
	if n, err := steps.ResetForItems(context.Background(), []string{"e1"}, []string{"blackframe"}); err != nil || n != 1 {
		t.Fatalf("ResetForItems: %d, %v", n, err)
	}
	if r := row(t, st, "e1", "blackframe"); r.status != "pending" || r.failures != 0 || r.retryAt || r.dispatchedAt || r.last != "ffmpeg exited 1" {
		t.Errorf("a reset step: %+v", r)
	}
}

// On a catalog without migration 033 the steps are written as before, an
// error cleaned as well: nothing of the retries is kept, and nothing fails.
func TestStepsWithoutTheRetriesMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	steps := processing.New(st.Pool())
	ctx := context.Background()
	for _, status := range []string{processing.StatusInProgress, processing.StatusFailed, processing.StatusFailed} {
		if err := steps.Upsert(ctx, "m1", "transcode", status, s("token rejected: Bearer "+strings.Repeat("b", 30)), nil); err != nil {
			t.Fatalf("upsert %s without 033: %v", status, err)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm1'
		AND status = 'failed' AND attempts = 3 AND error = 'token rejected: Bearer REDACTED'`); n != 1 {
		t.Error("the step was not written as before 033")
	}
	if n, err := steps.ResetForItems(ctx, []string{"m1"}, []string{"transcode"}); err != nil || n != 1 {
		t.Errorf("ResetForItems without 033: %d, %v", n, err)
	}
}

// A worker's error reaches the table cleaned: no credential, and a long one
// cut on a character, which Postgres takes (a cut inside one it refused, and
// the report was lost).
func TestAStepsErrorIsKeptClean(t *testing.T) {
	st := storetest.Open(t)
	steps := processing.New(st.Pool())
	long := strings.Repeat("Fehler bei der Größe ", 40) // multibyte, over 500 characters
	for item, msg := range map[string]string{
		"m1": "POST http://km:hunter2@katalog/x?token=" + strings.Repeat("t", 40) + " failed",
		"m2": long,
	} {
		if err := steps.Upsert(context.Background(), item, "package", processing.StatusFailed, &msg, nil); err != nil {
			t.Fatalf("upsert %s: %v", item, err)
		}
	}
	var e1, e2 string
	if err := st.Pool().QueryRow(context.Background(), `SELECT
		(SELECT error FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm1'),
		(SELECT error FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'm2')`).Scan(&e1, &e2); err != nil {
		t.Fatal(err)
	}
	if e1 != "POST http://REDACTED@katalog/x?token=REDACTED failed" {
		t.Errorf("the error kept a credential: %q", e1)
	}
	if utf8.RuneCountInString(e2) != processing.MaxErrorLength || !strings.HasSuffix(e2, "…") {
		t.Errorf("a long error: %d characters", utf8.RuneCountInString(e2))
	}
}
