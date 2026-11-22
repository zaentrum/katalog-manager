package library

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// fullEssence is the essence of a stereo 1080p original with English and
// German audio and subtitles, every fact the essence holds said; a package
// that carries all of it has the same.
func fullEssence() map[string]any {
	return map[string]any{"maxAudioChannels": 2, "surround": false, "losslessAudio": false, "objectAudio": false,
		"maxVideoHeight": 1080, "videoBitDepth": 10, "hdr10Metadata": false, "dolbyVision": false, "stereo3d": false,
		"interlaced": false, "audioLanguages": []string{"de", "en"}, "subtitleLanguages": []string{"de", "en"},
		"subtitleTracks": 2, "imageSubtitles": false, "styledSubtitles": false, "fonts": false, "chapters": false,
		"commentaryTracks": 0, "commentarySubtitles": 0, "audioDescriptionTracks": 0, "sdhSubtitleLanguages": []string{},
		"forcedSubtitleLanguages": []string{}, "closedCaptions": false}
}

// rewriteSource writes the fixture's source record again with its essence
// essence and, when streams is not nil, the streams it lists, its copies of
// its sidecars and its checksums as they were.
func (f *retireFixture) rewriteSource(t *testing.T, essence map[string]any, streams []any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.sourceDir, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	rec["essence"] = essence
	if streams != nil {
		rec["streams"] = streams
	}
	copies := map[string]string{}
	entries, err := os.ReadDir(f.sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "source.json" || e.Name() == SumsFile {
			continue
		}
		c, err := os.ReadFile(filepath.Join(f.sourceDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		copies[e.Name()] = string(c)
	}
	librarytest.WriteSource(t, f.sourceDir, rec, copies)
}

// A retirement gives up what the master spec drops by design (ADR-0015,
// decision 2) and nothing else: surround and the channels above the
// package's, lossless and object-based audio, Dolby Vision where the package
// keeps the HDR (HDR10, or HLG of an HLG base), 3D. Any other difference
// keeps the original until the title is packaged again - interlacing among
// them, which the list doesn't name, an HDR picture packaged SDR, a track the
// package has none of -, and so do the chapters and the picture area
// (measured on the original) while the catalog doesn't know them, the retire
// job naming them kept. A track the package carries under a label it may take
// (und as en or zxx, en as zxx) is no difference; any other label is, and one
// whose track the package's doesn't name can't be told. A dry
// run of a list says so of each title, and the retire job
// (library.originals=delete-after-package) holds what it keeps, its package
// a stereo packager's.
func TestWhatARetirementGivesUpIsWhatTheSpecDrops(t *testing.T) {
	ctx := context.Background()
	with := func(base map[string]any, kv ...any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	// a package record of essence e, its top video rendition of range r
	record := func(e map[string]any, r string, subs ...map[string]any) map[string]any {
		listed := []any{}
		if subs == nil {
			subs = []map[string]any{{"language": "de"}, {"language": "en"}}
		}
		for i, sub := range subs {
			x := map[string]any{"id": "sub" + string(rune('0'+i)), "path": "subs/" + string(rune('0'+i)) + ".vtt",
				"format": "webvtt"}
			for k, v := range sub {
				x[k] = v
			}
			listed = append(listed, x)
		}
		return map[string]any{"essence": e, "subtitles": listed, "renditions": map[string]any{"video": []any{
			map[string]any{"id": "v0", "codec": "hvc1.2.4.L120", "height": 1080, "videoRange": r}}},
			"scan": verifiedProgressive()}
	}
	// the streams of a Dolby Vision original whose base layer is of the
	// compatibility id base, and its two subtitles
	dolbyStreams := func(base int) []any {
		return []any{map[string]any{"index": 0, "type": "video", "codec": "hevc", "captionSignature": false, "height": 1080,
			"fieldOrder": "progressive", "hdr": map[string]any{"format": "dolby-vision",
				"dolbyVision": map[string]any{"profile": 8, "blCompatibilityId": base}}},
			map[string]any{"index": 1, "type": "audio", "channels": 2},
			map[string]any{"index": 2, "type": "subtitle", "codec": "subrip", "language": "de"},
			map[string]any{"index": 3, "type": "subtitle", "codec": "subrip", "language": "en"}}
	}
	// the streams of an original with a picture, its audio tracks in the
	// languages langs, from index 1 on, and its two subtitles
	audioStreams := func(langs ...string) []any {
		out := []any{map[string]any{"index": 0, "type": "video", "codec": "h264", "height": 1080, "fieldOrder": "progressive"}}
		for i, l := range langs {
			out = append(out, map[string]any{"index": 1 + i, "type": "audio", "channels": 2, "language": l})
		}
		return append(out, map[string]any{"index": 1 + len(langs), "type": "subtitle", "codec": "subrip", "language": "de"},
			map[string]any{"index": 2 + len(langs), "type": "subtitle", "codec": "subrip", "language": "en"})
	}
	// the streams of an original whose picture is of the range format, its
	// English track and its two subtitles
	hdrStreams := func(format string) []any {
		out := audioStreams("en")
		out[0] = map[string]any{"index": 0, "type": "video", "codec": "hevc", "captionSignature": false, "height": 1080,
			"fieldOrder": "progressive", "hdr": map[string]any{"format": format}}
		return out
	}
	// the streams of an original whose picture's field order is order, its
	// English track and its two subtitles
	fieldStreams := func(order string) []any {
		out := audioStreams("en")
		out[0] = map[string]any{"index": 0, "type": "video", "codec": "mpeg2video", "height": 576, "fieldOrder": order}
		return out
	}
	// a window of a read of a ten-minute video: at at (a fraction of it,
	// which lies at at*600 s), 48 frames or frames, whether they showed
	// captions; one that decoded no frame says no times (null), as the
	// analyzer sends it
	win := func(at float64, frames int, captions bool) map[string]any {
		w := map[string]any{"at": at, "start": nil, "end": nil, "frames": frames, "captions": captions}
		if frames > 0 {
			w["start"], w["end"] = at*600, at*600+1.88
			w["interlacedFrames"], w["repeatFrames"] = 0, 0
		}
		return w
	}
	// a read of every planned window (the start, 10 to 90 %), captions in
	// those at captioned
	readOf := func(captioned ...float64) []map[string]any {
		var out []map[string]any
		for _, at := range []float64{0, 0.1, 0.3, 0.5, 0.7, 0.9} {
			out = append(out, win(at, 48, slices.Contains(captioned, at)))
		}
		return out
	}
	// a read of a video of the length duration (s) at its n planned windows,
	// none with captions
	readOfLength := func(duration float64, n int) []map[string]any {
		var out []map[string]any
		for _, w := range readOf()[:n] {
			w["start"], w["end"] = w["at"].(float64)*duration, w["at"].(float64)*duration+1.88
			out = append(out, w)
		}
		return out
	}
	// ws with its window at at so
	with1 := func(ws []map[string]any, w map[string]any) []map[string]any {
		out := slices.Clone(ws)
		for i, x := range out {
			if x["at"] == w["at"] {
				out[i] = w
				return out
			}
		}
		return append(out, w)
	}
	// what a read of the original's frames found of its closed captions,
	// window by window, of the length duration (0: none said)
	captionsOf := func(ws []map[string]any, duration float64) func(t *testing.T, f *listFixture) {
		return func(t *testing.T, f *listFixture) {
			has, frames := false, 0
			for _, w := range ws {
				has = has || w["captions"].(bool)
				frames += w["frames"].(int)
			}
			b, _ := json.Marshal(ws)
			var d *float64
			if duration > 0 {
				d = &duration
			}
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2, captions = $3,
				readreport = NULL,
				framesdecoded = $4, duration = $5, readspanstart = 0, readspanend = $5 WHERE source_id = $1`, rtSource, string(b), has,
				max(frames, 1), d)
		}
	}
	captionsRead := func(ws []map[string]any) func(t *testing.T, f *listFixture) { return captionsOf(ws, 600) }
	// what the copy rule says of a copy no record proves
	copyKept := CopyUnproven + " (encoder copy), and no record that proves the copy is read yet"
	captionsUnread := func(t *testing.T, f *listFixture) {
		storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_sourcecaptions WHERE source_id = $1`, rtSource)
	}
	// rec with its top video rendition the original's own stream, copied
	// (which the copy rule holds), or encoded
	copied := func(rec map[string]any) map[string]any {
		rec["renditions"].(map[string]any)["video"].([]any)[0].(map[string]any)["encoder"] = "copy"
		return rec
	}
	encoded := func(rec map[string]any) map[string]any {
		rec["renditions"].(map[string]any)["video"].([]any)[0].(map[string]any)["encoder"] = "hevc_nvenc"
		return rec
	}
	// rec with its scan as the packager copies it from the transcoder's
	// renditions contract
	scanned := func(rec map[string]any, done string, found ...string) map[string]any {
		scan := map[string]any{"done": done, "frameRate": "25"}
		if len(found) > 0 {
			scan["found"] = found[0]
			// a scan that found the picture progressive verified it whole
			scan["verified"] = readThrough()
		}
		rec["scan"] = scan
		return rec
	}
	// rec with no scan at all: no package of it examined the original's
	// fields
	unscanned := func(rec map[string]any) map[string]any {
		delete(rec, "scan")
		return rec
	}
	// rec with its audio renditions, each a language and the stream it says
	// it was made from (-1: none)
	withAudio := func(rec map[string]any, tracks ...any) map[string]any {
		var audio []any
		for i := 0; i+1 < len(tracks); i += 2 {
			a := map[string]any{"id": "a" + string(rune('0'+i/2)), "codec": "mp4a.40.2", "channels": 2, "language": tracks[i]}
			if n := tracks[i+1].(int); n >= 0 {
				a["sourceStreamIndex"] = n
			}
			audio = append(audio, a)
		}
		rec["renditions"].(map[string]any)["audio"] = audio
		return rec
	}
	full := fullEssence()
	dolby := with(full, "dolbyVision", true, "hdr10Metadata", true)
	deUnd, deEn := with(full, "audioLanguages", []string{"de", "und"}), with(full, "audioLanguages", []string{"de", "en"})
	// SpecHeld, as a reader reads it
	const unlabelled = "its package has an audio track that doesn't say which of its original's streams it was made from, " +
		"and its original's stream 2 (und) has no track of its that does: whether it is that one can't be told"
	spec := func(beyond ...string) string {
		return "its package lacks what the master spec keeps: " + strings.Join(beyond, ", ") +
			"; it is kept until the title is packaged again"
	}
	// kept for now, an original in fields, for why
	fieldsKept := func(why string) string { return FieldsKept + ": " + why }
	// kept for good for the closed captions the original carries, where
	carriedAt := func(where string) string { return CaptionsCarried + " at " + where + "; " + captionsCarriedNote }
	// kept until a read tells, for why
	// kept, a read of its original saying nothing of the video
	refused := func(why string) string {
		return CaptionsUnknown + ": a read of its original says nothing of the video: " + why + "; the captions " +
			"backfill reads it again (captions.backfill)"
	}
	unknownFor := func(why string) string {
		return CaptionsUnknown + ": " + why + "; the captions backfill reads " + map[bool]string{true: "them",
			false: "it"}[strings.Contains(why, "decoded no frame")] + " again (captions.backfill)"
	}
	for _, c := range []struct {
		name    string
		src     map[string]any
		streams []any // the original's streams; none listed when nil
		pkg     map[string]any
		change  func(t *testing.T, f *listFixture)
		kept    string // why a dry run skips it; "" it would retire
		held    bool   // the retire job holds it for the version (else it is not due)
		job     string // why the retire job holds it, when it is not kept
	}{
		// what the spec drops by design
		{name: "surround and the channels above the package's", src: with(full, "surround", true, "maxAudioChannels", 6),
			pkg: record(full, "SDR")},
		{name: "lossless audio", src: with(full, "losslessAudio", true), pkg: record(full, "SDR")},
		{name: "object-based audio", src: with(full, "objectAudio", true), pkg: record(full, "SDR")},
		{name: "Dolby Vision, the package keeping HDR10", src: dolby, streams: dolbyStreams(1),
			pkg: withAudio(record(with(full, "hdr10Metadata", true), "PQ"), "", 1)},
		{name: "Dolby Vision on an HLG base, the package keeping HLG", src: with(full, "dolbyVision", true),
			streams: dolbyStreams(4), pkg: withAudio(record(full, "HLG"), "", 1)},
		{name: "3D", src: with(full, "stereo3d", true), pkg: record(full, "SDR")},
		// a language the package carries on the same track under another label
		{name: "an audio track relabelled from und to en", src: deUnd, streams: audioStreams("de", "und"),
			pkg: withAudio(record(with(full, "audioLanguages", []string{"de", "en"}), "SDR"), "de", 1, "en", 2)},
		{name: "an audio track relabelled from und to zxx", src: deUnd, streams: audioStreams("de", "und"),
			pkg: withAudio(record(with(full, "audioLanguages", []string{"de", "zxx"}), "SDR"), "de", 1, "zxx", 2)},
		{name: "an audio track relabelled from en to zxx", src: deEn, streams: audioStreams("de", "en"),
			pkg: withAudio(record(with(full, "audioLanguages", []string{"de", "zxx"}), "SDR"), "de", 1, "zxx", 2)},
		// any other difference
		{name: "an audio language", src: full, pkg: record(with(full, "audioLanguages", []string{"en"}), "SDR"),
			kept: spec("audioLanguages:de"), held: true},
		{name: "an audio language the package has no track of", src: deEn, streams: audioStreams("de", "en"),
			pkg:  withAudio(record(with(full, "audioLanguages", []string{"en"}), "SDR"), "en", 2),
			kept: spec("audioLanguages:de", "audioTrack:1"), held: true},
		{name: "an audio track relabelled from en to und", src: deEn, streams: audioStreams("de", "en"),
			pkg:  withAudio(record(with(full, "audioLanguages", []string{"de", "und"}), "SDR"), "de", 1, "und", 2),
			kept: spec("audioLanguages:en"), held: true},
		{name: "de and en, the one track made of the de stream labelled en", src: deEn, streams: audioStreams("de", "en"),
			pkg:  withAudio(record(with(full, "audioLanguages", []string{"en"}), "SDR"), "en", 1),
			kept: spec("audioLanguages:de", "audioLanguages:en", "audioTrack:2"), held: true},
		{name: "a second track of a language the package keeps, dropped", src: deEn, streams: audioStreams("de", "de", "en"),
			pkg: withAudio(record(deEn, "SDR"), "de", 1, "en", 3), kept: spec("audioTrack:2"), held: true},
		// a record written before its tracks said their streams: their
		// languages counted
		{name: "migrated, a second German track of two dropped", src: deEn, streams: audioStreams("de", "de", "en"),
			pkg: withAudio(record(deEn, "SDR"), "de", -1, "en", -1), kept: spec("audioTrack:de"), held: true},
		{name: "migrated, every track carried", src: deEn, streams: audioStreams("de", "en"),
			pkg: withAudio(record(deEn, "SDR"), "de", -1, "en", -1)},
		{name: "HLG, the package SDR", src: full, streams: hdrStreams("hlg"), pkg: withAudio(record(full, "SDR"), "en", 1),
			kept: spec("hdr"), held: true},
		{name: "HDR10 without its static metadata, the package SDR", src: full, streams: hdrStreams("hdr10"),
			pkg: withAudio(record(full, "SDR"), "en", 1), kept: spec("hdr"), held: true},
		{name: "an audio track that doesn't say which stream it was made from", src: deUnd, streams: audioStreams("de", "und"),
			pkg:  withAudio(record(with(full, "audioLanguages", []string{"de", "zxx"}), "SDR"), "de", 1, "zxx", -1),
			kept: unlabelled, held: true, job: unlabelled + "; the original is kept"},
		// an original in fields, by the widest reading, is not retired for
		// now, whatever its package's scan says was done
		{name: "interlaced, the package's scan inverse-telecine", src: with(full, "interlaced", true),
			pkg: scanned(record(full, "SDR"), "inverse-telecine"), kept: fieldsKept("its record says interlaced"), held: true},
		{name: "interlaced, the package's scan progressive", src: with(full, "interlaced", true),
			pkg: scanned(record(full, "SDR"), "progressive"), kept: fieldsKept("its record says interlaced"), held: true},
		{name: "interlaced, the package's scan deinterlaced-field", src: with(full, "interlaced", true),
			pkg: scanned(record(full, "SDR"), "deinterlaced-field"), kept: fieldsKept("its record says interlaced"), held: true},
		{name: "interlaced, the package's scan deinterlaced-frame", src: with(full, "interlaced", true),
			pkg: scanned(record(full, "SDR"), "deinterlaced-frame"), kept: fieldsKept("its record says interlaced"), held: true},
		{name: "interlaced, the package with no scan", src: with(full, "interlaced", true), pkg: record(full, "SDR"),
			kept: fieldsKept("its record says interlaced"), held: true},
		{name: "in fields by its stream's field order, the package's scan deinterlaced-field", src: full,
			streams: fieldStreams("bb"), pkg: withAudio(scanned(record(full, "SDR"), "deinterlaced-field"), "en", 1),
			kept: fieldsKept("its record's field order is bb"), held: true},
		{name: "a field order of unknown, the package's scan progressive", src: full, streams: fieldStreams("unknown"),
			pkg:  withAudio(scanned(record(full, "SDR"), "progressive", NothingFound), "en", 1),
			kept: fieldsKept("its record's field order is unknown"), held: true},
		{name: "in fields by its stored probe, its record progressive", src: full, streams: fieldStreams("progressive"),
			pkg: withAudio(scanned(record(full, "SDR"), "inverse-telecine", NothingFound), "en", 1),
			change: func(t *testing.T, f *listFixture) {
				// the probe the record was made of, kept beside it and in its
				// checksums
				librarytest.Write(t, filepath.Join(f.sourceDir, "ffprobe.json"), []byte(`{"format": {"format_name":
					"matroska,webm"}, "streams": [{"index": 0, "codec_type": "video", "codec_name": "mpeg2video", "field_order": "tt", "start_time": "0.000000",
					"avg_frame_rate": "24/1"}]}`))
				f.rewriteSource(t, full, fieldStreams("progressive"))
			}, kept: fieldsKept("its probe's field order is tt"), held: true},
		// found to hold fields, though its record says progressive
		{name: "a telecined film its record calls progressive, the package's scan inverse-telecine", src: full,
			streams: fieldStreams("progressive"),
			pkg:     withAudio(scanned(record(full, "SDR"), "inverse-telecine", "telecine"), "en", 1),
			kept:    fieldsKept("the scan of its package of version " + rtVersion + " found telecine"), held: true},
		{name: "HEVC field pictures its record calls progressive, the package's scan fields-kept", src: full,
			streams: fieldStreams("progressive"),
			pkg:     withAudio(scanned(record(full, "SDR"), "fields-kept", "field-pictures"), "en", 1),
			kept:    fieldsKept("the scan of its package of version " + rtVersion + " found field-pictures"), held: true},
		{name: "a scan that found it couldn't tell", src: full, streams: fieldStreams("progressive"),
			pkg:  withAudio(scanned(record(full, "SDR"), "progressive", "unknown"), "en", 1),
			kept: fieldsKept("the scan of its package of version " + rtVersion + " found unknown"), held: true},
		{name: "found in fields by another version's package", src: full, streams: fieldStreams("progressive"),
			pkg: withAudio(scanned(record(full, "SDR"), "none", NothingFound), "en", 1),
			change: func(t *testing.T, f *listFixture) {
				librarytest.WriteVersion(t, VersionDir(f.itemDir, "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"), librarytest.Version{
					VersionID: "0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f", PackageID: "1d2e3f4a-5b6c-4d7e-8f9a-0b1c2d3e4f5a",
					SourceIDs: []string{rtSource}, CreatedAt: "2026-10-01T10:00:00Z",
					Package: scanned(record(full, "SDR"), "deinterlaced-field", "fields")})
			}, kept: fieldsKept("the scan of its package of version 0c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f found fields"),
			held: true},
		// a field order neither its record nor its probe states is not
		// known, whatever a package's scan says: no word of the transcoder's
		// lets an original go
		{name: "no field order stated, the package with no scan", src: full, streams: audioStreams("en"),
			pkg: withAudio(unscanned(record(full, "SDR")), "en", 1), change: noProbe(audioStreams("en")),
			kept: fieldsKept("its record's field order is not stated"), held: true},
		{name: "no field order stated, the package's scan found it progressive", src: full, streams: audioStreams("en"),
			pkg: withAudio(scanned(record(full, "SDR"), "none", NothingFound), "en", 1), change: noProbe(audioStreams("en")),
			kept: fieldsKept("its record's field order is not stated"), held: true},
		{name: "no field order stated, the package's scan found film", src: full, streams: audioStreams("en"),
			pkg: withAudio(scanned(record(full, "SDR"), "inverse-telecine", "film"), "en", 1), change: noProbe(audioStreams("en")),
			kept: fieldsKept("the scan of its package of version " + rtVersion + " found film"), held: true},
		// neither marked nor found: the ordinary way
		{name: "a progressive original, the package's scan found it so", src: full, streams: fieldStreams("progressive"),
			pkg: withAudio(scanned(record(full, "SDR"), "none", NothingFound), "en", 1)},
		// a scan that says no found, or did something to the picture, holds
		{name: "a progressive original, the package's scan none, nothing found said", src: full,
			streams: fieldStreams("progressive"), pkg: withAudio(scanned(record(full, "SDR"), "none"), "en", 1),
			kept: fieldsKept("the scan of its package of version " + rtVersion + " found nothing"), held: true},
		{name: "a progressive original, the package's scan deinterlaced-frame", src: full, streams: fieldStreams("progressive"),
			pkg:  withAudio(scanned(record(full, "SDR"), "deinterlaced-frame", NothingFound), "en", 1),
			kept: fieldsKept("the scan of its package of version " + rtVersion + " did deinterlaced-frame"), held: true},
		{name: "a subtitle language", src: full, pkg: record(with(full, "subtitleLanguages", []string{"en"}, "subtitleTracks", 1),
			"SDR", map[string]any{"language": "en"}), kept: spec("subtitleLanguages:de", "subtitleTracks"), held: true},
		{name: "a forced subtitle", src: with(full, "forcedSubtitleLanguages", []string{"de"}), pkg: record(full, "SDR"),
			kept: spec("forcedSubtitleLanguages:de"), held: true},
		{name: "an SDH subtitle", src: with(full, "sdhSubtitleLanguages", []string{"en"}), pkg: record(full, "SDR"),
			kept: spec("sdhSubtitleLanguages:en"), held: true},
		{name: "a style", src: with(full, "styledSubtitles", true), pkg: record(full, "SDR"), kept: spec("styledSubtitles"),
			held: true},
		{name: "a font", src: with(full, "fonts", true), pkg: record(full, "SDR"), kept: spec("fonts"), held: true},
		// closed captions, of the original alone: one that carries them in
		// any window of its read, or whose record says it does, is kept for
		// good whatever its package carries, a copy too; one whose captions
		// can't be told is kept until a read tells; one a read of every
		// planned window found none in goes on them
		{name: "closed captions in a window, the package encoded", src: full, pkg: encoded(record(full, "SDR")),
			change: captionsRead(readOf(0.5)), kept: carriedAt("50%"), held: true},
		{name: "closed captions in a window, the package copied", src: full, pkg: copied(record(full, "SDR")),
			change: captionsRead(readOf(0, 0.9)), kept: carriedAt("start, 90%"), held: true},
		{name: "closed captions in a window, the package's record says it carries them", src: full,
			pkg: encoded(record(with(full, "closedCaptions", true), "SDR")), change: captionsRead(readOf(0.1)),
			kept: carriedAt("10%"), held: true},
		{name: "closed captions the original's record says, none in the windows of a read", src: with(full,
			"closedCaptions", true), pkg: copied(record(full, "SDR")), change: captionsRead(readOf()),
			kept: CaptionsCarried + " (its record says so); " + captionsCarriedNote, held: true},
		{name: "closed captions the original's record says, not read", src: with(full, "closedCaptions", true),
			pkg: encoded(record(full, "SDR")), change: captionsUnread,
			kept: CaptionsCarried + " (its record says so); " + captionsCarriedNote, held: true},
		{name: "no closed captions in any planned window, the package encoded", src: full, pkg: encoded(record(full, "SDR")),
			change: captionsRead(readOf())},
		{name: "no closed captions in any planned window, the package copied", src: full, pkg: copied(record(full, "SDR")),
			change: captionsRead(readOf()), kept: copyKept, held: true, job: copyKept + "; the original is kept"},
		{name: "a short video read whole, its length said", src: full, pkg: encoded(record(full, "SDR")),
			change: func(t *testing.T, f *listFixture) {
				// the record's length: the span it is read over
				f.rewriteRecord(t, func(rec map[string]any) {
					rec["container"].(map[string]any)["durationMs"] = 40000
				}, nil)
				captionsOf([]map[string]any{{"at": 0.0, "start": 0.0, "end": 39.958, "frames": 960, "packets": 960,
					"leading": 0, "whole": true, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}}, 40)(t, f)
			}},
		{name: "a short video's windows", src: full, pkg: encoded(record(full, "SDR")),
			change: func(t *testing.T, f *listFixture) {
				f.rewriteRecord(t, func(rec map[string]any) {
					rec["container"].(map[string]any)["durationMs"] = 40000
				}, nil)
				captionsOf(readOfLength(40, 3), 40)(t, f)
			}, kept: refused("two of its picture's places lie closer than a window reaches (2.5 s: 48 packets at its frame " +
				"step, its gop and 0.5 s): such a picture is read whole, every packet decoded, not in windows"), held: true,
			job: refused("two of its picture's places lie closer than a window reaches (2.5 s: 48 packets at its frame "+
				"step, its gop and 0.5 s): such a picture is read whole, every packet decoded, not in windows") +
				"; the original is kept"},
		{name: "fewer windows than its length plans", src: full, pkg: encoded(record(full, "SDR")),
			change: captionsRead(readOf()[:4]),
			kept: refused("it has no window at 70%, 90% (a read is of its start and 10, 30, 50, 70 and 90 % of its " +
				"picture's span, six windows)"), held: true,
			job: refused("it has no window at 70%, 90% (a read is of its start and 10, 30, 50, 70 and 90 % of its "+
				"picture's span, six windows)") + "; the original is kept"},
		{name: "a read of the start alone", src: full, pkg: encoded(record(full, "SDR")),
			change: captionsRead(readOf()[:1]),
			kept: refused("it has no window at 10%, 30%, 50%, 70%, 90% (a read is of its start and 10, 30, 50, 70 and " +
				"90 % of its picture's span, six windows)"), held: true,
			job: refused("it has no window at 10%, 30%, 50%, 70%, 90% (a read is of its start and 10, 30, 50, 70 and "+
				"90 % of its picture's span, six windows)") + "; the original is kept"},
		{name: "a window of the original that decoded no frame", src: full, pkg: encoded(record(full, "SDR")),
			change: captionsRead(with1(readOf(), win(0.5, 0, false))),
			kept:   unknownFor("its original's windows at 50% decoded no frame"), held: true,
			job: unknownFor("its original's windows at 50% decoded no frame") + "; the original is kept"},
		{name: "windows that read the same frames over (a clock that starts again)", src: full,
			pkg: encoded(record(full, "SDR")), change: captionsRead([]map[string]any{win(0, 48, false),
				{"at": 0.1, "start": 95445.1, "end": 95447.0, "frames": 47, "captions": false},
				{"at": 0.3, "start": 95445.1, "end": 95447.0, "frames": 47, "captions": false},
				{"at": 0.5, "start": 95445.1, "end": 95447.0, "frames": 47, "captions": false},
				{"at": 0.7, "start": 95445.1, "end": 95447.0, "frames": 47, "captions": false},
				{"at": 0.9, "start": 95445.1, "end": 95447.0, "frames": 47, "captions": false}}),
			kept: refused("windows at 10%, 30% start in the wrong order"), held: true,
			job: refused("windows at 10%, 30% start in the wrong order") + "; the original is kept"},
		{name: "closed captions not read, the package encoded", src: full, pkg: record(full, "SDR"), change: captionsUnread,
			kept: captionsUnknownWhy, held: true, job: captionsUnknownWhy + "; the original is kept"},
		{name: "closed captions not read, the package copied", src: full, pkg: copied(record(full, "SDR")),
			change: captionsUnread, kept: copyKept, held: true, job: copyKept + "; the original is kept"},
		{name: "a lower bit depth", src: full, pkg: record(with(full, "videoBitDepth", 8), "SDR"), kept: spec("videoBitDepth"),
			held: true},
		{name: "a lower height", src: with(full, "maxVideoHeight", 2160), pkg: record(full, "SDR"), kept: spec("maxVideoHeight"),
			held: true},
		{name: "the HDR", src: with(full, "hdr10Metadata", true), pkg: record(full, "PQ"), kept: spec("hdr10Metadata"), held: true},
		{name: "a commentary", src: with(full, "commentaryTracks", 1), pkg: record(full, "SDR"), kept: spec("commentaryTracks"),
			held: true},
		{name: "an audio description", src: with(full, "audioDescriptionTracks", 1), pkg: record(full, "SDR"),
			kept: spec("audioDescriptionTracks"), held: true},
		{name: "Dolby Vision, the package SDR", src: with(full, "dolbyVision", true), streams: dolbyStreams(1),
			pkg: withAudio(record(full, "SDR"), "", 1), kept: spec("dolbyVision", "hdr"), held: true},
		{name: "Dolby Vision on an HDR10 base, the package HLG", src: with(full, "dolbyVision", true), streams: dolbyStreams(1),
			pkg: withAudio(record(full, "HLG"), "", 1), kept: spec("dolbyVision"), held: true},
		{name: "the channels, the package keeping no stereo", src: with(full, "surround", true, "maxAudioChannels", 6),
			pkg: record(with(full, "maxAudioChannels", 1), "SDR"), kept: spec("maxAudioChannels"), held: true},
		// what a package can't show, until it is known
		{name: "chapters the catalog lacks", src: full, pkg: record(full, "SDR"),
			change: func(t *testing.T, f *listFixture) {
				storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_chapterreads WHERE item_id = $1`, rtFilm)
			}, kept: unshownChapters},
		{name: "a picture area not measured", src: full, pkg: record(full, "SDR"),
			change: func(t *testing.T, f *listFixture) {
				storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_pictureareas WHERE id = $1`, rtFilm)
			}, kept: unshownPicture},
		{name: "a picture area measured on the package", src: full, pkg: record(full, "SDR"),
			change: func(t *testing.T, f *listFixture) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_pictureareas SET measuredon = 'package', sourceid = NULL
					WHERE id = $1`, rtFilm)
			}, kept: unshownPicture},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := listFixtureOf(t, newRetireFixtureOf(t, c.src, full))
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_workerrules (worker, version, rules, ruleshash, reportedat, changedat)
				VALUES ('packager', 'test', '{"surroundAudio": "off"}', 'test', now(), now())`)
			if c.streams != nil {
				f.rewriteSource(t, c.src, c.streams)
			}
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: c.pkg})
			if c.change != nil {
				c.change(t, f)
			}
			_, items := f.dry(t, rtFilm)
			switch {
			case len(items) != 1:
				t.Fatalf("the dry run's titles: %v", items)
			case c.kept == "" && !wouldRetire(items[0]):
				t.Errorf("the dry run: %s; want it would retire", said(items[0]))
			case c.kept != "" && said(items[0]) != "skipped: "+c.kept:
				t.Errorf("the dry run: %s\nwant skipped: %s", said(items[0]), c.kept)
			}

			// the retire job, as the policy has it retire originals
			f.set(t, SettingOriginals, OriginalsDelete)
			rep, err := f.r.Pass(ctx)
			if err != nil {
				t.Fatalf("the pass: %v", err)
			}
			switch {
			case c.kept == "":
				if rep.Originals != 1 {
					t.Fatalf("the retire job's pass: %+v; want the original retired", rep)
				}
				return
			case c.held:
				if rep.Originals != 0 || rep.Held != 1 {
					t.Fatalf("the retire job's pass: %+v; want the original held", rep)
				}
				why := c.job
				if why == "" {
					why = c.kept
				}
				if got := f.step(t, rtFilm); got != "failed 1 error="+why+" details="+HeldDetails(rtVersion) {
					t.Errorf("the retire step: %s", got)
				}
			default:
				// not due, and the pass names it, saying why
				want := Report{Kept: []Kept{{SourceID: rtSource, ItemID: rtFilm, Why: c.kept, For: KeptUnshown}}}
				if rep.String() != want.String() || len(rep.Kept) != 1 || rep.Kept[0] != want.Kept[0] {
					t.Fatalf("the retire job's pass: %+v; want the original not due, named kept: %+v", rep, want.Kept)
				}
			}
			f.kept(t)
		})
	}
}

