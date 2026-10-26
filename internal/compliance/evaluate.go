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
	// Detail is what a tool said of it (an exit code, a codec id), for
	// under the finding's details and never in its text.
	Detail *string `json:"detail,omitempty"`
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
	// Its file (its primary asset): where it is, its size, resolution,
	// codec, bit rate and duration as the catalog's probe of it holds them,
	// and its source's size, when the asset names one; a source being
	// retired leaves no original to read. ItemDurMs is the title's runtime,
	// as the reference database gives it.
	Path                      *string
	FileBytes, SourceBytes    *int64
	FileResolution, FileCodec *string
	FileBitrateKbps           *int64
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
	// AudioLanguages are the languages an admin set for its audio tracks,
	// by the track's place among the original's audio streams (migration
	// 037's com_nalet_katalog_itemtracklanguages), which the packager
	// names a track by.
	AudioLanguages map[int]string
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
	Codec      *string  `json:"codec"`
	Channels   *float64 `json:"channels"`
	BitrateBps *float64 `json:"bitrateBps"`
	Language   *string  `json:"language"`
}

// sourceRecord is what the check reads of sources/<id>/source.json.
type sourceRecord struct {
	File struct {
		SizeBytes *float64 `json:"sizeBytes"`
	} `json:"file"`
	Container struct {
		// Format is ffprobe's name of the container ("matroska,webm").
		Format     *string  `json:"format"`
		DurationMs *float64 `json:"durationMs"`
	} `json:"container"`
	Streams []struct {
		Index        *float64 `json:"index"`
		Type         string   `json:"type"`
		Codec        *string  `json:"codec"`
		Language     *string  `json:"language"`
		LanguageRaw  *string  `json:"languageRaw"`
		Variant      *string  `json:"variant"`
		Form         *string  `json:"form"`
		Channels     *float64 `json:"channels"`
		Profile      *string  `json:"profile"`
		PixelFormat  *string  `json:"pixelFormat"`
		BitDepth     *float64 `json:"bitDepth"`
		Width        *float64 `json:"width"`
		Height       *float64 `json:"height"`
		Bitrate      *float64 `json:"bitrate"`
		Title        *string  `json:"title"`
		Dispositions struct {
			AttachedPic bool `json:"attachedPic"`
			Commentary  bool `json:"commentary"`
		} `json:"dispositions"`
		// Hdr is a video stream's high dynamic range: its Dolby Vision
		// configuration as ffprobe read it (source.schema.json's
		// videoStream.hdr.dolbyVision).
		Hdr struct {
			DolbyVision *struct {
				Profile *float64 `json:"profile"`
			} `json:"dolbyVision"`
		} `json:"hdr"`
	} `json:"streams"`
}

// result is what the check found of a title.
type result struct {
	ItemID, Kind                                string
	Findings                                    []finding
	OriginalBytes, PackageBytes, EstimatedBytes *int64
	Unchecked                                   string
	// Blocked is why processing the title again can't help, as its
	// original's record or its failed step tells (blockedOf), "" when
	// nothing tells it can't.
	Blocked string
	// Measures are what the report's columns show of it.
	Measures measures
}

