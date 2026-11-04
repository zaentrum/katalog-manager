package itemactions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// With the v2 layout a title is given a file from the arrivals or from
// .work/replace/, from where it is taken into the arrivals first (not over a
// file of its name there). The new file is a source of the title of its own,
// which its asset names; the old original stays one, present. A title whose
// original was retired is given a file anew. A file under the legacy media
// root is refused, and the old file deleted when asked is a source no more.
func TestReplaceSourceWithTheV2Layout(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LibraryRoot: dir, WorkRoot: dir + "/.work",
		ArrivalsRoot: dir + "/.work/incoming", ExtrasRoot: dir + "/.work/extras"}
	f := &replacing{st: st, cfg: cfg, dir: dir, re: &fakeReencoder{}}
	svc := New(st, cfg, processing.New(st.Pool()), nil)
	old := f.write(t, ".work/incoming/Bunny (2008).mkv", 100)
	storetest.AddItem(t, st, bunny, "movie", "Big Buck Bunny", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, qh1, state)
		VALUES ('s-old', $1, 'Bunny (2008).mkv', $2, 100, NULL, 'present')`, bunny, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid)
		VALUES ('a-bunny', $1, $2, 100, true, 'primary', 's-old')`, bunny, old)

	handed := f.write(t, ".work/replace/Big Buck Bunny (2008).mov", 5000)
	res, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: handed})
	if err != nil || !res.Replaced {
		t.Fatalf("ReplaceSource from .work/replace: %+v, %v", res, err)
	}
	taken := filepath.Join(cfg.ArrivalsRoot, "Big Buck Bunny (2008).mov")
	if res.Path != taken {
		t.Errorf("the title's file is %s, want %s", res.Path, taken)
	}
	if _, err := os.Stat(taken); err != nil {
		t.Errorf("the file was not taken into the arrivals: %v", err)
	}
	if _, err := os.Stat(handed); !os.IsNotExist(err) {
		t.Errorf("the file handed over is still in .work/replace: %v", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_itemsources s
		ON s.id = p.sourceid WHERE p.id = 'a-bunny' AND p.path = $1 AND s.arrivalpath = $1 AND s.state = 'present'
		AND s.sizebytes = 5000 AND s.id <> 's-old'`, taken); n != 1 {
		t.Error("the new file is no source of the title its asset names")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = 's-old' AND state = 'present'
		AND arrivalpath = $1`, old); n != 1 {
		t.Error("the old original is no source any more")
	}
	// A file of the same name handed over again: the arrivals hold one.
	again := f.write(t, ".work/replace/Big Buck Bunny (2008).mov", 6000)
	if _, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: again}); err == nil ||
		!strings.Contains(err.Error(), "is there already") {
		t.Errorf("a name the arrivals hold: %v", err)
	}
	media := f.write(t, "media/Big Buck Bunny.mkv", 10)
	if _, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: media}); err == nil ||
		!strings.Contains(err.Error(), "is not under ARRIVALS_ROOT or .work/replace") {
		t.Errorf("a file of the legacy media root: %v", err)
	}

	// Retired: no file, its original deleted. It is given one anew, and the
	// old file of the replace before is deleted when asked.
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, bunny)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL WHERE arrivalpath = $1`, taken)
	better := f.write(t, ".work/incoming/Bunny 4K.mkv", 9000)
	res, err = svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: better, DeleteOldFile: true})
	if err != nil || !res.Replaced || res.OldPath != "" {
		t.Fatalf("a retired title given a file: %+v, %v", res, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_itemsources s
		ON s.id = p.sourceid WHERE p.item_id = $1 AND p.isprimary AND p.kind = 'primary' AND p.path = $2 AND s.state = 'present'`,
		bunny, better); n != 1 {
		t.Error("the retired title has no file anew")
	}
	if !strings.Contains(res.Message, "its original was retired") {
		t.Errorf("the message: %s", res.Message)
	}

	// The old original deleted with a replace is no source any more.
	newer := f.write(t, ".work/incoming/Bunny 8K.mkv", 9500)
	if res, err = svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: newer, DeleteOldFile: true}); err != nil ||
		!res.OldFileDeleted {
		t.Fatalf("with the old file deleted: %+v, %v", res, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE filename = 'Bunny 4K.mkv'
		AND state = 'removed' AND arrivalpath IS NULL`); n != 1 {
		t.Error("the deleted old file is still a source")
	}
}

// A title whose version being built is in the library (a run placed it, its
// handover not recorded) is given no other file: the run taken again
// reports that version as it is. Once it is recorded, it may be.
func TestReplaceSourceWaitsForALostHandover(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LibraryRoot: dir, WorkRoot: dir + "/.work",
		ArrivalsRoot: dir + "/.work/incoming", ExtrasRoot: dir + "/.work/extras"}
	f := &replacing{st: st, cfg: cfg, dir: dir, re: &fakeReencoder{}}
	svc := New(st, cfg, processing.New(st.Pool()), nil)
	const bunnyID = "b0b0b0b0-0000-4000-8000-000000000001"
	old := f.write(t, ".work/incoming/Bunny (2008).mkv", 100)
	storetest.AddItem(t, st, bunnyID, "movie", "Big Buck Bunny", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('s-old', $1, 'Bunny (2008).mkv', $2, 100, 'present')`, bunnyID, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid)
		VALUES ('a-bunny', $1, $2, 100, true, 'primary', 's-old')`, bunnyID, old)
	const vid = "9b0b0b0b-0000-4000-8000-000000000002"
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, ARRAY['s-old'], 'building')`,
		vid, bunnyID)
	f.write(t, "movies/b0/"+bunnyID+"/versions/"+vid+"/version.json", 10)
	handed := f.write(t, ".work/replace/Bunny 4K.mkv", 5000)
	if _, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunnyID, Path: handed}); err == nil ||
		!strings.Contains(err.Error(), "its handover is not recorded yet") {
		t.Fatalf("ReplaceSource while a placed version waits for its handover: %v", err)
	}
	if _, err := os.Stat(handed); err != nil {
		t.Errorf("the file handed over was taken: %v", err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET state = 'complete' WHERE id = $1`, vid)
	if res, err := svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunnyID, Path: handed}); err != nil || !res.Replaced {
		t.Errorf("ReplaceSource once the version is recorded: %+v, %v", res, err)
	}
}

// busyReencoder is a re-encoder that tells a title busy (why) or idle ("").
type busyReencoder struct {
	fakeReencoder
	why  string
	asks int
}

func (b *busyReencoder) BusyIn(context.Context, pgx.Tx, string) (string, error) {
	b.asks++
	return b.why, nil
}

// Asked only while the title is idle (ifIdle), a replace of a title whose
// encode runs is refused with SOURCE_BUSY before anything changes: the
// title keeps its file, the new one stays in .work/replace. A permanent
// refusal is said first, before the title is asked about; and a service whose
// re-encoder cannot tell whether the title is idle refuses rather than guess.
// Idle, the title is given the file.
func TestReplaceSourceIfIdle(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LibraryRoot: dir, WorkRoot: dir + "/.work",
		ArrivalsRoot: dir + "/.work/incoming", ExtrasRoot: dir + "/.work/extras"}
	f := &replacing{st: st, cfg: cfg, dir: dir}
	re := &busyReencoder{why: "transcode is running: its worker last reported at 2026-11-04T10:00:00Z"}
	svc := New(st, cfg, processing.New(st.Pool()), nil).WithReencoder(re)
	old := f.write(t, ".work/incoming/Bunny (2008).mkv", 100)
	storetest.AddItem(t, st, bunny, "movie", "Big Buck Bunny", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('s-old', $1, 'Bunny (2008).mkv', $2, 100, 'present')`, bunny, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid)
		VALUES ('a-bunny', $1, $2, 100, true, 'primary', 's-old')`, bunny, old)
	handed := f.write(t, ".work/replace/acquire/tg_1/Big Buck Bunny (2008).mkv", 5000)
	in := graph.ReplaceSourceRequest{ItemID: bunny, Path: handed, DeleteOldFile: true, Reencode: true, IfIdle: true}
	kept := func(what string) {
		t.Helper()
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bunny' AND path = $1`, old); n != 1 {
			t.Errorf("%s: the title lost its file", what)
		}
		for _, p := range []string{old, handed} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %s went: %v", what, p, err)
			}
		}
		if len(re.take()) > 0 {
			t.Errorf("%s: the title was encoded again", what)
		}
	}

	_, err = svc.ReplaceSource(asOperator, in)
	if r := sourceRefusal(t, err); r.Code != codeSourceBusy || !strings.Contains(r.Message, "(ifIdle): transcode is running") {
		t.Errorf("a busy title: %+v", r)
	}
	kept("a busy title")

	// A file the catalog does not take is refused as such, the title not asked.
	re.asks = 0
	notVideo := f.write(t, ".work/replace/acquire/tg_1/Big Buck Bunny (2008).nfo", 10)
	_, err = svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: notVideo, IfIdle: true})
	if r := sourceRefusal(t, err); r.Code != codeSourceRefused || re.asks != 0 {
		t.Errorf("a file that is no title's, busy: %+v, asked %d", r, re.asks)
	}

	plain := New(st, cfg, processing.New(st.Pool()), nil).WithReencoder(&fakeReencoder{})
	_, err = plain.ReplaceSource(asOperator, in)
	if r := sourceRefusal(t, err); r.Code != codeSourceRefused || !strings.Contains(r.Message, "nothing here tells") {
		t.Errorf("a service that cannot tell: %+v", r)
	}
	kept("a service that cannot tell")

	re.why = ""
	res, err := svc.ReplaceSource(asOperator, in)
	if err != nil || !res.Replaced || !res.OldFileDeleted || res.Path != filepath.Join(cfg.ArrivalsRoot, "Big Buck Bunny (2008).mkv") {
		t.Fatalf("an idle title: %+v %v", res, err)
	}
	if ids := re.take(); len(ids) != 1 || ids[0] != bunny {
		t.Errorf("encoded again: %v", ids)
	}
}

