package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// checkOf is the check of a catalog on the library's v2 layout, whose library
// lies in a folder of the test's, with both workers' rules reported.
func checkOf(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	st := storetest.Open(t)
	dir := t.TempDir()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, 'library.layout', 'v2')`)
	s := New(st, config.Config{LibraryRoot: dir})
	reportFor(t, s, Transcoder, transcoderReport)
	reportFor(t, s, Packager, packagerReport)
	return s, st, dir
}

// reportFor reports rules for worker as the worker does.
func reportFor(t *testing.T, s *Service, worker, rules string) Reported {
	t.Helper()
	r, err := s.ReportRules(context.Background(), worker, []byte(`{"version": "1.4.0", "rules": `+rules+`}`))
	if err != nil {
		t.Fatalf("the %s's report: %v", worker, err)
	}
	return r
}

// The titles of the tests, by a name: an id as the library names a folder.
func uuid(n int) string { return fmt.Sprintf("0c0c0c0c-0000-4000-8000-%012d", n) }

// packaged is a packaged title as a test lays it out.
type packaged struct {
	n              int    // its number: its id is uuid(n), its version's and source's are of it too
	kind, title    string // movie or episode
	series         string // an episode's series' id
	width, height  int
	rate           float64 // the package's video, bit/s
	encoder        string
	original, size int64 // the original's and the package's size
	srcCh, pkgCh   int   // the most audio channels of the source and of the package
	srcSubs        []string
	pkgSubs        []string
}

func (p packaged) id() string      { return uuid(p.n) }
func (p packaged) version() string { return uuid(1000 + p.n) }
func (p packaged) source() string  { return uuid(2000 + p.n) }

// add lays the title out: its item, its file, its source and complete
// version in the catalog, and in the library the records of both.
func (p packaged) add(t *testing.T, st *store.Store, dir string) {
	t.Helper()
	parent := ""
	if p.kind == "episode" {
		parent = p.series
	}
	storetest.AddItem(t, st, p.id(), p.kind, p.title, parent)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET durationms = 7200000 WHERE id = $1`, p.id())
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, recordedat)
		VALUES ($1, $2, 'original.mkv', $3, 'present', now())`, p.source(), p.id(), p.original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes, sourceid)
		VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', $3, $4)`, p.id(), dir+"/versions/"+p.id()+"/original.mkv", p.original, p.source())
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, completedat)
		VALUES ($1, $2, ARRAY[$3]::varchar[], 'complete', $4, now())`, p.version(), p.id(), p.source(), uuid(3000+p.n))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes, versionid)
		VALUES (gen_random_uuid()::varchar, $1, 'package.json', false, 'packaged', 'hvc1.2.4.L153.B0', $2, $3)`, p.id(), p.size, p.version())
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'transcode', 'not_applicable', 1),
		       (gen_random_uuid()::varchar, now(), now(), $1, 'package', 'done', 1)`, p.id())

	paths := library.Paths{Root: dir}
	itemDir := paths.ItemDir(library.Place{ID: p.id(), Type: p.kind, SeriesID: p.series})
	var subs []any
	for _, s := range p.pkgSubs {
		form, lang, _ := strings.Cut(s, ":")
		subs = append(subs, map[string]any{"format": form, "language": lang})
	}
	audio := []any{map[string]any{"channels": 2, "bitrateBps": 192000}}
	var surround []any
	if p.pkgCh > 2 {
		surround = append(surround, map[string]any{"channels": p.pkgCh, "bitrateBps": 448000})
	}
	librarytest.WriteVersion(t, library.VersionDir(itemDir, p.version()), librarytest.Version{
		VersionID: p.version(), PackageID: uuid(3000 + p.n), SourceIDs: []string{p.source()}, CreatedAt: "2026-10-01T08:00:00Z",
		Package: map[string]any{"sizeBytes": p.size, "durationMs": 7200000,
			"renditions": map[string]any{"video": []any{map[string]any{"id": "v0", "codec": "hvc1.2.4.L153.B0",
				"width": p.width, "height": p.height, "bitrateBps": p.rate, "encoder": p.encoder}},
				"audio": audio, "audioSurround": surround},
			"subtitles": subs,
			"essence":   map[string]any{"surround": p.pkgCh > 2, "maxAudioChannels": p.pkgCh}},
	})
	streams := []any{map[string]any{"index": 0, "type": "video", "codec": "hevc", "width": p.width, "height": p.height,
		"dispositions": map[string]any{"default": true}},
		map[string]any{"index": 1, "type": "audio", "codec": "eac3", "channels": p.srcCh}}
	for i, s := range p.srcSubs {
		codec, lang, _ := strings.Cut(s, ":")
		streams = append(streams, map[string]any{"index": 2 + i, "type": "subtitle", "codec": codec, "language": lang})
	}
	librarytest.WriteSource(t, library.SourceDir(itemDir, p.source()), map[string]any{"sourceId": p.source(),
		"file": map[string]any{"name": "original.mkv", "sizeBytes": p.original}, "container": map[string]any{"durationMs": 7200000},
		"streams": streams, "essence": map[string]any{"surround": p.srcCh > 2, "maxAudioChannels": p.srcCh}}, nil)
}

// unpackaged adds a title with a file and no package; steps are its
// transcode's status and its package's ("" for none).
func unpackaged(t *testing.T, st *store.Store, n int, title, file, transcode, pkg string) string {
	t.Helper()
	id := uuid(n)
	storetest.AddItem(t, st, id, "movie", title, "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes, resolution)
		VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', 4000000000, '1920x1080')`, id, file)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET durationms = 6000000 WHERE id = $1`, id)
	for step, status := range map[string]string{"transcode": transcode, "package": pkg} {
		if status != "" {
			storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status,
				attempts, error) VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3::varchar, 1,
				CASE WHEN $3::varchar = 'failed' THEN 'ffmpeg exited 1' END)`, id, step, status)
		}
	}
	return id
}

// spaced is s with each no-break space a space, each no-break hyphen a
// hyphen.
func spaced(s string) string { return strings.NewReplacer(nbsp, " ", nbhy, "-").Replace(s) }

// rowsOf writes a report's rows, one a line: "title: RULE, RULE".
func rowsOf(rep graph.ComplianceReport) string {
	var out []string
	for _, r := range rep.Rows {
		var rules []string
		for _, f := range r.Findings {
			rules = append(rules, f.Rule)
		}
		out = append(out, r.Title+": "+strings.Join(rules, ", "))
	}
	return strings.Join(out, "\n")
}

