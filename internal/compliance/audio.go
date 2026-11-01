package compliance

// The packager's rules of a title's audio tracks, as it applies them when it
// packages one (packager.py): the key it tells a track's language by, which
// tracks a client is offered under the catalog's language settings, which
// one is the default, which are for the film as it plays — no commentary, no
// audio description — and so which get a 5.1 companion. The check reads them
// to tell what processing a title again makes of its original's audio
// (planAudio), each as the packager has it, the words of a track's title
// read as its record logic reads them.

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// Which tracks the packager makes a 5.1 companion of, as its report says
// (surroundTracks).
const (
	// tracksEvery is a packager that doesn't report which: it is planned as
	// the check planned it before (oldCommentary), but for the language
	// table, which follows the packager's now (languageKey).
	tracksEvery = "every"
	// tracksShown is the surround tracks a client is offered and the
	// default one, a commentary or an audio description never; of a title
	// none of whose tracks shown is surround, its first surround track.
	tracksShown = "shown"
)

// tracks is which tracks the packager makes a 5.1 companion of, as it
// reports it: tracksShown of "shown"; tracksEvery where it reports none (or
// a blank); "" of what the check doesn't know, which it can't plan.
func (p *PackagerRules) tracks() string {
	if p.SurroundTracks == nil {
		return tracksEvery
	}
	switch v := strings.ToLower(strings.TrimSpace(*p.SurroundTracks)); v {
	case "":
		return tracksEvery
	case tracksShown:
		return tracksShown
	}
	return ""
}

// The catalog's settings the packager reads which tracks a client is
// offered by.
const (
	settingWhitelist    = "packager.language_whitelist"
	settingKeepOriginal = "packager.keep_original_if_single"
)

// languageSettings are the settings the packager shows a title's tracks by,
// as it reads them at every run (worker.py, _parse_settings, of GET
// /api/settings): its language whitelist (packager.language_whitelist,
// comma-separated, each language without the spaces around it, in lower
// case; none shows every track), and whether a title of a single language
// the whitelist hides shows it all (packager.keep_original_if_single: true,
// 1 or yes; true where it is not set, and false of a blank).
type languageSettings struct {
	whitelist    []string
	keepOriginal bool
}

// languageSettingsOf are the packager's language settings of the settings
// as the workers read them (model.WorkerSettings).
func languageSettingsOf(settings map[string]*model.Setting) languageSettings {
	l := languageSettings{keepOriginal: true}
	if s, ok := settings[settingWhitelist]; ok {
		for _, part := range strings.Split(s.ValueText, ",") {
			if w := strings.ToLower(strip(part)); w != "" {
				l.whitelist = append(l.whitelist, w)
			}
		}
	}
	if s, ok := settings[settingKeepOriginal]; ok {
		switch strings.ToLower(strip(s.ValueText)) {
		case "true", "1", "yes":
		default:
			l.keepOriginal = false
		}
	}
	return l
}

// key is the settings in one line, for the hash of the rules a title is
// checked against.
func (l languageSettings) key() string {
	keep := "false"
	if l.keepOriginal {
		keep = "true"
	}
	return "whitelist=" + strings.Join(l.whitelist, ",") + " keepOriginal=" + keep
}

// strip is s without the whitespace around it, as Python's str.strip()
// takes whitespace: Unicode's, and the separators U+001C to U+001F.
func strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// packagerISO6391 is the packager's own table of the ISO 639-2 codes it
// reads as an ISO 639-1 one (packager.py, _ISO639_1): a code it lacks is a
// key of its own ("arm", where languages.Tag reads "hy").
var packagerISO6391 = map[string]string{
	"eng": "en", "deu": "de", "ger": "de", "fra": "fr", "fre": "fr",
	"spa": "es", "ita": "it", "jpn": "ja", "zho": "zh", "chi": "zh",
	"por": "pt", "rus": "ru", "nld": "nl", "dut": "nl", "swe": "sv",
	"nor": "no", "nob": "nb", "nno": "nn", "dan": "da", "fin": "fi",
	"pol": "pl", "ces": "cs", "cze": "cs", "slk": "sk", "slo": "sk",
	"hun": "hu", "ron": "ro", "rum": "ro", "tur": "tr", "ell": "el",
	"gre": "el", "heb": "he", "ara": "ar", "hin": "hi", "kor": "ko",
	"tha": "th", "vie": "vi", "ukr": "uk", "hrv": "hr", "srp": "sr",
	"slv": "sl", "bul": "bg", "cat": "ca", "ind": "id", "msa": "ms",
	"may": "ms", "fas": "fa", "per": "fa", "isl": "is", "ice": "is",
	"roh": "rm", "lat": "la", "est": "et", "lav": "lv", "lit": "lt",
}

