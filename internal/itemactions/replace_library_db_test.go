package itemactions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// root is refused, and the old original asked to go is superseded by the new
// one, kept where it lies.
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

	// The old original asked to go is superseded by the new one: there
	// still, where it lies, and no file of the title's.
	newer := f.write(t, ".work/incoming/Bunny 8K.mkv", 9500)
	if res, err = svc.ReplaceSource(asOperator, graph.ReplaceSourceRequest{ItemID: bunny, Path: newer, DeleteOldFile: true}); err != nil ||
		res.OldFileDeleted || !res.OldFileSuperseded {
		t.Fatalf("with the old file asked to go: %+v, %v", res, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources o JOIN com_nalet_katalog_itemsources n
		ON n.id = o.supersededby WHERE o.filename = 'Bunny 4K.mkv' AND o.state = 'superseded' AND o.arrivalpath = $1
		AND o.supersededat IS NOT NULL AND n.arrivalpath = $2 AND n.state = 'present'`, better, newer); n != 1 {
		t.Error("the old original is not superseded by the new one where it lies")
	}
	if _, err := os.Stat(better); err != nil {
		t.Errorf("the superseded original is gone: %v", err)
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
	if err != nil || !res.Replaced || !res.OldFileSuperseded || res.Path != filepath.Join(cfg.ArrivalsRoot, "Big Buck Bunny (2008).mkv") {
		t.Fatalf("an idle title: %+v %v", res, err)
	}
	if ids := re.take(); len(ids) != 1 || ids[0] != bunny {
		t.Errorf("encoded again: %v", ids)
	}
}

// inVersion is a v2 catalog whose movie's original lies in the folder of its
// complete version (versions/<versionId>/original.mkv), its source present
// there and recorded, its asset naming it, and a file handed over in
// .work/replace/.
type inVersion struct {
	f              *replacing
	svc            *Service
	itemDir, old   string
	handed, arrive string
}

const (
	ivVersion = "9c0b0b0b-0000-4000-8000-00000000000c"
	ivPackage = "4c0b0b0b-0000-4000-8000-00000000000c"
	ivSource  = "5c0b0b0b-0000-4000-8000-00000000000c"
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
	librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: ivVersion, PackageID: ivPackage, SourceIDs: []string{ivSource},
		CreatedAt: "2026-10-06T10:00:00Z"})
	iv.old = filepath.Join(vdir, "original.mkv")
	librarytest.Write(t, iv.old, make([]byte, 100))
	librarytest.WriteSource(t, library.SourceDir(iv.itemDir, ivSource), map[string]any{"sourceId": ivSource,
		"file": map[string]any{"name": "original.mkv", "sizeBytes": 100}}, nil)
	storetest.AddItem(t, st, bunny, "movie", "Big Buck Bunny", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state, recordedat)
		VALUES ($3, $1, 'original.mkv', $2, 100, 'present', now())`, bunny, iv.old, ivSource)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$5::varchar], 'complete', $3, $4, now() - interval '1 day')`, ivVersion, bunny, ivPackage, vdir, ivSource)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid)
		VALUES ('a-bunny', $1, $2, 100, true, 'primary', $3)`, bunny, iv.old, ivSource)
	iv.handed = iv.f.write(t, ".work/replace/acquire/tg_1/Big.Buck.Bunny.2008.2160p.mkv", 5000)
	iv.arrive = filepath.Join(cfg.ArrivalsRoot, "Big.Buck.Bunny.2008.2160p.mkv")
	return iv
}

// asAddon is an addon's service account, as acquire's token is.
var asAddon = auth.WithPrincipal(context.Background(), &auth.Principal{Subject: "addon-1", Client: "example-addon",
	Claims: map[string]any{"realm_access": map[string]any{"roles": []any{"zaentrum-addon"}}}})