// library is the catalog of the tests: a 4K remux copied 1:1, a film within
// its caps, an episode over its cap in a series, a film never packaged, one
// whose transcode failed, a disc image, and a film whose package lost its
// source's 5.1 and its German image subtitles.
func library4(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	s, st, dir := checkOf(t)
	series := uuid(900)
	storetest.AddItem(t, st, series, "series", "A Coastal Show", "")
	for _, p := range []packaged{
		{n: 1, kind: "movie", title: "A Remux Kept As It Was", width: 3840, height: 2160, rate: 73e6, encoder: "copy",
			original: 63e9, size: 59e9, srcCh: 6, pkgCh: 6},
		{n: 2, kind: "movie", title: "A Film Within Its Caps", width: 1920, height: 1080, rate: 5e6, encoder: "copy",
			original: 5e9, size: 4.6e9, srcCh: 2, pkgCh: 2},
		{n: 3, kind: "episode", title: "An Episode Over Its Cap", series: series, width: 1920, height: 1080, rate: 7e6, encoder: "copy",
			original: 3e9, size: 2.7e9, srcCh: 2, pkgCh: 2},
		{n: 4, kind: "movie", title: "A Film That Lost Its Tracks", width: 1920, height: 1080, rate: 4e6, encoder: "copy",
			original: 4e9, size: 3.9e9, srcCh: 6, pkgCh: 2, srcSubs: []string{"hdmv_pgs_subtitle:de", "hdmv_pgs_subtitle:en"},
			pkgSubs: []string{"pgs:eng"}},
	} {
		p.add(t, st, dir)
	}
	unpackaged(t, st, 5, "A Film Never Packaged", dir+"/arrivals/a-film-never-packaged.mkv", "", "")
	unpackaged(t, st, 6, "A Film Whose Transcode Failed", dir+"/arrivals/a-film-whose-transcode-failed.mkv", "failed", "")
	unpackaged(t, st, 7, "A Disc Image", dir+"/arrivals/a-disc-image.iso", "", "")
	unpackaged(t, st, 8, "A Film Being Encoded", dir+"/arrivals/a-film-being-encoded.mkv", "pending", "")
	return s, st, dir
}

