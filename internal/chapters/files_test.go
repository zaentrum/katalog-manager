package chapters

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slow sets the waits of a test's reads of the library's files short: a read
// waited for wait at most, a claim's records for records, a claim for budget.
// Once the test is over, every read it left behind is (freed).
func slow(t *testing.T, wait, records, budget time.Duration) {
	t.Helper()
	w, r, b := FileWait, recordsFor, claimBudget
	FileWait, recordsFor, claimBudget = wait, records, budget
	t.Cleanup(func() {
		defer func() { FileWait, recordsFor, claimBudget = w, r, b }()
		freed(t)
	})
}

// stuck makes path a file whose read does not answer while the test runs (a
// FIFO no one writes to): a storage that hangs on it.
func stuck(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// a writer that comes and goes ends the read left behind
		if f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
}

// hang makes a look at path not answer while the test runs, or until after
// (a storage that hangs on it, or answers each look that late).
func hang(t *testing.T, path string, after time.Duration) {
	t.Helper()
	release, stat := make(chan struct{}), statFile
	statFile = func(p string) (fs.FileInfo, error) {
		if path == "" || p == path {
			select {
			case <-release:
			case <-time.After(after):
			}
		}
		return stat(p)
	}
	t.Cleanup(func() {
		close(release)
		defer func() { statFile = stat }()
		freed(t)
	})
}

// freed waits for the reads of the library's files a test left behind: it
// takes every slot, which it can once each of them gave its slot back, and
// gives them back.
func freed(t *testing.T) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for i := 0; i < fileSlots; i++ {
		select {
		case slots <- struct{}{}:
		case <-timeout:
			t.Fatalf("%d reads of the library's files are not over", fileSlots-i)
		}
	}
	for i := 0; i < fileSlots; i++ {
		<-slots
	}
}

// A source record whose read does not answer is waited for FileWait at most:
// the claim answers in time, its title waits its backoff as no failure, and
// the claim reads no other record. The next claim reads the others while the
// read left behind keeps its slot; once every slot is kept by reads not
// answered, a claim starts none and answers at once, and its titles are as
// they were.
func TestARecordReadThatDoesNotAnswerIsLeftForLater(t *testing.T) {
	slow(t, 2*time.Second, 6*time.Second, 12*time.Second)
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	m1, s1 := uid('a', 1), uid('b', 1) // its record's probe does not answer
	l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	stuck(t, filepath.Join(l.recordDir(m1, s1), ProbeFile))
	m2, s2 := uid('a', 2), uid('b', 2)
	l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 1300, threeMarks()...)

	began := time.Now()
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if took := time.Since(began); err != nil || took > FileWait+5*time.Second || c.Settled != 0 || len(c.Items) != 0 ||
		!strings.Contains(c.Idle, "did not answer in time") || c.RetryAfterSeconds != 60 {
		t.Fatalf("the claim: %+v, %v, in %s", c, err, took)
	}
	if r := l.read(m1); r.State != StatePending || r.Failures != 0 || r.Transients != 1 ||
		!r.NextAttemptAt.Equal(now.Add(time.Hour)) || r.RecordSourceID != nil ||
		!strings.Contains(deref(r.RecordNote), "did not answer in time") || !needs(t, l.st, m1) {
		t.Fatalf("m1, its record not answered: %+v", r)
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), m2); r != nil {
		t.Fatalf("m2 was read in a claim that had a read not answered: %+v", r)
	}

	// the next claim reads m2's record (m1 waits its backoff)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || c.Settled != 1 || l.count(m2) != 3 {
		t.Fatalf("the next claim: %+v, %v, %d chapters", c, err, l.count(m2))
	}
	// a second record that does not answer keeps the other slot
	m3, s3 := uid('a', 3), uid('b', 3)
	l.title(m3, s3, opts{chapters: true})
	l.record(m3, s3, 1300, threeMarks()...)
	stuck(t, filepath.Join(l.recordDir(m3, s3), ProbeFile))
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || c.Settled != 0 || l.read(m3).Transients != 1 ||
		len(slots) != fileSlots {
		t.Fatalf("a second record not answered: %+v, %v, %d slots kept", c, err, len(slots))
	}
	// every slot kept: a claim starts no read, answers at once, and leaves its
	// titles as they were
	m4, s4 := uid('a', 4), uid('b', 4)
	l.title(m4, s4, opts{chapters: true})
	l.record(m4, s4, 1300, threeMarks()...)
	began = time.Now()
	c, err = s.Claim(ctx, "analyzer-0", 1)
	if took := time.Since(began); err != nil || took >= FileWait || c.Settled != 0 || len(c.Items) != 0 ||
		!strings.Contains(c.Idle, "has not answered the reads before this one yet") {
		t.Fatalf("a claim while every slot is kept: %+v, %v, in %s", c, err, took)
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), m4); r != nil {
		t.Errorf("m4, not read: %+v", r)
	}
	for _, id := range []string{m1, m3} {
		if r := l.read(id); r.Failures != 0 || r.State != StatePending {
			t.Errorf("%s: %+v", id[:8], r)
		}
	}
}

