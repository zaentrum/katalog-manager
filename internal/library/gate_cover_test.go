package library

import (
	"strings"
	"testing"
)

// A cover is the record logic's attached_picture, line for line: marked as
// an attached file (the attached_pic disposition, or both a filename and a
// mimetype tag, an empty one present too, never one alone), of a cover's
// codec (mjpeg, png, bmp, its name as given) always, its mimetype where it
// has one image/* (trimmed, in lower case; an empty one is no image/*, and
// the disposition stands in only where it has none), of at most one frame
// (nb_frames and every tag whose name begins with NUMBER_OF_FRAMES, each a
// count in digits alone; one that is there and can't be read - " 5",
// "1e3" as text, "2.0", a boolean, null - makes it no cover) and of no DURATION
// tag (any name that begins with duration, in any case); the stream's own
// duration, which ffprobe gives a stream from its file, is no mark. The
// disposition marks it only as the number 1 or true.
func TestACoverIsTheRecordLogicsAttachedPicture(t *testing.T) {
	for _, c := range []struct {
		name, stream string
		cover        bool
	}{
		{"attached_pic, mjpeg", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}`, true},
		{"attached_pic, png, a mimetype spaced and in capitals", `"codec_name": "png", "disposition": {"attached_pic": 1},
			"tags": {"mimetype": " IMAGE/PNG "}`, true},
		{"tags alone, bmp", `"codec_name": "bmp", "tags": {"filename": "cover.bmp", "mimetype": "image/bmp"}`, true},
		{"tags in capitals", `"codec_name": "mjpeg", "tags": {"FILENAME": "c.jpg", "MimeType": "image/jpeg"}`, true},
		{"an empty filename tag, present", `"codec_name": "mjpeg", "tags": {"filename": "", "mimetype": "image/jpeg"}`, true},
		{"an empty mimetype tag, no disposition", `"codec_name": "mjpeg", "tags": {"filename": "c.jpg", "mimetype": ""}`,
			false},
		{"an empty mimetype tag, the disposition", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"filename": "c.jpg", "mimetype": "  "}`, false},
		{"a filename alone", `"codec_name": "mjpeg", "tags": {"filename": "c.jpg"}`, false},
		{"a mimetype alone", `"codec_name": "mjpeg", "tags": {"mimetype": "image/jpeg"}`, false},
		{"no marks", `"codec_name": "mjpeg"`, false},
		{"webp, tagged an image", `"codec_name": "webp", "tags": {"filename": "c.webp", "mimetype": "image/webp"}`, false},
		{"h264, tagged an image", `"codec_name": "h264", "tags": {"filename": "c.jpg", "mimetype": "image/jpeg"}`, false},
		{"png, tagged a video", `"codec_name": "png", "tags": {"filename": "c.png", "mimetype": "video/mp4"}`, false},
		{"MJPEG in capitals", `"codec_name": "MJPEG", "disposition": {"attached_pic": 1}`, false},
		{"attached_pic \"0\", no mark", `"codec_name": "mjpeg", "disposition": {"attached_pic": "0"}`, false},
		{"attached_pic \"1\", no mark", `"codec_name": "mjpeg", "disposition": {"attached_pic": "1"}`, false},
		{"attached_pic 2, no mark", `"codec_name": "mjpeg", "disposition": {"attached_pic": 2}`, false},
		{"attached_pic 1.0, no mark", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1.0}`, false},
		{"attached_pic true", `"codec_name": "mjpeg", "disposition": {"attached_pic": true}`, true},
		{"attached_pic 0, no tags", `"codec_name": "mjpeg", "disposition": {"attached_pic": 0}`, false},
		{"nb_frames 1", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "1"`, true},
		{"nb_frames 2", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "2"`, false},
		{"nb_frames 2.0", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "2.0"`, false},
		{"nb_frames 2.0, a number", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": 2.0`, false},
		{"nb_frames 1e3, a number", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": 1e3`, false},
		{"nb_frames \" 5\", a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": " 5"`, false},
		{"nb_frames \"1e3\", a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": "1e3"`, false},
		{"nb_frames true, a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": true`, false},
		{"nb_frames null, a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": null`, false},
		{"nb_frames -1, a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": "-1"`, false},
		{"nb_frames inf, a count that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"nb_frames": "inf"`, false},
		{"NUMBER_OF_FRAMES-eng 2", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"NUMBER_OF_FRAMES-eng": "2"}`, false},
		{"NUMBER_OF_FRAMES-eng 1", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"NUMBER_OF_FRAMES-eng": "1"}`, true},
		{"NUMBER_OF_FRAMES-eng that can't be read", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"NUMBER_OF_FRAMES-eng": "one"}`, false},
		{"nb_frames 1, NUMBER_OF_FRAMES 2", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "1",
			"tags": {"NUMBER_OF_FRAMES": "2"}`, false},
		{"nb_frames 3, NUMBER_OF_FRAMES 1", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "nb_frames": "3",
			"tags": {"NUMBER_OF_FRAMES": "1"}`, false},
		{"a DURATION-eng tag", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"DURATION-eng": "00:00:00.040000000"}`, false},
		{"a duration tag in lower case", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1},
			"tags": {"duration": "00:00:00.040000000"}`, false},
		{"a duration of its own", `"codec_name": "mjpeg", "disposition": {"attached_pic": 1}, "duration": "600.000000"`,
			true},
	} {
		st := doc(t, `{"index": 0, "codec_type": "video", `+c.stream+`}`)
		if got := probeCover(st); got != c.cover {
			t.Errorf("%s: a cover %v; want %v", c.name, got, c.cover)
		}
		// the closing rule reads its probe's streams by the same rule
		if vs := viewsOf(doc(t, `{"streams": [{"index": 0, "codec_type": "video", `+c.stream+`}]}`), true, nil); len(vs) != 1 ||
			isCover(vs[0]) != c.cover {
			t.Errorf("%s: the closing rule's view a cover %v; want %v", c.name, !c.cover, c.cover)
		}
	}
}

// coverProbes are stored probes of a film (h264, at 0, 24 frames a second)
// and another video stream before it: a cover marked by its tags alone,
// which is passed over (the film has a plan), and a webp image so tagged,
// which is no cover - a second video stream, so no plan and off the lists.
const (
	coverFirstProbe = `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "mjpeg", "disposition":
		{"attached_pic": 0}, "tags": {"filename": "cover.jpg", "mimetype": "image/jpeg"}}, {"index": 1, "codec_type":
		"video", "codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "24/1"}]}`
	webpFirstProbe = `{"streams": [{"index": 0, "codec_type": "video", "codec_name": "webp", "disposition":
		{"attached_pic": 0}, "tags": {"filename": "cover.webp", "mimetype": "image/webp"}}, {"index": 1, "codec_type":
		"video", "codec_name": "h264", "field_order": "progressive", "start_time": "0.000000", "avg_frame_rate": "24/1"}]}`
)