func reportOf(t *testing.T, s *Service, f graph.ComplianceFilter, first int32, after string) graph.ComplianceReport {
	t.Helper()
	rep, err := s.Report(context.Background(), f, graph.ComplianceOrder{Sort: SortPackageSize, Descending: true}, first, after)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// A pass checks every title once, keeps what it found, and the report reads
// it: the titles that break a rule, the largest package first (then the
// largest original), each with its findings, sizes and estimate; a title
// within the rules is no row, and one whose pipeline runs is none either.
func TestAPassChecksTheLibraryAndTheReportReadsIt(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.EvaluatedAt != nil || rep.Total != 0 || rep.Pending != 8 ||
		rep.CheckedTitles != 0 {
		t.Fatalf("before a pass: evaluated %v, total %d, pending %d, checked %d; want never, none, the 8 titles and none",
			rep.EvaluatedAt, rep.Total, rep.Pending, rep.CheckedTitles)
	}
	n, err := s.Pass(ctx)
	if err != nil || n != 8 {
		t.Fatalf("the first pass checked %d titles, %v; want the 8 with a file or a package", n, err)
	}
	rep := reportOf(t, s, graph.ComplianceFilter{}, 100, "")
	want := `A Remux Kept As It Was: OVER_BITRATE_CAP, OVER_SIZE_CAP
A Film That Lost Its Tracks: NO_SURROUND, IMAGE_SUBTITLES_MISSING
An Episode Over Its Cap: OVER_BITRATE_CAP
A Film Never Packaged: NOT_PACKAGED
A Film Whose Transcode Failed: PACKAGING_FAILED
A Disc Image: PACKAGING_FAILED`
	if got := rowsOf(rep); got != want {
		t.Errorf("the report:\n%s\nwant:\n%s", got, want)
	}
	if rep.Total != 6 || rep.Pending != 0 || rep.EvaluatedAt == nil || rep.Evaluating || rep.NextCursor != nil {
		t.Errorf("total %d, pending %d, evaluated %v, evaluating %v, next %v", rep.Total, rep.Pending, rep.EvaluatedAt, rep.Evaluating, rep.NextCursor)
	}
	if rep.CheckedTitles != 8 {
		t.Errorf("%d titles checked, want the 8, those within the rules too", rep.CheckedTitles)
	}
	if len(rep.NotEvaluated) != 0 {
		t.Errorf("not evaluated, with both workers' rules: %q", rep.NotEvaluated)
	}
	top := rep.Rows[0]
	if spaced(top.Findings[0].Text) != "The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie." ||
		*top.PackageBytes != 59e9 || *top.OriginalBytes != 63e9 || top.Kind != "movie" || top.Queue != nil || top.ReworkBlocked != nil {
		t.Errorf("the 4K remux: %+v", top)
	}
	if want := float64(int64((14e6 + 192e3 + 448e3) * 7200 / 8)); top.EstimatedBytes == nil || *top.EstimatedBytes != want {
		t.Errorf("the 4K remux's estimate: %v, want %v", top.EstimatedBytes, want)
	}
	storm := rep.Rows[2]
	if storm.SeriesTitle == nil || *storm.SeriesTitle != "A Coastal Show" || storm.Kind != "episode" {
		t.Errorf("the episode: %+v", storm)
	}
	if disc := rep.Rows[5]; disc.Findings[0].Text != discImageText || disc.ReworkBlocked == nil ||
		*disc.ReworkBlocked != "its file is a disc image, which the transcoder doesn’t encode" || disc.EstimatedBytes != nil {
		t.Errorf("the disc image: %+v", disc)
	}
	if failed := rep.Rows[4]; failed.Findings[0].Text != "The transcoder failed." || failed.Findings[0].Detail == nil ||
		*failed.Findings[0].Detail != "ffmpeg exited 1" {
		t.Errorf("the failed transcode: %+v", failed.Findings)
	}
	// each row's outcome, and the remux's columns
	var outcomes []string
	for _, r := range rep.Rows {
		o := "-"
		if r.Outcome != nil {
			o = *r.Outcome
		}
		outcomes = append(outcomes, o)
	}
	if got := strings.Join(outcomes, " "); got != "REENCODE REPACKAGE REENCODE FIRST_PACKAGE FIRST_PACKAGE BLOCKED" {
		t.Errorf("the outcomes: %s", got)
	}
	if top.Resolution == nil || *top.Resolution != "3840×2160" || top.VideoCodec == nil || *top.VideoCodec != "HEVC" ||
		top.VideoBitrate == nil || *top.VideoBitrate != 73e6 || top.VideoBitrateEstimated || top.BitrateCap == nil ||
		*top.BitrateCap != 14e6 || top.SizeCap == nil || *top.SizeCap != 15*gib || top.DurationSeconds == nil ||
		*top.DurationSeconds != 7200 || top.PackageAudio == nil || *top.PackageAudio != "5.1 + 2.0" {
		t.Errorf("the 4K remux's columns: %+v", top)
	}
	summary := map[string]graph.ComplianceSummary{}
	var order []string
	for _, c := range rep.Summary {
		summary[c.Rule] = c
		order = append(order, c.Rule)
	}
	if strings.Join(order, " ") != strings.Join(Rules, " ") {
		t.Errorf("the summary's rules: %v, want every rule in order", order)
	}
	if c := summary[OverBitrateCap]; c.Titles != 2 || c.PackageBytes != 59e9+2.7e9 || c.EstimatedBytes == nil {
		t.Errorf("the summary of the bitrate cap: %+v", c)
	}
	if c := summary[VideoNotHEVC]; c.Titles != 0 || c.PackageBytes != 0 || c.EstimatedBytes != nil {
		t.Errorf("the summary of a rule no title breaks: %+v", c)
	}

	// a second pass finds nothing to do
	if n, err := s.Pass(ctx); err != nil || n != 0 {
		t.Errorf("a second pass checked %d titles, %v; want none", n, err)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE rules = '{}'`); got != 2 {
		t.Errorf("%d titles found within the rules, want the film within its caps and the one being encoded", got)
	}
}

// The report narrows to a kind, to titles that break any of some rules, and
// to a title or a series whose title holds a search, and pages: each page
// after the one before, none twice; the summary and the titles checked
// count the titles of the kind and the search, whichever rules are asked
// for, the titles checked those within the rules too.
func TestTheReportFiltersAndPages(t *testing.T) {
	s, _, _ := library4(t)
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		f       graph.ComplianceFilter
		want    string
		checked int32
	}{
		{"episodes", graph.ComplianceFilter{Kind: "episode"}, "An Episode Over Its Cap: OVER_BITRATE_CAP", 1},
		{"not packaged or failed", graph.ComplianceFilter{Rules: []string{NotPackaged, PackagingFailed}},
			"A Film Never Packaged: NOT_PACKAGED\nA Film Whose Transcode Failed: PACKAGING_FAILED\nA Disc Image: PACKAGING_FAILED", 8},
		{"by the series’ title", graph.ComplianceFilter{Search: "COASTAL"}, "An Episode Over Its Cap: OVER_BITRATE_CAP", 1},
		{"by the title", graph.ComplianceFilter{Search: "remux"}, "A Remux Kept As It Was: OVER_BITRATE_CAP, OVER_SIZE_CAP", 1},
		{"movies over a cap", graph.ComplianceFilter{Kind: "movie", Rules: []string{OverBitrateCap}},
			"A Remux Kept As It Was: OVER_BITRATE_CAP, OVER_SIZE_CAP", 7},
		{"nothing", graph.ComplianceFilter{Search: "no such title"}, "", 0},
	} {
		rep := reportOf(t, s, tc.f, 100, "")
		if got := rowsOf(rep); got != tc.want || int(rep.Total) != len(rep.Rows) || rep.CheckedTitles != tc.checked {
			t.Errorf("%s: total %d, %d checked (want %d)\n%s\nwant:\n%s", tc.name, rep.Total, rep.CheckedTitles, tc.checked,
				got, tc.want)
		}
	}
	rep := reportOf(t, s, graph.ComplianceFilter{Kind: "movie", Rules: []string{OverBitrateCap}}, 100, "")
	// the totals are the whole filter's, the rules too
	if m := rep.Matching; m.Titles != 1 || m.OriginalBytes != 63e9 || m.PackageBytes != 59e9 || m.EstimatedBytes == nil ||
		*m.EstimatedBytes != float64(int64((14e6+192e3+448e3)*7200/8)) {
		t.Errorf("the totals of movies over a cap: %+v", m)
	}
	all := reportOf(t, s, graph.ComplianceFilter{}, 2, "")
	if m := all.Matching; m.Titles != 6 || m.Titles != all.Total || m.PackageBytes != 59e9+3.9e9+2.7e9 ||
		m.OriginalBytes != 63e9+4e9+3e9+3*4e9 || m.EstimatedBytes == nil {
		t.Errorf("the totals of every title, a page of 2: %+v", m)
	}
	for _, c := range rep.Summary {
		if c.Rule == PackagingFailed && c.Titles != 2 {
			t.Errorf("the summary of movies, asked for the bitrate cap: %d failed, want the 2 movies' whatever the rules", c.Titles)
		}
	}

	var pages []string
	after := ""
	for i := 0; i < 10; i++ {
		rep := reportOf(t, s, graph.ComplianceFilter{}, 2, after)
		if rep.Total != 6 {
			t.Errorf("page %d: total %d, want 6", i, rep.Total)
		}
		pages = append(pages, rowsOf(rep))
		if rep.NextCursor == nil {
			break
		}
		after = *rep.NextCursor
	}
	if got := strings.Join(pages, "\n"); got != rowsOf(reportOf(t, s, graph.ComplianceFilter{}, 100, "")) || len(pages) != 3 {
		t.Errorf("3 pages of 2:\n%s", got)
	}
	if _, err := s.Report(context.Background(), graph.ComplianceFilter{}, graph.ComplianceOrder{}, 2, "not-a-cursor"); err == nil {
		t.Error("a cursor of no page was taken")
	}
	if _, err := s.Report(context.Background(), graph.ComplianceFilter{Rules: []string{"TOO_LOUD"}}, graph.ComplianceOrder{}, 2, ""); err == nil {
		t.Error("an unknown rule was taken")
	}
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 0, ""); len(rep.Rows) != 0 || rep.Total != 6 {
		t.Errorf("a page of none: %d rows, total %d", len(rep.Rows), rep.Total)
	}
}

// The report sorts its rows in every order, either way, a null key last
// either way and a tie by the title's id; its pages, each after the one
// before by a cursor of that order, are the order whole, and a cursor of
// another order is refused.
func TestTheReportSortsInEveryOrder(t *testing.T) {
	s, _, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	// key is a row's value of a sort, nil for none (a null)
	key := func(sort string, r graph.ComplianceRow) []any {
		f := func(p *float64) any {
			if p == nil {
				return nil
			}
			return *p
		}
		switch sort {
		case SortPackageSize:
			return []any{f(r.PackageBytes), f(r.OriginalBytes)}
		case SortOriginalSize:
			return []any{f(r.OriginalBytes), f(r.PackageBytes)}
		case SortSaving:
			if r.PackageBytes == nil || r.EstimatedBytes == nil {
				return []any{nil, f(r.PackageBytes)}
			}
			return []any{*r.PackageBytes - *r.EstimatedBytes, f(r.PackageBytes)}
		case SortVideoBitrate:
			return []any{f(r.VideoBitrate), f(r.PackageBytes)}
		}
		series := r.Title
		if r.SeriesTitle != nil {
			series = *r.SeriesTitle
		}
		i := func(p *int32) any {
			if p == nil {
				return nil
			}
			return float64(*p)
		}
		return []any{strings.ToLower(series), i(r.Season), i(r.Episode), strings.ToLower(r.Title)}
	}
	// before says a sorts before b: each key in the direction, a null last
	before := func(a, b []any, desc bool) int {
		for i := range a {
			switch x, y := a[i], b[i]; {
			case x == nil && y == nil:
				continue
			case x == nil:
				return 1
			case y == nil:
				return -1
			default:
				c := 0
				if xs, ok := x.(string); ok {
					c = strings.Compare(xs, y.(string))
				} else if xf, yf := x.(float64), y.(float64); xf < yf {
					c = -1
				} else if xf > yf {
					c = 1
				}
				if desc {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
		}
		return 0
	}
	ids := func(rows []graph.ComplianceRow) string {
		var out []string
		for _, r := range rows {
			out = append(out, r.ItemID[len(r.ItemID)-2:])
		}
		return strings.Join(out, " ")
	}
	for _, sort := range Sorts {
		for _, desc := range []bool{true, false} {
			o := graph.ComplianceOrder{Sort: sort, Descending: desc}
			whole, err := s.Report(ctx, graph.ComplianceFilter{}, o, 100, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(whole.Rows) != 6 {
				t.Fatalf("%s %v: %d rows", sort, desc, len(whole.Rows))
			}
			for i := 1; i < len(whole.Rows); i++ {
				a, b := whole.Rows[i-1], whole.Rows[i]
				if c := before(key(sort, a), key(sort, b), desc); c > 0 || c == 0 && a.ItemID > b.ItemID {
					t.Errorf("%s %v: %s before %s (%v, %v)", sort, desc, a.Title, b.Title, key(sort, a), key(sort, b))
				}
			}
			var paged []graph.ComplianceRow
			after := ""
			for page := 0; page < 10; page++ {
				rep, err := s.Report(ctx, graph.ComplianceFilter{}, o, 2, after)
				if err != nil {
					t.Fatalf("%s %v, page %d: %v", sort, desc, page, err)
				}
				paged = append(paged, rep.Rows...)
				if rep.NextCursor == nil {
					break
				}
				after = *rep.NextCursor
			}
			if ids(paged) != ids(whole.Rows) {
				t.Errorf("%s %v: the pages %s, the order whole %s", sort, desc, ids(paged), ids(whole.Rows))
			}
		}
	}
	// by title, from A: an episode by its series
	byTitle, err := s.Report(ctx, graph.ComplianceFilter{}, graph.ComplianceOrder{Sort: SortTitle}, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(byTitle); got != `An Episode Over Its Cap: OVER_BITRATE_CAP
A Disc Image: PACKAGING_FAILED
A Film Never Packaged: NOT_PACKAGED
A Film That Lost Its Tracks: NO_SURROUND, IMAGE_SUBTITLES_MISSING
A Film Whose Transcode Failed: PACKAGING_FAILED
A Remux Kept As It Was: OVER_BITRATE_CAP, OVER_SIZE_CAP` {
		t.Errorf("by title:\n%s", got)
	}
	// a cursor of one order is none of another's
	first, err := s.Report(ctx, graph.ComplianceFilter{}, graph.ComplianceOrder{Sort: SortSaving, Descending: true}, 2, "")
	if err != nil || first.NextCursor == nil {
		t.Fatalf("a first page: %v, %v", first.NextCursor, err)
	}
	if _, err := s.Report(ctx, graph.ComplianceFilter{}, graph.ComplianceOrder{Sort: SortTitle}, 2, *first.NextCursor); err == nil ||
		!strings.Contains(err.Error(), "another order") {
		t.Errorf("a cursor of another order: %v", err)
	}
	if _, err := s.Report(ctx, graph.ComplianceFilter{}, graph.ComplianceOrder{Sort: "LOUDNESS"}, 2, ""); err == nil {
		t.Error("an unknown sort: no error")
	}
}

// A row says where its title stands in the re-encode queue, the latest
// place it had there, and why it waits or failed.
func TestARowSaysItsPlaceInTheQueue(t *testing.T) {
	s, st, _ := library4(t)
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state, selection, reason)
		VALUES ('q1', $1, 'failed', 'items', 'transcode failed: out of memory'), ('q2', $1, 'queued', 'items', 'transcode is running')`, uuid(1))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_reencodequeue (id, item_id, state, selection, sentat)
		VALUES ('q3', $1, 'sent', 'items', now())`, uuid(3))
	rep := reportOf(t, s, graph.ComplianceFilter{}, 100, "")
	for _, r := range rep.Rows {
		want := map[string]string{uuid(1): "queued: The transcoder is working on it.", uuid(3): "sent: "}[r.ItemID]
		got := ""
		if r.Queue != nil {
			got = *r.Queue + ": "
			if r.QueueReason != nil {
				got += *r.QueueReason
			}
		}
		if got != want {
			t.Errorf("%s: queue %q, want %q", r.Title, got, want)
		}
	}
	if q := rep.Queue; q != (graph.ComplianceQueueCounts{Queued: 1, Processing: 1, Processed: 0, Failed: 1}) {
		t.Errorf("the queue's counts: %+v", q)
	}
}

// bus takes every event the queue sends.
type bus struct{ sent int }

func (b *bus) Enabled() bool { return true }
func (b *bus) Publish(_ context.Context, msgs []events.Message) []error {
	b.sent += len(msgs)
	return make([]error, len(msgs))
}

// The reasons the re-encode queue keeps of a title, as the queue writes them,
// are said in the reader's words: one waiting while its transcode runs, one
// failed in its transcode.
func TestTheQueuesReasonsInWords(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	q := retry.New(st, processing.DefaultPolicy(), &bus{}, time.Minute)
	if _, err := q.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{uuid(1)}}); err != nil {
		t.Fatal(err)
	}
	reason := func() string {
		t.Helper()
		for _, r := range reportOf(t, s, graph.ComplianceFilter{Search: "remux"}, 100, "").Rows {
			if r.Queue != nil && r.QueueReason != nil {
				if r.QueueDetail != nil {
					return *r.Queue + ": " + *r.QueueReason + " {" + *r.QueueDetail + "}"
				}
				return *r.Queue + ": " + *r.QueueReason
			}
			if r.Queue != nil {
				return *r.Queue + ":"
			}
		}
		return "none"
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'in_progress', modifiedat = localtimestamp
		WHERE item_id = $1 AND step = 'transcode'`, uuid(1))
	if _, err := q.DrainQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reason(); !strings.HasPrefix(got, "queued: The transcoder is working on it. {its worker last reported at ") ||
		!strings.HasSuffix(got, "; timeout 6h}") {
		t.Errorf("a title waiting while its transcode runs: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done' WHERE item_id = $1 AND step = 'transcode'`, uuid(1))
	if _, err := q.DrainQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reason(); got != "sent:" {
		t.Errorf("a title sent: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', error = 'ffmpeg exited 1',
		nextretryat = NULL WHERE item_id = $1 AND step = 'transcode'`, uuid(1))
	if _, err := q.DrainQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reason(); got != "failed: The transcoder failed. {ffmpeg exited 1}" {
		t.Errorf("a title whose transcode failed: %q", got)
	}
}

// What a request to process titles again did is said in a sentence each:
// those queued, those queued already, and those skipped, by why; and every
// title it skipped is named, as a list names it, with its reason, in the
// reader's words: as the queue tells, not in the queue's.
func TestWhatARequestDidInWords(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	q := retry.New(st, processing.DefaultPolicy(), &bus{}, time.Minute)
	say := func(items ...string) (string, string) {
		t.Helper()
		res, err := q.EnqueueReencode(ctx, graph.ReencodeRequest{Items: items})
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Queued(ctx, res, false)
		if err != nil {
			t.Fatal(err)
		}
		if m.Message != requestMessage(m.QueuedText, m.AlreadyText, m.NotQueuedText) {
			t.Errorf("the message %q says other than its three texts", m.Message)
		}
		if int(m.Skipped) != len(m.SkippedTitles) {
			t.Errorf("%d skipped, and %d of them named", m.Skipped, len(m.SkippedTitles))
		}
		var skips []string
		for _, sk := range m.SkippedTitles {
			skips = append(skips, sk.Title+": "+sk.Reason)
			if sk.ItemID == "" {
				t.Errorf("a skip without its id: %+v", sk)
			}
		}
		return m.Message, strings.Join(skips, "\n")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET year = 2004 WHERE id = $1`, uuid(7))
	// the film whose transcode failed, the disc image and a title there is not
	msg, skips := say(uuid(6), uuid(7), uuid(99))
	if msg != "1 title queued to be processed again. 2 titles weren’t queued: 1 is a disc image and 1 is not in the catalog." ||
		skips != uuid(99)+": it is not in the catalog\n"+
			"A Disc Image (2004): its file is a disc image, which the transcoder doesn’t encode" {
		t.Errorf("one queued, two skipped: %q\n%s", msg, skips)
	}
	if msg, skips := say(uuid(6)); msg != "1 title already in the queue." || skips != "" {
		t.Errorf("one queued already: %q\n%s", msg, skips)
	}

	// no file; the original deleted after packaging; the original being
	// deleted; an episode without its file; a series without episodes; a
	// season
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true`, uuid(5))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, deletedat)
		VALUES ('gone', $1, 'original.mkv', 1, 'deleted', now())`, uuid(2))
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true`, uuid(2))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring' WHERE item_id = $1`, uuid(4))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2 WHERE id = $1`, uuid(3))
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true`, uuid(3))
	storetest.AddItem(t, st, uuid(901), "series", "An Empty Show", "")
	storetest.AddItem(t, st, uuid(902), "season", "Season 1", uuid(900))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1 WHERE id = $1`, uuid(902))
	msg, skips = say(uuid(5), uuid(2), uuid(4), uuid(3), uuid(901), uuid(902), uuid(1))
	if want := "1 title queued to be processed again. 6 titles weren’t queued: the original of 1 was deleted " +
		"after packaging, the original of 1 is being deleted after packaging, 2 have no file, 1 is a series without " +
		"episodes and 1 isn’t a movie, an episode or a series."; msg != want {
		t.Errorf("one queued, six skipped:\n%q\nwant\n%q", msg, want)
	}
	if want := `An Empty Show: the series has no episodes
A Coastal Show · S01: it isn’t a movie, an episode or a series
A Film Never Packaged: it has no file
A Film Within Its Caps: its original was deleted after packaging
A Film That Lost Its Tracks: its original is being deleted after packaging
A Coastal Show · S01E02 · An Episode Over Its Cap: it has no file`; skips != want {
		t.Errorf("the titles skipped:\n%s\nwant:\n%s", skips, want)
	}
	if m, err := s.Queued(ctx, graph.ReencodeEnqueued{}, true); err != nil || m.QueuedText != nil || m.AlreadyText != nil ||
		m.NotQueuedText == nil || *m.NotQueuedText != "Nothing was queued: no title fits the filter anymore." ||
		m.Message != *m.NotQueuedText || m.SkippedTitles == nil || len(m.SkippedTitles) != 0 {
		t.Errorf("nothing matched: %+v, %v", m, err)
	}
	if m, err := s.Queued(ctx, graph.ReencodeEnqueued{}, false); err != nil || m.NotQueuedText == nil ||
		*m.NotQueuedText != "Nothing was queued: no title was selected." {
		t.Errorf("nothing named: %+v, %v", m, err)
	}
}

// The report's queueable are what a request by its filter queues, title for
// title and byte for byte: the titles that fit it but those queued already,
// waiting or sent, and those that processing again can't help (a disc image,
// an original deleted after packaging or being deleted); a title done or
// failed in the queue before is queued again. Once they are queued, none is
// left, and the request says why.
func TestTheQueueableAreWhatAFilterQueues(t *testing.T) {
	s, st, dir := library4(t)
	ctx := context.Background()
	for _, p := range []packaged{
		{n: 10, kind: "movie", title: "A Film Whose Original Was Deleted", width: 1920, height: 1080, rate: 9e6, encoder: "copy",
			original: 6e9, size: 5.5e9, srcCh: 2, pkgCh: 2},
		{n: 11, kind: "movie", title: "A Film Whose Original Is Being Deleted", width: 1920, height: 1080, rate: 9e6, encoder: "copy",
			original: 6e9, size: 5.4e9, srcCh: 2, pkgCh: 2},
	} {
		p.add(t, st, dir)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true`, uuid(10))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now() WHERE item_id = $1`, uuid(10))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring' WHERE item_id = $1`, uuid(11))
	// a film the transcoder refused for its Dolby Vision profile 7: it would
	// refuse it again
	unpackaged(t, st, 12, "A Film In Dolby Vision 7", dir+"/arrivals/a-film-in-dolby-vision-7.mkv", "", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status,
		attempts, error) VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'transcode', 'failed', 1,
		'Dolby Vision profile 7 needs a tone-mapping encode; kept the original')`, uuid(12))
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	q := retry.New(st, processing.DefaultPolicy(), &bus{}, time.Minute)
	if _, err := q.EnqueueReencode(ctx, graph.ReencodeRequest{Items: []string{uuid(3), uuid(4), uuid(5), uuid(6)}}); err != nil {
		t.Fatal(err)
	}
	// the episode processed again before, the film that lost its tracks
	// failed in the queue: both are queued again; the film never packaged
	// waits, the one whose transcode failed is being processed
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'done', sentat = now(), doneat = now() WHERE item_id = $1`, uuid(3))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'failed', sentat = now(), doneat = now(),
		reason = 'transcode failed: ffmpeg exited 1' WHERE item_id = $1`, uuid(4))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'sent', sentat = now() WHERE item_id = $1`, uuid(6))

	all := graph.ComplianceFilter{}
	rep := reportOf(t, s, all, 100, "")
	if got := rowsOf(rep); got != `A Remux Kept As It Was: OVER_BITRATE_CAP, OVER_SIZE_CAP
A Film Whose Original Was Deleted: OVER_BITRATE_CAP
A Film Whose Original Is Being Deleted: OVER_BITRATE_CAP
A Film That Lost Its Tracks: NO_SURROUND, IMAGE_SUBTITLES_MISSING
An Episode Over Its Cap: OVER_BITRATE_CAP
A Film Never Packaged: NOT_PACKAGED
A Film Whose Transcode Failed: PACKAGING_FAILED
A Disc Image: PACKAGING_FAILED
A Film In Dolby Vision 7: PACKAGING_FAILED` {
		t.Fatalf("the report:\n%s", got)
	}
	if dv := rep.Rows[len(rep.Rows)-1]; dv.ReworkBlocked == nil || *dv.ReworkBlocked != "Dolby Vision profile 7 needs a tone-mapping encode" ||
		dv.EstimatedBytes != nil {
		t.Errorf("the film the transcoder would refuse again: %+v", dv)
	}
	want := map[string]bool{uuid(1): true, uuid(4): true, uuid(3): true}
	var sum sized
	var orig, pkg, est int64
	for _, r := range rep.Rows {
		if !want[r.ItemID] {
			continue
		}
		if r.Queue != nil && (*r.Queue == "queued" || *r.Queue == "sent") || r.ReworkBlocked != nil {
			t.Errorf("%s: queue %v, blocked %v, and queueable", r.Title, r.Queue, r.ReworkBlocked)
		}
		orig, pkg, est = orig+int64(*r.OriginalBytes), pkg+int64(*r.PackageBytes), est+int64(*r.EstimatedBytes)
	}
	sum.orig, sum.pkg, sum.est = &orig, &pkg, &est
	wantTotals := totalsOf([]sized{sum})
	wantTotals.Titles = 3
	if got := rep.Queueable; got.Titles != wantTotals.Titles || got.OriginalBytes != wantTotals.OriginalBytes ||
		got.PackageBytes != wantTotals.PackageBytes || got.EstimatedBytes == nil || *got.EstimatedBytes != *wantTotals.EstimatedBytes {
		t.Errorf("queueable: %+v, want %+v (est %v)", got, wantTotals, *wantTotals.EstimatedBytes)
	}
	if m := rep.Matching; m.Titles != 9 || rep.Total != 9 {
		t.Errorf("matching: %+v, total %d; want the 9 titles", m, rep.Total)
	}
	got, err := s.Queueable(ctx, all)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != uuid(1)+" "+uuid(4)+" "+uuid(3) {
		t.Errorf("the queueable: %v; want the remux, the film that lost its tracks and the episode", got)
	}
	// a filter: the movies over a cap, of which only the remux can be
	// processed again
	movies, err := s.Queueable(ctx, graph.ComplianceFilter{Kind: "movie", Rules: []string{OverBitrateCap}})
	if err != nil || strings.Join(movies, " ") != uuid(1) {
		t.Errorf("the movies over a cap: %+v, %v", movies, err)
	}
	if r := reportOf(t, s, graph.ComplianceFilter{Kind: "movie", Rules: []string{OverBitrateCap}}, 1, ""); r.Queueable.Titles != 1 ||
		r.Queueable.PackageBytes != 59e9 || r.Queueable.OriginalBytes != 63e9 || r.Matching.Titles != 3 {
		t.Errorf("the movies over a cap, a page of 1: queueable %+v, matching %+v", r.Queueable, r.Matching)
	}

	// what the request queues is what was offered
	res, err := q.EnqueueReencode(ctx, graph.ReencodeRequest{Items: got})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Queued(ctx, res, true)
	if err != nil || m.Queued != rep.Queueable.Titles || m.AlreadyQueued != 0 || m.Skipped != 0 || m.QueuedText == nil ||
		*m.QueuedText != "3 titles queued to be processed again" || m.AlreadyText != nil || m.NotQueuedText != nil ||
		m.Message != "3 titles queued to be processed again." {
		t.Errorf("the request: %+v, %v", m, err)
	}
	var qn int32
	var qo, qp, qe float64
	if err := st.Pool().QueryRow(ctx, `SELECT count(*), COALESCE(sum(c.originalbytes), 0)::float8,
			COALESCE(sum(c.packagebytes), 0)::float8, COALESCE(sum(c.estimatedbytes), 0)::float8
		FROM com_nalet_katalog_reencodequeue q JOIN com_nalet_katalog_compliance c ON c.item_id = q.item_id
		WHERE q.state = 'queued' AND q.item_id <> $1`, uuid(5)).Scan(&qn, &qo, &qp, &qe); err != nil {
		t.Fatal(err)
	}
	if qn != rep.Queueable.Titles || qo != rep.Queueable.OriginalBytes || qp != rep.Queueable.PackageBytes ||
		qe != *rep.Queueable.EstimatedBytes {
		t.Errorf("queued %d titles of %v, %v and %v bytes; offered %+v", qn, qo, qp, qe, rep.Queueable)
	}

	// none is left to queue, and a request says why
	if r := reportOf(t, s, all, 100, ""); r.Queueable.Titles != 0 || r.Queueable.OriginalBytes != 0 || r.Queueable.EstimatedBytes != nil {
		t.Errorf("queueable once queued: %+v", r.Queueable)
	}
	left, err := s.Queueable(ctx, all)
	if err != nil || len(left) != 0 {
		t.Fatalf("the queueable once queued: %v, %v", left, err)
	}
	m, err = s.Queued(ctx, graph.ReencodeEnqueued{}, true)
	if err != nil || m.NotQueuedText == nil || *m.NotQueuedText != "Nothing was queued: no title fits the filter anymore." {
		t.Errorf("a request with none left: %+v, %v", m, err)
	}
}

