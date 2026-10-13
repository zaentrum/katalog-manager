package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A film the run stages beside the store before it, as the tool does with
// --originals link --checksums pending: its original and the subtitle file
// beside it linked, not moved, and its package's checksums pending.
const (
	mgBeside    = "a3a3a3a3-0000-4000-8000-00000000003a"
	mgBesideOld = "media/Beside (2023)/Beside (2023).mkv"
)

// newBesideFixture is the migration fixture with that film.
func newBesideFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := newMigrationFixture(t)
	storetest.AddItem(t, f.st, mgBeside, "movie", "Film Beside", "")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2026-10-01 09:00:00.25' WHERE id = $1`, mgBeside)
	f.into, f.beside = true, true
	f.packaged(t, mgBeside, mgBesideOld, false)
	return f
}

// one reports whether the two names are there and are the one file.
func one(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// An adopt of a plan staged beside the store before it gives each original a
// second name in its version's folder and each subtitle file beside it one
// among the arrivals, and leaves the first where the store before plays it:
// the catalog holds what a move leaves, and the version, whose package's
// checksums are pending, is complete and not verified. Its chain holds as far
// as it goes. A revert takes the second names away again and nothing else.
func TestAnAdoptBesideTheStoreBeforeLinksItsOriginals(t *testing.T) {
	f := newBesideFixture(t)
	ctx := context.Background()
	ids := []string{mgBeside}
	before, tree := f.stateOf(t, ids...), f.tree(t)
	rep, err := f.migration(t).Adopt(ctx, ids)
	if err != nil || rep.Adopted != 1 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	old := filepath.Join(f.dir, mgBesideOld)
	oldSub := strings.TrimSuffix(old, ".mkv") + ".en.srt"
	vdir := VersionDir(f.itemDir(mgBeside), IDOf(mgBeside+":version"))
	into := filepath.Join(vdir, "original.mkv")
	sub := filepath.Join(f.p.Arrivals, "Beside (2023)", "Beside (2023).en.srt")
	if !one(old, into) || !one(oldSub, sub) {
		t.Fatalf("the original and its version's name one file: %v; the subtitle file and its arrival: %v", one(old, into), one(oldSub, sub))
	}
	for sql, want := range map[string]string{
		`SELECT s.arrivalpath || ' ' || a.path || ' ' || a.kind FROM com_nalet_katalog_itemsources s
			JOIN com_nalet_katalog_playbackassets a ON a.sourceid = s.id WHERE s.item_id = '` + mgBeside + `'`: into + " " + into + " primary",
		`SELECT state || ' ' || (verifiedat IS NULL) || ' ' || (verifiedlevel IS NULL)
			FROM com_nalet_katalog_itemversions WHERE item_id = '` + mgBeside + `'`: "complete true true",
		`SELECT path FROM com_nalet_katalog_subtitleassets WHERE id = 'se-` + mgBeside[:4] + `'`: sub,
		// what the plan says the record probed of the original, which the catalog before never knew
		`SELECT codec || ' ' || resolution || ' ' || bitratekbps || ' ' || durationms FROM com_nalet_katalog_playbackassets
			WHERE id = 'pa-` + mgBeside[:4] + `'`: "hevc 3840x2160 6053 475057",
	} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, sql).Scan(&got); err != nil || got != want {
			t.Errorf("%s:\n got  %q, %v\n want %q", sql, got, err, want)
		}
	}
	c, err := VerifyVersion(vdir, true)
	if err != nil || !c.Pending || c.Listed != nil {
		t.Errorf("the pending chain: %+v, %v", c, err)
	}
	if statOK(filepath.Join(vdir, SumsFile)) {
		t.Error("a pending version has a checksums file")
	}

	rep, err = f.migration(t).Revert(ctx, ids)
	if err != nil || rep.Reverted != 1 || rep.Refused+rep.Failed != 0 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if got := f.stateOf(t, ids...); got != before {
		t.Errorf("the database after the revert:\n%s\nbefore the adopt:\n%s", got, before)
	}
	if got := f.tree(t); got != tree {
		t.Errorf("the tree after the revert:\n%s\nbefore the adopt:\n%s", got, tree)
	}
	if exists(into) || exists(sub) || !statOK(old) || !statOK(oldSub) {
		t.Error("the revert did not take the second names away, or took a first")
	}
	if rep, err := f.migration(t).Adopt(ctx, ids); err != nil || rep.Adopted != 1 || !one(old, into) {
		t.Errorf("Adopt after the revert: %+v, %v", rep, err)
	}
}

