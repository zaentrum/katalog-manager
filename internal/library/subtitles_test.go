package library

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Two films for the subtitles plan: one the catalog keeps a complete version
// of, whose package holds five subtitles, and one whose version is still
// being built.
// longTitle is a commentary's title longer than the catalog keeps of a label.
var longTitle = "Commentary by the director, the editor and the composer, recorded for the anniversary " +
	"release, on how the film's ending changed between the first cut and the last"

const (
	mgSubs     = "a5a5a5a5-0000-4000-8000-00000000005a"
	mgSubsBusy = "a6a6a6a6-0000-4000-8000-00000000006a"
)

// The rows of a run's subtitles plan the catalog lacks are written as the
// catalog writes a package's when it takes it — the path in the version's
// folder, the package's format and language, its name or else its title,
// never the default, forced as the package says — each looked at again first:
// a subtitle made of a sidecar, one whose file is not there, one the package
// says otherwise of than the plan, and one of a version the catalog does not
// keep complete are left, with why; one the catalog holds by its language and
// file name is unchanged. Every row is journaled, and a second call writes
// nothing.
func TestTheSubtitlesPlanWritesTheRowsTheCatalogLacks(t *testing.T) {
	f := newMigrationFixture(t)
	ctx := context.Background()
	storetest.AddItem(t, f.st, mgSubs, "movie", "Film With Subtitles", "")
	storetest.AddItem(t, f.st, mgSubsBusy, "movie", "Film Being Built", "")
	vid, busy := IDOf(mgSubs+":version"), IDOf(mgSubsBusy+":version")
	vdir, bdir := VersionDir(f.itemDir(mgSubs), vid), VersionDir(f.itemDir(mgSubsBusy), busy)
	librarytest.Write(t, filepath.Join(vdir, PackageFile), librarytest.JSON(t, map[string]any{"subtitles": []any{
		map[string]any{"id": "s0", "path": "subs/0.sup", "format": "pgs", "language": "eng", "title": "English"},
		map[string]any{"id": "s1", "path": "subs/1.vtt", "format": "webvtt", "language": "ger", "name": " ",
			"title": "German (forced)", "forced": true},
		map[string]any{"id": "s2", "path": "subs/2.vtt", "format": "webvtt", "language": "fre",
			"fromSidecar": "sources/b1b1b1b1-0000-4000-8000-000000000001/subtitle-1.fre.srt"},
		map[string]any{"id": "s3", "path": "subs/3.vtt", "format": "webvtt", "language": "ita"},
		map[string]any{"id": "s4", "path": "subs/4.vtt", "format": "webvtt", "language": "spa"},
		map[string]any{"id": "s5", "path": "subs/5.vtt", "format": "webvtt", "language": "eng", "title": longTitle},
	}}))
	for _, n := range []string{"0.sup", "1.vtt", "2.vtt", "3.vtt", "5.vtt"} { // 4.vtt is not there
		librarytest.Write(t, filepath.Join(vdir, "subs", n), []byte("WEBVTT\n"))
	}
	librarytest.Write(t, filepath.Join(bdir, PackageFile), librarytest.JSON(t, map[string]any{"subtitles": []any{
		map[string]any{"id": "s0", "path": "subs/0.vtt", "format": "webvtt", "language": "eng"}}}))
	librarytest.Write(t, filepath.Join(bdir, "subs", "0.vtt"), []byte("WEBVTT\n"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir) VALUES ($1, $2, 'complete', $3)`,
		vid, mgSubs, vdir)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir) VALUES ($1, $2, 'building', $3)`,
		busy, mgSubsBusy, bdir)
	// the Italian one the catalog holds already, where the store before kept it
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault, isforced)
		VALUES ('held-ita', $1, '/store/before/subs/3.vtt', 'webvtt', 'ita', NULL, true, false)`, mgSubs)

	row := func(item, dir, rel, format, lang string, label any, forced bool) map[string]any {
		return map[string]any{"itemId": item, "path": filepath.Join(dir, rel), "format": format, "lang": lang,
			"label": label, "isdefault": false, "isforced": forced}
	}
	librarytest.Write(t, filepath.Join(f.runDir, SubtitlesPlan), librarytest.JSON(t, map[string]any{"rows": []any{
		row(mgSubs, vdir, "subs/0.sup", "pgs", "eng", "English", false),
		row(mgSubs, vdir, "subs/1.vtt", "webvtt", "ger", "German (forced)", true),
		row(mgSubs, vdir, "subs/2.vtt", "webvtt", "fre", nil, false),
		row(mgSubs, vdir, "subs/3.vtt", "webvtt", "ita", nil, false),
		row(mgSubs, vdir, "subs/4.vtt", "webvtt", "spa", nil, false),
		row(mgSubs, vdir, "subs/5.vtt", "webvtt", "eng", longTitle, false),
		row(mgSubs, vdir, "subs/0.sup", "pgs", "dut", "English", false),
		row(mgSubsBusy, bdir, "subs/0.vtt", "webvtt", "eng", nil, false),
	}}))

	rep, err := f.migration(t).Subtitles(ctx, nil)
	if err != nil || rep.Rows != 3 || rep.Unchanged != 1 || len(rep.Skipped) != 4 {
		t.Fatalf("Subtitles: %+v, %v", rep, err)
	}
	reasons := map[string]string{}
	for _, s := range rep.Skipped {
		reasons[filepath.Base(s.Path)+" "+s.ItemID[:4]] += s.Reason + ";"
	}
	for key, want := range map[string]string{
		"2.vtt " + mgSubs[:4]:     "made of a sidecar",
		"4.vtt " + mgSubs[:4]:     "its file is not there",
		"0.sup " + mgSubs[:4]:     "says otherwise of it than the plan",
		"0.vtt " + mgSubsBusy[:4]: "as building, not complete",
	} {
		if !strings.Contains(reasons[key], want) {
			t.Errorf("%s left %q, want a reason with %q (all: %v)", key, reasons[key], want, reasons)
		}
	}
	var got string
	if err := f.st.Pool().QueryRow(ctx, `SELECT string_agg(path || ' ' || format || ' ' || lang || ' ' || coalesce(label, '-')
			|| ' default=' || isdefault || ' forced=' || isforced, E'\n' ORDER BY path)
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND id <> 'held-ita'`, mgSubs).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(vdir, "subs/0.sup") + " pgs eng English default=false forced=false\n" +
		filepath.Join(vdir, "subs/1.vtt") + " webvtt ger German (forced) default=false forced=true\n" +
		filepath.Join(vdir, "subs/5.vtt") + " webvtt eng " + string([]rune(longTitle)[:LabelWidth-1]) + "… default=false forced=false"; got != want {
		t.Errorf("the rows written:\n%s\nwant\n%s", got, want)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, mgSubs); n != 4 {
		t.Errorf("the item holds %d subtitle rows, want the 3 written and the 1 held", n)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, mgSubsBusy); n != 0 {
		t.Errorf("the film being built got %d subtitle rows", n)
	}
	if states := journalStates(t, filepath.Join(f.runDir, SubtitlesJournal)); states != "skipped:4 unchanged:1 written:3" {
		t.Errorf("the journal: %s", states)
	}

	rep, err = f.migration(t).Subtitles(ctx, nil)
	if err != nil || rep.Rows != 0 || rep.Unchanged != 4 || len(rep.Skipped) != 4 {
		t.Errorf("Subtitles again: %+v, %v", rep, err)
	}
	rep, err = f.migration(t).Subtitles(ctx, []string{mgSubsBusy})
	if err != nil || rep.Rows+rep.Unchanged != 0 || len(rep.Skipped) != 1 {
		t.Errorf("Subtitles of the one item: %+v, %v", rep, err)
	}
}

// A run without a subtitles plan writes nothing and journals nothing.
func TestARunWithoutASubtitlesPlanWritesNothing(t *testing.T) {
	f := newMigrationFixture(t)
	rep, err := f.migration(t).Subtitles(context.Background(), nil)
	if err != nil || rep.Rows+rep.Unchanged+len(rep.Skipped) != 0 {
		t.Errorf("Subtitles: %+v, %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(f.runDir, SubtitlesJournal)); !os.IsNotExist(err) {
		t.Errorf("a journal was made: %v", err)
	}
}

// journalStates counts a journal's lines by state, as "state:n" sorted.
func journalStates(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	n := map[string]int{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var e JournalEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Op == "subtitle" {
			n[e.State]++
		}
	}
	out := []string{}
	for _, k := range []string{"skipped", "unchanged", "written"} {
		if n[k] > 0 {
			out = append(out, k+":"+strconv.Itoa(n[k]))
		}
	}
	return strings.Join(out, " ")
}
