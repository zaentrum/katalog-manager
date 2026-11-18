package library

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// The deletion gate is the validator's: testdata/gate/cases.json holds what
// validate-library-v2.py's deletion_gate gives for each case, and the port
// gives the same, sorted.
func TestTheDeletionGateIsTheValidators(t *testing.T) {
	raw, err := os.ReadFile("testdata/gate/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases.([]any) {
		d := c.(Doc)
		name := str(d, "name")
		var sources []Doc
		ss, _ := d.Get("sources")
		for _, s := range ss.([]any) {
			sources = append(sources, s.(Doc))
		}
		p, _ := d.Get("package")
		want := stringsOf(must2(d.Get("gate")))
		got := DeletionGate(sources, p.(Doc))
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s:\n got  %v\n want %v", name, got, want)
		}
	}
}

func must2(v any, _ bool) any { return v }

// The gate of a version is read from its records: the essence of its
// sources' source.json minus its package.json's.
func TestGateOfReadsTheRecords(t *testing.T) {
	item := t.TempDir()
	librarytest.WriteSource(t, SourceDir(item, "s1"), map[string]any{"essence": stated(map[string]any{
		"surround": true, "maxAudioChannels": 6, "subtitleLanguages": []string{"de", "en"}}), "streams": filmStreams()},
		statedProbe(nil))
	librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
		SourceIDs: []string{"s1"}, CreatedAt: "2026-10-06T09:00:00Z", Package: map[string]any{
			"scan": verifiedProgressive(), "essence": stated(map[string]any{
				"surround": false, "maxAudioChannels": 2, "subtitleLanguages": []string{"en"}}),
			"subtitles": []any{map[string]any{"id": "sub0", "path": "subs/0.vtt", "format": "webvtt", "language": "en"}}}})
	got, err := GateOf(item, "v1", "s1")
	if err != nil || strings.Join(got, " ") != "maxAudioChannels subtitleLanguages:de surround" {
		t.Errorf("the gate: %v, %v", got, err)
	}
	if _, err := GateOf(item, "v1", "s2"); err == nil {
		t.Error("the gate of a source without a record")
	}
	if _, err := GateOf(filepath.Join(item, "none"), "v1", "s1"); err == nil {
		t.Error("the gate of a version without a record")
	}
}

// pgsOriginal is the essence of a 5.1 original with n image (PGS) subtitle
// tracks in three languages, and its streams as its record lists them.
func pgsOriginal(n int) (map[string]any, []any) {
	essence := map[string]any{"surround": true, "maxAudioChannels": 6, "audioLanguages": []string{"en"},
		"subtitleTracks": n, "commentarySubtitles": 0, "imageSubtitles": true, "styledSubtitles": false,
		"subtitleLanguages": []string{"de", "en", "fr"}, "sdhSubtitleLanguages": []string{}, "forcedSubtitleLanguages": []string{}}
	streams := []any{map[string]any{"index": 0, "type": "video"}, map[string]any{"index": 1, "type": "audio", "channels": 6}}
	for i := range n {
		streams = append(streams, map[string]any{"index": 2 + i, "type": "subtitle", "codec": "hdmv_pgs_subtitle",
			"form": "image", "language": []string{"de", "en", "fr"}[i%3]})
	}
	return essence, streams
}

// stereoCarrying is the essence of a stereo package, with the subtitle facts
// of the essence subs.
func stereoCarrying(subs map[string]any) map[string]any {
	out := map[string]any{"surround": false, "maxAudioChannels": 2, "audioLanguages": []string{"en"},
		"maxVideoHeight": 1080, "videoBitDepth": 8, "commentaryTracks": 0, "audioDescriptionTracks": 0}
	for _, k := range []string{"subtitleTracks", "commentarySubtitles", "imageSubtitles", "styledSubtitles",
		"subtitleLanguages", "sdhSubtitleLanguages", "forcedSubtitleLanguages"} {
		if v, ok := subs[k]; ok {
			out[k] = v
		}
	}
	return out
}