// measures are what the report's columns show of a title, as the check
// measured it: its video's size and codec (the package's, else the
// original's), its video's bit rate (as OVER_BITRATE_CAP measures it;
// estimated when it was read off a size and a duration), the transcoder's
// caps that hold for it, its duration, and the audio of its package and of
// its original, from their records.
type measures struct {
	Resolution, VideoCodec      *string
	VideoBitrate                *float64
	BitrateEstimated            bool
	BitrateCap, DurationSeconds *float64
	SizeCap                     *int64
	PackageAudio, OriginalAudio *string
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
	addStep := func(worker string, s step) {
		text, detail := stepText(worker, s, rs.retries)
		res.Findings = append(res.Findings, finding{Rule: PackagingFailed, Text: text, Detail: detail})
	}

	if !t.packaged() {
		switch {
		case t.Path != nil && processing.IsDiscImage(*t.Path):
			add(PackagingFailed, discImageText, nil, nil)
		case t.Transcode.is(processing.StatusFailed, processing.StatusSkipped):
			addStep("transcoder", t.Transcode)
		case t.Package.is(processing.StatusFailed, processing.StatusSkipped):
			addStep("packager", t.Package)
		case t.Original.State != library.OriginalPresent:
			// no original to package, nor a package: processing it again cannot help
		case t.Transcode.is(processing.StatusPending, processing.StatusInProgress),
			t.Package.is(processing.StatusPending, processing.StatusInProgress):
			// on its way: its pipeline runs
		case t.Taken:
			add(NotPackaged, takenInText, nil, nil)
		case t.Transcode.is(processing.StatusDone, processing.StatusNotApplicable) && t.Package.is(processing.StatusDone):
			add(NotPackaged, finishedText, nil, nil)
		default:
			add(NotPackaged, notPackagedText, nil, nil)
		}
		res.Blocked = blockedBy(t, r, rs)
		res.EstimatedBytes = estimate(t, r, rs, res)
		res.Measures = measure(t, r, rs, unknown)
		return res
	}

	copied := t.copied(r)
	bucket, bucketed := t.bucket(r, rs)
	if tr := rs.transcoder; tr != nil && tr.t != nil {
		if bucketed {
			if limit, ok := tr.t.capOf(t.Kind, bucket); ok && limit > 0 {
				if rate, approx, ok := videoRate(r, copied); ok && rate > limit*1e6*(1+tolerance) &&
					!(copied == no && encodedWithin(t.Transcode, limit)) {
					text, measured, allowed := bitrateWords(rate, approx, limit*1e6, bucket, t.Kind)
					add(OverBitrateCap, text, measured, allowed)
				}
			}
		}
		if capGiB := tr.t.Caps.MovieMaxGiB; t.Kind == "movie" && capGiB != nil && *capGiB > 0 && copied == yes &&
			res.OriginalBytes != nil && float64(*res.OriginalBytes) > *capGiB*gib {
			text, measured, allowed := sizeWords(*capGiB * gib)
			add(OverSizeCap, text, measured, allowed)
		}
	}
	if pr := rs.packager; pr != nil && pr.p != nil {
		if surroundOn(pr.p) && r.srcEssence != nil && r.pkgEssence != nil && library.SurroundLost(r.srcEssence, r.pkgEssence) {
			had, hadOK := essenceNumber(r.srcEssence, "maxAudioChannels")
			kept, keptOK := essenceNumber(r.pkgEssence, "maxAudioChannels")
			text, measured, allowed := surroundWords(had, kept, hadOK, keptOK, pr.p)
			add(NoSurround, text, measured, allowed)
		}
		if pr.p.ImageSubtitles != nil && r.src != nil && r.pkg != nil {
			if byForm, langs := missingImageSubtitles(r, pr.p.ImageSubtitles); len(langs) > 0 {
				text, measured, allowed := imageSubtitleWords(byForm, langs, pr.p.ImageSubtitles)
				add(ImageSubtitlesMissing, text, measured, allowed)
			}
		}
	}
	if tr := rs.transcoder; tr != nil && tr.t != nil {
		if ok, _ := tr.t.hevcRequired(); ok {
			if codec := t.videoCodec(r); codec != "" && !isHEVC(codec) {
				text, measured, allowed, detail := codecWords(codec)
				res.Findings = append(res.Findings, finding{Rule: VideoNotHEVC, Text: text, Measured: measured, Limit: allowed,
					Detail: detail})
			}
		}
	}
	res.Blocked = blockedBy(t, r, rs)
	res.EstimatedBytes = estimate(t, r, rs, res)
	res.Measures = measure(t, r, rs, copied)
	return res
}

