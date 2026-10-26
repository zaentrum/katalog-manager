package compliance

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// The rules a title is held against, as the report names them.
const (
	NotPackaged           = "NOT_PACKAGED"
	PackagingFailed       = "PACKAGING_FAILED"
	OverBitrateCap        = "OVER_BITRATE_CAP"
	OverSizeCap           = "OVER_SIZE_CAP"
	NoSurround            = "NO_SURROUND"
	ImageSubtitlesMissing = "IMAGE_SUBTITLES_MISSING"
	VideoNotHEVC          = "VIDEO_NOT_HEVC"
)

// Rules are every rule, in the order the report lists them.
var Rules = []string{NotPackaged, PackagingFailed, OverBitrateCap, OverSizeCap, NoSurround, ImageSubtitlesMissing, VideoNotHEVC}

// DiscImageText is what a title whose file is a disc image is found to be:
// the pipeline encodes none.
const DiscImageText = "disc image: not encoded"

// tolerance is how far above its cap a package's video may measure and still
// be within it: the packager measures a rendition from its segments, whose
// boxes add a little to the stream, and the transcoder judged the source by
// its own measure, strictly above the cap.
const tolerance = 0.03

// gib is a GiB, in bytes.
const gib = 1 << 30

// finding is a rule a title breaks, as the check keeps it.
type finding struct {
	Rule     string  `json:"rule"`
	Text     string  `json:"text"`
	Measured *string `json:"measured"`
	Limit    *string `json:"limit"`
}

// step is a title's transcode or package as the check reads it.
type step struct {
	Status, Error, Details *string
	NextRetryAt            *time.Time
}

func (s step) is(statuses ...string) bool {
	if s.Status == nil {
		return false
	}
	for _, st := range statuses {
		if *s.Status == st {
			return true
		}
	}
	return false
}

// title is what the catalog holds of a movie or an episode for the check.
type title struct {
	ID, Kind string // movie or episode
	SeriesID string // an episode's series, whose folder holds it
	// Its file (its primary asset): where it is, its size, resolution and
	// duration, and its source's size, when the asset names one; a source
	// being retired leaves no original to read.
	Path                      *string
	FileBytes, SourceBytes    *int64
	FileResolution            *string
	FileDurationMs, ItemDurMs *int64
	// Its complete version (the package that plays), the sources it is made
	// of and the size of the first; whether a version was taken in without
	// a package.
	Version        *string
	VersionSources []string
	VersionBytes   *int64
	Taken          bool
	// Its packaged playback row (a package of either layout).
	PkgCodec, PkgResolution *string
	PkgBytes, PkgDurationMs *int64
	Transcode, Package      step
	// Original is what it has of its original (library.OriginalsOf).
	Original library.Original
}

// packaged reports whether the title has a package that plays.
func (t title) packaged() bool { return t.Version != nil || t.PkgCodec != nil || t.PkgBytes != nil }

// records are what the check read of a packaged title's records (the v2
// layout's): its package and the source it was made of, each with its
// essence; nil for one it did not read, and why in unchecked.
type records struct {
	pkg                    *packageRecord
	src                    *sourceRecord
	pkgEssence, srcEssence library.Doc
	unchecked              string
}

// packageRecord is what the check reads of versions/<id>/package.json.
type packageRecord struct {
	SizeBytes  *float64 `json:"sizeBytes"`
	DurationMs *float64 `json:"durationMs"`
	Renditions struct {
		Video []struct {
			Codec      string   `json:"codec"`
			Width      *float64 `json:"width"`
			Height     *float64 `json:"height"`
			BitrateBps *float64 `json:"bitrateBps"`
			Encoder    *string  `json:"encoder"`
		} `json:"video"`
		Audio         []rendition `json:"audio"`
		AudioSurround []rendition `json:"audioSurround"`
	} `json:"renditions"`
	Subtitles []struct {
		Format            *string  `json:"format"`
		Language          *string  `json:"language"`
		SourceStreamIndex *float64 `json:"sourceStreamIndex"`
	} `json:"subtitles"`
}

type rendition struct {
	Channels   *float64 `json:"channels"`
	BitrateBps *float64 `json:"bitrateBps"`
}

