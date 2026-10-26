package compliance

import (
	"encoding/json"
	"strconv"
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
		"ladder": "source:hevc", "extraLadder": "source:hevc", "dolbyVision": {"profile5": "reshaped", "profile7": "refused"}}`
	packagerReport = `{"surroundAudio": "eac3", "surroundLayout": "5.1", "surroundBitrate": "448k", "stereoAudio": "aac",
		"stereoBitrate": "192k", "imageSubtitles": ["pgs", "vobsub"]}`
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
// found is what the check found of a title, a finding a line, its no-break
// spaces and hyphens written plain.
func found(r result) string {
	return strings.NewReplacer(nbsp, " ", nbhy, "-").Replace(foundRaw(r))
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
		if f.Detail != nil {
			s += " {" + *f.Detail + "}"
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
			"OVER_BITRATE_CAP The video runs at 9.4 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080p movie. [9.4 Mbit/s / 8 Mbit/s]"},
		{"a 2160 movie within", "movie", 3840, 2160, 13.9e6, ""},
		{"a 2160 movie over", "movie", 3840, 2160, 73e6,
			"OVER_BITRATE_CAP The video runs at 73 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie. [73 Mbit/s / 14 Mbit/s]"},
		{"a scope picture is 2160 by its width", "movie", 3840, 1600, 20e6,
			"OVER_BITRATE_CAP The video runs at 20 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie. [20 Mbit/s / 14 Mbit/s]"},
		{"a 2560 wide picture is 1080", "movie", 2560, 1440, 12e6,
			"OVER_BITRATE_CAP The video runs at 12 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080p movie. [12 Mbit/s / 8 Mbit/s]"},
		{"a 1080 episode within", "episode", 1920, 1080, 5.9e6, ""},
		{"a 1080 episode over", "episode", 1920, 1080, 7e6,
			"OVER_BITRATE_CAP The video runs at 7 Mbit/s, above the 6 Mbit/s the transcoder allows for a 1080p episode. [7 Mbit/s / 6 Mbit/s]"},
		{"a 2160 episode within", "episode", 3840, 2160, 7.9e6, ""},
		{"a 2160 episode over", "episode", 3840, 2160, 12.5e6,
			"OVER_BITRATE_CAP The video runs at 12.5 Mbit/s, above the 8 Mbit/s the transcoder allows for a 2160p episode. [12.5 Mbit/s / 8 Mbit/s]"},
		{"a copy just above the cap, within the tolerance", "movie", 3840, 2160, 14.3e6, ""},
		{"a copy above the tolerance", "movie", 3840, 2160, 14.5e6,
			"OVER_BITRATE_CAP The video runs at 14.5 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie. [14.5 Mbit/s / 14 Mbit/s]"},
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
		"OVER_BITRATE_CAP The video runs at 12 Mbit/s, above the 8 Mbit/s the transcoder allows for a 1080p movie. [12 Mbit/s / 8 Mbit/s]" {
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

// A movie's original over the file size cap breaks it only where its
// package copies its video as it is: of a package that doesn't say whether
// it did, nothing is found.
func TestTheSizeCapNeedsACopy(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	ti := packagedTitle("movie")
	big := int64(60e9)
	ti.VersionBytes = &big
	pkg := pkgJSON(1920, 1080, 6e6, "copy")
	if got := evaluate(ti, recordsOf(t, pkg, srcJSON(1920, 1080)), rs); !got.breaks(OverSizeCap) {
		t.Errorf("a copy of a 60 GB original: %q, want it over the size cap", found(got))
	}
	ti.Transcode = step{}
	unsaid := strings.Replace(pkg, `, "encoder": "copy"`, ``, 1)
	if got := evaluate(ti, recordsOf(t, unsaid, srcJSON(1920, 1080)), rs); got.breaks(OverSizeCap) {
		t.Errorf("a package that doesn't say it copied: %q, want no size cap", found(got))
	}
}

// What the transcoder makes again breaks no rule of its own: a CPU
// transcoder whose ladder leaves the original's codec to the default keeps a
// browser-friendly H.264 original as it is, whatever its bit rate or its
// size (decision.py's CPU rule), and an encode at the transcoder's
// override of the cap (NVENC_MAXRATE_*) is within what it allows; a 5.1
// track that is only a commentary gets no companion, so a package without
// one lacks no surround.
func TestWhatTheTranscoderMakesAgainBreaksNoRule(t *testing.T) {
	cpu := strings.NewReplacer(`"codec": "hevc_nvenc", "preset": "p5", "cq": 23`, `"codec": "libx265", "preset": "medium", "cq": null, "crf": 24`,
		`"ladder": "source:hevc"`, `"ladder": "source"`).Replace(transcoderReport)
	pkg := `{"sizeBytes": 20000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "avc1.640028", "width": 1920, "height": 1080, "bitrateBps": 20000000, "encoder": "copy"}],
		"audio": [{"channels": 2, "bitrateBps": 192000}]}, "essence": {"surround": false, "maxAudioChannels": 2}}`
	src := `{"file": {"sizeBytes": 18000000000}, "container": {"durationMs": 7200000},
		"streams": [{"index": 0, "type": "video", "codec": "h264", "profile": "High", "pixelFormat": "yuv420p", "width": 1920, "height": 1080},
		{"index": 1, "type": "audio", "codec": "aac", "channels": 2}], "essence": {"surround": false, "maxAudioChannels": 2}}`
	ti := packagedTitle("movie")
	big := int64(18e9)
	ti.VersionBytes = &big
	if got := evaluate(ti, recordsOf(t, pkg, src), stateOf(t, cpu, packagerReport)); len(got.Findings) != 0 {
		t.Errorf("an H.264 copy a CPU transcoder keeps: %q, want nothing", found(got))
	}
	if got := evaluate(ti, recordsOf(t, pkg, src), stateOf(t, transcoderReport, packagerReport)); !got.breaks(OverBitrateCap) ||
		!got.breaks(OverSizeCap) || !got.breaks(VideoNotHEVC) {
		t.Errorf("the same H.264 copy, an NVENC transcoder that makes HEVC: %q, want it over both caps and not HEVC", found(got))
	}

	override := stateOf(t, strings.Replace(transcoderReport, `"maxrate2160Mbps": null`, `"maxrate2160Mbps": 20`, 1), packagerReport)
	enc := packagedTitle("movie")
	enc.Transcode = step{Status: strp(processing.StatusDone),
		Details: strp("profile=nvenc-2160p src_codec=hevc res=3840x2160 maxrate=20Mbps rate=40.0Mbps(stream) cap=movie-2160:14Mbps over=bitrate")}
	if got := evaluate(enc, recordsOf(t, pkgJSON(3840, 2160, 16.5e6, "hevc_nvenc"), srcJSON(3840, 2160)), override); got.breaks(OverBitrateCap) ||
		got.Measures.BitrateCap == nil || *got.Measures.BitrateCap != 20e6 {
		t.Errorf("an encode at the 20 Mbit/s override: %q, cap %v; want none over it, the limit 20 Mbit/s", found(got),
			got.Measures.BitrateCap)
	}
	if got := evaluate(packagedTitle("movie"), recordsOf(t, pkgJSON(3840, 2160, 25e6, "copy"), srcJSON(3840, 2160)), override); !got.breaks(OverBitrateCap) {
		t.Errorf("a copy at 25 Mbit/s, over the override: %q", found(got))
	}

	commentaryOnly := `{"container": {"durationMs": 7200000},
		"streams": [{"index": 0, "type": "video", "codec": "hevc", "width": 1920, "height": 1080},
		{"index": 1, "type": "audio", "codec": "aac", "channels": 2, "language": "en"},
		{"index": 2, "type": "audio", "codec": "ac3", "channels": 6, "language": "en", "title": "Director's commentary"}],
		"essence": {"surround": true, "maxAudioChannels": 6}}`
	stereo := `{"sizeBytes": 4000000000, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "hvc1.1.6.L120.B0", "width": 1920, "height": 1080, "bitrateBps": 4000000, "encoder": "copy"}],
		"audio": [{"channels": 2, "bitrateBps": 192000}, {"channels": 2, "bitrateBps": 192000}]},
		"essence": {"surround": false, "maxAudioChannels": 2}}`
	if got := evaluate(packagedTitle("movie"), recordsOf(t, stereo, commentaryOnly), stateOf(t, transcoderReport, packagerReport)); got.breaks(NoSurround) {
		t.Errorf("a 5.1 that is only a commentary: %q, want no surround missing", found(got))
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
	if want := "OVER_BITRATE_CAP The video runs at 30 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie. " +
		"[30 Mbit/s / 14 Mbit/s]"; got != want {
		t.Errorf("a copy measured by its source's stream:\n got  %q\n want %q", got, want)
	}
	got = found(evaluate(packagedTitle("movie"), recordsOf(t, pkg, `{"streams": [], "essence": {}}`), rs))
	// 31.5 GB over two hours is 35 Mbit/s, less 0.832 of audio
	if want := "OVER_BITRATE_CAP The video runs at about 34.2 Mbit/s, above the 14 Mbit/s the transcoder allows for a 2160p movie. " +
		"[about 34.2 Mbit/s / 14 Mbit/s]"; got != want {
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
			"OVER_SIZE_CAP The original is larger than the 16.1 GB the transcoder allows for a movie, and the package copied " +
				"its video. [- / 16.1 GB]"},
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
			"NO_SURROUND The package has 2 audio channels, and the original has 6. The 5.1 E-AC-3 track the packager makes is missing. " +
				"[2 channels / 5.1 E-AC-3]"},
		{"a 7.1 source, a stereo package", 8, 2,
			"NO_SURROUND The package has 2 audio channels, and the original has 8. The 5.1 E-AC-3 track the packager makes is missing. " +
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
// does not carry (DVB) is not looked for, nor VobSub of an original that is
// no Matroska one, which the packager can't extract; and a package subtitle
// made from a source stream counts for that stream's language whatever its
// own tag.
func TestImageSubtitles(t *testing.T) {
	src := `{"container": {"format": "matroska,webm"}, "streams": [{"index": 0, "type": "video", "width": 1920, "height": 1080},
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
			"IMAGE_SUBTITLES_MISSING The original’s PGS subtitles in German, and its VobSub subtitles in French, are missing from " +
				"the package. The packager keeps PGS and VobSub subtitles. [German, French / PGS, VobSub]"},
		{"every image subtitle carried", `{"format": "pgs", "language": "eng"}, {"format": "pgs", "language": "ger"},
			{"format": "vobsub", "language": "fre"}`, ""},
		{"a subtitle by its source stream", `{"format": "pgs", "language": "eng"}, {"format": "pgs", "language": "und", "sourceStreamIndex": 3},
			{"format": "vobsub", "language": "fre"}`, ""},
		{"a text track is no image subtitle", `{"format": "webvtt", "language": "eng"}, {"format": "webvtt", "language": "ger"},
			{"format": "webvtt", "language": "fre"}`, "IMAGE_SUBTITLES_MISSING The original’s PGS subtitles in German and English, and " +
			"its VobSub subtitles in French, are missing from the package. The packager keeps PGS and VobSub subtitles. " +
			"[German, English, French / PGS, VobSub]"},
	} {
		if got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(tc.subs), src), rs)); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	mp4 := strings.Replace(src, `"matroska,webm"`, `"mov,mp4,m4a,3gp,3g2,mj2"`, 1)
	got := found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(`{"format": "pgs", "language": "eng"}, {"format": "pgs", "language": "ger"}`),
		mp4), rs))
	if got != "" {
		t.Errorf("VobSub of an MP4 original: %q, want nothing", got)
	}
	pgsOnly := stateOf(t, transcoderReport, strings.Replace(packagerReport, `["pgs", "vobsub"]`, `["pgs"]`, 1))
	got = found(evaluate(packagedTitle("movie"), recordsOf(t, pkg(`{"format": "pgs", "language": "eng"}`), src), pgsOnly))
	if want := "IMAGE_SUBTITLES_MISSING The original’s PGS subtitles in German are missing from the package. The packager keeps " +
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
	rs.retries = true
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
	skippedWhy := base()
	skippedWhy.Package = step{Status: strp(processing.StatusSkipped), Error: strp("handoff v0 has no keyframe")}
	refused := base()
	refused.Transcode = step{Status: strp(processing.StatusFailed), Error: strp("Dolby Vision profile 8.0 needs a tone-mapping encode; kept the original")}
	reshaped := base()
	reshaped.Transcode = step{Status: strp(processing.StatusFailed),
		Error: strp("Dolby Vision profile 5: 3 of the first 10 decoded frames carry the RPU that libplacebo reshapes them with; kept the original")}
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
		{"its transcode failed", failed, "PACKAGING_FAILED The transcoder failed. {ffmpeg exited 1}"},
		{"its transcode refused, a profile it needs to tone-map", refused,
			"PACKAGING_FAILED The transcoder left it as it is: Dolby Vision profile 8.0 needs a tone-mapping encode."},
		{"its transcode refused, for frames without their metadata", reshaped, "PACKAGING_FAILED The transcoder left it as it is. " +
			"{Dolby Vision profile 5: 3 of the first 10 decoded frames carry the RPU that libplacebo reshapes them with}"},
		{"its package failed, retried", retried, "PACKAGING_FAILED The packager failed, and tries again by itself. {shaka exited 2}"},
		{"its package skipped", skipped, "PACKAGING_FAILED The packager skipped it. {no video stream}"},
		{"its package skipped, for a reason the words don’t know", skippedWhy, "PACKAGING_FAILED The packager skipped it. " +
			"{handoff v0 has no keyframe}"},
		{"a disc image", disc, "PACKAGING_FAILED " + discImageText},
		{"no original and no package", retired, ""},
	} {
		if got := found(evaluate(tc.t, records{}, rs)); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
	// a retry is said only while one is really scheduled: not with no sweep
	// to send it
	rs.retries = false
	if got := found(evaluate(retried, records{}, rs)); got != "PACKAGING_FAILED The packager failed. {shaka exited 2}" {
		t.Errorf("a retry scheduled and no sweep: %q", got)
	}
}