// measure is what the report's columns show of a title (measures).
func measure(t title, r records, rs rulesState, copied tri) measures {
	var m measures
	if w, h, ok := t.picture(r); ok {
		m.Resolution = strp(strconv.Itoa(w) + "×" + strconv.Itoa(h))
	}
	if c := t.videoCodec(r); c != "" {
		m.VideoCodec = strp(codecName(c))
	} else if r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type == "video" && !s.Dispositions.AttachedPic && s.Codec != nil && *s.Codec != "" {
				m.VideoCodec = strp(codecName(*s.Codec))
				break
			}
		}
	}
	dur, durOK := t.durationMs(r)
	if durOK {
		m.DurationSeconds = roundp(dur/1000, 1)
	}
	if t.packaged() {
		if rate, approx, ok := videoRate(r, copied); ok {
			m.VideoBitrate, m.BitrateEstimated = roundp(rate, 0), approx
		}
	} else if t.FileBytes != nil && *t.FileBytes > 0 && durOK {
		// the original's, of its file's size over its duration
		m.VideoBitrate, m.BitrateEstimated = roundp(float64(*t.FileBytes)*8*1000/dur, 0), true
	}
	if tr := rs.transcoder; tr != nil && tr.t != nil {
		if bucket, ok := t.bucket(r, rs); ok {
			if c, ok := tr.t.capOf(t.Kind, bucket); ok && c > 0 {
				m.BitrateCap = roundp(c*1e6, 0)
			}
		}
		if c := tr.t.Caps.MovieMaxGiB; t.Kind == "movie" && c != nil && *c > 0 {
			b := int64(*c * gib)
			m.SizeCap = &b
		}
	}
	if r.pkg != nil {
		var tracks []string
		for _, group := range [][]rendition{r.pkg.Renditions.AudioSurround, r.pkg.Renditions.Audio} {
			for _, a := range group {
				codec, ch := "", 0.0
				if a.Codec != nil {
					codec = *a.Codec
				}
				if a.Channels != nil {
					ch = *a.Channels
				}
				tracks = append(tracks, trackText(ch, codec, ""))
			}
		}
		m.PackageAudio = audioText(tracks)
	}
	if r.src != nil {
		var tracks []string
		for _, s := range r.src.Streams {
			if s.Type != "audio" {
				continue
			}
			codec, profile, ch := "", "", 0.0
			if s.Codec != nil {
				codec = *s.Codec
			}
			if s.Profile != nil {
				profile = *s.Profile
			}
			if s.Channels != nil {
				ch = *s.Channels
			}
			tracks = append(tracks, trackText(ch, codec, profile))
		}
		m.OriginalAudio = audioText(tracks)
	}
	return m
}

// picture is the size of a title's video: the package's top rendition's,
// else its original's video stream's (cover art aside), else its file's as
// the catalog holds it ("1920x1080").
func (t title) picture(r records) (int, int, bool) {
	if r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		v := r.pkg.Renditions.Video[0]
		if v.Width != nil && v.Height != nil && *v.Width > 0 && *v.Height > 0 {
			return int(*v.Width), int(*v.Height), true
		}
	}
	if r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type == "video" && !s.Dispositions.AttachedPic && s.Width != nil && s.Height != nil && *s.Width > 0 && *s.Height > 0 {
				return int(*s.Width), int(*s.Height), true
			}
		}
	}
	for _, res := range []*string{t.PkgResolution, t.FileResolution} {
		if res == nil {
			continue
		}
		if w, h := resolution(*res); w > 0 && h > 0 {
			return int(w), int(h), true
		}
	}
	return 0, 0, false
}

// roundp is v rounded to places, as a pointer.
func roundp(v float64, places int) *float64 {
	f := roundTo(v, places)
	return &f
}

