package library

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
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
// [0][0][0][0]), its height, and whether it is a cover (attachedPicture).
type streamView struct {
	doc              Doc
	kind, codec, tag string
	height           float64
	cover            bool
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
		var c coverMarks
		if probe {
			v.kind, v.codec = str(st, "codec_type"), str(st, "codec_name")
			c.pic, c.codec = markedAt(st, "disposition", "attached_pic"), valueOf(st.Get("codec_name"))
		} else {
			v.kind, v.codec = str(st, "type"), str(st, "codec")
			c.pic, c.codec = markedAt(st, "dispositions", "attachedPic"), valueOf(st.Get("codec"))
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
		c.tags, c.clash = rawTagsOf(st)
		if src != nil {
			v.tag = norm(str(src, "codec_tag_string"))
			// the probe's own tags of the stream, which clash as they clash
			// there
			tags, clash := rawTagsOf(src)
			for k, x := range tags {
				if _, ok := c.tags[k]; !ok {
					c.tags[k] = x
				}
			}
			c.clash = c.clash || clash
			c.frames, c.hasFrames = src.Get("nb_frames")
		}
		if strings.HasPrefix(v.tag, "[") {
			v.tag = ""
		}
		// a record keeps an attachment's filename and mimetype of its own
		if !probe {
			for _, k := range []string{"filename", "mimetype"} {
				if x, ok := st.Get(k); ok {
					if _, had := c.tags[k]; !had {
						c.tags[k] = x
					}
				}
			}
		}
		v.cover = attachedPicture(c)
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

// rawTagsOf are the stream st's tags by their names in lower case (pyLower),
// their values as given (a later name of the same case-folded name over an
// earlier), as the record logic's attached_picture reads them; none where
// its tags are no object. clash says two names that differ only by case
// give values Python takes for unequal (pyEqual): such tags are there, and
// make the stream no image, so no cover.
func rawTagsOf(st Doc) (tags map[string]any, clash bool) {
	tags = map[string]any{}
	td, ok := valueOf(st.Get("tags")).(Doc)
	if !ok {
		return tags, false
	}
	for _, f := range td {
		tags[pyLower(f.Key)] = f.Value
	}
	for _, f := range td {
		if !pyEqual(tags[pyLower(f.Key)], f.Value) {
			clash = true
		}
	}
	return tags, clash
}

// pyLower is s as Python's str.lower() writes it: Go's lower case but for
// the capital I with a dot above, which Python lowers to an i and a
// combining dot (U+0130: so its "IMAGE/png" is no image/*).
func pyLower(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "\u0130", "i\u0307"))
}

// pyEqual reports whether Python takes the values a and b, as JSON gives
// them, for equal: numbers (a boolean among them, 1 or 0) by their value, an
// int and a float alike; texts by their characters; lists item by item;
// objects key by key; null to null alone.
func pyEqual(a, b any) bool {
	if x, ok := pyExact(a); ok {
		y, ok := pyExact(b)
		return ok && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case Doc:
		y, ok := b.(Doc)
		if !ok {
			return false
		}
		dx, dy := map[string]any{}, map[string]any{}
		for _, f := range x {
			dx[f.Key] = f.Value
		}
		for _, f := range y {
			dy[f.Key] = f.Value
		}
		if len(dx) != len(dy) {
			return false
		}
		for k, v := range dx {
			w, has := dy[k]
			if !has || !pyEqual(v, w) {
				return false
			}
		}
		return true
	}
	return false
}

// pyExact is the value of the number v (a boolean 1 or 0) as Python holds
// it: an int's exactly, a float's exactly as its double; false of no number.
func pyExact(v any) (*big.Rat, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewRat(1, 1), true
		}
		return new(big.Rat), true
	case json.Number:
		if !strings.ContainsAny(x.String(), ".eE") {
			return new(big.Rat).SetString(x.String())
		}
		f, err := x.Float64()
		if err != nil {
			return nil, false
		}
		return new(big.Rat).SetFloat64(f), true
	case int:
		return new(big.Rat).SetInt64(int64(x)), true
	case int64:
		return new(big.Rat).SetInt64(x), true
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return new(big.Rat).SetFloat64(x), true
	}
	return nil, false
}

// coverMarks are what the cover rule (attachedPicture) reads of a stream:
// its attached picture disposition (pic: coverMark), its codec name as
// given, its tags (rawTagsOf), whether two of them clash (clash), and its
// frames counted (nb_frames, hasFrames: given).
type coverMarks struct {
	pic       bool
	codec     any
	tags      map[string]any
	clash     bool
	frames    any
	hasFrames bool
}

