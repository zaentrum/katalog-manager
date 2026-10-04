package scanner

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// This process names itself by its host and a tag drawn when it started, the
// same for every scan it runs.
func TestRunnerIsTheHostAndATagOfThisProcess(t *testing.T) {
	host, _ := os.Hostname()
	if !regexp.MustCompile(`^[^/]+/[0-9a-f]{8}$`).MatchString(Runner()) || !strings.HasPrefix(Runner(), host+"/") {
		t.Errorf("Runner() = %q, want %q and a tag of 8 hex digits", Runner(), host+"/")
	}
	if Runner() != Runner() || New(nil, config.Config{}, nil, nil).runner != Runner() {
		t.Error("the runner changes within a process")
	}
}

// A walk gives its job a word at most as often as the scan's timeout allows:
// every 30 seconds, or every third of a timeout shorter than 90 seconds.
func TestABeatComesWellWithinTheScansTimeout(t *testing.T) {
	for timeout, want := range map[time.Duration]time.Duration{
		15 * time.Minute: 30 * time.Second, 90 * time.Second: 30 * time.Second, time.Minute: 20 * time.Second,
	} {
		if got := beatInterval(timeout); got != want {
			t.Errorf("beatInterval(%v) = %v, want %v", timeout, got, want)
		}
	}
	if got := New(nil, config.Config{StepTimeouts: map[string]time.Duration{"scan": 30 * time.Second}}, nil, nil).beat; got != 10*time.Second {
		t.Errorf("a scan timeout of 30s beats every %v, want 10s", got)
	}
}

// library is a media root with a film in a folder, a film's subtitles and a
// file the scanner skips: six entries a walk visits, the root among them.
func library(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{"Example (2024)/Example (2024).mkv", "Example (2024)/Example (2024).en.srt", "notes.txt", "Other (2023).mp4"} {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A walk calls its beat at every entry it visits, files it skips among them.
func TestAWalkBeatsAtEveryEntry(t *testing.T) {
	st := storetest.Open(t)
	s := New(st, config.Config{NFSRoot: library(t)}, processing.New(st.Pool()), nil)
	beats := 0
	if _, err := s.walk(context.Background(), func() { beats++ }); err != nil {
		t.Fatal(err)
	}
	if beats != 6 {
		t.Errorf("%d beats, want one for each of the 6 entries", beats)
	}
}

// A scan names this process as its runner and gives its job a word while it
// walks, as often as its beat says, and its end stands; a scan whose beat is
// longer than the walk says nothing until it ends.
func TestAScanSaysItIsAliveWhileItWalks(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	for _, tc := range []struct {
		beat  time.Duration
		spoke bool
	}{{0, true}, {time.Hour, false}} {
		s := New(st, config.Config{NFSRoot: library(t)}, processing.New(st.Pool()), nil)
		s.beat = tc.beat
		job, err := st.StartScanJob(ctx, "nfs", s.runner)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_scanjobs SET heartbeatat = now() - interval '1 hour' WHERE id = $1`, job.ID)
		s.runScan(ctx, job.ID)
		spoke := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1
			AND heartbeatat > now() - interval '1 minute'`, job.ID) == 1
		if spoke != tc.spoke {
			t.Errorf("a beat every %v: the job spoke while the scan walked: %v, want %v", tc.beat, spoke, tc.spoke)
		}
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1
			AND status = 'done' AND finishedat IS NOT NULL AND runner = $2`, job.ID, Runner()); n != 1 {
			t.Errorf("a beat every %v: the scan did not end done, run by this process", tc.beat)
		}
	}
}

// Trigger starts a scan job this process runs; the scan ends it.
func TestTriggerStartsAJobThisProcessRuns(t *testing.T) {
	st := storetest.Open(t)
	s := New(st, config.Config{NFSRoot: library(t)}, processing.New(st.Pool()), nil)
	id, err := s.Trigger(context.Background(), "nfs")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1 AND status = 'running'`, id) == 1 {
		if time.Now().After(deadline) {
			t.Fatal("the scan did not end")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id = $1
		AND status = 'done' AND runner = $2 AND heartbeatat IS NOT NULL AND filesseen = 2`, id, Runner()); n != 1 {
		t.Error("the scan job is not this process's, done, with the two films seen")
	}
}

// At startup the scan jobs a previous process of this host left running, and
// those that name no runner, are failed, interrupted; this process's and
// another host's run on.
func TestAtStartupThePreviousProcesssScansAreFailed(t *testing.T) {
	st := storetest.Open(t)
	host := Runner()[:strings.LastIndexByte(Runner(), '/')]
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_scanjobs (id, source, status, startedat, runner) VALUES
		('before', 'nfs', 'running', localtimestamp, $1), ('unnamed', 'nfs', 'running', localtimestamp, NULL),
		('mine', 'nfs', 'running', localtimestamp, $2), ('there', 'nfs', 'running', localtimestamp, 'elsewhere/00000000')`,
		host+"/00000000", Runner())
	s := New(st, config.Config{}, processing.New(st.Pool()), nil)
	n, err := s.FailInterrupted(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("FailInterrupted: %d, %v; want 2", n, err)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id IN ('before', 'unnamed')
		AND status = 'failed' AND errormessage = $1 AND finishedat IS NOT NULL`, InterruptedReason); got != 2 {
		t.Errorf("%d of the previous process's scans are failed, interrupted; want both", got)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_scanjobs WHERE id IN ('mine', 'there') AND status = 'running'`); got != 2 {
		t.Errorf("%d of this process's and another host's scans run on, want both", got)
	}
}