// What processing a title again can't help is told by its original's record
// or its failed step alone, and only when it is certain: an original without
// video, a Dolby Vision profile the transcoder leaves as it is (7, which it
// never plans; 5 only while it reports it can't reshape it; one of base
// layer 0, as its failed step says). A title so blocked has no estimate;
// any other is offered.
func TestWhatTheTranscoderWouldRefuseAgain(t *testing.T) {
	reshapes := stateOf(t, transcoderReport, packagerReport)
	refuses := stateOf(t, strings.Replace(transcoderReport, `"profile5": "reshaped"`, `"profile5": "refused"`, 1), packagerReport)
	silent := stateOf(t, strings.Replace(transcoderReport, `, "dolbyVision": {"profile5": "reshaped", "profile7": "refused"}`, ``, 1),
		packagerReport)
	dv := func(profile string) string {
		return `{"streams": [{"index": 0, "type": "video", "codec": "hevc", "width": 3840, "height": 2160,
			"hdr": {"format": "dolby-vision", "dolbyVision": {"profile": ` + profile + `, "rpuPresent": true}}},
			{"index": 1, "type": "audio", "codec": "eac3", "channels": 6}], "essence": {"dolbyVision": true}}`
	}
	coverArtOnly := `{"streams": [{"index": 0, "type": "video", "codec": "mjpeg", "width": 600, "height": 600,
		"dispositions": {"attachedPic": true}}, {"index": 1, "type": "audio", "codec": "flac", "channels": 2}], "essence": {}}`
	pkg := pkgJSON(3840, 2160, 30e6, "copy")
	stepped := func(stepName, state, reason string) title {
		p := "/library/arrivals/a-film.mkv"
		ti := title{ID: "a1", Kind: "movie", Path: &p, Original: library.Original{State: library.OriginalPresent},
			FileResolution: strp("3840x2160"), ItemDurMs: i64(7_200_000)}
		s := step{Status: strp(state), Error: strp(reason)}
		if stepName == "transcode" {
			ti.Transcode = s
		} else {
			ti.Transcode, ti.Package = status(processing.StatusDone), s
		}
		return ti
	}
	const noVulkan = "Dolby Vision profile 5 is reshaped to HDR10 on a Vulkan device, and this host has none (no Vulkan loader); " +
		"kept the original"
	for _, tc := range []struct {
		name string
		t    title
		r    records
		rs   rulesState
		want string
	}{
		{"an original of profile 7", packagedTitle("movie"), recordsOf(t, pkg, dv("7")), reshapes, "dolbyvision:7"},
		{"of profile 7, the transcoder saying nothing of it", packagedTitle("movie"), recordsOf(t, pkg, dv("7")), silent, "dolbyvision:7"},
		{"of profile 5, the transcoder reshaping it", packagedTitle("movie"), recordsOf(t, pkg, dv("5")), reshapes, ""},
		{"of profile 5, the transcoder refusing it", packagedTitle("movie"), recordsOf(t, pkg, dv("5")), refuses, "dolbyvision:5"},
		{"of profile 5, the transcoder saying nothing of it", packagedTitle("movie"), recordsOf(t, pkg, dv("5")), silent, ""},
		{"of profile 8", packagedTitle("movie"), recordsOf(t, pkg, dv("8")), refuses, ""},
		{"an original with no video but its cover art", packagedTitle("movie"), recordsOf(t, pkg, coverArtOnly), reshapes, "novideo"},
		{"an original whose streams were never probed", packagedTitle("movie"), recordsOf(t, pkg, `{"streams": [], "essence": {}}`),
			reshapes, ""},
		{"a transcode refused for profile 7", stepped("transcode", processing.StatusFailed,
			"Dolby Vision profile 7 needs a tone-mapping encode; kept the original"), records{}, reshapes, "dolbyvision:7"},
		{"a transcode refused for a base layer of compatibility 0", stepped("transcode", processing.StatusFailed,
			"Dolby Vision profile 8.0 needs a tone-mapping encode; kept the original"), records{}, reshapes, "dolbyvision:8.0"},
		{"a transcode refused without a Vulkan device, the transcoder reshaping now", stepped("transcode",
			processing.StatusFailed, noVulkan), records{}, reshapes, ""},
		{"a transcode refused without a Vulkan device, the transcoder refusing still", stepped("transcode",
			processing.StatusFailed, noVulkan), records{}, refuses, "dolbyvision:5"},
		{"a title never packaged whose original's record holds no video but its cover art", stepped("package",
			processing.StatusSkipped, "skipped by an admin"), recordsOf(t, "", coverArtOnly), reshapes, "novideo"},
		{"a transcode that failed otherwise", stepped("transcode", processing.StatusFailed, "ffmpeg exited 1"), records{},
			reshapes, ""},
	} {
		res := evaluate(tc.t, tc.r, tc.rs)
		if res.Blocked != tc.want || len(res.Findings) == 0 {
			t.Errorf("%s: blocked %q, want %q (%s)", tc.name, res.Blocked, tc.want, found(res))
		}
		if (res.EstimatedBytes == nil) != (tc.want != "") {
			t.Errorf("%s: an estimate of %v, blocked %q", tc.name, res.EstimatedBytes, res.Blocked)
		}
	}
}