// blockedBy is why processing a title again can't help that the check can
// be certain of, of two facts alone; "" when neither tells, and the title is
// offered:
//   - its original's record (sources/<id>/source.json, read with the
//     version of a packaged title): its streams, probed, hold no video but
//     cover art, or its video stream's hdr.dolbyVision.profile is one the
//     transcoder leaves as it is (refusesDolbyVision);
//   - its failed or skipped step: the worker's reason says the original has
//     no video, or the transcoder refused its Dolby Vision profile, as it
//     still does.
func blockedBy(t title, r records, rs rulesState) string {
	var tr *TranscoderRules
	if rs.transcoder != nil {
		tr = rs.transcoder.t
	}
	for _, s := range []step{t.Transcode, t.Package} {
		if !s.is(processing.StatusFailed, processing.StatusSkipped) || s.Error == nil {
			continue
		}
		reason := strings.TrimSpace(*s.Error)
		if clause, ok := knownReason(reason); ok && clause == noVideoClause {
			return blockedNoVideo
		}
		if m := refusedProfileRE.FindStringSubmatch(reason); m != nil && tr.refusesDolbyVision(m[1]) {
			return blockedDolbyVision + m[1]
		}
		if m := noVulkanRE.FindStringSubmatch(reason); m != nil && tr.refusesDolbyVision(m[1]) {
			return blockedDolbyVision + m[1]
		}
	}
	if r.src == nil || len(r.src.Streams) == 0 {
		return ""
	}
	for _, s := range r.src.Streams {
		if s.Type != "video" || s.Dispositions.AttachedPic {
			continue
		}
		if dv := s.Hdr.DolbyVision; dv != nil && dv.Profile != nil {
			if p := strconv.Itoa(int(*dv.Profile)); tr.refusesDolbyVision(p) {
				return blockedDolbyVision + p
			}
		}
		return "" // the first video stream is the one the transcoder plans
	}
	return blockedNoVideo
}

// The reasons the transcoder gives of an original it refused for its Dolby
// Vision (decision.py's dolby_vision_blocker, plan_renditions).
var (
	refusedProfileRE = regexp.MustCompile(`^Dolby Vision profile ([0-9]+(?:\.[0-9]+)?) needs a tone-mapping encode; kept the original$`)
	noVulkanRE       = regexp.MustCompile(`^Dolby Vision profile ([0-9]+) is reshaped to HDR10 on a Vulkan device, and this host has none`)
)

// refusesDolbyVision reports whether the transcoder is certain to leave an
// original of Dolby Vision profile as it is: profile 7, which it never plans
// (decision.py's DOVI_REFUSED_PROFILES), unless it reports something else
// of it; a profile of base-layer compatibility 0 ("8.0"), which it refuses
// too; profile 5, which it reshapes on a Vulkan device, only while it
// reports it refuses it (a transcoder that doesn't report it is taken to
// reshape it). t is nil for a transcoder that never reported.
func (t *TranscoderRules) refusesDolbyVision(profile string) bool {
	said := ""
	if t != nil {
		said = t.DolbyVision["profile"+profile]
	}
	switch {
	case profile == "7":
		return said == "" || said == dvRefused
	case strings.HasSuffix(profile, ".0"):
		return true
	}
	return said == dvRefused
}

// dvRefused is what the transcoder reports of a Dolby Vision profile it
// leaves as it is.
const dvRefused = "refused"