// The rows are checked against the rules they were found by: a report of the
// same rules by another version changes nothing, and rules that changed have
// every title checked again by the next pass, against them.
func TestTheRulesHashMakesRowsStale(t *testing.T) {
	s, _, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := s.ReportRules(ctx, Transcoder, []byte(`{"version": "1.5.0", "rules": `+
		strings.Replace(transcoderReport, `"preset": "p5"`, `"preset": "p6"`, 1)+`}`))
	if err != nil || again.Changed {
		t.Fatalf("the same rules by another version, another preset: %+v, %v; want them unchanged", again, err)
	}
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.Pending != 0 {
		t.Errorf("after the same rules: %d titles pending, want none", rep.Pending)
	}
	if r := reportFor(t, s, Transcoder, strings.Replace(transcoderReport, `"movie2160Mbps": 14`, `"movie2160Mbps": 80`, 1)); !r.Changed {
		t.Fatal("a cap that changed was not taken for a change")
	}
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.Pending != 8 {
		t.Errorf("after a cap changed: %d titles pending, want every one", rep.Pending)
	}
	if n, err := s.Pass(ctx); err != nil || n != 8 {
		t.Fatalf("the pass after the change checked %d, %v", n, err)
	}
	rep := reportOf(t, s, graph.ComplianceFilter{Search: "remux"}, 100, "")
	if got := rowsOf(rep); got != "A Remux Kept As It Was: OVER_SIZE_CAP" {
		t.Errorf("the 4K remux against a cap of 80 Mbit/s: %q", got)
	}
	var keys []string
	for _, r := range rep.Rules.Transcoder.Rules {
		keys = append(keys, r.Key+"="+r.Value)
	}
	if got := spaced(strings.Join(keys, " ")); !strings.Contains(got, "caps.movie2160Mbps=at most 80 Mbit/s") ||
		!strings.Contains(got, "encoder.preset=p5") {
		t.Errorf("the transcoder's rules in the report: %s", got)
	}
}

