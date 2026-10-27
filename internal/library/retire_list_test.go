package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// gateOf is a compliance check that passes every title but those it names,
// saying why not.
type gateOf map[string]string

func (g gateOf) Passed(_ context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		out[id] = g[id]
	}
	return out, nil
}

// listFixture is the retire job's fixture as the installation keeps its
// library, library.originals keep, its movie's version verified once, with a
// list runner whose compliance check is gate and whose pace is noted, not
// waited for (slept).
type listFixture struct {
	*retireFixture
	l     *RetireList
	gate  gateOf
	slept []time.Duration
}

// The other movie of the list tests, which no list names unless a test does.
const (
	ltFilm    = "6f1b2c3d-4e5f-4a7b-8c9d-0e1f2a3b4c5d"
	ltSource  = "1c7d8e9f-0a1b-4c2d-8e3f-4a5b6c7d8e9f"
	ltVersion = "8b3f4a5b-6c7d-4e8f-9a0b-1c2d3e4f5a6b"
	ltPackage = "3e2f1a0b-9c8d-4e7f-8a6b-5c4d3e2f1a0b"
)

func newListFixture(t *testing.T) *listFixture {
	t.Helper()
	return listFixtureOf(t, newRetireFixture(t))
}

func listFixtureOf(t *testing.T, f *retireFixture) *listFixture {
	t.Helper()
	f.set(t, SettingOriginals, OriginalsKeep)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now() - interval '2 days', verifiedlevel = 'full'`)
	lf := &listFixture{retireFixture: f, gate: gateOf{}}
	lf.l = NewRetireList(f.st.Pool(), f.cfg, processing.New(f.st.Pool()), lf.gate)
	lf.l.sleep = func(ctx context.Context, d time.Duration) bool {
		lf.slept = append(lf.slept, d)
		return ctx.Err() == nil
	}
	return lf
}

// run runs a list of ids on an admin's word, a dry run or not, to its end,
// and answers the run and its titles.
func (f *listFixture) run(t *testing.T, dry bool, ids ...string) (ListRun, []ListItem) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.l.Start(ctx, ids, dry, "an-admin"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.l.Work(ctx); err != nil {
		t.Fatalf("Work: %v", err)
	}
	return f.latest(t)
}

// latest is the run begun last and its titles.
func (f *listFixture) latest(t *testing.T) (ListRun, []ListItem) {
	t.Helper()
	ctx := context.Background()
	run, err := f.l.Latest(ctx)
	if err != nil || run == nil {
		t.Fatalf("Latest: %v, %v", run, err)
	}
	items, err := f.l.Items(ctx, run.ID, 500, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return *run, items
}

// said is a title's result and why, as a test compares them.
func said(it ListItem) string {
	return deref(it.Result) + ": " + deref(it.Reason)
}

// eventOfSource is the event.json of the original-deleted event of the
// source s.
func (f *listFixture) eventOfSource(t *testing.T, s *Source) string {
	t.Helper()
	dir, err := FindEvent(f.itemDir, deref(s.RetireEventID), EventOriginalDeleted)
	if err != nil || dir == "" {
		t.Fatalf("the event of %s: %q, %v", s.ID, dir, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, EventFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A dry run says what it would retire and touches nothing; the run retires
// exactly the originals of the titles it names, through the retire job's own
// path: the event recorded on the admin's word with what the package does not
// carry of the original, the original and its sidecars moved into the trash,
// the source deleted, the retire step done. library.originals stays keep,
// and the retire job retires nothing else.
func TestAListRetiresTheOriginalsOfTheTitlesItNames(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	f.addMovie(t, ltFilm, ltSource, ltVersion, ltPackage, "Another Film")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = 'full'`)
	size := f.source(t, rtSource).SizeBytes

	run, items := f.run(t, true, rtFilm)
	if run.State != RunDone || !run.DryRun || run.RequestedBy != "an-admin" || run.Total != 1 || run.WouldRetire != 1 ||
		run.Pending != 0 || run.Retired != 0 || run.Bytes != size || run.FinishedAt == nil {
		t.Fatalf("the dry run: %+v", run)
	}
	if len(items) != 1 || said(items[0]) != "would-retire: " || deref(items[0].SourceID) != rtSource ||
		items[0].Bytes == nil || *items[0].Bytes != size || deref(items[0].Title) != "Example Film" || items[0].At == nil {
		t.Fatalf("the dry run's title: %+v", items)
	}
	f.kept(t)
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step after a dry run: %s", got)
	}

	run, items = f.run(t, false, rtFilm)
	if run.State != RunDone || run.DryRun || run.Retired != 1 || run.Bytes != size || said(items[0]) != "retired: " {
		t.Fatalf("the run: %+v, %s", run, said(items[0]))
	}
	s := f.source(t, rtSource)
	if s.State != SourceDeleted || deref(s.DeletedBy) != RetireListedBy("an-admin") || s.ArrivalPath != nil ||
		string(s.Lost) != `["maxAudioChannels"]` {
		t.Fatalf("the source: %+v", s)
	}
	if ev := f.eventOfSource(t, s); !strings.Contains(ev, `"by": "katalog-manager (retireOriginals, by an-admin)"`) ||
		!strings.Contains(ev, `"reason": "`+RetireListedReason+`"`) || !strings.Contains(ev, `"accepted": [
    "maxAudioChannels"
  ]`) {
		t.Errorf("the event:\n%s", ev)
	}
	want := []string{"Example Film.de.srt", "Example Film.en.srt", "Example Film.mkv", "Example Film.nfo"}
	if got := files(t, deref(s.TrashPath)); strings.Join(got, "|") != strings.Join(want, "|") ||
		!strings.HasPrefix(deref(s.TrashPath), filepath.Join(f.p.Work, WorkTrash)) {
		t.Errorf("the trash %s holds %v, want %v", deref(s.TrashPath), got, want)
	}
	if got := f.step(t, rtFilm); got != "done 0 error=- details=lost: maxAudioChannels" {
		t.Errorf("the retire step: %s", got)
	}
	set, err := ReadSettings(ctx, f.st.Pool())
	if err != nil || set.Originals != OriginalsKeep || set.DeleteOriginals() {
		t.Errorf("library.originals after the run: %q, %v", set.Originals, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key = 'library.originals'
		AND valuetext = 'keep'`); n != 1 {
		t.Errorf("library.originals rows: %d", n)
	}
	if rep := f.pass(t); rep.Originals != 0 || rep.Failed != 0 {
		t.Errorf("the retire job's pass after the run: %+v", rep)
	}
	if o := f.source(t, ltSource); o.State != SourcePresent || !exists(deref(o.ArrivalPath)) {
		t.Errorf("the original no list named: %s", o.State)
	}
	// listed again: retired already, so skipped
	if _, items = f.run(t, false, rtFilm); said(items[0]) != "skipped: its original was retired already" {
		t.Errorf("listed again: %s", said(items[0]))
	}
}

// A title is skipped, saying why, while any condition does not hold, and
// nothing of it is touched, whether in a dry run or not.
func TestAListChecksEveryCondition(t *testing.T) {
	ctx := context.Background()
	const series = "2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a"
	for _, c := range []struct {
		name  string
		fx    func(t *testing.T) *listFixture
		setup func(t *testing.T, f *listFixture) string // the title listed
		want  string
		kept  bool
	}{
		{"not in the catalog", nil, func(t *testing.T, f *listFixture) string { return "0d0d0d0d-0000-4000-8000-000000000001" },
			"skipped: it is not in the catalog", true},
		{"a series", nil, func(t *testing.T, f *listFixture) string {
			storetest.AddItem(t, f.st, series, "series", "A Coastal Show", "")
			return series
		}, "skipped: a series has no original of its own: a list names movies and episodes", true},
		{"held", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
			return rtFilm
		}, "skipped: its originals are held (holdOriginal)", true},
		{"no complete version", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'building' WHERE id = $1`, rtVersion)
			return rtFilm
		}, "skipped: it has no complete version", true},
		{"never verified", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = NULL, verifiedlevel = NULL`)
			return rtFilm
		}, "skipped: its complete version " + rtVersion + " was never verified", true},
		{"a step at work", nil, func(t *testing.T, f *listFixture) string {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "transcode", processing.StatusInProgress, nil,
				nil); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its transcode step runs", true},
		{"a step to be tried again", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed',
				nextretryat = now() + interval '1 hour' WHERE item_id = $1 AND step = 'package'`, rtFilm)
			return rtFilm
		}, "skipped: its package step failed and is to be tried again", true},
		{"in the re-encode queue", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby)
				VALUES ('q1', $1, 'items', 'an-admin')`, rtFilm)
			return rtFilm
		}, "skipped: it is in the re-encode queue (queued)", true},
		{"the compliance check", nil, func(t *testing.T, f *listFixture) string {
			f.gate[rtFilm] = "the compliance check finds it breaks OVER_BITRATE_CAP"
			return rtFilm
		}, "skipped: the compliance check finds it breaks OVER_BITRATE_CAP", true},
		{"its package lacks its surround", func(t *testing.T) *listFixture {
			return listFixtureOf(t, newRetireFixtureOf(t, fiveOne, stereo))
		}, func(t *testing.T, f *listFixture) string { return rtFilm }, "skipped: " + SurroundHeld, true},
		{"its original moved", nil, func(t *testing.T, f *listFixture) string {
			if err := os.Rename(f.original, f.original+".moved"); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its original isn’t where the catalog says: ", false},
		{"its original of another size", nil, func(t *testing.T, f *listFixture) string {
			librarytest.Write(t, f.original, []byte("not the original"))
			return rtFilm
		}, "skipped: its original is 16 bytes, and the catalog recorded ", false},
	} {
		for _, dry := range []bool{true, false} {
			name := c.name
			if dry {
				name += ", a dry run"
			}
			t.Run(name, func(t *testing.T) {
				var f *listFixture
				if c.fx != nil {
					f = c.fx(t)
				} else {
					f = newListFixture(t)
				}
				id := c.setup(t, f)
				run, items := f.run(t, dry, id)
				if run.State != RunDone || run.Skipped != 1 || len(items) != 1 || !strings.HasPrefix(said(items[0]), c.want) {
					t.Fatalf("%+v: %s, want %s", run, said(items[0]), c.want)
				}
				if c.kept {
					f.kept(t)
					if got := f.step(t, rtFilm); got != "none" {
						t.Errorf("the retire step: %s", got)
					}
				}
			})
		}
	}
}

// One list runs at a time, in one instance at a time: another is refused
// while it runs, and an instance the list's lock is taken from does nothing.
func TestOneListRunsAtATime(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	if _, err := f.l.Start(ctx, []string{rtFilm}, true, "an-admin"); err != nil {
		t.Fatal(err)
	}
	var refused *ListRefused
	if _, err := f.l.Start(ctx, []string{rtFilm}, false, "an-admin"); !errors.As(err, &refused) || refused.Code != "RUNNING" ||
		!strings.HasPrefix(refused.Msg, "a dry run of a list of 1 title runs since ") ||
		!strings.HasSuffix(refused.Msg, ", 1 of them still to come: wait for it to finish") {
		t.Fatalf("a second list while one runs: %v", err)
	}
	// another instance has the list
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+listLock+`)`); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.latest(t); run.State != RunRunning || run.Pending != 1 {
		t.Errorf("worked while another instance has the list: %+v", run)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+listLock+`)`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if err := f.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if run, _ := f.latest(t); run.State != RunDone || run.WouldRetire != 1 {
		t.Errorf("the list once its lock is free: %+v", run)
	}
	if _, err := f.l.Start(ctx, []string{rtFilm}, false, "an-admin"); err != nil {
		t.Errorf("a list once the one before is done: %v", err)
	}
}

// What a list is refused: no title, more than MaxList, the legacy layout.
func TestAListIsRefused(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	many := make([]string, MaxList+1)
	for i := range many {
		many[i] = NewID()
	}
	for _, c := range []struct {
		ids  []string
		code string
	}{{nil, "EMPTY"}, {[]string{" ", ""}, "EMPTY"}, {many, "TOO_MANY"}} {
		var refused *ListRefused
		if _, err := f.l.Start(ctx, c.ids, true, "an-admin"); !errors.As(err, &refused) || refused.Code != c.code {
			t.Errorf("%d titles: %v, want %s", len(c.ids), err, c.code)
		}
	}
	f.set(t, SettingLayout, LayoutLegacy)
	var refused *ListRefused
	if _, err := f.l.Start(ctx, []string{rtFilm}, true, "an-admin"); !errors.As(err, &refused) || refused.Code != "LEGACY" {
		t.Errorf("the legacy layout: %v", err)
	}
	if run, _ := f.l.Latest(ctx); run != nil {
		t.Errorf("a list refused ran: %+v", run)
	}
}

// A list a restart interrupted goes on where it stopped: a retirement it
// claimed is resumed on its word, one the retire job called off meanwhile
// (library.originals keep) is checked anew and retired, and one whose event
// it recorded, which the job finished meanwhile, is retired, on its word.
func TestAListResumesWhereARestartStoppedIt(t *testing.T) {
	ctx := context.Background()
	// begin is a list interrupted once it claimed its title's original, its
	// event recorded too when recorded.
	begin := func(t *testing.T, f *listFixture, recorded bool) {
		t.Helper()
		run, err := f.l.Start(ctx, []string{rtFilm}, false, "an-admin")
		if err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_retirerunitems SET phase = 'retiring', source_id = $2 WHERE run_id = $1`,
			run.ID, rtSource)
		rt := &retirement{src: f.source(t, rtSource), itemDir: f.itemDir,
			auth: Authority{By: RetireListedBy("an-admin"), Reason: RetireListedReason}}
		set, _ := ReadSettings(ctx, f.st.Pool())
		now := time.Now().UTC()
		if !recorded {
			rt.ver, _ = Current(ctx, f.st.Pool(), rtFilm)
			if ok, err := f.r.claim(ctx, f.p, now, rt); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			return
		}
		if err := f.r.prepare(ctx, f.p, set, now, rt); err != nil || rt.src == nil {
			t.Fatalf("prepare: %v", err)
		}
	}
	// restarted is the list after a restart: a runner of its own.
	restarted := func(t *testing.T, f *listFixture) (ListRun, []ListItem) {
		t.Helper()
		f.l = NewRetireList(f.st.Pool(), f.cfg, processing.New(f.st.Pool()), f.gate)
		if err := f.l.Work(ctx); err != nil {
			t.Fatal(err)
		}
		return f.latest(t)
	}
	retired := func(t *testing.T, f *listFixture, run ListRun, items []ListItem) {
		t.Helper()
		if run.State != RunDone || run.Retired != 1 || said(items[0]) != "retired: " {
			t.Fatalf("the list: %+v, %s", run, said(items[0]))
		}
		s := f.source(t, rtSource)
		if s.State != SourceDeleted || deref(s.DeletedBy) != RetireListedBy("an-admin") {
			t.Errorf("the source: %s by %s", s.State, deref(s.DeletedBy))
		}
		if ev := f.eventOfSource(t, s); !strings.Contains(ev, `"by": "katalog-manager (retireOriginals, by an-admin)"`) {
			t.Errorf("the event:\n%s", ev)
		}
		if evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events")); len(evs) != 1 {
			t.Errorf("events: %v", evs)
		}
		if got := files(t, deref(s.TrashPath)); len(got) != 4 {
			t.Errorf("the trash: %v", got)
		}
	}

	t.Run("claimed", func(t *testing.T) {
		f := newListFixture(t)
		begin(t, f, false)
		run, items := restarted(t, f)
		retired(t, f, run, items)
	})
	t.Run("claimed, called off by the retire job", func(t *testing.T) {
		f := newListFixture(t)
		begin(t, f, false)
		if rep := f.pass(t); rep.Originals != 0 {
			t.Fatalf("the job's pass: %+v", rep)
		}
		if s := f.source(t, rtSource); s.State != SourcePresent {
			t.Fatalf("the source the job called off: %s", s.State)
		}
		run, items := restarted(t, f)
		retired(t, f, run, items)
	})
	t.Run("recorded, finished by the retire job", func(t *testing.T) {
		f := newListFixture(t)
		begin(t, f, true)
		if rep := f.pass(t); rep.Originals != 1 {
			t.Fatalf("the job's pass: %+v", rep)
		}
		run, items := restarted(t, f)
		retired(t, f, run, items)
	})
}