// An original the retire job holds - its records leave its subtitles unknown
// - is looked at once: held for its version, it takes no turn of the job's
// rate after that, and another title's original is retired at the next
// passes, the rate at one a pass.
func TestAnOriginalItsRecordsLeaveUnknownTakesNoOtherTurn(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
		SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"essence": f.pkg}})
	// completed first: the job looks at it first
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '2 hours' WHERE id = $1`,
		rtVersion)
	f.set(t, SettingRetireRate, "1")
	base := time.Now()
	f.r.now = func() time.Time { return base }
	if rep, err := f.r.Pass(ctx); rep.Held != 1 || err != nil {
		t.Errorf("the first pass: %+v, %v; want the original held", rep, err)
	}
	const film2, src2, ver2, pkg2 = "6f1b2d3e-4c5a-4b7f-9e0d-1c2b3d4e5f60", "1c7d8e9f-0a1b-4c2d-9e3f-4a5b6c7d8e9f",
		"8b3f4a5b-6c7d-4e8f-9a0b-1c2d3e4f5a6c", "3e2d1c4b-5a6f-4d7e-8f9a-0b1c2d3e4f50"
	f.addMovie(t, film2, src2, ver2, pkg2, "Other Film")
	held := 0
	for i := 1; i <= 8; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		f.r.now = func() time.Time { return at }
		rep, _ := f.r.Pass(ctx)
		held += rep.Held + rep.Failed
	}
	if held != 0 {
		t.Errorf("the held original was looked at %d times again in 8 passes", held)
	}
	if s := f.source(t, src2); s.State != SourceDeleted {
		t.Errorf("the other film's original, which its package carries, is %s after 8 passes", s.State)
	}
	if got := f.step(t, rtFilm); !strings.HasPrefix(got, "failed 1 error=its package's record says nothing about its subtitles") ||
		!strings.HasSuffix(got, " details="+HeldDetails(rtVersion)) {
		t.Errorf("the held original's retire step: %s", got)
	}
	if s := f.source(t, rtSource); s.State != SourcePresent || !exists(f.original) {
		t.Errorf("the held original: %s, there %v", s.State, exists(f.original))
	}
}

// The retire job names an original it leaves out for what its package can't
// show, with the reason, in the report of every pass; the sweep says it once
// until that changes.
func TestTheRetireJobNamesWhatItWaitsFor(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_chapterreads WHERE item_id = $1`, rtFilm)
	rep, err := f.r.Pass(ctx)
	if err != nil || rep.Originals != 0 || len(rep.Kept) != 1 || rep.Kept[0] != (Kept{SourceID: rtSource, ItemID: rtFilm,
		Why: unshownChapters, For: KeptUnshown}) {
		t.Fatalf("the pass: %+v, %v", rep, err)
	}
	const said = "1 originals kept until their chapters and picture area are known"
	if rep.String() != said {
		t.Errorf("the pass says %q, want %q", rep.String(), said)
	}
	f.r.every = 0
	first, err := f.r.Sweep(ctx)
	if err != nil || first != said {
		t.Errorf("the sweep: %q, %v", first, err)
	}
	if again, err := f.r.Sweep(ctx); err != nil || again != "" {
		t.Errorf("the sweep again, nothing changed: %q, %v", again, err)
	}
	f.settle(t, rtFilm, rtSource, rtVersion)
	if done, err := f.r.Sweep(ctx); err != nil || done != "1 originals deleted" {
		t.Errorf("the sweep once its chapters are known: %q, %v", done, err)
	}
}

