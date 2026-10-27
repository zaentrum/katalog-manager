package library

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// gateOf is a compliance check that passes every title but those it names,
// saying why not; with turn, asked first, its word on a title is what turn
// says when it says (the test's means to change it, or anything, while a run
// is at the title).
type gateOf struct {
	why   map[string]string
	turn  func(id string) (string, bool)
	err   error
	asked int
}

func (g *gateOf) Passed(_ context.Context, ids []string) (map[string]string, error) {
	g.asked++
	if g.err != nil {
		return nil, g.err
	}
	out := map[string]string{}
	for _, id := range ids {
		if g.turn != nil {
			if why, ok := g.turn(id); ok {
				out[id] = why
				continue
			}
		}
		out[id] = g.why[id]
	}
	return out, nil
}

// listFixture is the retire job's fixture as the installation keeps its
// library, library.originals keep and library.retire.list on, its movie's
// version verified once, with a list runner whose compliance check is gate
// and whose waits are noted, not waited for (slept), each calling onSleep
// first when it is set.
type listFixture struct {
	*retireFixture
	l       *RetireList
	gate    *gateOf
	slept   []time.Duration
	onSleep func(d time.Duration)
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
	f.set(t, SettingRetireList, RetireListOn)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now() - interval '2 days', verifiedlevel = 'full'`)
	lf := &listFixture{retireFixture: f, gate: &gateOf{why: map[string]string{}}}
	lf.l = lf.runner(f.cfg)
	return lf
}

// runner is a list runner of the fixture at the paths cfg configures, as
// after a restart: its waits noted.
func (f *listFixture) runner(cfg config.Config) *RetireList {
	l := NewRetireList(f.st.Pool(), cfg, processing.New(f.st.Pool()), f.gate)
	l.sleep = func(ctx context.Context, d time.Duration) bool {
		if f.onSleep != nil {
			f.onSleep(d)
		}
		f.slept = append(f.slept, d)
		return ctx.Err() == nil
	}
	return l
}

// addOther gives the fixture its other movie, verified.
func (f *listFixture) addOther(t *testing.T) {
	t.Helper()
	f.addMovie(t, ltFilm, ltSource, ltVersion, ltPackage, "Another Film")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = 'full'`)
}

// start starts a list as req asks, on an admin's word, and works it to its
// end; it answers the run and its titles.
func (f *listFixture) start(t *testing.T, req ListRequest) (ListRun, []ListItem) {
	t.Helper()
	ctx := context.Background()
	req.By = "an-admin"
	if _, err := f.l.Start(ctx, req); err != nil {
		t.Fatalf("Start(%+v): %v", req, err)
	}
	if err := f.l.Work(ctx); err != nil {
		t.Fatalf("Work: %v", err)
	}
	return f.latest(t)
}

// dry is a dry run of ids to its end.
func (f *listFixture) dry(t *testing.T, ids ...string) (ListRun, []ListItem) {
	t.Helper()
	return f.start(t, ListRequest{Items: ids, DryRun: true})
}

// retire is a run of the titles the dry run from found it would retire, to
// its end.
func (f *listFixture) retire(t *testing.T, from string) (ListRun, []ListItem) {
	t.Helper()
	return f.start(t, ListRequest{FromRun: from})
}

// run is a dry run of ids, or with dry false a dry run of them and then a
// run of the titles it found it would retire: the dry run when it found
// none, else the run.
func (f *listFixture) run(t *testing.T, dry bool, ids ...string) (ListRun, []ListItem) {
	t.Helper()
	run, items := f.dry(t, ids...)
	if dry || run.WouldRetire == 0 {
		return run, items
	}
	return f.retire(t, run.ID)
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

// wouldRetire reports whether a dry run would retire the title it, as it
// says of one: what the package doesn't carry of the original, and the files
// that go with it.
func wouldRetire(it ListItem) bool {
	return strings.HasPrefix(said(it), "would-retire: what its package doesn’t carry of it: ") &&
		strings.Contains(said(it), "; the files that go with it: ")
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

// A dry run says what it would retire - what the package doesn't carry of
// the original, the files that go with it - and touches nothing; a run of
// the titles it found retires exactly their originals, each checked again,
// through the retire job's own path: the event recorded on the admin's word
// with what the package does not carry of the original, the original and its
// sidecars moved into the trash, the source deleted, the retire step done.
// library.originals stays keep, and the retire job touches nothing else.
func TestAListRetiresTheOriginalsOfTheTitlesItNames(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	f.addOther(t)
	size := f.source(t, rtSource).SizeBytes

	const absent = "0d0d0d0d-0000-4000-8000-000000000004"
	dry, items := f.dry(t, rtFilm, absent)
	if dry.State != RunDone || !dry.DryRun || dry.FromRun != nil || dry.RequestedBy != "an-admin" || dry.Total != 2 ||
		dry.WouldRetire != 1 || dry.Skipped != 1 || dry.Pending != 0 || dry.Retired != 0 || dry.Bytes != size ||
		dry.FinishedAt == nil {
		t.Fatalf("the dry run: %+v", dry)
	}
	want := "would-retire: what its package doesn’t carry of it: maxAudioChannels; the files that go with it: " +
		f.de + ", " + f.en + ", " + f.nfo
	if len(items) != 2 || said(items[0]) != want || deref(items[0].SourceID) != rtSource || items[0].Bytes == nil ||
		*items[0].Bytes != size || deref(items[0].Title) != "Example Film" || items[0].At == nil ||
		said(items[1]) != "skipped: it is not in the catalog" {
		t.Fatalf("the dry run's titles: %s | %s\nwant %s", said(items[0]), said(items[1]), want)
	}
	f.kept(t)
	if got := f.step(t, rtFilm); got != "none" {
		t.Errorf("the retire step after a dry run: %s", got)
	}

	// the run takes the titles the dry run found it would retire, and no other
	run, items := f.retire(t, dry.ID)
	if run.State != RunDone || run.DryRun || deref(run.FromRun) != dry.ID || run.Total != 1 || run.Retired != 1 ||
		run.Bytes != size || len(items) != 1 || items[0].ItemID != rtFilm || said(items[0]) != "retired: " {
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
	trashed := []string{"Example Film.de.srt", "Example Film.en.srt", "Example Film.mkv", "Example Film.nfo"}
	if got := files(t, deref(s.TrashPath)); strings.Join(got, "|") != strings.Join(trashed, "|") ||
		!strings.HasPrefix(deref(s.TrashPath), filepath.Join(f.p.Work, WorkTrash)) {
		t.Errorf("the trash %s holds %v, want %v", deref(s.TrashPath), got, trashed)
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
	if got := f.step(t, ltFilm); got != "none" {
		t.Errorf("the retire job claimed an original with library.originals keep: its step %s", got)
	}
	// listed again: retired already, so skipped
	if _, items = f.dry(t, rtFilm); said(items[0]) != "skipped: its original was retired already" {
		t.Errorf("listed again: %s", said(items[0]))
	}
}

// dbPrint is a digest of every table of the test's schema but the run's own.
func dbPrint(t *testing.T, f *listFixture) string {
	t.Helper()
	ctx := context.Background()
	rows, err := f.st.Pool().Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = current_schema()
		AND tablename NOT IN ('com_nalet_katalog_retireruns', 'com_nalet_katalog_retirerunitems') ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	rows.Close()
	var out []string
	for _, tb := range tables {
		var d *string
		if err := f.st.Pool().QueryRow(ctx, `SELECT md5(string_agg(md5(t::text), '' ORDER BY md5(t::text))) FROM `+tb+` t`).
			Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, tb+"="+deref(d))
	}
	return strings.Join(out, "\n")
}

// fsPrint lists every file and folder under root with its mode, size and
// modification time.
func fsPrint(t *testing.T, root string) string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		out = append(out, p+" "+fi.Mode().String()+" "+strconv.FormatInt(fi.Size(), 10)+" "+
			fi.ModTime().UTC().Format(time.RFC3339Nano))
		return nil
	})
	return strings.Join(out, "\n")
}

