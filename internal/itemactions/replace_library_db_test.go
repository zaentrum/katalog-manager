package itemactions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/graph"
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
