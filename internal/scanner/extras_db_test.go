package scanner

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// sender records the extras whose triggers the scanner sends.
type sender struct {
	mu   sync.Mutex
	sent []string
}

func (s *sender) Send(_ context.Context, ids []string, source string) (int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.sent = append(s.sent, source+" "+id)
	}
	return len(ids), 0, nil
}

func (s *sender) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sent
	s.sent = nil
	return out
}

// media writes the files, each a few bytes of its own name, under a new
// media root, and answers the root.
func media(t *testing.T, files ...string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		put(t, root, f)
	}
	return root
}

func put(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("the file "+rel), 0o644); err != nil {
		t.Fatal(err)
	}
}

// walker is a scanner of root, with the convention on or off, and what it
// sends.
func walker(t *testing.T, st *store.Store, root string, on bool) (*Scanner, *sender) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE key = 'extras.scan'`)
	if on {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('s-extras', 'extras.scan', ' On ')`)
	}
	snd := &sender{}
	return New(st, config.Config{NFSRoot: root}, processing.New(st.Pool()), nil).WithExtras(snd), snd
}

func walk(t *testing.T, s *Scanner) {
	t.Helper()
	if _, err := s.walk(context.Background(), func() {}); err != nil {
		t.Fatal(err)
	}
}

// itemOf is the item whose file is at rel, "" when there is none.
func itemOf(t *testing.T, st *store.Store, root, rel string) string {
	t.Helper()
	var id string
	_ = st.Pool().QueryRow(context.Background(), `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1`,
		filepath.Join(root, rel)).Scan(&id)
	return id
}

// extrasOf are the extras the catalog holds, one per line, by file: "<the
// file of its title> <kind> <title> <its file> <state> <season>", the files
// relative to root, - for none.
func extrasOf(t *testing.T, st *store.Store, root string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT COALESCE(p.path, x.item_id), x.kind, x.title, x.sourcepath,
		x.state, COALESCE(x.seasonnumber::text, '-'), x.hidden, x.registeredby
		FROM com_nalet_katalog_itemextras x
		LEFT JOIN com_nalet_katalog_playbackassets p ON p.item_id = x.item_id AND p.isprimary
		LEFT JOIN com_nalet_katalog_items i ON i.id = x.item_id
		WHERE x.removedat IS NULL AND (p.path IS NOT NULL OR i.type = 'series')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var title, kind, name, path, state, season, by string
		var hidden bool
		if err := rows.Scan(&title, &kind, &name, &path, &state, &season, &hidden, &by); err != nil {
			t.Fatal(err)
		}
		rel := func(p string) string { return strings.TrimPrefix(p, root+"/") }
		line := rel(title) + " | " + kind + " | " + name + " | " + rel(path) + " | " + state + " | " + season
		if hidden {
			line += " | hidden"
		}
		if by != "scanner" {
			line += " | " + by
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// countOf counts what a query counts.
func countOf(t *testing.T, st *store.Store, sql string, args ...any) int {
	return storetest.Count(t, st, sql, args...)
}

// With the convention off a file the scanner always took for a trailer is
// skipped, no extra is taken in, and no trailer row is written; the trailer
// row the scanner once wrote for a file it meets goes. Every other file is
// an item, as before.
func TestNoExtrasWithTheConventionOff(t *testing.T) {
	st := storetest.Open(t)
	root := media(t, "A.mkv", "A-trailer.mkv", "B.mkv", "B - Featurette.mkv", "trailers/X.mkv", "Film/extras/Y.mkv")
	storetest.AddItem(t, st, "b0b0b0b0-0000-4000-8000-000000000001", "movie", "B", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind) VALUES
		('b', 'b0b0b0b0-0000-4000-8000-000000000001', $1, true, 'primary'),
		('legacy', 'b0b0b0b0-0000-4000-8000-000000000001', $2, false, 'trailer')`, root+"/B.mkv", root+"/A-trailer.mkv")
	s, snd := walker(t, st, root, false)
	walk(t, s)
	for _, rel := range []string{"A.mkv", "B.mkv", "B - Featurette.mkv", "Film/extras/Y.mkv"} {
		if itemOf(t, st, root, rel) == "" {
			t.Errorf("%s is no item", rel)
		}
	}
	if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE path LIKE '%trailer%' OR kind = 'trailer'`); n != 0 {
		t.Errorf("%d assets of trailers, the scanner's old trailer row among them", n)
	}
	if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras`); n != 0 || len(snd.take()) != 0 {
		t.Errorf("%d extras taken in with the convention off", n)
	}
}

// In a flat folder of many titles' files, a file is an extra of the title
// whose file it names, the longest name winning, and of no other: a file
// that names none, or a name two titles' files share, is skipped, and none
// of them is an item. The extras taken in wait to be packaged, and their
// triggers go.
func TestAFlatFolderAttachesOnlyToTheFileItNames(t *testing.T) {
	st := storetest.Open(t)
	root := media(t, "A.mkv", "A-trailer.mkv", "A - Behind the Scenes - Music.mkv", "B.mkv", "B.teaser.2.mp4",
		"B 2.mkv", "B 2-trailer.mkv", "trailer.mkv", "C-trailer.mkv", "D.mkv", "D.mp4", "D-trailer.mkv")
	s, snd := walker(t, st, root, true)
	walk(t, s)
	want := `A.mkv | behind-the-scenes | Music | A - Behind the Scenes - Music.mkv | pending | -
A.mkv | trailer | Trailer | A-trailer.mkv | pending | -
B 2.mkv | trailer | Trailer | B 2-trailer.mkv | pending | -
B.mkv | teaser | Teaser 2 | B.teaser.2.mp4 | pending | -`
	if got := extrasOf(t, st, root); got != want {
		t.Errorf("extras:\n%s\nwant:\n%s", got, want)
	}
	if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 5 {
		t.Errorf("%d items, want A, B, B 2 and both D", n)
	}
	if sent := snd.take(); len(sent) != 4 || !strings.HasPrefix(sent[0], "scanner ") {
		t.Errorf("sent %v, want the four extras by the scanner", sent)
	}
	// A walk again finds the same extras; those still waiting to be sent
	// (the stand-in sends nothing) are sent again, those sent are not.
	walk(t, s)
	if got := extrasOf(t, st, root); got != want {
		t.Errorf("a second walk:\n%s", got)
	}
	if sent := snd.take(); len(sent) != 4 {
		t.Errorf("the second walk sent %d, want the four still waiting", len(sent))
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued'`)
	walk(t, s)
	if sent := snd.take(); len(sent) != 0 {
		t.Errorf("a walk sent the triggers of extras sent already: %v", sent)
	}
}