// inVersion is a v2 catalog whose movie's original lies in the folder of its
// complete version (versions/<versionId>/original.mkv), its source present
// there and its asset naming it, and a file handed over in .work/replace/.
type inVersion struct {
	f              *replacing
	svc            *Service
	itemDir, old   string
	handed, arrive string
}

const (
	ivVersion = "9c0b0b0b-0000-4000-8000-00000000000c"
	ivPackage = "4c0b0b0b-0000-4000-8000-00000000000c"
)

func newInVersion(t *testing.T) *inVersion {
	t.Helper()
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LibraryRoot: dir, WorkRoot: dir + "/.work",
		ArrivalsRoot: dir + "/.work/incoming", ExtrasRoot: dir + "/.work/extras", AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon",
		ServiceClients: []string{"zaentrum-manager"}}
	iv := &inVersion{f: &replacing{st: st, cfg: cfg, dir: dir, re: &fakeReencoder{}}}
	iv.svc = New(st, cfg, processing.New(st.Pool()), nil).WithReencoder(&busyReencoder{})
	p := library.PathsOf(cfg)
	iv.itemDir = p.MovieDir(bunny)
	vdir := library.VersionDir(iv.itemDir, ivVersion)
	librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: ivVersion, PackageID: ivPackage, SourceIDs: []string{"s-old"},
		CreatedAt: "2026-10-06T10:00:00Z"})
	iv.old = filepath.Join(vdir, "original.mkv")
	librarytest.Write(t, iv.old, make([]byte, 100))
	storetest.AddItem(t, st, bunny, "movie", "Big Buck Bunny", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('s-old', $1, 'original.mkv', $2, 100, 'present')`, bunny, iv.old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY['s-old'::varchar], 'complete', $3, $4, now() - interval '1 day')`, ivVersion, bunny, ivPackage, vdir)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid)
		VALUES ('a-bunny', $1, $2, 100, true, 'primary', 's-old')`, bunny, iv.old)
	iv.handed = iv.f.write(t, ".work/replace/acquire/tg_1/Big.Buck.Bunny.2008.2160p.mkv", 5000)
	iv.arrive = filepath.Join(cfg.ArrivalsRoot, "Big.Buck.Bunny.2008.2160p.mkv")
	return iv
}

// asAddon is an addon's service account, as acquire's token is.
var asAddon = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "addon-1", Client: "example-addon",
	Claims: map[string]any{"realm_access": map[string]any{"roles": []any{"zaentrum-addon"}}}})

// A title given another file in place of its original in its version's
// folder, the old file asked to go, has the old one go the way the retire
// job takes an original: into the library's trash, its source deleted by the
// addon that asked, its original-deleted event recorded — the answer says
// where it lies and until when.
func TestAnOriginalInItsVersionsFolderGoesIntoTheTrash(t *testing.T) {
	iv := newInVersion(t)
	res, err := iv.svc.ReplaceSource(asAddon, graph.ReplaceSourceRequest{ItemID: bunny, Path: iv.handed, DeleteOldFile: true,
		Reencode: true, IfIdle: true})
	if err != nil || !res.Replaced || !res.OldFileDeleted || res.Path != iv.arrive || res.OldPath != iv.old {
		t.Fatalf("the replace: %+v %v", res, err)
	}
	src, err := library.SourceByID(context.Background(), iv.f.st.Pool(), "s-old")
	if err != nil || src == nil || src.State != library.SourceDeleted || src.RetireEventAt == nil ||
		derefTest(src.DeletedBy) != "replaceSource, asked by addon-1 (client example-addon, an addon (zaentrum-addon))" {
		t.Fatalf("the old source: %+v %v", src, err)
	}
	trash := filepath.Join(library.PathsOf(iv.f.cfg).TrashDir(*src.RetireEventAt, "s-old"), "original.mkv")
	if _, err := os.Stat(trash); err != nil || res.OldFileTrash != trash {
		t.Errorf("the old file is not in the trash at %s (the answer says %q): %v", trash, res.OldFileTrash, err)
	}
	if _, err := os.Stat(iv.old); !os.IsNotExist(err) {
		t.Errorf("the old file is still in its version's folder: %v", err)
	}
	if dir, _ := library.FindEvent(iv.itemDir, derefTest(src.RetireEventID), library.EventOriginalDeleted); dir == "" {
		t.Error("no original-deleted event recorded")
	}
	if !strings.Contains(res.Message, "the old file, the title's original in version "+ivVersion+", is in the library's trash, "+trash+
		", until ") || !strings.Contains(res.Message, "from where it can be put back; its source s-old is noted deleted") {
		t.Errorf("the message: %s", res.Message)
	}
}

// While the retire job's lock is held — a pass, or an admin's list —, a
// replace that asks its old file to go is refused as busy once its wait is
// over, before anything changes: the title keeps its file, the old one stays
// in its version's folder, the file handed over in .work/replace.
func TestAReplaceWaitsForTheRetireJob(t *testing.T) {
	iv := newInVersion(t)
	release, err := library.HoldRetire(context.Background(), iv.f.st.Pool(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	retireWait = 300 * time.Millisecond
	t.Cleanup(func() { retireWait = time.Minute })
	_, err = iv.svc.ReplaceSource(asAddon, graph.ReplaceSourceRequest{ItemID: bunny, Path: iv.handed, DeleteOldFile: true, Reencode: true})
	if r := sourceRefusal(t, err); r.Code != codeSourceBusy || !strings.Contains(r.Message, "the library's retire job is running") {
		t.Errorf("a replace while the retire job runs: %+v", r)
	}
	if n := storetest.Count(t, iv.f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bunny' AND path = $1`, iv.old); n != 1 {
		t.Error("the title lost its file")
	}
	for _, p := range []string{iv.old, iv.handed} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s went: %v", p, err)
		}
	}
	if src, _ := library.SourceByID(context.Background(), iv.f.st.Pool(), "s-old"); src == nil || src.State != library.SourcePresent {
		t.Errorf("the old source: %+v", src)
	}
}

func derefTest(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
