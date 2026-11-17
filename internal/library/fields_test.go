package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// sourceRecord writes the source record of sid in the item folder itemDir,
// of the container format container: a full essence (interlaced false, as
// the record logic writes it of a stream whose field order ffprobe leaves
// out) and the streams streams; probe, when not "", is its stored probe.
func sourceRecord(t *testing.T, itemDir, sid, container string, streams []any, probe string) Doc {
	t.Helper()
	rec := map[string]any{"sourceId": sid, "file": map[string]any{"name": sid + ".file", "sizeBytes": 1},
		"container": map[string]any{"format": container, "durationMs": 600000}, "essence": fullEssence(),
		"streams": streams}
	librarytest.WriteSource(t, SourceDir(itemDir, sid), rec, nil)
	if probe != "" {
		if err := os.WriteFile(filepath.Join(SourceDir(itemDir, sid), ProbeFile), []byte(probe), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	d, err := readRecordDoc(filepath.Join(SourceDir(itemDir, sid), "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// packageOf writes the version vid made of the sources sids, its package's
// scan scan (nil: a package from before the packager wrote one).
func packageOf(t *testing.T, itemDir, vid string, scan map[string]any, sids ...string) {
	t.Helper()
	pkg := packageRecord(fullEssence())
	delete(pkg, "scan")
	if scan != nil {
		pkg["scan"] = scan
	}
	librarytest.WriteVersion(t, VersionDir(itemDir, vid), librarytest.Version{VersionID: vid, PackageID: "p-" + vid,
		SourceIDs: sids, CreatedAt: "2026-10-01T10:00:00Z", Package: pkg})
}

// noFieldOrder is a picture stream whose record states no field order.
var noFieldOrder = []any{map[string]any{"index": 0, "type": "video", "codec": "mpeg4", "height": 576}}

// noFieldOrderProbe is its stored probe, which states none either.
const noFieldOrderProbe = `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mpeg4", "height": 576}]}`

// A field order nothing states keeps the original as its group's policy
// says (for now every group's), whatever a package's scan says: one that
// found it progressive lets it go no more than none does. Only an
// examination of the original itself would, which the catalog reads none of
// yet.
func TestAFieldOrderNothingStatesKeepsWhateverAScanSays(t *testing.T) {
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "avi", noFieldOrder, noFieldOrderProbe)
	packageOf(t, d, "v-a", nil, "a")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why != absentFieldOrderWhy {
		t.Errorf("an absent field order with no scan: %q; want it kept", why)
	}
	packageOf(t, d, "v-b", verifiedProgressive(), "a")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why != absentFieldOrderWhy {
		t.Errorf("an absent field order its own package's scan found progressive: %q; want it kept", why)
	}
}

// A scan examines the original its package was made of (version.json's
// sourceIds): the scan of a package of another file (a master of the title)
// is no examination of this one, and the gate of the one against that master
// keeps it.
func TestAnotherFilesScanExaminesNotThisOne(t *testing.T) {
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "avi", noFieldOrder, noFieldOrderProbe)
	sourceRecord(t, d, "b", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "h264",
		"fieldOrder": "progressive"}}, "")
	packageOf(t, d, "v-a", nil, "a")
	packageOf(t, d, "v-b", verifiedProgressive(), "b")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why == "" {
		t.Errorf("an original never examined goes on the scan of a package of another file")
	}
	g, err := ReadGateWith(d, "v-b", GateFacts{}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if g.Scan == "" {
		t.Errorf("the gate of an original against a master of another file: not in fields (lost %v)", g.Lost)
	}
}

// A package record or a stored probe that can't be read (cut short, say,
// by a write that didn't finish) keeps the original: what it says can't be
// told. One not there says nothing.
func TestAnUnreadablePackageOrProbeKeeps(t *testing.T) {
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "hevc",
		"fieldOrder": "progressive"}}, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "hevc",
		"field_order": "progressive"}]}`)
	packageOf(t, d, "v-old", map[string]any{"found": "field-pictures"}, "a")
	packageOf(t, d, "v-new", verifiedProgressive(), "a")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why == "" {
		t.Fatal("the old package's scan found fields, and the original is not in fields")
	}
	cut := func(path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b[:len(b)/2], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cut(filepath.Join(VersionDir(d, "v-old"), PackageFile))
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); !strings.Contains(why, "can't be read") {
		t.Errorf("the package that found fields unreadable: %q; want it kept", why)
	}
	// the old package gone, the stored probe cut short
	if err := os.RemoveAll(VersionDir(d, "v-old")); err != nil {
		t.Fatal(err)
	}
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); why != "" {
		t.Fatalf("with every package progressive: %q", why)
	}
	cut(filepath.Join(SourceDir(d, "a"), ProbeFile))
	g, err := ReadGateWith(d, "v-new", GateFacts{}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.Scan, "its stored probe can't be read") {
		t.Errorf("a stored probe that can't be read: scan %q; want it kept", g.Scan)
	}
}

// Its picture is its one picture stream: a second video stream (a picture
// written as a track of its own, not marked as an attachment) that states
// progressive states nothing of the picture's, and keeps the original, as
// which is its picture can't be told.
func TestAnotherVideoStreamStatesNothingOfThePicture(t *testing.T) {
	d := t.TempDir()
	streams := []any{map[string]any{"index": 0, "type": "video", "codec": "mpeg4", "height": 576},
		map[string]any{"index": 1, "type": "video", "codec": "mjpeg", "height": 240, "fieldOrder": "progressive"}}
	rec := sourceRecord(t, d, "a", "matroska,webm", streams, "")
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	if why, _ := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil); !strings.Contains(why, "2 picture streams") {
		t.Errorf("a second video stream: %q; want it kept", why)
	}
}

// The policy's group is the picture's codec and the record's container
// (container.format).
func TestTheGroupIsThePicturesCodecAndTheRecordsContainer(t *testing.T) {
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "avi", noFieldOrder, noFieldOrderProbe)
	packageOf(t, d, "v-a", nil, "a")
	var seen []FieldOrderGroup
	defer func(p func(FieldOrderGroup) bool) { AbsentFieldOrder = p }(AbsentFieldOrder)
	AbsentFieldOrder = func(g FieldOrderGroup) bool { seen = append(seen, g); return true }
	inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, nil)
	if len(seen) != 1 || seen[0] != (FieldOrderGroup{Codec: "mpeg4", Container: "avi"}) {
		t.Errorf("the group of an AVI's MPEG-4 picture: %+v; want {mpeg4 avi}", seen)
	}
}

// A read of the frames says of each window how many were interlaced and how
// many repeat a field: any above 0 is in fields; a window that decoded
// frames and says neither (an older read) leaves its fields not known.
func TestAReadsInterlacedOrRepeatedFramesAreFields(t *testing.T) {
	d := t.TempDir()
	rec := sourceRecord(t, d, "a", "matroska,webm", []any{map[string]any{"index": 0, "type": "video", "codec": "h264",
		"fieldOrder": "progressive"}}, "")
	packageOf(t, d, "v-a", verifiedProgressive(), "a")
	some := 3
	for _, c := range []struct {
		name            string
		change          func(*CaptionWindow)
		fields, unknown string
	}{
		{"none", func(*CaptionWindow) {}, "", ""},
		{"interlaced", func(w *CaptionWindow) { w.InterlacedFrames = &some }, "a read of its frames found interlaced ones at 50%", ""},
		{"repeated", func(w *CaptionWindow) { w.RepeatFrames = &some }, "a read of its frames found ones that repeat a field at 50%", ""},
		{"not said", func(w *CaptionWindow) { w.InterlacedFrames, w.RepeatFrames = nil, nil }, "",
			FramesUnknown + ": a read of its original says nothing of interlaced or repeated frames at 50%"},
	} {
		ws := every()
		c.change(&ws[3])
		fields, unknown := inFields(d, "", nil, []string{"a"}, []Doc{rec}, nil, read(600, ws...).Captions)
		if fields != c.fields || !strings.HasPrefix(unknown, c.unknown) || (c.unknown == "") != (unknown == "") {
			t.Errorf("%s: fields %q, unknown %q; want %q, %q", c.name, fields, unknown, c.fields, c.unknown)
		}
	}
}

// A video stream is an attached picture, and no film, as the analyzer
// counts one: the container marks it as an attached file (a Matroska
// attachment's filename and mimetype tags, or the attached picture flag),
// it is an image (its mimetype image/*; an MP4's cover, which has none, of
// a codec of CoverCodecs) and it has no length of its own (no more than one
// frame counted, no DURATION tag). Its file's name decides nothing. A video
// stream of an image not so marked, or with a length, is a film, which
// keeps the original where it makes more than one.
func TestAnAttachedPictureIsKnownByItsMarksAnImageAndNoLength(t *testing.T) {
	doc := func(js string) Doc {
		d, err := DecodeDoc([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	const film = `{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"}`
	for _, c := range []struct {
		name   string
		second string
		films  int
	}{
		{"a Matroska attachment", `{"index": 1, "codec_type": "video", "codec_name": "mjpeg",
			"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}`, 1},
		{"an attached picture with an image's mimetype", `{"index": 1, "codec_type": "video", "codec_name": "png",
			"disposition": {"attached_pic": 1}, "tags": {"mimetype": "image/png"}}`, 1},
		{"an MP4's cover: attached, no mimetype, one frame", `{"index": 1, "codec_type": "video",
			"codec_name": "mjpeg", "nb_frames": "1", "disposition": {"attached_pic": 1}}`, 1},
		{"an MP4's cover of BMP", `{"index": 1, "codec_type": "video", "codec_name": "bmp",
			"disposition": {"attached_pic": 1}}`, 1},
		{"attached, no mimetype, a codec no cover comes as", `{"index": 1, "codec_type": "video", "codec_name": "gif",
			"disposition": {"attached_pic": 1}}`, 2},
		{"an attached picture of a video codec", `{"index": 1, "codec_type": "video", "codec_name": "h264",
			"disposition": {"attached_pic": 1}}`, 2},
		{"an attached picture of more than one frame", `{"index": 1, "codec_type": "video", "codec_name": "mjpeg",
			"nb_frames": "250", "disposition": {"attached_pic": 1}}`, 2},
		{"an attachment with a length of its own", `{"index": 1, "codec_type": "video", "codec_name": "mjpeg",
			"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg", "DURATION": "00:05:00.000000000"}}`, 2},
		{"an attachment counted many frames", `{"index": 1, "codec_type": "video", "codec_name": "mjpeg",
			"tags": {"filename": "cover.jpg", "mimetype": "image/jpeg", "NUMBER_OF_FRAMES": "7500"}}`, 2},
		{"a file name alone", `{"index": 1, "codec_type": "video", "codec_name": "mjpeg",
			"tags": {"filename": "cover.jpg"}}`, 2},
		{"marks with another mimetype", `{"index": 1, "codec_type": "video", "codec_name": "h264",
			"tags": {"filename": "cover.jpg", "mimetype": "video/mp4"}}`, 2},
		{"an image as a video stream of its own, not marked", `{"index": 1, "codec_type": "video",
			"codec_name": "mjpeg"}`, 2},
	} {
		probe := doc(`{"format": {"format_name": "matroska,webm"}, "streams": [` + film + `, ` + c.second + `]}`)
		if n := len(filmsOf(probe, true, nil)); n != c.films {
			t.Errorf("%s: %d films; want %d", c.name, n, c.films)
		}
		_, unknown := captionsRule([]Doc{{}}, []Doc{probe}, read(600, every()...))
		if (c.films > 1) != strings.Contains(unknown, "video-streams:2") {
			t.Errorf("%s: captions unknown %q", c.name, unknown)
		}
	}
	// a source record's attached picture: by its own marks, its length by
	// its stored probe's stream of the same index where there is one
	rec := doc(`{"streams": [{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"},
		{"index": 1, "type": "video", "codec": "png", "dispositions": {"attachedPic": true}}]}`)
	if n := len(filmsOf(rec, false, nil)); n != 1 {
		t.Errorf("a record's attached picture: %d films; want 1", n)
	}
	long := doc(`{"streams": [` + film + `, {"index": 1, "codec_type": "video", "codec_name": "png", "nb_frames": "300",
		"disposition": {"attached_pic": 1}}]}`)
	if n := len(filmsOf(rec, false, long)); n != 2 {
		t.Errorf("a record's attached picture its probe counts 300 frames of: %d films; want 2", n)
	}
}