// A dry run of titles in every state touches no table but the run's own and
// no file: the database and the library are as they were.
func TestADryRunTouchesNothing(t *testing.T) {
	f := newListFixture(t)
	f.addOther(t)
	f.gate.why[ltFilm] = "the compliance check finds it breaks OVER_BITRATE_CAP"
	beforeDB, beforeFS := dbPrint(t, f), fsPrint(t, f.p.Root)
	run, items := f.dry(t, rtFilm, ltFilm, "0d0d0d0d-0000-4000-8000-000000000009")
	if run.WouldRetire != 1 || run.Skipped != 2 || !wouldRetire(items[0]) {
		t.Fatalf("the dry run: %+v, %s", run, said(items[0]))
	}
	if after := dbPrint(t, f); after != beforeDB {
		t.Errorf("the dry run changed the database:\n%s\n---\n%s", beforeDB, after)
	}
	if after := fsPrint(t, f.p.Root); after != beforeFS {
		t.Errorf("the dry run changed the library:\n%s\n---\n%s", beforeFS, after)
	}
}

// A title is skipped, saying why, while any condition does not hold, and
// nothing of it is touched: in a dry run, and in a run of the titles a dry
// run found it would retire, where the condition came to fail after the dry
// run (real).
func TestAListChecksEveryCondition(t *testing.T) {
	ctx := context.Background()
	const series = "2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a"
	for _, c := range []struct {
		name  string
		fx    func(t *testing.T) *listFixture
		setup func(t *testing.T, f *listFixture) string // the title listed
		want  string
		kept  bool
		real  bool // the condition may come to fail after a dry run
	}{
		{"not in the catalog", nil, func(t *testing.T, f *listFixture) string { return "0d0d0d0d-0000-4000-8000-000000000001" },
			"skipped: it is not in the catalog", true, false},
		{"a series", nil, func(t *testing.T, f *listFixture) string {
			storetest.AddItem(t, f.st, series, "series", "A Coastal Show", "")
			return series
		}, "skipped: a series has no original of its own: a list names movies and episodes", true, false},
		{"an item of no type", nil, func(t *testing.T, f *listFixture) string {
			storetest.AddItem(t, f.st, series, "series", "A Coastal Show", "")
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET type = '' WHERE id = $1`, series)
			return series
		}, "skipped: it is an item of no type: a list names movies and episodes", true, false},
		{"held", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
			return rtFilm
		}, "skipped: its originals are held (holdOriginal)", true, true},
		{"no complete version", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'building' WHERE id = $1`, rtVersion)
			return rtFilm
		}, "skipped: it has no complete version", true, true},
		{"never verified", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = NULL, verifiedlevel = NULL`)
			return rtFilm
		}, "skipped: its complete version " + rtVersion + " was never verified", true, true},
		{"a version of two originals, both present", nil, func(t *testing.T, f *listFixture) string {
			const other = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
				VALUES ($1, $2, 'original-2.mkv', 10, 'present')`, other, rtFilm)
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET sourceids = ARRAY[$2::varchar, $3::varchar]
				WHERE id = $1`, rtVersion, rtSource, other)
			return rtFilm
		}, "skipped: its version " + rtVersion + " was made of 2 originals, and a list retires the original of a version " +
			"made of exactly one", true, true},
		{"a version of two originals, one retired", nil, func(t *testing.T, f *listFixture) string {
			const other = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, deletedat,
				deletedby) VALUES ($1, $2, 'original-2.mkv', 10, 'deleted', now(), 'someone')`, other, rtFilm)
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET sourceids = ARRAY[$2::varchar, $3::varchar]
				WHERE id = $1`, rtVersion, rtSource, other)
			return rtFilm
		}, "skipped: its version " + rtVersion + " was made of 2 originals, and a list retires the original of a version " +
			"made of exactly one", true, true},
		{"a step at work", nil, func(t *testing.T, f *listFixture) string {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "transcode", processing.StatusInProgress, nil,
				nil); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its transcode step runs", true, true},
		{"a step to be tried again", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed',
				nextretryat = now() + interval '1 hour' WHERE item_id = $1 AND step = 'package'`, rtFilm)
			return rtFilm
		}, "skipped: its package step failed and is to be tried again", true, true},
		{"in the re-encode queue", nil, func(t *testing.T, f *listFixture) string {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby)
				VALUES ('q1', $1, 'items', 'an-admin')`, rtFilm)
			return rtFilm
		}, "skipped: it is in the re-encode queue (queued)", true, true},
		{"the compliance check", nil, func(t *testing.T, f *listFixture) string {
			f.gate.why[rtFilm] = "the compliance check finds it breaks OVER_BITRATE_CAP"
			return rtFilm
		}, "skipped: the compliance check finds it breaks OVER_BITRATE_CAP", true, true},
		{"the compliance check saying nothing", nil, func(t *testing.T, f *listFixture) string {
			f.l.gate = gateFunc(func(context.Context, []string) (map[string]string, error) { return map[string]string{}, nil })
			return rtFilm
		}, "", true, false},
		{"its package lacks its surround", func(t *testing.T) *listFixture {
			return listFixtureOf(t, newRetireFixtureOf(t, fiveOne, stereo))
		}, func(t *testing.T, f *listFixture) string { return rtFilm }, "skipped: " + SurroundHeld, true, false},
		{"its original moved", nil, func(t *testing.T, f *listFixture) string {
			if err := os.Rename(f.original, f.original+".moved"); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its original isn’t where the catalog says: ", false, true},
		{"its original of another size", nil, func(t *testing.T, f *listFixture) string {
			librarytest.Write(t, f.original, []byte("not the original"))
			return rtFilm
		}, "skipped: its original is 16 bytes, and the catalog recorded ", false, true},
		{"its original a link", nil, func(t *testing.T, f *listFixture) string {
			if err := os.Rename(f.original, f.original+".target"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(f.original+".target", f.original); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its original is no file of its own (a link, a folder): ", false, true},
		{"its version's folder gone", nil, func(t *testing.T, f *listFixture) string {
			if err := os.Rename(f.versionDir, f.versionDir+".gone"); err != nil {
				t.Fatal(err)
			}
			return rtFilm
		}, "skipped: its version’s folder isn’t there: ", false, true},
		{"its version's checksums pending", nil, func(t *testing.T, f *listFixture) string {
			if err := os.RemoveAll(f.versionDir); err != nil {
				t.Fatal(err)
			}
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"essence": f.pkg},
				Pending: true})
			return rtFilm
		}, "skipped: its version’s checksums are pending: no file of its package was hashed yet, so nothing proves it",
			false, true},
		{"the source's record changed", nil, func(t *testing.T, f *listFixture) string {
			librarytest.Write(t, filepath.Join(f.sourceDir, "Example Film.nfo"), []byte("<movie>changed</movie>\n"))
			return rtFilm
		}, "skipped: the source’s record does not verify: ", false, true},
		{"a subtitle paired after packaging", nil, func(t *testing.T, f *listFixture) string {
			fr := filepath.Join(f.folder, "Example Film.fr.srt")
			librarytest.Write(t, fr, []byte("1\n00:00:01,000 --> 00:00:02,000\nBonjour\n"))
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang)
				VALUES ('late', $1, $2, 'srt', 'fr')`, rtFilm, fr)
			return rtFilm
		}, "skipped: the subtitle file ", false, true},
	} {
		for _, dry := range []bool{true, false} {
			if !dry && !c.real {
				continue
			}
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
				var run ListRun
				var items []ListItem
				if dry {
					run, items = f.dry(t, c.setup(t, f))
				} else {
					from, _ := f.dry(t, rtFilm)
					if from.WouldRetire != 1 {
						t.Fatalf("the dry run before: %+v", from)
					}
					c.setup(t, f)
					run, items = f.retire(t, from.ID)
				}
				if c.want == "" {
					if run.Skipped != 1 || said(items[0]) != "skipped: the compliance check said nothing of it" {
						t.Fatalf("%+v: %s", run, said(items[0]))
					}
					return
				}
				if run.State != RunDone || run.Skipped != 1 || len(items) != 1 || !strings.HasPrefix(said(items[0]), c.want) {
					t.Fatalf("%+v: %s, want %s", run, said(items[0]), c.want)
				}
				if s := f.source(t, rtSource); s.State != SourcePresent || s.RetireEventID != nil {
					t.Errorf("the source: %s, event %v", s.State, s.RetireEventID)
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

// A run checks a title once it may act on it, its pace kept and the retire
// job's lock held: what turned while it waited for either holds it back. And
// the retire path asks the list again right before it records the event, so
// that what turned while it verified the version holds it back too.
func TestAListChecksATitleOnceItMayActOnIt(t *testing.T) {
	ctx := context.Background()
	twoFilms := func(t *testing.T) (*listFixture, ListRun) {
		t.Helper()
		f := newListFixture(t)
		f.addOther(t)
		f.set(t, SettingRetireRate, "1")
		dry, _ := f.dry(t, rtFilm, ltFilm)
		if dry.WouldRetire != 2 {
			t.Fatalf("the dry run: %+v", dry)
		}
		return f, dry
	}
	t.Run("the compliance check turns while the pace waits", func(t *testing.T) {
		f, dry := twoFilms(t)
		f.onSleep = func(time.Duration) { f.gate.why[ltFilm] = "the compliance check finds it breaks OVER_BITRATE_CAP" }
		_, items := f.retire(t, dry.ID)
		if said(items[0]) != "retired: " || said(items[1]) != "skipped: the compliance check finds it breaks OVER_BITRATE_CAP" ||
			f.source(t, ltSource).State != SourcePresent {
			t.Errorf("%s | %s; the second's source %s", said(items[0]), said(items[1]), f.source(t, ltSource).State)
		}
	})
	t.Run("queued while the pace waits", func(t *testing.T) {
		f, dry := twoFilms(t)
		f.onSleep = func(time.Duration) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby)
				VALUES ('q-pace', $1, 'items', 'an-admin') ON CONFLICT DO NOTHING`, ltFilm)
		}
		_, items := f.retire(t, dry.ID)
		if said(items[1]) != "skipped: it is in the re-encode queue (queued)" || f.source(t, ltSource).State != SourcePresent {
			t.Errorf("%s; its source %s", said(items[1]), f.source(t, ltSource).State)
		}
	})
	lockedBy := func(t *testing.T, f *listFixture) func() {
		t.Helper()
		conn, err := f.st.Pool().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(conn.Release)
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+retireLockKey+`)`); err != nil {
			t.Fatal(err)
		}
		return func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+retireLockKey+`)`) }
	}
	t.Run("held while the retire job's lock is waited for", func(t *testing.T) {
		f := newListFixture(t)
		dry, _ := f.dry(t, rtFilm)
		unlock := lockedBy(t, f)
		f.onSleep = func(d time.Duration) {
			if d == f.l.lockWait {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
				unlock()
			}
		}
		if _, items := f.retire(t, dry.ID); said(items[0]) != "skipped: its originals are held (holdOriginal)" {
			t.Errorf("%s", said(items[0]))
		}
		f.kept(t)
		if got := f.step(t, rtFilm); got != "none" {
			t.Errorf("the retire step: %s", got)
		}
	})
	t.Run("the compliance check turns while the version is verified", func(t *testing.T) {
		f := newListFixture(t)
		dry, _ := f.dry(t, rtFilm)
		asked := f.gate.asked
		// its word turns from the second time the run asks it: once the run
		// checked the title, before the retire path records the event
		f.gate.turn = func(string) (string, bool) {
			if f.gate.asked > asked+1 {
				return "the compliance check finds it breaks NO_SURROUND", true
			}
			return "", false
		}
		_, items := f.retire(t, dry.ID)
		if said(items[0]) != "skipped: the compliance check finds it breaks NO_SURROUND" {
			t.Errorf("%s", said(items[0]))
		}
		f.kept(t)
		if got := f.step(t, rtFilm); got != "pending 0 error=- details=the compliance check finds it breaks NO_SURROUND" {
			t.Errorf("the retire step: %s", got)
		}
	})
}

// One list runs at a time, in one instance at a time: another is refused
// while it runs, and an instance the list's lock is taken from does nothing.
func TestOneListRunsAtATime(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	if _, err := f.l.Start(ctx, ListRequest{Items: []string{rtFilm}, DryRun: true, By: "an-admin"}); err != nil {
		t.Fatal(err)
	}
	var refused *ListRefused
	if _, err := f.l.Start(ctx, ListRequest{Items: []string{rtFilm}, DryRun: true, By: "an-admin"}); !errors.As(err, &refused) ||
		refused.Code != "RUNNING" || !strings.HasPrefix(refused.Msg, "a dry run of a list of 1 title runs since ") ||
		!strings.HasSuffix(refused.Msg, ", 1 of them still to come: wait for it to finish, or stop it (stopRetireRun)") {
		t.Fatalf("a second list while one runs: %v", err)
	}
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
	if _, err := f.l.Start(ctx, ListRequest{Items: []string{rtFilm}, DryRun: true, By: "an-admin"}); err != nil {
		t.Errorf("a list once the one before is done: %v", err)
	}
}

// What a list is refused: no title, an id that is no title's, more than
// MaxList, a run of titles named, titles and a dry run both, a run of what
// is no dry run done with a title it would retire, the legacy layout, and as
// library.retire.list says (off: none; dry-run: no run).
func TestAListIsRefused(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	many := make([]string, MaxList+1)
	for i := range many {
		many[i] = NewID()
	}
	// refusal is what Start says of req: its refusal, or "ran" of a run it
	// began (which is stopped, so that the next may run).
	refusal := func(req ListRequest) string {
		t.Helper()
		req.By = "an-admin"
		var refused *ListRefused
		_, err := f.l.Start(ctx, req)
		switch {
		case err == nil:
			if _, err := f.l.Stop(ctx, "an-admin"); err != nil {
				t.Fatal(err)
			}
			return "ran"
		case errors.As(err, &refused):
			return refused.Code + ": " + refused.Msg
		}
		t.Fatalf("%+v: %v", req, err)
		return ""
	}
	exactly := make([]string, MaxList)
	copy(exactly, many)
	for _, c := range []struct {
		name string
		req  ListRequest
		want string
	}{
		{"no title", ListRequest{DryRun: true}, "EMPTY: no title is listed"},
		{"blanks", ListRequest{Items: []string{" ", ""}, DryRun: true}, "EMPTY: no title is listed"},
		{"an id that is no title's", ListRequest{Items: []string{rtFilm, "m1", "A1B2", "x", "y", "z"}, DryRun: true},
			`BAD_ID: an id of a title is a lower-case UUID, and these are not: "m1", "A1B2", "x", and 2 more`},
		{"an id that is no title's, of three", ListRequest{Items: []string{"m1", rtFilm + " ", "x", "y"}, DryRun: true},
			`BAD_ID: an id of a title is a lower-case UUID, and these are not: "m1", "x", "y"`},
		{"more than MaxList", ListRequest{Items: many, DryRun: true}, fmt.Sprintf("TOO_MANY: %d titles are listed, and a list "+
			"names %d at most", MaxList+1, MaxList)},
		{"exactly MaxList", ListRequest{Items: exactly, DryRun: true}, "ran"},
		{"a run of titles named", ListRequest{Items: []string{rtFilm}}, "NAMED_TITLES: a list of titles named is a dry run: " +
			"retire the titles a dry run found it would, with fromRun (its id)"},
		{"titles and a dry run", ListRequest{Items: []string{rtFilm}, FromRun: rtFilm}, "BOTH: a list names titles, or the " +
			"dry run whose titles it retires, and not both"},
		{"a run of no run", ListRequest{FromRun: "0d0d0d0d-0000-4000-8000-000000000003"},
			"FROM_RUN: there is no run 0d0d0d0d-0000-4000-8000-000000000003"},
	} {
		if got := refusal(c.req); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// a run of a run that is no dry run, or a dry run that found none
	dry, _ := f.dry(t, rtFilm)
	run, _ := f.retire(t, dry.ID)
	if got := refusal(ListRequest{FromRun: run.ID}); got != "FROM_RUN: run "+run.ID+" is no dry run: a run retires the "+
		"titles a dry run found it would" {
		t.Errorf("a run of a run: %s", got)
	}
	none, _ := f.dry(t, rtFilm)
	if got := refusal(ListRequest{FromRun: none.ID}); got != "FROM_RUN: dry run "+none.ID+" found no title whose original "+
		"it would retire" {
		t.Errorf("a run of a dry run that found none: %s", got)
	}
	stopped, err := f.l.Start(ctx, ListRequest{Items: []string{ltFilm}, DryRun: true, By: "an-admin"})
	if err != nil {
		t.Fatal(err)
	}
	if got := refusal(ListRequest{FromRun: stopped.ID}); got != "FROM_RUN: dry run "+stopped.ID+" is running: a run takes "+
		"the titles of a dry run done" {
		t.Errorf("a run of a dry run that runs: %s", got)
	}
	if _, err := f.l.Stop(ctx, "an-admin"); err != nil {
		t.Fatal(err)
	}
	if got := refusal(ListRequest{FromRun: stopped.ID}); got != "FROM_RUN: dry run "+stopped.ID+" is stopped: a run takes "+
		"the titles of a dry run done" {
		t.Errorf("a run of a dry run stopped: %s", got)
	}
	f.set(t, SettingRetireList, RetireListDryRun)
	if got := refusal(ListRequest{FromRun: dry.ID}); got != "DRY_RUN_ONLY: library.retire.list is dry-run: a list only says "+
		"what it would retire; set it to on to retire the titles a dry run found it would" {
		t.Errorf("a run with library.retire.list dry-run: %s", got)
	}
	f.set(t, SettingRetireList, RetireListOff)
	if got := refusal(ListRequest{Items: []string{rtFilm}, DryRun: true}); got != "OFF: library.retire.list is off: no list "+
		"runs; set it to dry-run to see what a list would retire" {
		t.Errorf("library.retire.list off: %s", got)
	}
	f.set(t, SettingRetireList, "maybe")
	if got := refusal(ListRequest{FromRun: dry.ID}); !strings.HasPrefix(got, "DRY_RUN_ONLY: ") {
		t.Errorf("library.retire.list of no value it knows, a dry-run's: %s", got)
	}
	f.set(t, SettingRetireList, RetireListOn)
	f.set(t, SettingLayout, LayoutLegacy)
	if got := refusal(ListRequest{Items: []string{rtFilm}, DryRun: true}); !strings.HasPrefix(got, "LEGACY: ") {
		t.Errorf("the legacy layout: %s", got)
	}
}

// A list is stopped after the title it is at: by an admin (stopRetireRun),
// once the title has its result or before it acts on the next, its titles
// still to come left as they are; and by library.retire.list, off (any run)
// or no longer on (a run that retires).
func TestAListIsStopped(t *testing.T) {
	ctx := context.Background()
	t.Run("by an admin, while the pace waits", func(t *testing.T) {
		f := newListFixture(t)
		f.addOther(t)
		f.set(t, SettingRetireRate, "1")
		dry, _ := f.dry(t, rtFilm, ltFilm)
		f.onSleep = func(time.Duration) {
			if run, err := f.l.Stop(ctx, "another-admin"); err != nil || run == nil || run.State != RunStopped {
				t.Errorf("Stop: %+v, %v", run, err)
			}
		}
		run, items := f.retire(t, dry.ID)
		if run.State != RunStopped || deref(run.Error) != "stopped by another-admin (stopRetireRun)" || run.Retired != 1 ||
			run.Pending != 1 || said(items[0]) != "retired: " || items[1].Result != nil {
			t.Fatalf("the run stopped: %+v; %s | %s", run, said(items[0]), said(items[1]))
		}
		if s := f.source(t, ltSource); s.State != SourcePresent || f.step(t, ltFilm) != "none" {
			t.Errorf("the title to come: %s, step %s", s.State, f.step(t, ltFilm))
		}
		if run, err := f.l.Stop(ctx, "another-admin"); err != nil || run != nil {
			t.Errorf("Stop with none running: %+v, %v", run, err)
		}
		if err := f.l.Work(ctx); err != nil {
			t.Fatal(err)
		}
		if run, _ := f.latest(t); run.State != RunStopped || run.Pending != 1 {
			t.Errorf("a stopped run worked: %+v", run)
		}
	})
	for _, c := range []struct{ value, want string }{
		{RetireListOff, "library.retire.list is off: the run stopped"},
		{RetireListDryRun, "library.retire.list is dry-run: no original is retired, and the run stopped"},
	} {
		t.Run("by library.retire.list "+c.value, func(t *testing.T) {
			f := newListFixture(t)
			dry, _ := f.dry(t, rtFilm)
			if _, err := f.l.Start(ctx, ListRequest{FromRun: dry.ID, By: "an-admin"}); err != nil {
				t.Fatal(err)
			}
			f.set(t, SettingRetireList, c.value)
			if err := f.l.Work(ctx); err != nil {
				t.Fatal(err)
			}
			run, items := f.latest(t)
			if run.State != RunStopped || deref(run.Error) != c.want || items[0].Result != nil {
				t.Errorf("%+v: %s", run, said(items[0]))
			}
			f.kept(t)
		})
	}
	t.Run("by library.retire.list dry-run while the retire job's lock is waited for", func(t *testing.T) {
		f := newListFixture(t)
		dry, _ := f.dry(t, rtFilm)
		conn, err := f.st.Pool().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+retireLockKey+`)`); err != nil {
			t.Fatal(err)
		}
		f.onSleep = func(d time.Duration) {
			if d == f.l.lockWait {
				f.set(t, SettingRetireList, RetireListDryRun)
				_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+retireLockKey+`)`)
			}
		}
		if _, err := f.l.Start(ctx, ListRequest{FromRun: dry.ID, By: "an-admin"}); err != nil {
			t.Fatal(err)
		}
		if err := f.l.Work(ctx); err != nil {
			t.Fatal(err)
		}
		run, items := f.latest(t)
		if run.State != RunStopped || deref(run.Error) != "library.retire.list is dry-run: no original is retired, and the "+
			"run stopped" || items[0].Result != nil {
			t.Errorf("%+v: %s", run, said(items[0]))
		}
		f.kept(t)
		if got := f.step(t, rtFilm); got != "none" {
			t.Errorf("the retire step: %s", got)
		}
	})
	t.Run("a dry run by library.retire.list off", func(t *testing.T) {
		f := newListFixture(t)
		if _, err := f.l.Start(ctx, ListRequest{Items: []string{rtFilm}, DryRun: true, By: "an-admin"}); err != nil {
			t.Fatal(err)
		}
		f.set(t, SettingRetireList, RetireListOff)
		if err := f.l.Work(ctx); err != nil {
			t.Fatal(err)
		}
		if run, _ := f.latest(t); run.State != RunStopped || deref(run.Error) != "library.retire.list is off: the run stopped" {
			t.Errorf("%+v", run)
		}
	})
}

// A title an error stops is tried again a while later, a few times, each
// wait longer, and then failed, saying so, and the run goes on; a run whose
// own titles fail so several in a row stops, failed, saying the last error.
func TestAListOutlastsAnError(t *testing.T) {
	f := newListFixture(t)
	f.addOther(t)
	// failing titles of the catalog, so that the compliance check is asked of
	// them: n of them
	failing := func(n, from int) []string {
		ids := make([]string, n)
		for i := range ids {
			id := fmt.Sprintf("0d0d0d0d-0000-4000-8000-%012d", from+i)
			ids[i] = id
			storetest.AddItem(t, f.st, id, "movie", "A Film "+id[len(id)-3:], "")
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
				VALUES ($1, $1, 'original.mkv', 1, 'present')`, id)
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, completedat,
				verifiedat) VALUES ($1, $1, ARRAY[$1]::varchar[], 'complete', now(), now())`, id)
		}
		return ids
	}
	f.gate.err = errors.New("the catalog is not there")
	run, items := f.dry(t, rtFilm)
	if run.State != RunDone || run.Failed != 1 || said(items[0]) != "failed: an error stopped it 3 times, the last: the catalog "+
		"is not there" {
		t.Fatalf("a title an error stopped: %+v, %s", run, said(items[0]))
	}
	if len(f.slept) != 2 || f.slept[0] != f.l.backoff || f.slept[1] != 2*f.l.backoff {
		t.Errorf("the waits: %v, want %v and twice it", f.slept, f.l.backoff)
	}
	// the titles in a row an error stopped are counted of a run: the next
	// one's four are fewer than stop it
	if run, _ = f.dry(t, failing(runErrors-1, 100)...); run.State != RunDone || run.Failed != runErrors-1 {
		t.Errorf("a run of %d titles an error stops, after one of one: %+v", runErrors-1, run)
	}

	// an error once: tried again, and it goes on
	f.slept = nil
	f.gate.err = nil
	errs := 0
	f.l.gate = gateFunc(func(ctx context.Context, ids []string) (map[string]string, error) {
		if errs++; errs == 1 {
			return nil, errors.New("a moment's error")
		}
		return f.gate.Passed(ctx, ids)
	})
	if _, items := f.dry(t, rtFilm, ltFilm); !wouldRetire(items[0]) || !wouldRetire(items[1]) || len(f.slept) != 1 {
		t.Errorf("an error once: %s | %s, waits %v", said(items[0]), said(items[1]), f.slept)
	}

	// errors that stop every title: the run stops after runErrors of them,
	// saying the last, cut to what a run keeps
	long := "the catalog is not there: " + strings.Repeat("a reason of many words ", 30)
	f.l.gate = gateFunc(func(context.Context, []string) (map[string]string, error) { return nil, errors.New(long) })
	run, _ = f.dry(t, failing(runErrors+2, 200)...)
	want := clip(fmt.Sprintf("errors stopped %d titles in a row, the last: an error stopped it 3 times, the last: %s", runErrors,
		long), 500)
	if run.State != RunFailed || run.Failed != runErrors || run.Pending != 2 || deref(run.Error) != want {
		t.Errorf("a run every title of which fails: %+v", run)
	}
}

// An error of the catalog's in the retire path, once the run claimed the
// original, is one as any: the title is tried again a while later, where its
// retirement stopped, and retired once the error is over, or failed after the
// third time, its claim left to the retire job, which calls it off while
// library.originals keeps originals.
func TestAListOutlastsAnErrorInTheRetirePath(t *testing.T) {
	for _, c := range []struct {
		name   string
		errors int // the calls of the compliance check after the first that fail
		want   string
	}{
		{"once", 1, "retired: "},
		{"every time", 99, "failed: an error stopped it 3 times, the last: the compliance check can’t be reached"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newListFixture(t)
			dry, _ := f.dry(t, rtFilm)
			calls := 0
			f.l.gate = gateFunc(func(ctx context.Context, ids []string) (map[string]string, error) {
				if calls++; calls > 1 && calls <= 1+c.errors {
					return nil, errors.New("the compliance check can’t be reached")
				}
				return f.gate.Passed(ctx, ids)
			})
			f.slept = nil
			if _, items := f.retire(t, dry.ID); said(items[0]) != c.want {
				t.Fatalf("%s\nwant %s", said(items[0]), c.want)
			}
			// the waits after an error (another test's pass may hold the retire
			// job's lock a while too)
			var waits []time.Duration
			for _, d := range f.slept {
				if d != f.l.lockWait {
					waits = append(waits, d)
				}
			}
			if c.errors == 1 {
				if s := f.source(t, rtSource); s.State != SourceDeleted || len(waits) != 1 || f.events(t) != 1 {
					t.Errorf("the source: %s; waits %v; events %d", s.State, waits, f.events(t))
				}
				return
			}
			if s := f.source(t, rtSource); s.State != SourceRetiring || s.RetireEventID == nil || f.events(t) != 0 ||
				len(waits) != 2 || waits[1] != 2*waits[0] {
				t.Errorf("the source: %s, its claim %v; events %d; waits %v", s.State, s.RetireEventID, f.events(t), waits)
			}
			if rep := f.pass(t); rep.Originals != 0 || rep.Failed != 0 {
				t.Errorf("the retire job's pass: %+v", rep)
			}
			f.kept(t)
		})
	}
}

// gateFunc is a compliance check of a function.
type gateFunc func(ctx context.Context, ids []string) (map[string]string, error)

func (g gateFunc) Passed(ctx context.Context, ids []string) (map[string]string, error) {
	return g(ctx, ids)
}

// A list a restart interrupted goes on where it stopped: a retirement it
// claimed is resumed on its word, asking the list again (what turned since
// holds it back); one the retire job called off meanwhile (library.originals
// keep) is checked anew and retired; one whose event it recorded, which the
// job finished meanwhile, is retired, on its word; and an original deleted
// meanwhile on another's word is no retirement of the run's.
func TestAListResumesWhereARestartStoppedIt(t *testing.T) {
	ctx := context.Background()
	// begin is a run of the title interrupted once it claimed its original,
	// its event recorded too when recorded.
	begin := func(t *testing.T, f *listFixture, recorded bool) {
		t.Helper()
		dry, _ := f.dry(t, rtFilm)
		run, err := f.l.Start(ctx, ListRequest{FromRun: dry.ID, By: "an-admin"})
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
		f.l = f.runner(f.cfg)
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
	t.Run("claimed, the compliance check turned since", func(t *testing.T) {
		f := newListFixture(t)
		begin(t, f, false)
		f.gate.why[rtFilm] = "the compliance check finds it breaks OVER_BITRATE_CAP"
		_, items := restarted(t, f)
		if said(items[0]) != "skipped: the compliance check finds it breaks OVER_BITRATE_CAP" {
			t.Errorf("%s", said(items[0]))
		}
		if s := f.source(t, rtSource); s.State != SourcePresent || s.RetireEventID != nil || !exists(f.original) {
			t.Errorf("the source: %s, event %v", s.State, s.RetireEventID)
		}
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
	t.Run("deleted meanwhile on another's word", func(t *testing.T) {
		f := newListFixture(t)
		begin(t, f, false)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now(),
			deletedby = 'replaceSource' WHERE id = $1`, rtSource)
		run, items := restarted(t, f)
		if run.Retired != 0 || said(items[0]) != "skipped: its original was deleted meanwhile, by replaceSource" {
			t.Errorf("%+v: %s", run, said(items[0]))
		}
	})
}