// What the report's columns show of a title is measured as the findings
// are: the package's video (else the original's) for its size, codec and bit
// rate, the bit rate estimated when read off a size; the caps of its kind and
// bucket, a movie's file size alone; its duration; and the audio of its
// package and of its original, as their records list it.
func TestTheColumnsAreWhatTheCheckMeasured(t *testing.T) {
	rs := stateOf(t, transcoderReport, packagerReport)
	remuxPkg := `{"sizeBytes": 58800000000, "durationMs": 7980000, "renditions": {"video": [{"codec": "hvc1.2.4.L153.B0",
		"width": 3840, "height": 2160, "bitrateBps": 73000000, "encoder": "copy"}],
		"audio": [{"codec": "mp4a.40.2", "channels": 2, "bitrateBps": 192000}, {"codec": "mp4a.40.2", "channels": 2, "bitrateBps": 192000}],
		"audioSurround": [{"codec": "ec-3", "channels": 6, "bitrateBps": 448000}]}, "essence": {"surround": true, "maxAudioChannels": 6}}`
	remuxSrc := `{"streams": [{"index": 0, "type": "video", "codec": "hevc", "width": 3840, "height": 2160},
		{"index": 1, "type": "audio", "codec": "truehd", "channels": 8, "profile": "Dolby TrueHD + Dolby Atmos"},
		{"index": 2, "type": "audio", "codec": "dts", "channels": 6, "profile": "DTS-HD MA"},
		{"index": 3, "type": "audio", "codec": "ac3", "channels": 2}], "essence": {"surround": true, "maxAudioChannels": 8}}`
	str := func(p *string) string {
		if p == nil {
			return "-"
		}
		return strings.NewReplacer(nbsp, " ", nbhy, "-").Replace(*p)
	}
	num := func(p *float64) string {
		if p == nil {
			return "-"
		}
		return strconv.FormatFloat(*p, 'f', -1, 64)
	}
	cols := func(m measures) string {
		size := "-"
		if m.SizeCap != nil {
			size = strconv.FormatInt(*m.SizeCap, 10)
		}
		return strings.Join([]string{str(m.Resolution), str(m.VideoCodec), num(m.VideoBitrate), strconv.FormatBool(m.BitrateEstimated),
			num(m.BitrateCap), size, num(m.DurationSeconds), str(m.PackageAudio), str(m.OriginalAudio)}, " | ")
	}
	unpackaged := func() title {
		p := "/library/arrivals/a-film.mkv"
		return title{ID: "a1", Kind: "movie", Path: &p, FileBytes: i64(4_500_000_000), FileResolution: strp("1920x1080"),
			ItemDurMs: i64(6_000_000), Original: library.Original{State: library.OriginalPresent}}
	}
	sized := `{"sizeBytes": 31500000000, "durationMs": 7200000, "renditions": {"video": [{"codec": "hvc1.2.4.L153.B0",
		"width": 3840, "height": 2160, "encoder": "copy"}], "audio": [{"channels": 2, "bitrateBps": 192000}]}, "essence": {}}`
	for _, tc := range []struct {
		name string
		t    title
		r    records
		rs   rulesState
		want string
	}{
		{"a 4K remux", packagedTitle("movie"), recordsOf(t, remuxPkg, remuxSrc), rs,
			"3840×2160 | HEVC | 73000000 | false | 14000000 | 16106127360 | 7980 | 5.1 E-AC-3 + 2.0 AAC ×2 | " +
				"7.1 TrueHD Atmos + 5.1 DTS-HD MA + 2.0 AC-3"},
		{"a package measured by its size", packagedTitle("movie"), recordsOf(t, sized, `{"streams": [], "essence": {}}`), rs,
			"3840×2160 | HEVC | 34808000 | true | 14000000 | 16106127360 | 7200 | 2.0 | -"},
		{"an episode, which has no file size cap", packagedTitle("episode"), recordsOf(t, pkgJSON(1920, 1080, 7e6, "copy"),
			srcJSON(1920, 1080)), rs, "1920×1080 | HEVC | 7000000 | false | 6000000 | - | 7200 | 2.0 | 2.0 AC-3"},
		{"a film never packaged, measured by its file", unpackaged(), records{}, rs,
			"1920×1080 | - | 6000000 | true | 8000000 | 16106127360 | 6000 | - | -"},
		{"a transcoder that reports no caps", packagedTitle("movie"), recordsOf(t, remuxPkg, remuxSrc),
			stateOf(t, "", packagerReport), "3840×2160 | HEVC | 73000000 | false | - | - | 7980 | 5.1 E-AC-3 + 2.0 AAC ×2 | " +
				"7.1 TrueHD Atmos + 5.1 DTS-HD MA + 2.0 AC-3"},
	} {
		if got := cols(evaluate(tc.t, tc.r, tc.rs).Measures); got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, got, tc.want)
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

// The rules the check holds no title against under the rules as reported
// are those notEvaluated names whole: a worker's that never reported, one
// whose report gives no limit it is checked by or switches every one off
// (all four bit rate caps, the file size cap, where 2160p begins), the video
// format where the ladder makes no video HEVC (libx265 with its default
// ladder), what the packager makes nothing of, and what the older layout
// keeps no records for. A rule some of whose limits are set is checked; and
// whatever is off, notEvaluated says why.
func TestTheRulesCheckedAreThoseReported(t *testing.T) {
	libx265 := strings.NewReplacer(`"codec": "hevc_nvenc", "preset": "p5", "cq": 23`,
		`"codec": "libx265", "preset": "medium", "cq": null, "crf": 24`, `"ladder": "source:hevc"`, `"ladder": "source"`).
		Replace(transcoderReport)
	for _, tc := range []struct {
		name, transcoder, packager string
		legacy                     bool
		off                        string
	}{
		{"both workers reported", transcoderReport, packagerReport, false, ""},
		{"neither worker reported", "", "", false, "OVER_BITRATE_CAP OVER_SIZE_CAP NO_SURROUND IMAGE_SUBTITLES_MISSING VIDEO_NOT_HEVC"},
		{"the transcoder alone not", "", packagerReport, false, "OVER_BITRATE_CAP OVER_SIZE_CAP VIDEO_NOT_HEVC"},
		{"the packager alone not", transcoderReport, "", false, "NO_SURROUND IMAGE_SUBTITLES_MISSING"},
		{"libx265 with its default ladder", libx265, packagerReport, false, "VIDEO_NOT_HEVC"},
		{"libx265 with a ladder that names HEVC", strings.Replace(libx265, `"ladder": "source"`, `"ladder": "source:hevc"`, 1),
			packagerReport, false, ""},
		{"a transcoder that reports no renditions", strings.Replace(transcoderReport, `"ladder": "source:hevc", `, ``, 1),
			packagerReport, false, "VIDEO_NOT_HEVC"},
		{"every bit rate cap off", strings.NewReplacer(`"movie1080Mbps": 8`, `"movie1080Mbps": 0`, `"movie2160Mbps": 14`,
			`"movie2160Mbps": 0`, `"episode1080Mbps": 6`, `"episode1080Mbps": 0`, `"episode2160Mbps": 8`, `"episode2160Mbps": 0`).
			Replace(transcoderReport), packagerReport, false, "OVER_BITRATE_CAP"},
		{"one bit rate cap off, one not reported", strings.NewReplacer(`"movie1080Mbps": 8`, `"movie1080Mbps": 0`,
			`"movie2160Mbps": 14, `, ``).Replace(transcoderReport), packagerReport, false, ""},
		{"no 2160p bucket", strings.Replace(transcoderReport, `"bucket2160": {"minHeight": 2000, "minWidth": 3200},`, ``, 1),
			packagerReport, false, "OVER_BITRATE_CAP"},
		{"any movie file size", strings.Replace(transcoderReport, `"movieMaxGiB": 15`, `"movieMaxGiB": 0`, 1), packagerReport, false,
			"OVER_SIZE_CAP"},
		{"no surround track, no image subtitles kept", transcoderReport, strings.NewReplacer(`"surroundAudio": "eac3"`,
			`"surroundAudio": "off"`, `["pgs", "vobsub"]`, `[]`).Replace(packagerReport), false, "NO_SURROUND IMAGE_SUBTITLES_MISSING"},
		{"the older layout", transcoderReport, packagerReport, true, "OVER_BITRATE_CAP NO_SURROUND IMAGE_SUBTITLES_MISSING"},
	} {
		rs := stateOf(t, tc.transcoder, tc.packager)
		rs.v2 = !tc.legacy
		said, off := coverage(rs)
		var got []string
		for _, r := range Rules {
			if off[r] {
				got = append(got, r)
			}
		}
		if strings.Join(got, " ") != tc.off {
			t.Errorf("%s: off %q, want %q", tc.name, strings.Join(got, " "), tc.off)
		}
		if len(off) > 0 && len(said) == 0 || strings.Join(notEvaluated(rs, 0), "\n") != strings.Join(said, "\n") {
			t.Errorf("%s: notEvaluated %q, coverage %q", tc.name, notEvaluated(rs, 0), said)
		}
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

// The transcoder's report of the tests with its table of rung maxrates, as
// the transcoder reports it (rungMaxrateMbps).
var transcoderWithTable = strings.Replace(transcoderReport, `"ladder": "source:hevc"`, `"ladder": "source:hevc",
	"rungMaxrateMbps": {"360": {"h264": 0.8, "hevc": 0.6}, "480": {"h264": 1.5, "hevc": 1.2}, "540": {"h264": 2.0, "hevc": 1.6},
	"720": {"h264": 3.0, "hevc": 2.5}, "1080": {"h264": 6.0, "hevc": 8.0}, "1440": {"h264": 9.0, "hevc": 8.0},
	"2160": {"h264": 16.0, "hevc": 14.0}}`, 1)

// estimateDur is the duration of the estimate tests' titles, ms.
const estimateDur = 7200000.0

// at is bytes of bps over the estimate tests' duration.
func at(bps float64) int64 { return int64(bps * estimateDur / 8000) }

// extras is what a package of the estimate tests holds beside its
// renditions: 40 MB.
const extras = 40_000_000

// pkgOf is a package record of a video of codec at width x height, copied
// or encoded (encoder), its renditions measured at rate bit/s, stereo
// renditions at 192k and companions of their rates (each of a language of
// languages), and extras beside them: its size is what they make.
func pkgOf(codec string, width, height int, rate float64, encoder string, stereo int, companions ...float64) string {
	media := rate + float64(stereo)*192e3
	var audio, surround []string
	for i := 0; i < stereo; i++ {
		audio = append(audio, `{"codec": "mp4a.40.2", "channels": 2, "bitrateBps": 192000}`)
	}
	for i, c := range companions {
		media += c
		surround = append(surround, `{"codec": "ec-3", "channels": 6, "bitrateBps": `+ftoa(c)+`, "language": "`+
			[]string{"eng", "ger", "fre"}[i%3]+`"}`)
	}
	size := at(media) + extras
	return `{"sizeBytes": ` + strconv.FormatInt(size, 10) + `, "durationMs": 7200000,
		"renditions": {"video": [{"codec": "` + codec + `", "width": ` + itoa(width) + `, "height": ` + itoa(height) +
		`, "bitrateBps": ` + ftoa(rate) + `, "encoder": "` + encoder + `"}], "audio": [` + strings.Join(audio, ", ") + `],
		"audioSurround": [` + strings.Join(surround, ", ") + `]},
		"essence": {"surround": ` + strconv.FormatBool(len(companions) > 0) + `, "maxAudioChannels": ` +
		map[bool]string{true: "6", false: "2"}[len(companions) > 0] + `}}`
}

// srcOf is an original's record: its video stream (more of its facts in
// video, "" none) and its audio streams.
func srcOf(video string, audio ...string) string {
	return `{"container": {"format": "matroska,webm", "durationMs": 7200000}, "streams": [` +
		strings.Join(append([]string{video}, audio...), ", ") + `], "essence": {"surround": true, "maxAudioChannels": 8}}`
}

// hevcVideo is an HEVC video stream of the original at width x height.
func hevcVideo(width, height int, more string) string {
	return `{"index": 0, "type": "video", "codec": "hevc", "profile": "Main 10", "pixelFormat": "yuv420p10le", "width": ` +
		itoa(width) + `, "height": ` + itoa(height) + more + `}`
}

// An estimate of the package processing a title again makes is about what
// today's workers make of its original, for its duration: the video of the
// ladder's rungs (an original that breaks a cap encoded at its maxrate, one
// that breaks none copied at its own bit rate, an earlier encode at today's
// maxrate about the same again), the packager's audio, and what the package
// holds beside its renditions. It is an estimate, not a bound; a title
// processing again can't help has none.
func TestTheEstimate(t *testing.T) {
	rs := stateOf(t, transcoderWithTable, packagerReport)
	english := `{"index": 1, "type": "audio", "codec": "eac3", "channels": 6, "language": "en", "languageRaw": "eng", "bitrate": 640000}`
	englishAC3 := `{"index": 1, "type": "audio", "codec": "ac3", "channels": 6, "language": "en", "languageRaw": "eng"}`
	encoded := func(details string) title {
		ti := packagedTitle("movie")
		ti.Transcode = step{Status: strp(processing.StatusDone), Details: strp(details)}
		return ti
	}
	h264 := `{"index": 0, "type": "video", "codec": "h264", "profile": "High", "pixelFormat": "yuv420p", "width": 1920, "height": 1080}`
	for _, tc := range []struct {
		name     string
		ti       title
		pkg, src string
		rs       rulesState
		want     int64
	}{
		{"a 2160 remux over its cap: the video encoded at the cap, the English 5.1 E-AC-3 copied at its own 640 kbit/s",
			packagedTitle("movie"), pkgOf("hvc1.2.4.L153.B0", 3840, 2160, 65e6, "copy", 1, 640e3),
			srcOf(hevcVideo(3840, 2160, ""), english), rs, at(14e6+192e3+640e3) + extras},
		{"the same, the ladder of three rungs counted, each at the transcoder's table",
			packagedTitle("movie"), pkgOf("hvc1.2.4.L153.B0", 3840, 2160, 65e6, "copy", 1, 640e3),
			srcOf(hevcVideo(3840, 2160, ""), english),
			stateOf(t, strings.Replace(transcoderWithTable, `"ladder": "source:hevc"`, `"ladder": "source:hevc,720p:h264,480p:h264"`, 1),
				packagerReport), at(14e6+3e6+1.5e6+192e3+640e3) + extras},
		{"a 1080p package within its caps that lacks its 5.1: the video copied again, the AC-3 5.1 encoded",
			packagedTitle("movie"), pkgOf("hvc1.1.6.L120.B0", 1920, 1080, 4e6, "copy", 1),
			srcOf(hevcVideo(1920, 1080, ""), englishAC3), rs, at(4e6+192e3+448e3) + extras},
		{"an encode at the maxrate an encode runs at today: about the same again", encoded("profile=nvenc-1080p maxrate=8Mbps"),
			pkgOf("hvc1.1.6.L120.B0", 1920, 1080, 2.8e6, "hevc_nvenc", 1), srcOf(h264, englishAC3), rs,
			at(2.8e6+192e3+448e3) + extras},
		{"an encode at another maxrate: today's maxrate", encoded("profile=nvenc-1080p maxrate=10Mbps"),
			pkgOf("hvc1.1.6.L120.B0", 1920, 1080, 2.8e6, "hevc_nvenc", 1), srcOf(h264, englishAC3), rs,
			at(8e6+192e3+448e3) + extras},
		{"an HEVC original whose record names no profile: the profile of the copy's codec, Main, copied again",
			packagedTitle("movie"), pkgOf("hvc1.1.6.L120.B0", 1920, 1080, 4e6, "copy", 1),
			srcOf(`{"index": 0, "type": "video", "codec": "hevc", "width": 1920, "height": 1080}`, englishAC3), rs,
			at(4e6+192e3+448e3) + extras},
		{"the same of a range extension's codec: nothing copied blind, an encode at the cap",
			packagedTitle("movie"), pkgOf("hvc1.4.10.L120.B0", 1920, 1080, 4e6, "copy", 1),
			srcOf(`{"index": 0, "type": "video", "codec": "hevc", "width": 1920, "height": 1080}`, englishAC3), rs,
			at(8e6+192e3+448e3) + extras},
	} {
		got := evaluate(tc.ti, recordsOf(t, tc.pkg, tc.src), tc.rs)
		if got.EstimatedBytes == nil || *got.EstimatedBytes != tc.want {
			t.Errorf("%s: %q, estimated %v, want %d", tc.name, found(got), deref64(got.EstimatedBytes), tc.want)
		}
	}

	// a title never packaged: of its file alone, the cap of its kind and
	// bucket and, not knowing its original's tracks, one stereo rendition and
	// a companion; of its original's record, an HEVC original within its caps
	// copied at its own bit rate, and its tracks
	notYet := title{ID: "a2", Kind: "episode", Path: strp("/library/arrivals/an-episode.mkv"), FileResolution: strp("1920x1080"),
		ItemDurMs: i64(2700000), Original: library.Original{State: library.OriginalPresent}}
	if got, want := evaluate(notYet, records{}, rs), int64((6e6+192e3+448e3)*2700/8); got.EstimatedBytes == nil ||
		*got.EstimatedBytes != want {
		t.Errorf("an episode never packaged, its file alone: %q, estimated %v, want %d", found(got), deref64(got.EstimatedBytes), want)
	}
	film := notYet
	film.Kind, film.ItemDurMs = "movie", i64(int64(estimateDur))
	german := `{"index": 2, "type": "audio", "codec": "ac3", "channels": 6, "language": "de", "languageRaw": "ger"}`
	stereoEnglish := `{"index": 1, "type": "audio", "codec": "aac", "channels": 2, "language": "en"}`
	if got, want := evaluate(film, recordsOf(t, "", srcOf(hevcVideo(1920, 1080, `, "bitrate": 5000000`), stereoEnglish, german)), rs),
		at(5e6+2*192e3+448e3); got.EstimatedBytes == nil || *got.EstimatedBytes != want {
		t.Errorf("a film never packaged, its original's record: %q, estimated %v, want %d", found(got), deref64(got.EstimatedBytes), want)
	}
	// its file's own duration before the reference database's runtime
	short := notYet
	short.FileDurationMs = i64(2400000)
	if got, want := evaluate(short, records{}, rs), int64((6e6+192e3+448e3)*2400/8); got.EstimatedBytes == nil ||
		*got.EstimatedBytes != want {
		t.Errorf("an episode whose file runs 40 minutes: estimated %v, want %d", deref64(got.EstimatedBytes), want)
	}
	// a ladder of rungs the transcoder reports no table of tells nothing
	noTable := stateOf(t, strings.Replace(transcoderReport, `"ladder": "source:hevc"`, `"ladder": "source:hevc,720p"`, 1), packagerReport)
	if got := evaluate(packagedTitle("movie"), recordsOf(t, pkgOf("hvc1.2.4.L153.B0", 3840, 2160, 65e6, "copy", 1),
		srcOf(hevcVideo(3840, 2160, ""), english)), noTable); !got.breaks(OverBitrateCap) || got.EstimatedBytes != nil {
		t.Errorf("a ladder without the table: %q, estimated %v, want none", found(got), deref64(got.EstimatedBytes))
	}
	gone := packagedTitle("movie")
	gone.Original = library.Original{State: library.OriginalRetired}
	if got := evaluate(gone, recordsOf(t, pkgOf("hvc1.2.4.L153.B0", 3840, 2160, 65e6, "copy", 1), srcOf(hevcVideo(3840, 2160, ""))),
		rs); !got.breaks(OverBitrateCap) || got.EstimatedBytes != nil {
		t.Errorf("an original deleted after packaging: %q, estimated %v, want the finding and no estimate", found(got),
			deref64(got.EstimatedBytes))
	}
}

func i64(n int64) *int64 { return &n }

func deref64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// The estimate counts the audio the packager makes of the original: a stereo
// rendition of each of its tracks, whatever its language, and a 5.1
// companion of the first track of more than two channels of each language
// that is no commentary, the track itself where it is a 5.1 of the
// companion's codec, at its own bit rate (its record's, else its
// companion's in the package). The count comes off the original's record,
// its tracks in the languages an admin set for them; else off the
// package's record; else one stereo rendition and, as the original had
// surround, a companion (TestTheEstimate).
func TestTheEstimateCountsTheAudioThePackagerMakes(t *testing.T) {
	rs := stateOf(t, transcoderWithTable, packagerReport)
	english := `{"index": 1, "type": "audio", "codec": "eac3", "channels": 6, "language": "en", "languageRaw": "eng", "bitrate": 640000}`
	german := `{"index": 2, "type": "audio", "codec": "dts", "profile": "DTS-HD MA", "channels": 6, "language": "de", "languageRaw": "ger"}`
	french := `{"index": 3, "type": "audio", "codec": "aac", "channels": 2, "language": "fr", "languageRaw": "fre"}`
	untagged := func(index int, codec string, channels int) string {
		return `{"index": ` + itoa(index) + `, "type": "audio", "codec": "` + codec + `", "channels": ` + itoa(channels) + `}`
	}
	// a 2160 remux over its cap: the video encoded at 14 Mbit/s
	remux := func(stereo int, companions ...float64) string {
		return pkgOf("hvc1.2.4.L153.B0", 3840, 2160, 65e6, "copy", stereo, companions...)
	}
	for _, tc := range []struct {
		name      string
		languages map[int]string
		pkg, src  string
		rs        rulesState
		want      int64
	}{
		{"three languages: 3 stereo renditions, the English 5.1 E-AC-3 copied at its 640 kbit/s and the German DTS encoded",
			nil, remux(1), srcOf(hevcVideo(3840, 2160, ""), english, german, french), rs,
			at(14e6+3*192e3+640e3+448e3) + extras},
		{"the same, the packager making no 5.1", nil, remux(1), srcOf(hevcVideo(3840, 2160, ""), english, german, french),
			stateOf(t, transcoderWithTable, strings.Replace(packagerReport, `"surroundAudio": "eac3"`, `"surroundAudio": "off"`, 1)),
			at(14e6+3*192e3) + extras},
		{"an E-AC-3 5.1 whose record names no bit rate: its companion's in the package", nil, remux(1, 768e3),
			srcOf(hevcVideo(3840, 2160, ""), strings.Replace(english, `, "bitrate": 640000`, ``, 1)), rs,
			at(14e6+192e3+768e3) + extras},
		{"a second English 5.1 and two commentaries: a stereo rendition each, one companion, of the first English " +
			"track, encoded", nil, remux(1), srcOf(hevcVideo(3840, 2160, ""),
			`{"index": 1, "type": "audio", "codec": "truehd", "channels": 8, "language": "en", "languageRaw": "eng", "bitrate": 4500000}`,
			`{"index": 2, "type": "audio", "codec": "ac3", "channels": 6, "language": "en", "languageRaw": "eng"}`,
			`{"index": 3, "type": "audio", "codec": "aac", "channels": 2, "language": "en", "title": "Commentary with the director"}`,
			`{"index": 4, "type": "audio", "codec": "eac3", "channels": 6, "language": "de", "dispositions": {"commentary": true}}`),
			rs, at(14e6+4*192e3+448e3) + extras},
		{"two 5.1 tracks of no language: one companion", nil, remux(1),
			srcOf(hevcVideo(3840, 2160, ""), untagged(1, "truehd", 8), untagged(2, "dts", 6)), rs, at(14e6+2*192e3+448e3) + extras},
		{"the same, an admin naming them English and German: two", map[int]string{0: "eng", 1: "deu"}, remux(1),
			srcOf(hevcVideo(3840, 2160, ""), untagged(1, "truehd", 8), untagged(2, "dts", 6)), rs,
			at(14e6+2*192e3+2*448e3) + extras},
		{"an original's record without its audio: the package's 3 stereo renditions and its companion, at its measured " +
			"640 kbit/s", nil, remux(3, 640e3), srcOf(hevcVideo(3840, 2160, "")), rs, at(14e6+3*192e3+640e3) + extras},
	} {
		ti := packagedTitle("movie")
		ti.AudioLanguages = tc.languages
		got := evaluate(ti, recordsOf(t, tc.pkg, tc.src), tc.rs)
		if got.EstimatedBytes == nil || *got.EstimatedBytes != tc.want {
			t.Errorf("%s: %q, estimated %v, want %d", tc.name, found(got), deref64(got.EstimatedBytes), tc.want)
		}
	}
}

// h264Found is what an H.264 package is found to be where every video is to
// be HEVC.
const h264Found = "The video is H.264, and the transcoder makes every video HEVC. [H.264 / HEVC] {avc1.640028}"

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}
