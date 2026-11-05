package library

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// aVersionOf writes a version whose package's files are files into a folder
// of its own, which it answers.
func aVersionOf(t testing.TB, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "versions", "9a2e0000-0000-4000-8000-000000000002")
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: "9a2e0000-0000-4000-8000-000000000002",
		PackageID: "4f1d0000-0000-4000-8000-000000000005", SourceIDs: []string{"0b6c0000-0000-4000-8000-000000000003"},
		CreatedAt: "2026-10-06T09:00:00Z", Files: files})
	return dir
}

// segments are n segments of a picture, by their paths in a version's
// folder, each as long as the others.
func segments(n int) map[string]string {
	files := map[string]string{}
	for i := range n {
		files[segment(i)] = fmt.Sprintf("a segment of the picture, number %04d", i)
	}
	return files
}

func segment(i int) string { return fmt.Sprintf("hls/v0/seg%04d.m4s", i) }

// onHash has hashFile call f with the path of each file before it hashes
// it, until the test ends: an error f answers is the read's, as a share that
// fails the read answers it.
func onHash(t testing.TB, f func(ctx context.Context, path string) error) {
	t.Helper()
	was := hashFile
	t.Cleanup(func() { hashFile = was })
	hashFile = func(ctx context.Context, path string) (string, int64, error) {
		if err := f(ctx, path); err != nil {
			return "", 0, err
		}
		return was(ctx, path)
	}
}

// lateness is how late a slow share answers the read of the file at path,
// from 10 to 20 ms (15 on average), the same each time: a round trip and a
// seek on slow disks.
func lateness(path string) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(filepath.Base(path)))
	return 10*time.Millisecond + time.Duration(h.Sum32()%11)*time.Millisecond
}

// checkersLeft are the stacks of the goroutines of checkFiles still there,
// once those returning had a moment to.
func checkersLeft() string {
	var left []string
	for wait := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		left = left[:0]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, "library.checkFiles") {
				left = append(left, g)
			}
		}
		if len(left) == 0 || time.Now().After(wait) {
			return strings.Join(left, "\n\n")
		}
	}
}

// Files checked a few at once answer what checking them one after another
// answers, however long each takes: the error of the first of them that
// fails, whichever fails first, or their bytes summed. Every file up to the
// first that fails is checked, once, and none before it is given up; one
// after it may be.
func TestFilesCheckedAFewAtOnceAnswerAsOneAfterAnother(t *testing.T) {
	rnd := rand.New(rand.NewPCG(5, 11))
	for round := range 150 {
		n := rnd.IntN(30)
		fails := make([]error, n)
		wait := make([]time.Duration, n)
		first, sum := n, int64(0)
		for i := range n {
			if rnd.IntN(6) == 0 {
				fails[i] = fmt.Errorf("file %d does not hold", i)
				first = min(first, i)
			}
			wait[i] = time.Duration(rnd.IntN(600)) * time.Microsecond
			sum += int64(i*1000 + 1)
		}
		var want error
		if first < n {
			want, sum = fails[first], 0
		}
		for _, reads := range []int{1, 2, verifyReads, 32} {
			var mu sync.Mutex
			checked := make([]int, n)
			got, err := checkFiles(context.Background(), reads, n, func(ctx context.Context, i int) (int64, error) {
				mu.Lock()
				checked[i]++
				mu.Unlock()
				select {
				case <-time.After(wait[i]):
				case <-ctx.Done():
					if i < first {
						t.Errorf("round %d, %d at once: file %d, before the first that fails (%d), was given up",
							round, reads, i, first)
					}
					return 0, ctx.Err()
				}
				if fails[i] != nil {
					return 0, fails[i]
				}
				return int64(i*1000 + 1), nil
			})
			if err != want || got != sum {
				t.Errorf("round %d, %d at once: %d, %v; want %d, %v", round, reads, got, err, sum, want)
			}
			for i := range n {
				if checked[i] > 1 || i <= first && checked[i] != 1 {
					t.Errorf("round %d, %d at once: file %d checked %d times (the first that fails: %d)", round, reads, i,
						checked[i], first)
				}
			}
		}
	}
}

