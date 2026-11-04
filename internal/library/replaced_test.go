package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// rtNewSource is the source of the file the fixture's film is given in place
// of its original.
const rtNewSource = "1c7d8e9f-0a1b-4c2d-8e3f-4a5b6c7d8e9f"

const rtReplacer = "addon-1 (client example-addon, an addon (zaentrum-addon))"

// replaced has the fixture's film given another file as replaceSource gives
// it one with the v2 layout — its asset names a new file in the arrivals, a
// source of its own —, its original in the folder of its version, and the
// title held and the policy keeping originals, which hold no replace back.
// It answers where the old original lies.
func (f *retireFixture) replaced(t *testing.T) string {
	t.Helper()
	old := f.intoVersion(t, rtVersion)
	cur := filepath.Join(f.p.Arrivals, "Example Film 2160p.mkv")
	librarytest.Write(t, cur, bytes.Repeat([]byte("new "), 50000))
	size, qh1, err := QH1(cur)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, qh1, state)
		VALUES ($1, $2, 'Example Film 2160p.mkv', $3, $4, $5, 'present')`, rtNewSource, rtFilm, cur, size, qh1)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_playbackassets SET path = $2, sourceid = $3 WHERE id = $1`,
		"orig-"+rtFilm[:8], cur, rtNewSource)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, rtFilm)
	f.set(t, SettingOriginals, OriginalsKeep)
	return old
}

// keptAsTheTitles fails unless what replaceSource gave the title and what
// came with its old original are as they were: the asset names the new
// file, the subtitle files lie where they arrived and their rows name them,
// and the title's retire step is untouched.
func (f *retireFixture) keptAsTheTitles(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var path, source string
	if err := f.st.Pool().QueryRow(ctx, `SELECT path, sourceid FROM com_nalet_katalog_playbackassets WHERE id = $1`,
		"orig-"+rtFilm[:8]).Scan(&path, &source); err != nil || source != rtNewSource || !strings.HasSuffix(path, "Example Film 2160p.mkv") {
		t.Errorf("the title's file: %s %s, %v", path, source, err)
	}
	for _, p := range []string{f.en, f.de, f.nfo} {
		if !exists(p) {
			t.Errorf("%s left where it arrived", p)
		}
	}
	for id, want := range map[string]string{"en-" + rtFilm[:8]: f.en, "de-" + rtFilm[:8]: f.de} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, `SELECT path FROM com_nalet_katalog_subtitleassets WHERE id = $1`, id).Scan(&got); err != nil ||
			got != want {
			t.Errorf("subtitle row %s names %s, %v; want %s", id, got, err, want)
		}
	}
	if step := f.step(t, rtFilm); step != "none" {
		t.Errorf("the title's retire step: %s", step)
	}
	if s := f.source(t, rtNewSource); s.State != SourcePresent {
		t.Errorf("the new source: %+v", s)
	}
}

