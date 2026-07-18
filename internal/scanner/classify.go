package scanner

import (
	"regexp"
	"strconv"
	"strings"
)

// Extension sets (lowercased, with leading dot) — mirror NfsScanner.VIDEO_EXTS /
// AUDIO_EXTS / SUB_EXTS exactly.
var (
	videoExts = map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".m4v": true, ".webm": true}
	audioExts = map[string]bool{".flac": true, ".mp3": true, ".ogg": true, ".m4a": true, ".opus": true, ".wav": true}
	subExts   = map[string]bool{".srt": true, ".vtt": true, ".ass": true, ".ssa": true}
)

// Patterns — ported verbatim from NfsScanner. Go's regexp (RE2) does not support
// \b at the engine level the same way Java does; RE2 DOES support \b, so these
// compile and behave equivalently for the ASCII tokens used here.
var (
	episodePattern   = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)
	parenYearPattern = regexp.MustCompile(`\(((?:19|20)\d{2})\)`)
	yearPattern      = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
	cleanupPattern   = regexp.MustCompile(`[._]+`)
	// LANG_SUFFIX = \.([a-zA-Z]{2,3}(?:[-_][a-zA-Z]{2,4})?)$
	langSuffix = regexp.MustCompile(`\.([a-zA-Z]{2,3}(?:[-_][a-zA-Z]{2,4})?)$`)
	// bare-year strip used in extractTitle: Java is replaceAll("\\s+(?:19|20)\\d{2}(?=\\s|$)","")
	// — a GLOBAL strip of a bare year token followed by whitespace OR end-of-string,
	// anywhere in the name. RE2 lacks lookahead, so capture the trailing separator and
	// re-emit it ($1), applied globally with ReplaceAllString.
	trailingBareYear = regexp.MustCompile(`\s+(?:19|20)\d{2}(\s|$)`)
)

// classify reproduces NfsScanner.classify(rel, isVideo, isAudio).
// rel is the path relative to the scan root, using '/' separators.
func classify(rel string, isVideo, isAudio bool) string {
	if isAudio {
		return "track"
	}
	if hasSeriesFolder(rel) || episodePattern.MatchString(rel) {
		return "episode"
	}
	return "movie"
}

// seriesFolderNames are the path segments that mark a TV library subtree. A file
// anywhere under one of these is treated as episodic (the segment after it is the
// show name — see seriesTitleFor). Matching whole segments (not a substring) so a
// top-level "series/…" is caught, not only a nested "/series/…".
var seriesFolderNames = map[string]bool{"series": true, "tv": true, "shows": true, "tvshows": true}

func hasSeriesFolder(rel string) bool {
	for _, seg := range strings.Split(strings.ToLower(rel), "/") {
		if seriesFolderNames[seg] {
			return true
		}
	}
	return false
}

// seriesTitleFor derives the show name for an episode file. It prefers the folder
// immediately under a series/tv/shows parent (the conventional
// "series/<Show>/<file>" layout); failing that it strips the SxxEyy token and
// everything after it from the filename. The result is normalised like a title so
// it matches the series parent's TMDB search.
func seriesTitleFor(rel, filename string) string {
	parts := strings.Split(rel, "/")
	for i := 0; i+1 < len(parts); i++ {
		if seriesFolderNames[strings.ToLower(parts[i])] {
			if t := showTitle(parts[i+1]); t != "" {
				return t
			}
		}
	}
	base := stripExt(filename)
	if loc := episodePattern.FindStringIndex(base); loc != nil {
		base = base[:loc[0]]
	}
	return showTitle(base)
}

// showTitle normalises a raw folder/filename fragment into a clean show title:
// separators to spaces, a (year) or trailing bare year stripped, then cleanTitle.
func showTitle(raw string) string {
	name := cleanupPattern.ReplaceAllString(raw, " ")
	if parenYearPattern.MatchString(name) {
		name = parenYearPattern.ReplaceAllString(name, "")
	} else {
		name = trailingBareYear.ReplaceAllString(name, "$1")
	}
	return cleanTitle(collapseWS(name))
}

// extractTitle reproduces NfsScanner.extractTitle(filename, type).
func extractTitle(filename, typ string) string {
	name := filename
	if dot := strings.LastIndex(name, "."); dot > 0 {
		name = name[:dot]
	}
	name = cleanupPattern.ReplaceAllString(name, " ")
	if parenYearPattern.MatchString(name) {
		name = parenYearPattern.ReplaceAllString(name, "")
	} else {
		name = trailingBareYear.ReplaceAllString(name, "$1")
	}
	replacer := strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ")
	name = replacer.Replace(name)
	if typ == "episode" {
		name = episodePattern.ReplaceAllString(name, " ")
	}
	name = collapseWS(name)
	return cleanTitle(name)
}

// extractYear reproduces NfsScanner.extractYear(s): prefer the parenthesised
// year, else the first bare 4-digit year.
func extractYear(s string) *int32 {
	if m := parenYearPattern.FindStringSubmatch(s); m != nil {
		if y, err := strconv.Atoi(m[1]); err == nil {
			v := int32(y)
			return &v
		}
		return nil
	}
	if m := yearPattern.FindString(s); m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			v := int32(y)
			return &v
		}
	}
	return nil
}