// A folder of extras beside one title's file is that title's extras, a kind
// its file's name says winning over the folder's, and so is a file named by
// a kind alone beside it; beside the files of several titles it is no one's.
// A film named by a word that is a kind of extras too ("Other") is a film.
func TestFoldersOfExtras(t *testing.T) {
	st := storetest.Open(t)
	root := media(t, "Movie (2020)/Movie (2020).mkv", "Movie (2020)/trailers/Official.mkv",
		"Movie (2020)/extras/Behind the Scenes.mkv", "Movie (2020)/featurettes/teaser.mkv", "Movie (2020)/trailer-2.mkv",
		"Movie (2020)/Deleted Scenes/The Diner.mkv",
		"Movie (2020)/featurettes/Movie (2020) - The Score.mkv", "Movie (2020)/interviews/Cast - Interview.mkv",
		"Flat1.mkv", "Flat2.mkv", "trailers/Ambiguous.mkv", "Two/One.mkv", "Two/Other.mkv", "Two/extras/x.mkv",
		"Empty/extras/y.mkv", "Shorts/Short.mkv")
	s, _ := walker(t, st, root, true)
	walk(t, s)
	want := `Movie (2020)/Movie (2020).mkv | behind-the-scenes | Behind the Scenes | Movie (2020)/extras/Behind the Scenes.mkv | pending | -
Movie (2020)/Movie (2020).mkv | deleted-scene | The Diner | Movie (2020)/Deleted Scenes/The Diner.mkv | pending | -
Movie (2020)/Movie (2020).mkv | featurette | The Score | Movie (2020)/featurettes/Movie (2020) - The Score.mkv | pending | -
Movie (2020)/Movie (2020).mkv | interview | Interview | Movie (2020)/interviews/Cast - Interview.mkv | pending | -
Movie (2020)/Movie (2020).mkv | teaser | Teaser | Movie (2020)/featurettes/teaser.mkv | pending | -
Movie (2020)/Movie (2020).mkv | trailer | Official | Movie (2020)/trailers/Official.mkv | pending | -
Movie (2020)/Movie (2020).mkv | trailer | Trailer 2 | Movie (2020)/trailer-2.mkv | pending | -`
	if got := extrasOf(t, st, root); got != want {
		t.Errorf("extras:\n%s\nwant:\n%s", got, want)
	}
	for _, rel := range []string{"trailers/Ambiguous.mkv", "Two/extras/x.mkv", "Empty/extras/y.mkv"} {
		if itemOf(t, st, root, rel) != "" {
			t.Errorf("%s is an item", rel)
		}
	}
	for _, rel := range []string{"Two/One.mkv", "Two/Other.mkv", "Shorts/Short.mkv"} {
		if itemOf(t, st, root, rel) == "" {
			t.Errorf("%s is no item", rel)
		}
	}
}

