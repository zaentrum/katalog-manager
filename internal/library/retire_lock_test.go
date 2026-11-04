package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// holdRetireLock holds one of the retire job's locks, key, on a connection of
// its own, as an instance of the release before (retireLockLegacy) or of this
// one runs a pass, until the answer is called.
func (f *retireFixture) holdRetireLock(t *testing.T, key string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+key+`)`); err != nil {
		conn.Release()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+key+`)`)
			conn.Release()
		})
	}
	t.Cleanup(release)
	return release
}

// While a release rolls out, an instance of the one before runs beside this
// one, and holds the retire job's lock unscoped: a pass runs only once it
// holds both locks, that one and the catalog's own, so that a pass of either
// release never runs beside one of the other, nor beside an admin's list. A
// pass that finds either held does nothing, and lets go of what it took.
func TestAPassRunsOnlyWithBothLocksOfTheRetireJob(t *testing.T) {
	for _, key := range retireLocks {
		t.Run(key, func(t *testing.T) {
			f := newRetireFixture(t)
			f.r.lockWait = 100 * time.Millisecond
			release := f.holdRetireLock(t, key)
			if rep := f.pass(t); rep != (Report{}) {
				t.Errorf("a pass while %s is held: %+v", key, rep)
			}
			f.kept(t)
			release()
			// what the refused pass took is let go of: the next one runs
			if rep := f.pass(t); rep.Originals != 1 {
				t.Errorf("the pass once %s is let go of: %+v", key, rep)
			}
		})
	}
}

// The retire job's own lock is a catalog's, scoped by its schema: held for
// another catalog on the same database, it holds back none of this one's
// passes. (The unscoped one, of the release before, is every catalog's.)
func TestTheRetireJobsOwnLockIsTheCatalogs(t *testing.T) {
	f := newRetireFixture(t)
	other := &retireFixture{st: storetest.Open(t)}
	release := other.holdRetireLock(t, retireLockKey)
	defer release()
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("a pass while another catalog's own lock is held: %+v", rep)
	}
}

// An admin's list waits for both of the retire job's locks as a pass takes
// them, and holds both while it retires an original.
func TestAListWaitsForBothLocksOfTheRetireJob(t *testing.T) {
	for _, key := range retireLocks {
		t.Run(key, func(t *testing.T) {
			f := newRetireFixture(t)
			l := NewRetireList(f.st.Pool(), f.cfg, processing.New(f.st.Pool()), nil)
			l.lockWait = 10 * time.Millisecond
			release := f.holdRetireLock(t, key)
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			// a connection of its own: one whose query the end of ctx cut is closed
			waiting, err := f.st.Pool().Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if unlock, err := l.lockRetire(ctx, waiting); err != nil || unlock != nil {
				if unlock != nil {
					unlock()
				}
				waiting.Release()
				t.Fatalf("the list's lock while %s is held: %v, %v; want it to wait", key, unlock != nil, err)
			}
			waiting.Release()
			release()
			conn, err := f.st.Pool().Acquire(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			held, err := l.lockRetire(context.Background(), conn)
			if err != nil || held == nil {
				t.Fatalf("the list's lock once %s is let go of: %v", key, err)
			}
			var once sync.Once
			unlock := func() { once.Do(held) }
			defer unlock()
			// it holds both: a pass does nothing meanwhile
			f.r.lockWait = 100 * time.Millisecond
			if rep := f.pass(t); rep != (Report{}) {
				t.Errorf("a pass while a list holds the locks: %+v", rep)
			}
			unlock()
			if rep := f.pass(t); rep.Originals != 1 {
				t.Errorf("the pass once the list let go: %+v", rep)
			}
		})
	}
}

// A retirement goes on only by the claim it holds: one whose source was
// called off meanwhile (present again), or claimed again by another (another
// event), records no event and moves no file, and leaves the row as whoever
// changed it left it — however its own copy of the source reads.
func TestAStaleRetirementRecordsAndMovesNothing(t *testing.T) {
	for _, c := range []struct {
		name, change string
		state        string
	}{
		{"called off", `UPDATE com_nalet_katalog_itemsources SET state = 'present', retireeventid = NULL, retireeventat = NULL,
			trashpath = NULL WHERE id = $1`, SourcePresent},
		{"claimed again", `UPDATE com_nalet_katalog_itemsources SET retireeventid = gen_random_uuid()::varchar
			WHERE id = $1`, SourceRetiring},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			ctx := context.Background()
			set, err := ReadSettings(ctx, f.st.Pool())
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			rt := &retirement{src: f.source(t, rtSource), auth: byPolicy, itemDir: f.itemDir}
			if rt.ver, err = Current(ctx, f.st.Pool(), rtFilm); err != nil || rt.ver == nil {
				t.Fatalf("the version: %v", err)
			}
			if claimed, err := f.r.claim(ctx, f.p, now, rt); err != nil || !claimed {
				t.Fatalf("the claim: %v, %v", claimed, err)
			}
			stale := *rt.src
			f.execSQL(t, c.change, rtSource)

			deleted, err := f.r.retireOf(ctx, f.p, set, now, &retirement{src: &stale, auth: byPolicy})
			var refused *retireFailed
			if deleted || !errors.As(err, &refused) {
				t.Fatalf("a stale retirement: deleted %v, %v; want it refused", deleted, err)
			}
			for _, path := range []string{f.original, f.en, f.de, f.nfo} {
				if !exists(path) {
					t.Errorf("%s was moved", path)
				}
			}
			if evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events")); len(evs) > 0 {
				t.Errorf("events recorded: %v", evs)
			}
			if got := files(t, filepath.Join(f.p.Work, WorkTrash)); len(got) > 0 {
				t.Errorf("the trash holds %v", got)
			}
			if s := f.source(t, rtSource); s.State != c.state {
				t.Errorf("the source is %s; want %s, as it was changed", s.State, c.state)
			}
		})
	}
}

// tx B of a retirement notes its original deleted only while its source is
// retiring: one that is not any more fails it, saying so, and changes
// nothing.
func TestARetirementIsNotNotedDoneOnceItsSourceIsNoLongerRetiring(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	s := f.source(t, rtSource)
	stale := *s
	stale.State = SourceRetiring
	err := f.r.deleted(ctx, time.Now().UTC(), &retirement{src: &stale, auth: byPolicy, itemDir: f.itemDir})
	if err == nil {
		t.Fatal("tx B of a source that is present: nil; want an error")
	}
	if got := f.source(t, rtSource); got.State != SourcePresent || got.DeletedAt != nil {
		t.Errorf("the source after: %s, deleted at %v; want present", got.State, got.DeletedAt)
	}
	if step := f.step(t, rtFilm); step != "none" {
		t.Errorf("the retire step: %s; want none", step)
	}
}

// execSQL runs sql with args on the fixture's catalog.
func (f *retireFixture) execSQL(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.st.Pool().Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