// A failure of a retirement once its event is recorded says what it is: the
// retire job finishes it.
func TestAListSaysAFailureOnceTheEventIsRecorded(t *testing.T) {
	f := newListFixture(t)
	dry, _ := f.dry(t, rtFilm)
	// the trash's day is a file: the original can't be moved into it
	trash := f.p.TrashDay(time.Now())
	librarytest.Write(t, trash, []byte("no folder"))
	_, items := f.retire(t, dry.ID)
	if !strings.HasPrefix(said(items[0]), "failed: its original-deleted event is recorded, and the rest of its retirement "+
		"failed: ") || !strings.HasSuffix(said(items[0]), "; the retire job finishes it") {
		t.Errorf("%s", said(items[0]))
	}
	if s := f.source(t, rtSource); s.State != SourceRetiring {
		t.Errorf("the source: %s", s.State)
	}
}

// The library's name of an original is all a list retires: a file with a
// second name, a hard link another tree shares, is retired by the library's
// name, moved into the trash, its other name staying as it is, the one file
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
	if _, items := f.dry(t, rtFilm); !wouldRetire(items[0]) {
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
		t.Fatalf("the trash's name %v, the other name %v, the library's name there %v", errA, errB, exists(f.original))
	}
	if links(t, other) != 2 {
		t.Errorf("the other name's links: %d, want 2 (it and the trash's)", links(t, other))
	}
	if got, _ := os.ReadFile(other); !bytes.Equal(got, content) {
		t.Error("the other name's content changed")
	}
}

