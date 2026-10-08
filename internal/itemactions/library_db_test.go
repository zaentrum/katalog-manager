package itemactions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A library of the v2 layout with titles in it.
const (
	v2Film    = "f0f0f0f0-0000-4000-8000-000000000001"
	v2Retired = "f1f1f1f1-0000-4000-8000-000000000002"
)

type v2Library struct {
	st  *store.Store
	cfg config.Config
	p   library.Paths
	svc *Service
}

func newV2Library(t *testing.T) *v2Library {
	t.Helper()
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LibraryRoot: dir}
	svc := New(st, cfg, processing.New(st.Pool()), nil)
	svc.events = &fakeBus{}
	return &v2Library{st: st, cfg: cfg, p: library.PathsOf(cfg), svc: svc}
}

// title gives the library a title of type typ under parent: its original in
// the arrivals (a present source), and, with packaged, its complete version
// in the record. It answers the original's path and the version's folder.
func (l *v2Library) title(t *testing.T, id, typ, parent string, packaged bool) (string, string) {
	t.Helper()
	storetest.AddItem(t, l.st, id, typ, "Title "+id[:4], parent)
	original := filepath.Join(l.p.Arrivals, "Title "+id[:4], "Title "+id[:4]+".mkv")
	librarytest.Write(t, original, []byte("an original"))
	storetest.Exec(t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ('s-' || left($1::varchar, 8), $1::varchar, 'x.mkv', $2, 11, 'present')`, id, original)
	storetest.Exec(t, l.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES ('p-' || left($1::varchar, 8), $1::varchar, $2, true, 'primary', 's-' || left($1::varchar, 8))`, id, original)
	if !packaged {
		return original, ""
	}
	pl, err := library.PlaceOf(context.Background(), l.st.Pool(), id)
	if err != nil {
		t.Fatal(err)
	}
	vid := "9" + id[1:]
	dir := library.VersionDir(l.p.ItemDir(pl), vid)
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: vid, PackageID: library.NewID(),
		SourceIDs: []string{"s-" + id[:8]}, CreatedAt: "2026-10-06T10:00:00Z"})
	storetest.Exec(t, l.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir, completedat)
		VALUES ($1, $2, ARRAY['s-' || left($2::varchar, 8)], 'complete', $3, now())`, vid, id, dir)
	return original, dir
}

// retire has the title's original deleted after packaging, as the retire
// job leaves it.
func (l *v2Library) retire(t *testing.T, id string) {
	t.Helper()
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL, retireeventid = 'ev-1',
		retireeventat = '2026-10-06 11:00:00+00', deletedat = '2026-10-06 11:00:05+00' WHERE item_id = $1`, id)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_playbackassets SET isprimary = false, kind = 'original' WHERE item_id = $1`, id)
}

// With the v2 layout a title whose original was deleted after packaging is
// not packaged again: 409, saying so and naming the event. A series'
// packaging leaves out an episode whose original is being deleted.
func TestPackageItemOfARetiredTitle(t *testing.T) {
	l := newV2Library(t)
	l.title(t, v2Retired, "movie", "", true)
	l.retire(t, v2Retired)
	res, err := l.svc.PackageItem(context.Background(), v2Retired)
	want := "packaged; nothing to package from: the original was deleted after packaging (event ev-1, 2026-10-06T11:00:00Z)"
	if !errors.Is(err, ErrRetired) || err.Error() != want || res.Message == nil || *res.Message != want {
		t.Errorf("PackageItem: %+v, %v; want %q", res, err, want)
	}
	if got := transcode(t, l.st, v2Retired); got != "none" {
		t.Errorf("the retired title's transcode: %s", got)
	}

	storetest.AddItem(t, l.st, series, "series", "A Series", "")
	l.title(t, episode1, "episode", series, false)
	l.title(t, episode2, "episode", series, false)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring', retireeventid = 'ev-2',
		retireeventat = now() WHERE item_id = $1`, episode2)
	sres, err := l.svc.PackageItem(context.Background(), series)
	if err != nil || sres.EpisodesEnqueued == nil || *sres.EpisodesEnqueued != 1 {
		t.Errorf("PackageItem(series): %+v, %v; want the episode with its original", sres, err)
	}
	if got := transcode(t, l.st, episode2); got != "none" {
		t.Errorf("the retiring episode's transcode: %s", got)
	}
}

