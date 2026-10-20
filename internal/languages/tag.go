package languages

import (
	"regexp"
	"strings"
)

// TagWidth is the most characters a language tag may have where the catalog
// keeps one (com_nalet_katalog_subtitleassets.lang): a language with its
// script and region ("zh-Hant-TW") fits.
const TagWidth = 10

var (
	primaryRE = regexp.MustCompile(`^[a-z]{2,3}$`)
	scriptRE  = regexp.MustCompile(`^[a-z]{4}$`)
	regionRE  = regexp.MustCompile(`^([a-z]{2}|[0-9]{3})$`)
	variantRE = regexp.MustCompile(`^([a-z0-9]{5,8}|[0-9][a-z0-9]{3})$`)
)

// Tag spells a language as a BCP 47 tag the way people's software reads one:
// the language's ISO 639-1 code where it has one ("de" of "de", "deu" and
// "ger"), else its ISO 639-2/T code, then its script in title case
// ("zh-Hans"), its region in capitals ("pt-BR") and any variant in lower
// case; "_" is taken for "-". It reports false for what names no language a
// subtitle can be in (und, zxx, mul, mis, an empty tag), for a tag of other
// subtags than those, and for one longer than TagWidth.
func Tag(code string) (string, bool) {
	parts := strings.Split(strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "_", "-")), "-")
	primary := parts[0]
	if !primaryRE.MatchString(primary) {
		return "", false
	}
	switch primary {
	case Unknown, NoDialogue, "mul", "mis":
		return "", false
	}
	if len(primary) == 3 {
		if t, ok := fromBibliographic[primary]; ok {
			primary = t
		}
		if one, ok := toISO6391[primary]; ok {
			primary = one
		}
	} else if t, ok := fromISO6391[primary]; ok {
		primary = toISO6391[t] // a withdrawn code ("iw") as the one that replaced it ("he")
	}
	out := []string{primary}
	seen := 0 // 1 after a script, 2 after a region, 3 in the variants
	for _, p := range parts[1:] {
		switch {
		case seen < 1 && scriptRE.MatchString(p):
			out = append(out, strings.ToUpper(p[:1])+p[1:])
			seen = 1
		case seen < 2 && regionRE.MatchString(p):
			out = append(out, strings.ToUpper(p))
			seen = 2
		case variantRE.MatchString(p):
			out = append(out, p)
			seen = 3
		default:
			return "", false
		}
	}
	tag := strings.Join(out, "-")
	if len(tag) > TagWidth {
		return "", false
	}
	return tag, true
}

// toISO6391 maps an ISO 639-2/T code to the ISO 639-1 code of its language.
var toISO6391 = func() map[string]string {
	m := make(map[string]string, len(fromISO6391))
	for one, t := range fromISO6391 {
		switch one {
		case "iw", "in", "ji", "jw", "mo": // withdrawn: the current code wins
			continue
		}
		m[t] = one
	}
	return m
}()

// fromBibliographic maps the ISO 639-2/B codes that differ from their
// terminology form to it ("ger" to "deu").
var fromBibliographic = map[string]string{
	"alb": "sqi", "arm": "hye", "baq": "eus", "bur": "mya", "chi": "zho", "cze": "ces", "dut": "nld",
	"fre": "fra", "geo": "kat", "ger": "deu", "gre": "ell", "ice": "isl", "mac": "mkd", "mao": "mri",
	"may": "msa", "per": "fas", "rum": "ron", "slo": "slk", "tib": "bod", "wel": "cym",
}
