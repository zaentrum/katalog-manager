package compliance

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// The reports the tests hold titles against, as the workers send them.
const (
	transcoderReport = `{"caps": {"movie1080Mbps": 8, "movie2160Mbps": 14, "movieMaxGiB": 15, "episode1080Mbps": 6, "episode2160Mbps": 8},
		"bucket2160": {"minHeight": 2000, "minWidth": 3200},
		"encoder": {"codec": "hevc_nvenc", "preset": "p5", "cq": 23, "maxrate1080Mbps": null, "maxrate2160Mbps": null},
		"ladder": "source:hevc", "extraLadder": "source:hevc"}`
	packagerReport = `{"surroundAudio": "eac3", "surroundLayout": "5.1", "surroundBitrate": "448k", "stereoBitrate": "192k",
		"imageSubtitles": ["pgs", "vobsub"]}`
)

// stateOf is the rules of the reports given ("" for a worker that did not
// report), on the v2 layout.
func stateOf(t *testing.T, transcoder, packager string) rulesState {
	t.Helper()
	rs := rulesState{v2: true}
	if transcoder != "" {
		var tr TranscoderRules
		if err := json.Unmarshal([]byte(transcoder), &tr); err != nil {
			t.Fatal(err)
		}
		rs.transcoder = &workerReport{raw: []byte(transcoder), hash: tr.hash(), t: &tr}
	}
	if packager != "" {
		var p PackagerRules
		if err := json.Unmarshal([]byte(packager), &p); err != nil {
			t.Fatal(err)
		}
		rs.packager = &workerReport{raw: []byte(packager), hash: p.hash(), p: &p}
	}
	return rs
}