// Once a file fails, no file after it is read any more: the one being read
// is given up, and no other is started.
func TestAFileThatFailsStopsTheFilesAfterIt(t *testing.T) {
	failed := errors.New("it does not hold")
	var mu sync.Mutex
	var checked []int
	began := time.Now()
	_, err := checkFiles(context.Background(), 2, 100, func(ctx context.Context, i int) (int64, error) {
		mu.Lock()
		checked = append(checked, i)
		mu.Unlock()
		if i == 0 {
			time.Sleep(50 * time.Millisecond)
			return 0, failed
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(5 * time.Second):
			return 1, nil
		}
	})
	if took := time.Since(began); err != failed || took > 2*time.Second || len(checked) != 2 {
		t.Errorf("%v after %s, the files %v checked; want the first's error, the second given up, no other checked",
			err, took, checked)
	}
}

// Of several files that do not hold, a full verification names the first
// by name, as reading them one after another does, though the files after
// it failed first: it waits for the read of the first, which is slow.
func TestAFullVerificationNamesTheFirstFileThatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		fail map[int]error // the files whose read fails, and how; the others that do not hold are changed
		says string        // how the error says the first does not hold
	}{
		{"three changed", nil, "its hash is sha256:"},
		{"the first unread", map[int]error{5: errors.New("input/output error"), 20: errors.New("stale file handle")},
			"input/output error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := aVersionOf(t, segments(40))
			for _, i := range []int{5, 20, 31} {
				changed := fmt.Sprintf("a segment of the pictur3, number %04d", i) // as long as it was
				librarytest.Write(t, filepath.Join(dir, segment(i)), []byte(changed))
			}
			var mu sync.Mutex
			var answered []string // the files that do not hold, as their reads answered
			onHash(t, func(ctx context.Context, path string) error {
				rel, _ := filepath.Rel(dir, path)
				if rel == segment(5) {
					time.Sleep(200 * time.Millisecond)
				}
				for _, i := range []int{5, 20, 31} {
					if rel == segment(i) {
						mu.Lock()
						answered = append(answered, rel)
						mu.Unlock()
						return c.fail[i]
					}
				}
				return nil
			})
			_, one := verifyChain(ctx, dir, VersionFile, versionPackageDirs, true, 1)
			answered = nil
			_, err := verifyChain(ctx, dir, VersionFile, versionPackageDirs, true, verifyReads)
			var b *Broken
			if !errors.As(err, &b) || b.File != filepath.Join(dir, segment(5)) || !strings.HasPrefix(b.Reason, c.says) {
				t.Fatalf("%d at once: %v, want %s not holding", verifyReads, err, segment(5))
			}
			if one == nil || err.Error() != one.Error() {
				t.Errorf("%d at once: %v; one after another: %v", verifyReads, err, one)
			}
			if len(answered) == 0 || answered[0] == segment(5) {
				t.Errorf("the reads answered %v: no file after the first failed first", answered)
			}
		})
	}
}

