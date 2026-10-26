package main

import (
	"context"
	"encoding/json"
	"net/http"
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
		"stereoBitrate": "192k", "imageSubtitles": ["pgs", "vobsub"]}}`
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
		notEvaluated evaluatedAt evaluating pending total nextCursor
		matching { titles originalBytes packageBytes estimatedBytes } queue { queued processing processed failed }
		queueable { titles originalBytes packageBytes estimatedBytes }
		summary { rule titles packageBytes estimatedBytes }
		rows { itemId title year kind seriesId seriesTitle season episode findings { rule text measured limit detail }
			originalBytes packageBytes estimatedBytes queue queueReason queueDetail reworkBlocked evaluatedAt } } }`
	for who, token := range map[string]string{"a viewer": viewer, "an addon": addon, "the service account": service} {
		for _, doc := range []string{report, `{ pipelineRules { transcoder { worker } } }`,
			`mutation { queueRework(items: ["` + remuxID + `"]) { queued } }`,
			`mutation { queueReworkMatching(rules: [OVER_BITRATE_CAP]) { queued } }`,
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
			NextCursor          *string
			Matching, Queueable struct {
				Titles                      int
				OriginalBytes, PackageBytes float64
				EstimatedBytes              *float64
			}
			Queue   struct{ Queued, Processing, Processed, Failed int }
			Summary []struct {
				Rule           string
				Titles         int
				PackageBytes   float64
				EstimatedBytes *float64
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
	spaced := func(s string) string { return strings.ReplaceAll(s, "\u00a0", " ") }
	if remux.ItemID != remuxID || spaced(remux.Findings[0].Text) != "The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder "+
		"allows for a 2160p movie." || spaced(*remux.Findings[0].Measured) != "73 Mbit/s" || spaced(*remux.Findings[0].Limit) != "14 Mbit/s" ||
		*remux.PackageBytes != 59e9 || *remux.OriginalBytes != 63e9 || remux.EstimatedBytes == nil || remux.Queue != nil {
		t.Errorf("the remux: %+v", remux)
	}
	if f := rep.Rows[2].Findings[0]; f.Text != "The transcoder failed." || f.Detail == nil || *f.Detail != "ffmpeg exited 1" {
		t.Errorf("the film whose transcode failed: %+v", f)
	}
	if r := rep.Rules.Transcoder.Rules[12]; r.Key != "ladder" || r.Value != "one, at the original’s size, in HEVC" ||
		r.Detail == nil || *r.Detail != "source:hevc" {
		t.Errorf("the transcoder's renditions: %+v", r)
	}
	if _, err := time.Parse(time.RFC3339, remux.EvaluatedAt); err != nil {
		t.Errorf("evaluatedAt %q: %v", remux.EvaluatedAt, err)
	}
	if m := rep.Matching; m.Titles != 3 || m.OriginalBytes != 63e9+4e9 || m.PackageBytes != 59e9 || m.EstimatedBytes == nil {
		t.Errorf("the totals: %+v", m)
	}
	if q, m := rep.Queueable, rep.Matching; q.Titles != m.Titles || q.OriginalBytes != m.OriginalBytes || q.PackageBytes != m.PackageBytes ||
		q.EstimatedBytes == nil || *q.EstimatedBytes != *m.EstimatedBytes {
		t.Errorf("queueable %+v, before anything is queued: want the totals, %+v", q, m)
	}
	if rep.Total != 3 || rep.Pending != 0 || rep.Evaluating || rep.EvaluatedAt == nil || rep.NextCursor != nil ||
		len(rep.NotEvaluated) != 0 || len(rep.Summary) != len(compliance7) {
		t.Errorf("the report: total %d, pending %d, evaluating %v, evaluated %v, next %v, not evaluated %q, %d summaries",
			rep.Total, rep.Pending, rep.Evaluating, rep.EvaluatedAt, rep.NextCursor, rep.NotEvaluated, len(rep.Summary))
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
	mutate(`mutation { q: queueReworkMatching(kind: MOVIE, rules: [OVER_BITRATE_CAP, VIDEO_NOT_HEVC], search: "remux") {
		queued alreadyQueued skipped message ` + texts + ` } }`)
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
		r.Queueable.PackageBytes != 0 || r.Queueable.EstimatedBytes != nil || r.Queue.Processing != 1 {
		t.Errorf("queueable %+v of matching %+v, the queue %+v; want the film whose transcode failed alone", r.Queueable, r.Matching, r.Queue)
	}
	offered := data.LibraryCompliance.Queueable
	mutate(`mutation { q: queueReworkMatching { queued alreadyQueued skipped skippedTitles { itemId title reason } message } }`)
	if q := queued.Q; q.Queued != offered.Titles || q.AlreadyQueued != 0 || q.Skipped != 0 || len(q.SkippedTitles) != 0 ||
		q.Message != "1 title queued to be processed again." {
		t.Errorf("queueReworkMatching, every title that fits: %+v; offered %+v", q, offered)
	}
	if got := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_reencodequeue WHERE item_id = 'm1' AND state = 'queued'`); got != 1 {
		t.Errorf("the film whose transcode failed: %d rows queued, want 1", got)
	}
	query(report)
	if q := data.LibraryCompliance.Queueable; q.Titles != 0 {
		t.Errorf("queueable once queued: %+v", q)
	}
	mutate(`mutation { q: queueReworkMatching { queued alreadyQueued skipped message ` + texts + ` } }`)
	if q := queued.Q; q.Queued != 0 || q.AlreadyQueued != 0 || q.Skipped != 0 || q.QueuedText != nil || q.AlreadyText != nil ||
		said(q.NotQueuedText) != "Nothing was queued: no title fits the filter anymore." || q.Message != said(q.NotQueuedText) {
		t.Errorf("queueReworkMatching, none left: %+v", q)
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
	mutate(`mutation { q: queueReworkMatching(rules: [IMAGE_SUBTITLES_MISSING]) { queued alreadyQueued skipped message } }`)
	if q := queued.Q; q.Queued != 0 || q.Message != "Nothing was queued: no title fits the filter anymore." {
		t.Errorf("a filter no title matches: %+v", q)
	}

	_, a := in.gql(t, "/api/manage/query", admin, `mutation { refreshCompliance(kind: MOVIE) }`)
	if len(a.Errors) > 0 || string(a.Data) != `{"refreshCompliance":true}` {
		t.Errorf("refreshCompliance: %s %v", a.Data, a.Errors)
	}
	query(report)
	if p := data.LibraryCompliance.Pending; p != 4 {
		t.Errorf("after a refresh of the movies: %d pending, want the 4 movies", p)
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
		p.Rules[2].Key != "imageSubtitles" || p.Rules[2].Value != "PGS, VobSub" {
		t.Errorf("the packager's rules: %+v", p)
	}
}

// compliance7 are the rules a report sums up, one summary each.
var compliance7 = []string{"NOT_PACKAGED", "PACKAGING_FAILED", "OVER_BITRATE_CAP", "OVER_SIZE_CAP", "NO_SURROUND",
	"IMAGE_SUBTITLES_MISSING", "VIDEO_NOT_HEVC"}