// pgsFiles are a version's package files with the n image subtitles
// pgsListed lists.
func pgsFiles(n int) map[string]string {
	out := map[string]string{}
	for k, v := range librarytest.DefaultFiles {
		out[k] = v
	}
	for i := range n {
		out["subs/"+strconv.Itoa(i)+".sup"] = "PG a picture subtitle " + strconv.Itoa(i)
	}
	return out
}

// pgsListed is a package record's list of n image subtitles, as the original's
// streams were.
func pgsListed(n int) []any {
	out := []any{}
	for i := range n {
		out = append(out, map[string]any{"id": "sub" + strconv.Itoa(i), "path": "subs/" + strconv.Itoa(i) + ".sup",
			"format": "pgs", "language": []string{"de", "en", "fr"}[i%3], "sourceStreamIndex": 2 + i})
	}
	return out
}

// An original's subtitles go only where its package's record tells what it
// carries of them. A record that says nothing about them - no subtitle field
// at all, or an essence that claims the original's subtitles while the
// record lists none of them - is unknown, and unknown is not let go: the gate
// can't be told, where before it said the original lost nothing of its
// subtitles. So is a record whose essence says more tracks than it lists, or
// says nothing of a subtitle fact the original has, and an original whose
// record lists subtitle streams its essence says nothing of.
func TestTheGateDoesNotLetGoSubtitlesTheRecordsLeaveUnknown(t *testing.T) {
	src, streams := pgsOriginal(12)
	noLanguages := stereoCarrying(src)
	delete(noLanguages, "subtitleLanguages")
	audioOnly := map[string]any{"surround": true, "maxAudioChannels": 6, "audioLanguages": []string{"en"}}
	noCount := stereoCarrying(src)
	delete(noCount, "subtitleTracks")
	var englishNowhere, englishThere []any
	for i := range 12 {
		englishNowhere = append(englishNowhere, map[string]any{"id": "sub" + strconv.Itoa(i), "path": "subs/nowhere-" +
			strconv.Itoa(i) + ".vtt", "format": "webvtt", "language": "en"})
		englishThere = append(englishThere, map[string]any{"id": "sub" + strconv.Itoa(i), "path": "subs/" + strconv.Itoa(i) +
			".sup", "format": "webvtt", "language": "eng"})
	}
	for _, c := range []struct {
		name    string
		source  map[string]any
		pkg     map[string]any
		unknown string
	}{
		{"a package record with no subtitle field at all",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(nil)},
			"its package's record says nothing about its subtitles (its original has 12 subtitle tracks)"},
		{"an essence that claims the original's subtitles, its record listing none",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(src)},
			"its package's record says nothing about its subtitles (its original has 12 subtitle tracks)"},
		{"a subtitles field that is null",
			map[string]any{"essence": src},
			map[string]any{"essence": stereoCarrying(src), "subtitles": nil},
			"its package's record says nothing about its subtitles (its original has 12 subtitle tracks)"},
		{"an essence that says more tracks than its record lists",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(src), "subtitles": []any{}},
			"its package's essence says 12 subtitle tracks, and its record lists 0"},
		{"an essence that says nothing of a subtitle fact the original has",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": noLanguages, "subtitles": pgsListed(12)},
			"its package's essence says nothing of subtitleLanguages (its original has 12 subtitle tracks)"},
		{"an original never probed: no stream, no subtitle fact",
			map[string]any{"essence": map[string]any{}, "streams": []any{}},
			map[string]any{"essence": stereoCarrying(nil), "subtitles": []any{}},
			"its original's record lists no stream, and its essence says nothing of its subtitles (it was never probed)"},
		{"an empty list under an essence that claims the languages and pictures, without a count",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": noCount, "subtitles": []any{}},
			"its package's essence says imageSubtitles, subtitleLanguages:de, subtitleLanguages:en, subtitleLanguages:fr, and " +
				"its record lists no such subtitle (its original has 12 subtitle tracks)"},
		{"a list of one language, of files that aren't there, under an essence that claims three",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(src), "subtitles": englishNowhere},
			"its package's essence says 12 subtitle tracks, and its record lists 0 (its original has 12 subtitle tracks)"},
		{"a list of one language under an essence that claims three",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(src), "subtitles": englishThere},
			"its package's essence says imageSubtitles, subtitleLanguages:de, subtitleLanguages:fr, and its record lists no " +
				"such subtitle (its original has 12 subtitle tracks)"},
		{"an original whose essence says nothing of the subtitle streams its record lists",
			map[string]any{"essence": audioOnly, "streams": streams[:4]},
			map[string]any{"essence": stereoCarrying(nil)},
			"its original's record lists 2 subtitle tracks, and its essence says nothing of subtitleTracks, commentarySubtitles, " +
				"imageSubtitles, styledSubtitles, subtitleLanguages, sdhSubtitleLanguages, forcedSubtitleLanguages"},
	} {
		t.Run(c.name, func(t *testing.T) {
			item := t.TempDir()
			librarytest.WriteSource(t, SourceDir(item, "s1"), c.source, statedProbe(nil))
			librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
				SourceIDs: []string{"s1"}, CreatedAt: "2026-10-06T09:00:00Z", Files: pgsFiles(12), Package: c.pkg})
			got, err := GateOf(item, "v1", "s1")
			if err == nil || !strings.HasPrefix(err.Error(), c.unknown) ||
				!strings.HasSuffix(err.Error(), ": what its package carries of them can't be told") {
				t.Errorf("the gate: %v, %v\nwant it unknown: %s", got, err, c.unknown)
			}
		})
	}
}

