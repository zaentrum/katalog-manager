package library

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The guards of the retire path that hold whoever's word it retires an
// original on: each holds although every check before it held.

// claimed is the fixture's original claimed for its retirement on the
// policy's word, its event not recorded yet.
func (f *retireFixture) claimed(t *testing.T) *retirement {
	t.Helper()
	ctx := context.Background()
	rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir, auth: byPolicy}
	rt.ver, _ = Current(ctx, f.st.Pool(), rtFilm)
	if ok, err := f.r.claim(ctx, f.p, time.Now().UTC(), rt); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	return rt
}

// recorded is the fixture's original with its event recorded on the policy's
// word, its files not moved yet.
func (f *retireFixture) recorded(t *testing.T) *retirement {
	t.Helper()
	ctx := context.Background()
	set, _ := ReadSettings(ctx, f.st.Pool())
	rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir, auth: byPolicy}
	if err := f.r.prepare(ctx, f.p, set, time.Now().UTC(), rt); err != nil || rt.src == nil {
		t.Fatalf("prepare: %v", err)
	}
	return rt
}

// events are the events recorded of the fixture's movie.
func (f *retireFixture) events(t *testing.T) int {
	t.Helper()
	evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events"))
	return len(evs)
}

// The claim holds to what it claims on, whatever was checked before it: a
// title held, or a step that reads the original at work, is not claimed.
func TestAClaimHoldsToItsConditions(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(t *testing.T, f *retireFixture){
		"held": func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
		},
		"a step at work": func(t *testing.T, f *retireFixture) {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "subtitle", processing.StatusPending, nil, nil); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixture(t)
			setup(t, f)
			rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir, auth: byPolicy}
			rt.ver, _ = Current(ctx, f.st.Pool(), rtFilm)
			if ok, err := f.r.claim(ctx, f.p, time.Now().UTC(), rt); err != nil || ok {
				t.Errorf("claimed: %v, %v", ok, err)
			}
			f.kept(t)
			if got := f.step(t, rtFilm); got != "none" {
				t.Errorf("the retire step: %s", got)
			}
		})
	}
}

// A retirement on the policy's word that a restart interrupted before its
// event is recorded is called off when the title is held since, or a step
// that reads the original is at work again: the source is present again and
// the retire step waits, saying why.
func TestARetirementIsCalledOffWhenItsTitleIsHeldOrAtWork(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *retireFixture)
		want  string
	}{
		{"held", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
		}, "pending 0 error=- details=the title holds its originals (retirehold): the original is kept"},
		{"a step at work", func(t *testing.T, f *retireFixture) {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "transcode", processing.StatusInProgress, nil,
				nil); err != nil {
				t.Fatal(err)
			}
		}, "pending 0 error=- details=the title's pipeline reads its original again: the retirement waits for it"},
		// called off before anything is read again: the original that
		// changed since is no failure of a retirement called off
		{"held, its original changed since", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
			librarytest.Write(t, f.original, bytes.Repeat([]byte("Other Films! "), 40000))
		}, "pending 0 error=- details=the title holds its originals (retirehold): the original is kept"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.claimed(t)
			c.setup(t, f)
			if rep := f.pass(t); rep.Originals != 0 || rep.Failed != 0 {
				t.Errorf("the pass: %+v", rep)
			}
			f.kept(t)
			if got := f.step(t, rtFilm); got != c.want {
				t.Errorf("the retire step: %s\nwant %s", got, c.want)
			}
		})
	}
}

// An original of the size recorded and of other bytes is no original as
// recorded: its quick hash tells it, and it is kept.
func TestAnOriginalOfTheSizeRecordedAndOtherBytesIsKept(t *testing.T) {
	f := newRetireFixture(t)
	fi, err := os.Stat(f.original)
	if err != nil {
		t.Fatal(err)
	}
	other := bytes.Repeat([]byte("Other Films! "), 40000)
	if int64(len(other)) != fi.Size() {
		t.Fatalf("the other bytes are %d, the original %d", len(other), fi.Size())
	}
	librarytest.Write(t, f.original, other)
	if _, err := f.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "the original changed since it "+
		"was recorded: "+f.original+" is "+strconv.FormatInt(fi.Size(), 10)+" bytes with quick hash ") {
		t.Errorf("the pass: %v", err)
	}
	f.kept(t)
	if got, _ := os.ReadFile(f.original); !bytes.Equal(got, other) {
		t.Error("the file was touched")
	}
}