// stepText is what a failed or skipped transcode or package of worker says,
// and the worker's reason, its detail: a retry is said only while one is
// really scheduled, the step's next retry set and the sweep that sends it
// running (retries).
func stepText(worker string, s step, retries bool) (string, *string) {
	reason := ""
	if s.Error != nil && strings.TrimSpace(*s.Error) != "" {
		reason = *s.Error
	} else if s.Details != nil {
		reason = *s.Details
	}
	return stepWords(worker, *s.Status, reason, retries && *s.Status == processing.StatusFailed && s.NextRetryAt != nil)
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

// videoRate is the average bit rate of the package's video, bit/s, and
// whether it is approximate, from the most reliable measure the records give:
//
//  1. the package's top rendition's bitrateBps: what the packager measured of
//     the video it packaged, the sizes of its segments over their durations,
//     from start to end. It is the stream's own, the audio and the subtitles
//     apart, and the same whether the video was copied or encoded.
//  2. for a video copied as it is, the bit rate the source record gives its
//     video stream (the stream's, or its statistics tag): the stream copied.
//  3. the package's size over its duration, less its audio renditions' bit
//     rates: its subtitles and pictures count with it, so it errs high, as
//     the transcoder's own last measure does, and it is approximate.
//
// The packaged playback row's bit rate is none of them: it is the master
// playlist's peak bandwidth, the video's peaks and the audio together, which
// an encode capped at the cap goes above.
func videoRate(r records, copied tri) (float64, bool, bool) {
	if r.pkg != nil && len(r.pkg.Renditions.Video) > 0 {
		if b := r.pkg.Renditions.Video[0].BitrateBps; b != nil && *b > 0 {
			return *b, false, true
		}
	}
	if copied == yes && r.src != nil {
		for _, s := range r.src.Streams {
			if s.Type == "video" && !s.Dispositions.AttachedPic && s.Bitrate != nil && *s.Bitrate > 0 {
				return *s.Bitrate, false, true
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
			return rate, true, true
		}
	}
	return 0, false, false
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

// durationMs is how long the title runs, as the first that says: its
// package's record, its original's record, its packaged row, the catalog's
// probe of its file, and only then the title's runtime as the reference
// database gives it.
func (t title) durationMs(r records) (float64, bool) {
	for _, d := range []*float64{pkgDuration(r), srcDuration(r), i64f(t.PkgDurationMs), i64f(t.FileDurationMs), i64f(t.ItemDurMs)} {
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

// isH264 reports whether a package's codec string is H.264 (avc1, avc3), or
// a library's name for it.
func isH264(codec string) bool {
	c := strings.ToLower(codec)
	for _, p := range []string{"avc1", "avc3", "h264"} {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// hevcRequired reports whether the transcoder's rules make a package's video
// HEVC whatever its source, and why not, in a sentence the report shows: its
// ladder's source rung is HEVC, and either names its codec (source:hevc) or
// its encoder is no CPU one (on libx265 a rung whose codec the ladder leaves
// to the default keeps a browser-friendly H.264 source as it is).
func (t *TranscoderRules) hevcRequired() (bool, string) {
	if t.Ladder == nil {
		return false, "The transcoder doesn’t report its renditions, so the video format can’t be checked."
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
			return false, "The transcoder makes the rendition at the original’s size in H.264, so H.264 breaks no rule."
		case named:
			return true, ""
		case t.Encoder.Codec == nil:
			return false, "The transcoder doesn’t report its encoder, so the video format can’t be checked."
		case strings.EqualFold(strings.TrimSpace(*t.Encoder.Codec), "libx265"):
			return false, "This transcoder keeps H.264 video as it is when browsers can play it, so H.264 breaks no rule."
		}
		return true, ""
	}
	return false, "The transcoder makes no rendition at the original’s size, so the video format can’t be checked."
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

// missingImageSubtitles are the image subtitles of the original of a form
// the packager carries (carried) that the package carries none of in their
// language: the language tags of each form ("pt-BR", with the script and the
// region the original's stream names), and every tag, sorted. A language is
// held by the language alone: a package's image subtitle carries the
// language its record gives it, and the language of the original's stream
// it was made from.
func missingImageSubtitles(r records, carried []string) (map[string][]string, []string) {
	carries := map[string]bool{}
	for _, f := range carried {
		carries[strings.ToLower(strings.TrimSpace(f))] = true
	}
	langOfStream := map[int]string{}
	wanted := map[string]map[string]bool{}            // language -> forms
	tagsOf := map[string]map[string]map[string]bool{} // language -> form -> tags
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
		tag := library.StreamLanguage(s.Language, s.LanguageRaw, s.Variant)
		lang, _, _ := strings.Cut(tag, "-")
		if tag == "" {
			tag, lang = languages.Unknown, languages.Unknown
		}
		if s.Index != nil {
			langOfStream[int(*s.Index)] = lang
		}
		if !image || !carries[form] {
			continue
		}
		if wanted[lang] == nil {
			wanted[lang], tagsOf[lang] = map[string]bool{}, map[string]map[string]bool{}
		}
		wanted[lang][form] = true
		if tagsOf[lang][form] == nil {
			tagsOf[lang][form] = map[string]bool{}
		}
		tagsOf[lang][form][tag] = true
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
	byForm := map[string][]string{}
	all := map[string]bool{}
	for lang, forms := range wanted {
		if has[lang] {
			continue
		}
		for f := range forms {
			for tag := range tagsOf[lang][f] {
				byForm[f] = append(byForm[f], tag)
				all[tag] = true
			}
		}
	}
	langs := make([]string, 0, len(all))
	for tag := range all {
		langs = append(langs, tag)
	}
	sort.Strings(langs)
	for f := range byForm {
		sort.Strings(byForm[f])
	}
	return byForm, langs
}