func links(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no link count here")
	}
	return uint64(st.Nlink)
}

// The installation's shape: the original in its version's folder, no name it
// arrived under, a second hard link outside every root the retire path
// deletes under (the other tree's), its sidecars' rows pointing into the
// record. A list retires the library's name alone - the record, the other
// name and its sidecar stay - and once the trash's grace is over its purge
// unlinks the trash's name alone.
func TestAListRetiresAnOriginalOfTheInstallationsShape(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	lib := f.intoVersion(t, rtVersion)
	other := filepath.Join(t.TempDir(), "Example Film (2020)", "Example Film (2020).mkv")
	if err := os.MkdirAll(filepath.Dir(other), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(lib, other); err != nil {
		t.Fatal(err)
	}
	oldSub := filepath.Join(filepath.Dir(other), "Example Film (2020).en.srt")
	enCopy := filepath.Join(f.sourceDir, "subtitle-1.en.srt")
	b, _ := os.ReadFile(enCopy)
	librarytest.Write(t, oldSub, b)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_subtitleassets SET path = $2 WHERE id = $1`, "en-"+rtFilm[:8], enCopy)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_subtitleassets SET path = $2 WHERE id = $1`, "de-"+rtFilm[:8],
		filepath.Join(f.sourceDir, "subtitle-2.de.srt"))
	content, _ := os.ReadFile(other)
	_, items := f.run(t, false, rtFilm)
	if said(items[0]) != "retired: " {
		t.Fatalf("the run: %s", said(items[0]))
	}
	s := f.source(t, rtSource)
	if got := files(t, deref(s.TrashPath)); strings.Join(got, "|") != "original.mkv" {
		t.Errorf("the trash holds %v, want the original alone", got)
	}
	for _, p := range []string{f.en, f.de, f.nfo, oldSub, enCopy, filepath.Join(f.sourceDir, "subtitle-2.de.srt"),
		filepath.Join(f.sourceDir, "source.json")} {
		if !exists(p) {
			t.Errorf("%s was moved", p)
		}
	}
	if links(t, other) != 2 || exists(lib) {
		t.Errorf("after the run: links %d, the library's name there %v", links(t, other), exists(lib))
	}
	if _, err := VerifyVersion(f.versionDir, true); err != nil {
		t.Errorf("the version after the run: %v", err)
	}
	set, _ := ReadSettings(ctx, f.st.Pool())
	if n, err := f.r.purge(f.p, set, time.Now().Add(72*time.Hour)); err != nil || n != 1 {
		t.Fatalf("the purge: %d, %v", n, err)
	}
	if exists(filepath.Join(deref(s.TrashPath), "original.mkv")) || links(t, other) != 1 || !exists(oldSub) {
		t.Errorf("after the purge: the trash's name there, links %d, the other's sidecar there %v", links(t, other), exists(oldSub))
	}
	if got, _ := os.ReadFile(other); !bytes.Equal(got, content) {
		t.Error("the other name's content changed")
	}
}

