package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A trailer that was once scanned as an item of its own moves to its movie;
// the item it leaves behind without files is removed, and the deletion log
// says the scanner removed it, and why.
func TestAttachTrailerRecordsTheItemItRemoves(t *testing.T) {
	st := storetest.Open(t)
	const movie, orphan = "f1f1f1f1-0000-4000-8000-000000000001", "f2f2f2f2-0000-4000-8000-000000000002"
	root := t.TempDir()
	dir := filepath.Join(root, "Example (2024)")
	feature := filepath.Join(dir, "Example (2024).mkv")
	trailer := filepath.Join(dir, "trailers", "Example (2024)-trailer.mkv")
	for _, f := range []string{feature, trailer} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, movie, "movie", "Example", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-feature', $1, $2, true)`, movie, feature)
	storetest.AddItem(t, st, orphan, "movie", "Example (2024)-trailer", "")
	// Not primary, so the parent lookup (any primary file in the movie's
	// folder) can only find the feature.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-trailer', $1, $2, false)`, orphan, trailer)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
		VALUES ('step-orphan', $1, 'scan', 'done')`, orphan)

	s := New(st, config.Config{NFSRoot: root}, processing.New(st.Pool()), nil)
	var res scanResult
	s.attachTrailer(context.Background(), st.Pool(), trailer, trailer, &res)

	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets
		WHERE id = 'asset-trailer' AND item_id = $1 AND kind = 'trailer'`, movie) != 1 {
		t.Fatal("the trailer did not move to its movie")
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, orphan) != 0 {
		t.Fatal("the item left without files was not removed")
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1`, orphan) != 0 {
		t.Error("the removed item's processing steps were left behind")
	}
	d, ok := storetest.Deleted(t, st, orphan)
	if !ok {
		t.Fatal("the scanner removed an item without a row in the deletion log")
	}
	if d.DeletedBy != deletedByScanner || d.Type != "movie" || d.Title != "Example (2024)-trailer" ||
		d.Reason == nil || !strings.Contains(*d.Reason, movie) {
		t.Fatalf("log row = %+v, reason %v; want the scanner, naming the movie", d, d.Reason)
	}
	if _, ok := storetest.Deleted(t, st, movie); ok {
		t.Fatal("the movie is in the deletion log")
	}
}

// The subtitle files beside a film are recorded, each with the language its
// name gives and a label, and none is the default: not the first the
// directory lists (Film.de.srt before Film.en.srt), and not one an earlier
// scan marked so. A file of another film, or one that is no subtitle, is not
// the film's.
func TestSubtitleFilesBesideAFilmAreNeverTheDefault(t *testing.T) {
	st := storetest.Open(t)
	const movie = "f3f3f3f3-0000-4000-8000-000000000003"
	root := t.TempDir()
	dir := filepath.Join(root, "Example (2024)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	feature := filepath.Join(dir, "Example (2024).mkv")
	for _, f := range []string{"Example (2024).mkv", "Example (2024).de.srt", "Example (2024).en.srt",
		"Example (2024).vtt", "Example (2024).pt-BR.ass", "Other (2020).en.srt", "Example (2024).nfo"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	storetest.AddItem(t, st, movie, "movie", "Example", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('asset-feature', $1, $2, true)`, movie, feature)
	// an earlier scan marked the first file the default
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault)
		VALUES ('sub-de', $1, $2, 'srt', 'de', 'Deutsch', true)`, movie, filepath.Join(dir, "Example (2024).de.srt"))

	s := New(st, config.Config{NFSRoot: root}, processing.New(st.Pool()), nil)
	for run := 1; run <= 2; run++ {
		s.scanSidecars(context.Background(), st.Pool(), feature, movie)
		rows, err := st.Pool().Query(context.Background(), `SELECT path, format, COALESCE(lang, '-'), label, isdefault
			FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY path`, movie)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var path, format, lang, label string
			var def bool
			if err := rows.Scan(&path, &format, &lang, &label, &def); err != nil {
				t.Fatal(err)
			}
			got = append(got, strings.Join([]string{filepath.Base(path), format, lang, label, strconv.FormatBool(def)}, " "))
		}
		rows.Close()
		want := []string{
			"Example (2024).de.srt srt de Deutsch false",
			"Example (2024).en.srt srt en English false",
			"Example (2024).pt-BR.ass ass pt-br Português false",
			"Example (2024).vtt vtt - Subtitles false",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("scan %d:\n%s\nwant:\n%s", run, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = 'sub-de'`); n != 1 {
		t.Error("the file an earlier scan recorded did not keep its row")
	}
}
