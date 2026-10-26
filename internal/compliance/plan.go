package compliance

// What today's transcoder makes of a title's original, as its planner
// (decision.py, plan_renditions) makes it from what the transcoder reports
// of itself: the rungs of its ladder, which of them copy the original's
// video, and the maxrate it encodes the others at. The check reads it to
// tell whether processing a title again would copy its video as it is,
// whatever the caps, and to estimate the package that processing makes.

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ladderRung is a rung of the transcoder's ladder, as its report gives it
// (ladderRungs) or its spec reads (ladder): its name ("source" or "720p"),
// its codec, whether the spec names the codec, and the maxrate it sets
// (bit/s; none: the planner's).
type ladderRung struct {
	Name       string   `json:"name"`
	Codec      string   `json:"codec"`
	CodecNamed bool     `json:"codecNamed"`
	MaxrateBps *float64 `json:"maxrateBps"`
}

// height is the height of the rung's box, 0 for the original's size.
func (g ladderRung) height() int {
	if g.Name == "source" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSuffix(g.Name, "p"))
	return n
}

// The planner's grammar of a ladder (decision.py, parse_ladder): rungs by
// comma, each "source" or a height ("720p"), then a codec or a maxrate
// ("3M", "1500k").
var (
	rungNameRE = regexp.MustCompile(`^(source|[0-9]{3,4}p)$`)
	rungRateRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([km])$`)
)

// rungs are the transcoder's ladder: the rungs its report gives, else its
// spec as the planner reads it ("source" at the original's size in HEVC, a
// height scaled in H.264, unless a rung names its codec; the empty spec is
// "source"); false when it reports neither, or a spec the planner refuses.
func (t *TranscoderRules) rungs() ([]ladderRung, bool) {
	if len(t.LadderRungs) > 0 {
		return t.LadderRungs, true
	}
	if t.Ladder == nil {
		return nil, false
	}
	source := []ladderRung{{Name: "source", Codec: "hevc"}}
	var out []ladderRung
	for _, tok := range strings.Split(strings.ToLower(*t.Ladder), ",") {
		parts := strings.Split(strings.TrimSpace(tok), ":")
		name := strings.TrimSpace(parts[0])
		if name == "" && len(parts) == 1 {
			continue
		}
		if !rungNameRE.MatchString(name) {
			return nil, false
		}
		g := ladderRung{Name: name, Codec: "h264"}
		if name == "source" {
			g.Codec = "hevc"
		}
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if p == "hevc" || p == "h264" {
				g.Codec, g.CodecNamed = p, true
				continue
			}
			m := rungRateRE.FindStringSubmatch(p)
			if m == nil {
				return nil, false
			}
			v, _ := strconv.ParseFloat(m[1], 64)
			if m[2] == "m" {
				v *= 1e6
			} else {
				v *= 1e3
			}
			g.MaxrateBps = &v
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return source, true
	}
	return out, true
}

// sourceRung is the ladder's rung at the original's size ("source"), and
// false when it has none.
func (t *TranscoderRules) sourceRung() (ladderRung, bool) {
	rungs, ok := t.rungs()
	if !ok {
		return ladderRung{}, false
	}
	for _, g := range rungs {
		if g.Name == "source" {
			return g, true
		}
	}
	return ladderRung{}, false
}

// encoderIsCPU reports whether the transcoder's HEVC encoder is libx265.
func (t *TranscoderRules) encoderIsCPU() bool {
	return t.Encoder.Codec != nil && strings.EqualFold(strings.TrimSpace(*t.Encoder.Codec), "libx265")
}

// copiesH264 reports whether the ladder's rung at the original's size keeps
// a browser-friendly H.264 original as it is, whatever its bit rate or its
// size (decision.py, plan_renditions rules 2 and 3): an H.264 rung does,
// and so does an HEVC one whose codec the spec leaves to the default on a
// CPU transcoder (libx265), unless the ladder names it (source:hevc).
func (t *TranscoderRules) copiesH264() bool {
	g, ok := t.sourceRung()
	if !ok {
		return false
	}
	return g.Codec == "h264" || (g.Codec == "hevc" && !g.CodecNamed && t.encoderIsCPU())
}

// tableMaxrate is the maxrate the planner gives a rung of height and codec
// the ladder sets none for, bit/s (decision.py, _table_maxrate: the row of
// the next height up, the last one above it), from the transcoder's report
// (rungMaxrateMbps); false when it reports none.
func (t *TranscoderRules) tableMaxrate(height int, codec string) (float64, bool) {
	if len(t.RungMaxrateMbps) == 0 {
		return 0, false
	}
	var rows []int
	for h := range t.RungMaxrateMbps {
		if n, err := strconv.Atoi(h); err == nil {
			rows = append(rows, n)
		}
	}
	if len(rows) == 0 {
		return 0, false
	}
	sort.Ints(rows)
	row := rows[len(rows)-1]
	for _, h := range rows {
		if height <= h {
			row = h
			break
		}
	}
	v, ok := t.RungMaxrateMbps[strconv.Itoa(row)][codec]
	return v * 1e6, ok && v > 0
}

// sourceMaxrate is the maxrate of an encode at the original's size, bit/s
// (decision.py, _source_maxrate): HEVC at the override of its bucket, else
// the cap of its kind and bucket; H.264 at the table's rate of its height.
// False when that is not known (an HEVC cap switched off, whose fallback is
// the transcoder's own).
func (t *TranscoderRules) sourceMaxrate(kind string, bucket, height int, codec string) (float64, bool) {
	if codec != "hevc" {
		return t.tableMaxrate(height, codec)
	}
	m, ok := t.maxrateOf(kind, bucket)
	return m * 1e6, ok
}

// limitOf is the bit rate a package's video of kind in bucket may run at,
// Mbit/s, while the cap of its kind and bucket holds (false when it reports
// none, or one of 0, which allows any): the transcoder's override of the
// bucket, at which it encodes whatever the cap (NVENC_MAXRATE_*), else the
// cap.
func (t *TranscoderRules) limitOf(kind string, bucket int) (float64, bool) {
	if c, ok := t.capOf(kind, bucket); !ok || c <= 0 {
		return 0, false
	}
	return t.maxrateOf(kind, bucket)
}

// original is what the planner reads of a title's original video
// (decision.py, SourceInfo): its codec, its profile and its pixel format
// (ffprobe's names, lower case), its bits per sample, its size, its video's
// own bit rate (bit/s, 0 unknown), its Dolby Vision profile ("" none), and
// its file's size (bytes, 0 unknown).
type original struct {
	codec, profile, pixFmt string
	bitDepth               float64
	width, height          float64
	rate                   float64
	dv                     string
	sizeBytes              float64
}

// originalOf is what the records and the catalog say of the title's
// original video, each fact from the first that tells it: its record's first
// video stream that is no cover art; the package's top rendition, where that
// is the original's stream copied (its measured bit rate is the stream's,
// and a copied H.264 one was browser-friendly, or the transcoder had not
// copied it); the catalog's probe of its file, its bit rate the file's
// (which counts its audio, and errs high).
func (t title) originalOf(r records, copied tri, dur float64) original {
	var o original
	if r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type != "video" || s.Dispositions.AttachedPic {
				continue
			}
			o.codec, o.profile, o.pixFmt = lowerOf(s.Codec), lowerOf(s.Profile), lowerOf(s.PixelFormat)
			if s.BitDepth != nil {
				o.bitDepth = *s.BitDepth
			}
			if s.Width != nil && s.Height != nil {
				o.width, o.height = *s.Width, *s.Height
			}
			if s.Bitrate != nil && *s.Bitrate > 0 {
				o.rate = *s.Bitrate
			}
			if dv := s.Hdr.DolbyVision; dv != nil && dv.Profile != nil {
				o.dv = strconv.Itoa(int(*dv.Profile))
			}
			break
		}
	}
	if copied == yes && r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		v := r.pkg.Renditions.Video[0]
		if o.codec == "" {
			switch c := strings.ToLower(v.Codec); {
			case isH264(c):
				o.codec, o.pixFmt = "h264", "yuv420p"
			case isHEVC(c):
				o.codec, o.profile = "hevc", "main"
				if strings.HasPrefix(c[4:], ".2") {
					o.profile = "main 10"
				}
			}
		}
		if (o.width == 0 || o.height == 0) && v.Width != nil && v.Height != nil {
			o.width, o.height = *v.Width, *v.Height
		}
		if o.rate == 0 && v.BitrateBps != nil && *v.BitrateBps > 0 {
			o.rate = *v.BitrateBps
		}
	}
	if o.codec == "" {
		o.codec = lowerOf(t.FileCodec)
	}
	if (o.width == 0 || o.height == 0) && t.FileResolution != nil {
		o.width, o.height = resolution(*t.FileResolution)
	}
	if b := t.originalBytes(r); b != nil && *b > 0 {
		o.sizeBytes = float64(*b)
	}
	if o.rate == 0 {
		switch {
		case t.FileBitrateKbps != nil && *t.FileBitrateKbps > 0:
			o.rate = float64(*t.FileBitrateKbps) * 1000
		case o.sizeBytes > 0 && dur > 0:
			o.rate = o.sizeBytes * 8 * 1000 / dur
		}
	}
	return o
}

func lowerOf(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*s))
}

// friendlyH264 reports whether the original is H.264 every browser plays,
// which the planner may copy (decision.py, h264_browser_friendly): 4:2:0 of
// 8 bits (yuv420p, yuvj420p), and of the Baseline, Main or High profile, or
// one ffprobe leaves empty; never a Dolby Vision 5 one, which is reshaped.
func (o original) friendlyH264() bool {
	switch o.profile {
	case "baseline", "constrained baseline", "main", "high", "":
	default:
		return false
	}
	return o.codec == "h264" && (o.pixFmt == "yuv420p" || o.pixFmt == "yuvj420p") && o.dv != "5"
}

// hevcCopyable reports whether the original's HEVC video may go into a
// package as it is (decision.py, hevc_copy_blocker): of the Main or Main 10
// profile, 4:2:0, at most 10 bits, and no Dolby Vision 5; a profile left
// empty is judged by the pixel format alone, and with neither known nothing
// is copied blind.
func (o original) hevcCopyable() bool {
	if o.dv == "5" {
		return false
	}
	chroma, depth := pixFmtLayout(o.pixFmt)
	if depth == 0 {
		depth = int(o.bitDepth)
	}
	if depth == 0 {
		depth = 8
	}
	switch {
	case chroma != "" && chroma != "420", depth > 10:
		return false
	case o.profile != "" && o.profile != "main" && o.profile != "main 10":
		return false
	}
	return chroma != "" || o.profile != ""
}

// overCap reports whether the original breaks a cap the planner encodes it
// for (decision.py, cap_check): its video strictly above the cap of its kind
// and bucket, or a movie's file above the file size cap. An unknown bit
// rate or size breaks none.
func (o original) overCap(t *TranscoderRules, kind string, bucket int) bool {
	if c, ok := t.capOf(kind, bucket); ok && c > 0 && o.rate > c*1e6 {
		return true
	}
	g := t.Caps.MovieMaxGiB
	return kind == "movie" && g != nil && *g > 0 && o.sizeBytes > *g*gib
}

// The pixel formats' layouts (decision.py, pix_fmt_layout), of the names a
// record of an original's video gives: planar YUV ("yuv420p10le"),
// semi-planar ("p010le", "nv12").
var (
	planarYUVRE   = regexp.MustCompile(`^yuv[aj]?(4[0-4][0-4]|41[01])p([0-9]+)?(?:le|be)?$`)
	semiPlanarRE  = regexp.MustCompile(`^p([024])([0-9][0-9])(?:le|be)?$`)
	semiPlanarYUV = map[string]string{"0": "420", "2": "422", "4": "444"}
	namedLayouts  = map[string][2]string{"nv12": {"420", "8"}, "nv21": {"420", "8"}, "nv16": {"422", "8"},
		"nv24": {"444", "8"}, "yuyv422": {"422", "8"}, "uyvy422": {"422", "8"}, "y210le": {"422", "10"}}
)

// pixFmtLayout is the chroma subsampling ("420") and the bits per sample of
// a pixel format, "" and 0 for one it doesn't know.
func pixFmtLayout(name string) (string, int) {
	if l, ok := namedLayouts[name]; ok {
		n, _ := strconv.Atoi(l[1])
		return l[0], n
	}
	if m := planarYUVRE.FindStringSubmatch(name); m != nil {
		n := 8
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		return m[1], n
	}
	if m := semiPlanarRE.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[2])
		return semiPlanarYUV[m[1]], n
	}
	return "", 0
}

// boxWidth is the width of the 16:9 box of a ladder height (decision.py,
// box_for_height): 720 is 1280 wide.
func boxWidth(height int) float64 {
	return math.Ceil(float64(height)*16/9/2) * 2
}

// planVideo is the bit rate, bit/s, of the video renditions today's
// transcoder makes of a title's original (decision.py, plan_renditions):
// each rung of its ladder, at the original's size where the original fits
// in the rung's box (it never upscales), else scaled into the box. A rung
// at the original's size copies its video, at the video's own bit rate,
// where the planner copies: HEVC that every decoder plays and that breaks
// no cap, for an HEVC rung; browser-friendly H.264 for an H.264 rung, and
// for an HEVC one whose codec the ladder leaves to the default on a CPU
// transcoder. Any other rung is an encode at its maxrate: the ladder's,
// else the planner's table's for a scaled rung, else at the original's size
// the transcoder's maxrate of its kind and bucket for HEVC (its override,
// else its cap) and the table's for H.264. An encode at the original's size
// at the maxrate the package's own encode had (its transcode's details) is
// about that encode again: its measured bit rate. A rung of the size and
// codec of an earlier one counts once. False when a rate it needs is not
// known.
func planVideo(t title, r records, rs rulesState, copied tri, dur float64) (float64, bool) {
	tr := rs.transcoder.t
	rungs, ok := tr.rungs()
	if !ok {
		return 0, false
	}
	bucket, ok := t.bucket(r, rs)
	if !ok {
		return 0, false
	}
	o := t.originalOf(r, copied, dur)
	type key struct {
		w, h  float64
		codec string
	}
	seen := map[key]bool{}
	total := 0.0
	for _, g := range rungs {
		h := g.height()
		scaled := h > 0 && !(o.height > 0 && o.height <= float64(h) && o.width <= boxWidth(h))
		codec := g.Codec
		var rate float64
		var known bool
		switch {
		case !scaled && codec == "hevc" && o.codec == "hevc" && o.hevcCopyable() && !o.overCap(tr, t.Kind, bucket):
			rate, known = o.rate, o.rate > 0
		case !scaled && codec == "h264" && o.friendlyH264():
			rate, known = o.rate, o.rate > 0
		case !scaled && codec == "hevc" && !g.CodecNamed && tr.encoderIsCPU() && o.friendlyH264():
			codec = "h264"
			rate, known = o.rate, o.rate > 0
		case g.MaxrateBps != nil && *g.MaxrateBps > 0:
			rate, known = *g.MaxrateBps, true
		case scaled:
			rate, known = tr.tableMaxrate(h, codec)
		default:
			rate, known = tr.sourceMaxrate(t.Kind, bucket, int(o.height), codec)
			if prior, ok := priorEncode(t, r, copied, codec, rate); ok {
				rate = prior
			}
		}
		if !known {
			return 0, false
		}
		k := key{o.width, o.height, codec}
		if scaled {
			k = key{boxWidth(h), float64(h), codec}
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		total += rate
	}
	return total, total > 0
}

// priorEncode is the measured bit rate of the package's top rendition when
// it is an encode of codec at maxrate (bit/s), as its transcode's details
// say it ran: an encode of the original at the same maxrate is about the
// same again. False for a package whose video was copied, of another
// codec, or encoded at another maxrate.
func priorEncode(t title, r records, copied tri, codec string, maxrate float64) (float64, bool) {
	if copied != no || r.pkg == nil || len(r.pkg.Renditions.Video) == 0 || t.Transcode.Details == nil {
		return 0, false
	}
	v := r.pkg.Renditions.Video[0]
	if v.BitrateBps == nil || *v.BitrateBps <= 0 || isHEVC(v.Codec) != (codec == "hevc") {
		return 0, false
	}
	m := maxrateRE.FindStringSubmatch(*t.Transcode.Details)
	if m == nil {
		return 0, false
	}
	was, err := strconv.ParseFloat(m[1], 64)
	if err != nil || math.Abs(was*1e6-maxrate) >= 1 {
		return 0, false
	}
	return *v.BitrateBps, true
}