// A title given another file in place of its original in its version's
// folder, the old file asked to go, keeps the old one where it lies,
// superseded by the new one: no file is deleted or moved but the one handed
// over, no original-deleted event is recorded, and the title's folder
// records a note of what became of the old original, by the addon that
// asked, naming its source, whose record is there. The answer says so.
func TestAnOldOriginalAskedToGoIsSuperseded(t *testing.T) {
	iv := newInVersion(t)
	res, err := iv.svc.ReplaceSource(asAddon, graph.ReplaceSourceRequest{ItemID: bunny, Path: iv.handed, DeleteOldFile: true,
		Reencode: true, IfIdle: true})
	if err != nil || !res.Replaced || res.OldFileDeleted || !res.OldFileSuperseded || res.Path != iv.arrive || res.OldPath != iv.old {
		t.Fatalf("the replace: %+v %v", res, err)
	}
	ctx := context.Background()
	src, err := library.SourceByID(ctx, iv.f.st.Pool(), ivSource)
	if err != nil || src == nil || src.State != library.SourceSuperseded || derefTest(src.ArrivalPath) != iv.old ||
		src.RetireEventID != nil || src.TrashPath != nil || src.DeletedBy != nil {
		t.Fatalf("the old source: %+v %v", src, err)
	}
	if by, err := library.SupersededBy(ctx, iv.f.st.Pool(), ivSource); err != nil || by == "" {
		t.Fatalf("superseded by: %q, %v", by, err)
	} else if n := storetest.Count(t, iv.f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bunny'
		AND sourceid = $1 AND path = $2`, by, iv.arrive); n != 1 {
		t.Errorf("the source that took its place, %s, is not the title's file's", by)
	}
	if _, err := os.Stat(iv.old); err != nil {
		t.Errorf("the old file left its version's folder: %v", err)
	}
	if got := filesUnder(t, filepath.Join(iv.f.cfg.WorkRoot, "trash")); len(got) > 0 {
		t.Errorf("the trash holds %v", got)
	}
	evs, err := library.ReadEvents(iv.itemDir)
	if err != nil || len(evs) != 1 {
		t.Fatalf("the title's events: %v, %v; want its note", evs, err)
	}
	kind, _ := evs[0].Get("kind")
	by, _ := evs[0].Get("by")
	sid, _ := evs[0].Get("sourceId")
	reason, _ := evs[0].Get("reason")
	if kind != library.EventNote || by != "replaceSource, asked by addon-1 (client example-addon, an addon (zaentrum-addon))" ||
		sid != ivSource || !strings.Contains(reason.(string), "its original original.mkv is superseded, kept where it lies") {
		t.Errorf("the note: %v", evs[0])
	}
	if !strings.Contains(res.Message, "the old file stays where it lies, superseded: the retire job moves it into the library's trash "+
		"once a version of the title made of the new file is complete and verified; the title's folder records it (note ") {
		t.Errorf("the message: %s", res.Message)
	}
}

// A title that holds its originals (holdOriginal) is given its new file all
// the same, its old original superseded, and kept so: the answer says it
// stays while the title holds them.
func TestAHeldTitlesOldOriginalStaysSuperseded(t *testing.T) {
	iv := newInVersion(t)
	storetest.Exec(t, iv.f.st, `UPDATE com_nalet_katalog_items SET retirehold = true WHERE id = $1`, bunny)
	res, err := iv.svc.ReplaceSource(asAddon, graph.ReplaceSourceRequest{ItemID: bunny, Path: iv.handed, DeleteOldFile: true})
	if err != nil || !res.OldFileSuperseded || !strings.Contains(res.Message, "and not while the title holds its originals (holdOriginal)") {
		t.Fatalf("the replace of a held title: %+v %v", res, err)
	}
	if src, _ := library.SourceByID(context.Background(), iv.f.st.Pool(), ivSource); src == nil || src.State != library.SourceSuperseded {
		t.Errorf("the old source: %+v", src)
	}
}

// A title whose original is being retired is given no other file, asked to
// keep the old one or not: refused as busy, nothing changed — the title
// keeps its file, the old one stays in its version's folder, retiring, and
// the file handed over in .work/replace.
func TestAReplaceOfAnOriginalBeingRetiredIsBusy(t *testing.T) {
	for _, drop := range []bool{true, false} {
		iv := newInVersion(t)
		storetest.Exec(t, iv.f.st, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring' WHERE id = $1`, ivSource)
		_, err := iv.svc.ReplaceSource(asAddon, graph.ReplaceSourceRequest{ItemID: bunny, Path: iv.handed, DeleteOldFile: drop, Reencode: true})
		if r := sourceRefusal(t, err); r.Code != codeSourceBusy || !strings.Contains(r.Message, "is being retired") {
			t.Errorf("deleteOldFile %v, while the original is retired: %+v", drop, r)
		}
		if n := storetest.Count(t, iv.f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'a-bunny' AND path = $1
			AND sourceid = $2`, iv.old, ivSource); n != 1 {
			t.Error("the title lost its file")
		}
		for _, p := range []string{iv.old, iv.handed} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s went: %v", p, err)
			}
		}
		if n := storetest.Count(t, iv.f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE item_id = $1`, bunny); n != 1 {
			t.Errorf("%d sources, want the old one alone", n)
		}
		if src, _ := library.SourceByID(context.Background(), iv.f.st.Pool(), ivSource); src == nil || src.State != library.SourceRetiring {
			t.Errorf("the old source: %+v", src)
		}
	}
}

// filesUnder lists the files under dir.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func derefTest(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