// sourceRecord is what the check reads of sources/<id>/source.json.
type sourceRecord struct {
	File struct {
		SizeBytes *float64 `json:"sizeBytes"`
	} `json:"file"`
	Container struct {
		DurationMs *float64 `json:"durationMs"`
	} `json:"container"`
	Streams []struct {
		Index        *float64 `json:"index"`
		Type         string   `json:"type"`
		Codec        *string  `json:"codec"`
		Language     *string  `json:"language"`
		LanguageRaw  *string  `json:"languageRaw"`
		Form         *string  `json:"form"`
		Width        *float64 `json:"width"`
		Height       *float64 `json:"height"`
		Bitrate      *float64 `json:"bitrate"`
		Dispositions struct {
			AttachedPic bool `json:"attachedPic"`
		} `json:"dispositions"`
	} `json:"streams"`
}

// result is what the check found of a title.
type result struct {
	ItemID, Kind                                string
	Findings                                    []finding
	OriginalBytes, PackageBytes, EstimatedBytes *int64
	Unchecked                                   string
}

// rules are the rules a result breaks, in its findings' order.
func (r result) rules() []string {
	out := make([]string, 0, len(r.Findings))
	for _, f := range r.Findings {
		out = append(out, f.Rule)
	}
	return out
}

func (r result) breaks(rules ...string) bool {
	for _, f := range r.Findings {
		for _, rule := range rules {
			if f.Rule == rule {
				return true
			}
		}
	}
	return false
}

func strp(s string) *string { return &s }

// mbit writes a bit rate in Mbit/s: "73 Mbit/s", "8.4 Mbit/s".
func mbit(bps float64) string {
	return strconv.FormatFloat(roundTo(bps/1e6, 1), 'f', -1, 64) + " Mbit/s"
}

// gibs writes a size in GiB: "59.8 GiB".
func gibs(b float64) string {
	return strconv.FormatFloat(roundTo(b/gib, 1), 'f', -1, 64) + " GiB"
}

func roundTo(f float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	if f >= 0 {
		return float64(int64(f*p+0.5)) / p
	}
	return -float64(int64(-f*p+0.5)) / p
}