// A sidecar another names too - another title's subtitle row or playback
// row, or another original's own place - stays where it is when the original
// it came with is retired: what names it still names a file there, and the
// row of the title retired points at its copy all the same. A dry run says
// it is no file that goes with the original.
func TestASidecarAnotherNamesStays(t *testing.T) {
	ctx := context.Background()
	for name, share := range map[string]func(t *testing.T, f *listFixture){
		"another title's subtitle row": func(t *testing.T, f *listFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label)
				VALUES ('shared-en', $1, $2, 'srt', 'en', 'English')`, ltFilm, f.en)
		},
		"another title's playback row": func(t *testing.T, f *listFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, codec, sizebytes, kind)
				VALUES ('shared-en', $1, $2, 'srt', 1, 'extra')`, ltFilm, f.en)
		},
		"another original's place": func(t *testing.T, f *listFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2 WHERE id = $1`, ltSource, f.en)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newListFixture(t)
			f.addOther(t)
			share(t, f)
			dry, items := f.dry(t, rtFilm)
			if want := "would-retire: what its package doesn’t carry of it: maxAudioChannels; the files that go with it: " +
				f.de + ", " + f.nfo; said(items[0]) != want {
				t.Errorf("the dry run: %s\nwant %s", said(items[0]), want)
			}
			if _, items = f.retire(t, dry.ID); said(items[0]) != "retired: " {
				t.Fatalf("the run: %s", said(items[0]))
			}
			s := f.source(t, rtSource)
			if !exists(f.en) || exists(f.de) || exists(f.nfo) {
				t.Errorf("the shared file there %v; the others there %v, %v", exists(f.en), exists(f.de), exists(f.nfo))
			}
			if got := files(t, deref(s.TrashPath)); strings.Join(got, "|") != "Example Film.de.srt|Example Film.mkv|Example Film.nfo" {
				t.Errorf("the trash holds %v", got)
			}
			var path string
			if err := f.st.Pool().QueryRow(ctx, `SELECT path FROM com_nalet_katalog_subtitleassets WHERE id = $1`,
				"en-"+rtFilm[:8]).Scan(&path); err != nil || path != filepath.Join(f.versionDir, "subs/0.vtt") {
				t.Errorf("the retired title's row: %s, %v", path, err)
			}
		})
	}
}

// An original that lies outside every root the retire path deletes under is
// no original a list retires: a dry run says so, and so does a run once it
// came to lie there after its dry run, before anything is claimed.
func TestAListRefusesAnOriginalOutsideTheRoots(t *testing.T) {
	away := func(t *testing.T, f *listFixture) string {
		t.Helper()
		to := filepath.Join(t.TempDir(), "Example Film.mkv")
		if err := os.Rename(f.original, to); err != nil {
			t.Fatal(err)
		}
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2 WHERE id = $1`, rtSource, to)
		return to
	}
	want := func(to string) string {
		return "skipped: " + to + " lies outside the arrivals (ARRIVALS_ROOT, EXTRAS_ROOT) and the media root (NFS_ROOT): " +
			"the retire job deletes nothing there"
	}
	f := newListFixture(t)
	to := away(t, f)
	if _, items := f.dry(t, rtFilm); said(items[0]) != want(to) {
		t.Errorf("a dry run: %s", said(items[0]))
	}
	g := newListFixture(t)
	dry, _ := g.dry(t, rtFilm)
	to = away(t, g)
	if _, items := g.retire(t, dry.ID); said(items[0]) != want(to) {
		t.Errorf("a run: %s", said(items[0]))
	}
	if s := g.source(t, rtSource); s.State != SourcePresent || s.RetireEventID != nil || !exists(to) ||
		g.step(t, rtFilm) != "none" {
		t.Errorf("the source: %s, event %v, the file there %v, step %s", s.State, s.RetireEventID, exists(to), g.step(t, rtFilm))
	}
}