// Without a worker's report its rules are not held against a title, and the
// report says what is not checked: only what the catalog's tables tell (not
// packaged, failed) is found.
func TestUnreportedRulesAreNotEvaluated(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	s := New(st, config.Config{LibraryRoot: dir})
	packaged{n: 1, kind: "movie", title: "A Remux Kept As It Was", width: 3840, height: 2160, rate: 73e6, encoder: "copy",
		original: 63e9, size: 59e9, srcCh: 6, pkgCh: 2}.add(t, st, dir)
	unpackaged(t, st, 5, "A Film Never Packaged", dir+"/arrivals/a-film-never-packaged.mkv", "", "")
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep := reportOf(t, s, graph.ComplianceFilter{}, 100, "")
	if got := rowsOf(rep); got != "A Film Never Packaged: NOT_PACKAGED" {
		t.Errorf("no rules reported:\n%s", got)
	}
	want := []string{"The transcoder hasn’t reported its rules, so bit rates, file sizes and the video format can’t be checked.",
		"The packager hasn’t reported what it keeps, so surround and image subtitles can’t be checked."}
	if strings.Join(rep.NotEvaluated, "\n") != strings.Join(want, "\n") || rep.Rules.Transcoder != nil || rep.Rules.Packager != nil {
		t.Errorf("not evaluated:\n%s\nrules %+v", strings.Join(rep.NotEvaluated, "\n"), rep.Rules)
	}
	if rep.Rows[0].EstimatedBytes != nil {
		t.Errorf("an estimate without the workers' rules: %v", *rep.Rows[0].EstimatedBytes)
	}

	// the packager reports: its rules are checked from the next pass on, the
	// transcoder's still not
	reportFor(t, s, Packager, strings.Replace(packagerReport, `"eac3"`, `"off"`, 1))
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rep = reportOf(t, s, graph.ComplianceFilter{}, 100, "")
	if got := rowsOf(rep); got != "A Film Never Packaged: NOT_PACKAGED" {
		t.Errorf("a packager that makes no 5.1:\n%s", got)
	}
	if got := strings.Join(rep.NotEvaluated, "\n"); got != want[0]+"\nThe packager makes no surround track, so a package without one breaks no rule." {
		t.Errorf("not evaluated:\n%s", got)
	}
	reportFor(t, s, Packager, packagerReport)
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rowsOf(reportOf(t, s, graph.ComplianceFilter{}, 100, "")); got != "A Remux Kept As It Was: NO_SURROUND\nA Film Never Packaged: NOT_PACKAGED" {
		t.Errorf("the packager's 5.1 reported:\n%s", got)
	}
}