// evaluate holds a title against the rules rs, from what the catalog holds of
// it and what its records r say.
func evaluate(t title, r records, rs rulesState) result {
	res := result{ItemID: t.ID, Kind: t.Kind, Unchecked: r.unchecked}
	res.OriginalBytes = t.originalBytes(r)
	res.PackageBytes = t.packageBytes(r)
	add := func(rule, text string, measured, limit *string) {
		res.Findings = append(res.Findings, finding{Rule: rule, Text: text, Measured: measured, Limit: limit})
	}

	if !t.packaged() {
		switch {
		case t.Path != nil && processing.IsDiscImage(*t.Path):
			add(PackagingFailed, DiscImageText, nil, nil)
		case t.Transcode.is(processing.StatusFailed, processing.StatusSkipped):
			add(PackagingFailed, stepText("transcode", t.Transcode), nil, nil)
		case t.Package.is(processing.StatusFailed, processing.StatusSkipped):
			add(PackagingFailed, stepText("package", t.Package), nil, nil)
		case t.Original.State != library.OriginalPresent:
			// no original to package, nor a package: nothing a rework can do
		case t.Transcode.is(processing.StatusPending, processing.StatusInProgress),
			t.Package.is(processing.StatusPending, processing.StatusInProgress):
			// on its way: its pipeline runs
		case t.Taken:
			add(NotPackaged, "not packaged: taken in without a package, it plays from its original", nil, nil)
		case t.Transcode.is(processing.StatusDone, processing.StatusNotApplicable) && t.Package.is(processing.StatusDone):
			add(NotPackaged, "not packaged: its transcode and its package are done, and no package is recorded", nil, nil)
		default:
			add(NotPackaged, "not packaged", nil, nil)
		}
		res.EstimatedBytes = estimate(t, r, rs, res)
		return res
	}

	copied := t.copied(r)
	bucket, bucketed := t.bucket(r, rs)
	if tr := rs.transcoder; tr != nil && tr.t != nil {
		if bucketed {
			if limit, ok := tr.t.capOf(t.Kind, bucket); ok && limit > 0 {
				if rate, from, ok := videoRate(r, copied); ok && rate > limit*1e6*(1+tolerance) &&
					!(copied == no && encodedWithin(t.Transcode, limit)) {
					text := "video " + mbit(rate)
					if from != "" {
						text += " (" + from + ")"
					}
					add(OverBitrateCap, fmt.Sprintf("%s, over the %s cap for a %d %s", text, mbit(limit*1e6), bucket, t.Kind),
						strp(mbit(rate)), strp(mbit(limit*1e6)))
				}
			}
		}
		if capGiB := tr.t.Caps.MovieMaxGiB; t.Kind == "movie" && capGiB != nil && *capGiB > 0 && copied == yes &&
			res.OriginalBytes != nil && float64(*res.OriginalBytes) > *capGiB*gib {
			size, limit := gibs(float64(*res.OriginalBytes)), gibs(*capGiB*gib)
			add(OverSizeCap, fmt.Sprintf("original %s, over the %s cap for a movie, and its video copied as it is", size, limit),
				strp(size), strp(limit))
		}
	}
	if pr := rs.packager; pr != nil && pr.p != nil {
		if surroundOn(pr.p) && r.srcEssence != nil && r.pkgEssence != nil && library.SurroundLost(r.srcEssence, r.pkgEssence) {
			text, measured, limit := surroundText(r.srcEssence, r.pkgEssence)
			add(NoSurround, text, measured, limit)
		}
		if pr.p.ImageSubtitles != nil && r.src != nil && r.pkg != nil {
			if forms, langs := missingImageSubtitles(r, pr.p.ImageSubtitles); len(langs) > 0 {
				add(ImageSubtitlesMissing, fmt.Sprintf("%s subtitles in %s not in the package", strings.Join(forms, " and "),
					strings.Join(langs, ", ")), strp(strings.Join(langs, ", ")), nil)
			}
		}
	}
	if tr := rs.transcoder; tr != nil && tr.t != nil {
		if ok, _ := tr.t.hevcRequired(); ok {
			if codec := t.videoCodec(r); codec != "" && !isHEVC(codec) {
				add(VideoNotHEVC, fmt.Sprintf("video %s (%s), not hevc", codecFamily(codec), codec), strp(codec), strp("hevc"))
			}
		}
	}
	res.EstimatedBytes = estimate(t, r, rs, res)
	return res
}

// stepText is what a failed or skipped transcode or package says, its
// reason, and when it is retried, if it is.
func stepText(name string, s step) string {
	text := name + " " + strings.ReplaceAll(*s.Status, "_", " ")
	if s.Error != nil && strings.TrimSpace(*s.Error) != "" {
		text += ": " + strings.TrimSpace(*s.Error)
	} else if s.Details != nil && strings.TrimSpace(*s.Details) != "" {
		text += ": " + strings.TrimSpace(*s.Details)
	}
	if *s.Status == processing.StatusFailed && s.NextRetryAt != nil {
		text += "; retried at " + s.NextRetryAt.UTC().Format(time.RFC3339)
	}
	return text
}

// tri is yes, no or unknown.
type tri int

const (
	unknown tri = iota
	yes
	no
)

// copied says whether the package's video is its original's own stream,
// copied: the top rendition's encoder says ("copy", else the encoder that
// made it); a package whose record names none, by the transcode, which
// encoded nothing (not applicable) or encoded it (done).
func (t title) copied(r records) tri {
	if r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		if e := r.pkg.Renditions.Video[0].Encoder; e != nil && strings.TrimSpace(*e) != "" {
			if strings.EqualFold(strings.TrimSpace(*e), "copy") {
				return yes
			}
			return no
		}
	}
	switch {
	case t.Transcode.is(processing.StatusNotApplicable):
		return yes
	case t.Transcode.is(processing.StatusDone):
		return no
	}
	return unknown
}

// maxrateRE is the maxrate an encode's transcode details name.
var maxrateRE = regexp.MustCompile(`(?:^|\s)maxrate=([0-9]+(?:\.[0-9]+)?)Mbps(?:\s|$)`)

