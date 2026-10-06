package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An item is recorded once: its folder gets item.json, its identity as the
// catalog holds it (the moment it was created among it, its reference ids
// as a record names them), and the checksums that list exactly it; the
// catalog notes it. A series is recorded before its first episode, which
// names it and its numbers. Recording again writes nothing.
func TestAnItemIsRecordedOnce(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	p := PathsOf(config.Config{LibraryRoot: t.TempDir()})
	storetest.AddItem(t, st, seriesID, "series", "Pioneer One", "")
	storetest.AddItem(t, st, seasonID, "season", "Season 1", seriesID)
	storetest.AddItem(t, st, nestedID, "episode", " Alone in\nthe Night ", seasonID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET createdat = '2026-09-01 10:20:30.456', seasonnumber = 1,
		episodenumber = 2, createdby = NULL WHERE id = $1`, nestedID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET createdat = '2026-09-01 10:00:00', createdby = 'katalog-manager/ingest'
		WHERE id = $1`, seriesID)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid) VALUES
		('x1', $1, 'tmdb', '39272'), ('x2', $1, 'imdb', 'tt1748166'), ('x3', $1, 'omdb', 'z'), ('x4', $2, 'tmdb-episode', '937631'),
		('x5', $2, 'imdb', 'not-an-id'), ('x6', $1, 'tvdb', '182701')`, seriesID, nestedID)

	dir, err := p.EnsureItemRecord(ctx, st.Pool(), nestedID)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(p.SeriesDir(seriesID), "episodes", nestedID); dir != want {
		t.Errorf("the episode's folder: %s, want %s", dir, want)
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	episode := `{
  "schema": "zaentrum.library.item/2",
  "itemId": "` + nestedID + `",
  "type": "episode",
  "title": "Alone in the Night",
  "externalIds": {
    "tmdbEpisode": "937631"
  },
  "createdAt": "2026-09-01T10:20:30Z",
  "createdBy": "katalog-manager/scanner",
  "seriesId": "` + seriesID + `",
  "seasonNumber": 1,
  "episodeNumber": 2,
  "episodeCode": "S01E02"
}
`
	if got := read(filepath.Join(dir, ItemFile)); got != episode {
		t.Errorf("the episode's item.json:\n%s\nwant:\n%s", got, episode)
	}
	if got, want := read(filepath.Join(dir, SumsFile)), SHA256([]byte(episode))+"  item.json\n"; got != want {
		t.Errorf("its checksums: %q, want %q", got, want)
	}
	series := read(filepath.Join(p.SeriesDir(seriesID), ItemFile))
	if !strings.Contains(series, `"externalIds": {
    "imdb": "tt1748166",
    "tmdbTv": "39272",
    "tvdb": "182701"
  },
  "createdAt": "2026-09-01T10:00:00Z",
  "createdBy": "katalog-manager/ingest"
}`) || strings.Contains(series, "seriesId") {
		t.Errorf("the series' item.json:\n%s", series)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE recordedat IS NOT NULL`); n != 2 {
		t.Errorf("%d items recorded, want the series and the episode", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND recordedat IS NULL`, seasonID); n != 1 {
		t.Error("the season was recorded")
	}

	// Again, and with the catalog changed: nothing is written again.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET title = 'Renamed' WHERE id = $1`, nestedID)
	if _, err := p.EnsureItemRecord(ctx, st.Pool(), nestedID); err != nil {
		t.Fatal(err)
	}
	if got := read(filepath.Join(dir, ItemFile)); got != episode {
		t.Errorf("recorded again, the record changed:\n%s", got)
	}
	// A folder recorded already (a record written, the catalog not told) is
	// noted, not written again; one cut short before its checksums is.
	const film = "f2f2f2f2-0000-4000-8000-000000000009"
	storetest.AddItem(t, st, film, "movie", "A Film", "")
	fdir := p.MovieDir(film)
	if err := WriteCovered(fdir, map[string][]byte{ItemFile: []byte("{\"written\": \"elsewhere\"}\n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EnsureItemRecord(ctx, st.Pool(), film); err != nil {
		t.Fatal(err)
	}
	if got := read(filepath.Join(fdir, ItemFile)); got != "{\"written\": \"elsewhere\"}\n" {
		t.Errorf("a complete record was written again: %s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND recordedat IS NOT NULL`, film); n != 1 {
		t.Error("a folder recorded already was not noted")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET recordedat = NULL WHERE id = $1`, film)
	if err := os.Remove(filepath.Join(fdir, SumsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EnsureItemRecord(ctx, st.Pool(), film); err != nil {
		t.Fatal(err)
	}
	if got := read(filepath.Join(fdir, ItemFile)); !strings.Contains(got, `"title": "A Film"`) {
		t.Errorf("a record cut short was not written again: %s", got)
	}
}

// What cannot be recorded says why, and writes nothing: an episode without
// its numbers or its series, with a negative number, a title without a title,
// a season; an episode whose series cannot be recorded says so.
func TestWhatCannotBeRecorded(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	root := t.TempDir()
	p := PathsOf(config.Config{LibraryRoot: root})
	storetest.AddItem(t, st, seriesID, "series", "Pioneer One", "")
	storetest.AddItem(t, st, episodeID, "episode", "Earthfall", seriesID)
	storetest.AddItem(t, st, nestedID, "episode", "Negative", seriesID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = -1, episodenumber = 1 WHERE id = $1`, nestedID)
	storetest.AddItem(t, st, movieID, "movie", " ", "")
	storetest.AddItem(t, st, seasonID, "season", "Season 1", seriesID)
	const blank = "b1b1b1b1-0000-4000-8000-000000000008"
	storetest.AddItem(t, st, blank, "series", "\n", "")
	const ofBlank = "b2b2b2b2-0000-4000-8000-000000000009"
	storetest.AddItem(t, st, ofBlank, "episode", "Of a blank series", blank)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = $1`, ofBlank)
	for id, says := range map[string]string{
		episodeID: "an episode needs its season and episode numbers before it is recorded",
		nestedID:  "an episode is recorded with numbers of 0 or more, not S-1E1",
		movieID:   "an item needs a title before it is recorded",
		seasonID:  "a season is not recorded in the library",
		ofBlank:   "its series " + blank + " is not recorded: an item needs a title",
	} {
		_, err := p.EnsureItemRecord(ctx, st.Pool(), id)
		var b *Blocked
		if !errors.As(err, &b) || !strings.Contains(b.Reason, says) {
			t.Errorf("%s: %v, want it blocked saying %q", id, err, says)
		}
	}
	if _, err := p.EnsureItemRecord(ctx, st.Pool(), "nothing"); !errors.Is(err, ErrNoItem) {
		t.Errorf("an item there is not: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, MoviesDir)); len(entries) != 0 {
		t.Errorf("a blocked movie left %d folders", len(entries))
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE recordedat IS NOT NULL AND id <> $1`, seriesID); n != 0 {
		t.Errorf("%d blocked items recorded", n)
	}
}
