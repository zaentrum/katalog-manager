package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The fixture's movie given another file: its new source and the version
// made of it.
const (
	rtNewSource  = "1c7d8e9f-0a1b-4c2d-8e3f-4a5b6c7d8e9f"
	rtNewVersion = "2b3f4a5b-6c7d-4e8f-9a0b-1c2d3e4f5a6b"
	rtNewPackage = "3a4b5c6d-7e8f-4a9b-8c0d-1e2f3a4b5c6d"
)

// supersede gives the fixture's movie another file, as replaceSource does
// with the library's v2 layout: its new original in the arrivals, a source
// of its own its asset names, the old original superseded by it where it
// lies. With complete, the version made of the new file is complete an hour
// ago, of a package whose essence is pkg, and the old version superseded by
// it: the title's master no longer needs the old original. The policy keeps
// originals: a superseded one goes on the replace's word alone.
func (f *retireFixture) supersede(t *testing.T, complete bool, pkg map[string]any) string {
	t.Helper()
	f.set(t, SettingOriginals, OriginalsKeep)
	f.set(t, SettingSupersededDelay, "10m") // the master below is complete an hour ago
	cur := filepath.Join(f.p.Arrivals, "Example Film 4K.mkv")
	librarytest.Write(t, cur, []byte(strings.Repeat("a better file ", 30000)))
	size, qh1, err := QH1(cur)
	if err != nil {
		t.Fatal(err)
	}
	f.execSQL(t, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath, sizebytes, qh1, state)
		VALUES ($1, $2, 'Example Film 4K.mkv', $3, 'Example Film 4K.mkv', $4, $5, 'present')`, rtNewSource, rtFilm, cur, size, qh1)
	f.execSQL(t, `UPDATE com_nalet_katalog_playbackassets SET path = $2, sizebytes = $3, sourceid = $4
		WHERE item_id = $1 AND kind = 'primary'`, rtFilm, cur, size, rtNewSource)
	if err := Supersede(context.Background(), f.st.Pool(), rtSource, rtNewSource); err != nil {
		t.Fatal(err)
	}
	if complete {
		f.complete(t, pkg)
	}
	return cur
}

// complete makes the version of the new file complete, an hour ago, of a
// package whose essence is pkg, and the old version superseded by it.
func (f *retireFixture) complete(t *testing.T, pkg map[string]any) {
	t.Helper()
	dir := VersionDir(f.itemDir, rtNewVersion)
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: rtNewVersion, PackageID: rtNewPackage,
		SourceIDs: []string{rtNewSource}, CreatedAt: "2026-11-04T10:00:00Z", Package: map[string]any{"essence": pkg}})
	f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2, supersededat = now()
		WHERE id = $1`, rtVersion, rtNewVersion)
	f.execSQL(t, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`, rtNewVersion, rtFilm, rtNewSource,
		rtNewPackage, dir)
}

// superseded checks that the fixture's old original is kept, superseded:
// where it lies, its sidecars too, no event recorded, the trash empty, and
// its row superseded by the new file's source.
func (f *retireFixture) superseded(t *testing.T, what string) *Source {
	t.Helper()
	for _, path := range []string{f.original, f.en, f.de, f.nfo} {
		if !exists(path) {
			t.Errorf("%s: %s is gone", what, path)
		}
	}
	s := f.source(t, rtSource)
	if s.State != SourceSuperseded || s.RetireEventID != nil || s.TrashPath != nil || deref(s.ArrivalPath) != f.original {
		t.Errorf("%s: the old source is %s, event %v, trash %v, at %v; want superseded where it lies", what, s.State,
			s.RetireEventID, s.TrashPath, s.ArrivalPath)
	}
	if by, err := SupersededBy(context.Background(), f.st.Pool(), rtSource); err != nil || by != rtNewSource {
		t.Errorf("%s: superseded by %q, %v; want %s", what, by, err, rtNewSource)
	}
	if evs := f.originalDeletedEvents(t); len(evs) > 0 {
		t.Errorf("%s: original-deleted events recorded: %v", what, evs)
	}
	if got := files(t, filepath.Join(f.p.Work, WorkTrash)); len(got) > 0 {
		t.Errorf("%s: the trash holds %v", what, got)
	}
	return s
}

// originalDeletedEvents are the original-deleted events the fixture's
// title's folder records.
func (f *retireFixture) originalDeletedEvents(t *testing.T) []Doc {
	t.Helper()
	evs, err := ReadEvents(f.itemDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []Doc
	for _, ev := range evs {
		if kind, _ := ev.Get("kind"); kind == EventOriginalDeleted {
			out = append(out, ev)
		}
	}
	return out
}

// An original another file took the place of stays, superseded, while the
// title's master is still made of it (the new file's version not complete
// yet); once the version made of the new file is complete and verified, the
// retire job retires it, whatever library.originals says: verified, its
// original-deleted event recorded — naming the version made of it, with what
// that version's package does not carry of it, by the retire job, saying the
// master took its place —, the file alone moved into the trash, its subtitle
// files staying the title's where they are, its source deleted, the title's
// retire step left alone, and the master verified in full. The new original
// and its sidecars are not touched.
func TestASupersededOriginalIsRetiredOnceItsTitlesMasterIsVerified(t *testing.T) {
	f := newRetireFixture(t)
	cur := f.supersede(t, false, withFiveOne)
	if rep := f.pass(t); rep != (Report{}) {
		t.Errorf("a pass while the master is made of the old original: %+v", rep)
	}
	f.superseded(t, "the master made of it")

	f.complete(t, withFiveOne)
	if rep := f.pass(t); rep.Superseded != 1 || rep.Originals != 0 || rep.Failed != 0 {
		t.Fatalf("the pass once the master is complete: %+v", rep)
	}
	s := f.source(t, rtSource)
	if s.State != SourceDeleted || s.ArrivalPath != nil || s.DeletedAt == nil || deref(s.DeletedBy) != SupersededRetiredBy ||
		s.RetireEventAt == nil {
		t.Fatalf("the old source: %+v", s)
	}
	trash := f.p.TrashDir(*s.RetireEventAt, rtSource)
	if got := files(t, trash); len(got) != 1 || got[0] != "Example Film.mkv" {
		t.Errorf("the trash holds %v, want the old original alone", got)
	}
	for _, kept := range []string{f.en, f.de, f.nfo, cur} {
		if !exists(kept) {
			t.Errorf("%s is gone", kept)
		}
	}
	if exists(f.original) {
		t.Error("the old original is still where it lay")
	}
	evs := f.originalDeletedEvents(t)
	if len(evs) != 1 {
		t.Fatalf("original-deleted events: %v", evs)
	}
	ev := evs[0]
	vid, _ := ev.Get("versionId")
	sid, _ := ev.Get("sourceId")
	by, _ := ev.Get("by")
	reason, _ := ev.Get("reason")
	acc, _ := ev.Get("accepted")
	if vid != rtVersion || sid != rtSource || by != SupersededRetiredBy || reason != supersededReason(rtNewSource, rtNewVersion) ||
		strings.Join(stringsOf(acc), ",") != "maxAudioChannels" {
		t.Errorf("the event: %v", ev)
	}
	if step := f.step(t, rtFilm); step != "none" {
		t.Errorf("the title's retire step: %s; want it left alone", step)
	}
	if n := countOf(t, f, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND verifiedlevel = 'full'`,
		rtNewVersion); n != 1 {
		t.Error("the master was not verified in full")
	}
	if n := countOf(t, f, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND state = 'present'
		AND arrivalpath = $2`, rtNewSource, cur); n != 1 {
		t.Error("the new original is not present where it is")
	}
	if n := countOf(t, f, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND path IN ($2, $3)`,
		rtFilm, f.en, f.de); n != 2 {
		t.Error("the subtitle files beside the old original are not the title's where they are")
	}
	// The trash keeps it for its grace, then its day goes.
	f.r.now = func() time.Time { return s.RetireEventAt.Add(49 * time.Hour) }
	f.pass(t)
	if exists(trash) {
		t.Error("the trash's day outlived its grace")
	}
}

