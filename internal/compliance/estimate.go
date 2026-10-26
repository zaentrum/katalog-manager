package compliance

import (
	"strings"

	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// estimate is about what the title's package is to be once it is processed
// again, bytes: an estimate as accurate as the records allow, not a bound.
// Processing a title again runs the transcoder on its original and the
// packager on what it makes (the re-encode queue resets both), so the
// package is:
//
//   - the video of every rung today's transcoder plans of the original
//     (planVideo): a copy at the original's own bit rate, an encode at its
//     maxrate, or about the package's own encode where that ran at the
//     maxrate an encode runs at today;
//   - the audio today's packager makes of it (planAudio): a stereo
//     rendition of every audio track, a 5.1 companion of the first
//     surround track of each language;
//   - what the package holds beside its renditions, which processing makes
//     again (its subtitles, its trickplay pictures, its playlists), as its
//     record measures it (packageExtras);
//
// for the title's duration. Image subtitles processing adds to a package
// are not counted: the original's record gives no size of them. Nil when
// the records don't tell: a title processing again can't help (no original,
// a disc image, one the transcoder would refuse again), no duration, a
// worker that never reported, or a rate neither reports.
func estimate(t title, r records, rs rulesState, res result) *int64 {
	if len(res.Findings) == 0 || res.Blocked != "" || t.Original.State != library.OriginalPresent ||
		(t.Path != nil && processing.IsDiscImage(*t.Path)) {
		return nil
	}
	dur, durOK := t.durationMs(r)
	if !durOK || rs.packager == nil || rs.packager.p == nil || rs.transcoder == nil || rs.transcoder.t == nil {
		return nil
	}
	p := rs.packager.p
	plan := planAudio(t, r, p)
	stereo := 0.0
	if p.StereoBitrate != nil {
		stereo, _ = parseBitrate(*p.StereoBitrate)
	}
	audio, ok := audioRate(plan.stereo, plan.companions, stereo)
	if !ok {
		return nil
	}
	video, ok := planVideo(t, r, rs, t.copied(r), dur)
	if !ok {
		return nil
	}
	n := int64((video+audio)*dur/8000) + packageExtras(r, dur)
	return &n
}

// packageExtras is what the package holds beside its renditions, bytes: its
// subtitles, its trickplay pictures, its playlists and init segments, which
// processing again makes anew. It is the package's size, as its record
// gives it, less what its renditions' measured bit rates make over its
// duration; 0 without a package, with a rendition not measured, or with
// nothing left.
func packageExtras(r records, dur float64) int64 {
	if r.pkg == nil || r.pkg.SizeBytes == nil || *r.pkg.SizeBytes <= 0 || len(r.pkg.Renditions.Video) == 0 {
		return 0
	}
	if r.pkg.DurationMs != nil && *r.pkg.DurationMs > 0 {
		dur = *r.pkg.DurationMs
	}
	media := 0.0
	for _, v := range r.pkg.Renditions.Video {
		if v.BitrateBps == nil || *v.BitrateBps <= 0 {
			return 0
		}
		media += *v.BitrateBps
	}
	for _, a := range append(append([]rendition(nil), r.pkg.Renditions.Audio...), r.pkg.Renditions.AudioSurround...) {
		if a.BitrateBps == nil || *a.BitrateBps <= 0 {
			return 0
		}
		media += *a.BitrateBps
	}
	rest := *r.pkg.SizeBytes - media*dur/8000
	if rest <= 0 {
		return 0
	}
	return int64(rest)
}

// audioRate is the bit rate of stereo renditions at stereo bit/s each and of
// companions of their bit rates, bit/s; false when a rate it needs is not
// known.
func audioRate(renditions int, companions []float64, stereo float64) (float64, bool) {
	if renditions > 0 && stereo <= 0 {
		return 0, false
	}
	total := float64(renditions) * stereo
	for _, c := range companions {
		if c <= 0 {
			return 0, false
		}
		total += c
	}
	return total, true
}

// Where an audioPlan's count comes from.
const (
	planOriginal = "original" // the original's record
	planPackage  = "package"  // the package's record
	planAssumed  = "assumed"  // neither: one stereo rendition
)

// audioPlan is the audio a package of a title is to carry once it is
// processed again: how many stereo renditions, and the bit rates of its 5.1
// companions (bit/s; 0 where nothing tells), and where the count comes from
// (planOriginal, planPackage, planAssumed).
type audioPlan struct {
	from       string
	stereo     int
	companions []float64
}

// planAudio is the audio the packager makes of a title's original, as its
// rules make it (packager.py, package_item and _prepare_source): a stereo
// rendition of every audio track, whatever its language — its language
// whitelist (packager.languages) and keep_original_if_single only say which
// of them the player lists (_visible_indices: "We never drop tracks at the
// packager level"), so they change nothing of the package's size — and a
// 5.1 companion of the first track of more than two channels of each
// language that is no commentary (_surround_plan), the track itself where it
// is a 5.1 of the companion's codec (at its own bit rate: its record's,
// else that of the package's companion of its language), else an encode at
// the packager's surround bit rate. The transcoder's encode carries every
// audio track of the original as it is (ffmpeg.py: -map 0:a?, -c:a copy), so
// either way they are the original's tracks. They are counted off the
// original's record (sources/<id>/source.json: its audio streams, in their
// order, each in the language an admin set for it, which the packager takes
// over the file's); else off the package's record (versions/<id>/
// package.json: renditions.audio, and renditions.audioSurround at the larger
// of each one's measured bit rate and the packager's); else one stereo
// rendition. Without the original's streams, a companion is counted when the
// original had surround or its record doesn't say.
func planAudio(t title, r records, p *PackagerRules) audioPlan {
	rate := 0.0
	if p.SurroundBitrate != nil {
		rate, _ = parseBitrate(*p.SurroundBitrate)
	}
	on := surroundOn(p)
	target := ""
	if on {
		target = strings.ToLower(strings.TrimSpace(*p.SurroundAudio))
	}
	if r.src != nil {
		plan := audioPlan{from: planOriginal}
		seen := map[string]bool{}
		for _, s := range r.src.Streams {
			if s.Type != "audio" {
				continue
			}
			ordinal := plan.stereo
			plan.stereo++
			ch := 0.0
			if s.Channels != nil {
				ch = *s.Channels
			}
			if !on || ch <= 2 || commentary(s.Title, s.Dispositions.Commentary) {
				continue
			}
			lang := languageKey(s.LanguageRaw, s.Language)
			if o, ok := t.AudioLanguages[ordinal]; ok {
				lang = languageKey(&o)
			}
			if seen[lang] {
				continue
			}
			seen[lang] = true
			companion := rate
			if s.Codec != nil && strings.EqualFold(strings.TrimSpace(*s.Codec), target) && ch == 6 {
				if s.Bitrate != nil && *s.Bitrate > 0 {
					companion = *s.Bitrate
				} else if measured, ok := companionOf(r, lang); ok {
					companion = measured
				}
			}
			plan.companions = append(plan.companions, companion)
		}
		if plan.stereo > 0 {
			return plan
		}
	}
	had := true
	if r.srcEssence != nil {
		v, ok := r.srcEssence.Get("surround")
		had = !ok || v == true
	}
	plan := audioPlan{from: planAssumed, stereo: 1}
	if r.pkg != nil && len(r.pkg.Renditions.Audio) > 0 {
		plan = audioPlan{from: planPackage, stereo: len(r.pkg.Renditions.Audio)}
		if on {
			for _, a := range r.pkg.Renditions.AudioSurround {
				c := rate
				if a.BitrateBps != nil && *a.BitrateBps > c {
					c = *a.BitrateBps
				}
				plan.companions = append(plan.companions, c)
			}
		}
	}
	if on && had && len(plan.companions) == 0 {
		plan.companions = []float64{rate}
	}
	return plan
}

// companionOf is the measured bit rate of the package's 5.1 companion of
// the language lang, bit/s; false for a package with none of it.
func companionOf(r records, lang string) (float64, bool) {
	if r.pkg == nil {
		return 0, false
	}
	for _, a := range r.pkg.Renditions.AudioSurround {
		if languageKey(a.Language) == lang && a.BitrateBps != nil && *a.BitrateBps > 0 {
			return *a.BitrateBps, true
		}
	}
	return 0, false
}

// commentary reports whether the packager takes an audio track for a
// commentary, which gets no 5.1 companion (packager.py, _is_commentary): its
// disposition says it is one, or its title holds "comment" or "kommentar".
func commentary(title *string, disposition bool) bool {
	if disposition {
		return true
	}
	if title == nil {
		return false
	}
	t := strings.ToLower(*title)
	return strings.Contains(t, "comment") || strings.Contains(t, "kommentar")
}

// languageKey is the key the packager tells a track's language by
// (packager.py, _lang_key), of the first code that names one: its primary
// subtag, its ISO 639-1 code where it has one ("de" of "ger", "deu",
// "de-CH"), else as it is ("zxx", "gsw"); "und" when none does.
func languageKey(codes ...*string) string {
	for _, c := range codes {
		if c == nil {
			continue
		}
		primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(*c, "_", "-"))), "-")
		if primary == "" {
			continue
		}
		if tag, ok := languages.Tag(primary); ok {
			primary, _, _ = strings.Cut(tag, "-")
		}
		return primary
	}
	return languages.Unknown
}
