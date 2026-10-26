package compliance

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// EnvTexts names the file TestTheServerTexts writes the list of every text
// the check writes to, for a review of the words.
const EnvTexts = "COMPLIANCE_TEXTS"

// text is one text the check writes, where it shows, and what it says.
type text struct{ section, source, field, words string }

// serverTexts renders every text the check writes for a reader, with the
// rules as the workers report them by default (movies 8 and 14 Mbit/s and
// 15 GiB, episodes 6 and 8 Mbit/s, hevc_nvenc, source:hevc; the packager's
// 5.1 E-AC-3 at 448k, stereo at 192k, PGS and VobSub) and the values of real
// titles: a 4K remux of 73 Mbit/s in a package of 58.8 GB.
func serverTexts(t *testing.T) []text {
	t.Helper()
	rs := stateOf(t, transcoderReport, packagerReport)
	rs.retries = true // the sweep retries a failed step
	var out []text
	findIn := func(rs rulesState, source string, ti title, r records) {
		t.Helper()
		res := evaluate(ti, r, rs)
		if len(res.Findings) == 0 {
			t.Fatalf("%s: nothing found", source)
		}
		m := res.Measures
		for _, c := range []struct {
			field string
			value *string
		}{{"resolution", m.Resolution}, {"videoCodec", m.VideoCodec}, {"packageAudio", m.PackageAudio},
			{"originalAudio", m.OriginalAudio}} {
			if c.value != nil {
				out = append(out, text{"columns", source, c.field, *c.value})
			}
		}
		for _, f := range res.Findings {
			out = append(out, text{"finding", source, f.Rule + " text", f.Text})
			if f.Measured != nil {
				out = append(out, text{"finding", source, f.Rule + " measured", *f.Measured})
			}
			if f.Limit != nil {
				out = append(out, text{"finding", source, f.Rule + " limit", *f.Limit})
			}
			if f.Detail != nil {
				out = append(out, text{"finding", source, f.Rule + " detail", *f.Detail})
			}
		}
	}
	find := func(source string, ti title, r records) {
		t.Helper()
		findIn(rs, source, ti, r)
	}
	remux := packagedTitle("movie")
	remux.VersionBytes = i64(63_400_000_000)
	pkg := func(width, height int, rate float64, encoder string) string {
		return `{"sizeBytes": 58800000000, "durationMs": 7980000, "renditions": {"video": [{"codec": "hvc1.2.4.L153.B0",
			"width": ` + itoa(width) + `, "height": ` + itoa(height) + `, "bitrateBps": ` + ftoa(rate) + `, "encoder": "` + encoder + `"}],
			"audio": [{"channels": 2, "bitrateBps": 192000}]}, "subtitles": [], "essence": {"surround": false, "maxAudioChannels": 2}}`
	}
	src := `{"file": {"sizeBytes": 63400000000}, "container": {"durationMs": 7980000},
		"streams": [{"index": 0, "type": "video", "codec": "hevc", "width": 3840, "height": 2160, "bitrate": 73000000},
		{"index": 1, "type": "audio", "codec": "truehd", "channels": 8},
		{"index": 2, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "de", "form": "image"},
		{"index": 3, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "fr", "form": "image"},
		{"index": 4, "type": "subtitle", "codec": "dvd_subtitle", "language": "it", "form": "image"}],
		"essence": {"surround": true, "maxAudioChannels": 8}}`
	find("a 4K remux of 73 Mbit/s, 8-channel sound, PGS and VobSub, its 58.8 GB package a copy", remux,
		recordsOf(t, pkg(3840, 2160, 73e6, "copy"), src))
	find("a 1080 movie of 9.4 Mbit/s", packagedTitle("movie"), recordsOf(t, pkgJSON(1920, 1080, 9.4e6, "copy"), srcJSON(1920, 1080)))
	find("a 1080 episode of 7 Mbit/s", packagedTitle("episode"), recordsOf(t, pkgJSON(1920, 1080, 7e6, "copy"), srcJSON(1920, 1080)))
	find("a 2160 episode of 12.5 Mbit/s", packagedTitle("episode"), recordsOf(t, pkgJSON(3840, 2160, 12.5e6, "copy"), srcJSON(3840, 2160)))
	sized := `{"sizeBytes": 31500000000, "durationMs": 7200000, "renditions": {"video": [{"codec": "hvc1.2.4.L153.B0",
		"width": 3840, "height": 2160, "encoder": "copy"}], "audio": [{"channels": 2, "bitrateBps": 192000}]}, "essence": {}}`
	find("a 2160 movie whose package gives no bit rate, measured by its size", packagedTitle("movie"),
		recordsOf(t, sized, `{"streams": [], "essence": {}}`))
	stereo := `{"renditions": {"video": [{"codec": "hvc1.1.6.L120.B0", "width": 1920, "height": 1080, "bitrateBps": 5000000,
		"encoder": "copy"}]}, "subtitles": [{"format": "pgs", "language": "ger"}], "essence": {"surround": false, "maxAudioChannels": 2}}`
	find("a 5.1 source, a stereo package, one language’s PGS missing", packagedTitle("movie"), recordsOf(t, stereo,
		`{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
		{"index": 1, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "de"},
		{"index": 2, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "ja"}],
		"essence": {"surround": true, "maxAudioChannels": 6}}`))
	find("a source with surround of no known channels", packagedTitle("movie"), recordsOf(t, stereo,
		`{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080}], "essence": {"surround": true}}`))
	find("an original whose PGS subtitles in Portuguese of Brazil and simplified Chinese are missing", packagedTitle("movie"),
		recordsOf(t, pkgJSON(1920, 1080, 5e6, "copy"), `{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
		{"index": 1, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "por", "variant": "Brazilian"},
		{"index": 2, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "chi", "variant": "Simplified"}],
		"essence": {"surround": false, "maxAudioChannels": 2}}`))
	h264 := `{"renditions": {"video": [{"codec": "avc1.640028", "width": 1920, "height": 1080, "bitrateBps": 6200000,
		"encoder": "copy"}]}, "essence": {}}`
	find("an H.264 package", packagedTitle("movie"), recordsOf(t, h264, srcJSON(1920, 1080)))
	unpackaged := func(mod func(*title)) title {
		p := "/library/arrivals/a-film.mkv"
		ti := title{ID: "a1", Kind: "movie", Path: &p, FileResolution: strp("1920x1080"), ItemDurMs: i64(6_000_000),
			Original: library.Original{State: library.OriginalPresent}}
		mod(&ti)
		return ti
	}
	find("a film never packaged", unpackaged(func(*title) {}), records{})
	find("a film taken in", unpackaged(func(ti *title) { ti.Taken = true }), records{})
	find("a film whose transcode and package finished without a package", unpackaged(func(ti *title) {
		ti.Transcode, ti.Package = status(processing.StatusNotApplicable), status(processing.StatusDone)
	}), records{})
	find("a film whose transcode failed", unpackaged(func(ti *title) {
		ti.Transcode = step{Status: strp(processing.StatusFailed), Error: strp("ffmpeg exited 1")}
	}), records{})
	find("a film whose transcode was refused", unpackaged(func(ti *title) {
		ti.Transcode = step{Status: strp(processing.StatusFailed),
			Error: strp("Dolby Vision profile 7 needs a tone-mapping encode; kept the original")}
	}), records{})
	find("a film whose transcode was refused for its colours on this transcoder", unpackaged(func(ti *title) {
		ti.Transcode = step{Status: strp(processing.StatusFailed),
			Error: strp("Dolby Vision profile 5 is reshaped to HDR10 on a Vulkan device, and this host has none " +
				"(no Vulkan loader); kept the original")}
	}), records{})
	find("a film whose transcode failed in ffprobe", unpackaged(func(ti *title) {
		ti.Transcode = step{Status: strp(processing.StatusFailed), Error: strp("ffprobe: exit status 1")}
	}), records{})
	at := mustTime(t, "2026-10-26T10:00:00Z")
	retried := func(ti *title) {
		ti.Transcode = status(processing.StatusDone)
		ti.Package = step{Status: strp(processing.StatusFailed), Error: strp("shaka-packager exited 1"), NextRetryAt: &at}
	}
	find("a film whose package failed, a retry scheduled", unpackaged(retried), records{})
	noSweep := rs
	noSweep.retries = false
	findIn(noSweep, "a film whose package failed, a retry scheduled and no sweep to send it", unpackaged(retried), records{})
	find("a film whose package was skipped", unpackaged(func(ti *title) {
		ti.Transcode = status(processing.StatusDone)
		ti.Package = step{Status: strp(processing.StatusSkipped), Error: strp("the source has no video stream")}
	}), records{})
	find("a film whose package was skipped for a reason the words don’t know", unpackaged(func(ti *title) {
		ti.Transcode = status(processing.StatusDone)
		ti.Package = step{Status: strp(processing.StatusSkipped), Error: strp("handoff v0 has no keyframe in its first 10s")}
	}), records{})
	find("a film whose file is a disc image", unpackaged(func(ti *title) { *ti.Path = "/library/arrivals/a-film.iso" }), records{})

	notEval := func(source, transcoder, packager string, v2 bool, unchecked int32) {
		t.Helper()
		rs := stateOf(t, transcoder, packager)
		rs.v2 = v2
		lines := notEvaluated(rs, unchecked)
		if len(lines) == 0 {
			t.Fatalf("%s: nothing not evaluated", source)
		}
		for _, l := range lines {
			out = append(out, text{"notEvaluated", source, "line", l})
		}
	}
	notEval("neither worker reported", "", "", true, 0)
	notEval("the transcoder reports no 2160 bucket, and no file size", strings.NewReplacer(
		`"bucket2160": {"minHeight": 2000, "minWidth": 3200},`, ``, `"movieMaxGiB": 15, `, ``).Replace(transcoderReport), packagerReport, true, 0)
	notEval("the transcoder reports no limit for 2160 movies, and none for 1080 episodes", strings.NewReplacer(
		`"movie2160Mbps": 14, `, ``, `"episode1080Mbps": 6, `, `"episode1080Mbps": 0, `, `"movieMaxGiB": 15, `, `"movieMaxGiB": 0, `).Replace(transcoderReport),
		packagerReport, true, 0)
	notEval("the transcoder on libx265 with the default ladder", strings.NewReplacer(`"ladder": "source:hevc"`, `"ladder": "source"`,
		`"codec": "hevc_nvenc"`, `"codec": "libx265"`).Replace(transcoderReport), packagerReport, true, 0)
	notEval("the transcoder makes H.264 at the original’s size", strings.Replace(transcoderReport, `"ladder": "source:hevc"`,
		`"ladder": "source:h264"`, 1), packagerReport, true, 0)
	notEval("the transcoder makes no rendition at the original’s size", strings.Replace(transcoderReport, `"ladder": "source:hevc"`,
		`"ladder": "720p:hevc"`, 1), packagerReport, true, 0)
	notEval("the transcoder reports no ladder", strings.Replace(transcoderReport, `"ladder": "source:hevc", `, ``, 1), packagerReport, true, 0)
	notEval("the transcoder reports no encoder, its ladder the default", strings.NewReplacer(`"ladder": "source:hevc"`, `"ladder": "source"`,
		`"codec": "hevc_nvenc", `, ``).Replace(transcoderReport), packagerReport, true, 0)
	notEval("the packager makes no surround track and keeps no image subtitles", transcoderReport,
		`{"surroundAudio": "off", "surroundBitrate": "448k", "stereoBitrate": "192k", "imageSubtitles": []}`, true, 0)
	notEval("the packager reports neither its surround track nor its image subtitles", transcoderReport,
		`{"surroundBitrate": "448k", "stereoBitrate": "192k"}`, true, 0)
	notEval("the legacy library layout", transcoderReport, packagerReport, false, 0)
	notEval("the records of 1 title can’t be read", transcoderReport, packagerReport, true, 1)
	notEval("the records of 2,140 titles can’t be read", transcoderReport, packagerReport, true, 2140)

	queue := func(state, reason, source string) {
		t.Helper()
		w, d := queueWords(state, reason)
		if w == nil {
			t.Fatalf("the queue's %s reason %q says nothing", state, reason)
		}
		out = append(out, text{"queueReason", source, state, *w})
		if d != nil {
			out = append(out, text{"queueReason", source, state + " detail", *d})
		}
	}
	queue("queued", "transcode is running: its worker last reported at 2026-10-26T09:40:00Z, and encoding it again would run it "+
		"twice; wait for it, or for its timeout of 6h", "queued, its transcode running")
	queue("queued", "package is running: its worker last reported at 2026-10-26T09:40:00Z, and encoding it again would run it "+
		"twice; wait for it, or for its timeout of 2h", "queued, its package running")
	queue("queued", "transcode is waiting for its worker since 2026-10-26T09:40:00Z, and encoding it again would run it twice; "+
		"wait for it, or for its timeout of 6h", "queued, its transcode waiting to start")
	queue("queued", "package is waiting for its worker since 2026-10-26T09:40:00Z, and encoding it again would run it twice; "+
		"wait for it, or for its timeout of 2h", "queued, its package waiting to start")
	queue("queued", "the re-encode could not be sent: broker: not the leader", "queued, its event not sent")
	queue("queued", "", "queued and not yet sent, as a request leaves it")
	queue("queued", "held back by an older queue", "queued, for a reason the words don’t know")
	queue("failed", "transcode failed: ffmpeg exited 1", "failed in its transcode")
	queue("failed", "package failed: shaka-packager exited 1", "failed in its package")
	queue("failed", "transcode failed: Dolby Vision profile 7 needs a tone-mapping encode; kept the original", "failed, its transcode refused")
	queue("failed", "transcode failed: no error given", "failed in its transcode, saying nothing")
	queue("failed", "package skipped: the source has no video stream", "failed, its package skipped")
	queue("failed", "package not applicable", "failed, its package not applicable")
	queue("failed", "its transcode and package are gone", "failed, its steps removed")
	queue("failed", "unknown item", "failed, the title deleted")
	queue("failed", "the original was deleted after packaging (event 2f1c9e7a-0000-4000-8000-000000000001, 2026-10-06T10:00:00Z); "+
		newArrivalText, "failed, its original deleted")
	queue("failed", "the original is being deleted after packaging (event 2f1c9e7a-0000-4000-8000-000000000001, "+
		"2026-10-06T10:00:00Z); "+newArrivalText, "failed, its original being deleted")
	queue("failed", "the movie has no file to encode", "failed, no file")
	queue("failed", "its file is a "+processing.DiscImageReason, "failed, a disc image")
	queue("failed", "the library's compliance check found that its original is Dolby Vision profile 7, which the transcoder "+
		"leaves as it is", "failed before it was sent, the check finding Dolby Vision profile 7")
	queue("failed", "the library's compliance check found that its original has no video", "failed before it was sent, "+
		"the check finding no video")

	for _, b := range []struct{ source, kind string }{
		{"its original deleted after packaging", graph.SkipDeleted},
		{"its original being deleted after packaging", graph.SkipDeleting},
		{"no file", graph.SkipNoFile},
		{"a disc image", graph.SkipDiscImage},
		{"an original without video, as its record or its step says", blockedNoVideo},
		{"an original of Dolby Vision profile 7, as its record or its refused transcode says", blockedDolbyVision + "7"},
		{"an original of Dolby Vision profile 8.0, as its refused transcode says", blockedDolbyVision + "8.0"},
		{"an original of Dolby Vision profile 5, while the transcoder reports it can’t reshape it", blockedDolbyVision + "5"},
	} {
		out = append(out, text{"reworkBlocked", b.source, "reason", skipReason(b.kind)})
	}

	for _, m := range []struct {
		source          string
		queued, already int
		skipped         map[string]int
		matching        bool
	}{
		{"12 queued", 12, 0, nil, false},
		{"1 queued", 1, 0, nil, false},
		{"1,284 queued by a filter", 1284, 0, nil, true},
		{"12 queued, 3 already in the queue", 12, 3, nil, false},
		{"none queued, 1 already in the queue", 0, 1, nil, false},
		{"12 queued, 2 not, their originals deleted", 12, 0, map[string]int{graph.SkipDeleted: 2}, false},
		{"none queued, 3 not, their originals deleted", 0, 0, map[string]int{graph.SkipDeleted: 3}, false},
		{"1 queued, 1 not, its original being deleted", 1, 0, map[string]int{graph.SkipDeleting: 1}, false},
		{"5 queued, 2 already in the queue, 4 not for three reasons", 5, 2,
			map[string]int{graph.SkipDeleted: 2, graph.SkipNoFile: 1, graph.SkipDiscImage: 1}, true},
		{"none queued, 1 not, a disc image", 0, 0, map[string]int{graph.SkipDiscImage: 1}, false},
		{"none queued, 2 not, disc images", 0, 0, map[string]int{graph.SkipDiscImage: 2}, false},
		{"none queued, 2 not, no file", 0, 0, map[string]int{graph.SkipNoFile: 2}, false},
		{"none queued, 1 not, a series without episodes", 0, 0, map[string]int{graph.SkipNoEpisodes: 1}, false},
		{"none queued, 1 not, not in the catalog", 0, 0, map[string]int{graph.SkipUnknown: 1}, false},
		{"none queued, 2 not, a season and a title not in the catalog", 0, 0,
			map[string]int{graph.SkipType: 1, graph.SkipUnknown: 1}, false},
		{"none queued, 1 not, an original without video", 0, 0, map[string]int{graph.SkipNoVideo: 1}, false},
		{"none queued, 2 not, originals without video", 0, 0, map[string]int{graph.SkipNoVideo: 2}, false},
		{"none queued, 3 not, Dolby Vision profile 7", 0, 0, map[string]int{graph.SkipDolbyVision + "7": 3}, false},
		{"none queued, 1 not, Dolby Vision profile 5 on a transcoder without a GPU", 0, 0,
			map[string]int{graph.SkipDolbyVision + "5": 1}, false},
		{"2 queued, 5 not: a disc image, no video, Dolby Vision profiles 5 and 7", 2, 0, map[string]int{graph.SkipDiscImage: 1,
			graph.SkipNoVideo: 1, graph.SkipDolbyVision + "7": 2, graph.SkipDolbyVision + "5": 1}, false},
		{"none queued, 1 not for a reason the words don’t know", 0, 0, map[string]int{skipOther: 1}, false},
		{"no title selected", 0, 0, nil, false},
		{"no title fits the filter any more", 0, 0, nil, true},
	} {
		queued, already, notQueued := requestTexts(m.queued, m.already, m.skipped, m.matching)
		for _, f := range []struct {
			field string
			words *string
		}{{"queuedText", queued}, {"alreadyText", already}, {"notQueuedText", notQueued}} {
			if f.words != nil {
				out = append(out, text{"ComplianceQueued", m.source, f.field, *f.words})
			}
		}
		out = append(out, text{"ComplianceQueued", m.source, "message", requestMessage(queued, already, notQueued)})
	}

	for _, k := range skipKinds {
		out = append(out, text{"ComplianceSkip", "a title skipped: " + k, "reason", skipReason(k)})
	}
	for _, n := range titleCases {
		out = append(out, text{"ComplianceSkip", n.source, "title", titleText(uuid(99), n.typ, n.title, n.series, n.year, n.season, n.episode)})
	}

	for _, w := range []struct{ name, raw string }{
		{Transcoder, transcoderReport},
		{Packager, packagerReport},
		{"transcoder on libx265, with a ladder of three and the extras’ default", strings.NewReplacer(
			`"codec": "hevc_nvenc", "preset": "p5", "cq": 23`, `"codec": "libx265", "preset": "medium", "cq": null, "crf": 28`,
			`"ladder": "source:hevc"`, `"ladder": "source,720p:h264:2500k,480p"`, `"extraLadder": "source:hevc"`,
			`"extraLadder": "720p:h264,480p:h264"`, `"episode1080Mbps": 6`, `"episode1080Mbps": 0`,
			`"maxrate2160Mbps": null`, `"maxrate2160Mbps": 18`).Replace(transcoderReport)},
		{"packager making no surround track, of an image that doesn’t report its stereo codec", strings.NewReplacer(
			`"surroundAudio": "eac3"`, `"surroundAudio": "off"`, `"stereoAudio": "aac", `, ``,
			`["pgs", "vobsub"]`, `[]`).Replace(packagerReport)},
	} {
		for _, r := range settingsOf([]byte(w.raw)) {
			out = append(out, text{"pipelineRules", w.name + " " + r.Key, "label", r.Label})
			out = append(out, text{"pipelineRules", w.name + " " + r.Key, "value", r.Value})
			if r.Detail != nil {
				out = append(out, text{"pipelineRules", w.name + " " + r.Key, "detail", *r.Detail})
			}
		}
	}
	return out
}