// A title whose package completed is checked again by the next pass (Touch),
// one the check held no row of too; refreshCompliance has every title of a
// kind checked again.
func TestATitleTouchedIsCheckedAgain(t *testing.T) {
	s, st, dir := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	// the film within its caps is packaged again, its 5.1 lost
	p := packaged{n: 2, kind: "movie", title: "A Film Within Its Caps", width: 1920, height: 1080, rate: 5e6, encoder: "copy",
		original: 5e9, size: 4.6e9, srcCh: 6, pkgCh: 2}
	itemDir := library.Paths{Root: dir}.MovieDir(p.id())
	librarytest.WriteSource(t, library.SourceDir(itemDir, p.source()), map[string]any{"sourceId": p.source(),
		"streams": []any{}, "essence": map[string]any{"surround": true, "maxAudioChannels": 6}}, nil)
	if n, err := s.Pass(ctx); err != nil || n != 0 {
		t.Fatalf("a pass before the title is touched checked %d, %v", n, err)
	}
	s.Touch(ctx, p.id())
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.Pending != 1 {
		t.Errorf("a title touched: %d pending, want it", rep.Pending)
	}
	if n, err := s.Pass(ctx); err != nil || n != 1 {
		t.Fatalf("the pass after a touch checked %d, %v", n, err)
	}
	if got := rowsOf(reportOf(t, s, graph.ComplianceFilter{Search: "within its"}, 100, "")); got != "A Film Within Its Caps: NO_SURROUND" {
		t.Errorf("the title packaged again: %q", got)
	}

	// a title the catalog gained, touched before any pass: it waits, and is
	// no row of the report until then
	id := unpackaged(t, st, 9, "A Film Just Taken In", dir+"/arrivals/a-film-just-taken-in.mkv", "", "")
	s.Touch(ctx, id)
	if rep := reportOf(t, s, graph.ComplianceFilter{Search: "just taken"}, 100, ""); rep.Total != 0 || rep.Pending != 1 {
		t.Errorf("a new title touched: total %d, pending %d", rep.Total, rep.Pending)
	}
	if n, err := s.Pass(ctx); err != nil || n != 1 {
		t.Fatalf("the pass after the new title checked %d, %v", n, err)
	}

	ok, err := s.Refresh(ctx, graph.ComplianceEpisode)
	if err != nil || !ok {
		t.Fatalf("refresh: %v, %v", ok, err)
	}
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.Pending != 1 {
		t.Errorf("after a refresh of the episodes: %d pending, want the one episode", rep.Pending)
	}
	if n, err := s.Pass(ctx); err != nil || n != 1 {
		t.Errorf("the pass after a refresh of the episodes checked %d, %v", n, err)
	}
}