// recordsOf are a title's records as the check reads them, from a package
// record and a source record written as JSON.
func recordsOf(t *testing.T, pkg, src string) records {
	t.Helper()
	var r records
	if pkg != "" {
		r.pkg = &packageRecord{}
		if err := json.Unmarshal([]byte(pkg), r.pkg); err != nil {
			t.Fatal(err)
		}
		var err error
		if r.pkgEssence, err = library.EssenceOf([]byte(pkg)); err != nil {
			t.Fatal(err)
		}
	}
	if src != "" {
		r.src = &sourceRecord{}
		if err := json.Unmarshal([]byte(src), r.src); err != nil {
			t.Fatal(err)
		}
		var err error
		if r.srcEssence, err = library.EssenceOf([]byte(src)); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func status(s string) step { return step{Status: &s} }

// packagedTitle is a movie or an episode with a complete version, its
// original present.
func packagedTitle(kind string) title {
	v := "9b0f7b9e-0000-4000-8000-000000000001"
	return title{ID: "a1a1a1a1-0000-4000-8000-000000000001", Kind: kind, Version: &v,
		VersionSources: []string{"5c5c5c5c-0000-4000-8000-000000000001"}, Original: library.Original{State: library.OriginalPresent},
		Transcode: status(processing.StatusNotApplicable), Package: status(processing.StatusDone)}
}

// pkgJSON is a package record whose top rendition is width x height at
// bitrate bit/s, made by encoder, the rest as a package of a film of two
// hours with a stereo track.
func pkgJSON(width, height int, bitrate float64, encoder string) string {
	return `{"sizeBytes": 9000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "hvc1.2.4.L153.B0", "width": ` + itoa(width) + `, "height": ` + itoa(height) +
		`, "bitrateBps": ` + ftoa(bitrate) + `, "encoder": "` + encoder + `"}],
		"audio": [{"channels": 2, "bitrateBps": 192000}]},
		"subtitles": [], "essence": {"surround": false, "maxAudioChannels": 2}}`
}

func srcJSON(width, height int) string {
	return `{"file": {"sizeBytes": 9500000000}, "container": {"durationMs": 7200000},
		"streams": [{"index": 0, "type": "video", "codec": "hevc", "width": ` + itoa(width) + `, "height": ` + itoa(height) + `,
		"dispositions": {"default": true}}, {"index": 1, "type": "audio", "codec": "ac3", "channels": 2}],
		"essence": {"surround": false, "maxAudioChannels": 2}}`
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func ftoa(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// found writes a result's findings, one a line: "RULE text [measured /
// limit]", a no-break space written as a space.
func found(r result) string {
	return strings.ReplaceAll(foundRaw(r), nbsp, " ")
}

func foundRaw(r result) string {
	var out []string
	for _, f := range r.Findings {
		s := f.Rule + " " + f.Text
		if f.Measured != nil || f.Limit != nil {
			m, l := "-", "-"
			if f.Measured != nil {
				m = *f.Measured
			}
			if f.Limit != nil {
				l = *f.Limit
			}
			s += " [" + m + " / " + l + "]"
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

// A package's video is held against the transcoder's cap of its kind and its
// bucket (2160 from 2000 lines tall or 3200 wide): within it, nothing; above
// it, beyond the tolerance, OVER_BITRATE_CAP with what was measured and the
// cap. A movie's caps are 8 and 14 Mbit/s, an episode's 6 and 8.
func TestTheBitrateCapsByKindAndBucket(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	for _, tc := range []struct {
		name          string
		kind          string
		width, height int
		rate          float64
		want          string
	}{
		{"a 1080 movie within", "movie", 1920, 1080, 7.9e6, ""},
		{"a 1080 movie over", "movie", 1920, 800, 9.4e6,
			"OVER_BITRATE_CAP The video runs at 9.4 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080 movie. [9.4 Mbit/s / 8 Mbit/s]"},
		{"a 2160 movie within", "movie", 3840, 2160, 13.9e6, ""},
		{"a 2160 movie over", "movie", 3840, 2160, 73e6,
			"OVER_BITRATE_CAP The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160 movie. [73 Mbit/s / 14 Mbit/s]"},
		{"a scope picture is 2160 by its width", "movie", 3840, 1600, 20e6,
			"OVER_BITRATE_CAP The video runs at 20 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160 movie. [20 Mbit/s / 14 Mbit/s]"},
		{"a 2560 wide picture is 1080", "movie", 2560, 1440, 12e6,
			"OVER_BITRATE_CAP The video runs at 12 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080 movie. [12 Mbit/s / 8 Mbit/s]"},
		{"a 1080 episode within", "episode", 1920, 1080, 5.9e6, ""},
		{"a 1080 episode over", "episode", 1920, 1080, 7e6,
			"OVER_BITRATE_CAP The video runs at 7 Mbit/s, above the 6 Mbit/s the transcoder allows for a 1080 episode. [7 Mbit/s / 6 Mbit/s]"},
		{"a 2160 episode within", "episode", 3840, 2160, 7.9e6, ""},
		{"a 2160 episode over", "episode", 3840, 2160, 12.5e6,
			"OVER_BITRATE_CAP The video runs at 12.5 Mbit/s, above the 8 Mbit/s the transcoder allows for a 2160 episode. [12.5 Mbit/s / 8 Mbit/s]"},
		{"a copy just above the cap, within the tolerance", "movie", 3840, 2160, 14.3e6, ""},
		{"a copy above the tolerance", "movie", 3840, 2160, 14.5e6,
			"OVER_BITRATE_CAP The video runs at 14.5 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160 movie. [14.5 Mbit/s / 14 Mbit/s]"},
	} {
		got := found(evaluate(packagedTitle(tc.kind), recordsOf(t, pkgJSON(tc.width, tc.height, tc.rate, "copy"),
			srcJSON(tc.width, tc.height)), rs))
		if got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// The bucket is the source's, its tallest and its widest video stream (cover
// art aside), as the transcoder judges it; without a source record the
// package's top rendition's size, which is the source's.
func TestTheBucketIsTheSources(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	cover := `{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
		{"index": 1, "type": "video", "width": 4000, "height": 4000, "dispositions": {"attachedPic": true}}], "essence": {}}`
	if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkgJSON(1920, 1080, 12e6, "copy"), cover), rs)); got !=
		"OVER_BITRATE_CAP The video runs at 12 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080 movie. [12 Mbit/s / 8 Mbit/s]" {
		t.Errorf("a 1080 source with large cover art: %q, want it in the 1080 bucket", got)
	}
	wide := `{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
		{"index": 2, "type": "video", "width": 3840, "height": 1608}], "essence": {}}`
	if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkgJSON(1920, 1080, 12e6, "copy"), wide), rs)); got != "" {
		t.Errorf("a source whose second stream is 3840 wide: %q, want it in the 2160 bucket, within its cap", got)
	}
	if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkgJSON(3840, 2160, 12e6, "copy"), ""), rs)); got != "" {
		t.Errorf("no source record, a 3840x2160 package at 12 Mbit/s: %q, want it within the 2160 cap", got)
	}
}