// languageKey is the key the packager tells a language by (packager.py,
// _lang_key): the primary subtag of a tag ("de" of "de-CH", "_" taken for
// "-"), in lower case, as packagerISO6391 reads a three-letter one ("de" of
// "ger" and "deu"), else as it is ("gsw"); "und" of none.
func languageKey(tag string) string {
	if tag == "" {
		tag = languages.Unknown
	}
	t, _, _ := strings.Cut(strings.ReplaceAll(strings.ToLower(strip(tag)), "_", "-"), "-")
	if one, ok := packagerISO6391[t]; ok {
		t = one
	}
	if t == "" {
		return languages.Unknown
	}
	return t
}

// noLanguage are the keys of a track that names no language (packager.py,
// _NO_LANGUAGE): undetermined, and no linguistic content. No whitelist hides
// one.
var noLanguage = map[string]bool{languages.Unknown: true, languages.NoDialogue: true}

// audioTrack is an audio stream of an original as the packager reads it: its
// channels, its codec and bit rate, the key of its language (the one an
// admin set for it, else its file's tag), its title, and its dispositions.
type audioTrack struct {
	channels                                float64
	codec                                   string
	bitrate                                 *float64
	key, title                              string
	flagged, comment, impaired, description bool
}

// audioTracks are the original's audio streams as its record lists them, in
// their order, as the packager reads them: a track's language is the one an
// admin set for it (t.AudioLanguages, by its place among them, a code of
// three letters as the packager takes one), else the code its file carried
// (the record's languageRaw, else its language), else none.
func audioTracks(t title, r records) []audioTrack {
	if r.src == nil {
		return nil
	}
	var out []audioTrack
	for _, s := range r.src.Streams {
		if s.Type != "audio" {
			continue
		}
		tag := ""
		switch o, ok := t.AudioLanguages[len(out)]; {
		case ok && languages.IsCode(strings.ToLower(strip(o))):
			tag = o
		case s.LanguageRaw != nil:
			tag = *s.LanguageRaw
		case s.Language != nil:
			tag = *s.Language
		}
		a := audioTrack{bitrate: s.Bitrate, key: languageKey(tag), flagged: s.Dispositions.Default,
			comment: s.Dispositions.Commentary, impaired: s.Dispositions.VisualImpaired,
			description: s.Dispositions.Descriptions}
		if s.Channels != nil {
			a.channels = *s.Channels
		}
		if s.Codec != nil {
			a.codec = *s.Codec
		}
		if s.Title != nil {
			a.title = *s.Title
		}
		out = append(out, a)
	}
	return out
}

// The words a title says a track is a commentary with, and those that take
// them back, as the record logic reads them (libv2_records, record logic 9:
// COMMENTARY_WORDS, NO_COMMENTARY_RE): one of them right after a negation,
// or past articles and possessives alone ("No commentary", "without the
// director's commentary", "ohne Kommentar", "keine Kommentare", "sans
// commentaire", "sin comentarios", "non-commentary"), or a word that says
// it itself ("Kommentarlos", "commentary-free"). A negation of anything
// else leaves the commentary one ("No-spoiler commentary", "Commentary with
// no music"). Python's \b and \s are Unicode's: a word's letters and
// digits are any script's, a space any Unicode space, and the start of a
// negation is the start of the title or a character of no word, which the
// match takes with it (RE2 has no look-behind), so that a negation reads the
// same.
const (
	commentaryWords = `commentary|kommentar|commentaire|comentario|commento|commentaar`
	pySpace         = `\s\x0b\x1c-\x1f\x85\p{Z}`
	pyWord          = `\p{L}\p{N}_`
	// articleOrPossessive is a word that may stand between a negation and
	// the word it takes back, three at most (_ARTICLE_OR_POSSESSIVE)
	articleOrPossessive = `(?:the|any|a|an|audio|director(?:'|’)?s|des|del|di|van|de|der|die|das|le|la|el)[` + pySpace + `\-]+`
)