// titleCases are the titles a skip names, as the catalog holds them, and
// what each is called.
var titleCases = []struct {
	source                string
	typ, title, series    *string
	year, season, episode *int32
	want                  string
}{
	{"a movie", strp("movie"), strp("A Harbour in Winter"), nil, i32p(1991), nil, nil, "A Harbour in Winter (1991)"},
	{"a movie of no known year", strp("movie"), strp("A Harbour in Winter"), nil, nil, nil, nil, "A Harbour in Winter"},
	{"an episode", strp("episode"), strp("The Storm"), strp("A Coastal Show"), nil, i32p(1), i32p(2), "A Coastal Show · S01E02 · The Storm"},
	{"an episode of no known season", strp("episode"), strp("The Storm"), strp("A Coastal Show"), nil, nil, i32p(2), "A Coastal Show · E02 · The Storm"},
	{"an episode of no known number or series", strp("episode"), strp("The Storm"), nil, nil, nil, nil, "The Storm"},
	{"a season", strp("season"), strp("Season 1"), strp("A Coastal Show"), nil, i32p(1), nil, "A Coastal Show · S01"},
	{"a series", strp("series"), strp("A Coastal Show"), nil, i32p(2019), nil, nil, "A Coastal Show (2019)"},
	{"a title the catalog holds nothing of", nil, nil, nil, nil, nil, nil, uuid(99)},
}

