package itemactions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An episode another's file covers is packaged with that file: packageItem
// of it enqueues its holder, the result the holder's and saying so, and its
// own transcode is never made. A title whose file is a disc image is not
// packaged (ErrDiscImage), nor is a series' episode whose file is one.
func TestPackageItemOfACoveredEpisodeOrADiscImage(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, series, "series", "A Series", "")
	storetest.AddItem(t, st, episode1, "episode", "Pilot", series)
	storetest.AddItem(t, st, episode2, "episode", "Pilot, Part Two", series)
	storetest.AddItem(t, st, "disc", "movie", "On A Disc", "")
	storetest.AddItem(t, st, "discep", "episode", "On A Disc Too", series)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES
		('a1', $1, '/media/Pilot.S01E01E02.mkv', true), ('a2', 'disc', '/media/Disc.ISO', true), ('a3', 'discep', '/media/Ep.img', true)`,
		episode1)
	if _, err := library.Link(ctx, st.Pool(), episode1, episode2); err != nil {
		t.Fatal(err)
	}
	b := &fakeBus{}
	svc := New(st, config.Config{}, processing.New(st.Pool()), nil)
	svc.events = b

	res, err := svc.PackageItem(ctx, episode2)
	if err != nil || res.Status == nil || *res.Status != "pending" || !strings.HasPrefix(*res.Message, library.CoveredNote(episode2, episode1)) {
		t.Fatalf("package the covered episode: %+v, %v", res, err)
	}
	if got := b.take(); got != events.TopicAnalyzed+" "+episode1+" episode transcode package" {
		t.Errorf("sent %q, want the holder's transcode", got)
	}
	if got := transcode(t, st, episode2); got != "not_applicable sent=false retry=false "+processing.CoveredReason(episode1) {
		t.Errorf("the covered episode's transcode: %s", got)
	}

	res, err = svc.PackageItem(ctx, "disc")
	if !errors.Is(err, ErrDiscImage) || res.Message == nil || !strings.Contains(*res.Message, processing.DiscImageReason) {
		t.Errorf("package a disc image: %+v, %v", res, err)
	}
	res, err = svc.PackageItem(ctx, series)
	if err != nil || *res.EpisodesEnqueued != 0 {
		t.Errorf("package the series, its holder busy and an episode a disc image: %+v, %v", res, err)
	}
	if got := b.take(); got != "" {
		t.Errorf("sent %q for a disc image", got)
	}
}

// Removing an episode another's file covers touches no file: the holder's
// file and package stay, and the holder is marked changed (its file covers
// one episode fewer). Removing the holder keeps the episodes its file covered
// besides it, unlinked, with no file now, and the result names them; a
// series removed takes them all.
func TestRemovingAHolderOrAnEpisodeItCovers(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages"}
	const e3 = "e3e3e3e3-0000-4000-8000-000000000004"
	storetest.AddItem(t, st, series, "series", "A Series", "")
	for i, id := range []string{episode1, episode2, e3} {
		storetest.AddItem(t, st, id, "episode", "Pilot", series)
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = $2 WHERE id = $1`, id, i+1)
	}
	file := filepath.Join(cfg.NFSRoot, "series/A Series/A.Series.S01E01-E03.mkv")
	pkg := packageRoot(cfg.PackagesRoot, "episode", episode1)
	for _, p := range []string{file, filepath.Join(pkg, "manifest.json")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind) VALUES
		('a1', $1, $2, true, 'primary'), ('p1', $1, $3, false, 'packaged')`, episode1, file, filepath.Join(pkg, "manifest.json"))
	for _, id := range []string{episode2, e3} {
		if _, err := library.Link(ctx, st.Pool(), episode1, id); err != nil {
			t.Fatal(err)
		}
	}
	svc := New(st, cfg, processing.New(st.Pool()), nil)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)

	res, err := svc.RemoveItem(ctx, e3, true, true, "")
	if err != nil || res.ItemsRemoved != 1 || res.FilesRemoved != 0 || res.PackagesRemoved != 0 || len(res.Unlinked) != 0 {
		t.Fatalf("remove a covered episode: %+v, %v", res, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the holder's file: %v", err)
	}
	if _, err := os.Stat(pkg); err != nil {
		t.Errorf("the holder's package: %v", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND modifiedat > '2001-01-01'`,
		episode1); n != 1 {
		t.Error("the holder, whose file covers one episode fewer, is not marked changed")
	}

	res, err = svc.RemoveItem(ctx, episode1, false, false, "")
	if err != nil || res.ItemsRemoved != 1 || len(res.Unlinked) != 1 || res.Unlinked[0] != episode2 {
		t.Fatalf("remove the holder: %+v, %v; want S01E02 kept, unlinked", res, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND coveredby IS NULL
		AND modifiedat > '2001-01-01'`, episode2); n != 1 {
		t.Error("S01E02 is not kept, unlinked and marked changed")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2`, episode2, processing.UncoveredReason(episode1, "that covered it was removed")); n != len(processing.CoveredSteps) {
		t.Errorf("%d of S01E02's steps say it has no file now", n)
	}
	if _, ok := storetest.Deleted(t, st, episode2); ok {
		t.Error("S01E02 is in the deletion log")
	}

	// A series takes its episodes, the holder and the ones it covers alike.
	res, err = svc.RemoveItem(ctx, series, false, false, "")
	if err != nil || res.ItemsRemoved != 2 || len(res.Unlinked) != 0 {
		t.Errorf("remove the series: %+v, %v", res, err)
	}
}

// replaceSource of an episode another's file covers gives its holder the
// new file, the answer the holder's and saying so.
func TestReplacingTheFileOfACoveredEpisodeReplacesItsHolders(t *testing.T) {
	f := newReplacing(t)
	ctx := context.Background()
	storetest.AddItem(t, f.st, series, "series", "A Series", "")
	storetest.AddItem(t, f.st, episode1, "episode", "Pilot", series)
	storetest.AddItem(t, f.st, episode2, "episode", "Pilot, Part Two", series)
	old := f.write(t, "media/A.Series.S01E01E02.mkv", 100)
	cur := f.write(t, "media/A.Series.S01E01E02.1080p.mkv", 200)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary)
		VALUES ('a-e1', $1, $2, 100, true)`, episode1, old)
	if _, err := library.Link(ctx, f.st.Pool(), episode1, episode2); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.ReplaceSource(ctx, graph.ReplaceSourceRequest{ItemID: episode2, Path: cur, Reencode: true})
	if err != nil || !res.Replaced || res.ItemID != episode1 || res.OldPath != old ||
		!strings.HasPrefix(res.Message, library.CoveredNote(episode2, episode1)) {
		t.Fatalf("replace the covered episode's file: %+v, %v", res, err)
	}
	if got := assetOf(t, f.st, "a-e1"); !strings.Contains(got, cur) {
		t.Errorf("the holder's asset: %s", got)
	}
	if got := f.re.take(); len(got) != 1 || got[0] != episode1 {
		t.Errorf("encoded again: %v, want the holder", got)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, episode2); n != 0 {
		t.Error("the covered episode got a file")
	}
	if h, err := library.HolderOf(ctx, f.st.Pool(), episode2); err != nil || h != episode1 {
		t.Errorf("the covered episode is covered by %q, %v; want the holder still", h, err)
	}
}