// The original a title was given another file in place of, in its version's
// folder, goes the way the retire job takes one, whatever holds a
// retirement back (the title held, originals kept): into the trash, as
// .work/trash/<day>/<sourceId>/original.mkv, its source deleted by whoever
// asked the replace, its original-deleted event recorded saying it was
// replaced. The subtitle files that came with it, the title's file and its
// retire step are left as they are; the version's folder keeps its records
// and verifies. A pass of the retire job leaves it in the trash until the
// trash's grace is over after its day, and purges it then.
func TestAReplacedOriginalGoesIntoTheTrash(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	old := f.replaced(t)
	release, err := HoldRetire(ctx, f.st.Pool(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	by := ReplacedBy(rtReplacer)
	tr, err := f.r.TrashReplaced(ctx, rtFilm, old, by)
	release()
	if err != nil {
		t.Fatal(err)
	}
	s := f.source(t, rtSource)
	if s.State != SourceDeleted || s.ArrivalPath != nil || deref(s.DeletedBy) != by || s.DeletedAt == nil || s.RetireEventAt == nil ||
		deref(s.RetireEventID) != tr.EventID {
		t.Fatalf("the old source: %+v", s)
	}
	trash := f.p.TrashDir(*s.RetireEventAt, rtSource)
	if tr.SourceID != rtSource || tr.VersionID != rtVersion || tr.Trash != trash || deref(s.TrashPath) != trash {
		t.Errorf("trashed: %+v, its row's trash %s, want %s", tr, deref(s.TrashPath), trash)
	}
	if got := files(t, trash); strings.Join(got, "|") != "original.mkv" {
		t.Errorf("the trash holds %v, want the original alone", got)
	}
	if exists(old) || !exists(filepath.Join(f.versionDir, VersionFile)) || !exists(filepath.Join(f.versionDir, CompleteFile)) {
		t.Errorf("the version's folder: original %v, its record %v", exists(old), exists(filepath.Join(f.versionDir, VersionFile)))
	}
	if _, err := VerifyVersion(f.versionDir, true); err != nil {
		t.Errorf("the version does not verify once its original went: %v", err)
	}
	dir, err := FindEvent(f.itemDir, tr.EventID, EventOriginalDeleted)
	if err != nil || dir == "" || !exists(filepath.Join(dir, SumsFile)) {
		t.Fatalf("the event: %q, %v", dir, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, EventFile))
	ev, err := DecodeDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"by": by, "reason": ReplacedReason, "versionId": rtVersion, "sourceId": rtSource,
		"kind": EventOriginalDeleted} {
		if got, _ := ev.Get(k); got != want {
			t.Errorf("the event's %s = %v, want %v", k, got, want)
		}
	}
	f.keptAsTheTitles(t)
	day := time.Date(s.RetireEventAt.Year(), s.RetireEventAt.Month(), s.RetireEventAt.Day(), 0, 0, 0, 0, time.UTC)
	if want := day.Add(48 * time.Hour); !tr.PurgedFrom.Equal(want) {
		t.Errorf("purged from %s, want %s (its day's end and the grace of 24h)", tr.PurgedFrom, want)
	}

	if rep := f.pass(t); rep.Originals != 0 || rep.Purged != 0 || rep.Failed != 0 {
		t.Errorf("a pass within the grace: %+v", rep)
	}
	if !exists(filepath.Join(trash, "original.mkv")) {
		t.Error("the original left the trash within its grace")
	}
	f.r.now = func() time.Time { return tr.PurgedFrom.Add(time.Minute) }
	if rep := f.pass(t); rep.Purged != 1 || exists(trash) {
		t.Errorf("a pass once the grace is over: %+v, the trash there %v", rep, exists(trash))
	}
	f.keptAsTheTitles(t)
}

// A replace stopped between its steps is finished by the retire job the way
// the replace takes it, not the way a retirement is held back: claimed, its
// event not recorded yet (known by the replace its row names), and with its
// event recorded, the original not moved yet. Either way the original goes
// into the trash, its subtitle files and the title's retire step left alone.
func TestAReplacedOriginalLeftRetiringIsFinishedByTheRetireJob(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		name := "claimed"
		if recorded {
			name = "its event recorded"
		}
		t.Run(name, func(t *testing.T) {
			f := newRetireFixture(t)
			ctx := context.Background()
			old := f.replaced(t)
			src, err := SourceByID(ctx, f.st.Pool(), rtSource)
			if err != nil {
				t.Fatal(err)
			}
			by := ReplacedBy(rtReplacer)
			rt := &retirement{src: src, auth: Authority{By: by, Reason: ReplacedReason}, itemDir: f.itemDir, replaced: true}
			if ok, err := f.r.claimReplaced(ctx, f.p, time.Now().UTC(), rt); err != nil || !ok {
				t.Fatalf("the claim: %v %v", ok, err)
			}
			if recorded {
				if err := f.r.recordReplaced(ctx, rt); err != nil {
					t.Fatal(err)
				}
			}
			if s := f.source(t, rtSource); s.State != SourceRetiring || deref(s.DeletedBy) != by || !exists(old) {
				t.Fatalf("stopped: %+v, the original there %v", s, exists(old))
			}

			if rep := f.pass(t); rep.Originals != 1 || rep.Failed != 0 {
				t.Fatalf("the pass: %+v", rep)
			}
			s := f.source(t, rtSource)
			if s.State != SourceDeleted || deref(s.DeletedBy) != by || exists(old) {
				t.Fatalf("finished: %+v, the original there %v", s, exists(old))
			}
			if got := files(t, f.p.TrashDir(*s.RetireEventAt, rtSource)); strings.Join(got, "|") != "original.mkv" {
				t.Errorf("the trash holds %v, want the original alone", got)
			}
			dir, err := FindEvent(f.itemDir, deref(s.RetireEventID), EventOriginalDeleted)
			if err != nil || dir == "" {
				t.Fatalf("the event: %q %v", dir, err)
			}
			b, _ := os.ReadFile(filepath.Join(dir, EventFile))
			if ev, err := DecodeDoc(b); err != nil {
				t.Fatal(err)
			} else if reason, _ := ev.Get("reason"); reason != ReplacedReason {
				t.Errorf("the event's reason: %v", reason)
			}
			f.keptAsTheTitles(t)
		})
	}
}