func i32p(n int32) *int32 { return &n }

// A skipped title is named as a list names it: a movie by its title and
// year, an episode by its series, its number and its title, a season by its
// series and its number; a part the catalog doesn't hold is left out, and a
// title of which it holds nothing is its id.
// A title queued and not yet sent waits in the queue: when the queue keeps
// no reason of it, and when the words don't know its reason, which is then
// its detail; one the queue holds back for its step says why. A title sent,
// done or failed without a reason is said nothing of.
func TestAQueuedTitleWaitsInTheQueue(t *testing.T) {
	said := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	for _, tc := range []struct{ state, reason, text, detail string }{
		{"queued", "", "It is waiting in the queue.", "-"},
		{"queued", "  ", "It is waiting in the queue.", "-"},
		{"queued", "held back by an older queue", "It is waiting in the queue.", "held back by an older queue"},
		{"queued", "transcode is waiting for its worker", "It is waiting for the transcoder.", "-"},
		{"sent", "", "-", "-"},
		{"done", "", "-", "-"},
		{"failed", "", "-", "-"},
		{"failed", "the library's compliance check found that its original is Dolby Vision profile 7, which the transcoder leaves " +
			"as it is", "Dolby Vision profile 7 needs a tone-mapping encode.", "-"},
		{"failed", "the library's compliance check found that its original has no video", "The original has no video.", "-"},
	} {
		text, detail := queueWords(tc.state, tc.reason)
		if said(text) != tc.text || said(detail) != tc.detail {
			t.Errorf("%s %q: %q {%s}, want %q {%s}", tc.state, tc.reason, said(text), said(detail), tc.text, tc.detail)
		}
	}
}