// An original outside every root the retire path deletes under is refused
// before its event is recorded, and once it is (where the catalog says it
// lies changed since) its retirement stops before it deletes anything: the
// file stays, and the source is not noted deleted.
func TestAnOriginalOutsideTheRootsIsNotDeleted(t *testing.T) {
	ctx := context.Background()
	outside := func(t *testing.T, f *retireFixture) string {
		t.Helper()
		to := filepath.Join(t.TempDir(), "elsewhere", "Example Film.mkv")
		if err := os.MkdirAll(filepath.Dir(to), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.original, to); err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2 WHERE id = $1`, rtSource, to)
		return to
	}
	t.Run("before its event", func(t *testing.T) {
		f := newRetireFixture(t)
		to := outside(t, f)
		if _, err := f.r.Pass(ctx); err == nil || !strings.Contains(err.Error(), to+" lies outside the arrivals") {
			t.Errorf("the pass: %v", err)
		}
		if s := f.source(t, rtSource); s.State != SourcePresent || s.RetireEventID != nil || f.events(t) != 0 || !exists(to) {
			t.Errorf("the source: %s, event %v, events %d, the file there %v", s.State, s.RetireEventID, f.events(t), exists(to))
		}
	})
	t.Run("once its event is recorded", func(t *testing.T) {
		f := newRetireFixture(t)
		f.recorded(t)
		to := outside(t, f)
		if _, err := f.r.Pass(ctx); err == nil || !strings.Contains(err.Error(), to+" lies outside the arrivals") {
			t.Errorf("the pass: %v", err)
		}
		if s := f.source(t, rtSource); s.State != SourceRetiring || !exists(to) {
			t.Errorf("the source: %s, the file there %v", s.State, exists(to))
		}
	})
}

// An original that became a link once its event was recorded is not
// deleted: the link is no file of the library's own.
func TestAnOriginalThatBecameALinkIsNotDeleted(t *testing.T) {
	f := newRetireFixture(t)
	f.recorded(t)
	target := filepath.Join(t.TempDir(), "Example Film.mkv")
	b, _ := os.ReadFile(f.original)
	librarytest.Write(t, target, b)
	if err := os.Remove(f.original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, f.original); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "the original "+f.original+
		" is no file of its own (a link, a folder): it is not deleted") {
		t.Errorf("the pass: %v", err)
	}
	if fi, err := os.Lstat(f.original); err != nil || fi.Mode()&os.ModeSymlink == 0 || !exists(target) {
		t.Errorf("the link: %v, %v; the file it names there %v", fi, err, exists(target))
	}
	if s := f.source(t, rtSource); s.State != SourceRetiring {
		t.Errorf("the source: %s", s.State)
	}
}

// With library.originals keep the retire job claims nothing: no title's
// retire step is touched.
func TestTheRetireJobClaimsNothingWhileOriginalsAreKept(t *testing.T) {
	f := newRetireFixture(t)
	f.set(t, SettingOriginals, OriginalsKeep)
	if rep := f.pass(t); rep.Originals != 0 || rep.Failed != 0 {
		t.Errorf("the pass: %+v", rep)
	}
	f.kept(t)
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step: %s", got)
	}
}

// A day of the trash goes once library.trash.grace is over after the day's
// end: an original moved into it at its last minute has its whole grace.
func TestTheTrashKeepsADayForItsWholeGrace(t *testing.T) {
	f := newRetireFixture(t)
	set, err := ReadSettings(context.Background(), f.st.Pool())
	if err != nil || set.TrashGrace != 24*time.Hour {
		t.Fatalf("library.trash.grace: %v, %v", set.TrashGrace, err)
	}
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	librarytest.Write(t, filepath.Join(f.p.TrashDir(day.Add(23*time.Hour+59*time.Minute), "a"), "file"), []byte("x"))
	for _, c := range []struct {
		now  time.Time
		kept bool
	}{
		{day.Add(47*time.Hour + 59*time.Minute), true},
		{day.Add(48*time.Hour + time.Second), false},
	} {
		n, err := f.r.purge(f.p, set, c.now)
		if err != nil || exists(f.p.TrashDay(day)) != c.kept || (n == 0) != c.kept {
			t.Errorf("at %s: purged %d (%v), the day there %v, want %v", c.now.Format(time.RFC3339), n, err,
				exists(f.p.TrashDay(day)), c.kept)
		}
	}
}