// episodeCoords parses the SxxEyy token from a filename (group1=season,
// group2=episode). Returns nil pointers when absent / unparseable.
func episodeCoords(name string) (season, episode *int32) {
	m := episodePattern.FindStringSubmatch(name)
	if m == nil {
		return nil, nil
	}
	if s, err := strconv.Atoi(m[1]); err == nil {
		v := int32(s)
		season = &v
	}
	if e, err := strconv.Atoi(m[2]); err == nil {
		v := int32(e)
		episode = &v
	}
	return season, episode
}

// isTrailerPath reproduces NfsScanner.isTrailerPath(absPath, filename).
func isTrailerPath(absPath, filename string) bool {
	lower := strings.ToLower(filename)
	base := lower
	if dot := strings.LastIndex(lower, "."); dot > 0 {
		base = lower[:dot]
	}
	if base == "trailer" {
		return true
	}
	if strings.HasSuffix(base, "-trailer") || strings.HasSuffix(base, ".trailer") ||
		strings.HasSuffix(base, "_trailer") || strings.HasSuffix(base, " trailer") {
		return true
	}
	return strings.Contains(strings.ToLower(absPath), "/trailers/")
}

// stripExt mirrors NfsScanner.stripExt.
func stripExt(name string) string {
	if dot := strings.LastIndex(name, "."); dot >= 0 {
		return name[:dot]
	}
	return name
}

// collapseWS replaces runs of whitespace with a single space and trims.
func collapseWS(s string) string {
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(s, " "))
}

// languageLabel reproduces NfsScanner.languageLabel(lang).
func languageLabel(lang string) string {
	l := strings.ToLower(strings.ReplaceAll(lang, "_", "-"))
	primary := l
	if dash := strings.IndexByte(l, '-'); dash >= 0 {
		primary = l[:dash]
	}
	switch primary {
	case "en", "eng":
		return "English"
	case "de", "deu", "ger":
		return "Deutsch"
	case "fr", "fra", "fre":
		return "Français"
	case "es", "spa":
		return "Español"
	case "it", "ita":
		return "Italiano"
	case "pt", "por":
		return "Português"
	case "nl", "nld", "dut":
		return "Nederlands"
	case "ja", "jpn":
		return "日本語"
	case "zh", "chi", "zho":
		return "中文"
	case "ko", "kor":
		return "한국어"
	case "ru", "rus":
		return "Русский"
	case "pl", "pol":
		return "Polski"
	case "tr", "tur":
		return "Türkçe"
	case "ar", "ara":
		return "العربية"
	case "sv", "swe":
		return "Svenska"
	case "no", "nor":
		return "Norsk"
	case "da", "dan":
		return "Dansk"
	case "fi", "fin":
		return "Suomi"
	default:
		return strings.ToUpper(primary)
	}
}

// cleanTokenPatterns ports EnrichmentService.cleanTitle's token list. Each is
// matched case-insensitively and replaced with a single space. Order matters:
// composite tokens (WEBDL-1080p) before single tokens. The (?i) flag is baked in
// at compile time.
var cleanTokenPatterns = compileCleanTokens([]string{
	`\bRemux-?\d+p?\b`, `\bWEB[ -]?DL[ -]?\d+p?\b`,
	`\bWEB[ -]?Rip[ -]?\d+p?\b`, `\bBluray-?\d+p?\b`,
	`\bHDTV-?\d+p?\b`, `\bBDRip-?\d+p?\b`,
	`\bDVDRip\b`, `\bDVDScr\b`, `\bBRRip\b`,
	`\b\d{3,4}p\b`, `\b\d{3,4}i\b`,
	`\b(?:2160|1080|720|480)p\b`,
	`\bWEB[ -]?DL\b`, `\bWEB\b`, `\bBluray\b`,
	`\bSDTV\b`, `\bDVD\b`, `\bTELESYNC\b`, `\bProper\b`,
	`\bRepack\b`, `\bRemastered\b`, `\bInternal\b`, `\bLimited\b`,
	`\bHDR(?:10\+?)?\b`, `\bDV\b`, `\bDolby[ -]?Vision\b`,
	`\b(?:h|x)\.?26[45]\b`, `\bHEVC\b`, `\bAVC\b`,
	`\bDTS(?:[ -]?HD)?\b`, `\bDDP?5\.1\b`, `\bAAC\b`,
	`\bTrueHD\b`, `\bAtmos\b`,
	`\bIMAX\b`, `\b4K\b`, `\bUHD\b`,
	`\bExtended\b`, `\bDirector'?s? Cut\b`, `\bUnrated\b`,
	`\bMultiSubs?\b`, `\bMulti\b`, `\bDual[ -]?Audio\b`,
	`\b(?:Eng|Ger|Fre|Spa|Ita|Jpn|Chi)(?:Sub|Audio)?\b`,
})

var (
	cleanBrackets    = regexp.MustCompile(`[\[\](){}]`)
	cleanTrailingSep = regexp.MustCompile(`[-_.]+\s*$`)
)

func compileCleanTokens(toks []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(toks))
	for i, t := range toks {
		out[i] = regexp.MustCompile(`(?i)` + t)
	}
	return out
}

// cleanTitle ports EnrichmentService.cleanTitle (public static, shared with the
// scanner). The tmdb package is being written concurrently and is not importable
// here, so the logic is reproduced verbatim. Returns the original when the result
// would be empty.
func cleanTitle(raw string) string {
	s := raw
	for _, re := range cleanTokenPatterns {
		s = re.ReplaceAllString(s, " ")
	}
	s = cleanBrackets.ReplaceAllString(s, " ")
	s = cleanTrailingSep.ReplaceAllString(s, " ")
	s = collapseWS(s)
	if s == "" {
		return raw
	}
	return s
}