// A superseded original is held against its master, made of another file, by
// its tracks' languages counted (the master's tracks say the new file's
// streams): one with two German tracks against a master with one is kept,
// whatever an admin says; one whose master carries every track goes.
func TestASupersededOriginalIsHeldByItsTracksCounted(t *testing.T) {
	full := fullEssence()
	streams := []any{map[string]any{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"},
		map[string]any{"index": 1, "type": "audio", "channels": 2, "language": "de"},
		map[string]any{"index": 2, "type": "audio", "channels": 2, "language": "de"},
		map[string]any{"index": 3, "type": "audio", "channels": 2, "language": "en"},
		map[string]any{"index": 4, "type": "subtitle", "codec": "subrip", "language": "de"},
		map[string]any{"index": 5, "type": "subtitle", "codec": "subrip", "language": "en"}}
	master := func(audio ...string) map[string]any {
		rec := packageRecord(full)
		var tracks []any
		for i, l := range audio {
			tracks = append(tracks, map[string]any{"id": "a" + strconv.Itoa(i), "channels": 2, "language": l,
				"sourceStreamIndex": 1 + i})
		}
		rec["renditions"] = map[string]any{"audio": tracks}
		return rec
	}
	for _, c := range []struct {
		name   string
		master map[string]any
		held   string
	}{
		{"a master with one German track of two", master("de", "en"), "audioTrack:de is more than the master spec drops by design"},
		{"a master with every track", master("de", "de", "en"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, full, full)
			f.rewriteSource(t, full, streams)
			f.supersede(t, false, nil)
			// the new file's record: its subtitles the master's are made of
			librarytest.WriteSource(t, SourceDir(f.itemDir, rtNewSource), map[string]any{"sourceId": rtNewSource,
				"essence": full, "streams": streams}, statedProbe(nil))
			f.completeRecord(t, c.master)
			rep := f.pass(t)
			if c.held == "" {
				if rep.Superseded != 1 {
					t.Fatalf("the pass: %+v; want it retired", rep)
				}
				return
			}
			if rep.Superseded != 0 || rep.ForAnAdmin != 1 {
				t.Fatalf("the pass: %+v; want it kept", rep)
			}
			if x := f.supersededInfo(t); x.Hold == nil || !strings.HasPrefix(*x.Hold, "kept: ") || !strings.Contains(*x.Hold, c.held) {
				t.Errorf("the hold: %v", deref(x.Hold))
			}
		})
	}
}