// A package the transcoder encoded is never flagged as over the cap it was
// encoded under: an encode capped at the cap is within it on average, and one
// whose details say its maxrate was the cap is within it whatever a measure
// says. An encode above its cap that no detail vouches for (an older
// transcoder's, with a higher maxrate) is over it.
func TestAnEncodeUnderTheCapIsNeverFlagged(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	enc := packagedTitle("movie")
	enc.Transcode = step{Status: strp(processing.StatusDone),
		Details: strp("profile=nvenc-2160p src_codec=hevc res=3840x2160 maxrate=14Mbps ladder=v0:hevc_nvenc:3840x2160")}
	for _, tc := range []struct {
		name string
		t    title
		rate float64
		enc  string
		want bool
	}{
		{"an encode at its cap", enc, 13.8e6, "hevc_nvenc", false},
		{"an encode measured above its cap, its maxrate the cap", enc, 15.2e6, "hevc_nvenc", false},
		{"an encode of an older transcoder, at 18 Mbit/s", packagedTitle("movie"), 18e6, "libx265", true},
		{"a copy at 18 Mbit/s", packagedTitle("movie"), 18e6, "copy", true},
	} {
		got := evaluate(tc.t, recordsOf(t, pkgJSON(3840, 2160, tc.rate, tc.enc), srcJSON(3840, 2160)), rs)
		if got.breaks(OverBitrateCap) != tc.want {
			t.Errorf("%s: %q, want flagged %v", tc.name, found(got), tc.want)
		}
	}
}

// Without the package's own measure, a copied video is measured by the
// stream of the source it copies, and anything else by the package's size
// less its audio.
func TestTheVideoIsMeasuredByTheMostReliableRecord(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	pkg := `{"sizeBytes": 31500000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "hvc1.2.4.L153.B0", "width": 3840, "height": 2160, "encoder": "copy"}],
		"audio": [{"channels": 2, "bitrateBps": 192000}], "audioSurround": [{"channels": 6, "bitrateBps": 640000}]},
		"essence": {}}`
	src := `{"streams": [{"index": 0, "type": "video", "width": 3840, "height": 2160, "bitrate": 30000000}], "essence": {}}`
	got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg, src), rs))
	if want := "OVER_BITRATE_CAP The video runs at 30 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160 movie. " +
		"[30 Mbit/s / 14 Mbit/s]"; got != want {
		t.Errorf("a copy measured by its source's stream:\n got  %q\n want %q", got, want)
	}
	got = found(evaluate(packagedTitle("movie"), recordsOf(t, pkg, `{"streams": [], "essence": {}}`), rs))
	// 31.5 GB over two hours is 35 Mbit/s, less 0.832 of audio
	if want := "OVER_BITRATE_CAP The video runs at about 34.2 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160 movie. " +
		"[34.2 Mbit/s / 14 Mbit/s]"; got != want {
		t.Errorf("a package measured by its size:\n got  %q\n want %q", got, want)
	}
	if got := found(evaluate(packagedTitle("movie"), records{}, rs)); got != "" {
		t.Errorf("no records: %q, want nothing measured, nothing found", got)
	}
}