// The library's name of an original is all a list retires: a file with a
// second name, a hard link another tree shares, is retired by the library's
// name, moved into the trash, and its other name stays as it is, the one file
// with the name in the trash until the trash's grace is over.
func TestAListRetiresTheLibrarysNameOfAFileWithTwo(t *testing.T) {
	f := newListFixture(t)
	other := filepath.Join(f.cfg.NFSRoot, "an export", "Example Film.mkv")
	if err := os.MkdirAll(filepath.Dir(other), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f.original, other); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(other)
	if _, items := f.run(t, true, rtFilm); said(items[0]) != "would-retire: " {
		t.Fatalf("the dry run of a file of two names: %s", said(items[0]))
	}
	_, items := f.run(t, false, rtFilm)
	if said(items[0]) != "retired: " {
		t.Fatalf("a file of two names: %s", said(items[0]))
	}
	s := f.source(t, rtSource)
	trashed := filepath.Join(deref(s.TrashPath), "Example Film.mkv")
	a, errA := os.Stat(trashed)
	b, errB := os.Stat(other)
	if errA != nil || errB != nil || !os.SameFile(a, b) || exists(f.original) {
		t.Fatalf("the trash's name %v, the other name %v, one file %v, the library's name there %v", errA, errB,
			errA == nil && errB == nil && os.SameFile(a, b), exists(f.original))
	}
	if st, ok := b.Sys().(*syscall.Stat_t); ok && st.Nlink != 2 {
		t.Errorf("the other name's links: %d, want 2 (it and the trash's)", st.Nlink)
	}
	if got, _ := os.ReadFile(other); !bytes.Equal(got, content) {
		t.Error("the other name's content changed")
	}
}