// Where the package's record tells, the gate is what it was: a package that
// lists the original's subtitles loses nothing of them, and one whose record
// says it carries none (an empty list, an essence of none) loses them all,
// each named; an original with no subtitles asks nothing of the package's.
func TestTheGateOfSubtitlesTheRecordsTell(t *testing.T) {
	src, streams := pgsOriginal(12)
	none := map[string]any{"subtitleTracks": 0, "commentarySubtitles": 0, "imageSubtitles": false, "styledSubtitles": false,
		"subtitleLanguages": []string{}, "sdhSubtitleLanguages": []string{}, "forcedSubtitleLanguages": []string{}}
	stereoSource := map[string]any{"surround": false, "maxAudioChannels": 2, "audioLanguages": []string{"en"}}
	// the package's stereo track, made of the original's audio stream
	stereoTrack := map[string]any{"audio": []any{map[string]any{"id": "a0", "channels": 2, "sourceStreamIndex": 1}}}
	// each of the original's 12 subtitle streams, lost
	var tracksLost []string
	for i := range 12 {
		tracksLost = append(tracksLost, "subtitleTrack:"+strconv.Itoa(2+i))
	}
	sort.Strings(tracksLost)
	allLost := func(names ...string) string {
		out := append(slices.Clone(names), tracksLost...)
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	for _, c := range []struct {
		name   string
		source map[string]any
		pkg    map[string]any
		gate   string
	}{
		{"a package that lists the original's subtitles",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(src), "subtitles": pgsListed(12), "renditions": stereoTrack},
			"maxAudioChannels surround"},
		{"a package whose record says it carries none",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(none), "subtitles": []any{}, "renditions": stereoTrack},
			allLost("imageSubtitles", "maxAudioChannels", "subtitleLanguages:de", "subtitleLanguages:en", "subtitleLanguages:fr",
				"subtitleTracks", "surround")},
		{"a package whose record lists none, its essence silent",
			map[string]any{"essence": src, "streams": streams},
			map[string]any{"essence": stereoCarrying(nil), "subtitles": []any{}, "renditions": stereoTrack},
			// the validator's: a count the package lacks is lost, though the original's is 0
			allLost("commentarySubtitles", "imageSubtitles", "maxAudioChannels", "subtitleLanguages:de", "subtitleLanguages:en",
				"subtitleLanguages:fr", "subtitleTracks", "surround")},
		{"an original with no subtitles",
			map[string]any{"essence": stereoSource, "streams": streams[:2]},
			map[string]any{"essence": stereoCarrying(none), "renditions": stereoTrack},
			""},
	} {
		t.Run(c.name, func(t *testing.T) {
			item := t.TempDir()
			source := maps.Clone(c.source)
			source["essence"] = stated(source["essence"].(map[string]any))
			librarytest.WriteSource(t, SourceDir(item, "s1"), source, statedProbe(nil))
			librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
				SourceIDs: []string{"s1"}, CreatedAt: "2026-10-06T09:00:00Z", Files: pgsFiles(12), Package: c.pkg})
			got, err := GateOf(item, "v1", "s1")
			if err != nil || strings.Join(got, " ") != c.gate {
				t.Errorf("the gate: %v, %v\nwant %s", got, err, c.gate)
			}
		})
	}
}

