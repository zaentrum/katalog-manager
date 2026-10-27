package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/retry"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The titles of the compliance test, by their ids as the library names a
// folder: a remux packaged with its video copied as it is, an episode of a
// series within its caps, and a film never packaged.
const (
	remuxID   = "0c0c0c0c-0000-4000-8000-000000000101"
	remuxVer  = "0c0c0c0c-0000-4000-8000-000000000201"
	remuxSrc  = "0c0c0c0c-0000-4000-8000-000000000301"
	showID    = "0c0c0c0c-0000-4000-8000-000000000102"
	episodeID = "0c0c0c0c-0000-4000-8000-000000000103"
	epVer     = "0c0c0c0c-0000-4000-8000-000000000203"
	epSrc     = "0c0c0c0c-0000-4000-8000-000000000303"
	neverID   = "0c0c0c0c-0000-4000-8000-000000000104"
)

// layOut gives a title its original, its complete version and their records
// in the library, its package's video at rate bit/s, copied.
func layOut(t *testing.T, in *instance, id, typ, title, parent, ver, src string, width, height int, rate float64,
	original, size int64) {
	t.Helper()
	st := in.st
	storetest.AddItem(t, st, id, typ, title, parent)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, recordedat)
		VALUES ($1, $2, 'original.mkv', $3, 'present', now())`, src, id, original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes, sourceid)
		VALUES (gen_random_uuid()::varchar, $1, '/library/original.mkv', true, 'primary', $2, $3)`, id, original, src)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, completedat)
		VALUES ($1, $2, ARRAY[$3]::varchar[], 'complete', now())`, ver, id, src)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, codec, sizebytes, versionid)
		VALUES (gen_random_uuid()::varchar, $1, 'package.json', false, 'packaged', 'hvc1.2.4.L153.B0', $2, $3)`, id, size, ver)
	series := ""
	if typ == "episode" {
		series = parent
	}
	dir := library.PathsOf(in.cfg).ItemDir(library.Place{ID: id, Type: typ, SeriesID: series})
	librarytest.WriteVersion(t, library.VersionDir(dir, ver), librarytest.Version{VersionID: ver, PackageID: ver, SourceIDs: []string{src},
		CreatedAt: "2026-10-01T08:00:00Z", Package: map[string]any{"sizeBytes": size, "durationMs": 7200000,
			"renditions": map[string]any{"video": []any{map[string]any{"codec": "hvc1.2.4.L153.B0", "width": width, "height": height,
				"bitrateBps": rate, "encoder": "copy"}}, "audio": []any{map[string]any{"channels": 2, "bitrateBps": 192000}}},
			"essence": map[string]any{"surround": false, "maxAudioChannels": 2}}})
	librarytest.WriteSource(t, library.SourceDir(dir, src), map[string]any{"sourceId": src,
		"streams": []any{map[string]any{"index": 0, "type": "video", "codec": "hevc", "width": width, "height": height}},
		"essence": map[string]any{"surround": false, "maxAudioChannels": 2}}, nil)
}

// offer is what the console sends back with a request by the filter of what
// it offered (filter, the report's arguments, "" none): the titles and the
// fingerprint of the report's queueable, as it read them, the request's
// arguments after the filter's.
func offer(t *testing.T, in *instance, token, filter string) string {
	t.Helper()
	args, sep := "", ""
	if filter != "" {
		args, sep = "("+filter+")", ", "
	}
	_, a := in.gql(t, "/api/manage/query", token, `{ libraryCompliance`+args+` { queueable { titles fingerprint } } }`)
	var d struct {
		LibraryCompliance struct {
			Queueable struct {
				Titles      int
				Fingerprint string
			}
		}
	}
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &d) != nil || d.LibraryCompliance.Queueable.Fingerprint == "" {
		t.Fatalf("the queueable of %q: %s %v", filter, a.Data, a.Errors)
	}
	q := d.LibraryCompliance.Queueable
	return filter + sep + "expected: " + strconv.Itoa(q.Titles) + ", fingerprint: " + strconv.Quote(q.Fingerprint)
}

