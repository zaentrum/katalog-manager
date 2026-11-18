package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// An essence fact the gate compares is of its type as the record logic
// writes it: a flag a boolean, a count a whole number, a language list a
// list of languages; null a picture's height or bit depth only of a record
// that lists no picture. Any other value is no "nothing to lose", and holds
// the original; so does a package flag that is no boolean.
func TestAnEssenceFactOfAnotherTypeHolds(t *testing.T) {
	for name, change := range map[string]func(e map[string]any){
		"height and bit depth null":  func(e map[string]any) { e["videoBitDepth"], e["maxVideoHeight"] = nil, nil },
		"bit depth a string":         func(e map[string]any) { e["videoBitDepth"] = "10" },
		"a count a list":             func(e map[string]any) { e["subtitleTracks"] = []any{} },
		"audioLanguages a string":    func(e map[string]any) { e["audioLanguages"] = "de" },
		"a language list of numbers": func(e map[string]any) { e["subtitleLanguages"] = []any{5} },
		"maxAudioChannels a string":  func(e map[string]any) { e["maxAudioChannels"] = "6" },
		"dolbyVision null":           func(e map[string]any) { e["dolbyVision"] = nil },
		"closedCaptions null":        func(e map[string]any) { e["closedCaptions"] = nil },
		"a count of a fraction":      func(e map[string]any) { e["commentaryTracks"] = 1.5 },
		"a count below 0":            func(e map[string]any) { e["audioDescriptionTracks"] = -1 },
		"a flag a number":            func(e map[string]any) { e["surround"] = 1 },
	} {
		d := t.TempDir()
		e := fullEssence()
		change(e)
		librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a", "essence": e,
			"container": map[string]any{"format": "matroska,webm", "durationMs": 600000}, "streams": filmStreams()},
			statedProbe(nil))
		pkg := packageRecord(map[string]any{"maxVideoHeight": 720, "videoBitDepth": 8})
		librarytest.WriteVersion(t, VersionDir(d, "v-a"), librarytest.Version{VersionID: "v-a", PackageID: "p-a",
			SourceIDs: []string{"a"}, CreatedAt: "2026-10-01T10:00:00Z", Package: pkg})
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.Contains(g.Unknown, "as no value of its kind") {
			t.Errorf("%s: %+v, %v; want it held", name, g, err)
		}
	}
	// null height and bit depth of a record that lists no picture says none
	d := t.TempDir()
	e := fullEssence()
	e["videoBitDepth"], e["maxVideoHeight"] = nil, nil
	librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a", "essence": e,
		"container": map[string]any{"format": "matroska,webm", "durationMs": 600000},
		"streams":   []any{map[string]any{"index": 0, "type": "audio", "channels": 2}}}, nil)
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	if g, err := ReadGate(d, "v-a", "a"); err != nil || strings.Contains(g.Unknown, "as no value of its kind") {
		t.Errorf("a record of no picture, its height and bit depth null: %+v, %v; want them read as none", g, err)
	}
	// a package flag that is no boolean
	d = t.TempDir()
	sourceRecord(t, d, "a", "matroska,webm", filmStreams(), progressiveProbe)
	pkg := packageRecord(fullEssence())
	pkg["essence"].(map[string]any)["dolbyVision"] = "yes"
	librarytest.WriteVersion(t, VersionDir(d, "v-a"), librarytest.Version{VersionID: "v-a", PackageID: "p-a",
		SourceIDs: []string{"a"}, CreatedAt: "2026-10-01T10:00:00Z", Package: pkg})
	if g, err := ReadGateWith(d, "v-a", read(600, every()...), "a"); err != nil ||
		!strings.Contains(g.Unknown, "its package's essence states dolbyVision as no boolean") {
		t.Errorf("a package flag that is no boolean: %+v, %v; want it held", g, err)
	}
}

// An encoder word the transcoder doesn't write of an encode holds the
// original as one whose copy can't be told; its own words let it go.
func TestOnlyTheTranscodersEncoderWordsSayAnEncode(t *testing.T) {
	for _, w := range []string{"copied", "stream copy", "copy-h264", "passthrough", "remux", "none", "-c:v copy",
		"Copy", "LIBX264", "x264"} {
		pkg := doc(t, `{"renditions": {"video": [{"id": "v0", "encoder": "`+w+`"}]}}`)
		if why := copyHolds(pkg); why == "" {
			t.Errorf("encoder %q: let go; want it held", w)
		}
	}
	for _, w := range TranscoderEncoders {
		if why := copyHolds(doc(t, `{"renditions": {"video": [{"id": "v0", "encoder": "`+w+`"}]}}`)); why != "" {
			t.Errorf("encoder %q: %q; want it to go", w, why)
		}
		if why := copyHolds(doc(t, `{"renditions": {"video": [{"id": "v0", "encoder": " `+w+`"}]}}`)); why == "" {
			t.Errorf("encoder %q spaced: let go; want it held", w)
		}
	}
}