// A request's line of the titles it didn't queue says those the check found
// the transcoder can't encode as it says the others: an original without
// video, and a Dolby Vision profile, said of the profile whatever the
// number of its titles.
func TestARequestSaysWhatTheCheckFoundBlocked(t *testing.T) {
	for _, tc := range []struct {
		skipped map[string]int
		want    string
	}{
		{map[string]int{graph.SkipNoVideo: 1}, "1 title wasn’t queued: the original has no video."},
		{map[string]int{graph.SkipNoVideo: 2}, "2 titles weren’t queued: their originals have no video."},
		{map[string]int{graph.SkipDolbyVision + "7": 3}, "3 titles weren’t queued: Dolby Vision profile 7 needs a tone-mapping encode."},
		{map[string]int{graph.SkipDolbyVision + "5": 1}, "1 title wasn’t queued: Dolby Vision profile 5 needs a GPU to " +
			"reshape its colours, and this transcoder has none."},
		{map[string]int{graph.SkipDiscImage: 1, graph.SkipNoVideo: 1, graph.SkipDolbyVision + "7": 2, graph.SkipDolbyVision + "5": 1},
			"5 titles weren’t queued: 1 is a disc image, the original of 1 has no video, 1 is Dolby Vision profile 5 " +
				"and 2 are Dolby Vision profile 7."},
	} {
		if _, _, got := requestTexts(0, 0, tc.skipped, false); got == nil || *got != tc.want {
			t.Errorf("%v: %v, want %q", tc.skipped, got, tc.want)
		}
	}
}

