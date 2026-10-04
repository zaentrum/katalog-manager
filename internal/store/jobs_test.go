package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 034 a scan job has these columns: the two it adds, nothing in them
// for a job older than it.
const scanJobShape = `id character varying(36) NOT NULL
source character varying(20) NOT NULL
status character varying(20) NOT NULL
startedat timestamp without time zone NULL
finishedat timestamp without time zone NULL
errormessage text NULL
filesseen integer NULL
itemsinserted integer NULL
itemsupdated integer NULL
runner character varying(255) NULL
heartbeatat timestamp with time zone NULL`

// 034 gives a scan job its runner and its last word, neither for a job older
// than it; running it again changes nothing, and the startup check applies it
// where a column of it is missing, and only then.
func TestScanJobRunnerMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat)
		VALUES ('j0', 'nfs', 'running', '2026-10-01 10:00:00')`)
	if ready, err := st.ScanJobRunnerReady(ctx); err != nil || ready {
		t.Fatalf("ScanJobRunnerReady on a catalog without 034: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureScanJobRunner(ctx); err != nil {
			t.Fatalf("EnsureScanJobRunner, %d. time: %v", run, err)
		}
	}
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.ScanJobRunner); err != nil {
			t.Fatalf("applying 034 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.ScanJobRunnerReady(ctx); err != nil || !ready {
		t.Fatalf("ScanJobRunnerReady after EnsureScanJobRunner: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_scanjobs"); got != scanJobShape {
		t.Errorf("scan jobs after 034:\n%s\nwant:\n%s", got, scanJobShape)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = 'j0'
		AND status = 'running' AND startedat = '2026-10-01 10:00:00' AND runner IS NULL AND heartbeatat IS NULL`); n != 1 {
		t.Error("a scan job older than 034 must come through as it was, naming no runner")
	}

	// A column missing brings the migration back, and the other keeps what
	// it holds.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_scanjobs SET runner = 'host-a/0001' WHERE id = 'j0'`)
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_scanjobs DROP COLUMN heartbeatat`)
	if ready, err := st.ScanJobRunnerReady(ctx); err != nil || ready {
		t.Fatalf("ScanJobRunnerReady with a column of 034 missing: %v, %v", ready, err)
	}
	if err := st.EnsureScanJobRunner(ctx); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.ScanJobRunnerReady(ctx); err != nil || !ready {
		t.Fatalf("ScanJobRunnerReady after the startup check: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = 'j0' AND runner = 'host-a/0001'`); n != 1 {
		t.Error("applying 034 again changed what a scan job holds")
	}
}

// storetest.Open gives a test every migration the service applies at startup,
// 034 among them.
func TestTheTestSchemaHasTheScanJobRunner(t *testing.T) {
	if ready, err := storetest.Open(t).ScanJobRunnerReady(context.Background()); err != nil || !ready {
		t.Fatalf("ScanJobRunnerReady on the test schema: %v, %v", ready, err)
	}
}

// heard is how long ago a scan job's last word was, in seconds.
func heard(t *testing.T, st *store.Store, id string) float64 {
	t.Helper()
	var secs float64
	if err := st.Pool().QueryRow(context.Background(), `SELECT extract(epoch FROM now() - heartbeatat)::float8
		FROM com_nalet_katalog_scanjobs WHERE id = $1`, id).Scan(&secs); err != nil {
		t.Fatalf("the last word of %s: %v", id, err)
	}
	return secs
}