// The gate's encoder words are the transcoder's, as its RungPlan.encoder
// says them (src/transcoder/decision.py:825 of zaentrum/transcoder at
// cf2c15f): the list is pinned here, so a change of it is made against
// that file, never by the way.
func TestTheTranscodersEncoderWordsAreTheGates(t *testing.T) {
	if got := strings.Join(TranscoderEncoders, " "); got != "hevc_nvenc h264_nvenc libx265 libx264" {
		t.Errorf("the gate's encoder words: %s; want the transcoder's four at cf2c15f (hevc_nvenc h264_nvenc libx265 "+
			"libx264), or this pin and its comment moved to the transcoder's file of the new words", got)
	}
}

// A window whose frames can't be read (a string, a fraction, null, none, a
// count below 0) is a failed one, and keeps what it found: captions true
// carry them, frames counted interlaced put the original in fields.
func TestAWindowOfFramesThatCantBeReadKeepsWhatItFound(t *testing.T) {
	for _, frames := range []string{`"frames": "48", `, `"frames": 48.0, `, `"frames": null, `, ``, `"frames": -1, `} {
		var w CaptionWindow
		if err := json.Unmarshal([]byte(`{"at": 0.3, "start": 180, "end": 182, `+frames+`"captions": true,
			"interlacedFrames": 5, "repeatFrames": 0}`), &w); err != nil {
			t.Fatal(err)
		}
		ws := every()
		ws[2] = w
		if w.Unreadable == "" || !w.Captions || w.InterlacedFrames == nil || *w.InterlacedFrames != 5 {
			t.Errorf("frames %q: %+v; want a failed window that showed captions and counted 5", frames, w)
		}
		if carried, _ := captionsRule([]Doc{film(600)}, planProbes, read(600, ws...)); !strings.HasPrefix(carried, CaptionsCarried) {
			t.Errorf("frames %q: carried %q; want the captions it showed carried", frames, carried)
		}
		if fields, _ := inFields(t.TempDir(), "", nil, nil, []Doc{film(600)}, nil, read(600, ws...).Captions); fields == "" {
			t.Errorf("frames %q: not in fields; want its interlaced frames counted", frames)
		}
	}
	// a key in another case is the same key: captions false, then Captions
	// true, is a failed window that showed captions
	var w CaptionWindow
	if err := json.Unmarshal([]byte(`{"at": 0.3, "start": 180, "end": 182, "frames": 48, "captions": false,
		"Captions": true, "interlacedFrames": 0, "repeatFrames": 0}`), &w); err != nil || w.Unreadable == "" || !w.Captions {
		t.Errorf("captions in two cases: %+v, %v; want a failed window that showed captions", w, err)
	}
}

// A picture its record marks as captions (the dispositions' captions)
// carries them; a captionSignature that is no boolean, of any codec, says
// nothing the captions can be read by.
func TestARecordsCaptionsMarksOfItsPictureHold(t *testing.T) {
	rec := film(600)
	streams, _ := rec.Get("streams")
	streams.([]any)[0] = append(streams.([]any)[0].(Doc), Field{"dispositions", Doc{{Key: "captions", Value: true}}})
	if carried, _ := captionsRule([]Doc{rec}, planProbes, read(600, every()...)); !strings.HasPrefix(carried, CaptionsCarried) {
		t.Errorf("a picture its record marks as captions: carried %q; want them carried", carried)
	}
	for _, v := range []any{"false", 0, "true", []any{}} {
		rec := film(600)
		streams, _ := rec.Get("streams")
		streams.([]any)[0] = append(streams.([]any)[0].(Doc), Field{"captionSignature", v})
		if carried, unknown := captionsRule([]Doc{rec}, planProbes, read(600, every()...)); carried != "" ||
			!strings.Contains(unknown, "captionSignature of its picture is no boolean") {
			t.Errorf("an h264 picture's captionSignature %v: carried %q, unknown %q; want it not known", v, carried, unknown)
		}
	}
}