// The library's compliance through the service as main wires it, with the
// real retries and their queue: the workers report their rules on the REST
// route (a viewer may not); the check finds the remux over its cap; an admin
// reads the report and the rules in GraphQL, and reworks the titles through
// the re-encode queue, by a filter and by name, a title queued already
// counted so; a viewer, an addon and the service account are refused every
// field of it; and nothing is queued but what an admin asked for.
func TestLibraryComplianceThroughTheService(t *testing.T) {
	b := &markingBus{}
	in := newInstanceWired(t, wiring{pipeline: func(st *store.Store) graph.Pipeline {
		return retry.New(st, processing.DefaultPolicy(), b, time.Minute)
	}, compliance: true})
	st := in.st
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('set-layout', 'library.layout', 'v2')`)
	viewer, addon, service, admin := in.iss.Viewer(t), in.iss.Addon(t), in.iss.Service(t, "zaentrum-manager"), in.iss.Admin(t)

	transcoder := `{"version": "3f2a9c1", "rules": {"caps": {"movie1080Mbps": 8, "movie2160Mbps": 14, "movieMaxGiB": 15,
		"episode1080Mbps": 6, "episode2160Mbps": 8}, "bucket2160": {"minHeight": 2000, "minWidth": 3200},
		"encoder": {"codec": "hevc_nvenc", "preset": "p5", "cq": 23, "maxrate1080Mbps": null, "maxrate2160Mbps": null},
		"ladder": "source:hevc", "extraLadder": "source:hevc"}}`
	packager := `{"version": "3f2a9c1", "rules": {"surroundAudio": "eac3", "surroundLayout": "5.1", "surroundBitrate": "448k",
		"stereoAudio": "aac", "stereoBitrate": "192k", "imageSubtitles": ["pgs", "vobsub"]}}`
	if code := in.put(t, "/api/analyze/rules/transcoder", viewer, transcoder); code != http.StatusForbidden {
		t.Errorf("a viewer's report of rules: %d, want 403", code)
	}
	for worker, body := range map[string]string{"transcoder": transcoder, "packager": packager} {
		if code := in.put(t, "/api/analyze/rules/"+worker, service, body); code != http.StatusOK {
			t.Fatalf("the %s's report: %d", worker, code)
		}
	}
	if code := in.put(t, "/api/analyze/rules/analyzer", service, `{"rules": {}}`); code != http.StatusBadRequest {
		t.Errorf("a report of a worker that has no rules here: %d, want 400", code)
	}
	if code := in.put(t, "/api/analyze/rules/transcoder", service, `{"rules": {"caps": {"movie1080Mbps": "eight"}}}`); code != http.StatusBadRequest {
		t.Errorf("a report whose cap is no number: %d, want 400", code)
	}

	layOut(t, in, remuxID, "movie", "A Remux Kept As It Was", "", remuxVer, remuxSrc, 3840, 2160, 73e6, 63e9, 59e9)
	storetest.AddItem(t, st, showID, "series", "A Coastal Show", "")
	layOut(t, in, episodeID, "episode", "An Episode Within Its Caps", showID, epVer, epSrc, 1920, 1080, 5e6, 2e9, 1.8e9)
	storetest.AddItem(t, st, neverID, "movie", "A Film Never Packaged", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes, resolution)
		VALUES ('a-never', $1, '/library/arrivals/a-film-never-packaged.mkv', true, 4000000000, '1920x1080')`, neverID)
	if n, err := in.check.Pass(ctx); err != nil || n != 4 {
		t.Fatalf("the pass checked %d titles, %v; want the remux, the episode, the film never packaged and the film whose transcode failed", n, err)
	}

	const report = `{ libraryCompliance {
		rules { transcoder { worker version reportedAt rules { key label value detail } } packager { worker version } }
		notEvaluated evaluatedAt evaluating pending total checkedTitles nextCursor
		matching { titles originalBytes packageBytes estimatedBytes fingerprint } queue { queued processing processed failed }
		queueable { titles originalBytes packageBytes estimatedBytes fingerprint }
		summary { rule titles packageBytes estimatedBytes evaluated }
		rows { itemId title year kind seriesId seriesTitle season episode findings { rule text measured limit detail }
			originalBytes packageBytes estimatedBytes queue queueReason queueDetail reworkBlocked evaluatedAt
			resolution videoCodec videoBitrate videoBitrateEstimated bitrateCap sizeCap durationSeconds packageAudio
			originalAudio outcome } } }`
	for who, token := range map[string]string{"a viewer": viewer, "an addon": addon, "the service account": service} {
		for _, doc := range []string{report, `{ pipelineRules { transcoder { worker } } }`,
			`mutation { queueRework(items: ["` + remuxID + `"]) { queued } }`,
			`mutation { queueReworkMatching(rules: [OVER_BITRATE_CAP], expected: 0, fingerprint: "") { queued } }`,
			`mutation { refreshCompliance }`} {
			_, a := in.gql(t, "/api/manage/query", token, doc)
			if len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
				t.Errorf("%s: %s: %v, want it refused", who, doc, a.Errors)
			}
		}
	}

	var data struct {
		LibraryCompliance struct {
			Rules struct {
				Transcoder struct {
					Worker, Version, ReportedAt string
					Rules                       []struct {
						Key, Label, Value string
						Detail            *string
					}
				}
				Packager struct{ Worker, Version string }
			}
			NotEvaluated        []string
			EvaluatedAt         *string
			Evaluating          bool
			Pending             int
			Total               int
			CheckedTitles       *int
			NextCursor          *string
			Matching, Queueable struct {
				Titles                      int
				OriginalBytes, PackageBytes float64
				EstimatedBytes              *float64
				Fingerprint                 string
			}
			Queue   struct{ Queued, Processing, Processed, Failed int }
			Summary []struct {
				Rule           string
				Titles         int
				PackageBytes   float64
				EstimatedBytes *float64
				Evaluated      bool
			}
			Rows []struct {
				ItemID, Title, Kind string
				SeriesTitle         *string
				Findings            []struct {
					Rule, Text              string
					Measured, Limit, Detail *string
				}
				OriginalBytes, PackageBytes, EstimatedBytes *float64
				Queue, QueueReason, QueueDetail             *string
				ReworkBlocked                               *string
				Resolution, VideoCodec, Outcome             *string
				VideoBitrate, BitrateCap, SizeCap           *float64
				DurationSeconds                             *float64
				VideoBitrateEstimated                       bool
				PackageAudio, OriginalAudio                 *string
				EvaluatedAt                                 string
			}
		}
	}
	query := func(doc string) {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) > 0 {
			t.Fatalf("%s: %v", doc, a.Errors)
		}
		if err := json.Unmarshal(a.Data, &data); err != nil {
			t.Fatal(err)
		}
	}
	query(report)
	rep := data.LibraryCompliance
	var rows []string
	for _, r := range rep.Rows {
		var rules []string
		for _, f := range r.Findings {
			rules = append(rules, f.Rule)
		}
		rows = append(rows, r.Title+" ("+r.Kind+"): "+strings.Join(rules, ", "))
	}
	if got := strings.Join(rows, "\n"); got != "A Remux Kept As It Was (MOVIE): OVER_BITRATE_CAP, OVER_SIZE_CAP\n"+
		"A Film Never Packaged (MOVIE): NOT_PACKAGED\nA Film (MOVIE): PACKAGING_FAILED" {
		t.Errorf("the report's rows:\n%s", got)
	}
	remux := rep.Rows[0]
	spaced := func(s string) string { return strings.NewReplacer("\u00a0", " ", "\u2011", "-").Replace(s) }
	if remux.ItemID != remuxID || spaced(remux.Findings[0].Text) != "The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder "+
		"allows for a 2160p movie." || spaced(*remux.Findings[0].Measured) != "73 Mbit/s" || spaced(*remux.Findings[0].Limit) != "14 Mbit/s" ||
		*remux.PackageBytes != 59e9 || *remux.OriginalBytes != 63e9 || remux.EstimatedBytes == nil || remux.Queue != nil {
		t.Errorf("the remux: %+v", remux)
	}
	if remux.Resolution == nil || *remux.Resolution != "3840\u00d72160" || remux.VideoCodec == nil || *remux.VideoCodec != "HEVC" ||
		remux.VideoBitrate == nil || *remux.VideoBitrate != 73e6 || remux.BitrateCap == nil || *remux.BitrateCap != 14e6 ||
		remux.SizeCap == nil || *remux.SizeCap != 15*1024*1024*1024 || remux.Outcome == nil || *remux.Outcome != "REENCODE" ||
		remux.DurationSeconds == nil || *remux.DurationSeconds != 7200 || remux.VideoBitrateEstimated {
		t.Errorf("the remux's columns: %+v", remux)
	}
	if f := rep.Rows[2].Findings[0]; f.Text != "The transcoder failed." || f.Detail == nil || *f.Detail != "ffmpeg exited 1" {
		t.Errorf("the film whose transcode failed: %+v", f)
	}
	if r := rep.Rules.Transcoder.Rules[12]; r.Key != "ladder" || r.Value != "one, at the original’s size, in HEVC" ||
		r.Detail == nil || *r.Detail != "source:hevc" {
		t.Errorf("the transcoder's renditions: %+v", r)
	}
	// sorted by title, ascending, through GraphQL
	_, sorted := in.gql(t, "/api/manage/query", admin, `{ libraryCompliance(sort: TITLE, descending: false) { rows { title } } }`)
	var byTitle struct {
		LibraryCompliance struct{ Rows []struct{ Title string } }
	}
	if len(sorted.Errors) > 0 || json.Unmarshal(sorted.Data, &byTitle) != nil || len(byTitle.LibraryCompliance.Rows) != 3 ||
		byTitle.LibraryCompliance.Rows[0].Title != "A Film" || byTitle.LibraryCompliance.Rows[2].Title != "A Remux Kept As It Was" {
		t.Errorf("sorted by title: %s %v", sorted.Data, sorted.Errors)
	}
	if _, err := time.Parse(time.RFC3339, remux.EvaluatedAt); err != nil {
		t.Errorf("evaluatedAt %q: %v", remux.EvaluatedAt, err)
	}
	if m := rep.Matching; m.Titles != 3 || m.OriginalBytes != 63e9+4e9 || m.PackageBytes != 59e9 || m.EstimatedBytes == nil {
		t.Errorf("the totals: %+v", m)
	}
	if q, m := rep.Queueable, rep.Matching; q.Titles != m.Titles || q.OriginalBytes != m.OriginalBytes || q.PackageBytes != m.PackageBytes ||
		q.EstimatedBytes == nil || *q.EstimatedBytes != *m.EstimatedBytes || q.Fingerprint != m.Fingerprint ||
		q.Fingerprint != graph.Fingerprint([]string{remuxID, neverID, "m1"}) {
		t.Errorf("queueable %+v, before anything is queued: want the totals, %+v, the same titles", q, m)
	}
	if rep.CheckedTitles == nil || *rep.CheckedTitles != 4 {
		t.Errorf("titles checked: %v, want the 4, the episode within its caps too", rep.CheckedTitles)
	}
	if rep.Total != 3 || rep.Pending != 0 || rep.Evaluating || rep.EvaluatedAt == nil || rep.NextCursor != nil ||
		len(rep.NotEvaluated) != 0 || len(rep.Summary) != len(compliance7) {
		t.Errorf("the report: total %d, pending %d, evaluating %v, evaluated %v, next %v, not evaluated %q, %d summaries",
			rep.Total, rep.Pending, rep.Evaluating, rep.EvaluatedAt, rep.NextCursor, rep.NotEvaluated, len(rep.Summary))
	}
	// both workers reported, NVENC making every video HEVC: every rule is checked
	for _, c := range rep.Summary {
		if !c.Evaluated {
			t.Errorf("the summary of %s: not evaluated, with both workers' rules", c.Rule)
		}
	}
	if tr := rep.Rules.Transcoder; tr.Worker != "transcoder" || tr.Version != "3f2a9c1" || len(tr.Rules) != 14 ||
		tr.Rules[1].Key != "caps.movie2160Mbps" || spaced(tr.Rules[1].Value) != "at most 14 Mbit/s" || rep.Rules.Packager.Worker != "packager" {
		t.Errorf("the rules: %+v", rep.Rules)
	}

	// the remux by a filter, then the remux and the film never packaged by
	// name: the remux is queued already
	if b.take() != "" {
		t.Fatal("something was sent before anything was queued")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`); n != 0 {
		t.Fatalf("%d titles queued before an admin asked", n)
	}
	var queued struct {
		Q struct {
			Queued, AlreadyQueued, Skipped         int
			SkippedTitles                          []struct{ ItemID, Title, Reason string }
			QueuedText, AlreadyText, NotQueuedText *string
			Message                                string
		}
	}
	mutate := func(doc string) {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) > 0 {
			t.Fatalf("%s: %v", doc, a.Errors)
		}
		if err := json.Unmarshal(a.Data, &queued); err != nil {
			t.Fatal(err)
		}
	}
	said := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	const texts = "queuedText alreadyText notQueuedText"
	mutate(`mutation { q: queueReworkMatching(` + offer(t, in, admin, `kind: MOVIE, rules: [OVER_BITRATE_CAP, VIDEO_NOT_HEVC], search: "remux"`) +
		`) { queued alreadyQueued skipped message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 1 || q.AlreadyQueued != 0 || q.Skipped != 0 || q.Message != "1 title queued to be processed again." ||
		said(q.QueuedText) != "1 title queued to be processed again" || q.AlreadyText != nil || q.NotQueuedText != nil {
		t.Errorf("queueReworkMatching: %+v", q)
	}
	// a title the catalog doesn't hold is named with its id, and why
	const goneID = "0c0c0c0c-0000-4000-8000-000000000199"
	mutate(`mutation { q: queueRework(items: ["` + remuxID + `", "` + neverID + `", "` + showID + `", "` + goneID + `"]) {
		queued alreadyQueued skipped skippedTitles { itemId title reason } message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 2 || q.AlreadyQueued != 1 || q.Skipped != 1 || len(q.SkippedTitles) != 1 ||
		q.SkippedTitles[0].ItemID != goneID || q.SkippedTitles[0].Title != goneID ||
		q.SkippedTitles[0].Reason != "it is not in the catalog" ||
		said(q.QueuedText) != "2 titles queued to be processed again" || said(q.AlreadyText) != "1 title already in the queue" ||
		said(q.NotQueuedText) != "1 title wasn’t queued: it is not in the catalog." ||
		q.Message != "2 titles queued to be processed again. 1 title already in the queue. 1 title wasn’t queued: it is not in the catalog." {
		t.Errorf("queueRework: %+v", q)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE state = 'queued' AND selection = 'items'`); got != 3 {
		t.Errorf("%d titles queued, want the remux, the film and the series' episode", got)
	}
	if b.take() != "" {
		t.Error("a rework was sent straight away, and not through the queue's pace")
	}
	query(report)
	if q := data.LibraryCompliance.Rows[0].Queue; q == nil || *q != "queued" {
		t.Errorf("the remux's place in the queue: %v", q)
	}
	if q := data.LibraryCompliance.Queue; q.Queued != 3 || q.Processing != 0 || q.Processed != 0 || q.Failed != 0 {
		t.Errorf("the queue's counts: %+v", q)
	}

	// what "every title that fits" offers is what it queues: the film never
	// packaged is being processed, the remux waits, and a disc image can't
	// be processed again; the film whose transcode failed is left
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_reencodequeue SET state = 'sent', sentat = now() WHERE item_id = $1`, neverID)
	const discID = "0c0c0c0c-0000-4000-8000-000000000105"
	storetest.AddItem(t, st, discID, "movie", "A Disc Image", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes, resolution)
		VALUES ('a-disc', $1, '/library/arrivals/a-disc-image.iso', true, 7000000000, '1920x1080')`, discID)
	if n, err := in.check.Pass(ctx); err != nil || n != 1 {
		t.Fatalf("the pass checked %d titles, %v; want the disc image", n, err)
	}
	query(report)
	if r := data.LibraryCompliance; r.Matching.Titles != 4 || r.Queueable.Titles != 1 || r.Queueable.OriginalBytes != 0 ||
		r.Queueable.PackageBytes != 0 || r.Queueable.EstimatedBytes != nil || r.Queue.Processing != 1 ||
		r.Queueable.Fingerprint != graph.Fingerprint([]string{"m1"}) ||
		r.Matching.Fingerprint != graph.Fingerprint([]string{remuxID, neverID, "m1", discID}) {
		t.Errorf("queueable %+v of matching %+v, the queue %+v; want the film whose transcode failed alone", r.Queueable, r.Matching, r.Queue)
	}
	// and counts those in the queue already, the remux and the film never
	// packaged; of the disc image, which it never offered, it says nothing
	offered := data.LibraryCompliance.Queueable
	mutate(`mutation { q: queueReworkMatching(expected: ` + strconv.Itoa(offered.Titles) + `, fingerprint: ` +
		strconv.Quote(offered.Fingerprint) + `) { queued alreadyQueued skipped skippedTitles { itemId title reason } message } }`)
	if q := queued.Q; q.Queued != offered.Titles || q.AlreadyQueued != 2 || q.Skipped != 0 || len(q.SkippedTitles) != 0 ||
		q.Message != "1 title queued to be processed again. 2 titles already in the queue." {
		t.Errorf("queueReworkMatching, every title that fits: %+v; offered %+v", q, offered)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = 'm1' AND state = 'queued'`); got != 1 {
		t.Errorf("the film whose transcode failed: %d rows queued, want 1", got)
	}
	query(report)
	if q := data.LibraryCompliance.Queueable; q.Titles != 0 {
		t.Errorf("queueable once queued: %+v", q)
	}
	mutate(`mutation { q: queueReworkMatching(` + offer(t, in, admin, "") + `) { queued alreadyQueued skipped message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 0 || q.AlreadyQueued != 3 || q.Skipped != 0 || q.QueuedText != nil ||
		said(q.AlreadyText) != "3 titles already in the queue" || q.NotQueuedText != nil {
		t.Errorf("queueReworkMatching, none left to queue: %+v", q)
	}
	// a filter whose only title is in the queue says it is there
	mutate(`mutation { q: queueReworkMatching(` + offer(t, in, admin, `kind: MOVIE, rules: [OVER_SIZE_CAP]`) +
		`) { queued alreadyQueued skipped message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 0 || q.AlreadyQueued != 1 || q.Skipped != 0 || q.QueuedText != nil ||
		said(q.AlreadyText) != "1 title already in the queue" || q.NotQueuedText != nil || q.Message != "1 title already in the queue." {
		t.Errorf("queueReworkMatching of the remux, in the queue: %+v", q)
	}
	mutate(`mutation { q: queueRework(items: []) { queued alreadyQueued skipped message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 0 || said(q.NotQueuedText) != "Nothing was queued: no title was selected." {
		t.Errorf("queueRework of no title: %+v", q)
	}
	mutate(`mutation { q: queueRework(items: ["` + discID + `"]) { queued alreadyQueued skipped skippedTitles { itemId title reason } message } }`)
	if q := queued.Q; q.Queued != 0 || q.Skipped != 1 || len(q.SkippedTitles) != 1 || q.SkippedTitles[0].Title != "A Disc Image" ||
		q.SkippedTitles[0].Reason != "its file is a disc image, which the transcoder doesn’t encode" {
		t.Errorf("queueRework of the disc image: %+v", q)
	}
	mutate(`mutation { q: queueReworkMatching(` + offer(t, in, admin, `rules: [IMAGE_SUBTITLES_MISSING]`) +
		`) { queued alreadyQueued skipped message } }`)
	if q := queued.Q; q.Queued != 0 || q.Message != "Nothing was queued: no title fits the filter anymore." {
		t.Errorf("a filter no title matches: %+v", q)
	}

	// a film whose original is Dolby Vision profile 7, which the transcoder
	// leaves as it is: the check finds it so, and named by its id it is not
	// queued, through GraphQL or the REST route
	const dvID, dvVer, dvSrc = "0c0c0c0c-0000-4000-8000-000000000106", "0c0c0c0c-0000-4000-8000-000000000206",
		"0c0c0c0c-0000-4000-8000-000000000306"
	layOut(t, in, dvID, "movie", "A Film In Profile Seven", "", dvVer, dvSrc, 3840, 2160, 70e6, 60e9, 58e9)
	librarytest.WriteSource(t, library.SourceDir(library.PathsOf(in.cfg).ItemDir(library.Place{ID: dvID, Type: "movie"}), dvSrc),
		map[string]any{"sourceId": dvSrc, "streams": []any{map[string]any{"index": 0, "type": "video", "codec": "hevc",
			"width": 3840, "height": 2160, "hdr": map[string]any{"dolbyVision": map[string]any{"profile": 7}}}},
			"essence": map[string]any{"surround": false, "maxAudioChannels": 2}}, nil)
	if n, err := in.check.Pass(ctx); err != nil || n != 1 {
		t.Fatalf("the pass checked %d titles, %v; want the film in profile 7", n, err)
	}
	mutate(`mutation { q: queueRework(items: ["` + dvID + `"]) { queued alreadyQueued skipped skippedTitles { itemId title reason }
		message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 0 || q.Skipped != 1 || len(q.SkippedTitles) != 1 || q.SkippedTitles[0].ItemID != dvID ||
		q.SkippedTitles[0].Title != "A Film In Profile Seven" ||
		q.SkippedTitles[0].Reason != "Dolby Vision profile 7 needs a tone-mapping encode" ||
		said(q.NotQueuedText) != "1 title wasn’t queued: Dolby Vision profile 7 needs a tone-mapping encode." {
		t.Errorf("queueRework of the film in profile 7: %+v", q)
	}
	if code, body := in.post(t, "/api/library/reencode", service, `{"items": ["`+dvID+`"]}`); code != http.StatusOK ||
		!strings.Contains(body, `"queued":0`) ||
		!strings.Contains(body, "its original is Dolby Vision profile 7, which the transcoder leaves as it is") {
		t.Errorf("POST /api/library/reencode of the film in profile 7: %d %s", code, body)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = $1`, dvID); n != 0 {
		t.Errorf("the film in profile 7 was queued %d times", n)
	}

	_, a := in.gql(t, "/api/manage/query", admin, `mutation { refreshCompliance(kind: MOVIE) }`)
	if len(a.Errors) > 0 || string(a.Data) != `{"refreshCompliance":true}` {
		t.Errorf("refreshCompliance: %s %v", a.Data, a.Errors)
	}
	query(report)
	if p := data.LibraryCompliance.Pending; p != 5 {
		t.Errorf("after a refresh of the movies: %d pending, want the 5 movies", p)
	}
	_, a = in.gql(t, "/api/manage/query", admin, `{ pipelineRules { packager { worker version reportedAt rules { key value } } } }`)
	var rules struct {
		PipelineRules struct {
			Packager struct {
				Worker, Version, ReportedAt string
				Rules                       []struct{ Key, Value string }
			}
		}
	}
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &rules) != nil {
		t.Fatalf("pipelineRules: %s %v", a.Data, a.Errors)
	}
	if p := rules.PipelineRules.Packager; p.Worker != "packager" || p.Version != "3f2a9c1" || len(p.Rules) != 3 ||
		p.Rules[0].Key != "surroundAudio" || spaced(p.Rules[0].Value) != "5.1 E-AC-3, 448 kbit/s" ||
		p.Rules[1].Key != "stereoAudio" || spaced(p.Rules[1].Value) != "AAC, 192 kbit/s" ||
		p.Rules[2].Key != "imageSubtitles" || p.Rules[2].Value != "PGS, VobSub" {
		t.Errorf("the packager's rules: %+v", p)
	}
}

// A request by a filter that matches a title it queues and one processing
// again can't help queues the one, and its answer names no skipped title:
// the console never offers the other. Named by its id, that title is
// skipped, saying why.
func TestARequestByAFilterLeavesOutWhatCantBeProcessed(t *testing.T) {
	in := newInstanceWired(t, wiring{pipeline: func(st *store.Store) graph.Pipeline {
		return retry.New(st, processing.DefaultPolicy(), &markingBus{}, time.Minute)
	}, compliance: true})
	st, admin := in.st, in.iss.Admin(t)
	const discID = "0c0c0c0c-0000-4000-8000-000000000107"
	storetest.AddItem(t, st, discID, "movie", "A Disc Image", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes, resolution)
		VALUES ('a-disc', $1, '/library/arrivals/a-disc-image.iso', true, 7000000000, '1920x1080')`, discID)
	if n, err := in.check.Pass(context.Background()); err != nil || n != 2 {
		t.Fatalf("the pass checked %d titles, %v; want the film whose transcode failed and the disc image", n, err)
	}
	var answer struct {
		Q struct {
			Queued, AlreadyQueued, Skipped int
			SkippedTitles                  []struct{ ItemID, Reason string }
			NotQueuedText                  *string
			Message                        string
		}
	}
	mutate := func(doc string) {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) > 0 || json.Unmarshal(a.Data, &answer) != nil {
			t.Fatalf("%s: %s %v", doc, a.Data, a.Errors)
		}
	}
	// a console that offered other titles queues nothing, and says so: of
	// another count, from how many to how many; of as many others, that
	// they changed
	offered := graph.Fingerprint([]string{"m1"})
	for _, tc := range []struct{ args, want string }{
		{`expected: 2, fingerprint: "` + offered + `"`, "Nothing was queued: the matching titles changed, from 2 to 1."},
		{`expected: 1, fingerprint: "` + graph.Fingerprint([]string{"m2"}) + `"`, "Nothing was queued: the matching titles changed."},
		{`expected: 1, fingerprint: ""`, "Nothing was queued: the matching titles changed."},
	} {
		mutate(`mutation { q: queueReworkMatching(rules: [PACKAGING_FAILED], ` + tc.args + `) { queued alreadyQueued skipped
			skippedTitles { itemId reason } notQueuedText message } }`)
		if q := answer.Q; q.Queued != 0 || q.NotQueuedText == nil || *q.NotQueuedText != tc.want || q.Message != *q.NotQueuedText {
			t.Errorf("the filter, %s offered and m1 now: %+v, want %q", tc.args, q, tc.want)
		}
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue`); got != 0 {
		t.Errorf("%d titles queued, the titles changed", got)
	}
	if got := offer(t, in, admin, `rules: [PACKAGING_FAILED]`); got != `rules: [PACKAGING_FAILED], expected: 1, fingerprint: "`+offered+`"` {
		t.Errorf("the report offers %s, want the film whose transcode failed", got)
	}
	mutate(`mutation { q: queueReworkMatching(` + offer(t, in, admin, `rules: [PACKAGING_FAILED]`) + `) { queued alreadyQueued skipped
		skippedTitles { itemId reason } notQueuedText message } }`)
	if q := answer.Q; q.Queued != 1 || q.AlreadyQueued != 0 || q.Skipped != 0 || len(q.SkippedTitles) != 0 ||
		q.NotQueuedText != nil || q.Message != "1 title queued to be processed again." {
		t.Errorf("a filter of a title it queues and a disc image: %+v", q)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = 'm1'`); got != 1 {
		t.Errorf("the film whose transcode failed: %d rows queued, want 1", got)
	}
	mutate(`mutation { q: queueRework(items: ["` + discID + `"]) { queued alreadyQueued skipped
		skippedTitles { itemId reason } notQueuedText message } }`)
	if q := answer.Q; q.Queued != 0 || q.Skipped != 1 || len(q.SkippedTitles) != 1 || q.SkippedTitles[0].ItemID != discID ||
		q.SkippedTitles[0].Reason != "its file is a disc image, which the transcoder doesn’t encode" {
		t.Errorf("the disc image by its id: %+v", q)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = $1`, discID); got != 0 {
		t.Errorf("the disc image was queued %d times", got)
	}
}

// compliance7 are the rules a report sums up, one summary each.
var compliance7 = []string{"NOT_PACKAGED", "PACKAGING_FAILED", "OVER_BITRATE_CAP", "OVER_SIZE_CAP", "NO_SURROUND",
	"IMAGE_SUBTITLES_MISSING", "VIDEO_NOT_HEVC"}

// A list of originals to retire through the service as main wires it, with
// the real compliance check: an admin's dry run of two titles answers as it
// begins, runs in the background, and says of each why it would not be
// retired (the remux breaks its caps; the episode's version was never
// verified); a second list is refused while one runs, a run of titles named
// always; a run of a dry run's titles is refused while library.retire.list
// is dry-run, and with it on of a dry run that would retire none; an admin
// stops the list that runs; nothing is touched.
func TestAListOfOriginalsThroughTheService(t *testing.T) {
	in := newInstanceWired(t, wiring{compliance: true})
	st, ctx, admin := in.st, context.Background(), in.iss.Admin(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('set-layout', 'library.layout', 'v2')`)
	service := in.iss.Service(t, "zaentrum-manager")
	for worker, body := range map[string]string{
		"transcoder": `{"version": "3f2a9c1", "rules": {"caps": {"movie1080Mbps": 8, "movie2160Mbps": 14, "movieMaxGiB": 15,
			"episode1080Mbps": 6, "episode2160Mbps": 8}, "bucket2160": {"minHeight": 2000, "minWidth": 3200},
			"encoder": {"codec": "hevc_nvenc", "preset": "p5", "cq": 23, "maxrate1080Mbps": null, "maxrate2160Mbps": null},
			"ladder": "source:hevc", "extraLadder": "source:hevc"}}`,
		"packager": `{"version": "3f2a9c1", "rules": {"surroundAudio": "eac3", "surroundLayout": "5.1", "surroundBitrate": "448k",
			"stereoAudio": "aac", "stereoBitrate": "192k", "imageSubtitles": ["pgs", "vobsub"]}}`,
	} {
		if code := in.put(t, "/api/analyze/rules/"+worker, service, body); code != http.StatusOK {
			t.Fatalf("the %s's report: %d", worker, code)
		}
	}
	layOut(t, in, remuxID, "movie", "A Remux Kept As It Was", "", remuxVer, remuxSrc, 3840, 2160, 73e6, 63e9, 59e9)
	storetest.AddItem(t, st, showID, "series", "A Coastal Show", "")
	layOut(t, in, episodeID, "episode", "An Episode Within Its Caps", showID, epVer, epSrc, 1920, 1080, 5e6, 2e9, 1.8e9)
	if _, err := in.check.Pass(ctx); err != nil {
		t.Fatal(err)
	}

	var started struct {
		RetireOriginals struct {
			ID, State, RequestedBy string
			DryRun                 bool
			Total, Pending         int
		}
	}
	_, a := in.gql(t, "/api/manage/query", admin, `mutation { retireOriginals(items: ["`+remuxID+`", "`+episodeID+`"]) {
		id state dryRun requestedBy total pending } }`)
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &started) != nil {
		t.Fatalf("retireOriginals: %s %v", a.Data, a.Errors)
	}
	if r := started.RetireOriginals; r.State != "RUNNING" || !r.DryRun || r.Total != 2 || r.Pending != 2 || r.RequestedBy == "" {
		t.Errorf("the run as it begins: %+v", r)
	}
	refused := func(doc string) string {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", admin, doc)
		if len(a.Errors) != 1 {
			t.Fatalf("%s: %s %v", doc, a.Data, a.Errors)
		}
		code, _ := a.Errors[0].Extensions["code"].(string)
		return code + ": " + a.Errors[0].Message
	}
	if got := refused(`mutation { retireOriginals(items: ["` + remuxID + `"]) { id } }`); !strings.HasPrefix(got, "RUNNING: ") {
		t.Errorf("a second list while one runs: %s", got)
	}
	named := `mutation { retireOriginals(items: ["` + remuxID + `"], dryRun: false) { id } }`
	if got := refused(named); got != "DRY_RUN_ONLY: library.retire.list is dry-run: a list only says what it would retire; "+
		"set it to on to retire the titles a dry run found it would" {
		t.Errorf("a run of titles named while library.retire.list is dry-run: %s", got)
	}
	if err := in.list.Work(ctx); err != nil {
		t.Fatal(err)
	}
	var run struct {
		RetireRun struct {
			ID, State                   string
			DryRun                      bool
			FinishedAt                  *string
			Total, WouldRetire, Skipped int
			Items                       []struct {
				Position            int
				ItemID, Title       string
				Result, Reason      *string
				SourceID, VersionID *string
			}
		}
	}
	_, a = in.gql(t, "/api/manage/query", admin, `{ retireRun { id state dryRun finishedAt total wouldRetire skipped
		items { position itemId title result reason sourceId versionId } } }`)
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &run) != nil {
		t.Fatalf("retireRun: %s %v", a.Data, a.Errors)
	}
	r := run.RetireRun
	if r.ID != started.RetireOriginals.ID || r.State != "DONE" || !r.DryRun || r.FinishedAt == nil || r.Total != 2 ||
		r.Skipped != 2 || len(r.Items) != 2 {
		t.Fatalf("the run: %+v", r)
	}
	if it := r.Items[0]; it.ItemID != remuxID || it.Title != "A Remux Kept As It Was" || it.Result == nil || *it.Result != "SKIPPED" ||
		it.Reason == nil || *it.Reason != "its complete version "+remuxVer+" was never verified" || it.VersionID == nil ||
		*it.VersionID != remuxVer {
		t.Errorf("the remux: %+v", it)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = 'full'`)
	if _, a = in.gql(t, "/api/manage/query", admin, `mutation { retireOriginals(items: ["`+remuxID+`"]) { id } }`); len(a.Errors) > 0 {
		t.Fatal(a.Errors)
	}
	if err := in.list.Work(ctx); err != nil {
		t.Fatal(err)
	}
	_, a = in.gql(t, "/api/manage/query", admin, `{ retireRun { id state dryRun finishedAt total wouldRetire skipped
		items(result: SKIPPED) { position itemId title result reason sourceId } } }`)
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &run) != nil {
		t.Fatalf("retireRun: %s %v", a.Data, a.Errors)
	}
	if it := run.RetireRun.Items; len(it) != 1 || it[0].Reason == nil ||
		*it[0].Reason != "the compliance check finds it breaks OVER_BITRATE_CAP, OVER_SIZE_CAP" ||
		it[0].SourceID == nil || *it[0].SourceID != remuxSrc {
		t.Errorf("the remux, verified: %+v", it)
	}

	// a run of the dry run's titles: library.retire.list dry-run (its
	// default) lets none, and on, the dry run would retire none
	from := `mutation { retireOriginals(fromRun: "` + run.RetireRun.ID + `", dryRun: false) { id } }`
	if got := refused(from); got != "DRY_RUN_ONLY: library.retire.list is dry-run: a list only says what it would retire; "+
		"set it to on to retire the titles a dry run found it would" {
		t.Errorf("a run while library.retire.list is dry-run: %s", got)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('set-list', 'library.retire.list', 'on')`)
	if got := refused(from); got != "FROM_RUN: dry run "+run.RetireRun.ID+" found no title whose original it would retire" {
		t.Errorf("a run of a dry run that would retire none: %s", got)
	}
	if got := refused(named); got != "NAMED_TITLES: a list of titles named is a dry run: retire the titles a dry run found "+
		"it would, with fromRun (its id)" {
		t.Errorf("a run of titles named: %s", got)
	}
	if got := refused(`mutation { retireOriginals(items: ["A Remux"]) { id } }`); got != `BAD_ID: an id of a title is a `+
		`lower-case UUID, and these are not: "A Remux"` {
		t.Errorf("a list of no id: %s", got)
	}

	// an admin stops the list that runs, after the title it is at
	var stopped struct {
		StopRetireRun *struct {
			ID, State string
			Error     *string
			Pending   int
		}
	}
	_, a = in.gql(t, "/api/manage/query", admin, `mutation { stopRetireRun { id state error pending } }`)
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &stopped) != nil || stopped.StopRetireRun != nil {
		t.Errorf("stopRetireRun with none running: %s %v", a.Data, a.Errors)
	}
	if _, a = in.gql(t, "/api/manage/query", admin, `mutation { retireOriginals(items: ["`+episodeID+`"]) { id } }`); len(a.Errors) > 0 {
		t.Fatal(a.Errors)
	}
	_, a = in.gql(t, "/api/manage/query", admin, `mutation { stopRetireRun { id state error pending } }`)
	if len(a.Errors) > 0 || json.Unmarshal(a.Data, &stopped) != nil || stopped.StopRetireRun == nil {
		t.Fatalf("stopRetireRun: %s %v", a.Data, a.Errors)
	}
	if s := stopped.StopRetireRun; s.State != "STOPPED" || s.Error == nil ||
		*s.Error != "stopped by "+started.RetireOriginals.RequestedBy+" (stopRetireRun)" || s.Pending != 1 {
		t.Errorf("the run stopped: %+v", s)
	}
	if err := in.list.Work(ctx); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_retirerunitems WHERE result IS NULL`); n != 1 {
		t.Errorf("%d titles to come of the run stopped, want 1: it worked on", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE state = 'present'`); n != 2 {
		t.Errorf("%d originals present after the dry runs, want 2", n)
	}
}
