package itemactions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A film removed with its files and packages takes its extras along: their
// files under the media root and the extras' root (a library record's file,
// written once, and one elsewhere stay), their packages in packages/extras/
// and the handoff the transcoder left in the inbox; their rows go with it.
// Removed without, nothing leaves the disk.
func TestRemovingAFilmTakesItsExtrasFilesAndPackages(t *testing.T) {
	for _, withFiles := range []bool{true, false} {
		st := storetest.Open(t)
		dir := t.TempDir()
		cfg := config.Config{NFSRoot: dir + "/media", PackagesRoot: dir + "/packages", LegacyLibraryRoot: dir + "/library",
			ExtrasRoot: dir + "/extras"}
		const film = "f1f1f1f1-0000-4000-8000-000000000001"
		write := func(rel string) string {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return p
		}
		feature := write("media/A Film/A Film.mkv")
		inExtras := write("extras/a-film/trailer.mov")
		inMedia := write("media/A Film/A Film-teaser.mkv")
		inLibrary := write("library/movies/f1/" + film + "/extras/x3/featurette.mkv")
		elsewhere := write("elsewhere/interview.mkv")
		storetest.AddItem(t, st, film, "movie", "A Film", "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a1', $1, $2, true)`, film, feature)
		ids := map[string]string{"x1aaaaaa-0000-4000-8000-000000000001": inExtras, "x2bbbbbb-0000-4000-8000-000000000002": inMedia,
			"x3cccccc-0000-4000-8000-000000000003": inLibrary, "x4dddddd-0000-4000-8000-000000000004": elsewhere}
		var pkgs []string
		for id, src := range ids {
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
				VALUES ($1, $2, 'trailer', 'Trailer', 'api', $3)`, id, film, src)
			pkgs = append(pkgs, filepath.Dir(write("packages/extras/"+id[:2]+"/"+id+"/manifest.json")))
		}
		inbox := filepath.Dir(write("packages/_inbox/extra-x1aaaaaa-0000-4000-8000-000000000001/renditions.json"))
		svc := New(st, cfg, processing.New(st.Pool()), nil)
		res, err := svc.RemoveItem(context.Background(), film, withFiles, withFiles, "")
		if err != nil || !res.Deleted || len(res.Errors) > 0 {
			t.Fatalf("RemoveItem: %+v, %v", res, err)
		}
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras`); n != 0 {
			t.Errorf("%d extras outlived their film", n)
		}
		gone := func(p string) bool { _, err := os.Stat(p); return os.IsNotExist(err) }
		if !withFiles {
			for _, p := range append([]string{feature, inExtras, inMedia, inbox}, pkgs...) {
				if gone(p) {
					t.Errorf("without asking, %s left the disk", p)
				}
			}
			continue
		}
		if res.FilesRemoved != 3 || res.PackagesRemoved != 5 {
			t.Errorf("removed %d files and %d packages, want 3 and 5", res.FilesRemoved, res.PackagesRemoved)
		}
		for _, p := range append([]string{feature, inExtras, inMedia, inbox, filepath.Join(dir, "extras", "a-film")}, pkgs...) {
			if !gone(p) {
				t.Errorf("%s is left", p)
			}
		}
		for _, p := range []string{inLibrary, elsewhere, cfg.ExtrasRoot, cfg.NFSRoot} {
			if gone(p) {
				t.Errorf("%s went", p)
			}
		}
	}
}
