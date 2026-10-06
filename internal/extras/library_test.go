package extras

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// With the v2 layout an extra's file lies under ARRIVALS_ROOT (beside its
// title, as the scanner's convention finds it) or EXTRAS_ROOT, and never in
// the library's record: the media root and the share's legacy folders are no
// roots of it any more.
func TestTheRootsOfAnExtrasFileWithTheV2Layout(t *testing.T) {
	f := newFixture(t)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	f.cfg.LibraryRoot, f.cfg.ArrivalsRoot, f.cfg.ExtrasRoot = f.dir, f.dir+"/.work/incoming", f.dir+"/.work/extras"
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	ctx := asAdmin()
	for _, rel := range []string{".work/extras/bbb/trailer.mov", ".work/incoming/Big Buck Bunny-trailer.mkv"} {
		if res, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, rel, 100), Kind: "trailer"}); err != nil ||
			!res.Created {
			t.Errorf("%s: %+v, %v", rel, res, err)
		}
	}
	for _, rel := range []string{"media/Big Buck Bunny-teaser.mkv", "library/movies/x/teaser.mkv", "extras/bbb/teaser.mkv"} {
		_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, rel, 100), Kind: "teaser"})
		if r := refusal(t, err); r.Status != http.StatusBadRequest || !strings.Contains(r.Message, "is not under ARRIVALS_ROOT or EXTRAS_ROOT") {
			t.Errorf("%s: %d %s", rel, r.Status, r.Message)
		}
	}
	// An extras' root inside the record holds no original.
	f.cfg.ExtrasRoot = f.dir + "/movies/bb"
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, "movies/bb/teaser.mkv", 100), Kind: "teaser"})
	if r := refusal(t, err); r.Status != http.StatusBadRequest || !strings.Contains(r.Message, "is in the library's record") {
		t.Errorf("in the record: %d %s", r.Status, r.Message)
	}
}

// The roots follow the layout the settings say when an extra is taken in:
// with none set, a service that runs on through a switch of the layout takes
// an extra's file from the legacy layout's extras' folder, then from the v2
// layout's, without a restart.
func TestTheRootsFollowTheLayoutWithoutARestart(t *testing.T) {
	f := newFixture(t)
	f.cfg.LibraryRoot, f.cfg.ExtrasRoot, f.cfg.Share = "", "", f.dir
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	ctx := asAdmin()
	add := func(rel string) error {
		_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, rel, 100), Kind: "trailer"})
		return err
	}
	if err := add("extras/bbb/a.mov"); err != nil {
		t.Errorf("legacy, the share's extras folder: %v", err)
	}
	if err := add(".work/extras/bbb/b.mov"); err == nil {
		t.Error("legacy took a file from the v2 layout's extras' folder")
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	if err := add(".work/extras/bbb/c.mov"); err != nil {
		t.Errorf("v2, the work folder's extras: %v", err)
	}
	if err := add("extras/bbb/d.mov"); err == nil {
		t.Error("v2 took a file from the legacy layout's extras' folder")
	}
}

// With the v2 layout an extra whose folder is recorded is written once: it is
// not packaged again (a new extra replaces it), and a title's extras are
// packaged again but for those. Its removal records the extra-removed event
// in its title's folder, once, and its folder and its work entries go after
// the grace. An extra of the library before is still removed by its record's
// event only.
func TestARecordedExtraWithTheV2Layout(t *testing.T) {
	f := newFixture(t)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	f.cfg.LibraryRoot, f.cfg.ArrivalsRoot, f.cfg.ExtrasRoot = f.dir, f.dir+"/.work/incoming", f.dir+"/.work/extras"
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	ctx := asAdmin()
	p := library.PathsOf(f.cfg)
	const recorded = "bbbbbbbb-0000-4000-8000-000000000001"
	dir := library.ExtraDir(p.MovieDir(film), recorded)
	librarytest.WriteExtra(t, dir, librarytest.Extra{ExtraID: recorded, PackageID: library.NewID(), CreatedAt: "2026-10-06T10:00:00Z"})
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state, recordpath,
			packagepath, recordedat, packagedat) VALUES ($1, $2, 'featurette', 'Featurette', 'api', 'ready', $3, $3, now(), now())`,
		recorded, film, dir)
	for _, d := range []string{p.ExtraInboxDir(recorded), p.ExtraStagingDir(recorded)} {
		librarytest.Write(t, filepath.Join(d, "renditions.json"), []byte("{}"))
	}

	_, err := f.svc.PackageExtra(ctx, recorded)
	if r := refusal(t, err); r.Status != http.StatusConflict || r.Message != "extra "+recorded+" is recorded in the library ("+dir+
		"): written once; add the file again as a new extra" {
		t.Errorf("PackageExtra of a recorded extra: %d %s", r.Status, r.Message)
	}
	res, err := f.svc.PackageExtras(ctx, film)
	if err != nil || res.Message != "its extra is recorded in the library, written once: add the file again as a new extra" {
		t.Errorf("PackageExtras with a recorded extra alone: %+v, %v", res, err)
	}
	other := f.add(t, ".work/extras/bbb/b.mov")
	f.w.take()
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET dispatchedat = NULL, state = 'failed' WHERE id = $1`, other.ID)
	res, err = f.svc.PackageExtras(ctx, film)
	if err != nil || res.Queued != 1 || !strings.HasSuffix(res.Message,
		"; 1 recorded in the library left as they are (written once: add a file again as a new extra)") {
		t.Errorf("PackageExtras: %+v, %v", res, err)
	}

	for range 2 {
		x, err := f.svc.RemoveExtra(ctx, recorded, "a duplicate")
		if err != nil || x == nil || x.RemovedAt == nil {
			t.Fatalf("RemoveExtra: %+v, %v", x, err)
		}
	}
	recordedEvents, err := library.ReadEvents(p.MovieDir(film))
	if err != nil || len(recordedEvents) != 1 {
		t.Fatalf("the events: %v, %v", recordedEvents, err)
	}
	b, _ := library.Encode(recordedEvents[0])
	for _, want := range []string{`"kind": "extra-removed"`, `"extraId": "` + recorded + `"`, `"reason": "a duplicate"`, `"by": "admin-1"`,
		`"eventId": "` + library.IDOf("extra-removed:"+recorded) + `"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the event lacks %s:\n%s", want, b)
		}
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET nextretryat = now() - interval '1 second' WHERE id = $1`, recorded)
	if n, err := f.svc.DeleteRemovedPackages(context.Background()); err != nil || n != 1 {
		t.Fatalf("DeleteRemovedPackages: %d, %v", n, err)
	}
	for _, gone := range []string{dir, filepath.Dir(dir), p.ExtraInboxDir(recorded), p.ExtraStagingDir(recorded)} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is left: %v", gone, err)
		}
	}
	if _, err := os.Stat(p.MovieDir(film)); err != nil {
		t.Errorf("the title's folder went: %v", err)
	}

	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, recordpath)
		VALUES ('x-v1', $1, 'featurette', 'Featurette', 'library', 'library/movies/ea/`+film+`/extras/x-v1/')`, film)
	if r := refusal(t, func() error { _, err := f.svc.RemoveExtra(ctx, "x-v1", ""); return err }()); r.Status != http.StatusConflict {
		t.Errorf("an extra of the library before: %+v", r)
	}
}