var (
	commentaryRE   = regexp.MustCompile(`(?i)` + commentaryWords)
	noCommentaryRE = regexp.MustCompile(`(?i)(?:(?:^|[^` + pyWord + `])(?:(?:no|without|w/o|ohne|keine?[nrs]?|sans|sin|senza|zonder|geen)` +
		`[` + pySpace + `\-:]+(?:` + articleOrPossessive + `){0,3}(?:` + commentaryWords + `|comments?)` +
		`|non[` + pySpace + `\-]?(?:` + commentaryWords + `))` +
		`|kommentar[` + pySpace + `\-]?(?:los|frei)|commentary[` + pySpace + `\-]?free)`)
	// descriptionRE is a title that says a track is an audio description
	// (libv2_records' audio_purpose), of the title in lower case.
	descriptionRE = regexp.MustCompile(`audio description|descriptive (?:video|audio)|audiodeskription|audiodescription`)
)

// saysCommentary reports whether a title says its track is a commentary, as
// the record logic reads it (says_commentary): one of its commentary words
// that no negation takes back.
func saysCommentary(title string) bool {
	return commentaryRE.MatchString(noCommentaryRE.ReplaceAllString(title, " "))
}

// What an audio track is for, as the record logic reads it
// (libv2_records' audio_purpose).
const (
	purposeMain        = "main"
	purposeCommentary  = "commentary"
	purposeDescription = "description"
)

// purpose is what a track is for, as the record logic reads it
// (libv2_records, record logic 9: audio_purpose), the one rule the check
// tells a commentary and an audio description by: a commentary by its
// disposition (comment); an audio description by its own (visual_impaired,
// descriptions); else a commentary by its title (saysCommentary: "Director's
// Commentary", "Comentario", not "No commentary"); else an audio
// description by its title ("Audio Description", "Audiodeskription"); else
// the film's main audio.
func (a audioTrack) purpose() string {
	switch t := strings.ToLower(a.title); {
	case a.comment:
		return purposeCommentary
	case a.impaired || a.description:
		return purposeDescription
	case saysCommentary(t):
		return purposeCommentary
	case descriptionRE.MatchString(t):
		return purposeDescription
	}
	return purposeMain
}

// forTheFilm reports whether a track is for the film as it plays
// (packager.py, _is_main_audio): neither a commentary nor an audio
// description (purpose). Neither is ever the default where its language has
// such a track, nor any language's 5.1 companion.
func (a audioTrack) forTheFilm() bool { return a.purpose() == purposeMain }

// oldCommentary reports whether the check took a track for a commentary,
// which gets no 5.1 companion, before packagers reported which tracks get
// one: its disposition says so, or its title holds "comment" or
// "kommentar". A packager that doesn't report surroundTracks is planned as
// the check planned it before (tracksEvery), by this; one that reports it
// is planned by what a track is for as the record logic reads it (purpose).
func (a audioTrack) oldCommentary() bool {
	t := strings.ToLower(a.title)
	return a.comment || strings.Contains(t, "comment") || strings.Contains(t, "kommentar")
}