// A movie whose original is above the file size cap breaks it when its video
// is copied as it is; an encode of it does not, an episode has no such rule,
// and a cap of 0 is off.
func TestTheFileSizeCap(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	big := int64(20 * gib)
	small := int64(10 * gib)
	for _, tc := range []struct {
		name    string
		kind    string
		size    int64
		encoder string
		report  string
		want    string
	}{
		{"a copied movie of 20 GiB", "movie", big, "copy", transcoderReport,
			"OVER_SIZE_CAP The original is larger than the 16.1 GB the transcoder allows for a movie, and the package keeps " +
				"its video as it is. [- / 16.1 GB]"},
		{"an encoded movie of 20 GiB", "movie", big, "hevc_nvenc", transcoderReport, ""},
		{"a copied movie of 10 GiB", "movie", small, "copy", transcoderReport, ""},
		{"a copied episode of 20 GiB", "episode", big, "copy", transcoderReport, ""},
		{"the cap off", "movie", big, "copy", strings.Replace(transcoderReport, `"movieMaxGiB": 15`, `"movieMaxGiB": 0`, 1), ""},
	} {
		ti := packagedTitle(tc.kind)
		ti.VersionBytes = &tc.size
		got := found(evaluate(ti, recordsOf(t, pkgJSON(1920, 1080, 5e6, tc.encoder), srcJSON(1920, 1080)), stateOf(t, tc.report, packagerReport)))
		if got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	// a package whose record names no encoder: the transcode says (not
	// applicable: nothing was encoded)
	ti := packagedTitle("movie")
	ti.VersionBytes = &big
	pkg := strings.Replace(pkgJSON(1920, 1080, 5e6, "copy"), `, "encoder": "copy"`, "", 1)
	if got := evaluate(ti, recordsOf(t, pkg, srcJSON(1920, 1080)), rs); !got.breaks(OverSizeCap) {
		t.Errorf("no encoder named, the transcode not applicable: %q, want the size cap broken", found(got))
	}
	ti.Transcode = status(processing.StatusDone)
	if got := evaluate(ti, recordsOf(t, pkg, srcJSON(1920, 1080)), rs); got.breaks(OverSizeCap) {
		t.Errorf("no encoder named, the transcode done (an encode): %q, want nothing", found(got))
	}
}

// With the packager making a 5.1 companion, a package that lacks its source's
// surround breaks NO_SURROUND, as the retire job reads the essences: a 5.1 or
// a 7.1 source and a stereo package; one that kept a 5.1 does not, and a
// packager that makes none checks nothing.
func TestSurroundLostVersusKept(t *testing.T) {
	src := func(ch int) string {
		return `{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080}], "essence": {"surround": true, "maxAudioChannels": ` + itoa(ch) + `}}`
	}
	pkg := func(ch int) string {
		return `{"renditions": {"video": [{"codec": "hvc1.1.6.L120.B0", "width": 1920, "height": 1080, "bitrateBps": 5000000, "encoder": "copy"}]},
			"essence": {"surround": ` + map[bool]string{true: "true", false: "false"}[ch > 2] + `, "maxAudioChannels": ` + itoa(ch) + `}}`
	}
	rs := stateOf(t, transcoderReport, packagerReport)
	for _, tc := range []struct {
		name     string
		had, got int
		want     string
	}{
		{"a 5.1 source, a stereo package", 6, 2,
			"NO_SURROUND The package has 2 audio channels, and the source has 6. The 5.1 E-AC-3 track the packager makes is missing. " +
				"[2 channels / 5.1 E-AC-3]"},
		{"a 7.1 source, a stereo package", 8, 2,
			"NO_SURROUND The package has 2 audio channels, and the source has 8. The 5.1 E-AC-3 track the packager makes is missing. " +
				"[2 channels / 5.1 E-AC-3]"},
		{"a 5.1 source, its 5.1 kept", 6, 6, ""},
		{"a 7.1 source, a 5.1 kept", 8, 6, ""},
	} {
		if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(tc.got), src(tc.had)), rs)); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	off := stateOf(t, transcoderReport, strings.Replace(packagerReport, `"eac3"`, `"off"`, 1))
	if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(2), src(6)), off)); got != "" {
		t.Errorf("a packager that makes no 5.1: %q, want nothing", got)
	}
}