// gateDigits is a count as the record logic's gate_digits takes one a probe
// states - a whole JSON number from 0, or the same written in digits alone
// (0-9, of no more digits than Python reads: PyIntDigits) -, and whether it
// is one: "1e3", " 900", "+900", "1.0", 9.0, a boolean, null and a list are
// no counts.
func gateDigits(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case json.Number:
		// a float's text ("1.0", "1e3") is no run of digits
		n, ok := pyDigits(strings.TrimPrefix(x.String(), "-"))
		if !ok || strings.HasPrefix(x.String(), "-") && n.Sign() != 0 {
			return nil, false
		}
		return n, true
	case int:
		return big.NewInt(int64(x)), x >= 0
	case int64:
		return big.NewInt(x), x >= 0
	case string:
		if x == "" || strings.Trim(x, "0123456789") != "" {
			return nil, false
		}
		return pyDigits(x)
	}
	return nil, false
}

// pyStr is v as Python's str() writes a value JSON gives: text as it is,
// null None, a boolean True or False, a number as Python writes an int or a
// float; anything else as no text a rule reads.
func pyStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	if n, ok := pyNumber(v); ok {
		switch y := n.(type) {
		case int64:
			return strconv.FormatInt(y, 10)
		case float64:
			if f, err := pyFloat(y); err == nil {
				return f
			}
		}
	}
	return "<object>"
}

// attachedPicture reports whether a video stream of the marks c is a
// cover, a still picture the container holds as an attached file, as the
// record logic's attached_picture (schemas, libv2_records.py) reads one -
// all of these: marked as one (the attached_pic disposition, or both a
// filename and a mimetype tag, an empty one present too); an image of a
// cover's codec (CoverCodecs, its name as given), its mimetype, where it
// has one (trimmed and in lower case; an empty one, or one that is no text,
// is no image/*), image/*, and without one the attached_pic disposition (an
// MP4 cover), of tags no two of which clash (rawTagsOf); and of no length of
// its own: every count of its frames it is given (nb_frames, and every tag
// whose name begins with number_of_frames) a count in digits alone
// (gateDigits) of at most one - a count that is there but can't be read
// makes it no cover - and no tag whose name begins with duration (in any
// case). The disposition marks it only as the number 1 or true (coverMark).
// The duration ffprobe gives a stream is the file's where the container
// gives none, so it is no mark. A stream of any other codec - a webp image
// among them - is a video stream, whatever its marks say.
func attachedPicture(c coverMarks) bool {
	_, filename := c.tags["filename"]
	mimeTag, mimetype := c.tags["mimetype"]
	marked := c.pic || (filename && mimetype)
	codec, _ := c.codec.(string)
	image := !c.clash && slices.Contains(CoverCodecs, codec)
	if mimetype {
		// a value that is no text (null, a number) is none that begins
		// image/, as Python's str(tag or "") isn't
		mime := pyLower(pyStrip(pyStr(mimeTag)))
		image = image && strings.HasPrefix(mime, "image/")
	} else {
		image = image && c.pic
	}
	// the counts it gives: nb_frames, and every tag whose name begins with
	// number_of_frames (NUMBER_OF_FRAMES-eng); one that is there but can't
	// be read makes it no cover
	counts := []any{}
	if c.hasFrames {
		counts = append(counts, c.frames)
	}
	for k, x := range c.tags {
		if strings.HasPrefix(k, "number_of_frames") {
			counts = append(counts, x)
		}
	}
	for _, v := range counts {
		if n, ok := gateDigits(v); !ok || n.Cmp(big.NewInt(1)) > 0 {
			return false
		}
	}
	for k := range c.tags {
		if strings.HasPrefix(k, "duration") {
			return false
		}
	}
	return marked && image
}

// coverMark reports whether the disposition value v marks a stream as an
// attached picture: the number 1 or true, nothing else ("1", "0", 2, 1.0
// none).
func coverMark(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	n, ok := pyNumber(v)
	i, whole := n.(int64)
	return ok && whole && i == 1
}

// markedAt is coverMark of the stream st's dispositions under the key list,
// its flag key.
func markedAt(st Doc, list, key string) bool {
	d, _ := valueOf(st.Get(list)).(Doc)
	return coverMark(valueOf(d.Get(key)))
}

// probeCover is attachedPicture of the probe's stream st.
func probeCover(st Doc) bool {
	c := coverMarks{pic: markedAt(st, "disposition", "attached_pic"), codec: valueOf(st.Get("codec_name"))}
	c.tags, c.clash = rawTagsOf(st)
	c.frames, c.hasFrames = st.Get("nb_frames")
	return attachedPicture(c)
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

// isCover reports whether the video stream v is a cover (attachedPicture):
// a video stream of an image not so marked is a video stream like any
// other.
func isCover(v streamView) bool { return v.cover }

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