// Of a version's attached subtitles whose bytes do not hold to their names,
// a full verification names the first by name, though the one after it
// failed first; and it names one before any file of the package that does
// not hold, as it checks them first.
func TestAFullVerificationNamesTheFirstAttachedSubtitleThatDoesNotHold(t *testing.T) {
	ctx := context.Background()
	dir := aVersionOf(t, segments(12))
	librarytest.Write(t, filepath.Join(dir, segment(3)), []byte("a segment of the pictur3, number 0003"))
	var names []string
	for _, lang := range []string{"de", "en", "fr"} {
		name := AttachedName(SHA256([]byte("other bytes, in "+lang)), lang, false, false)
		librarytest.Write(t, AttachedPath(dir, name), []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n"+lang+"\n"))
		names = append(names, name)
	}
	sort.Strings(names)
	var mu sync.Mutex
	var answered []string
	onHash(t, func(ctx context.Context, path string) error {
		if filepath.Base(path) == names[0] {
			time.Sleep(200 * time.Millisecond)
		}
		if strings.HasPrefix(filepath.Base(path), AttachedPrefix) {
			mu.Lock()
			answered = append(answered, filepath.Base(path))
			mu.Unlock()
		}
		return nil
	})
	_, one := verifyChain(ctx, dir, VersionFile, versionPackageDirs, true, 1)
	answered = nil
	_, err := verifyChain(ctx, dir, VersionFile, versionPackageDirs, true, verifyReads)
	var b *Broken
	if !errors.As(err, &b) || b.File != AttachedPath(dir, names[0]) ||
		!strings.HasPrefix(b.Reason, "an attached subtitle whose hash is") {
		t.Fatalf("%d at once: %v, want %s not holding", verifyReads, err, names[0])
	}
	if one == nil || err.Error() != one.Error() {
		t.Errorf("%d at once: %v; one after another: %v", verifyReads, err, one)
	}
	if len(answered) != 3 || answered[0] == names[0] {
		t.Errorf("the subtitles answered %v: none after the first failed first", answered)
	}
}

// A verification counts the bytes of each file it lists once, however many
// it reads at once and in whatever order they finish: its size is theirs,
// summed, in full and of the chain alone, and holds against package.json;
// an attached subtitle beside them counts for nothing.
func TestAVerificationCountsTheBytesOfEachFileOnce(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{"subs/0.vtt": ""}
	rnd := rand.New(rand.NewPCG(3, 7))
	for i, size := range []int{1, 0, 2, 4095, 1<<20 - 1, 1 << 20, 1<<20 + 1, 3<<20 + 5} {
		files[fmt.Sprintf("hls/v0/seg%d.m4s", i)] = strings.Repeat("p", size)
	}
	for i := range 60 {
		files[fmt.Sprintf("trickplay/sprite-%04d.jpg", i)] = strings.Repeat("s", rnd.IntN(70000))
	}
	dir := aVersionOf(t, files)
	sub := []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nAttached\n")
	librarytest.Write(t, AttachedPath(dir, AttachedName(SHA256(sub), "de", false, false)), sub)
	var want int64
	listed, err := ReadChecksums(filepath.Join(dir, SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	for name := range listed {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		want += fi.Size()
	}
	onHash(t, func(ctx context.Context, path string) error {
		time.Sleep(lateness(path) / 10) // 1 to 2 ms, so that the files finish out of their order
		return nil
	})
	for _, full := range []bool{true, false} {
		for _, reads := range []int{1, verifyReads} {
			for range 3 {
				c, err := verifyChain(ctx, dir, VersionFile, versionPackageDirs, full, reads)
				if err != nil || c.SizeBytes != want {
					t.Fatalf("full %v, %d at once: %+v, %v; want %d bytes", full, reads, c, err, want)
				}
			}
		}
	}
}

// A shutdown stops a full verification at the files it is reading: none of
// its reads starts more than one file after it, it answers within a read or
// two, its error naming a file it was at with ctx's error, as it does
// reading one after another, and no read of it goes on once it answered.
func TestAFullVerificationStopsAtTheFilesItIsAt(t *testing.T) {
	if left := checkersLeft(); left != "" {
		t.Fatalf("reads left before the test:\n%s", left)
	}
	dir := aVersionOf(t, segments(1000))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var started int
	var stopped time.Time
	read := map[string]bool{} // the files whose read started
	after := 0                // the files whose read started after the shutdown
	onHash(t, func(ctx context.Context, path string) error {
		mu.Lock()
		read[path] = true
		if started++; started == 40 {
			stopped = time.Now()
			cancel()
		} else if !stopped.IsZero() {
			after++
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	c, err := VerifyVersionContext(ctx, dir, true)
	took := time.Since(stopped)
	var b *Broken
	if c != nil || !errors.As(err, &b) || b.Reason != context.Canceled.Error() || !read[b.File] {
		t.Fatalf("stopped: %+v, %v; want ctx's error, of a file it was at", c, err)
	}
	if after >= verifyReads {
		t.Errorf("%d files were started after the shutdown, %d reading at once", after, verifyReads)
	}
	if took > time.Second { // reading the 960 files left would take some 5 seconds
		t.Errorf("it answered %s after the shutdown", took)
	}
	if left := checkersLeft(); left != "" {
		t.Errorf("reads left once it answered:\n%s", left)
	}
}

// BenchmarkAFullVerificationOfASlowShare verifies in full a version of 2,000
// small files on a share that answers each read 10 to 20 ms late, reading
// them one after another and verifyReads at once.
func BenchmarkAFullVerificationOfASlowShare(b *testing.B) {
	files := map[string]string{}
	for i := range 2000 {
		files[segment(i)] = strings.Repeat("s", 1024+i%3072)
	}
	dir := aVersionOf(b, files)
	onHash(b, func(ctx context.Context, path string) error {
		time.Sleep(lateness(path))
		return nil
	})
	for _, reads := range []int{1, verifyReads} {
		b.Run(fmt.Sprintf("reads=%d", reads), func(b *testing.B) {
			for range b.N {
				if _, err := verifyChain(context.Background(), dir, VersionFile, versionPackageDirs, true, reads); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.N*len(files))/b.Elapsed().Seconds(), "files/s")
		})
	}
}