// A pass runs in one instance at a time: while another holds the lock, a
// pass checks nothing and the report says one runs. A title deleted takes
// its row with it, and one that is no title to check any more is pruned.
func TestOnePassAtATimeAndPruning(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+passLock+`)`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Pass(ctx); err != nil || n != 0 {
		t.Errorf("a pass while another runs checked %d, %v", n, err)
	}
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); !rep.Evaluating {
		t.Error("the report does not say a pass runs")
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+passLock+`)`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if rep := reportOf(t, s, graph.ComplianceFilter{}, 100, ""); rep.Evaluating {
		t.Error("the report says a pass runs, and none does")
	}
	if n, err := s.Pass(ctx); err != nil || n != 8 {
		t.Fatalf("the pass checked %d, %v", n, err)
	}

	if _, err := st.DeleteItems(ctx, []string{uuid(5)}, store.Deletion{By: "admin-1"}); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE item_id = $1`, uuid(5)); n != 0 {
		t.Errorf("a deleted title's row stayed")
	}
	// a film that has neither file nor package any more, and an episode that
	// another's file covers now
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, uuid(6))
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET coveredby = $2 WHERE id = $1`, uuid(3), uuid(1))
	s.Touch(ctx, uuid(2))
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{uuid(6), uuid(3)} {
		if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_compliance WHERE item_id = $1`, id); n != 0 {
			t.Errorf("%s: its row stayed", id)
		}
	}
}

// A record that cannot be read leaves the rules that read it unchecked of its
// title, and the report says so.
func TestARecordThatCannotBeRead(t *testing.T) {
	s, _, _ := checkOf(t)
	p := packaged{n: 1, kind: "movie", title: "A Remux Kept As It Was"}
	storetest.AddItem(t, s.st, p.id(), "movie", p.title, "")
	storetest.Exec(t, s.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, ARRAY[$3]::varchar[], 'complete')`,
		p.version(), p.id(), p.source())
	storetest.Exec(t, s.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes)
		VALUES ('pk', $1, 'package.json', false, 'packaged', 'avc1.640033', 59000000000)`, p.id())
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	if _, err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	// the report says how many; the log says which, and why
	if got := logged.String(); !strings.Contains(got, "compliance: 1 title whose records can't be read") ||
		!strings.Contains(got, "the first, "+p.id()+": versions/"+p.version()+"/package.json is not there") {
		t.Errorf("the log of a pass:\n%s", got)
	}
	rep := reportOf(t, s, graph.ComplianceFilter{}, 100, "")
	if got := rowsOf(rep); got != "A Remux Kept As It Was: VIDEO_NOT_HEVC" {
		t.Errorf("a title without its records:\n%s", got)
	}
	want := "The records of 1 title can’t be read, so its bit rate, surround and image subtitles can’t be checked."
	if len(rep.NotEvaluated) != 1 || rep.NotEvaluated[0] != want {
		t.Errorf("not evaluated: %q\nwant %q", rep.NotEvaluated, want)
	}
}

// The rules a worker reports are kept as it reported them, written out for
// the console; a report that is no report of the worker's rules is refused,
// and keeps nothing.
func TestTheRulesAWorkerReports(t *testing.T) {
	s, st, _ := checkOf(t)
	ctx := context.Background()
	rules, err := s.PipelineRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	row := func(r graph.RuleSetting) string {
		s := r.Key + " | " + r.Label + " | " + spaced(r.Value)
		if r.Detail != nil {
			s += " {" + *r.Detail + "}"
		}
		return s
	}
	for _, r := range rules.Transcoder.Rules {
		got = append(got, row(r))
	}
	want := `caps.movie1080Mbps | Video bit rate, 1080p movies | at most 8 Mbit/s