// At the gate: a cover before the film is passed over, and the film has a
// plan; a webp image so tagged is a second video stream: no plan, and the
// closing rule holds the original (two video streams).
func TestAWebpImageIsASecondVideoStream(t *testing.T) {
	if s, why := PictureSpan(film(600), doc(t, coverFirstProbe)); s == nil || s.Start != 0 || s.End != 600 {
		t.Errorf("a tagged cover before the film: %+v (%s); want the film's plan", s, why)
	}
	if s, why := PictureSpan(film(600), doc(t, webpFirstProbe)); s != nil || !strings.Contains(why, "2 video streams") {
		t.Errorf("a webp image before the film: %+v (%s); want no plan", s, why)
	}
	_, unknown := captionsRule([]Doc{film(600)}, []Doc{doc(t, webpFirstProbe)}, read(600, every()...))
	if !strings.HasPrefix(unknown, closedNotKnown) || !strings.Contains(unknown, "video-streams:2") {
		t.Errorf("a webp image before the film: unknown %q; want two video streams held", unknown)
	}
}

// On the retire path: a tagged cover before the film goes (the film has a
// plan, and the record's picture is the film: its record marks the cover an
// attached picture, as the record logic writes it); a webp image so tagged
// keeps the original, two video streams.
func TestTheRetireJobTakesAWebpImageForAVideoStream(t *testing.T) {
	streams := func(codec string, attached bool) func(rec map[string]any) {
		return func(rec map[string]any) {
			rec["streams"] = []any{map[string]any{"index": 0, "type": "video", "codec": codec,
				"dispositions": map[string]any{"attachedPic": attached}},
				map[string]any{"index": 1, "type": "video", "codec": "h264", "fieldOrder": "progressive"}}
		}
	}
	for _, c := range []struct {
		name, probe, kept string
		record            func(map[string]any)
	}{
		{"a tagged cover before the film", coverFirstProbe, "", streams("mjpeg", true)},
		{"a webp image before the film", webpFirstProbe, "video-streams:2", streams("webp", false)},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixtureOf(t, sameEssence, sameEssence)
			probe := c.probe
			f.rewriteRecord(t, c.record, &probe)
			if c.kept == "" {
				f.deletedBy(t, c.name)
				return
			}
			f.keptFor(t, c.name, c.kept)
		})
	}
}