// The retire path asks the list again right before it records the event
// (Authority.Recheck), once it claimed the original and verified its
// version: what turned once the run checked the title holds it back - the
// claim's own conditions (held, a step at work), the surround the retire job
// holds an original for, and every condition a list holds a title to. Each
// turns as the compliance check is asked of the title: as the run checks it
// (1), before the claim, or as the retire path asks again (2), right before
// the event.
func TestTheRetirePathAsksTheListAgain(t *testing.T) {
	ctx := context.Background()
	const other = "3c4d5e6f-7a8b-4c9d-8e0f-1a2b3c4d5e6f"
	held := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
	}
	atWork := func(t *testing.T, f *listFixture) {
		if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "chapter", processing.StatusPending, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	queued := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby)
			VALUES ('q-again', $1, 'items', 'an-admin')`, rtFilm)
	}
	second := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
			VALUES ($1, $2, 'original-2.mkv', 10, 'present')`, other, rtFilm)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET sourceids = sourceids || $2::varchar WHERE id = $1`,
			rtVersion, other)
	}
	covered := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $2 WHERE id = $1`, rtFilm, ltFilm)
	}
	const twoOriginals = "skipped: its version " + rtVersion + " was made of 2 originals, and a list retires the original " +
		"of a version made of exactly one"
	const coveredBy = "skipped: another episode’s file covers it: its original is that of Another Film (" + ltFilm + "), " +
		"which a list names by its own id"
	for _, c := range []struct {
		name   string
		call   int
		change func(t *testing.T, f *listFixture)
		want   string
	}{
		{"held before the claim", 1, held, "skipped: the title holds its originals (holdOriginal)"},
		{"a step at work before the claim", 1, atWork, "skipped: the title's pipeline reads its original again"},
		{"its package lacking its surround before the claim", 1, func(t *testing.T, f *listFixture) {
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"essence": stereo}})
		}, "skipped: " + SurroundHeld},
		{"queued before the claim", 1, queued, "skipped: it is in the re-encode queue (queued)"},
		{"a second original before the claim", 1, second, twoOriginals},
		{"another's file covering it before the claim", 1, covered, coveredBy},
		{"the compliance check turning", 2, func(t *testing.T, f *listFixture) {
			f.gate.why[rtFilm] = "the compliance check finds it breaks NO_SURROUND"
		}, "skipped: the compliance check finds it breaks NO_SURROUND"},
		{"held at the last", 2, held, "skipped: its originals are held (holdOriginal)"},
		{"a step at work at the last", 2, atWork, "skipped: its chapter step waits"},
		{"queued at the last", 2, queued, "skipped: it is in the re-encode queue (queued)"},
		{"a second original at the last", 2, second, twoOriginals},
		{"another original at the last", 2, func(t *testing.T, f *listFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
				VALUES ($1, $2, 'original-2.mkv', 10, 'present')`, other, rtFilm)
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET sourceids = ARRAY[$2::varchar] WHERE id = $1`,
				rtVersion, other)
		}, "skipped: its version " + rtVersion + " is made of another original now"},
		{"another's file covering it at the last", 2, covered, coveredBy},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newListFixture(t)
			f.addOther(t)
			dry, _ := f.dry(t, rtFilm)
			calls := 0
			f.gate.turn = func(string) (string, bool) {
				if calls++; calls == c.call {
					c.change(t, f)
				}
				return "", false
			}
			if _, items := f.retire(t, dry.ID); said(items[0]) != c.want {
				t.Errorf("%s\nwant %s", said(items[0]), c.want)
			}
			if calls < c.call {
				t.Errorf("the compliance check was asked %d times: nothing turned", calls)
			}
			f.kept(t)
		})
	}
}

