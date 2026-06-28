package tmdb

import (
	"regexp"
	"strings"
)

// tokenPatterns is the case-insensitive release-group / quality / codec token
// list, ported verbatim from EnrichmentService.cleanTitle (composite tokens
// before single tokens; order is load-bearing). Each is compiled with the
// (?i) inline flag.
var tokenPatterns = func() []*regexp.Regexp {
	raw := []string{
		`\bRemux-?\d+p?\b`, `\bWEB[ -]?DL[ -]?\d+p?\b`,
		`\bWEB[ -]?Rip[ -]?\d+p?\b`, `\bBluray-?\d+p?\b`,
		`\bHDTV-?\d+p?\b`, `\bBDRip-?\d+p?\b`,
		`\bDVDRip\b`, `\bDVDScr\b`, `\bBRRip\b`,
		`\b\d{3,4}p\b`, `\b\d{3,4}i\b`,
		`\b(2160|1080|720|480)p\b`,
		`\bWEB[ -]?DL\b`, `\bWEB\b`, `\bBluray\b`,
		`\bSDTV\b`, `\bDVD\b`, `\bTELESYNC\b`, `\bProper\b`,
		`\bRepack\b`, `\bRemastered\b`, `\bInternal\b`, `\bLimited\b`,
		`\bHDR(10\+?)?\b`, `\bDV\b`, `\bDolby[ -]?Vision\b`,
		`\b(?:h|x)\.?26[45]\b`, `\bHEVC\b`, `\bAVC\b`,
		`\bDTS(?:[ -]?HD)?\b`, `\bDDP?5\.1\b`, `\bAAC\b`,
		`\bTrueHD\b`, `\bAtmos\b`,
		`\bIMAX\b`, `\b4K\b`, `\bUHD\b`,
		`\bExtended\b`, `\bDirector'?s? Cut\b`, `\bUnrated\b`,
		`\bMultiSubs?\b`, `\bMulti\b`, `\bDual[ -]?Audio\b`,
		`\b(?:Eng|Ger|Fre|Spa|Ita|Jpn|Chi)(?:Sub|Audio)?\b`,
	}
	out := make([]*regexp.Regexp, len(raw))
	for i, p := range raw {
		out[i] = regexp.MustCompile(`(?i)` + p)
	}
	return out
}()

var (
	bracketRE   = regexp.MustCompile(`[\[\](){}]`)
	trailingRE  = regexp.MustCompile(`[-_.]+\s*$`)
	whitespceRE = regexp.MustCompile(`\s+`)
)

// cleanTitle strips Sonarr/Radarr-style release-group / quality / codec tokens
// from a filename title before TMDB search. Ported from
// EnrichmentService.cleanTitle (public static, shared with the scanner). Returns
// the original string if the cleaned result is empty.
func cleanTitle(raw string) string {
	s := raw
	for _, re := range tokenPatterns {
		s = re.ReplaceAllString(s, " ")
	}
	s = bracketRE.ReplaceAllString(s, " ")
	s = trailingRE.ReplaceAllString(s, " ")
	s = strings.TrimSpace(whitespceRE.ReplaceAllString(s, " "))
	if s == "" {
		return raw
	}
	return s
}