// The source's image subtitles of a form the packager carries are found
// missing in each language the package carries none of them in; a form it
// does not carry (DVB) is not looked for, and a package subtitle made from a
// source stream counts for that stream's language whatever its own tag.
func TestImageSubtitles(t *testing.T) {
	src := `{"streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
		{"index": 2, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "en", "languageRaw": "eng", "form": "image"},
		{"index": 3, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "de", "languageRaw": "ger", "form": "image"},
		{"index": 4, "type": "subtitle", "codec": "dvd_subtitle", "language": "fr", "languageRaw": "fre", "form": "image"},
		{"index": 5, "type": "subtitle", "codec": "dvb_subtitle", "language": "it", "languageRaw": "ita", "form": "image"},
		{"index": 6, "type": "subtitle", "codec": "subrip", "language": "es", "languageRaw": "spa", "form": "text"}],
		"essence": {"imageSubtitles": true}}`
	pkg := func(subs string) string {
		return `{"renditions": {"video": [{"codec": "hvc1.1.6.L120.B0", "width": 1920, "height": 1080, "bitrateBps": 5000000, "encoder": "copy"}]},
			"subtitles": [` + subs + `], "essence": {}}`
	}
	rs := stateOf(t, transcoderReport, packagerReport)
	for _, tc := range []struct{ name, subs, want string }{
		{"only the english pgs carried", `{"format": "pgs", "language": "eng"}, {"format": "webvtt", "language": "spa"}`,
			"IMAGE_SUBTITLES_MISSING The source’s PGS subtitles in German, and its VobSub subtitles in French, are missing from " +
				"the package. The packager keeps PGS and VobSub subtitles. [German, French / PGS, VobSub]"},
		{"every image subtitle carried", `{"format": "pgs", "language": "eng"}, {"format": "pgs", "language": "ger"},
			{"format": "vobsub", "language": "fre"}`, ""},
		{"a subtitle by its source stream", `{"format": "pgs", "language": "eng"}, {"format": "pgs", "language": "und", "sourceStreamIndex": 3},
			{"format": "vobsub", "language": "fre"}`, ""},
		{"a text track is no image subtitle", `{"format": "webvtt", "language": "eng"}, {"format": "webvtt", "language": "ger"},
			{"format": "webvtt", "language": "fre"}`, "IMAGE_SUBTITLES_MISSING The source’s PGS subtitles in German and English, and " +
			"its VobSub subtitles in French, are missing from the package. The packager keeps PGS and VobSub subtitles. " +
			"[German, English, French / PGS, VobSub]"},
	} {
		if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(tc.subs), src), rs)); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	pgsOnly := stateOf(t, transcoderReport, strings.Replace(packagerReport, `["pgs", "vobsub"]`, `["pgs"]`, 1))
	got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(`{"format": "pgs", "language": "eng"}`), src), pgsOnly))
	if want := "IMAGE_SUBTITLES_MISSING The source’s PGS subtitles in German are missing from the package. The packager keeps " +
		"PGS subtitles. [German / PGS]"; got != want {
		t.Errorf("a packager that carries pgs alone:\n got  %q\n want %q", got, want)
	}
}

