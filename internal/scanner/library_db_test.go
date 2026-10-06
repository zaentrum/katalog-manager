package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// v2 is a catalog with the library's v2 layout under a share: the scanner of
// its arrivals, and a way to write a file of n bytes, the byte b each.
type v2 struct {
	st    *store.Store
	cfg   config.Config
	s     *Scanner
	share string
}

func newV2(t *testing.T) *v2 {
	t.Helper()
	st := storetest.Open(t)
	share := t.TempDir()
	cfg := config.Config{NFSRoot: share + "/media", LibraryRoot: share, WorkRoot: share + "/.work",
		ArrivalsRoot: share + "/.work/incoming", ExtrasRoot: share + "/.work/extras"}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	return &v2{st: st, cfg: cfg, s: New(st, cfg, processing.New(st.Pool()), nil), share: share}
}

func (f *v2) write(t *testing.T, rel string, n int, b byte) string {
	t.Helper()
	p := filepath.Join(f.share, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Repeat(string(rune(b)), n)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *v2) scan(t *testing.T) scanResult {
	t.Helper()
	res, err := f.s.walk(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// sources lists the catalog's sources as "item-title state filename
// librarypath size", by title.
func (f *v2) sources(t *testing.T) string {
	t.Helper()
	rows, err := f.st.Pool().Query(context.Background(), `SELECT i.title || ' ' || s.state || ' ' || s.filename || ' ' ||
			COALESCE(s.librarypath, '-') || ' ' || s.sizebytes || ' ' || (p.sourceid = s.id)
		FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_items i ON i.id = s.item_id
		LEFT JOIN com_nalet_katalog_playbackassets p ON p.item_id = s.item_id AND p.isprimary
		ORDER BY i.title, s.createdat`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// With the v2 layout the scanner walks the arrivals, and nothing else of the
// share (not the media root, not a folder whose name begins with a dot): a new
// file is a title with its source, the file's name, place among the arrivals,
// size and quick hash, which its asset names. A scan that finds a file as it
// was leaves the title's modifiedat alone; a file that grew before its record
// was written gets its quick hash again, one recorded keeps its own.
func TestTheScannerTakesArrivalsInWithTheirSources(t *testing.T) {
	f := newV2(t)
	f.write(t, ".work/incoming/Sintel (2010)/Sintel (2010).mkv", 3000, 's')
	f.write(t, ".work/incoming/.hidden/Hidden (2001).mkv", 10, 'h')
	f.write(t, ".work/incoming/Tears of Steel (2012).mov", 2000, 't')
	f.write(t, "media/Elsewhere (1999).mkv", 10, 'e')
	f.write(t, ".work/staging/v1/Staged (2000).mkv", 10, 'x')
	if res := f.scan(t); res.itemsInserted != 2 {
		t.Fatalf("the first scan inserted %d titles, want the two arrivals", res.itemsInserted)
	}
	if got, want := f.sources(t), "Sintel present Sintel (2010).mkv Sintel (2010)/Sintel (2010).mkv 3000 true\n"+
		"Tears of Steel present Tears of Steel (2012).mov Tears of Steel (2012).mov 2000 true"; got != want {
		t.Errorf("the sources:\n%s\nwant:\n%s", got, want)
	}
	sintel := filepath.Join(f.cfg.ArrivalsRoot, "Sintel (2010)/Sintel (2010).mkv")
	_, qh1, _ := library.QH1(sintel)
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE arrivalpath = $1 AND qh1 = $2`,
		sintel, qh1); n != 1 {
		t.Error("Sintel's source does not hold its place and quick hash")
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	if res := f.scan(t); res.itemsInserted != 0 || res.itemsUpdated != 2 {
		t.Errorf("a scan again: %+v", res)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE modifiedat <> '2001-01-01'`); n != 0 {
		t.Errorf("a scan that found the files as they were bumped %d titles", n)
	}
	// Sintel grows; Tears of Steel, recorded, grows too.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET recordedat = now() WHERE filename LIKE 'Tears%'`)
	f.write(t, ".work/incoming/Sintel (2010)/Sintel (2010).mkv", 4000, 'S')
	f.write(t, ".work/incoming/Tears of Steel (2012).mov", 2500, 'T')
	f.scan(t)
	_, qh1, _ = library.QH1(sintel)
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE arrivalpath = $1 AND qh1 = $2
		AND sizebytes = 4000`, sintel, qh1); n != 1 {
		t.Error("Sintel's grown file did not get its quick hash again")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE filename LIKE 'Tears%' AND sizebytes = 2000`); n != 1 {
		t.Error("a recorded source took the size of a changed file")
	}
	// A title from before the v2 layout gets its source at the next scan.
	old := f.write(t, ".work/incoming/Old (1990).mkv", 100, 'o')
	storetest.AddItem(t, f.st, "01d01d01-0000-4000-8000-000000000001", "movie", "Old", "")
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a-old', $1, $2, true)`,
		"01d01d01-0000-4000-8000-000000000001", old)
	f.scan(t)
	if got := f.sources(t); !strings.HasPrefix(got, "Old present Old (1990).mkv Old (1990).mkv 100 true") {
		t.Errorf("a title from before the v2 layout:\n%s", got)
	}
}

// A file the walk meets at a new place is told by its size and quick hash: an
// original that moved takes its title along (its asset, its source and the
// subtitle files beside it), one copied is no title, nor is one that arrives
// again after it was deleted after packaging: it is in the library already.
func TestTheScannerKnowsTheLibrarysOriginals(t *testing.T) {
	f := newV2(t)
	f.write(t, ".work/incoming/Sintel.mkv", 3000, 's')
	f.write(t, ".work/incoming/Bunny.mkv", 2000, 'b')
	f.scan(t)
	var sintelID, bunnyID string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT (SELECT id FROM com_nalet_katalog_items WHERE title = 'Sintel'),
		(SELECT id FROM com_nalet_katalog_items WHERE title = 'Bunny')`).Scan(&sintelID, &bunnyID); err != nil {
		t.Fatal(err)
	}
	// Sintel moves into a folder, its subtitle file with it; Bunny is copied.
	if err := os.MkdirAll(filepath.Join(f.cfg.ArrivalsRoot, "Sintel (2010)"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(f.cfg.ArrivalsRoot, "Sintel (2010)", "Sintel.mkv")
	if err := os.Rename(filepath.Join(f.cfg.ArrivalsRoot, "Sintel.mkv"), moved); err != nil {
		t.Fatal(err)
	}
	f.write(t, ".work/incoming/Sintel (2010)/Sintel.en.srt", 10, 'x')
	f.write(t, ".work/incoming/copies/Bunny.mkv", 2000, 'b')
	if res := f.scan(t); res.itemsInserted != 0 {
		t.Errorf("a moved and a copied file made %d titles", res.itemsInserted)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_itemsources s
		ON s.id = p.sourceid WHERE p.item_id = $1 AND p.path = $2 AND s.arrivalpath = $2 AND s.librarypath = 'Sintel (2010)/Sintel.mkv'`,
		sintelID, moved); n != 1 {
		t.Error("Sintel's title did not follow its original")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND lang = 'en'`, sintelID); n != 1 {
		t.Error("the subtitle file beside the moved original is not the title's")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 2 {
		t.Errorf("%d titles, want the two", n)
	}

	// Bunny's original is deleted after packaging; it arrives again.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', arrivalpath = NULL, deletedat = now()
		WHERE item_id = $1`, bunnyID)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, bunnyID)
	if err := os.Remove(filepath.Join(f.cfg.ArrivalsRoot, "Bunny.mkv")); err != nil {
		t.Fatal(err)
	}
	if res := f.scan(t); res.itemsInserted != 0 {
		t.Errorf("an original arrived again made %d titles", res.itemsInserted)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE path LIKE '%Bunny.mkv'`); n != 0 {
		t.Errorf("the original that arrived again got %d asset rows", n)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.ArrivalsRoot, "copies", "Bunny.mkv")); err != nil {
		t.Errorf("the file that arrived again was not left alone: %v", err)
	}
}

// The scanner walks the root of the layout the settings say when it scans:
// with no root set, one that runs on through a switch walks the media root,
// then the arrivals, without a restart.
func TestTheScanRootFollowsTheLayoutWithoutARestart(t *testing.T) {
	st := storetest.Open(t)
	share := t.TempDir()
	f := &v2{st: st, cfg: config.Config{NFSRoot: share + "/media", Share: share}, share: share}
	f.s = New(st, f.cfg, processing.New(st.Pool()), nil)
	f.write(t, "media/Legacy (2001).mkv", 10, 'l')
	f.write(t, ".work/incoming/Arrival (2002).mkv", 10, 'a')
	if res := f.scan(t); res.itemsInserted != 1 {
		t.Errorf("legacy inserted %d titles, want the media root's", res.itemsInserted)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	if res := f.scan(t); res.itemsInserted != 1 {
		t.Errorf("v2 inserted %d titles, want the arrival", res.itemsInserted)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE title IN ('Legacy', 'Arrival')`); n != 2 {
		t.Errorf("%d titles, want both", n)
	}
}