// A scan job starts running and naming its runner, its last word when it
// starts; a word moves that on while it runs and never after it ended.
// Without 034 a job starts as before, and a word is nothing.
func TestStartAndBeatAScanJob(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	j, err := st.StartScanJob(ctx, "nfs", "host-a/0001")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "running" || j.Source != "nfs" || j.StartedAt == nil || j.FinishedAt != nil ||
		*j.FilesSeen != 0 || *j.ItemsInserted != 0 || *j.ItemsUpdated != 0 {
		t.Errorf("a scan job started: %+v", j)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND runner = 'host-a/0001'`, j.ID); n != 1 {
		t.Error("a scan job started without its runner")
	}
	if s := heard(t, st, j.ID); s < 0 || s > 5 {
		t.Errorf("a scan job's first word was %vs ago, want now", s)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_scanjobs SET heartbeatat = now() - interval '1 hour' WHERE id = $1`, j.ID)
	if err := st.BeatScanJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if s := heard(t, st, j.ID); s < 0 || s > 5 {
		t.Errorf("after a word, the last was %vs ago, want now", s)
	}
	if err := st.FinishScanJob(ctx, j.ID, store.ScanJobResult{Status: "done"}); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_scanjobs SET heartbeatat = now() - interval '1 hour' WHERE id = $1`, j.ID)
	if err := st.BeatScanJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if s := heard(t, st, j.ID); s < 3500 {
		t.Errorf("a word after the scan ended moved its last on (%vs ago)", s)
	}

	base := storetest.OpenBase(t)
	old, err := base.StartScanJob(ctx, "nfs", "host-a/0001")
	if err != nil || old.Status != "running" || old.StartedAt == nil {
		t.Fatalf("a scan job started without 034: %+v, %v", old, err)
	}
	if err := base.BeatScanJob(ctx, old.ID); err != nil {
		t.Errorf("a word without 034: %v", err)
	}
}

// scanJob gives the catalog a scan job: its status, its runner (nil: none)
// and the columns set says.
func scanJob(t *testing.T, st *store.Store, id, status string, runner *string, set string) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat, runner, heartbeatat)
		VALUES ($1, 'nfs', $2, localtimestamp, $3, now())`, id, status, runner)
	if set != "" {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_scanjobs SET `+set+` WHERE id = $1`, id)
	}
}

// jobState is a scan job's status and its error, "-" for none, and whether
// it has ended.
func jobState(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT status || ' ' || COALESCE(errormessage, '-') ||
		' ended=' || (finishedat IS NOT NULL) FROM com_nalet_katalog_scanjobs WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("the scan job %s: %v", id, err)
	}
	return out
}

// At startup the scan jobs a previous process left running are failed,
// saying why: one an earlier process of this host ran, and one that names no
// runner. This process's, another host's (one whose name begins with this
// host's among them) and every job that is not running stay as they are.
// Without 034 nothing is failed.
func TestFailInterruptedScanJobs(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	scanJob(t, st, "before-here", "running", str("host-a/0001"), "")
	scanJob(t, st, "unnamed", "running", nil, "")
	scanJob(t, st, "mine", "running", str("host-a/0002"), "")
	scanJob(t, st, "there", "running", str("host-b/0001"), "")
	scanJob(t, st, "a-longer-name", "running", str("host-ab/0001"), "")
	scanJob(t, st, "done", "done", str("host-a/0001"), "finishedat = localtimestamp")
	scanJob(t, st, "failed", "failed", nil, "finishedat = localtimestamp, errormessage = 'the disk is gone'")

	n, err := st.FailInterruptedScanJobs(ctx, "host-a/0002", "interrupted: a restart")
	if err != nil || n != 2 {
		t.Fatalf("FailInterruptedScanJobs: %d, %v; want 2", n, err)
	}
	for id, want := range map[string]string{
		"before-here":   "failed interrupted: a restart ended=true",
		"unnamed":       "failed interrupted: a restart ended=true",
		"mine":          "running - ended=false",
		"there":         "running - ended=false",
		"a-longer-name": "running - ended=false",
		"done":          "done - ended=true",
		"failed":        "failed the disk is gone ended=true",
	} {
		if got := jobState(t, st, id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	if n, err := st.FailInterruptedScanJobs(ctx, "host-a/0002", "interrupted: a restart"); err != nil || n != 0 {
		t.Errorf("a second time: %d, %v", n, err)
	}

	base := storetest.OpenBase(t)
	storetest.Exec(t, base, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat) VALUES ('j', 'nfs', 'running', localtimestamp)`)
	if n, err := base.FailInterruptedScanJobs(ctx, "host-a/0002", "interrupted"); err != nil || n != 0 {
		t.Errorf("without 034: %d, %v; want nothing failed", n, err)
	}
}

// The reaper's part: a scan job running without a word for longer than the
// timeout is failed, saying why — counted from its last word, or from its
// start when it has none (a job older than 034), or for ever when it has
// neither. One that spoke within the timeout, one that started within it and
// one that is not running stay as they are, whatever the session's time zone.
// Without 034 nothing is failed.
func TestFailSilentScanJobs(t *testing.T) {
	for _, tz := range []string{"", "Pacific/Kiritimati", "America/Los_Angeles"} {
		st := storetest.OpenInTimeZone(t, tz)
		ctx := context.Background()
		scanJob(t, st, "silent", "running", str("host-a/0001"), `heartbeatat = now() - interval '20 minutes',
			startedat = localtimestamp - interval '2 hours'`)
		scanJob(t, st, "speaking", "running", str("host-b/0001"), `heartbeatat = now() - interval '5 minutes',
			startedat = localtimestamp - interval '2 hours'`)
		scanJob(t, st, "old-silent", "running", nil, `heartbeatat = NULL, startedat = localtimestamp - interval '20 minutes'`)
		scanJob(t, st, "old-young", "running", nil, `heartbeatat = NULL, startedat = localtimestamp - interval '5 minutes'`)
		scanJob(t, st, "timeless", "running", nil, `heartbeatat = NULL, startedat = NULL`)
		scanJob(t, st, "ended", "done", str("host-a/0001"), `heartbeatat = now() - interval '2 hours', finishedat = localtimestamp`)

		n, err := st.FailSilentScanJobs(ctx, 15*time.Minute, "timed out: a test")
		if err != nil || n != 3 {
			t.Fatalf("%q: FailSilentScanJobs: %d, %v; want 3", tz, n, err)
		}
		for id, want := range map[string]string{
			"silent":     "failed timed out: a test ended=true",
			"old-silent": "failed timed out: a test ended=true",
			"timeless":   "failed timed out: a test ended=true",
			"speaking":   "running - ended=false",
			"old-young":  "running - ended=false",
			"ended":      "done - ended=true",
		} {
			if got := jobState(t, st, id); got != want {
				t.Errorf("%q: %s: %s, want %s", tz, id, got, want)
			}
		}
		if n, err := st.FailSilentScanJobs(ctx, time.Hour, "timed out"); err != nil || n != 0 {
			t.Errorf("%q: a longer timeout fails the speaking: %d, %v", tz, n, err)
		}
	}

	base := storetest.OpenBase(t)
	storetest.Exec(t, base, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat)
		VALUES ('j', 'nfs', 'running', localtimestamp - interval '1 day')`)
	if n, err := base.FailSilentScanJobs(context.Background(), time.Minute, "timed out"); err != nil || n != 0 {
		t.Errorf("without 034: %d, %v; want nothing failed", n, err)
	}
}