// passKeeping runs a pass that may refuse what it retires, saying why (a
// retireFailed), as it refuses an original it keeps: what it did.
func (f *retireFixture) passKeeping(t *testing.T) Report {
	t.Helper()
	rep, err := f.r.Pass(context.Background())
	var refused *retireFailed
	if err != nil && !errors.As(err, &refused) {
		t.Fatalf("the pass: %v", err)
	}
	return rep
}

// settings are the fixture's library settings as the catalog holds them.
func (f *retireFixture) settings(t *testing.T) Settings {
	t.Helper()
	set, err := ReadSettings(context.Background(), f.st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// countOf counts with sql on the fixture's catalog.
func countOf(t *testing.T, f *retireFixture, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A superseded original is retired only as an original is: it is kept,
// superseded, while the title holds its originals, while a step that reads
// an original runs, while its master's package lacks its surround (the pass
// counts it held, its row saying why), while its master does not verify, its
// file changed since it was recorded, its record does not verify, or a
// title's file is the file still — each failure noted on its row, the title's
// retire step left alone — and while the master is complete for less than
// library.retire.delay.
func TestASupersededOriginalWaitsForItsConditions(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		pkg    map[string]any
		before func(t *testing.T, f *retireFixture)
		says   string
		held   bool
	}{
		{name: "held", pkg: withFiveOne, before: func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
		}},
		{name: "a step reads an original", pkg: withFiveOne, before: func(t *testing.T, f *retireFixture) {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "subtitle", processing.StatusInProgress, nil, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "its surround lost", pkg: stereo, held: true, says: SurroundHeld + " (version " + rtNewVersion + ")"},
		{name: "the master does not verify", pkg: withFiveOne, says: "the title's master, version " + rtNewVersion + ", does not verify",
			before: func(t *testing.T, f *retireFixture) {
				librarytest.Write(t, filepath.Join(VersionDir(f.itemDir, rtNewVersion), "hls/v0/seg0.m4s"), []byte("damaged"))
			}},
		{name: "the file changed", pkg: withFiveOne, says: "the original changed since it was recorded",
			before: func(t *testing.T, f *retireFixture) {
				librarytest.Write(t, f.original, []byte(strings.Repeat("another file ", 30000)))
			}},
		{name: "its record does not verify", pkg: withFiveOne, says: "the source's record does not verify",
			before: func(t *testing.T, f *retireFixture) {
				librarytest.Write(t, filepath.Join(f.sourceDir, "Example Film.nfo"), []byte("<movie>changed</movie>\n"))
			}},
		{name: "a title's file still", pkg: withFiveOne, says: "is a file of item " + rtFilm + " still",
			before: func(t *testing.T, f *retireFixture) {
				f.execSQL(t, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind)
					VALUES ('x-old', $1, $2, false, 'original')`, rtFilm, f.original)
			}},
		{name: "the master complete just now", pkg: withFiveOne, before: func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET completedat = now() WHERE id = $1`, rtNewVersion)
		}},
		{name: "the master complete for less than library.superseded.delay", pkg: withFiveOne,
			before: func(t *testing.T, f *retireFixture) {
				f.set(t, SettingSupersededDelay, "7d")
				f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '6 days 23 hours'
					WHERE id = $1`, rtNewVersion)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sevenOne, withFiveOne)
			f.supersede(t, true, c.pkg)
			if c.before != nil {
				c.before(t, f)
			}
			rep := f.passKeeping(t)
			if rep.Superseded != 0 || (c.held && rep.Held != 1) {
				t.Errorf("the pass: %+v", rep)
			}
			s := f.superseded(t, c.name)
			if c.says != "" && !strings.Contains(deref(s.Error), c.says) {
				t.Errorf("its row says %q, want %q", deref(s.Error), c.says)
			}
			if c.says == "" && s.Error != nil {
				t.Errorf("its row says %q, want nothing", *s.Error)
			}
			if step := f.step(t, rtFilm); step != "none" {
				t.Errorf("the title's retire step: %s; want it left alone", step)
			}
			// One looked at in vain waits an hour before it is again.
			if c.says != "" {
				f.r.lockWait = 0
				if ids, err := f.r.supersededDue(ctx, f.settings(t), time.Now(), 10); err != nil || len(ids) != 0 {
					t.Errorf("due again at once: %v, %v", ids, err)
				}
				if ids, err := f.r.supersededDue(ctx, f.settings(t), time.Now().Add(61*time.Minute), 10); err != nil || len(ids) != 1 {
					t.Errorf("due an hour later: %v, %v", ids, err)
				}
			}
		})
	}
}

// A superseded original's retirement stopped by a crash resumes where it
// stopped, as a superseded original's, not a retirement by the policy: one
// claimed whose event is not recorded is checked again and retired; one whose
// event is recorded is finished, its file moved and its source deleted, by
// the retire job; and one called off meanwhile — the title holds its
// originals now — is superseded again, not present, the title's retire step
// left alone.
func TestASupersededOriginalsRetirementResumes(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, f *retireFixture) *retirement {
		t.Helper()
		s := f.source(t, rtSource)
		rt := &retirement{src: s, superseded: rtNewSource, itemDir: f.itemDir}
		var err error
		if rt.ver, err = f.r.masterOf(ctx, s); err != nil || rt.ver == nil {
			t.Fatalf("the master: %v", err)
		}
		if claimed, err := f.r.claimSuperseded(ctx, f.p, time.Now(), rt); err != nil || !claimed {
			t.Fatalf("the claim: %v, %v", claimed, err)
		}
		return rt
	}
	t.Run("claimed", func(t *testing.T) {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		claim(t, f)
		if rep := f.pass(t); rep.Originals+rep.Superseded != 1 || rep.Failed != 0 {
			t.Fatalf("the pass: %+v", rep)
		}
		if s := f.source(t, rtSource); s.State != SourceDeleted || deref(s.DeletedBy) != SupersededRetiredBy {
			t.Errorf("the old source: %s by %v", s.State, s.DeletedBy)
		}
		if len(f.originalDeletedEvents(t)) != 1 || exists(f.original) {
			t.Error("not retired as a superseded original")
		}
	})
	t.Run("its event recorded", func(t *testing.T) {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		rt := claim(t, f)
		s := rt.src
		reason := supersededReason(rtNewSource, rtNewVersion)
		if _, err := WriteEvent(f.itemDir, Event{ID: *s.RetireEventID, At: *s.RetireEventAt, By: SupersededRetiredBy,
			Kind: EventOriginalDeleted, VersionID: rtVersion, SourceID: rtSource, Reason: &reason, Accepted: []string{}}); err != nil {
			t.Fatal(err)
		}
		// the master would not verify now: past its event, that is not asked again
		librarytest.Write(t, filepath.Join(VersionDir(f.itemDir, rtNewVersion), "hls/v0/seg0.m4s"), []byte("damaged"))
		if rep := f.pass(t); rep.Originals+rep.Superseded != 1 || rep.Failed != 0 {
			t.Fatalf("the pass: %+v", rep)
		}
		if s := f.source(t, rtSource); s.State != SourceDeleted || exists(f.original) {
			t.Errorf("the old source: %s, its file there %v", s.State, exists(f.original))
		}
		if step := f.step(t, rtFilm); step != "none" {
			t.Errorf("the title's retire step: %s", step)
		}
	})
	t.Run("called off", func(t *testing.T) {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		claim(t, f)
		f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
		if rep := f.pass(t); rep.Originals+rep.Superseded != 0 || rep.Failed != 0 {
			t.Errorf("the pass: %+v", rep)
		}
		f.superseded(t, "called off")
		if step := f.step(t, rtFilm); step != "none" {
			t.Errorf("the title's retire step: %s; want it left alone", step)
		}
	})
	t.Run("failed before its event", func(t *testing.T) {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		claim(t, f)
		librarytest.Write(t, f.original, []byte(strings.Repeat("another file ", 30000)))
		f.passKeeping(t)
		if s := f.source(t, rtSource); s.State != SourceSuperseded || s.RetireEventID != nil ||
			!strings.Contains(deref(s.Error), "the original changed since it was recorded") {
			t.Errorf("the old source: %s, event %v, %v; want superseded again, saying why", s.State, s.RetireEventID, deref(s.Error))
		}
		if step := f.step(t, rtFilm); step != "none" {
			t.Errorf("the title's retire step: %s; want it left alone", step)
		}
	})
}

// An original replaced before any version was made of it has no record: it
// is retired once the title's master is verified with no event — nothing in
// the title's folder names it —, checked against what its row recorded, and
// its row says it is deleted. One stopped once its file left its place is
// finished with no check, its row deleted.
func TestASupersededOriginalWithNoRecordIsRetiredWithoutAnEvent(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET recordedat = NULL WHERE id = $1`, rtSource)
		if err := os.RemoveAll(f.sourceDir); err != nil {
			t.Fatal(err)
		}
		if stopped {
			ctx := context.Background()
			s := f.source(t, rtSource)
			rt := &retirement{src: s, superseded: rtNewSource, itemDir: f.itemDir}
			rt.ver, _ = f.r.masterOf(ctx, s)
			if claimed, err := f.r.claimSuperseded(ctx, f.p, time.Now(), rt); err != nil || !claimed {
				t.Fatalf("the claim: %v, %v", claimed, err)
			}
			librarytest.Write(t, filepath.Join(*rt.src.TrashPath, "Example Film.mkv"), []byte("moved"))
			if err := os.Remove(f.original); err != nil {
				t.Fatal(err)
			}
		}
		if rep := f.pass(t); rep.Originals+rep.Superseded != 1 || rep.Failed != 0 {
			t.Fatalf("stopped %v: the pass: %+v", stopped, rep)
		}
		if s := f.source(t, rtSource); s.State != SourceDeleted || s.ArrivalPath != nil {
			t.Errorf("stopped %v: the old source: %+v", stopped, s)
		}
		if evs, _ := ReadEvents(f.itemDir); len(evs) > 0 {
			t.Errorf("stopped %v: events recorded: %v", stopped, evs)
		}
		if exists(f.original) {
			t.Errorf("stopped %v: the old original is still where it lay", stopped)
		}
	}
}

// A superseded original in the folder of its version is retired so, its
// event naming that version; the version, superseded, is removed after its
// grace once its folder holds no original any more — in the pass that
// retires it —, as it is not before.
func TestASupersededOriginalInItsVersionsFolderIsRetired(t *testing.T) {
	f := newRetireFixture(t)
	f.supersede(t, true, withFiveOne)
	inVersion := filepath.Join(f.versionDir, "original.mkv")
	if err := os.Rename(f.original, inVersion); err != nil {
		t.Fatal(err)
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2, filename = 'original.mkv' WHERE id = $1`, rtSource, inVersion)
	f.set(t, SettingSupersededGrace, "0")
	// the old version waits while its folder holds the superseded original
	f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
	if rep := f.pass(t); rep.Versions != 0 || rep.Superseded != 0 {
		t.Errorf("a pass while the title is held: %+v", rep)
	}
	if !exists(inVersion) {
		t.Fatal("the superseded original left its version's folder")
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = false WHERE id = $1`, rtFilm)
	if rep := f.pass(t); rep.Superseded != 1 || rep.Versions != 1 {
		t.Fatalf("the pass: %+v; want the superseded original retired, then its version removed", rep)
	}
	evs := f.originalDeletedEvents(t)
	if len(evs) != 1 {
		t.Fatalf("events: %v", evs)
	}
	if vid, _ := evs[0].Get("versionId"); vid != rtVersion {
		t.Errorf("the event names version %v, want %s", vid, rtVersion)
	}
	if exists(inVersion) || exists(f.versionDir) {
		t.Error("the superseded original's version's folder is still there")
	}
	if n := countOf(t, f, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'removed'`, rtVersion); n != 1 {
		t.Error("the old version is not removed")
	}
}

// What makes a superseded original due, and what its claim holds it to,
// each on its own: a superseded source, of a title that does not hold its
// originals, none of whose steps that read an original runs, whose master —
// the title's own complete version — is not made of it and complete for the
// delay. A version of another title is no master of it.
func TestWhatIsDueAndClaimedOfASupersededOriginal(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		change func(t *testing.T, f *retireFixture)
	}{
		{"present, not superseded", func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET state = 'present' WHERE id = $1`, rtSource)
		}},
		{"held", func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
		}},
		{"a step reads an original", func(t *testing.T, f *retireFixture) {
			if err := processing.New(f.st.Pool()).Upsert(ctx, rtFilm, "chapter", processing.StatusPending, nil, nil); err != nil {
				t.Fatal(err)
			}
		}},
		{"its master made of it", func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET sourceids = sourceids || $2::varchar WHERE id = $1`,
				rtNewVersion, rtSource)
		}},
		{"its master another title's", func(t *testing.T, f *retireFixture) {
			storetest.AddItem(t, f.st, "7c7c7c7c-1111-4222-8333-444444444444", "movie", "Another Film", "")
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET item_id = '7c7c7c7c-1111-4222-8333-444444444444' WHERE id = $1`,
				rtNewVersion)
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'complete' WHERE id = $1`, rtVersion)
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET sourceids = '{}' WHERE id = $1`, rtVersion)
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, rtVersion)
		}},
		{"its master not complete", func(t *testing.T, f *retireFixture) {
			f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, rtNewVersion)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			f.supersede(t, true, withFiveOne)
			master, err := Current(ctx, f.st.Pool(), rtFilm)
			if err != nil || master == nil {
				t.Fatalf("the master: %v", err)
			}
			c.change(t, f)
			if ids, err := f.r.supersededDue(ctx, f.settings(t), time.Now(), 10); err != nil || len(ids) != 0 {
				t.Errorf("due: %v, %v; want none", ids, err)
			}
			rt := &retirement{src: f.source(t, rtSource), superseded: rtNewSource, itemDir: f.itemDir, ver: master}
			if claimed, err := f.r.claimSuperseded(ctx, f.p, time.Now(), rt); err != nil || claimed {
				t.Errorf("claimed: %v, %v; want it not", claimed, err)
			}
		})
	}
	// all of it holding, it is due and claimed
	f := newRetireFixture(t)
	f.supersede(t, true, withFiveOne)
	if ids, err := f.r.supersededDue(ctx, f.settings(t), time.Now(), 10); err != nil || len(ids) != 1 || ids[0] != rtSource {
		t.Fatalf("due: %v, %v", ids, err)
	}
	master, _ := Current(ctx, f.st.Pool(), rtFilm)
	rt := &retirement{src: f.source(t, rtSource), superseded: rtNewSource, itemDir: f.itemDir, ver: master}
	if claimed, err := f.r.claimSuperseded(ctx, f.p, time.Now(), rt); err != nil || !claimed || rt.src.State != SourceRetiring {
		t.Fatalf("claimed: %v, %v", claimed, err)
	}
	if by, _ := SupersededBy(ctx, f.st.Pool(), rtSource); by != rtNewSource {
		t.Errorf("claimed, it is superseded by %q still; want %s", by, rtNewSource)
	}
}

