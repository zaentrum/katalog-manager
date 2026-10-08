package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// legacy is a catalog with the legacy layout and its media root, and its
// scanner.
type legacy struct {
	st   *store.Store
	root string
	s    *Scanner
}

func newLegacy(t *testing.T, st *store.Store) *legacy {
	t.Helper()
	root := t.TempDir()
	return &legacy{st: st, root: root, s: New(st, config.Config{NFSRoot: root}, processing.New(st.Pool()), nil)}
}

func (f *legacy) write(t *testing.T, rel string) string {
	t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *legacy) scan(t *testing.T) scanResult {
	t.Helper()
	res, err := f.s.walk(context.Background(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// episodes lists the catalog's episodes as "SxxEyy file|- covered-by|-", by
// season and number, the holder named by its numbers.
func (f *legacy) episodes(t *testing.T) string {
	t.Helper()
	rows, err := f.st.Pool().Query(context.Background(), `SELECT format('S%sE%s', lpad(e.seasonnumber::text, 2, '0'),
			lpad(e.episodenumber::text, 2, '0')),
			COALESCE((SELECT a.path FROM com_nalet_katalog_playbackassets a WHERE a.item_id = e.id AND a.isprimary), '-'),
			COALESCE((SELECT format('S%sE%s', lpad(h.seasonnumber::text, 2, '0'), lpad(h.episodenumber::text, 2, '0'))
			          FROM com_nalet_katalog_items h WHERE h.id = e.coveredby), '-')
		FROM com_nalet_katalog_items e WHERE e.type = 'episode'
		ORDER BY e.seasonnumber, e.episodenumber, e.createdat`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var code, path, by string
		if err := rows.Scan(&code, &path, &by); err != nil {
			t.Fatal(err)
		}
		if path != "-" {
			path = filepath.Base(path)
		}
		out = append(out, code+" "+path+" "+by)
	}
	return strings.Join(out, "\n")
}

// episodeID is the id of the catalog's episode SxxEyy of season s, number e.
func (f *legacy) episodeID(t *testing.T, s, e int) string {
	t.Helper()
	var id string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT id FROM com_nalet_katalog_items
		WHERE type = 'episode' AND seasonnumber = $1 AND episodenumber = $2 ORDER BY createdat, id LIMIT 1`, s, e).Scan(&id); err != nil {
		t.Fatalf("episode S%02dE%02d: %v", s, e, err)
	}
	return id
}

// A file whose name numbers several episodes is its first one's, and covers
// the others of its series and season: one the catalog has is linked to it,
// one it has not is made, its scan step done, and neither runs a step of a
// file, each saying whose file covers it. An episode with a file of its own
// is left alone, its own file winning, and the scan's report says so; a
// resolution after a dash is no episode. A scan again changes nothing, and
// one that finds the file's name numbering one episode no more covers the
// others no more.
func TestAFileOfSeveralEpisodesCoversTheOthers(t *testing.T) {
	st := storetest.Open(t)
	f := newLegacy(t, st)
	ctx := context.Background()
	// The series is there, with an episode of no file yet (S05E18).
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, createdat, modifiedat)
		VALUES ('show', 'series', 'Show', 'show', now(), now())`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, parent_id, seasonnumber, episodenumber, createdat, modifiedat)
		VALUES ('e18', 'episode', 'The Eighteenth', 'show', 5, 18, now(), now())`)
	f.write(t, "series/Show/Show.S05E15-E16.mkv")
	f.write(t, "series/Show/Show.S05E17E18.mkv")
	both := f.write(t, "series/Show/Show.S05E19-20.mkv")
	f.write(t, "series/Show/Show.S05E20.mkv")
	f.write(t, "series/Show/Show.S05E21-720p.mkv")
	res := f.scan(t)
	if res.itemsInserted != 6 {
		t.Errorf("%d items made, want the five files' and S05E16's", res.itemsInserted)
	}
	want := "S05E15 Show.S05E15-E16.mkv -\nS05E16 - S05E15\nS05E17 Show.S05E17E18.mkv -\nS05E18 - S05E17\n" +
		"S05E19 Show.S05E19-20.mkv -\nS05E20 Show.S05E20.mkv -\nS05E21 Show.S05E21-720p.mkv -"
	if got := f.episodes(t); got != want {
		t.Errorf("the episodes:\n%s\nwant:\n%s", got, want)
	}
	e15, e16, e20 := f.episodeID(t, 5, 15), f.episodeID(t, 5, 16), f.episodeID(t, 5, 20)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND parent_id = 'show'
		AND title = 'Show' AND seasonnumber = 5 AND episodenumber = 16`, e16); n != 1 {
		t.Error("S05E16 is not made as the scan makes an episode, under its series")
	}
	reason := processing.CoveredReason(e15)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2 AND step = ANY($3)`, e16, reason, processing.CoveredSteps); n != len(processing.CoveredSteps) {
		t.Errorf("%d of S05E16's steps of a file do not apply, saying whose file covers it; want all %d", n, len(processing.CoveredSteps))
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND step = 'scan' AND status = 'done'`, e16); n != 1 {
		t.Error("S05E16's scan is not done")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'e18'
		AND status = 'not_applicable'`); n != len(processing.CoveredSteps) {
		t.Errorf("%d steps of S05E18, linked, do not apply", n)
	}
	if len(res.report) != 1 || res.report[0].Kind != model.ScanNoteNotLinked || res.report[0].Path != both ||
		res.report[0].ItemID == nil || *res.report[0].ItemID != e20 || !strings.Contains(res.report[0].Reason, "S05E20 has a file of its own") {
		t.Errorf("the report: %+v, want S05E20 not linked to %s", res.report, both)
	}

	// Again: nothing changes, the report says the same.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	steps := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps`)
	res = f.scan(t)
	if res.itemsInserted != 0 || len(res.report) != 1 {
		t.Errorf("a scan again made %d items, its report %+v", res.itemsInserted, res.report)
	}
	if got := f.episodes(t); got != want {
		t.Errorf("the episodes after a scan again:\n%s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE coveredby IS NOT NULL
		AND modifiedat <> '2001-01-01'`); n != 0 {
		t.Errorf("a scan again marked %d covered episodes changed", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps`); n != steps {
		t.Errorf("a scan again made %d steps", n-steps)
	}

	// S05E15's file is now one of S05E15 alone: S05E16 is covered no more.
	single := f.write(t, "series/Show/Show.S05E15.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET path = $2 WHERE item_id = $1`, e15, single)
	if err := os.Remove(filepath.Join(f.root, "series/Show/Show.S05E15-E16.mkv")); err != nil {
		t.Fatal(err)
	}
	f.scan(t)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND coveredby IS NULL
		AND modifiedat > '2001-01-01'`, e16); n != 1 {
		t.Error("S05E16 is still covered by the file of S05E15, whose name numbers it no more")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2`, e16, processing.UncoveredReason(e15, "covers it no more: its name numbers it no more")); n != len(processing.CoveredSteps) {
		t.Errorf("%d of S05E16's steps say its file covers it no more", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND modifiedat > '2001-01-01'`, e15); n != 1 {
		t.Error("S05E15, which covers S05E16 no more, is not marked changed")
	}
	_ = ctx
}

// A file the scan took in before it read a range of episodes (S01E01E02 was
// no episode's number to it) gets the numbers of its first, and covers the
// next one, made for it.
func TestAFileTakenInWithoutItsNumbersGetsThem(t *testing.T) {
	st := storetest.Open(t)
	f := newLegacy(t, st)
	file := f.write(t, "series/Pilot/Pilot.S01E01E02.mkv")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, createdat, modifiedat)
		VALUES ('pilot', 'series', 'Pilot', 'pilot', now(), now())`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, parent_id, createdat, modifiedat)
		VALUES ('old', 'episode', 'Pilot', 'pilot', now(), now())`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a', 'old', $1, true)`, file)
	f.scan(t)
	if got, want := f.episodes(t), "S01E01 Pilot.S01E01E02.mkv -\nS01E02 - S01E01"; got != want {
		t.Errorf("the episodes:\n%s\nwant:\n%s", got, want)
	}
}

// A disc image is no title's file: the scan passes over it, making no title
// and no asset, and its report says so. A title whose file is one already
// fails its steps that read it, for good, saying why, and the report names it.
func TestTheScannerPassesOverDiscImages(t *testing.T) {
	st := storetest.Open(t)
	f := newLegacy(t, st)
	iso := f.write(t, "Disc (2001)/Disc (2001).iso")
	img := f.write(t, "Image (2002).IMG")
	old := f.write(t, "Old (1999).iso")
	f.write(t, ".hidden.iso")
	storetest.AddItem(t, st, "old", "movie", "Old", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('a', 'old', $1, true)`, old)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status) VALUES
		('s1', 'old', 'chapter', 'pending'), ('s2', 'old', 'tidb', 'done')`)
	res := f.scan(t)
	if res.itemsInserted != 0 || res.filesSeen != 0 {
		t.Errorf("disc images made %d titles, %d files seen", res.itemsInserted, res.filesSeen)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items`); n != 1 {
		t.Errorf("%d titles, want the old one alone", n)
	}
	got := map[string]string{}
	for _, n := range res.report {
		item := ""
		if n.ItemID != nil {
			item = *n.ItemID
		}
		if n.Kind != model.ScanNoteUnsupported || n.Reason != processing.DiscImageReason {
			t.Errorf("a report entry %+v", n)
		}
		got[n.Path] = item
	}
	if len(got) != 3 || got[iso] != "" || got[img] != "" || got[old] != "old" {
		t.Errorf("the report names %v, want the three disc images, the old title's with it", got)
	}
	for _, step := range []string{"chapter", "transcode", "package"} {
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'old'
			AND step = $1 AND status = 'failed' AND error = $2 AND nextretryat IS NULL`, step, processing.DiscImageReason); n != 1 {
			t.Errorf("the old title's %s did not fail for good with the reason", step)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'old'
		AND step = 'tidb' AND status = 'done'`); n != 1 {
		t.Error("a step that is over was failed")
	}
	steps := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'old'`)
	f.scan(t)
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'old'
		AND failures = 1`); n != 3 {
		t.Errorf("a scan again counted another failure: %d of the steps failed once", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = 'old'`); n != steps {
		t.Errorf("a scan again made %d steps", n-steps)
	}
}