// A look for an original that does not answer is waited for FileWait at most:
// the claim answers in time and hands out nothing, the title waits its
// backoff as no failure, and no other is looked for in that claim; the next
// claim hands the next title out.
func TestALookThatDoesNotAnswerIsLeftForLater(t *testing.T) {
	slow(t, 300*time.Millisecond, 2*time.Second, 4*time.Second)
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingRecords, "0") // every title goes to the analyzer: its original is looked for
	m1, s1 := uid('a', 1), uid('b', 1)
	orig1 := l.title(m1, s1, opts{chapters: true})
	m2, s2 := uid('a', 2), uid('b', 2)
	l.title(m2, s2, opts{chapters: true})
	hang(t, orig1, time.Hour)

	began := time.Now()
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if took := time.Since(began); err != nil || took > FileWait+5*time.Second || len(c.Items) != 0 ||
		!strings.Contains(c.Idle, "did not answer in time") || c.RetryAfterSeconds != 60 {
		t.Fatalf("the claim: %+v, %v, in %s", c, err, took)
	}
	if r := l.read(m1); r.State != StateFailed || r.Failures != 0 || r.Transients != 1 || r.ClaimedAt != nil ||
		!r.NextAttemptAt.Equal(now.Add(time.Hour)) || !strings.Contains(deref(r.Reason), "looking for its original") {
		t.Fatalf("m1, its look not answered: %+v", r)
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), m2); r != nil {
		t.Fatalf("m2 was looked for in a claim that had a look not answered: %+v", r)
	}
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 || c.Items[0].ID != m2 {
		t.Fatalf("the next claim: %+v, %v", c, err)
	}
}

// A storage that answers every look within FileWait, but slowly, still has
// the claim answer within its budget: it starts no look whose wait would end
// past it, and the titles it did not look for are as they were.
func TestAClaimAnswersWithinItsBudget(t *testing.T) {
	slow(t, time.Second, time.Second, 2*time.Second)
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingRecords, "0")
	var ids []string
	for i := 1; i <= candidates; i++ { // their originals are not on disk
		id := uid('a', i)
		l.title(id, uid('b', i), opts{chapters: true, gone: true})
		ids = append(ids, id)
	}
	hang(t, "", 400*time.Millisecond) // every look answers after 400 ms

	began := time.Now()
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if took := time.Since(began); err != nil || took > claimBudget+2*time.Second || len(c.Items) != 0 ||
		c.Idle != "no title is handed out now: the claim's time is up" {
		t.Fatalf("the claim: %+v, %v, in %s", c, err, took)
	}
	looked := 0
	for _, id := range ids {
		r, err := ReadOf(ctx, l.st.Pool(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			continue
		}
		looked++
		if r.Failures != 0 || r.Transients != 1 {
			t.Errorf("%s: %+v", id[:8], r)
		}
	}
	// a look started only while its wait of a second ends within the claim's
	// two: at 0, 0.4 and 0.8 s
	if looked == 0 || looked > 3 {
		t.Errorf("%d of %d titles looked for in a claim of 2 s, each look 400 ms", looked, len(ids))
	}
}

// A read of the header that found none, of a title whose source record does
// not answer, is kept within FileWait as a none nothing confirms: no failure,
// though the compliance check's record says the original has chapters, and
// the record is read again.
func TestAnEmptyReadWhoseRecordDoesNotAnswerCountsNoFailure(t *testing.T) {
	slow(t, 300*time.Millisecond, 2*time.Second, 4*time.Second)
	l := newLib(t)
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	stuck(t, filepath.Join(l.recordDir(m1, s1), ProbeFile))
	began := time.Now()
	r := l.record_(m1, header(orig, []Chapter{}, 0), night)
	if took := time.Since(began); took > FileWait+5*time.Second || r.State != StateNone || r.Confirmed || r.Failures != 0 ||
		r.RecordSourceID != nil || !strings.Contains(deref(r.Reason), "did not answer in time") || !needs(t, l.st, m1) {
		t.Fatalf("an empty read whose record does not answer: %+v, in %s", r, took)
	}
}