// A title with no package is NOT_PACKAGED while its original is present and
// nothing of its pipeline waits or runs; PACKAGING_FAILED when its transcode
// or its package failed or was skipped, saying why; a disc image is never
// encoded.
func TestATitleWithoutAPackage(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	base := func() title {
		p := "/library/arrivals/a-film.mkv"
		return title{ID: "a1", Kind: "movie", Path: &p, Original: library.Original{State: library.OriginalPresent}}
	}
	failed := base()
	failed.Transcode = step{Status: strp(processing.StatusFailed), Error: strp("ffmpeg exited 1")}
	retried := base()
	at := mustTime(t, "2026-10-26T10:00:00Z")
	retried.Package = step{Status: strp(processing.StatusFailed), Error: strp("shaka exited 2"), NextRetryAt: &at}
	skipped := base()
	skipped.Package = step{Status: strp(processing.StatusSkipped), Error: strp("no video stream")}
	disc := base()
	*disc.Path = "/library/arrivals/a-film.iso"
	pending := base()
	pending.Transcode = status(processing.StatusPending)
	running := base()
	running.Package = status(processing.StatusInProgress)
	taken := base()
	taken.Taken = true
	doneNoVersion := base()
	doneNoVersion.Transcode, doneNoVersion.Package = status(processing.StatusNotApplicable), status(processing.StatusDone)
	retired := base()
	retired.Path, retired.Original = nil, library.Original{State: library.OriginalRetired}
	for _, tc := range []struct {
		name string
		t    title
		want string
	}{
		{"no step yet", base(), "NOT_PACKAGED It has no package, and none is being made."},
		{"its transcode waiting", pending, ""},
		{"its package running", running, ""},
		{"taken in", taken, "NOT_PACKAGED It was taken in without a package and plays from its original."},
		{"done, and no package", doneNoVersion, "NOT_PACKAGED Its transcode and packaging finished, and no package is recorded."},
		{"its transcode failed", failed, "PACKAGING_FAILED The transcoder failed: ffmpeg exited 1."},
		{"its package failed, retried", retried, "PACKAGING_FAILED The packager failed: shaka exited 2. A retry is scheduled."},
		{"its package skipped", skipped, "PACKAGING_FAILED The packager skipped it: no video stream."},
		{"a disc image", disc, "PACKAGING_FAILED " + discImageText},
		{"no original and no package", retired, ""},
	} {
		if got := found(evaluate(tc.t, records{}, rs)); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// A package's video that is not HEVC breaks VIDEO_NOT_HEVC when the
// transcoder's rules make every package HEVC: its ladder names the source
// rung's codec, or its encoder is NVENC. On libx265 with the codec left to
// the default, an H.264 source is kept as it is, and the codec is not held
// against the rules.
func TestTheVideoCodec(t *testing.T) {
	h264 := `{"renditions": {"video": [{"codec": "avc1.640028", "width": 1920, "height": 1080, "bitrateBps": 5000000, "encoder": "copy"}]}, "essence": {}}`
	for _, tc := range []struct{ name, report, want string }{
		{"source:hevc", transcoderReport, "VIDEO_NOT_HEVC " + h264Found},
		{"the default ladder on nvenc", strings.Replace(transcoderReport, `"ladder": "source:hevc"`, `"ladder": "source"`, 1),
			"VIDEO_NOT_HEVC " + h264Found},
		{"the default ladder on libx265", strings.NewReplacer(`"ladder": "source:hevc"`, `"ladder": "source"`,
			`"codec": "hevc_nvenc"`, `"codec": "libx265"`).Replace(transcoderReport), ""},
		{"source:hevc on libx265", strings.Replace(transcoderReport, `"codec": "hevc_nvenc"`, `"codec": "libx265"`, 1),
			"VIDEO_NOT_HEVC " + h264Found},
		{"an h264 source rung", strings.Replace(transcoderReport, `"ladder": "source:hevc"`, `"ladder": "source:h264"`, 1), ""},
	} {
		if got := found(evaluate(packagedTitle("movie"), recordsOf(t, h264, srcJSON(1920, 1080)), stateOf(t, tc.report, packagerReport))); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	hevc := packagedTitle("movie")
	if got := found(evaluate(hevc, recordsOf(t, pkgJSON(1920, 1080, 5e6, "copy"), srcJSON(1920, 1080)), stateOf(t, transcoderReport, packagerReport))); got != "" {
		t.Errorf("an hevc package: %q", got)
	}
}

// A rule of a worker that never reported is not held against a title: no
// cap, codec or size without the transcoder's report, no surround or image
// subtitle without the packager's; and no default stands in for one.
func TestUnreportedRulesAreNotChecked(t *testing.T) {
	src := `{"streams": [{"index": 0, "type": "video", "width": 3840, "height": 2160},
		{"index": 1, "type": "subtitle", "codec": "hdmv_pgs_subtitle", "language": "de"}],
		"essence": {"surround": true, "maxAudioChannels": 6}}`
	pkg := `{"sizeBytes": 60000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "avc1.640033", "width": 3840, "height": 2160, "bitrateBps": 73000000, "encoder": "copy"}]},
		"subtitles": [], "essence": {"surround": false, "maxAudioChannels": 2}}`
	ti := packagedTitle("movie")
	big := int64(60e9)
	ti.VersionBytes = &big
	all := evaluate(ti, recordsOf(t, pkg, src), stateOf(t, transcoderReport, packagerReport))
	if got := strings.Join(all.rules(), " "); got != "OVER_BITRATE_CAP OVER_SIZE_CAP NO_SURROUND IMAGE_SUBTITLES_MISSING VIDEO_NOT_HEVC" {
		t.Fatalf("both workers reported: %s", got)
	}
	if got := strings.Join(evaluate(ti, recordsOf(t, pkg, src), stateOf(t, "", packagerReport)).rules(), " "); got != "NO_SURROUND IMAGE_SUBTITLES_MISSING" {
		t.Errorf("no transcoder's report: %s, want the packager's rules alone", got)
	}
	if got := strings.Join(evaluate(ti, recordsOf(t, pkg, src), stateOf(t, transcoderReport, "")).rules(), " "); got != "OVER_BITRATE_CAP OVER_SIZE_CAP VIDEO_NOT_HEVC" {
		t.Errorf("no packager's report: %s, want the transcoder's rules alone", got)
	}
	if got := evaluate(ti, recordsOf(t, pkg, src), stateOf(t, "", "")); len(got.Findings) != 0 || got.EstimatedBytes != nil {
		t.Errorf("no report at all: %q, estimated %v, want nothing", found(got), got.EstimatedBytes)
	}
	// a report without a cap checks nothing of it
	noCap := strings.Replace(transcoderReport, `"movie2160Mbps": 14, `, "", 1)
	if got := evaluate(ti, recordsOf(t, pkg, src), stateOf(t, noCap, packagerReport)); got.breaks(OverBitrateCap) {
		t.Errorf("a report without the 2160 movie cap: %q", found(got))
	}
}

// What a title's package is to be at most once it is processed again:
// processing that encodes the video makes it at the cap with the packager's
// stereo and 5.1 audio for the title's duration; processing that only adds
// the 5.1 keeps the package and adds the 5.1; a title processing again cannot
// help has none.
func TestTheEstimate(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	src := `{"streams": [{"index": 0, "type": "video", "width": 3840, "height": 2160}], "container": {"durationMs": 7200000},
		"essence": {"surround": true, "maxAudioChannels": 6}}`
	over := `{"sizeBytes": 59000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "hvc1.2.4.L153.B0", "width": 3840, "height": 2160, "bitrateBps": 65000000, "encoder": "copy"}]},
		"essence": {"surround": true, "maxAudioChannels": 6}}`
	got := evaluate(packagedTitle("movie"), recordsOf(t, over, src), rs)
	// (14 Mbit/s + 192k + 448k) over 7200 s
	if want := int64((14e6 + 192e3 + 448e3) * 7200 / 8); got.EstimatedBytes == nil || *got.EstimatedBytes != want {
		t.Errorf("a 2160 movie over its cap: estimated %v, want %d", got.EstimatedBytes, want)
	}
	stereo := `{"sizeBytes": 4000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "hvc1.1.6.L120.B0", "width": 1920, "height": 1080, "bitrateBps": 4000000, "encoder": "copy"}]},
		"essence": {"surround": false, "maxAudioChannels": 2}}`
	got = evaluate(packagedTitle("movie"), recordsOf(t, stereo, strings.Replace(src, "3840, \"height\": 2160", "1920, \"height\": 1080", 1)), rs)
	if want := int64(4e9 + 448e3*7200/8); !got.breaks(NoSurround) || got.EstimatedBytes == nil || *got.EstimatedBytes != want {
		t.Errorf("a package that lacks its 5.1: %q, estimated %v, want %d", found(got), got.EstimatedBytes, want)
	}
	notYet := title{ID: "a2", Kind: "episode", Path: strp("/library/arrivals/an-episode.mkv"), FileResolution: strp("1920x1080"),
		ItemDurMs: i64(2700000), Original: library.Original{State: library.OriginalPresent}}
	got = evaluate(notYet, records{}, rs)
	// the episode cap, stereo and, not knowing its source, a 5.1
	if want := int64((6e6 + 192e3 + 448e3) * 2700 / 8); got.EstimatedBytes == nil || *got.EstimatedBytes != want {
		t.Errorf("an episode not packaged: %q, estimated %v, want %d", found(got), got.EstimatedBytes, want)
	}
	gone := packagedTitle("movie")
	gone.Original = library.Original{State: library.OriginalRetired}
	if got := evaluate(gone, recordsOf(t, over, src), rs); !got.breaks(OverBitrateCap) || got.EstimatedBytes != nil {
		t.Errorf("an original deleted after packaging: %q, estimated %v, want the finding and no estimate", found(got), got.EstimatedBytes)
	}
}

func i64(n int64) *int64 { return &n }

// h264Found is what an H.264 package is found to be where every video is to
// be HEVC.
const h264Found = "The video is H.264, and the transcoder makes every video HEVC. [H.264 (avc1.640028) / HEVC]"

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