// The gate holds a version made of one original track by track: every audio
// and subtitle stream of the original needs a package track that says it was
// made from it (sourceStreamIndex), whatever the other tracks are labelled.
// Its label is no difference where it is the stream's own, the stream's was
// und (the catalog learned it), or it is zxx or mul; any other, to und among
// them, loses the stream's language. A stream no track was made from is lost
// ("audioTrack:2"), and its language where no other stream of it is carried;
// where a package track says no stream, that can't be told. A subtitle no
// longer for the hard of hearing loses that. A version of several originals
// can't be held so (a track says its stream, not of which original), nor a
// master against an original it wasn't made of, which is held by the
// essences. An HDR picture packaged SDR is lost (hdr).
func TestTheGateHoldsAnOriginalTrackByTrack(t *testing.T) {
	audio := func(i int, lang string) map[string]any {
		return map[string]any{"index": i, "type": "audio", "language": lang, "channels": 2}
	}
	sub := func(i int, lang, purpose string) map[string]any {
		return map[string]any{"index": i, "type": "subtitle", "codec": "subrip", "language": lang, "purpose": purpose}
	}
	video := func(format string) map[string]any {
		return map[string]any{"index": 0, "type": "video", "hdr": map[string]any{"format": format}}
	}
	// st, its field order stated progressive: a master of another file
	// says nothing of this original's fields, its record does
	progressive := func(st map[string]any) map[string]any {
		out := maps.Clone(st)
		out["fieldOrder"] = "progressive"
		return out
	}
	track := func(lang string, from int) map[string]any {
		a := map[string]any{"id": "a" + strconv.Itoa(from) + lang, "codec": "mp4a.40.2", "channels": 2, "language": lang}
		if from >= 0 {
			a["sourceStreamIndex"] = from
		}
		return a
	}
	subtitle := func(lang string, from int, purpose string) map[string]any {
		x := map[string]any{"id": "s" + strconv.Itoa(from) + lang, "path": "subs/0.vtt", "format": "webvtt", "language": lang,
			"purpose": purpose}
		if from >= 0 {
			x["sourceStreamIndex"] = from
		}
		return x
	}
	noSubs := map[string]any{"subtitleTracks": 0, "commentarySubtitles": 0, "imageSubtitles": false, "styledSubtitles": false,
		"subtitleLanguages": []string{}, "sdhSubtitleLanguages": []string{}, "forcedSubtitleLanguages": []string{}}
	ess := func(kv ...any) map[string]any {
		out := map[string]any{"maxAudioChannels": 2, "surround": false}
		for k, v := range noSubs {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i].(string)] = kv[i+1]
		}
		return out
	}
	langs := func(l ...string) []string { return l }
	type source struct {
		id      string
		streams []any
		essence map[string]any
	}
	for _, c := range []struct {
		name      string
		sources   []source // the version's originals; the gate is read for the first
		made      []string // the originals the version was made of; the sources' when nil
		pkg       map[string]any
		renditon  map[string]any
		files     []string // the package's files besides librarytest.DefaultFiles
		gate      string
		unknownIs string
	}{
		{name: "und labelled en", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "und")},
			ess("audioLanguages", langs("de", "und"))}}, pkg: ess("audioLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("en", 2)}}},
		{name: "en labelled zxx", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "en")},
			ess("audioLanguages", langs("de", "en"))}}, pkg: ess("audioLanguages", langs("de", "zxx")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("zxx", 2)}}},
		{name: "und labelled zxx", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "und")},
			ess("audioLanguages", langs("de", "und"))}}, pkg: ess("audioLanguages", langs("de", "zxx")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("zxx", 2)}}},
		{name: "en labelled mul", sources: []source{{"s1", []any{video("sdr"), audio(1, "en")},
			ess("audioLanguages", langs("en"))}}, pkg: ess("audioLanguages", langs("mul")),
			renditon: map[string]any{"audio": []any{track("mul", 1)}}},
		{name: "en labelled und", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "en")},
			ess("audioLanguages", langs("de", "en"))}}, pkg: ess("audioLanguages", langs("de", "und")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("und", 2)}}, gate: "audioLanguages:en"},
		{name: "de and en, the one track made of the de stream labelled en",
			sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "en")}, ess("audioLanguages", langs("de", "en"))}},
			pkg:     ess("audioLanguages", langs("en")), renditon: map[string]any{"audio": []any{track("en", 1)}},
			gate: "audioLanguages:de audioLanguages:en audioTrack:2"},
		{name: "a second track of a language the package keeps, dropped",
			sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "de"), audio(3, "en")},
				ess("audioLanguages", langs("de", "en"))}}, pkg: ess("audioLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("en", 3)}}, gate: "audioTrack:2"},
		{name: "a track that says no stream", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "und")},
			ess("audioLanguages", langs("de", "und"))}}, pkg: ess("audioLanguages", langs("de", "zxx")),
			renditon:  map[string]any{"audio": []any{track("de", 1), track("zxx", -1)}},
			unknownIs: "its package has an audio track that doesn't say which of its original's streams it was made from"},
		{name: "a subtitle of de labelled en, a sidecar's beside it",
			sources: []source{{"s1", []any{video("sdr"), audio(1, "en"), sub(2, "de", "dialogue"), sub(3, "en", "dialogue")},
				ess("audioLanguages", langs("en"), "subtitleTracks", 2, "subtitleLanguages", langs("de", "en"))}},
			pkg: ess("audioLanguages", langs("en"), "subtitleTracks", 2, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("en", 1)}, "subtitles": []any{subtitle("en", 2, "dialogue"),
				map[string]any{"id": "side", "path": "subs/1.vtt", "format": "webvtt", "language": "en", "fromSidecar": "x"}}},
			gate: "subtitleLanguages:de subtitleLanguages:en subtitleTrack:3"},
		{name: "a subtitle of und labelled en", sources: []source{{"s1", []any{video("sdr"), audio(1, "en"),
			sub(2, "und", "dialogue")}, ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("und"))}},
			pkg:      ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("en", 1)}, "subtitles": []any{subtitle("en", 2, "dialogue")}}},
		{name: "a subtitle for the hard of hearing no longer", sources: []source{{"s1", []any{video("sdr"), audio(1, "en"),
			sub(2, "en", "sdh")}, ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en"),
			"sdhSubtitleLanguages", langs("en"))}},
			pkg:      ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("en", 1)}, "subtitles": []any{subtitle("en", 2, "dialogue")}},
			gate:     "sdhSubtitleLanguages:en"},
		{name: "a version of two originals", sources: []source{
			{"s1", []any{video("sdr"), audio(1, "de")}, ess("audioLanguages", langs("de"))},
			{"s2", []any{video("sdr"), audio(1, "en")}, ess("audioLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("en")), renditon: map[string]any{"audio": []any{track("en", 1)}},
			unknownIs: "its version is made of 2 originals"},
		// a master of another original: its tracks' indexes are that file's, so
		// their languages are counted
		{name: "a master of another original, und against en", made: []string{"s2"}, sources: []source{
			{"s1", []any{video("sdr"), audio(1, "und")}, ess("audioLanguages", langs("und"))},
			{"s2", []any{video("sdr"), audio(1, "en")}, ess("audioLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("en")), renditon: map[string]any{"audio": []any{track("en", 1)}},
			unknownIs: "its package's audio tracks don't say which of its original's streams they were made from, and it " +
				"has fewer tracks of und than its original (0 of 1) and more of en"},
		{name: "a master of another original with one German track of two", made: []string{"s2"}, sources: []source{
			{"s1", []any{progressive(video("sdr")), audio(1, "de"), audio(2, "de"), audio(3, "en")}, ess("audioLanguages", langs("de", "en"))},
			{"s2", []any{video("sdr"), audio(1, "de"), audio(2, "en")}, ess("audioLanguages", langs("de", "en"))}},
			pkg:      ess("audioLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("de", 1), track("en", 2)}}, gate: "audioTrack:de"},
		{name: "a master of another original carrying every track", made: []string{"s2"}, sources: []source{
			{"s1", []any{progressive(video("sdr")), audio(1, "de"), audio(2, "en")}, ess("audioLanguages", langs("de", "en"))},
			{"s2", []any{video("sdr"), audio(1, "en"), audio(2, "de")}, ess("audioLanguages", langs("de", "en"))}},
			pkg:      ess("audioLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("en", 1), track("de", 2)}}},
		// a record written before its tracks said their streams: their
		// languages are counted, and a subtitle made of a file beside the
		// original - its path names no subtitle stream of it - counts for none
		{name: "migrated, a second German track of two dropped", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"),
			audio(2, "de"), audio(3, "en")}, ess("audioLanguages", langs("de", "en"))}},
			pkg:      ess("audioLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("ger", -1), track("eng", -1)}}, gate: "audioTrack:de"},
		{name: "migrated, the embedded subtitle dropped, one made of a file beside it", sources: []source{{"s1",
			[]any{video("sdr"), audio(1, "en"), sub(2, "en", "dialogue")}, ess("audioLanguages", langs("en"),
				"subtitleTracks", 1, "subtitleLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("eng", -1)}, "subtitles": []any{map[string]any{"id": "sub1",
				"path": "subs/1.vtt", "format": "webvtt", "language": "eng"}}},
			gate: "subtitleLanguages:en subtitleTrack:en"},
		// a file beside the original numbered after the packaged file's
		// subtitles, where an encode left the embedded stream out (Matroska
		// carries no mov_text), takes that stream's number: it is none of
		// its streams; nor is one named otherwise
		{name: "migrated, a mov_text subtitle left out, a file beside it as subs/0", sources: []source{{"s1",
			[]any{video("sdr"), audio(1, "en"), map[string]any{"index": 2, "type": "subtitle", "codec": "mov_text",
				"language": "en", "purpose": "dialogue"}}, ess("audioLanguages", langs("en"), "subtitleTracks", 1,
				"subtitleLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("eng", -1)}, "subtitles": []any{map[string]any{"id": "sub0",
				"path": "subs/0.vtt", "format": "webvtt", "language": "eng"}}},
			gate: "subtitleLanguages:en subtitleTrack:en"},
		{name: "migrated, the embedded subtitle dropped, an old library file beside it", sources: []source{{"s1",
			[]any{video("sdr"), audio(1, "en"), sub(2, "en", "dialogue")}, ess("audioLanguages", langs("en"),
				"subtitleTracks", 1, "subtitleLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("en"), "subtitleTracks", 1, "subtitleLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("eng", -1)}, "subtitles": []any{map[string]any{"id": "sub0",
				"path": "subs/legacy_0.vtt", "format": "webvtt", "language": "eng"}}},
			files: []string{"subs/legacy_0.vtt"}, gate: "subtitleLanguages:en subtitleTrack:en"},
		{name: "migrated, every track carried and old library files beside them", sources: []source{{"s1",
			[]any{video("sdr"), audio(1, "en"), sub(2, "en", "dialogue"), sub(3, "de", "dialogue")},
			ess("audioLanguages", langs("en"), "subtitleTracks", 2, "subtitleLanguages", langs("de", "en"))}},
			pkg: ess("audioLanguages", langs("en"), "subtitleTracks", 2, "subtitleLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("eng", -1)}, "subtitles": []any{
				map[string]any{"id": "sub0", "path": "subs/0.vtt", "format": "webvtt", "language": "eng"},
				map[string]any{"id": "sub1", "path": "subs/1.vtt", "format": "webvtt", "language": "ger"},
				map[string]any{"id": "sub2", "path": "subs/legacy_1.vtt", "format": "webvtt", "language": "eng"}}},
			files: []string{"subs/legacy_1.vtt"}},
		{name: "migrated, every track carried", sources: []source{{"s1", []any{video("sdr"), audio(1, "de"), audio(2, "en"),
			sub(3, "en", "dialogue")}, ess("audioLanguages", langs("de", "en"), "subtitleTracks", 1,
			"subtitleLanguages", langs("en"))}},
			pkg: ess("audioLanguages", langs("de", "en"), "subtitleTracks", 2, "subtitleLanguages", langs("de", "en")),
			renditon: map[string]any{"audio": []any{track("ger", -1), track("eng", -1)}, "subtitles": []any{
				map[string]any{"id": "sub0", "path": "subs/0.vtt", "format": "webvtt", "language": "eng"},
				map[string]any{"id": "sub1", "path": "subs/1.vtt", "format": "webvtt", "language": "ger"}}}},
		{name: "HLG packaged SDR", sources: []source{{"s1", []any{video("hlg"), audio(1, "en")}, ess("audioLanguages",
			langs("en"))}}, pkg: ess("audioLanguages", langs("en")), renditon: map[string]any{"audio": []any{track("en", 1)},
			"video": []any{map[string]any{"id": "v0", "videoRange": "SDR"}}}, gate: "hdr"},
		{name: "HDR10 without its metadata packaged SDR", sources: []source{{"s1", []any{video("hdr10"), audio(1, "en")},
			ess("audioLanguages", langs("en"))}}, pkg: ess("audioLanguages", langs("en")),
			renditon: map[string]any{"audio": []any{track("en", 1)}, "video": []any{map[string]any{"id": "v0", "videoRange": "SDR"}}},
			gate:     "hdr"},
		{name: "HDR10 packaged PQ", sources: []source{{"s1", []any{video("hdr10"), audio(1, "en")}, ess("audioLanguages",
			langs("en"))}}, pkg: ess("audioLanguages", langs("en")), renditon: map[string]any{"audio": []any{track("en", 1)},
			"video": []any{map[string]any{"id": "v0", "videoRange": "PQ"}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			item := t.TempDir()
			var ids []string
			for _, src := range c.sources {
				librarytest.WriteSource(t, SourceDir(item, src.id), map[string]any{"essence": stated(src.essence),
					"streams": src.streams}, statedProbe(nil))
				ids = append(ids, src.id)
			}
			made := c.made
			if made == nil {
				made = ids
			}
			subs, _ := c.renditon["subtitles"].([]any)
			if subs == nil {
				subs = []any{}
			}
			ren := map[string]any{"audio": c.renditon["audio"]}
			if v, ok := c.renditon["video"]; ok {
				ren["video"] = v
			}
			files := map[string]string{}
			for k, v := range librarytest.DefaultFiles {
				files[k] = v
			}
			for _, f := range c.files {
				files[f] = "WEBVTT\n"
			}
			librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
				SourceIDs: made, CreatedAt: "2026-10-06T09:00:00Z", Files: files, Package: map[string]any{
					"essence": stated(c.pkg), "subtitles": subs, "renditions": ren}})
			// a master of another original is asked as one (the superseded
			// path's gate)
			g, err := ReadGateWith(item, "v1", GateFacts{CaptionsNotAsked: true, Master: !slices.Contains(made, ids[0])},
				ids[0])
			var got []string
			switch {
			case err == nil && g.Unknown != "":
				err = &GateUnknown{Why: g.Unknown}
			case err == nil:
				got = g.Lost
			}
			if c.unknownIs != "" {
				if err == nil || !strings.HasPrefix(err.Error(), c.unknownIs) {
					t.Errorf("the gate: %v, %v; want it unknown: %s", got, err, c.unknownIs)
				}
				return
			}
			if err != nil || strings.Join(got, " ") != c.gate {
				t.Errorf("the gate: %v, %v; want %q", got, err, c.gate)
			}
		})
	}
}

// An interlaced original whose package is progressive is kept: the gate names
// interlaced (GateFlags), which the master spec's list doesn't name. A list
// that names it lets it go, and the original's other losses are what they
// were.
func TestInterlacingIsKeptUntilTheListNamesIt(t *testing.T) {
	item := t.TempDir()
	librarytest.WriteSource(t, SourceDir(item, "s1"), map[string]any{"essence": stated(map[string]any{"interlaced": true,
		"surround": true, "maxAudioChannels": 6, "subtitleLanguages": []string{"en"}}), "streams": filmStreams()}, nil)
	librarytest.WriteVersion(t, VersionDir(item, "v1"), librarytest.Version{VersionID: "v1", PackageID: "p1",
		SourceIDs: []string{"s1"}, CreatedAt: "2026-10-06T09:00:00Z", Package: packageRecord(map[string]any{
			"interlaced": false, "surround": false, "maxAudioChannels": 2, "subtitleLanguages": []string{"en"}})})
	g, err := ReadGate(item, "v1", "s1")
	if err != nil || g.Unknown != "" || strings.Join(g.Lost, " ") != "interlaced maxAudioChannels surround" {
		t.Fatalf("the gate: %+v, %v", g, err)
	}
	if got := g.Beyond(); strings.Join(got, " ") != "interlaced" {
		t.Errorf("beyond the spec's list: %v; want interlaced", got)
	}
	if got := g.beyondOf(append(slices.Clone(SpecDropped), "interlaced")); len(got) != 0 {
		t.Errorf("beyond a list that names interlaced: %v; want nothing", got)
	}
	if !IsGateName("interlaced") {
		t.Error("interlaced is no name of the gate")
	}
}

// An original whose record and probe state no field order, and that no
// package's scan examined, counts as in fields as its group's policy says
// (AbsentFieldOrder): for now every group's keeps it; a policy that lets a
// group go lets those of it go the ordinary way, and keeps the others.
func TestAnAbsentFieldOrderIsDecidedByItsGroupsPolicy(t *testing.T) {
	recordOf := func(codec string) Doc {
		d, err := DecodeDoc([]byte(`{"container": {"format": "matroska,webm"}, "streams": [{"index": 0, "type": "video",
			"codec": "` + codec + `"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	dir := t.TempDir()
	for _, codec := range []string{"h264", "mpeg2video"} {
		if why, _ := inFields(dir, "", nil, []string{"s1"}, []Doc{recordOf(codec)}, nil, nil); why != absentFieldOrderWhy {
			t.Errorf("%s, no field order, no scan: %q; want it kept", codec, why)
		}
	}
	defer func(p func(FieldOrderGroup) bool) { AbsentFieldOrder = p }(AbsentFieldOrder)
	AbsentFieldOrder = func(g FieldOrderGroup) bool { return !(g.Codec == "h264" && g.Container == "matroska,webm") }
	if why, _ := inFields(dir, "", nil, []string{"s1"}, []Doc{recordOf("h264")}, nil, nil); why != "" {
		t.Errorf("a group the policy lets go: %q", why)
	}
	if why, _ := inFields(dir, "", nil, []string{"s1"}, []Doc{recordOf("mpeg2video")}, nil, nil); why != absentFieldOrderWhy {
		t.Errorf("a group the policy keeps: %q", why)
	}
}