// An admin of a name as long as a run keeps (255) retires as any: who the
// row says deleted the original is cut to what it keeps, and a run that
// resumes knows it for its own.
func TestAListOnTheWordOfAnAdminOfALongName(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	long := strings.Repeat("an-admin-", 40)
	if _, err := f.l.Start(ctx, ListRequest{Items: []string{rtFilm}, DryRun: true, By: long}); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	dry, _ := f.latest(t)
	if _, err := f.l.Start(ctx, ListRequest{FromRun: dry.ID, By: long}); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	run, items := f.latest(t)
	by := clip(RetireListedBy(clip(long, 255)), 255)
	if run.RequestedBy != clip(long, 255) || said(items[0]) != "retired: " {
		t.Fatalf("the run: %+v, %s", run, said(items[0]))
	}
	if s := f.source(t, rtSource); s.State != SourceDeleted || deref(s.DeletedBy) != by || len([]rune(by)) != 255 {
		t.Errorf("the source: %s by %q", s.State, deref(s.DeletedBy))
	}
	// resumed as a restart would: the original deleted on its word is its own
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_retireruns SET state = 'running', finishedat = NULL WHERE id = $1`, run.ID)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_retirerunitems SET result = NULL, reason = NULL, phase = 'retiring'
		WHERE run_id = $1`, run.ID)
	f.l = f.runner(f.cfg)
	if err := f.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if run, items = f.latest(t); run.State != RunDone || said(items[0]) != "retired: " {
		t.Errorf("resumed: %+v, %s", run, said(items[0]))
	}
}

// The record is never moved into the trash, whatever root lies over it: with
// the library a folder of NFS_ROOT, the subtitle rows of the original point
// into its record, and the original alone goes.
func TestTheRecordIsNeverMovedIntoTheTrash(t *testing.T) {
	f := newListFixture(t)
	f.intoVersion(t, rtVersion)
	cfg := f.cfg
	cfg.NFSRoot = cfg.LibraryRoot
	f.cfg = cfg
	f.l = f.runner(cfg)
	enCopy := filepath.Join(f.sourceDir, "subtitle-1.en.srt")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_subtitleassets SET path = $2 WHERE id = $1`, "en-"+rtFilm[:8], enCopy)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_subtitleassets SET path = $2 WHERE id = $1`, "de-"+rtFilm[:8],
		filepath.Join(f.sourceDir, "subtitle-2.de.srt"))
	_, items := f.run(t, false, rtFilm)
	s := f.source(t, rtSource)
	if got := files(t, deref(s.TrashPath)); said(items[0]) != "retired: " || strings.Join(got, "|") != "original.mkv" ||
		!exists(enCopy) {
		t.Errorf("%s; the trash %v; the record's copy there %v", said(items[0]), got, exists(enCopy))
	}
	if _, err := VerifySource(f.sourceDir); err != nil {
		t.Errorf("the record after: %v", err)
	}
}

// A list retires one original at a time, at most library.retire.rate a
// minute: the next waits for the next minute. A dry run keeps no pace.
func TestAListKeepsItsPace(t *testing.T) {
	f := newListFixture(t)
	f.addOther(t)
	f.set(t, SettingRetireRate, "1")
	dry, _ := f.dry(t, rtFilm, ltFilm)
	if len(f.slept) != 0 {
		t.Errorf("a dry run waited %v", f.slept)
	}
	run, items := f.retire(t, dry.ID)
	if run.Retired != 2 || said(items[0]) != "retired: " || said(items[1]) != "retired: " {
		t.Fatalf("the run: %+v", run)
	}
	if len(f.slept) != 1 || f.slept[0] <= 0 || f.slept[0] > time.Minute {
		t.Errorf("waited %v, want once, for the rest of the minute", f.slept)
	}
}

