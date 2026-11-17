package library

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The closing rule: the ways closed captions and fields are carried that the
// reads were made for and tested with. They are the analyzer's closed lists
// (its README, "The closed lists", and pipelines/captions.py: CONTAINERS,
// VIDEO_CODECS, CAPTION_CODECS, CAPTION_TAGS, SUBTITLE_CODECS, DATA_CODECS,
// DATA_TAGS, COVER_CODECS, VBI_HEIGHTS), by the same names, and the deletion
// waves' check holds the same; this is the one place katalog-manager holds
// them, and a name added to one side is added to the others in the same
// release. An original whose record or stored probe has anything off them,
// or whose read names it (the analyzer's offList), has closed captions and
// fields not known: it is kept, as no read says what such a file carries.
var (
	// CaptionContainers are the containers (ffprobe's format_name, as a
	// source record's container.format keeps it) whose streams the header
	// probe lists all of, caption tracks too.
	CaptionContainers = []string{"matroska,webm", "mov,mp4,m4a,3gp,3g2,mj2", "mpegts", "mpeg", "avi"}
	// CaptionCodecs are the video codecs whose decoders in ffprobe 7.1 export
	// the closed captions a picture carries (A53 side data: H.264 and HEVC
	// SEI, MPEG-1 and MPEG-2 picture user data, AV1 metadata).
	CaptionCodecs = []string{"h264", "hevc", "mpeg1video", "mpeg2video", "av1"}
	// CaptionStreamCodecs and CaptionStreamTags are the streams of closed
	// captions of their own, of any kind (a QuickTime or MP4 track of them):
	// the original carries them.
	CaptionStreamCodecs = []string{"eia_608"}
	CaptionStreamTags   = []string{"c608", "c708"}
	// SubtitleCodecs are the subtitle streams the subtitle rule holds: they
	// are no closed captions. Any other subtitle stream is off the list.
	SubtitleCodecs = []string{"subrip", "srt", "ass", "ssa", "webvtt", "mov_text", "text", "microdvd",
		"hdmv_pgs_subtitle", "dvd_subtitle", "dvb_subtitle", "dvb_teletext"}
	// DataCodecs and DataTags are the data streams that carry no captions:
	// splice markers, ID3, an EPG, and a QuickTime timecode track, of no
	// codec ffprobe names (its tag, tmcd). Any other data stream is off the
	// list.
	DataCodecs = []string{"scte_35", "timed_id3", "epg"}
	DataTags   = []string{"tmcd"}
	// ScannedCodecs are the video codecs whose captions a read of windows
	// may not see (HEVC's suffix SEI, which ffprobe 7.1's decoder exports
	// none of; the analyzer reads the bytes of each window's packets, never
	// of all of them): captions known as none need a read of every packet of
	// the picture (the record's captionSignature false).
	ScannedCodecs = []string{"hevc"}
	// CoverCodecs are the codecs an MP4's cover art comes as, which has no
	// mimetype (isCover).
	CoverCodecs = []string{"mjpeg", "png", "bmp"}
	// VBIHeights are the picture heights that keep an analog capture's
	// vertical blanking lines, line 21's captions among them, in the
	// picture: they are not read.
	VBIHeights = []float64{486, 512, 608}
)

// Audio and attachment streams are on the list whatever their codec.

// closedNotKnown begins why the gate keeps an original the closing rule
// leaves its closed captions and fields not known of.
const closedNotKnown = "closed captions and fields not known"

// streamView is a stream of a source record or of a stored probe, in what
// the closing rule reads of it: its kind, codec and codec tag (codec "" for
// none, ffprobe's unknown too; a tag "" for none or one that is no name,
// [0][0][0][0]), its height, the container's marks of an attached file (the
// attached picture disposition, a Matroska attachment's filename and
// mimetype) and whether it has a length of its own (more than one frame
// counted, or a DURATION tag).
type streamView struct {
	doc                Doc
	kind, codec, tag   string
	height             float64
	pic                bool
	filename, mimetype string
	length             bool
}