// With the v2 layout a title is validated against its current version in
// the record: ok while its original and its package's chain hold; retired
// once its original was deleted after packaging and the chain holds; lost
// when the chain is broken, or no version is left; source_missing when its
// original is not there and no event says it was deleted; no_package
// without a version. A series counts the retired and the lost too.
func TestValidateItemWithTheV2Layout(t *testing.T) {
	l := newV2Library(t)
	ctx := context.Background()
	const (
		ok, retired, lostRetired, lostVersion, missing, unpackaged = "a0a0a0a0-0000-4000-8000-000000000001",
			"a1a1a1a1-0000-4000-8000-000000000002", "a2a2a2a2-0000-4000-8000-000000000003",
			"a3a3a3a3-0000-4000-8000-000000000004", "a4a4a4a4-0000-4000-8000-000000000005",
			"a5a5a5a5-0000-4000-8000-000000000006"
	)
	for _, id := range []string{ok, retired, lostRetired, lostVersion, missing} {
		l.title(t, id, "movie", "", true)
	}
	l.title(t, unpackaged, "movie", "", false)
	l.retire(t, retired)
	l.retire(t, lostRetired)
	for _, id := range []string{lostRetired, lostVersion} {
		v, _ := library.Current(ctx, l.st.Pool(), id)
		if err := os.Remove(filepath.Join(*v.Dir, library.PackageFile)); err != nil {
			t.Fatal(err)
		}
	}
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_playbackassets SET path = '/nowhere/x.mkv' WHERE item_id = $1`, missing)
	for id, want := range map[string]string{ok: "ok", retired: "retired", lostRetired: "lost", lostVersion: "lost",
		missing: "source_missing", unpackaged: "no_package"} {
		res, err := l.svc.ValidateItem(ctx, id)
		if err != nil || res.Code != want {
			t.Errorf("ValidateItem(%s): %s %q, %v; want %s", id, res.Code, res.Message, err, want)
		}
		if want == "retired" && res.Message != "Packaged as version 9"+id[1:]+
			", its chain holds; the original was deleted after packaging (event ev-1, 2026-10-06T11:00:00Z)." {
			t.Errorf("the retired title's message: %q", res.Message)
		}
	}
	storetest.Exec(t, l.st, `DELETE FROM com_nalet_katalog_itemversions WHERE item_id = $1`, retired)
	if res, _ := l.svc.ValidateItem(ctx, retired); res.Code != "lost" || !strings.HasPrefix(res.Message, "No complete version, and the original") {
		t.Errorf("a retired title without its version: %s %q", res.Code, res.Message)
	}

	storetest.AddItem(t, l.st, series, "series", "A Series", "")
	l.title(t, episode1, "episode", series, true)
	l.title(t, episode2, "episode", series, true)
	l.retire(t, episode2)
	res, err := l.svc.ValidateItem(ctx, series)
	if want := "1 ok, 1 retired, 0 not packaged, 0 stale, 0 source missing, 0 lost, 0 codec mismatch, 0 with findings (of 2 episodes)"; err != nil ||
		res.Code != "series" || res.Message != want {
		t.Errorf("ValidateItem(series): %q, %v; want %q", res.Message, err, want)
	}
	var codes []string
	for _, f := range res.Findings {
		codes = append(codes, f.Code+"="+f.Message)
	}
	if got := strings.Join(codes, " "); got != "episodes=2 ok=1 retired=1 no_package=0 stale=0 source_missing=0 lost=0 codec_mismatch=0 with_findings=0" {
		t.Errorf("the series' findings: %s", got)
	}
}

// With the v2 layout a removal deletes the originals the titles still have
// where they arrived, with deleteFiles, and with deletePackages their
// folders in the record (a series' with its episodes') and their entries in
// the work folder, the inbox's and the staging's, and the package store's
// folder of before. A retired original's trash and other titles' folders
// stay.
func TestRemoveItemWithTheV2Layout(t *testing.T) {
	l := newV2Library(t)
	ctx := context.Background()
	original, version := l.title(t, v2Film, "movie", "", true)
	filmDir := filepath.Dir(filepath.Dir(version))
	inbox, staging := l.p.InboxDir(v2Film), l.p.StagingDir("9"+v2Film[1:])
	legacy := filepath.Join(l.cfg.PackagesRoot, "movies", v2Film[:2], v2Film)
	trash := l.p.TrashDir(timeOf(t, "2026-10-06T11:00:00Z"), "s-old")
	for _, f := range []string{filepath.Join(inbox, "renditions.json"), filepath.Join(staging, ".packaging"),
		filepath.Join(legacy, "manifest.json"), filepath.Join(trash, "old.mkv")} {
		librarytest.Write(t, f, []byte("x"))
	}
	_, other := l.title(t, unrelated, "movie", "", true)

	res, err := l.svc.RemoveItem(ctx, v2Film, true, true, "")
	if err != nil || !res.Deleted || res.FilesRemoved != 1 || res.PackagesRemoved != 4 || len(res.Errors) != 0 {
		t.Fatalf("RemoveItem: %+v, %v", res, err)
	}
	for _, gone := range []string{original, filepath.Dir(original), filmDir, inbox, staging, legacy} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
	for _, kept := range []string{trash, other, l.p.Arrivals} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s is gone: %v", kept, err)
		}
	}

	storetest.AddItem(t, l.st, series, "series", "A Series", "")
	_, ep := l.title(t, episode1, "episode", series, true)
	seriesDir := l.p.SeriesDir(series)
	librarytest.Write(t, filepath.Join(seriesDir, library.ItemFile), []byte("{}\n"))
	res, err = l.svc.RemoveItem(ctx, series, false, true, "")
	if err != nil || res.ItemsRemoved != 2 || res.FilesRemoved != 0 {
		t.Fatalf("RemoveItem(series): %+v, %v", res, err)
	}
	for _, gone := range []string{seriesDir, ep} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
}

func timeOf(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// inVersion moves the title's original into its version's folder, as the
// packager renames it in, and has the catalog say so: it answers where it
// lies now.
func (l *v2Library) inVersion(t *testing.T, id, original, version string) string {
	t.Helper()
	to := filepath.Join(version, "original.mkv")
	if err := os.Rename(original, to); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(l.p.Arrivals, original)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2, librarypath = $3, filename = 'original.mkv'
		WHERE item_id = $1`, id, to, filepath.ToSlash(rel))
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_playbackassets SET path = $2 WHERE item_id = $1 AND kind = 'primary'`, id, to)
	return to
}

// An original in its version's folder is a file of its title: a removal
// that keeps the files puts it back where it arrived before the title's
// folder goes, and keeps the folder when its place is taken, saying so; one
// that deletes the files deletes it.
func TestARemovalKeepsAnOriginalInItsVersionsFolder(t *testing.T) {
	l := newV2Library(t)
	ctx := context.Background()
	original, version := l.title(t, v2Film, "movie", "", true)
	moved := l.inVersion(t, v2Film, original, version)
	filmDir := filepath.Dir(filepath.Dir(version))
	res, err := l.svc.RemoveItem(ctx, v2Film, false, true, "")
	if err != nil || !res.Deleted || res.FilesRemoved != 0 || len(res.Errors) != 0 {
		t.Fatalf("RemoveItem keeping the files: %+v, %v", res, err)
	}
	if _, err := os.Stat(original); err != nil {
		t.Errorf("the original is not back where it arrived: %v", err)
	}
	if _, err := os.Stat(filmDir); !os.IsNotExist(err) {
		t.Errorf("the title's folder is there: %v", err)
	}

	// Its place taken: the folder stays.
	original, version = l.title(t, unrelated, "movie", "", true)
	moved = l.inVersion(t, unrelated, original, version)
	librarytest.Write(t, original, []byte("another file"))
	res, err = l.svc.RemoveItem(ctx, unrelated, false, true, "")
	if err != nil || !res.Deleted || len(res.Errors) != 1 || !strings.Contains(res.Errors[0], "is kept, a file lying where it arrived") {
		t.Fatalf("RemoveItem with the original's place taken: %+v, %v", res, err)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("the original whose place is taken is gone: %v", err)
	}

	// Deleting the files deletes it.
	const third = "f3f3f3f3-0000-4000-8000-000000000003"
	original, version = l.title(t, third, "movie", "", true)
	moved = l.inVersion(t, third, original, version)
	res, err = l.svc.RemoveItem(ctx, third, true, true, "")
	if err != nil || res.FilesRemoved != 1 || len(res.Errors) != 0 {
		t.Fatalf("RemoveItem deleting the files: %+v, %v", res, err)
	}
	for _, gone := range []string{moved, original, filepath.Dir(filepath.Dir(version))} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is there: %v", gone, err)
		}
	}
}