// The titles of a run read a page at a time, in the list's order, of one
// result or of any.
func TestTheTitlesOfAListReadAPageAtATime(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	run, _ := f.dry(t, rtFilm, "0d0d0d0d-0000-4000-8000-000000000002", rtFilm, "0d0d0d0d-0000-4000-8000-000000000003")
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
// retires it only when every episode it covers holds too, also once it may
// act on it, and a list that names a covered episode is told whose original
// it is.
func TestAListOfAHolderAndTheEpisodesItsFileCovers(t *testing.T) {
	const (
		series  = "2d3e4f5a-6b7c-4d8e-9f0a-1b2c3d4e5f6a"
		covered = "7a8b9c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"
	)
	ctx := context.Background()
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
		if _, items := f.dry(t, rtFilm); said(items[0]) != "skipped: an episode its file covers keeps it, "+
			"A Coastal Show · S01E02 · The Second Hour: its originals are held (holdOriginal)" {
			t.Errorf("the holder: %s", said(items[0]))
		}
		f.kept(t)
	})
	t.Run("a covered episode in the re-encode queue", func(t *testing.T) {
		f := setup(t)
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, selection, enqueuedby, state)
			VALUES ('q1', $1, 'items', 'an-admin', 'sent')`, covered)
		if _, items := f.dry(t, rtFilm); !strings.HasSuffix(said(items[0]), ": it is in the re-encode queue (sent)") {
			t.Errorf("the holder: %s", said(items[0]))
		}
	})
	t.Run("a covered episode at work", func(t *testing.T) {
		f := setup(t)
		if err := processing.New(f.st.Pool()).Upsert(ctx, covered, "subtitle", processing.StatusPending, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, items := f.dry(t, rtFilm); said(items[0]) != "skipped: an episode its file covers keeps it, "+
			"A Coastal Show · S01E02 · The Second Hour: its subtitle step waits" {
			t.Errorf("the holder: %s", said(items[0]))
		}
	})
	t.Run("a covered episode held while the retire job's lock is waited for", func(t *testing.T) {
		f := setup(t)
		dry, _ := f.dry(t, rtFilm)
		conn, err := f.st.Pool().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+retireLockKey+`)`); err != nil {
			t.Fatal(err)
		}
		f.onSleep = func(d time.Duration) {
			if d == f.l.lockWait {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, covered)
				_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+retireLockKey+`)`)
			}
		}
		if _, items := f.retire(t, dry.ID); !strings.HasSuffix(said(items[0]), ": its originals are held (holdOriginal)") {
			t.Errorf("the holder: %s", said(items[0]))
		}
		if s := f.source(t, rtSource); s.State != SourcePresent {
			t.Errorf("the holder's source: %s", s.State)
		}
	})
	t.Run("a covered episode held as the retire path asks again", func(t *testing.T) {
		f := setup(t)
		dry, _ := f.dry(t, rtFilm)
		calls := 0
		f.gate.turn = func(string) (string, bool) {
			if calls++; calls == 2 {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, covered)
			}
			return "", false
		}
		if _, items := f.retire(t, dry.ID); said(items[0]) != "skipped: an episode its file covers keeps it, "+
			"A Coastal Show · S01E02 · The Second Hour: its originals are held (holdOriginal)" {
			t.Errorf("the holder: %s", said(items[0]))
		}
		f.kept(t)
	})
	t.Run("the covered episode listed", func(t *testing.T) {
		f := setup(t)
		if _, items := f.dry(t, covered); said(items[0]) != "skipped: another episode’s file covers it: its original is "+
			"that of A Coastal Show · S01E01 · Example Film ("+rtFilm+"), which a list names by its own id" {
			t.Errorf("the covered episode: %s", said(items[0]))
		}
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
	cfg.ArrivalsRoot = f.p.Arrivals // the arrivals stay where they are, the trash goes elsewhere
	cfg.WorkRoot = "/dev/a-trash-on-another-filesystem"
	f.l = f.runner(cfg)
	if _, items := f.dry(t, rtFilm); said(items[0]) != "skipped: the trash (WORK_ROOT) is on another filesystem than its "+
		"original: put the trash on the share its original is on, or set library.trash.grace to 0 to delete originals at once" {
		t.Errorf("a trash on another filesystem: %s", said(items[0]))
	}
	f.set(t, SettingTrashGrace, "0")
	if _, items := f.dry(t, rtFilm); !wouldRetire(items[0]) {
		t.Errorf("no trash: %s", said(items[0]))
	}
	f.kept(t)
}

// A list retires an original only while the retire job runs no pass: it
// waits for the job's lock, and retires once the pass is over.
func TestAListWaitsForTheRetireJobsPass(t *testing.T) {
	f := newListFixture(t)
	ctx := context.Background()
	dry, _ := f.dry(t, rtFilm)
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
	f.onSleep = func(d time.Duration) {
		if d == f.l.lockWait {
			if waited++; waited == 3 {
				if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+retireLockKey+`)`); err != nil {
					t.Error(err)
				}
			}
		}
	}
	if _, items := f.retire(t, dry.ID); said(items[0]) != "retired: " || waited != 3 {
		t.Errorf("the list once the pass is over: %s, waited %d times", said(items[0]), waited)
	}
}

// A run retires a title only as its dry run found it: the same original, the
// same version, and what retiring it gives up the same - what the package
// doesn't carry of the original, and the files that go with it into the
// trash. A title that changed since is skipped, saying how, before anything
// is claimed (the run's check) or right before the event (the retire path
// asks again), and nothing of it is touched.
func TestARunRetiresOnlyWhatItsDryRunReviewed(t *testing.T) {
	const (
		s2 = "4d5e6f7a-8b9c-4d0e-8f1a-2b3c4d5e6f7a"
		v2 = "5e6f7a8b-9c0d-4e1f-8a2b-3c4d5e6f7a8b"
		p2 = "6f7a8b9c-0d1e-4f2a-8b3c-4d5e6f7a8b9c"
	)
	// another original, its own record, and a complete version of it,
	// verified, the version the dry run checked superseded
	anotherOriginal := func(t *testing.T, f *listFixture) {
		folder := filepath.Join(f.p.Arrivals, "Example Film (2020) again")
		original := filepath.Join(folder, "Example Film.mkv")
		librarytest.Write(t, original, bytes.Repeat([]byte("a newer file "), 30000))
		size, qh1, err := QH1(original)
		if err != nil {
			t.Fatal(err)
		}
		sourceDir := SourceDir(f.itemDir, s2)
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath,
				sizebytes, qh1, state, recordedat, recorddir)
			VALUES ($1, $2, 'Example Film.mkv', $3, 'Example Film (2020) again/Example Film.mkv', $4, $5, 'present',
				now() - interval '1 hour', $6)`, s2, rtFilm, original, size, qh1, sourceDir)
		versionDir := VersionDir(f.itemDir, v2)
		librarytest.WriteVersion(t, versionDir, librarytest.Version{VersionID: v2, PackageID: p2, SourceIDs: []string{s2},
			CreatedAt: "2026-10-27T06:00:00Z", Package: map[string]any{"essence": f.pkg}})
		librarytest.WriteSource(t, sourceDir, map[string]any{"sourceId": s2, "file": map[string]any{"name": "Example Film.mkv",
			"sizeBytes": size, "fixity": map[string]any{"qh1": qh1}}, "sidecars": []any{}, "essence": f.src}, nil)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2,
			supersededat = now() WHERE id = $1`, rtVersion, v2)
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir,
				completedat, verifiedat, verifiedlevel)
			VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '30 minutes', now(), 'full')`,
			v2, rtFilm, s2, p2, versionDir)
	}
	// a newer version of the same original, verified, whose package lacks
	// the German subtitles
	newerVersion := func(t *testing.T, f *listFixture) {
		f.newer(t, v2, p2, map[string]any{"surround": true, "maxAudioChannels": 6, "audioLanguages": []string{"en"},
			"subtitleLanguages": []string{"en"}})
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = 'full'
			WHERE id = $1`, v2)
	}
	// the package of the version the dry run checked lacks the German
	// subtitles since
	lessCarried := func(t *testing.T, f *listFixture) {
		librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
			SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"essence": map[string]any{
				"surround": true, "maxAudioChannels": 6, "audioLanguages": []string{"en"}, "subtitleLanguages": []string{"en"}}}})
	}
	// another title's row names the German subtitle file since: it stays
	otherFiles := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang)
			VALUES ('shared-de', $1, $2, 'srt', 'de')`, ltFilm, f.de)
	}
	const original = "skipped: it changed since the dry run: its original is " + s2 + " now, and the dry run checked " + rtSource
	const version = "skipped: it changed since the dry run: its version is " + v2 + " now, and the dry run checked " + rtVersion
	const loss = "skipped: it changed since the dry run: what retiring it gives up is not what the dry run showed; now "
	for _, c := range []struct {
		name   string
		call   int // 0: before the run; 2: as the retire path asks again, right before the event
		change func(t *testing.T, f *listFixture)
		want   string // the reason, {de}, {en} and {nfo} its sidecars
	}{
		{"another original and version", 0, anotherOriginal, original},
		{"a newer version of the original", 0, newerVersion, version},
		{"its package carrying less", 0, lessCarried, loss + "what its package doesn’t carry of it: maxAudioChannels, " +
			"subtitleLanguages:de; the files that go with it: {de}, {en}, {nfo}"},
		{"other files going with it", 0, otherFiles, loss + "what its package doesn’t carry of it: maxAudioChannels; " +
			"the files that go with it: {en}, {nfo}"},
		{"a newer version of the original at the last", 2, newerVersion, version},
		{"its package carrying less at the last", 2, lessCarried, loss + "what its package doesn’t carry of it: " +
			"maxAudioChannels, subtitleLanguages:de; the files that go with it: {de}, {en}, {nfo}"},
		{"other files going with it at the last", 2, otherFiles, loss + "what its package doesn’t carry of it: " +
			"maxAudioChannels; the files that go with it: {en}, {nfo}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newListFixture(t)
			f.addOther(t)
			dry, items := f.dry(t, rtFilm)
			if !wouldRetire(items[0]) || deref(items[0].VersionID) != rtVersion {
				t.Fatalf("the dry run: %s, version %s", said(items[0]), deref(items[0].VersionID))
			}
			calls := 0
			if c.call == 0 {
				c.change(t, f)
			} else {
				f.gate.turn = func(string) (string, bool) {
					if calls++; calls == c.call {
						c.change(t, f)
					}
					return "", false
				}
			}
			_, items = f.retire(t, dry.ID)
			if want := strings.NewReplacer("{de}", f.de, "{en}", f.en, "{nfo}", f.nfo).Replace(c.want); said(items[0]) != want {
				t.Errorf("%s\nwant %s", said(items[0]), want)
			}
			if c.call > 0 && calls < c.call {
				t.Errorf("the compliance check was asked %d times: nothing changed", calls)
			}
			f.kept(t)
			if c.name == "another original and version" {
				if s := f.source(t, s2); s.State != SourcePresent || !exists(deref(s.ArrivalPath)) {
					t.Errorf("the original the dry run never checked: %s", s.State)
				}
			}
		})
	}
}

// What a dry run found holds for library.retire.list.maxAge (24h): a run of an
// older dry run is refused, and a title its dry run checked longer ago is
// skipped; with the setting longer, the same dry run is retired from.
func TestADryRunsWordHoldsForItsMaxAge(t *testing.T) {
	ctx := context.Background()
	f := newListFixture(t)
	dry, _ := f.dry(t, rtFilm)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_retireruns SET startedat = now() - interval '25 hours',
		finishedat = now() - interval '25 hours' WHERE id = $1`, dry.ID)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_retirerunitems SET at = now() - interval '25 hours' WHERE run_id = $1`,
		dry.ID)
	var refused *ListRefused
	if _, err := f.l.Start(ctx, ListRequest{FromRun: dry.ID, By: "an-admin"}); !errors.As(err, &refused) ||
		refused.Code != "FROM_RUN" || refused.Msg != "dry run "+dry.ID+" finished more than library.retire.list.maxAge (24h) "+
		"ago: what it found holds no more" {
		t.Fatalf("a run of a dry run 25 hours old: %v", err)
	}
	f.set(t, SettingRetireListMaxAge, "2d")
	if _, items := f.retire(t, dry.ID); said(items[0]) != "retired: " {
		t.Errorf("with library.retire.list.maxAge 2d: %s", said(items[0]))
	}

	// a title its dry run checked longer ago than the setting allows
	g := newListFixture(t)
	dry, _ = g.dry(t, rtFilm)
	run, err := g.l.Start(ctx, ListRequest{FromRun: dry.ID, By: "an-admin"})
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, g.st, `UPDATE com_nalet_katalog_retirerunitems SET reviewed_at = now() - interval '25 hours'
		WHERE run_id = $1`, run.ID)
	if err := g.l.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if _, items := g.latest(t); said(items[0]) != "skipped: its dry run checked it more than library.retire.list.maxAge "+
		"(24h) ago" {
		t.Errorf("a title its dry run checked 25 hours ago: %s", said(items[0]))
	}
	g.kept(t)
}