// A show's folders of extras are the extras of the one series its episodes
// belong to: series/<Show>/trailers/ of the series, series/<Show>/Season
// 01/extras/ of its first season; a season it has no episodes of is no
// one's.
func TestAShowsExtrasAreItsSeries(t *testing.T) {
	st := storetest.Open(t)
	root := media(t, "series/Pioneer One/Season 01/Pioneer One S01E01.mkv", "series/Pioneer One/Season 01/Pioneer One S01E02.mkv",
		"series/Pioneer One/Season 01/extras/Making of.mkv", "series/Pioneer One/trailers/Pioneer One - Trailer.mkv",
		"series/Pioneer One/Season 05/extras/x.mkv")
	s, _ := walker(t, st, root, true)
	walk(t, s)
	series := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE type = 'series'`)
	if series != 1 {
		t.Fatalf("%d series", series)
	}
	var id string
	if err := st.Pool().QueryRow(context.Background(), `SELECT id FROM com_nalet_katalog_items WHERE type = 'series'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	want := id + ` | making-of | Making Of | series/Pioneer One/Season 01/extras/Making of.mkv | pending | 1
` + id + ` | trailer | Trailer | series/Pioneer One/trailers/Pioneer One - Trailer.mkv | pending | -`
	if got := extrasOf(t, st, root); got != want {
		t.Errorf("extras:\n%s\nwant:\n%s", got, want)
	}
	if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE type = 'episode'`); n != 2 {
		t.Errorf("%d episodes, want the two", n)
	}
}

// An extra's file moved, its size and quick hash unchanged, is the same
// extra at its new path, with its id and its package; one gone is missing
// and hidden until it is back, ready again; one the scanner did not take in
// is not its to mark. A walk of a root that is not there marks nothing.
func TestAnExtrasFileMovedOrGone(t *testing.T) {
	st := storetest.Open(t)
	root := media(t, "A.mkv", "A-trailer.mkv")
	s, _ := walker(t, st, root, true)
	walk(t, s)
	var id string
	if err := st.Pool().QueryRow(context.Background(), `SELECT id FROM com_nalet_katalog_itemextras`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'ready', packagedat = now(), packagepath = '/p/extras/x'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		SELECT 'by-hand', item_id, 'teaser', 'Teaser', 'api', $1 FROM com_nalet_katalog_itemextras`, root+"/A-teaser-gone.mkv")

	if err := os.Rename(root+"/A-trailer.mkv", root+"/A.trailer.mkv"); err != nil {
		t.Fatal(err)
	}
	walk(t, s)
	if got, want := extrasOf(t, st, root), "A.mkv | teaser | Teaser | A-teaser-gone.mkv | pending | - | api\n"+
		"A.mkv | trailer | Trailer | A.trailer.mkv | ready | -"; got != want {
		t.Errorf("moved:\n%s\nwant:\n%s", got, want)
	}
	if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE id = $1 AND packagepath = '/p/extras/x'`, id); n != 1 {
		t.Error("the moved extra lost its id or its package")
	}

	if err := os.Rename(root+"/A.trailer.mkv", root+"/away.bin"); err != nil {
		t.Fatal(err)
	}
	walk(t, s)
	if got := extrasOf(t, st, root); !strings.Contains(got, "A.trailer.mkv | missing | - | hidden") || !strings.Contains(got, "A-teaser-gone.mkv | pending") {
		t.Errorf("gone:\n%s", got)
	}
	gone, _ := walker(t, st, root+"/unmounted", true)
	walk(t, gone)
	if err := os.Rename(root+"/away.bin", root+"/A.trailer.mkv"); err != nil {
		t.Fatal(err)
	}
	walk(t, s)
	if got := extrasOf(t, st, root); !strings.Contains(got, "A.mkv | trailer | Trailer | A.trailer.mkv | ready | -\n") &&
		!strings.HasSuffix(got, "A.mkv | trailer | Trailer | A.trailer.mkv | ready | -") {
		t.Errorf("back:\n%s", got)
	}
}

// A trailer once scanned as an item of its own becomes an extra of its film;
// the item it leaves without files is removed, and the deletion log says the
// scanner removed it, and why.
func TestATrailerOnceAnItemOfItsOwnBecomesAnExtra(t *testing.T) {
	st := storetest.Open(t)
	const movie, orphan = "f1f1f1f1-0000-4000-8000-000000000001", "f2f2f2f2-0000-4000-8000-000000000002"
	root := media(t, "Example (2024)/Example (2024).mkv", "Example (2024)/trailers/Example (2024)-trailer.mkv")
	storetest.AddItem(t, st, movie, "movie", "Example", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-feature', $1, $2, true)`, movie, root+"/Example (2024)/Example (2024).mkv")
	storetest.AddItem(t, st, orphan, "movie", "Example (2024)-trailer", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-trailer', $1, $2, true)`, orphan, root+"/Example (2024)/trailers/Example (2024)-trailer.mkv")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('step-orphan', $1, 'scan', 'done')`, orphan)
	s, _ := walker(t, st, root, true)
	walk(t, s)
	if got, want := extrasOf(t, st, root), "Example (2024)/Example (2024).mkv | trailer | Trailer | "+
		"Example (2024)/trailers/Example (2024)-trailer.mkv | pending | -"; got != want {
		t.Errorf("extras:\n%s\nwant:\n%s", got, want)
	}
	if countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, orphan) != 0 ||
		countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, orphan) != 0 {
		t.Fatal("the item left without files was not removed with its rows")
	}
	d, ok := storetest.Deleted(t, st, orphan)
	if !ok || d.DeletedBy != deletedByScanner || d.Reason == nil || *d.Reason != "its file is an extra of item "+movie {
		t.Fatalf("log row = %+v, %v; want the scanner, naming the film", d, ok)
	}
	if _, ok := storetest.Deleted(t, st, movie); ok {
		t.Fatal("the film is in the deletion log")
	}
}

// The trailer row the scanner once wrote for a file goes when it meets the
// file, whatever the setting, and none is written again.
func TestTheScannersTrailerRowsGoWhenMet(t *testing.T) {
	for _, on := range []bool{false, true} {
		st := storetest.Open(t)
		root := media(t, "A.mkv", "B.mkv", "A-trailer.mkv")
		storetest.AddItem(t, st, "b0b0b0b0-0000-4000-8000-000000000001", "movie", "B", "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind) VALUES
			('b', 'b0b0b0b0-0000-4000-8000-000000000001', $1, true, 'primary'),
			('legacy', 'b0b0b0b0-0000-4000-8000-000000000001', $2, false, 'trailer')`, root+"/B.mkv", root+"/A-trailer.mkv")
		s, _ := walker(t, st, root, on)
		walk(t, s)
		walk(t, s)
		if n := countOf(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE kind = 'trailer' OR path LIKE '%-trailer.mkv'`); n != 0 {
			t.Errorf("on %v: %d trailer rows", on, n)
		}
		if got := extrasOf(t, st, root); on != (got == "A.mkv | trailer | Trailer | A-trailer.mkv | pending | -") {
			t.Errorf("on %v: extras %q", on, got)
		}
	}
}