// A scan keeps its report with its job, and a catalog without migration 045
// is scanned as before: a file of several episodes is its first one's alone,
// and the job ends done, keeping no report.
func TestAScanJobKeepsItsReport(t *testing.T) {
	ctx := context.Background()
	for _, migrated := range []bool{true, false} {
		st := storetest.Open(t)
		if !migrated {
			storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_items DROP COLUMN coveredby`)
			storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_scanjobs DROP COLUMN report`)
		}
		f := newLegacy(t, st)
		f.write(t, "series/Show/Show.S01E01-E02.mkv")
		disc := f.write(t, "Disc (2001).iso")
		job, err := st.StartScanJob(ctx, "nfs", f.s.runner)
		if err != nil {
			t.Fatal(err)
		}
		f.s.runScan(ctx, job.ID)
		got, err := st.GetScanJob(ctx, job.ID)
		if err != nil || got == nil || got.Status != "done" {
			t.Fatalf("migrated %v: the job %+v, %v", migrated, got, err)
		}
		episodes := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE type = 'episode'`)
		switch {
		case migrated && (len(got.Report) != 1 || got.Report[0].Path != disc || episodes != 2):
			t.Errorf("the report %+v, %d episodes; want the disc image, and both episodes", got.Report, episodes)
		case !migrated && (got.Report != nil || episodes != 1):
			t.Errorf("without 045: the report %+v, %d episodes; want none, and the first episode alone", got.Report, episodes)
		}
		jobs, err := st.ListScanJobs(ctx, 10)
		if err != nil || len(jobs) != 1 || len(jobs[0].Report) != len(got.Report) {
			t.Errorf("migrated %v: the jobs listed %+v, %v", migrated, jobs, err)
		}
	}
}

// With the v2 layout a file of two episodes is taken in for its first one,
// with its source; the second, made for it, has none, and names the first.
func TestAnArrivalOfTwoEpisodesIsItsFirstOnes(t *testing.T) {
	f := newV2(t)
	f.write(t, ".work/incoming/series/Show/Show.S02E05E06.mkv", 3000, 'x')
	if res := f.scan(t); res.itemsInserted != 2 {
		t.Errorf("%d items made, want both episodes", res.itemsInserted)
	}
	if got, want := f.sources(t), "Show present Show.S02E05E06.mkv series/Show/Show.S02E05E06.mkv 3000 true"; got != want {
		t.Errorf("the sources:\n%s\nwant:\n%s", got, want)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items c JOIN com_nalet_katalog_items h ON h.id = c.coveredby
		WHERE c.seasonnumber = 2 AND c.episodenumber = 6 AND h.seasonnumber = 2 AND h.episodenumber = 5
		AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources s WHERE s.item_id = c.id)
		AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a WHERE a.item_id = c.id)`); n != 1 {
		t.Error("S02E06 is not made covered by S02E05's file, with no file or source of its own")
	}
}