// An admin's correction of a track's language (setTrackLanguage), recorded
// for that stream of the title's source with who and when, is that stream's
// language for the gate: a package track labelled so carries it. Without
// the record, or with one of another stream, one that doesn't say who set
// it, or one set before the original was recorded (of the file the title had
// before), the label differs and keeps the original, and the retire job's
// report names it with both labels, that an admin sees what to record.
func TestATrackLanguageAnAdminCorrectedIsTheStreamsLanguage(t *testing.T) {
	ctx := context.Background()
	full := with2(fullEssence(), "audioLanguages", []string{"de", "en"})
	streams := []any{map[string]any{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"},
		map[string]any{"index": 1, "type": "audio", "channels": 2, "language": "de"},
		map[string]any{"index": 2, "type": "audio", "channels": 2, "language": "en"},
		map[string]any{"index": 3, "type": "subtitle", "codec": "subrip", "language": "de"},
		map[string]any{"index": 4, "type": "subtitle", "codec": "subrip", "language": "en"}}
	// the package labels the original's second track, tagged en, de
	pkg := packageRecord(with2(full, "audioLanguages", []string{"de"}))
	pkg["renditions"] = map[string]any{"audio": []any{
		map[string]any{"id": "a0", "channels": 2, "language": "de", "sourceStreamIndex": 1},
		map[string]any{"id": "a1", "channels": 2, "language": "de", "sourceStreamIndex": 2}}}
	const labels = "its audio track 1 (stream 2) is en in its original and de in its package"
	for _, c := range []struct {
		name       string
		correction string // the row of com_nalet_katalog_itemtracklanguages; "" none
		goes       bool
	}{
		{"corrected by an admin", `('audio', 1, 'ger', 'an-admin', now())`, true},
		{"not corrected", "", false},
		{"another stream corrected", `('audio', 0, 'ger', 'an-admin', now())`, false},
		{"corrected by no one said", `('audio', 1, 'ger', NULL, now())`, false},
		// of the file the title had before: the original was recorded an hour ago
		{"corrected before the original was recorded", `('audio', 1, 'ger', 'an-admin', now() - interval '365 days')`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, full, full)
			f.rewriteSource(t, full, streams)
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Package: pkg})
			if c.correction != "" {
				storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language,
					modifiedby, modifiedat) VALUES ($1, `+strings.TrimPrefix(c.correction, "("), rtFilm)
			}
			rep, err := f.r.Pass(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c.goes {
				if rep.Originals != 1 {
					t.Errorf("the pass: %+v; want the original retired", rep)
				}
				return
			}
			if rep.Originals != 0 || rep.Held != 1 {
				t.Fatalf("the pass: %+v; want the original held", rep)
			}
			if len(rep.Kept) != 1 || rep.Kept[0] != (Kept{SourceID: rtSource, ItemID: rtFilm, Why: labels, For: KeptLabel}) ||
				!strings.Contains(rep.String(), "1 originals kept for a track labelled otherwise than its original's") {
				t.Errorf("the pass's report: %+v, %s", rep.Kept, rep.String())
			}
			f.kept(t)
		})
	}
}

func with2(base map[string]any, kv ...any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// noProbe is the change of a case that takes what its original's stored
// probe says of its picture away but the plan of it (its start and frame
// rate, which a read is held to), its record its essence as it is and the
// streams streams: a field order nothing states.
func noProbe(streams []any) func(*testing.T, *listFixture) {
	return func(t *testing.T, f *listFixture) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(f.sourceDir, "source.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rec map[string]any
		if err := json.Unmarshal(b, &rec); err != nil {
			t.Fatal(err)
		}
		essence, _ := rec["essence"].(map[string]any)
		if err := os.WriteFile(filepath.Join(f.sourceDir, ProbeFile), []byte(`{"format": {"format_name": "matroska,webm"},
			"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "start_time": "0.000000",
			"avg_frame_rate": "24/1"}]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		// and a record whose picture states no field order either
		var unstated []any
		for _, e := range streams {
			st := maps.Clone(e.(map[string]any))
			if st["type"] == "video" {
				delete(st, "fieldOrder")
			}
			unstated = append(unstated, st)
		}
		f.rewriteSource(t, essence, unstated)
	}
}