// encodedWithin reports whether the transcode encoded the video with a
// maxrate within limit Mbit/s, as its details say: an encode capped so is
// never above its cap on average, whatever a measure of it says.
func encodedWithin(s step, limit float64) bool {
	if !s.is(processing.StatusDone) || s.Details == nil {
		return false
	}
	m := maxrateRE.FindStringSubmatch(*s.Details)
	if m == nil {
		return false
	}
	rate, err := strconv.ParseFloat(m[1], 64)
	return err == nil && rate <= limit
}

// videoRate is the average bit rate of the package's video, bit/s, and how it
// was read, in words ("" for the package's own measure), from the most
// reliable measure the records give:
//
//  1. the package's top rendition's bitrateBps: what the packager measured of
//     the video it packaged, the sizes of its segments over their durations,
//     from start to end. It is the stream's own, the audio and the subtitles
//     apart, and the same whether the video was copied or encoded.
//  2. for a video copied as it is, the bit rate the source record gives its
//     video stream (the stream's, or its statistics tag): the stream copied.
//  3. the package's size over its duration, less its audio renditions' bit
//     rates: its subtitles and pictures count with it, so it errs high, as
//     the transcoder's own last measure does.
//
// The packaged playback row's bit rate is none of them: it is the master
// playlist's peak bandwidth, the video's peaks and the audio together, which
// an encode capped at the cap goes above.
func videoRate(r records, copied tri) (float64, string, bool) {
	if r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		if b := r.pkg.Renditions.Video[0].BitrateBps; b != nil && *b > 0 {
			return *b, "", true
		}
	}
	if copied == yes && r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type == "video" && !s.Dispositions.AttachedPic && s.Bitrate != nil && *s.Bitrate > 0 {
				return *s.Bitrate, "the source's video stream", true
			}
		}
	}
	if r.pkg != nil && r.pkg.SizeBytes != nil && r.pkg.DurationMs != nil && *r.pkg.SizeBytes > 0 && *r.pkg.DurationMs > 0 {
		rate := *r.pkg.SizeBytes * 8 * 1000 / *r.pkg.DurationMs
		for _, a := range append(append([]rendition(nil), r.pkg.Renditions.Audio...), r.pkg.Renditions.AudioSurround...) {
			if a.BitrateBps != nil {
				rate -= *a.BitrateBps
			}
		}
		if rate > 0 {
			return rate, "the package's size less its audio", true
		}
	}
	return 0, "", false
}

// capOf is the bit rate cap of a title of kind in bucket, Mbit/s (0: off),
// as the transcoder reported it; false when it reported none.
func (t *TranscoderRules) capOf(kind string, bucket int) (float64, bool) {
	var c *float64
	switch {
	case kind == "episode" && bucket == 2160:
		c = t.Caps.Episode2160Mbps
	case kind == "episode":
		c = t.Caps.Episode1080Mbps
	case bucket == 2160:
		c = t.Caps.Movie2160Mbps
	default:
		c = t.Caps.Movie1080Mbps
	}
	if c == nil {
		return 0, false
	}
	return *c, true
}

// maxrateOf is the maxrate the transcoder encodes a title of kind in bucket
// at, Mbit/s: its override of the bucket, else its cap; false when neither is
// reported (or the cap is off: the transcoder's fallback then is its own).
func (t *TranscoderRules) maxrateOf(kind string, bucket int) (float64, bool) {
	o := t.Encoder.Maxrate1080Mbps
	if bucket == 2160 {
		o = t.Encoder.Maxrate2160Mbps
	}
	if o != nil && *o > 0 {
		return *o, true
	}
	if c, ok := t.capOf(kind, bucket); ok && c > 0 {
		return c, true
	}
	return 0, false
}