// viewsOf are the streams of the record d: a probe's (probe, ffprobe's
// names) or a source record's, whose stream of an index names what the
// record doesn't keep (the codec tag, the frames counted, the tags) where
// the stored probe aligned, of the same source, has that stream.
func viewsOf(d Doc, probe bool, aligned Doc) []streamView {
	streams, _ := listOf(d, "streams")
	byIndex := map[string]Doc{}
	if aligned != nil {
		ps, _ := listOf(aligned, "streams")
		for _, e := range ps {
			if p, ok := e.(Doc); ok {
				if i, ok := p.Get("index"); ok {
					byIndex[fmt.Sprint(i)] = p
				}
			}
		}
	}
	var out []streamView
	for _, e := range streams {
		st, ok := e.(Doc)
		if !ok {
			continue
		}
		v := streamView{doc: st}
		src := st // where ffprobe's own names are
		if probe {
			v.kind, v.codec = str(st, "codec_type"), str(st, "codec_name")
			v.pic = flag(st, "disposition", "attached_pic")
		} else {
			v.kind, v.codec = str(st, "type"), str(st, "codec")
			v.pic = flag(st, "dispositions", "attachedPic")
			src = nil
			if i, ok := st.Get("index"); ok {
				src = byIndex[fmt.Sprint(i)]
			}
		}
		v.kind, v.codec = norm(v.kind), norm(v.codec)
		if v.codec == "unknown" {
			v.codec = ""
		}
		if h, ok := counted(st.Get("height")); ok {
			v.height = h
		}
		tags := tagsOf(st)
		if src != nil {
			v.tag = norm(str(src, "codec_tag_string"))
			for k, x := range tagsOf(src) {
				if _, ok := tags[k]; !ok {
					tags[k] = x
				}
			}
			if n, ok := counted(src.Get("nb_frames")); ok && n > 1 {
				v.length = true
			}
		}
		if strings.HasPrefix(v.tag, "[") {
			v.tag = ""
		}
		// a record keeps an attachment's filename and mimetype of its own
		for _, k := range []string{"filename", "mimetype"} {
			if x := strings.TrimSpace(str(st, k)); x != "" {
				if _, ok := tags[k]; !ok {
					tags[k] = x
				}
			}
		}
		v.filename, v.mimetype = tags["filename"], strings.ToLower(tags["mimetype"])
		if x, ok := tags["number_of_frames"]; ok {
			if n, ok := counted(x, true); ok && n > 1 {
				v.length = true
			}
		}
		for k := range tags {
			if strings.HasPrefix(k, "duration") {
				v.length = true
			}
		}
		out = append(out, v)
	}
	return out
}

// counted is the number v (as ffprobe writes a count: a string, "1"), and
// whether it is one; present says it was there.
func counted(v any, present bool) (float64, bool) {
	if !present {
		return 0, false
	}
	if s, ok := v.(string); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f, err == nil
	}
	return toFloat(v)
}