// The closing rule, the analyzer's closed lists: a container, a stream (of
// a kind or a codec off them: a subtitle stream not on SubtitleCodecs, a
// data stream not on DataCodecs or DataTags), more than one film, a film's
// codec, a picture of an analog capture's height, in the record or the
// stored probe, or one the read names (offList), leaves the original's
// closed captions and fields not known, and keeps it, saying which by the
// analyzer's names, whether the captions are read or not.
func TestTheClosingRuleKeepsWhatIsOffItsLists(t *testing.T) {
	const onList = `{"index": 0, "type": "video", "codec": "h264", "fieldOrder": "progressive"}`
	const mp4 = "mov,mp4,m4a,3gp,3g2,mj2"
	for _, c := range []struct {
		name, container, streams, probe string
		offList                         []string
		off                             string // the names, "" none
	}{
		{"on the lists", "matroska,webm", onList, "", nil, ""},
		{"an AVI", "avi", onList, "", nil, ""},
		{"a codec", "matroska,webm", `{"index": 0, "type": "video", "codec": "vc1", "fieldOrder": "progressive"}`, "",
			nil, "codec:vc1"},
		{"AV1", "matroska,webm", `{"index": 0, "type": "video", "codec": "av1", "fieldOrder": "progressive"}`, "", nil, ""},
		{"a container", "flv", onList, "", nil, "container:flv"},
		{"a data stream", "matroska,webm", onList + `, {"index": 1, "type": "data", "codec": "bin_data"}`, "", nil,
			"stream:data:bin_data"},
		{"a data stream on the list", "mpegts", onList + `, {"index": 1, "type": "data", "codec": "scte_35"}`, "", nil, ""},
		// subtitle and attachment streams in the probe alone: in a record
		// they are the track rules' besides
		{"subtitles on the list", "matroska,webm", onList, `{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive"}, {"index": 1, "codec_type": "subtitle", "codec_name": "subrip"},
			{"index": 2, "codec_type": "subtitle", "codec_name": "hdmv_pgs_subtitle"}]}`, nil, ""},
		{"a subtitle stream off the list", "matroska,webm", onList, `{"streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive"}, {"index": 1, "codec_type": "subtitle",
			"codec_name": "arib_caption"}]}`, nil, "stream:subtitle:arib_caption"},
		{"a Matroska caption track, no codec", "matroska,webm", onList, `{"format": {"format_name": "matroska,webm"},
			"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"},
			{"index": 1, "codec_type": "subtitle", "codec_tag_string": "[0][0][0][0]"}]}`, nil, "stream:subtitle:none"},
		{"fonts", "matroska,webm", onList, `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264",
			"field_order": "progressive"}, {"index": 1, "codec_type": "attachment", "codec_name": "ttf",
			"tags": {"filename": "a.ttf", "mimetype": "font/ttf"}}]}`, nil, ""},
		{"two films", "matroska,webm", onList + `, {"index": 1, "type": "video", "codec": "h264"}`, "", nil,
			"video-streams:2"},
		{"an analog capture's lines", "matroska,webm", `{"index": 0, "type": "video", "codec": "mpeg2video",
			"height": 486, "fieldOrder": "progressive"}`, "", nil, "picture:vbi-lines"},
		{"the probe's codec", "matroska,webm", onList, `{"format": {"format_name": "matroska,webm"}, "streams": [{"index": 0,
			"codec_type": "video", "codec_name": "vp9", "field_order": "progressive"}]}`, nil, "codec:vp9"},
		{"a timecode track", mp4, onList + `, {"index": 1, "type": "data", "codec": null}`,
			`{"format": {"format_name": "` + mp4 + `"}, "streams": [{"index": 0, "codec_type": "video",
			"codec_name": "h264", "field_order": "progressive"}, {"index": 1, "codec_type": "data",
			"codec_tag_string": "tmcd"}]}`, nil, ""},
		{"a data stream of no probe", mp4, onList + `, {"index": 1, "type": "data", "codec": null}`,
			"", nil, "stream:data:none"},
		{"the probe's data stream", mp4, onList, `{"format": {"format_name": "` + mp4 + `"},
			"streams": [{"index": 0, "codec_type": "video", "codec_name": "h264", "field_order": "progressive"},
			{"index": 1, "codec_type": "data", "codec_name": "bin_data"}]}`, nil, "stream:data:bin_data"},
		{"the probe's container", "matroska,webm", onList, `{"format": {"format_name": "flv"}, "streams": [{"index": 0,
			"codec_type": "video", "codec_name": "h264", "field_order": "progressive"}]}`, nil, "container:flv"},
		{"the read's", "matroska,webm", onList, "", []string{"codec:vc1"}, "codec:vc1"},
		{"the read's and the record's", "flv", onList, "", []string{"picture:vbi-lines"}, "container:flv, picture:vbi-lines"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := t.TempDir()
			librarytest.WriteSource(t, SourceDir(d, "a"), map[string]any{"sourceId": "a",
				"container": map[string]any{"format": c.container, "durationMs": 600000}, "essence": fullEssence(),
				"streams": mustJSON(t, "["+c.streams+"]")}, nil)
			if c.probe != "" {
				if err := os.WriteFile(filepath.Join(SourceDir(d, "a"), ProbeFile), []byte(c.probe), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			packageOf(t, d, "v-a", verifiedProgressive(), "a")
			f := read(600, every()...)
			f.Captions.OffList = c.offList
			want := closedNotKnown + ": off the closed lists: " + c.off
			for _, facts := range []GateFacts{f, {Captions: f.Captions, CaptionsNotAsked: true}} {
				g, err := ReadGateWith(d, "v-a", facts, "a")
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case c.off == "" && (g.Unknown != "" || g.Captions != ""):
					t.Errorf("captions asked %v: %+v; want it to go on captions", !facts.CaptionsNotAsked, g)
				case c.off != "" && g.Unknown != want:
					t.Errorf("captions asked %v: unknown %q; want %q", !facts.CaptionsNotAsked, g.Unknown, want)
				}
			}
		})
	}
}

// mustJSON is the JSON js decoded.
func mustJSON(t *testing.T, js string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