// A superseded original that lies outside every root the retire job deletes
// under (the arrivals, the extras' arrivals, the media root) and outside its
// version's folder, or under the package store — one inside the media root
// too —, is kept, its row saying why.
func TestASupersededOriginalOutsideTheRootsIsKept(t *testing.T) {
	for _, where := range []string{"elsewhere", "packages"} {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne)
		moved := filepath.Join(f.cfg.LibraryRoot, where, "Example Film.mkv")
		if where == "packages" {
			f.cfg.PackagesRoot = filepath.Join(f.cfg.NFSRoot, "packages")
			f.r = NewRetirer(f.st.Pool(), f.cfg, processing.New(f.st.Pool()))
			moved = filepath.Join(f.cfg.PackagesRoot, "Example Film.mkv")
		}
		if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(f.original, moved); err != nil {
			t.Fatal(err)
		}
		f.execSQL(t, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2 WHERE id = $1`, rtSource, moved)
		if rep := f.passKeeping(t); rep.Superseded != 0 {
			t.Errorf("%s: the pass: %+v", where, rep)
		}
		s := f.source(t, rtSource)
		if s.State != SourceSuperseded || !exists(moved) || !strings.Contains(deref(s.Error), "nothing is deleted") {
			t.Errorf("%s: the old source: %s, its file there %v, %q", where, s.State, exists(moved), deref(s.Error))
		}
	}
}

// The master of a superseded original is its title's complete version while
// that is not made of it; none while it is, or while the title has none.
func TestTheMasterOfASupersededOriginal(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	f.supersede(t, false, withFiveOne)
	s := f.source(t, rtSource)
	if v, err := f.r.masterOf(ctx, s); err != nil || v != nil {
		t.Errorf("while the complete version is made of it: %+v, %v", v, err)
	}
	f.complete(t, withFiveOne)
	if v, err := f.r.masterOf(ctx, s); err != nil || v == nil || v.ID != rtNewVersion {
		t.Errorf("once the new version is complete: %+v, %v", v, err)
	}
	f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, rtNewVersion)
	if v, err := f.r.masterOf(ctx, s); err != nil || v != nil {
		t.Errorf("with no complete version: %+v, %v", v, err)
	}
}

// rtThirdSource is a third file the fixture's movie is given, in place of the
// second.
const (
	rtThirdSource  = "4d8e9f0a-1b2c-4d3e-8f4a-5b6c7d8e9f0a"
	rtThirdVersion = "5e9f0a1b-2c3d-4e4f-8a5b-6c7d8e9f0a1b"
	rtThirdPackage = "6f0a1b2c-3d4e-4f5a-8b6c-7d8e9f0a1b2c"
)

// supersedeAgain gives the fixture's movie a third file in place of the
// second, as a second replace does: the second superseded by it where it
// lies. It answers the third file's path.
func (f *retireFixture) supersedeAgain(t *testing.T) string {
	t.Helper()
	cur := filepath.Join(f.p.Arrivals, "Example Film REMUX.mkv")
	librarytest.Write(t, cur, []byte(strings.Repeat("a third file ", 30000)))
	size, qh1, err := QH1(cur)
	if err != nil {
		t.Fatal(err)
	}
	f.execSQL(t, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath, sizebytes, qh1, state)
		VALUES ($1, $2, 'Example Film REMUX.mkv', $3, 'Example Film REMUX.mkv', $4, $5, 'present')`, rtThirdSource, rtFilm, cur, size, qh1)
	f.execSQL(t, `UPDATE com_nalet_katalog_playbackassets SET path = $2, sizebytes = $3, sourceid = $4
		WHERE item_id = $1 AND kind = 'primary'`, rtFilm, cur, size, rtThirdSource)
	if err := Supersede(context.Background(), f.st.Pool(), rtNewSource, rtThirdSource); err != nil {
		t.Fatal(err)
	}
	return cur
}

