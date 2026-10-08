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
// compile and behave equivalently for the ASCII tokens used here. An episode's
// number is read by episodeTokens (below), which reads a range too.
var (
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
	if hasSeriesFolder(rel) || hasEpisodeToken(rel) {
		return "episode"
	}
	return "movie"
}

// A name numbers an episode as S05E15, its season and its number, letter
// case aside, the token standing apart from the words around it (no letter,
// digit or underscore right before or after). One file that covers several
// episodes numbers the others after it, each in the order they air: S05E15E16,
// S05E15-E16 (or .E16, _E16, " E16"), S05E15-16, and so on (S05E15-E17,
// S05E15E16E17), and it covers every episode from its first to its last. A
// number after a dash followed by a digit or a p is none (S05E15-720p is the
// fifteenth episode in 720p), and so is one that does not come after the one
// before it, or that would make the file cover more than maxCovered
// episodes: the name then numbers the episodes before it.
var (
	episodeHead = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})`)
	episodeMore = regexp.MustCompile(`(?i)^(?:[ ._-]?E|-)(\d{1,3})`)
)

// maxCovered is the most episodes one file covers.
const maxCovered = 10

// episodeToken is where a name numbers its episodes: the token's bytes in the
// name, its season, and the first and the last episode the file covers (the
// same for a file of one).
type episodeToken struct {
	start, end          int
	season, first, last int
}

// episodeTokens are the tokens of name that number episodes, in order.
func episodeTokens(name string) []episodeToken {
	var out []episodeToken
	for _, m := range episodeHead.FindAllStringSubmatchIndex(name, -1) {
		end := m[1]
		if end < len(name) && isDigit(name[end]) {
			continue // E1500: no episode of a season numbers so
		}
		season, _ := strconv.Atoi(name[m[2]:m[3]])
		first, _ := strconv.Atoi(name[m[4]:m[5]])
		tok := episodeToken{start: m[0], season: season, first: first}
		// The episodes after the first, each where the one before ends.
		type more struct{ end, last int }
		var after []more
		at, last := end, first
		for {
			x := episodeMore.FindStringSubmatchIndex(name[at:])
			if x == nil {
				break
			}
			n, _ := strconv.Atoi(name[at+x[2] : at+x[3]])
			next := at + x[1]
			dashed := name[at] == '-' && isDigit(name[at+1])
			if next < len(name) && (isDigit(name[next]) || (dashed && (name[next] == 'p' || name[next] == 'P'))) {
				break // -720p, -1080: no episode
			}
			if n <= last || n-first+1 > maxCovered {
				break
			}
			after = append(after, more{next, n})
			at, last = next, n
		}
		// The token stands apart where it ends: the longest of it that does.
		for k := len(after); k >= 0; k-- {
			tok.end, tok.last = end, first
			if k > 0 {
				tok.end, tok.last = after[k-1].end, after[k-1].last
			}
			if tok.end == len(name) || !isWordByte(name[tok.end]) {
				out = append(out, tok)
				break
			}
		}
	}
	return out
}

// firstEpisodeToken is the first token of name that numbers episodes.
func firstEpisodeToken(name string) (episodeToken, bool) {
	toks := episodeTokens(name)
	if len(toks) == 0 {
		return episodeToken{}, false
	}
	return toks[0], true
}

// hasEpisodeToken reports whether s numbers an episode.
func hasEpisodeToken(s string) bool { return len(episodeTokens(s)) > 0 }

// withoutEpisodeTokens is s with every token that numbers episodes replaced
// by a space.
func withoutEpisodeTokens(s string) string {
	toks := episodeTokens(s)
	for i := len(toks) - 1; i >= 0; i-- {
		s = s[:toks[i].start] + " " + s[toks[i].end:]
	}
	return s
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isWordByte is a byte of a word as \b reads one: a letter, a digit or an
// underscore.
func isWordByte(b byte) bool {
	return isDigit(b) || b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
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
	if tok, ok := firstEpisodeToken(base); ok {
		base = base[:tok.start]
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
		name = withoutEpisodeTokens(name)
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

// episodeCoords parses the SxxEyy token from a filename: its season and the
// first episode it numbers, which the file belongs to. Returns nil pointers
// when absent.
func episodeCoords(name string) (season, episode *int32) {
	tok, ok := firstEpisodeToken(name)
	if !ok {
		return nil, nil
	}
	s, e := int32(tok.season), int32(tok.first)
	return &s, &e
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