// A pending chain proves no byte, so the retire job deletes no original on
// it: the retirement fails saying so and the original stays. Once the tool
// filled the checksums, the catalog notes each version its journal names
// verified in full at the moment it was — only while the folder is what the
// tool left — and the retire job retires the original: its name in the
// version's folder goes, and the store before keeps its file. A revert then
// brings the name back from the trash and takes it away as a link's.
func TestAPendingVersionKeepsItsOriginalUntilItsChecksumsAreFilled(t *testing.T) {
	f := newBesideFixture(t)
	ctx := context.Background()
	ids := []string{mgBeside}
	if rep, err := f.migration(t).Adopt(ctx, ids); err != nil || rep.Adopted != 1 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	old := filepath.Join(f.dir, mgBesideOld)
	vid := IDOf(mgBeside + ":version")
	vdir := VersionDir(f.itemDir(mgBeside), vid)
	into := filepath.Join(vdir, "original.mkv")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package'), ('s3', 'library.retire.delay', '0')`)
	r := NewRetirer(f.st.Pool(), f.cfg, processing.New(f.st.Pool()))
	rep, err := r.Pass(ctx)
	if rep.Originals != 0 || rep.Failed != 1 || !one(old, into) {
		t.Fatalf("the retire pass over a pending version: %+v, %v", rep, err)
	}
	var why string
	if err := f.st.Pool().QueryRow(ctx, `SELECT state || ': ' || COALESCE(error, '') FROM com_nalet_katalog_itemsources
		WHERE item_id = $1`, mgBeside).Scan(&why); err != nil || !strings.HasPrefix(why, "present: ") ||
		!strings.Contains(why, "checksums are pending") || !strings.Contains(why, "the original is kept") {
		t.Fatalf("the source after it: %q, %v", why, err)
	}

	// Nothing to note before the tool ran; a journal that names a folder
	// still pending, or one changed since, notes nothing.
	m := f.migration(t)
	if rep, err := m.Filled(ctx, nil); err != nil || rep.Versions+rep.Unchanged+len(rep.Skipped) != 0 {
		t.Errorf("Filled without a journal: %+v, %v", rep, err)
	}
	rel := m.relOf(vdir)
	journal := func(complete string) {
		t.Helper()
		librarytest.Write(t, filepath.Join(f.runDir, ChecksumsJournal), []byte(
			`{"at": "2026-10-09T02:00:00Z", "op": "fill", "dir": "`+rel+`", "before": "sha256:00", "after": "`+complete+`"}`+"\n"+
				`{"at": "2026-10-09T02:00:05Z", "op": "filled", "dir": "`+rel+`", "itemId": "`+mgBeside+`", "versionId": "`+vid+
				`", "packageId": "`+IDOf(mgBeside+":package")+`", "files": 10, "bytes": 1}`+"\n"+
				`{"at": "2026-10-09T02:00:06Z", "op": "fill", "dir": "a line a crash cut sho`))
	}
	journal("sha256:11")
	if rep, err := m.Filled(ctx, nil); err != nil || rep.Versions != 0 || len(rep.Skipped) != 1 ||
		rep.Skipped[0].Reason != "its checksums are pending still" {
		t.Errorf("Filled over a pending folder: %+v, %v", rep, err)
	}
	complete := librarytest.Fill(t, vdir)
	if rep, err := m.Filled(ctx, nil); err != nil || rep.Versions != 0 || len(rep.Skipped) != 1 ||
		!strings.Contains(rep.Skipped[0].Reason, "is not the one the run wrote") {
		t.Errorf("Filled over a folder the journal did not write: %+v, %v", rep, err)
	}
	journal(complete)
	if rep, err := m.Filled(ctx, []string{mgFilm}); err != nil || rep.Versions+rep.Unchanged+len(rep.Skipped) != 0 {
		t.Errorf("Filled for another item: %+v, %v", rep, err)
	}
	if rep, err := m.Filled(ctx, nil); err != nil || rep.Versions != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("Filled: %+v, %v", rep, err)
	}
	var noted string
	if err := f.st.Pool().QueryRow(ctx, `SELECT verifiedlevel || ' ' || to_char(verifiedat AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
		FROM com_nalet_katalog_itemversions WHERE id = $1`, vid).Scan(&noted); err != nil || noted != "full 2026-10-09T02:00:05Z" {
		t.Errorf("the version after Filled: %q, %v", noted, err)
	}
	if rep, err := m.Filled(ctx, nil); err != nil || rep.Versions != 0 || rep.Unchanged != 1 {
		t.Errorf("Filled again: %+v, %v", rep, err)
	}
	if c, err := VerifyVersion(vdir, true); err != nil || c.Pending || len(c.Listed) == 0 {
		t.Errorf("the filled chain: %+v, %v", c, err)
	}

	// The step's retry is not due yet: an admin's retry has the job look now.
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, mgBeside)
	if rep, err := r.Pass(ctx); err != nil || rep.Originals != 1 {
		t.Fatalf("the retire pass once filled: %+v, %v", rep, err)
	}
	if exists(into) || !statOK(old) {
		t.Fatalf("after the retire: the version's name there %v, the store's before %v", exists(into), statOK(old))
	}
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_settings`)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_itemprocessingsteps`)
	if rep, err := f.migration(t).Revert(ctx, ids); err != nil || rep.Reverted != 1 || rep.Refused+rep.Failed != 0 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if !statOK(old) || exists(into) || exists(f.itemDir(mgBeside)) {
		t.Fatalf("after the revert: the store's before there %v, the version's name %v, the item's folder %v",
			statOK(old), exists(into), exists(f.itemDir(mgBeside)))
	}
	if trash, _ := filepath.Glob(filepath.Join(f.p.Work, WorkTrash, "*", "*", "original.mkv")); len(trash) != 0 {
		t.Errorf("the trash still holds a name of the original: %v", trash)
	}
}