// The master of a superseded original is a complete version made of the
// latest file that took its place, the chain of files followed: a title given
// a file that was never encoded (a replace without a re-encode, or one whose
// re-encode failed), then another, keeps every file the latest took the place
// of while the latest has no complete version — whatever complete version it
// has, of an older file or of none of them —, and retires them once one made
// of the latest is complete.
func TestTheMasterIsMadeOfTheLatestFileThatTookItsPlace(t *testing.T) {
	ctx := context.Background()
	t.Run("the second file never encoded", func(t *testing.T) {
		f := newRetireFixture(t)
		second := f.supersede(t, false, withFiveOne)
		third := f.supersedeAgain(t)
		if latest, err := LatestOf(ctx, f.st.Pool(), rtSource); err != nil || latest != rtThirdSource {
			t.Fatalf("the latest file of the first: %q, %v", latest, err)
		}
		if rep := f.passKeeping(t); rep.Superseded != 0 {
			t.Errorf("a pass while the latest file has no version: %+v", rep)
		}
		for id, path := range map[string]string{rtSource: f.original, rtNewSource: second} {
			if s := f.source(t, id); s.State != SourceSuperseded || !exists(path) {
				t.Errorf("source %s: %s, its file there %v; want superseded, kept", id, s.State, exists(path))
			}
		}
		dir := VersionDir(f.itemDir, rtThirdVersion)
		librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: rtThirdVersion, PackageID: rtThirdPackage,
			SourceIDs: []string{rtThirdSource}, CreatedAt: "2026-11-04T12:00:00Z", Package: map[string]any{"essence": withFiveOne}})
		f.execSQL(t, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2, supersededat = now()
			WHERE id = $1`, rtVersion, rtThirdVersion)
		f.execSQL(t, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
			VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`, rtThirdVersion, rtFilm,
			rtThirdSource, rtThirdPackage, dir)
		f.passKeeping(t)
		if s := f.source(t, rtSource); s.State != SourceDeleted || exists(f.original) {
			t.Errorf("the first original once the third file's version is complete: %s, there %v", s.State, exists(f.original))
		}
		if !exists(third) {
			t.Error("the latest file went")
		}
	})
	t.Run("the second file encoded, the third not", func(t *testing.T) {
		f := newRetireFixture(t)
		f.supersede(t, true, withFiveOne) // the second file's version complete: the master of the first, until the third
		f.supersedeAgain(t)
		if rep := f.passKeeping(t); rep.Superseded != 0 {
			t.Errorf("a pass while the latest file has no version: %+v", rep)
		}
		f.superseded(t, "the latest file not encoded")
		master, _ := Current(ctx, f.st.Pool(), rtFilm)
		rt := &retirement{src: f.source(t, rtSource), superseded: rtNewSource, itemDir: f.itemDir, ver: master}
		if claimed, err := f.r.claimSuperseded(ctx, f.p, time.Now(), rt); err != nil || claimed {
			t.Errorf("claimed against a version of the second file: %v, %v; want it not", claimed, err)
		}
		if v, err := f.r.masterOf(ctx, f.source(t, rtSource)); err != nil || v != nil {
			t.Errorf("the master of the first: %+v, %v; want none", v, err)
		}
	})
}