// norm is v as the lists name it: trimmed, lower case.
func norm(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// flag reports whether the stream st's dispositions (under the key list)
// set the flag key.
func flag(st Doc, list, key string) bool {
	d, _ := st.Get(list)
	dd, ok := d.(Doc)
	if !ok {
		return false
	}
	v, _ := dd.Get(key)
	return truthy(v)
}

// tagsOf are the stream st's tags by their names in lower case.
func tagsOf(st Doc) map[string]string {
	out := map[string]string{}
	t, _ := st.Get("tags")
	if td, ok := t.(Doc); ok {
		for _, f := range td {
			if s, ok := f.Value.(string); ok {
				out[strings.ToLower(f.Key)] = strings.TrimSpace(s)
			} else if f.Value != nil {
				out[strings.ToLower(f.Key)] = fmt.Sprint(f.Value)
			}
		}
	}
	return out
}

// isCover reports whether the video stream v is an attached picture, a
// still picture and no film, whatever its file's name: the container marks
// it as an attached file (the attached picture disposition, or a Matroska
// attachment's filename and mimetype), it is an image (its mimetype
// image/*; an MP4's cover, which has none, of a codec of CoverCodecs) and it
// has no length of its own. A video stream of an image not so marked is a
// video stream like any other.
func isCover(v streamView) bool {
	marked := v.pic || (v.filename != "" && v.mimetype != "")
	image := v.pic && slices.Contains(CoverCodecs, v.codec)
	if v.mimetype != "" {
		image = strings.HasPrefix(v.mimetype, "image/")
	}
	return marked && image && !v.length
}

// closedOf holds the streams vs of a record to the closed lists, as the
// analyzer holds its header probe: what of them is off the lists
// ("stream:<kind>:<codec, else tag, else none>", "video-streams:<n>",
// "codec:<name>", "picture:vbi-lines"), its films (the video streams that
// are no attached picture) and its streams of captions of their own.
func closedOf(vs []streamView) (off []string, films []streamView, captioned []streamView) {
	for _, v := range vs {
		what := v.codec
		if what == "" {
			what = v.tag
		}
		if what == "" {
			what = "none"
		}
		kind := v.kind
		if kind == "" {
			kind = "none"
		}
		switch {
		case slices.Contains(CaptionStreamCodecs, v.codec) || slices.Contains(CaptionStreamTags, v.tag):
			captioned = append(captioned, v)
		case kind == "video":
			if !isCover(v) {
				films = append(films, v)
			}
		case kind == "audio" || kind == "attachment":
		case kind == "subtitle" && slices.Contains(SubtitleCodecs, v.codec):
		case kind == "data" && (slices.Contains(DataCodecs, v.codec) || slices.Contains(DataTags, v.tag)):
		default:
			off = append(off, "stream:"+kind+":"+what)
		}
	}
	if len(films) > 1 {
		off = append(off, fmt.Sprintf("video-streams:%d", len(films)))
	}
	for _, f := range films {
		// a record that names no codec says nothing of it
		if f.codec != "" && !slices.Contains(CaptionCodecs, f.codec) {
			off = append(off, "codec:"+f.codec)
		}
	}
	for _, f := range films {
		if slices.Contains(VBIHeights, f.height) {
			off = append(off, "picture:vbi-lines")
			break
		}
	}
	return off, films, captioned
}

// offListOf are the names, of the closing rule's lists, that the records of
// the sources (their source records, and their stored probes where there
// are ones: probes[i], nil for none, is records[i]'s) and their reads (said,
// the analyzer's offList as it names them) have off them, each once,
// sorted: "container:avi"... A record or a probe that says nothing of an
// axis (no container, no codec) has nothing off it there: what the file has
// the analyzer's header probe lists.
func offListOf(records, probes []Doc, said []string) []string {
	seen := map[string]bool{}
	containerOf := func(d Doc, key, name string) {
		c, _ := d.Get(key)
		cd, ok := c.(Doc)
		if !ok {
			return
		}
		if f := norm(str(cd, name)); f != "" && !slices.Contains(CaptionContainers, f) {
			seen["container:"+f] = true
		}
	}
	add := func(names []string) {
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				seen[n] = true
			}
		}
	}
	for i, rec := range records {
		var probe Doc
		if i < len(probes) {
			probe = probes[i]
		}
		containerOf(rec, "container", "format")
		off, _, _ := closedOf(viewsOf(rec, false, probe))
		add(off)
	}
	for _, probe := range probes {
		if probe == nil {
			continue
		}
		containerOf(probe, "format", "format_name")
		off, _, _ := closedOf(viewsOf(probe, true, nil))
		add(off)
	}
	add(said)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// offListWhy says of the names off the closing rule's lists why the gate
// keeps the original, by the analyzer's names: "closed captions and fields
// not known: off the closed lists: codec:vc1".
func offListWhy(names []string) string {
	return closedNotKnown + ": off the closed lists: " + strings.Join(names, ", ")
}

// filmsOf are the films of the record d (a probe's, probe, or a source
// record's, its stored probe aligned): its video streams that are no
// attached picture (isCover).
func filmsOf(d Doc, probe bool, aligned Doc) []Doc {
	_, films, _ := closedOf(viewsOf(d, probe, aligned))
	out := make([]Doc, len(films))
	for i, f := range films {
		out[i] = f.doc
	}
	return out
}