// bucket is the title's cap bucket, by the transcoder's rule (2160 from
// minHeight lines tall or minWidth wide, else 1080): of its source's video
// streams, the tallest and the widest (cover art aside), as its source record
// lists them, else of its package's top rendition, else of its packaged row,
// else of its file. False when the transcoder gives no bucket, or nothing
// gives a size.
func (t title) bucket(r records, rs rulesState) (int, bool) {
	if rs.transcoder == nil || rs.transcoder.t == nil {
		return 0, false
	}
	b := rs.transcoder.t.Bucket2160
	if b.MinHeight == nil || b.MinWidth == nil {
		return 0, false
	}
	var w, h float64
	if r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type != "video" || s.Dispositions.AttachedPic {
				continue
			}
			if s.Width != nil {
				w = max(w, *s.Width)
			}
			if s.Height != nil {
				h = max(h, *s.Height)
			}
		}
	}
	if w == 0 && h == 0 && r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		v := r.pkg.Renditions.Video[0]
		if v.Width != nil && v.Height != nil {
			w, h = *v.Width, *v.Height
		}
	}
	for _, res := range []*string{t.PkgResolution, t.FileResolution} {
		if w == 0 && h == 0 && res != nil {
			w, h = resolution(*res)
		}
	}
	if w == 0 && h == 0 {
		return 0, false
	}
	if h >= *b.MinHeight || w >= *b.MinWidth {
		return 2160, true
	}
	return 1080, true
}

// resolution reads "1920x800" as its width and height.
func resolution(s string) (float64, float64) {
	ws, hs, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	if !ok {
		return 0, 0
	}
	w, err1 := strconv.ParseFloat(ws, 64)
	h, err2 := strconv.ParseFloat(hs, 64)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return w, h
}

// originalBytes is the size of the title's original: of its complete
// version's source, else as the source record gives it, else of its file's
// source, else of its file.
func (t title) originalBytes(r records) *int64 {
	if t.VersionBytes != nil {
		return t.VersionBytes
	}
	if r.src != nil && r.src.File.SizeBytes != nil {
		n := int64(*r.src.File.SizeBytes)
		return &n
	}
	if t.SourceBytes != nil {
		return t.SourceBytes
	}
	return t.FileBytes
}

// packageBytes is the size of the title's package: as its record gives it,
// else as its packaged row does.
func (t title) packageBytes(r records) *int64 {
	if r.pkg != nil && r.pkg.SizeBytes != nil {
		n := int64(*r.pkg.SizeBytes)
		return &n
	}
	if t.packaged() {
		return t.PkgBytes
	}
	return nil
}

// durationMs is how long the title runs: its package, its source, its
// packaged row, the title, its file, as the first that says.
func (t title) durationMs(r records) (float64, bool) {
	for _, d := range []*float64{pkgDuration(r), srcDuration(r), i64f(t.PkgDurationMs), i64f(t.ItemDurMs), i64f(t.FileDurationMs)} {
		if d != nil && *d > 0 {
			return *d, true
		}
	}
	return 0, false
}

func pkgDuration(r records) *float64 {
	if r.pkg == nil {
		return nil
	}
	return r.pkg.DurationMs
}

func srcDuration(r records) *float64 {
	if r.src == nil {
		return nil
	}
	return r.src.Container.DurationMs
}

func i64f(v *int64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v)
	return &f
}

// videoCodec is the codec of the package's video: its top rendition's, else
// its packaged row's.
func (t title) videoCodec(r records) string {
	if r.pkg != nil && len(r.pkg.Renditions.Video) > 0 && strings.TrimSpace(r.pkg.Renditions.Video[0].Codec) != "" {
		return strings.TrimSpace(r.pkg.Renditions.Video[0].Codec)
	}
	if t.PkgCodec != nil {
		return strings.TrimSpace(*t.PkgCodec)
	}
	return ""
}