// A stored probe that lists no stream of the picture its record lists (one
// of another file, or cut short) is none the gate can read: it holds.
func TestAProbeWithoutTheRecordsPictureHolds(t *testing.T) {
	for name, probe := range map[string]string{
		"a format alone":  `{"format": {"format_name": "matroska,webm"}}`,
		"no stream":       `{"format": {"format_name": "matroska,webm"}, "streams": []}`,
		"no video stream": `{"streams": [{"index": 1, "codec_type": "audio", "codec_name": "aac"}]}`,
		"a cover alone":   `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "disposition": {"attached_pic": 1}}]}`,
	} {
		d := t.TempDir()
		sourceRecord(t, d, "a", "matroska,webm", filmStreams(), probe)
		packageOf(t, d, "v-a", verifiedProgressive(), "a")
		g, err := ReadGateWith(d, "v-a", read(600, every()...), "a")
		if err != nil || !strings.HasPrefix(g.Scan, FieldsKept+": its stored probe can't be read") {
			t.Errorf("%s: %+v, %v; want it held", name, g, err)
		}
	}
}

// On the retire path: an essence fact of another type, a version entry
// that is a link, an encoder word that is no encode's, a window whose
// frames can't be read, a record's captions mark, a captionSignature that
// is no boolean, and a probe without the record's picture keep the
// original; an older version whose scan says nothing it found lets it go.
func TestTheRetireJobKeepsWhatTheReviewFound(t *testing.T) {
	window := func(js string) func(f *retireFixture) {
		return func(f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_sourcecaptions SET captionwindows = $2::jsonb
				WHERE source_id = $1`, rtSource, strings.Replace(noCaptionsWindows, `{"at": 0.3, "start": 180, "end": 181.88, `+
				`"frames": 48, "captions": false, "interlacedFrames": 0, "repeatFrames": 0}`, js, 1))
		}
	}
	record := func(change func(rec map[string]any)) func(f *retireFixture) {
		return func(f *retireFixture) { f.rewriteRecord(t, change, nil) }
	}
	encoder := func(w string) func(f *retireFixture) {
		return func(f *retireFixture) {
			f.rewritePackage(t, func(pkg map[string]any) {
				pkg["renditions"] = map[string]any{"video": []any{map[string]any{"id": "v0", "encoder": w}}}
			}, nil)
		}
	}
	older := func(scan any) func(f *retireFixture) {
		return func(f *retireFixture) {
			pkg := packageRecord(f.pkg)
			pkg["scan"] = scan
			librarytest.WriteVersion(t, VersionDir(f.itemDir, "6d6d6d6d-0000-4000-8000-000000000006"),
				librarytest.Version{VersionID: "6d6d6d6d-0000-4000-8000-000000000006", PackageID: NewID(),
					SourceIDs: []string{rtSource}, CreatedAt: "2026-09-01T10:00:00Z", Package: pkg})
		}
	}
	for _, c := range []struct {
		name   string
		change func(f *retireFixture)
		kept   string // what the source's error holds; "" deleted
	}{
		{"height and bit depth null", record(func(rec map[string]any) {
			e := rec["essence"].(map[string]any)
			e["videoBitDepth"], e["maxVideoHeight"] = nil, nil
		}), "as no value of its kind"},
		{"a version entry that is a link", func(f *retireFixture) {
			// a version made of it that found fields, its folder a link
			other := filepath.Join(t.TempDir(), "elsewhere")
			pkg := packageRecord(f.pkg)
			pkg["scan"] = map[string]any{"found": "fields"}
			librarytest.WriteVersion(t, other, librarytest.Version{VersionID: "linked", PackageID: NewID(),
				SourceIDs: []string{rtSource}, CreatedAt: "2026-09-01T10:00:00Z", Package: pkg})
			if err := os.Symlink(other, filepath.Join(f.itemDir, "versions", "linked")); err != nil {
				t.Fatal(err)
			}
		}, "which is no folder of its own"},
		{"encoder stream copy", encoder("stream copy"), "no word the transcoder writes"},
		{"encoder passthrough", encoder("passthrough"), "no word the transcoder writes"},
		{"encoder hevc_nvenc", encoder("hevc_nvenc"), ""},
		{"captions in a window of frames as a string", window(`{"at": 0.3, "start": 180, "end": 181.88, "frames": "48",
			"captions": true, "interlacedFrames": 0, "repeatFrames": 0}`), CaptionsCarried},
		{"a picture its record marks as captions", record(func(rec map[string]any) {
			rec["streams"].([]any)[0].(map[string]any)["dispositions"] = map[string]any{"captions": true}
		}), CaptionsCarried},
		{"a captionSignature that is no boolean", record(func(rec map[string]any) {
			rec["streams"].([]any)[0].(map[string]any)["captionSignature"] = "false"
		}), "no boolean"},

		{"an older version's scan of found progressive, unverified", older(map[string]any{"found": NothingFound}), ""},
		{"an older version's scan of null", older(nil), ""},
		{"an older version's scan of found fields", older(map[string]any{"found": "fields"}), "found fields"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			c.change(f)
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
	// a stored probe whose only video stream is a cover (marked by its tags,
	// attached_pic 0) and no film: the probe's want of the record's picture
	// holds the original in fields - the gate shows it beside the captions
	// rule's hold, as such a probe gives no plan either - and the retire
	// pass keeps it
	t.Run("a probe without the picture", func(t *testing.T) {
		f := newRetireFixtureOf(t, sameEssence, sameEssence)
		probe := `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "start_time": "0.000000",
			"avg_frame_rate": "24/1", "disposition": {"attached_pic": 0}, "tags": {"filename": "cover.jpg",
			"mimetype": "image/jpeg"}}, {"index": 1, "codec_type": "audio", "codec_name": "aac"}]}`
		f.rewriteRecord(t, func(map[string]any) {}, &probe)
		g, err := ReadGateWith(f.itemDir, rtVersion, read(600, every()...), rtSource)
		if err != nil || !strings.HasPrefix(g.Scan, FieldsKept+": its stored probe can't be read: it lists no picture, "+
			"and its record lists one") || !strings.HasPrefix(g.Unknown, CaptionsNoPlan) {
			t.Errorf("the gate: %+v, %v; want the fields held by the probe, and no plan", g, err)
		}
		f.keptFor(t, "a probe without the picture", CaptionsNoPlan)
	})
}

// A read that found nothing is held to the record it was made under: once
// the record is written again, the gate takes it no more (the original is
// read again); a read that found captions is of the file, whichever record.
func TestTheGateTakesACleanReadOfItsOwnRecordAlone(t *testing.T) {
	f := newRetireFixtureOf(t, sameEssence, sameEssence)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET recordedat = recordedat + interval '1 second'
		WHERE id = $1`, rtSource)
	f.keptFor(t, "a clean read of the record before", "or a read that found nothing is of another record of it")
	g := newRetireFixtureOf(t, sameEssence, sameEssence)
	storetest.Exec(t, g.st, `UPDATE com_nalet_katalog_sourcecaptions SET captions = true, captionwindows = $2::jsonb
		WHERE source_id = $1`, rtSource, strings.Replace(noCaptionsWindows, `"at": 0.5, "start": 300, "end": 301.88, `+
		`"frames": 48, "captions": false`, `"at": 0.5, "start": 300, "end": 301.88, "frames": 48, "captions": true`, 1))
	storetest.Exec(t, g.st, `UPDATE com_nalet_katalog_itemsources SET recordedat = recordedat + interval '1 second'
		WHERE id = $1`, rtSource)
	g.keptFor(t, "a read with captions of the record before", CaptionsCarried)
}

// An extra's original the gate lets go is checked against its record again
// right before it is moved or unlinked: a file put in its place while its
// folder was verified stays where it is.
func TestAnExtrasOriginalIsCheckedAgainRightBeforeItGoes(t *testing.T) {
	for _, grace := range []string{"0", "24h"} {
		f := newRetireFixture(t)
		f.set(t, SettingTrashGrace, grace)
		f.r.extraGate = func(string) (Gate, error) { return Gate{}, nil }
		const extra = "e4e4e4e4-0000-4000-8000-000000000004"
		src := anExtra(t, f, extra)
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		other := []byte(strings.Repeat("y", len(b)))
		beforeExtraMove = func() { librarytest.Write(t, src, other) }
		rep, err := f.r.Pass(t.Context())
		beforeExtraMove = nil
		if rep.Extras != 0 || err == nil || !strings.Contains(err.Error(), "the original changed since it was recorded") {
			t.Errorf("trash grace %s: %+v, %v; want the file put in its place kept", grace, rep, err)
		}
		if got, _ := os.ReadFile(src); string(got) != string(other) {
			t.Errorf("trash grace %s: the file put in its place is gone", grace)
		}
	}
}