// A list retires one original at a time, at most library.retire.rate a
// minute: the next waits for the next minute.
func TestAListKeepsItsPace(t *testing.T) {
	f := newListFixture(t)
	f.addMovie(t, ltFilm, ltSource, ltVersion, ltPackage, "Another Film")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = 'full'`)
	f.set(t, SettingRetireRate, "1")
	run, items := f.run(t, false, rtFilm, ltFilm)
	if run.Retired != 2 || said(items[0]) != "retired: " || said(items[1]) != "retired: " {
		t.Fatalf("the list: %+v", run)
	}
	if len(f.slept) != 1 || f.slept[0] <= 0 || f.slept[0] > time.Minute {
		t.Errorf("waited %v, want once, for the rest of the minute", f.slept)
	}
	// a dry run touches nothing, and keeps no pace
	f.slept = nil
	if run, _ := f.run(t, true, rtFilm, ltFilm); run.Skipped != 2 || len(f.slept) != 0 {
		t.Errorf("a dry run: %+v, waited %v", run, f.slept)
	}
}

// The titles of a run read a page at a time, in the list's order, of one
// result or of any.
func TestTheTitlesOfAListReadAPageAtATime(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	run, _ := f.run(t, true, rtFilm, "0d0d0d0d-0000-4000-8000-000000000002", rtFilm, "0d0d0d0d-0000-4000-8000-000000000003")
	if run.Total != 3 || run.WouldRetire != 1 || run.Skipped != 2 {
		t.Fatalf("a list naming a title twice: %+v", run)
	}
	page, err := f.l.Items(ctx, run.ID, 2, 0, "")
	if err != nil || len(page) != 2 || page[0].Position != 1 || page[1].Position != 2 {
		t.Fatalf("the first page: %+v, %v", page, err)
	}
	if page, _ = f.l.Items(ctx, run.ID, 2, page[1].Position, ""); len(page) != 1 || page[0].Position != 3 {
		t.Errorf("the next page: %+v", page)
	}
	if page, _ = f.l.Items(ctx, run.ID, 100, 0, ListSkipped); len(page) != 2 || page[0].Position != 2 {
		t.Errorf("the skipped: %+v", page)
	}
	if page, _ = f.l.Items(ctx, run.ID, 100, 0, "pending"); len(page) != 0 {
		t.Errorf("those to come of a run done: %+v", page)
	}
}

// The original of a file that covers several episodes is its holder's: a list
// retires it only when every episode it covers holds too, and a list that
// names a covered episode is told whose original it is.
func TestAListOfAHolderAndTheEpisodesItsFileCovers(t *testing.T) {
	const (
		series   = "2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a"
		covered  = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
		holderTV = "Example Film"
	)
	setup := func(t *testing.T) *listFixture {
		t.Helper()
		f := newListFixture(t)
		// the fixture's movie becomes the holder, the first episode of a series
		storetest.AddItem(t, f.st, series, "series", "A Coastal Show", "")
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET type = 'episode', parent_id = $2, seasonnumber = 1,
			episodenumber = 1 WHERE id = $1`, rtFilm, series)
		storetest.AddItem(t, f.st, covered, "episode", "The Second Hour", series)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2, coveredby = $2
			WHERE id = $1`, covered, rtFilm)
		// its folder is an episode's
		dir := f.p.EpisodeDir(series, rtFilm)
		if err := os.MkdirAll(filepath.Dir(dir), 0o775); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.itemDir, dir); err != nil {
			t.Fatal(err)
		}
		f.itemDir, f.versionDir, f.sourceDir = dir, VersionDir(dir, rtVersion), SourceDir(dir, rtSource)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET dir = $2 WHERE id = $1`, rtVersion, f.versionDir)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET recorddir = $2 WHERE id = $1`, rtSource, f.sourceDir)
		return f
	}
	t.Run("a covered episode held", func(t *testing.T) {
		f := setup(t)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, covered)
		if _, items := f.run(t, false, rtFilm); said(items[0]) != "skipped: an episode its file covers keeps it, "+
			"A Coastal Show · S01E02 · The Second Hour: its originals are held (holdOriginal)" {
			t.Errorf("the holder: %s", said(items[0]))
		}
		f.kept(t)
	})
	t.Run("a covered episode in the re-encode queue", func(t *testing.T) {
		f := setup(t)
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby, state)
			VALUES ('q1', $1, 'items', 'an-admin', 'sent')`, covered)
		if _, items := f.run(t, false, rtFilm); !strings.HasSuffix(said(items[0]), ": it is in the re-encode queue (sent)") {
			t.Errorf("the holder: %s", said(items[0]))
		}
		f.kept(t)
	})
	t.Run("the covered episode listed", func(t *testing.T) {
		f := setup(t)
		if _, items := f.run(t, false, covered); said(items[0]) != "skipped: another episode’s file covers it: its original is "+
			"that of A Coastal Show · S01E01 · "+holderTV+" ("+rtFilm+"), which a list names by its own id" {
			t.Errorf("the covered episode: %s", said(items[0]))
		}
		f.kept(t)
	})
	t.Run("every episode holds", func(t *testing.T) {
		f := setup(t)
		if _, items := f.run(t, false, rtFilm); said(items[0]) != "retired: " {
			t.Fatalf("the holder: %s", said(items[0]))
		}
		if s := f.source(t, rtSource); s.State != SourceDeleted {
			t.Errorf("the holder's source: %s", s.State)
		}
	})
}