// A unit whose later move fails puts back what it did: a link's second name
// is taken away and its first stays. A plan that links from anywhere but the
// store before the library, or to anywhere a move may not go, is refused;
// and a link whose first name was removed since is reverted by putting the
// file back there.
func TestALinkIsUndoneByItsSecondName(t *testing.T) {
	f := newBesideFixture(t)
	ctx := context.Background()
	ids := []string{mgBeside}
	old := filepath.Join(f.dir, mgBesideOld)
	into := filepath.Join(VersionDir(f.itemDir(mgBeside), IDOf(mgBeside+":version")), "original.mkv")
	legacy := filepath.Join(f.runDir, "legacy", "movies", mgBeside[:2], mgBeside)
	tree := f.tree(t)
	librarytest.Write(t, filepath.Join(legacy, "in-the-way"), []byte("x"))
	rep, err := f.migration(t).Adopt(ctx, ids)
	if err != nil || rep.Failed != 1 || !strings.Contains(rep.Units[0].Reason, "its renames are put back") {
		t.Fatalf("Adopt with the last move in the way: %+v, %v", rep, err)
	}
	if err := os.RemoveAll(filepath.Join(f.runDir, "legacy")); err != nil {
		t.Fatal(err)
	}
	if got := f.tree(t); got != tree || exists(into) {
		t.Errorf("the tree after the failed adopt:\n%s\nbefore:\n%s", got, tree)
	}

	u := f.units[mgBeside]
	for name, change := range map[string]func(mv *Move){
		"from the library": func(mv *Move) { mv.From = filepath.Join(f.p.Root, MoviesDir, "x.mkv") },
		"to the packages":  func(mv *Move) { mv.To = filepath.Join(f.cfg.PackagesRoot, "x.mkv") },
	} {
		plan := *u
		plan.Moves = append([]Move{}, u.Moves...)
		for i := range plan.Moves {
			if plan.Moves[i].Kind == MoveLink && filepath.Ext(plan.Moves[i].From) == ".mkv" {
				change(&plan.Moves[i])
			}
		}
		if err := f.migration(t).checkPlan(ctx, &plan); err == nil || !strings.Contains(err.Error(), "the link move of") {
			t.Errorf("a link %s: %v", name, err)
		}
	}

	if rep, err := f.migration(t).Adopt(ctx, ids); err != nil || rep.Adopted != 1 || !one(old, into) {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	if err := os.Remove(old); err != nil {
		t.Fatal(err)
	}
	if rep, err := f.migration(t).Revert(ctx, ids); err != nil || rep.Reverted != 1 {
		t.Fatalf("Revert with the first name gone: %+v, %v", rep, err)
	}
	if got := f.tree(t); got != tree {
		t.Errorf("the tree after the revert:\n%s\nbefore the adopt:\n%s", got, tree)
	}
}

// An extra's checksums are never pending, and a checksums file beside a
// pending record, or a folder that does not hold what the record counted,
// breaks the chain.
func TestAPendingChainHoldsOnlyAsFarAsItGoes(t *testing.T) {
	dir := t.TempDir()
	v := librarytest.Version{VersionID: IDOf("v"), PackageID: IDOf("p"), CreatedAt: "2026-09-01T10:00:00Z", Pending: true}
	librarytest.WriteVersion(t, dir, v)
	if c, err := VerifyVersion(dir, false); err != nil || !c.Pending {
		t.Fatalf("a pending version: %+v, %v", c, err)
	}
	if _, err := VerifyExtra(dir, false); err == nil || !strings.Contains(err.Error(), "only a version's are") {
		t.Errorf("a pending extra: %v", err)
	}
	librarytest.Write(t, filepath.Join(dir, "hls", "v0", "seg1.m4s"), []byte("one more"))
	if _, err := VerifyVersion(dir, false); err == nil || !strings.Contains(err.Error(), "package.json counted") {
		t.Errorf("a file more than counted: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "hls", "v0", "seg1.m4s")); err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, filepath.Join(dir, "hls", "v0", "seg0.m4s"), []byte("a segment of another length"))
	if _, err := VerifyVersion(dir, false); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("other bytes than counted: %v", err)
	}
	librarytest.Write(t, filepath.Join(dir, "hls", "v0", "seg0.m4s"), []byte(librarytest.DefaultFiles["hls/v0/seg0.m4s"]))
	librarytest.Write(t, filepath.Join(dir, SumsFile), []byte(""))
	if _, err := VerifyVersion(dir, false); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Errorf("a checksums file beside a pending record: %v", err)
	}
}