caps.movie2160Mbps | Video bit rate, 2160p movies | at most 14 Mbit/s
caps.movieMaxGiB | File size, movies | at most 16.1 GB (15 GiB)
caps.episode1080Mbps | Video bit rate, 1080p episodes | at most 6 Mbit/s
caps.episode2160Mbps | Video bit rate, 2160p episodes | at most 8 Mbit/s
bucket2160.minHeight | 2160p from a height of | 2,000 pixels
bucket2160.minWidth | 2160p from a width of | 3,200 pixels
encoder.codec | Encoder | hevc_nvenc
encoder.preset | Encoder preset | p5
encoder.cq | Constant quality | 23
encoder.maxrate1080Mbps | Encode bit rate, 1080p | up to the caps above
encoder.maxrate2160Mbps | Encode bit rate, 2160p | up to the caps above
ladder | Renditions | one, at the original’s size, in HEVC {source:hevc}
extraLadder | Renditions of extras | one, at the original’s size, in HEVC {source:hevc}
dolbyVision.profile5 | Dolby Vision profile 5 | reshaped to HDR10 {reshaped}
dolbyVision.profile7 | Dolby Vision profile 7 | left as it is {refused}`
	if strings.Join(got, "\n") != want {
		t.Errorf("the transcoder's rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
	if rules.Transcoder.Version == nil || *rules.Transcoder.Version != "1.4.0" || rules.Transcoder.ReportedAt.IsZero() {
		t.Errorf("the transcoder's report: %+v", rules.Transcoder)
	}
	got = nil
	for _, r := range rules.Packager.Rules {
		got = append(got, row(r))
	}
	if want := `surroundAudio | Surround track | 5.1 E-AC-3, 448 kbit/s {surroundAudio=eac3, surroundLayout=5.1, surroundBitrate=448k}
stereoAudio | Stereo track | AAC, 192 kbit/s {stereoAudio=aac, stereoBitrate=192k}
imageSubtitles | Image subtitles kept | PGS, VobSub`; strings.Join(got, "\n") != want {
		t.Errorf("the packager's rules:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}

	before := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_workerrules WHERE rules::text LIKE '%"movie1080Mbps": 8%'`)
	for _, tc := range []struct {
		worker, body string
		says         string
	}{
		{"analyzer", `{"rules": {}}`, "no such worker"},
		{Transcoder, `not json`, "the body is no report of rules"},
		{Transcoder, `{"version": "1", "rules": []}`, "rules is no JSON object"},
		{Transcoder, `{"rules": {"caps": {"movie1080Mbps": "eight"}}}`, "caps.movie1080Mbps is a JSON string, and it is a number"},
		{Transcoder, `{"rules": {"caps": {"movie1080Mbps": -1}}}`, "caps.movie1080Mbps is -1, and it is 0 or above"},
		{Transcoder, `{"rules": {"dolbyVision": {"profile5": true}}}`, "is a JSON bool, and it is a string"},
		{Packager, `{"rules": {"surroundBitrate": "loud"}}`, `surroundBitrate is "loud", which is no bit rate`},
		{Packager, `{"rules": {"imageSubtitles": "pgs"}}`, "imageSubtitles is a JSON string, and it is a list"},
		{Packager, `{"version": "` + strings.Repeat("v", 256) + `", "rules": {}}`, "version is longer than 255 characters"},
	} {
		_, err := s.ReportRules(ctx, tc.worker, []byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s %s: %v, want it refused saying %q", tc.worker, tc.body, err, tc.says)
		}
	}
	if after := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_workerrules WHERE rules::text LIKE '%"movie1080Mbps": 8%'`); after != before {
		t.Error("a refused report changed the rules kept")
	}
	var kept map[string]any
	var raw string
	if err := st.Pool().QueryRow(ctx, `SELECT rules::text FROM com_nalet_katalog_workerrules WHERE worker = 'packager'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &kept); err != nil || kept["surroundAudio"] != "eac3" {
		t.Errorf("the packager's rules kept: %s", raw)
	}
}