// isHEVC reports whether a package's codec string is HEVC: hev1 or hvc1, a
// Dolby Vision HEVC sample entry (dvh1, dvhe), or a library's name for it.
func isHEVC(codec string) bool {
	c := strings.ToLower(codec)
	for _, p := range []string{"hev1", "hvc1", "dvh1", "dvhe", "hevc", "h265"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// codecFamily names the codec of a codec string as people know it.
func codecFamily(codec string) string {
	c := strings.ToLower(codec)
	for _, f := range []struct{ prefix, name string }{{"avc1", "h264"}, {"avc3", "h264"}, {"h264", "h264"},
		{"av01", "av1"}, {"vp09", "vp9"}, {"vp9", "vp9"}, {"mp4v", "mpeg-4"}} {
		if strings.HasPrefix(c, f.prefix) {
			return f.name
		}
	}
	return c
}

// hevcRequired reports whether the transcoder's rules make a package's video
// HEVC whatever its source, and why not: its ladder's source rung is HEVC,
// and either names its codec (source:hevc) or its encoder is no CPU one (on
// libx265 a rung whose codec the ladder leaves to the default keeps a
// browser-friendly H.264 source as it is).
func (t *TranscoderRules) hevcRequired() (bool, string) {
	if t.Ladder == nil {
		return false, "its report gives no ladder"
	}
	spec := strings.ToLower(strings.TrimSpace(*t.Ladder))
	tokens := strings.Split(spec, ",")
	if spec == "" {
		tokens = []string{"source"}
	}
	for _, tok := range tokens {
		parts := strings.Split(strings.TrimSpace(tok), ":")
		if strings.TrimSpace(parts[0]) != "source" {
			continue
		}
		codec, named := "hevc", false
		for _, p := range parts[1:] {
			if p = strings.TrimSpace(p); p == "hevc" || p == "h264" {
				codec, named = p, true
			}
		}
		switch {
		case codec != "hevc":
			return false, "its ladder's source rung is h264"
		case named:
			return true, ""
		case t.Encoder.Codec == nil:
			return false, "its report gives no encoder"
		case strings.EqualFold(strings.TrimSpace(*t.Encoder.Codec), "libx265"):
			return false, "its ladder leaves the source rung's codec to the default, and on libx265 a browser-friendly h264 source is kept as it is"
		}
		return true, ""
	}
	return false, "its ladder has no source rung"
}

// surroundOn reports whether the packager makes a 5.1 companion.
func surroundOn(p *PackagerRules) bool {
	return p.SurroundAudio != nil && strings.TrimSpace(*p.SurroundAudio) != "" &&
		!strings.EqualFold(strings.TrimSpace(*p.SurroundAudio), "off")
}

func essenceNumber(d library.Doc, key string) (float64, bool) {
	v, ok := d.Get(key)
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return x, true
	case interface{ Float64() (float64, error) }:
		f, err := x.Float64()
		return f, err == nil
	case int64:
		return float64(x), true
	case int:
		return float64(x), true
	}
	return 0, false
}

func channels(n float64) string {
	if int(n) == 1 {
		return "1 channel"
	}
	return fmt.Sprintf("%d channels", int(n))
}

// surroundText is what a package that lacks its source's surround is found
// to be: the source's channels, the package's, and a 5.1 companion's, which
// the package needs (a 7.1 source's 5.1 will do).
func surroundText(src, pkg library.Doc) (string, *string, *string) {
	had, hadOK := essenceNumber(src, "maxAudioChannels")
	kept, keptOK := essenceNumber(pkg, "maxAudioChannels")
	if !hadOK || had <= 2 {
		return "no 5.1 of the source's surround", nil, nil
	}
	text := fmt.Sprintf("no 5.1 of the source's %d-channel audio", int(had))
	if !keptOK {
		return text, nil, strp(channels(min(had, 6)))
	}
	return fmt.Sprintf("%s: the package carries %s", text, channels(kept)), strp(channels(kept)), strp(channels(min(had, 6)))
}

// imageForms are the forms of image subtitles, by the codec of the stream.
var imageForms = map[string]string{"hdmv_pgs_subtitle": "pgs", "dvd_subtitle": "vobsub", "dvb_subtitle": "dvb", "xsub": "xsub"}

// packageForms are the forms of the image subtitles of a package, by the
// format its record gives them.
var packageForms = map[string]string{"pgs": "pgs", "sup": "pgs", "vobsub": "vobsub", "dvb": "dvb", "xsub": "xsub"}

// languageOf is the primary language of a tag or a code ("de" of "de",
// "deu", "ger", "de-CH"), "und" when it names none.
func languageOf(codes ...*string) string {
	for _, c := range codes {
		if c == nil {
			continue
		}
		if t, ok := languages.Tag(*c); ok {
			primary, _, _ := strings.Cut(t, "-")
			return primary
		}
	}
	return languages.Unknown
}

// missingImageSubtitles are the image subtitles of the source of a form the
// packager carries (forms) that the package carries none of in their
// language: their forms and their languages, sorted. A package's image
// subtitle carries the language its record gives it, and the language of the
// source stream it was made from.
func missingImageSubtitles(r records, carried []string) ([]string, []string) {
	carries := map[string]bool{}
	for _, f := range carried {
		carries[strings.ToLower(strings.TrimSpace(f))] = true
	}
	langOfStream := map[int]string{}
	type want struct{ forms map[string]bool }
	wanted := map[string]*want{}
	for _, s := range r.src.Streams {
		if s.Type != "subtitle" {
			continue
		}
		codec := ""
		if s.Codec != nil {
			codec = strings.ToLower(strings.TrimSpace(*s.Codec))
		}
		form, image := imageForms[codec]
		if s.Form != nil && strings.EqualFold(*s.Form, "text") {
			image = false
		}
		lang := languageOf(s.Language, s.LanguageRaw)
		if s.Index != nil {
			langOfStream[int(*s.Index)] = lang
		}
		if !image || !carries[form] {
			continue
		}
		if wanted[lang] == nil {
			wanted[lang] = &want{forms: map[string]bool{}}
		}
		wanted[lang].forms[form] = true
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	has := map[string]bool{}
	for _, s := range r.pkg.Subtitles {
		if s.Format == nil || !carries[packageForms[strings.ToLower(strings.TrimSpace(*s.Format))]] {
			continue
		}
		has[languageOf(s.Language)] = true
		if s.SourceStreamIndex != nil {
			if l, ok := langOfStream[int(*s.SourceStreamIndex)]; ok {
				has[l] = true
			}
		}
	}
	forms := map[string]bool{}
	var langs []string
	for lang, w := range wanted {
		if has[lang] {
			continue
		}
		langs = append(langs, lang)
		for f := range w.forms {
			forms[f] = true
		}
	}
	sort.Strings(langs)
	out := make([]string, 0, len(forms))
	for f := range forms {
		out = append(out, f)
	}
	sort.Strings(out)
	return out, langs
}

// estimate is what the title's package is to be after its rework, at most,
// bytes; nil when that cannot be told. A rework that encodes the video (a
// title over a cap, of another codec, or not packaged) makes a package of the
// video at its maxrate (the transcoder's cap of its kind and bucket, or its
// override) and the packager's audio for one language, the stereo rendition
// and, when the source has surround or the record does not say, the 5.1
// companion, for the title's duration. A rework that only adds tracks (the
// 5.1, image subtitles) keeps the package's video: its package as it is,
// with the 5.1 companion added. A title a rework cannot help (no original,
// a disc image) has none.
func estimate(t title, r records, rs rulesState, res result) *int64 {
	if len(res.Findings) == 0 || t.Original.State != library.OriginalPresent ||
		(t.Path != nil && processing.IsDiscImage(*t.Path)) {
		return nil
	}
	dur, durOK := t.durationMs(r)
	if !durOK || rs.packager == nil || rs.packager.p == nil {
		return nil
	}
	p := rs.packager.p
	surround := 0.0
	if surroundOn(p) && p.SurroundBitrate != nil {
		had := true
		if r.srcEssence != nil {
			v, ok := r.srcEssence.Get("surround")
			had = !ok || v == true
		}
		if had {
			surround, _ = parseBitrate(*p.SurroundBitrate)
		}
	}
	if res.breaks(NotPackaged, PackagingFailed, OverBitrateCap, OverSizeCap, VideoNotHEVC) {
		if rs.transcoder == nil || rs.transcoder.t == nil || p.StereoBitrate == nil {
			return nil
		}
		bucket, ok := t.bucket(r, rs)
		if !ok {
			return nil
		}
		video, ok := rs.transcoder.t.maxrateOf(t.Kind, bucket)
		if !ok {
			return nil
		}
		stereo, _ := parseBitrate(*p.StereoBitrate)
		n := int64((video*1e6 + stereo + surround) * dur / 8000)
		return &n
	}
	if res.PackageBytes == nil {
		return nil
	}
	n := *res.PackageBytes
	if res.breaks(NoSurround) {
		n += int64(surround * dur / 8000)
	}
	return &n
}