func TestTheTitlesASkipNames(t *testing.T) {
	for _, n := range titleCases {
		if got := titleText(uuid(99), n.typ, n.title, n.series, n.year, n.season, n.episode); got != n.want {
			t.Errorf("%s: %q, want %q", n.source, got, n.want)
		}
	}
}

// newArrivalText ends the reason the queue keeps of a title whose original
// was deleted after packaging (retry's newArrival).
const newArrivalText = "a better version is a new arrival: put it in .work/replace and call replaceSource"

var (
	// leaks are words no text a reader sees may hold: the queue's verb is
	// "process again", and none promises, hedges or speaks the system's
	// words.
	leaks = regexp.MustCompile(`(?i)\b(rework\w*|re-?encod\w*|yet|saves?|saving|optimi[sz]\w*|seamless\w*|effortless\w*|simply|` +
		`successfully|please|pipeline|instance|deployment|candidate|may|might|cap|sources?|bitrates?|ffmpeg|ffprobe|shaka\S*|` +
		`(?:240|360|480|540|576|720|1080|1440|2160|4320))\b`)
	// plainHyphenRE is a format's or a codec's name written with a hyphen a
	// line may break at ("E-AC-3", "MPEG-4"), not the no-break one.
	plainHyphenRE = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[A-Z0-9]+`)
	// unitRE is a number and a unit, kept on one line.
	unitRE = regexp.MustCompile(`\d (GB|GiB|MB|Mbit/s|kbit/s|pixels)\b`)
)

// Every text the check writes for a reader is a sentence in sentence case
// that ends as one, in the reader's words, with no word that promises,
// hedges, or names the queue's work otherwise than "process again", no
// "source" for the original, no "bitrate" in one word, no height without
// its p and no tool's name; a number keeps its unit on its line, and an
// apostrophe is the typographic one. A detail is what a tool said, as it
// said it, and none of that holds of it. With COMPLIANCE_TEXTS set, the
// list of them goes to that file, for a review of the words. A format's or a
// codec's name keeps its hyphens on one line ("E‑AC‑3").
func TestTheServerTexts(t *testing.T) {
	texts := serverTexts(t)
	for _, x := range texts {
		if strings.HasSuffix(x.field, "detail") {
			if strings.TrimSpace(x.words) == "" {
				t.Errorf("%s, %s: an empty detail", x.section, x.source)
			}
			continue
		}
		sentenceText := x.section != "pipelineRules" && x.section != "ComplianceSkip" && x.section != "ComplianceQueued" &&
			x.section != "reworkBlocked" && x.section != "columns" && !strings.HasSuffix(x.field, "measured") &&
			!strings.HasSuffix(x.field, "limit")
		if x.section == "ComplianceQueued" {
			switch x.field {
			case "queuedText", "alreadyText":
				// a toast: no full stop
				if strings.HasSuffix(x.words, ".") {
					t.Errorf("%s, %s: the toast %q ends in a full stop", x.section, x.source, x.words)
				}
			default:
				if first := []rune(x.words)[0]; strings.ToUpper(string(first)) != string(first) || !strings.HasSuffix(x.words, ".") {
					t.Errorf("%s, %s: %q is no sentence", x.section, x.source, x.words)
				}
			}
		}
		if x.section == "ComplianceSkip" && x.field == "reason" || x.section == "reworkBlocked" {
			// a clause the console says after a colon: no capital but a
			// name's, no full stop
			first := []rune(x.words)[0]
			if strings.ToLower(string(first)) != string(first) && !strings.HasPrefix(x.words, "Dolby Vision") ||
				strings.HasSuffix(x.words, ".") {
				t.Errorf("%s, %s: %q is no clause to say after a colon", x.section, x.source, x.words)
			}
		}
		if m := plainHyphenRE.FindString(x.words); m != "" && !(x.section == "ComplianceSkip" && x.field == "title") {
			t.Errorf("%s, %s: %q writes %q with a hyphen a line may break at, not ‑", x.section, x.source, x.words, m)
		}
		if sentenceText {
			if first := []rune(x.words)[0]; strings.ToUpper(string(first)) != string(first) {
				t.Errorf("%s, %s: %q does not start as a sentence", x.section, x.source, x.words)
			}
			if !strings.HasSuffix(x.words, ".") {
				t.Errorf("%s, %s: %q does not end as a sentence", x.section, x.source, x.words)
			}
		}
		// a resolution is a width and a height, "1920×1080", no height alone
		if m := leaks.FindString(x.words); m != "" && !(x.section == "columns" && x.field == "resolution") {
			t.Errorf("%s, %s: %q says %q", x.section, x.source, x.words, m)
		}
		if m := unitRE.FindString(x.words); m != "" {
			t.Errorf("%s, %s: %q breaks %q across lines", x.section, x.source, x.words, m)
		}
		if strings.Contains(x.words, "  ") {
			t.Errorf("%s, %s: %q holds two spaces in a row", x.section, x.source, x.words)
		}
		if strings.Contains(x.words, "'") {
			t.Errorf("%s, %s: %q holds a typewriter apostrophe, not ’", x.section, x.source, x.words)
		}
	}
	path := os.Getenv(EnvTexts)
	if path == "" {
		return
	}
	var b strings.Builder
	b.WriteString("# The library compliance’s texts\n\n")
	b.WriteString("Every text katalog-manager writes for the compliance report, rendered with real values by\n")
	b.WriteString("`go test ./internal/compliance -run TestTheServerTexts` (`" + EnvTexts + "` set to this file).\n\n")
	for _, line := range []string{
		"The rules, as the workers report them: the transcoder allows movies 8 Mbit/s (1080p) and 14 Mbit/s (2160p) of",
		"video and a 15 GiB file, episodes 6 and 8 Mbit/s; it encodes with hevc_nvenc, preset p5, cq 23, ladder",
		"`source:hevc`, and reshapes Dolby Vision profile 5 to HDR10 (profile 7 it leaves as it is). The packager makes a",
		"5.1 E‑AC‑3 track at 448k and an AAC stereo track at 192k, and keeps PGS and VobSub subtitles. A failed step is",
		"retried by the sweep. Sizes are in decimal units, as the clients write them (1 GB is 1,000,000,000 bytes), so the",
		"transcoder’s 15 GiB reads 16.1 GB. Sizes and bit rates are written with a no-break space before their unit, a",
		"format’s name with no-break hyphens (E‑AC‑3), and every apostrophe is the typographic one (’). A line marked",
		"`detail` is what a tool said, as it said it: the console shows it under the row’s details, never in the text.",
	} {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	b.WriteString("Each line: the field, what the title or the state is, and the text.\n")
	section := ""
	for _, x := range texts {
		if x.section != section {
			section = x.section
			b.WriteString("\n## " + section + "\n\n")
		}
		b.WriteString("- `" + x.field + "` (" + x.source + "): " + x.words + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The limits a finding names are those the workers report, read when the
// text is written: other caps, another file size, another surround layout
// and other kept forms make other texts, and no number is typed in.
func TestTheLimitsAreTheReported(t *testing.T) {
	other := strings.NewReplacer(`"movie2160Mbps": 14`, `"movie2160Mbps": 20`, `"movieMaxGiB": 15`, `"movieMaxGiB": 30`).Replace(transcoderReport)
	otherP := strings.NewReplacer(`"surroundLayout": "5.1"`, `"surroundLayout": "7.1"`, `"eac3"`, `"ac3"`,
		`["pgs", "vobsub"]`, `["pgs"]`).Replace(packagerReport)
	ti := packagedTitle("movie")
	ti.VersionBytes = i64(63_400_000_000)
	src := `{"streams": [{"index": 0, "type": "video", "width": 3840, "height": 2160},
		{"index": 1, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "de"}], "essence": {"surround": true, "maxAudioChannels": 8}}`
	pkg := `{"renditions": {"video": [{"codec": "hvc1.2.4.L153.B0", "width": 3840, "height": 2160, "bitrateBps": 38000000,
		"encoder": "copy"}]}, "essence": {"surround": false, "maxAudioChannels": 2}}`
	got := found(evaluate(ti, recordsOf(t, pkg, src), stateOf(t, other, otherP)))
	for _, want := range []string{
		"The video runs at 38 Mbit/s, above the 20 Mbit/s the transcoder allows for a 2160p movie.",
		"The original is larger than the 32.2 GB the transcoder allows for a movie",
		"The 7.1 AC-3 track the packager makes is missing.",
		"[2 channels / 7.1 AC-3]",
		"The original’s PGS subtitles in German are missing from the package. The packager keeps PGS subtitles.",
		"[German / PGS]",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("with other rules:\n%s\nwant it to hold %q", got, want)
		}
	}
	// the packager's report without its layout: no layout is made up
	noLayout := strings.Replace(packagerReport, `"surroundLayout": "5.1", `, ``, 1)
	if got := found(evaluate(ti, recordsOf(t, pkg, src), stateOf(t, transcoderReport, noLayout))); !strings.Contains(got,
		"The E-AC-3 surround track the packager makes is missing.") || strings.Contains(got, "5.1") {
		t.Errorf("a packager's report without its layout:\n%s", got)
	}
}

// Sizes are written as the clients write them, in decimal units: GB with one
// decimal from 1 GB up, MB below it, something there never 0 MB; bit rates
// in Mbit/s, kbit/s below one; counts with a thousands separator.
func TestTheUnits(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{sizeText(58_800_000_000), "58.8 GB"}, {sizeText(18_000_000_000), "18 GB"}, {sizeText(1_400_000_000), "1.4 GB"},
		{sizeText(999_600_000), "1 GB"}, {sizeText(820_000_000), "820 MB"}, {sizeText(281_400_000), "281 MB"},
		{sizeText(300_000), "1 MB"}, {sizeText(0), "0 MB"}, {sizeText(15 * gib), "16.1 GB"},
		{rateText(73e6), "73 Mbit/s"}, {rateText(8.44e6), "8.4 Mbit/s"}, {rateText(448e3), "448 kbit/s"},
		{countText(2140), "2,140"}, {countText(1_234_567), "1,234,567"}, {countText(999), "999"},
		{titlesText(1), "1 title"}, {titlesText(2500), "2,500 titles"},
		{joinAnd([]string{"German"}), "German"}, {joinAnd([]string{"German", "French"}), "German and French"},
		{joinAnd([]string{"German", "French", "Italian"}), "German, French and Italian"},
		{audioFormatName("eac3", ""), "E-AC-3"}, {audioFormatName("ec-3", ""), "E-AC-3"}, {audioFormatName("mp4a.40.2", ""), "AAC"},
		{audioFormatName("dts", "DTS-HD MA"), "DTS-HD MA"}, {audioFormatName("dts", "DTS"), "DTS"},
		{audioFormatName("truehd", "Dolby TrueHD + Dolby Atmos"), "TrueHD Atmos"}, {audioFormatName("pcm_s24le", ""), "PCM"},
		{audioFormatName("eac3", "Dolby Digital Plus + Dolby Atmos"), "E-AC-3 Atmos"}, {audioFormatName("", ""), ""},
		{layoutText(8), "7.1"}, {layoutText(6), "5.1"}, {layoutText(2), "2.0"}, {layoutText(10), "10 channels"}, {layoutText(0), ""},
		{trackText(6, "dts", "DTS-HD MA"), "5.1 DTS-HD MA"}, {trackText(0, "aac", ""), "AAC"},
		{*audioText([]string{"5.1 E-AC-3", "2.0 AAC", "2.0 AAC"}), "5.1 E-AC-3 + 2.0 AAC ×2"},
		{languageName("de"), "German"}, {languageName("zh"), "Chinese"}, {languageName("und"), "an unknown language"},
		{languageName("ger"), "German"}, {languageName("pt-BR"), "Portuguese (Brazil)"}, {languageName("zh-Hans"), "Chinese (Simplified)"},
		{languageName("zh-Hant-TW"), "Chinese (Traditional, Taiwan)"}, {languageName("es-419"), "Spanish (Latin America)"},
		{languageName("de-CH"), "German (Switzerland)"}, {languageName("sr-Latn"), "Serbian (Latin)"},
	} {
		if got := strings.NewReplacer(nbsp, " ", nbhy, "-").Replace(tc.got); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}

// A ladder is said in words where its spec reads as one: how many
// renditions, at which size and in which format, and a maxrate a rung sets;
// on a CPU encoder, the rendition at the original's size that leaves its
// format to the default keeps H.264 as it is when browsers can play it. A
// spec the words can't read is said as it is.
func TestTheRenditionsInWords(t *testing.T) {
	for _, tc := range []struct {
		spec string
		cpu  bool
		want string
		ok   bool
	}{
		{"source:hevc", false, "one, at the original’s size, in HEVC", true},
		{"source", false, "one, at the original’s size, in HEVC", true},
		{"source", true, "one, at the original’s size, in HEVC (or H.264 as it is when browsers can play it)", true},
		{"source:hevc", true, "one, at the original’s size, in HEVC", true},
		{"source,720p", false, "two: at the original’s size in HEVC and 720p in H.264", true},
		{"720p:h264,480p:h264", false, "two, 720p and 480p, in H.264", true},
		{"source,720p:h264:2500k,480p", false,
			"three: at the original’s size in HEVC, 720p in H.264 (at most 2.5 Mbit/s) and 480p in H.264", true},
		{"1080p:hevc", false, "one, 1080p, in HEVC", true},
		{"source:av1", false, "", false},
		{"4k", false, "", false},
	} {
		got, ok := ladderWords(tc.spec, tc.cpu)
		if got = strings.ReplaceAll(got, nbsp, " "); got != tc.want || ok != tc.ok {
			t.Errorf("%s (cpu %v): %q, %v; want %q, %v", tc.spec, tc.cpu, got, ok, tc.want, tc.ok)
		}
	}
}