// A trash on another filesystem than the original refuses its title before
// anything is claimed (the rename into it would fail once the event is
// recorded, and the retirement would stay half done); with library.trash.grace
// 0, which deletes an original at once, it doesn't matter. /dev stands for
// another filesystem; a dry run touches nothing there.
func TestAListChecksTheTrashIsOnTheOriginalsFilesystem(t *testing.T) {
	f := newListFixture(t)
	if same, err := sameFilesystem(f.original, filepath.Join(f.p.Work, WorkTrash, "20261027", "not", "there", "yet")); err != nil || !same {
		t.Fatalf("the fixture's trash, not there yet: %v, %v", same, err)
	}
	if fi, err := os.Stat("/dev"); err != nil || !fi.IsDir() {
		t.Skip("no /dev to stand for another filesystem")
	}
	if same, err := sameFilesystem(f.original, "/dev"); err != nil || same {
		t.Skipf("/dev is on the original's filesystem here (%v)", err)
	}
	cfg := f.cfg
	cfg.WorkRoot = "/dev/a-trash-on-another-filesystem"
	f.l = NewRetireList(f.st.Pool(), cfg, processing.New(f.st.Pool()), f.gate)
	if _, items := f.run(t, true, rtFilm); said(items[0]) != "skipped: the trash (WORK_ROOT) is on another filesystem than its "+
		"original: put the trash on the share its original is on, or set library.trash.grace to 0 to delete originals at once" {
		t.Errorf("a trash on another filesystem: %s", said(items[0]))
	}
	f.set(t, SettingTrashGrace, "0")
	if _, items := f.run(t, true, rtFilm); said(items[0]) != "would-retire: " {
		t.Errorf("no trash: %s", said(items[0]))
	}
	f.kept(t)
}

// A list retires an original only while the retire job runs no pass: it
// waits for the job's lock, and retires once the pass is over.
func TestAListWaitsForTheRetireJobsPass(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+retireLockKey+`)`); err != nil {
		t.Fatal(err)
	}
	if rep := f.pass(t); rep != (Report{}) {
		t.Fatalf("a pass while another holds its lock: %+v", rep)
	}
	waited := 0
	f.l.sleep = func(ctx context.Context, d time.Duration) bool {
		if d == f.l.lockWait {
			if waited++; waited == 3 {
				if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+retireLockKey+`)`); err != nil {
					t.Error(err)
				}
			}
		}
		return ctx.Err() == nil
	}
	if _, items := f.run(t, false, rtFilm); said(items[0]) != "retired: " || waited != 3 {
		t.Errorf("the list once the pass is over: %s, waited %d times", said(items[0]), waited)
	}
}