// With a trash grace of 0 a replaced original is deleted at once, as a
// retired one is, and recorded all the same.
func TestAReplacedOriginalWithoutATrashGraceIsDeleted(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	old := f.replaced(t)
	f.set(t, SettingTrashGrace, "0s")
	tr, err := f.r.TrashReplaced(ctx, rtFilm, old, ReplacedBy(rtReplacer))
	if err != nil {
		t.Fatal(err)
	}
	s := f.source(t, rtSource)
	if tr.Trash != "" || exists(old) || exists(f.p.TrashDir(*s.RetireEventAt, rtSource)) || s.State != SourceDeleted {
		t.Errorf("trashed %+v, the original there %v, the source %+v", tr, exists(old), s)
	}
	if dir, _ := FindEvent(f.itemDir, tr.EventID, EventOriginalDeleted); dir == "" {
		t.Error("no event recorded")
	}
}

// Only the title's present original in one of its version folders goes this
// way: a file of the arrivals, one of another title's folder and one no
// present source names are refused, nothing changed. The retire job's lock
// held elsewhere is waited for, then given up as busy.
func TestWhatIsNotTrashedAsReplaced(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	old := f.replaced(t)
	for name, path := range map[string]string{
		"a file of the arrivals":      filepath.Join(f.p.Arrivals, "Example Film 2160p.mkv"),
		"a file of the version":       filepath.Join(f.versionDir, PackageFile),
		"another title's version":     filepath.Join(f.p.MovieDir("7d1e2f3a-4b5c-4d6e-8f7a-9b0c1d2e3f4a"), "versions", rtVersion, "original.mkv"),
		"an original no source names": filepath.Join(VersionDir(f.itemDir, "6e1f2a3b-4c5d-4e6f-8a7b-9c0d1e2f3a4b"), "original.mkv"),
	} {
		if _, err := f.r.TrashReplaced(ctx, rtFilm, path, ReplacedBy(rtReplacer)); err == nil {
			t.Errorf("%s was taken as the replaced original", name)
		}
	}
	if s := f.source(t, rtSource); s.State != SourcePresent || !exists(old) {
		t.Errorf("refusals changed the original: %+v, there %v", s, exists(old))
	}

	release, err := HoldRetire(ctx, f.st.Pool(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := HoldRetire(ctx, f.st.Pool(), 600*time.Millisecond); !errors.Is(err, ErrRetireBusy) || time.Since(start) < 500*time.Millisecond {
		t.Errorf("the lock held elsewhere: %v after %s, want it waited for, then busy", err, time.Since(start))
	}
	release()
	again, err := HoldRetire(ctx, f.st.Pool(), time.Second)
	if err != nil {
		t.Fatalf("the lock once given up: %v", err)
	}
	again()
}

// The retire job's lock is a catalog's own, as every advisory lock of the
// service is: held for one catalog (its schema), it holds back that
// catalog's passes and replaces, not another's on the same database.
func TestTheRetireLockIsTheCatalogsOwn(t *testing.T) {
	ctx := context.Background()
	one, other := storetest.Open(t), storetest.Open(t)
	release, err := HoldRetire(ctx, one.Pool(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := HoldRetire(ctx, one.Pool(), 300*time.Millisecond); !errors.Is(err, ErrRetireBusy) {
		t.Errorf("the same catalog's lock held: %v, want busy", err)
	}
	theirs, err := HoldRetire(ctx, other.Pool(), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("another catalog's lock: %v", err)
	}
	theirs()
}