// shownTracks are the tracks a client is offered under the language settings
// l (packager.py, _visible_indices), by their place: every track without a
// whitelist; else a track of no language (und, zxx) and one of a language the
// whitelist names, by their keys; and where that is none and
// keep_original_if_single holds, every track of a title whose tracks name a
// single language.
func shownTracks(tracks []audioTrack, l languageSettings) []bool {
	shown := make([]bool, len(tracks))
	if len(l.whitelist) == 0 {
		for i := range shown {
			shown[i] = true
		}
		return shown
	}
	wanted := map[string]bool{}
	for _, w := range l.whitelist {
		wanted[languageKey(w)] = true
	}
	some := false
	for i, a := range tracks {
		if noLanguage[a.key] || wanted[a.key] {
			shown[i], some = true, true
		}
	}
	if some || !l.keepOriginal {
		return shown
	}
	distinct := map[string]bool{}
	for _, a := range tracks {
		if !noLanguage[a.key] {
			distinct[a.key] = true
		}
	}
	if len(distinct) == 1 {
		for i := range shown {
			shown[i] = true
		}
	}
	return shown
}

// defaultTrack is the place of the track the packager makes the default one
// (packager.py, _pick_default_audio), -1 of none: of the first preferred
// language a track is of, else of every track, the first by its rank —
// shown before hidden, for the film before not, flagged default before not,
// then in their order. Its preferred languages are those it reports
// (preferredLanguages: its PREFERRED_LANGUAGES), else the whitelist's in
// its order (preferredOf).
func defaultTrack(tracks []audioTrack, shown []bool, preferred []string) int {
	if len(tracks) == 0 {
		return -1
	}
	before := func(i, j int) bool {
		a, b := tracks[i], tracks[j]
		switch {
		case shown[i] != shown[j]:
			return shown[i]
		case a.forTheFilm() != b.forTheFilm():
			return a.forTheFilm()
		case a.flagged != b.flagged:
			return a.flagged
		}
		return i < j
	}
	first := func(of func(int) bool) int {
		best := -1
		for i := range tracks {
			if of(i) && (best < 0 || before(i, best)) {
				best = i
			}
		}
		return best
	}
	for _, p := range preferred {
		want := languageKey(p)
		if i := first(func(i int) bool { return tracks[i].key == want }); i >= 0 {
			return i
		}
	}
	return first(func(int) bool { return true })
}

// companionTracks are the places of the tracks the packager makes a 5.1
// companion of, in their order (packager.py, _surround_plan): of each
// language the first track of more than two channels that is no commentary
// (oldCommentary) where it makes one of every surround track (tracksEvery);
// where it makes one of the tracks shown (tracksShown), of each language the
// first such track that is for the film (forTheFilm) and that a client is
// offered under the language settings l (shownTracks) or is the default one
// (defaultTrack, of the languages preferred), and where none is, the first
// track of more than two channels for the film, hidden though it is. A
// hidden track gets none where another gets one, though its stereo
// rendition is made; a title whose only surround tracks are hidden keeps a
// 5.1, of the first; a commentary or an audio description never gets one.
func companionTracks(tracks []audioTrack, which string, l languageSettings, preferred []string) []int {
	var shown []bool
	def := -1
	if which == tracksShown {
		shown = shownTracks(tracks, l)
		def = defaultTrack(tracks, shown, preferred)
	}
	var out []int
	seen := map[string]bool{}
	for i, a := range tracks {
		if a.channels <= 2 {
			continue
		}
		switch which {
		case tracksShown:
			if !a.forTheFilm() || !shown[i] && i != def {
				continue
			}
		default:
			if a.oldCommentary() {
				continue
			}
		}
		if seen[a.key] {
			continue
		}
		seen[a.key] = true
		out = append(out, i)
	}
	if which == tracksShown && len(out) == 0 {
		for i, a := range tracks {
			if a.channels > 2 && a.forTheFilm() {
				return []int{i}
			}
		}
	}
	return out
}

// preferredOf are the languages the packager p picks the default track of
// first (packager.py, build_package): those it reports (preferredLanguages,
// its PREFERRED_LANGUAGES), else the whitelist's of l, in its order — as a
// packager that reports none prefers, one that sets none.
func (p *PackagerRules) preferredOf(l languageSettings) []string {
	if len(p.PreferredLanguages) > 0 {
		return p.PreferredLanguages
	}
	return l.whitelist
}
