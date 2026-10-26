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
	var out []text
	find := func(source string, ti title, r records) {
		t.Helper()
		res := evaluate(ti, r, rs)
		if len(res.Findings) == 0 {
			t.Fatalf("%s: nothing found", source)
		}
		for _, f := range res.Findings {
			out = append(out, text{"finding", source, f.Rule + " text", f.Text})
			if f.Measured != nil {
				out = append(out, text{"finding", source, f.Rule + " measured", *f.Measured})
			}
			if f.Limit != nil {
				out = append(out, text{"finding", source, f.Rule + " limit", *f.Limit})
			}
		}
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
	at := mustTime(t, "2026-10-26T10:00:00Z")
	find("a film whose package failed, a retry scheduled", unpackaged(func(ti *title) {
		ti.Transcode = status(processing.StatusDone)
		ti.Package = step{Status: strp(processing.StatusFailed), Error: strp("shaka-packager exited 1"), NextRetryAt: &at}
	}), records{})
	find("a film whose package was skipped", unpackaged(func(ti *title) {
		ti.Transcode = status(processing.StatusDone)
		ti.Package = step{Status: strp(processing.StatusSkipped), Error: strp("the source has no video stream")}
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
	notEval("the transcoder makes H.264 at the source’s size", strings.Replace(transcoderReport, `"ladder": "source:hevc"`,
		`"ladder": "source:h264"`, 1), packagerReport, true, 0)
	notEval("the transcoder makes no rendition at the source’s size", strings.Replace(transcoderReport, `"ladder": "source:hevc"`,
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
		w := queueWords(state, reason)
		if w == nil {
			t.Fatalf("the queue's %s reason %q says nothing", state, reason)
		}
		out = append(out, text{"queueReason", source, state, *w})
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
	queue("failed", "transcode failed: ffmpeg exited 1", "failed in its transcode")
	queue("failed", "package failed: shaka-packager exited 1", "failed in its package")
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

	for _, b := range []struct{ source, words string }{
		{"its original deleted after packaging", deletedText}, {"its original being deleted after packaging", deletingText},
		{"no file", noFileText}, {"a disc image", discText},
	} {
		out = append(out, text{"reworkBlocked", b.source, "reason", b.words})
	}

	for _, m := range []struct {
		source          string
		queued, already int
		skipped         map[string]int
		by              graph.QueuedBy
	}{
		{"12 queued", 12, 0, nil, graph.QueuedBy{}},
		{"1 queued", 1, 0, nil, graph.QueuedBy{}},
		{"1,284 queued by a filter", 1284, 0, nil, graph.QueuedBy{Matching: true}},
		{"12 queued, 3 queued already", 12, 3, nil, graph.QueuedBy{}},
		{"none queued, 1 queued already", 0, 1, nil, graph.QueuedBy{}},
		{"12 queued, 2 skipped, their originals deleted", 12, 0, map[string]int{graph.SkipDeleted: 2}, graph.QueuedBy{}},
		{"1 queued, 1 skipped, its original being deleted", 1, 0, map[string]int{graph.SkipDeleting: 1}, graph.QueuedBy{}},
		{"5 queued, 2 queued already, 4 skipped for three reasons", 5, 2,
			map[string]int{graph.SkipDeleted: 2, graph.SkipNoFile: 1, graph.SkipDiscImage: 1}, graph.QueuedBy{Matching: true}},
		{"none queued, 1 skipped, a disc image", 0, 0, map[string]int{graph.SkipDiscImage: 1}, graph.QueuedBy{}},
		{"none queued, 2 skipped, disc images", 0, 0, map[string]int{graph.SkipDiscImage: 2}, graph.QueuedBy{}},
		{"none queued, 2 skipped, no file", 0, 0, map[string]int{graph.SkipNoFile: 2}, graph.QueuedBy{}},
		{"none queued, 1 skipped, a series without episodes", 0, 0, map[string]int{graph.SkipNoEpisodes: 1}, graph.QueuedBy{}},
		{"none queued, 1 skipped, not in the catalog", 0, 0, map[string]int{graph.SkipUnknown: 1}, graph.QueuedBy{}},
		{"none queued, 2 skipped, a season and a title not in the catalog", 0, 0,
			map[string]int{graph.SkipType: 1, graph.SkipUnknown: 1}, graph.QueuedBy{}},
		{"none queued, 1 skipped for a reason the words don't know", 0, 0, map[string]int{skipOther: 1}, graph.QueuedBy{}},
		{"no title named", 0, 0, nil, graph.QueuedBy{}},
		{"no title fits the filter", 0, 0, nil, graph.QueuedBy{Matching: true}},
		{"none queued by a filter, the 1 that fits queued already", 0, 0, nil, graph.QueuedBy{Matching: true, InQueue: 1}},
		{"none queued by a filter, the 3 that fit queued already", 0, 0, nil, graph.QueuedBy{Matching: true, InQueue: 3}},
		{"none queued by a filter, the 1 that fits blocked", 0, 0, nil, graph.QueuedBy{Matching: true, Blocked: 1}},
		{"none queued by a filter, the 2 that fit blocked", 0, 0, nil, graph.QueuedBy{Matching: true, Blocked: 2}},
		{"none queued by a filter, of 4 that fit 3 queued already and 1 blocked", 0, 0, nil,
			graph.QueuedBy{Matching: true, InQueue: 3, Blocked: 1}},
		{"none queued by a filter, of 2 that fit 1 queued already and 1 blocked", 0, 0, nil,
			graph.QueuedBy{Matching: true, InQueue: 1, Blocked: 1}},
	} {
		out = append(out, text{"ComplianceQueued.message", m.source, "message", queuedWords(m.queued, m.already, m.skipped, m.by)})
	}

	for _, k := range skipKinds {
		out = append(out, text{"ComplianceSkip", "a title skipped: " + k, "reason", skipReason(k)})
	}
	for _, n := range titleCases {
		out = append(out, text{"ComplianceSkip", n.source, "title", titleText(uuid(99), n.typ, n.title, n.series, n.year, n.season, n.episode)})
	}

	for _, w := range []struct{ name, raw string }{{Transcoder, transcoderReport}, {Packager, packagerReport}} {
		for _, r := range settingsOf([]byte(w.raw)) {
			out = append(out, text{"pipelineRules", w.name + " " + r.Key, "label", r.Label})
			out = append(out, text{"pipelineRules", w.name + " " + r.Key, "value", r.Value})
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
		`successfully|please|pipeline|instance|deployment|candidate|may|might|cap)\b`)
	// unitRE is a number and a unit, kept on one line.
	unitRE = regexp.MustCompile(`\d (GB|MB|Mbit/s|kbit/s)\b`)
)

// Every text the check writes for a reader is a sentence in sentence case
// that ends as one, in the reader's words, with no word that promises,
// hedges, or names the queue's work otherwise than "process again"; a
// number keeps its unit on its line, and an apostrophe is the typographic
// one. With COMPLIANCE_TEXTS set, the list of them goes to that file, for a
// review of the words.
func TestTheServerTexts(t *testing.T) {
	texts := serverTexts(t)
	for _, x := range texts {
		sentenceText := x.section != "pipelineRules" && x.section != "ComplianceSkip" &&
			!strings.HasSuffix(x.field, "measured") && !strings.HasSuffix(x.field, "limit")
		if x.section == "ComplianceSkip" && x.field == "reason" {
			// a clause the console says after a colon: no capital, no full stop
			if first := []rune(x.words)[0]; strings.ToLower(string(first)) != string(first) || strings.HasSuffix(x.words, ".") {
				t.Errorf("%s, %s: %q is no clause to say after a colon", x.section, x.source, x.words)
			}
		}
		if sentenceText {
			if first := []rune(x.words)[0]; strings.ToUpper(string(first)) != string(first) {
				t.Errorf("%s, %s: %q does not start as a sentence", x.section, x.source, x.words)
			}
			if !strings.HasSuffix(x.words, ".") {
				t.Errorf("%s, %s: %q does not end as a sentence", x.section, x.source, x.words)
			}
		}
		if m := leaks.FindString(x.words); m != "" && !(x.section == "pipelineRules" && x.field == "value") {
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
	b.WriteString("The rules, as the workers report them: the transcoder allows movies 8 Mbit/s (1080) and 14 Mbit/s (2160) of\n")
	b.WriteString("video and a 15 GiB file, episodes 6 and 8 Mbit/s; it encodes with hevc_nvenc, preset p5, cq 23, ladder\n")
	b.WriteString("`source:hevc`. The packager makes a 5.1 E-AC-3 track at 448k and the stereo track at 192k, and keeps PGS and\n")
	b.WriteString("VobSub subtitles. Sizes are in decimal units, as the clients write them (1 GB is 1,000,000,000 bytes), so\n")
	b.WriteString("the transcoder’s 15 GiB reads 16.1 GB. Sizes and bit rates are written with a no-break space before their\n")
	b.WriteString("unit, and every apostrophe is the typographic one (’).\n\n")
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
		"The video runs at 38 Mbit/s, above the 20 Mbit/s the transcoder allows for a 2160 movie.",
		"The original is larger than the 32.2 GB the transcoder allows for a movie",
		"The 7.1 AC-3 track the packager makes is missing.",
		"[2 channels / 7.1 AC-3]",
		"The source’s PGS subtitles in German are missing from the package. The packager keeps PGS subtitles.",
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
		{languageName("de"), "German"}, {languageName("zh"), "Chinese"}, {languageName("und"), "an unknown language"},
	} {
		if got := strings.ReplaceAll(tc.got, nbsp, " "); got != tc.want {
			t.Errorf("%q, want %q", got, tc.want)
		}
	}
}
