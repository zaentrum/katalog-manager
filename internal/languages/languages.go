// Package languages is how the catalog spells the language of a track: an
// ISO 639-2 code, three lowercase letters, in its bibliographic (B, "ger") or
// its terminology form (T, "deu"), both taken as they are. Two codes name no
// language one could follow: NoDialogue (zxx, no linguistic content: a film
// without dialogue) and Unknown (und, undetermined), which a track is when
// nothing says what it is.
package languages

import (
	"regexp"
	"strings"
)

const (
	// NoDialogue is zxx, ISO 639-2's "no linguistic content": a film, or a
	// track, without dialogue. People read it as "No dialogue".
	NoDialogue = "zxx"
	// Unknown is und, ISO 639-2's "undetermined": a track nothing says the
	// language of. People read it as "Unknown".
	Unknown = "und"
)

var codeRE = regexp.MustCompile(`^[a-z]{3}$`)

// IsCode reports whether s is a language as a track's language is set: three
// lowercase letters, as an ISO 639-2 code is (zxx and und among them).
func IsCode(s string) bool { return codeRE.MatchString(s) }

// Effective is the language a track plays as: the one an admin set
// (override), else the one its source tags it with, else Unknown.
func Effective(override, source *string) string {
	if override != nil && *override != "" {
		return *override
	}
	if source != nil {
		if s := strings.TrimSpace(*source); s != "" {
			return s
		}
	}
	return Unknown
}

// ISO6392 spells a language code as a three-letter ISO 639-2 code: a
// three-letter code as it is (lowercased), an ISO 639-1 code ("de", or "iw"
// withdrawn since) as its ISO 639-2/T code ("deu"); a region or script after
// it ("pt-BR", "zh_Hant") does not count. Anything else, an empty code
// included, is Unknown.
func ISO6392(code string) string {
	primary := strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(primary, "-_"); i >= 0 {
		primary = primary[:i]
	}
	switch {
	case IsCode(primary):
		return primary
	case len(primary) == 2:
		if t, ok := fromISO6391[primary]; ok {
			return t
		}
	}
	return Unknown
}

// fromISO6391 maps every ISO 639-1 code, and the withdrawn ones still found
// in file names, to its ISO 639-2/T code.
var fromISO6391 = map[string]string{
	"aa": "aar", "ab": "abk", "ae": "ave", "af": "afr", "ak": "aka", "am": "amh", "an": "arg", "ar": "ara",
	"as": "asm", "av": "ava", "ay": "aym", "az": "aze",
	"ba": "bak", "be": "bel", "bg": "bul", "bh": "bih", "bi": "bis", "bm": "bam", "bn": "ben", "bo": "bod",
	"br": "bre", "bs": "bos",
	"ca": "cat", "ce": "che", "ch": "cha", "co": "cos", "cr": "cre", "cs": "ces", "cu": "chu", "cv": "chv",
	"cy": "cym",
	"da": "dan", "de": "deu", "dv": "div", "dz": "dzo",
	"ee": "ewe", "el": "ell", "en": "eng", "eo": "epo", "es": "spa", "et": "est", "eu": "eus",
	"fa": "fas", "ff": "ful", "fi": "fin", "fj": "fij", "fo": "fao", "fr": "fra", "fy": "fry",
	"ga": "gle", "gd": "gla", "gl": "glg", "gn": "grn", "gu": "guj", "gv": "glv",
	"ha": "hau", "he": "heb", "hi": "hin", "ho": "hmo", "hr": "hrv", "ht": "hat", "hu": "hun", "hy": "hye",
	"hz": "her",
	"ia": "ina", "id": "ind", "ie": "ile", "ig": "ibo", "ii": "iii", "ik": "ipk", "io": "ido", "is": "isl",
	"it": "ita", "iu": "iku",
	"ja": "jpn", "jv": "jav",
	"ka": "kat", "kg": "kon", "ki": "kik", "kj": "kua", "kk": "kaz", "kl": "kal", "km": "khm", "kn": "kan",
	"ko": "kor", "kr": "kau", "ks": "kas", "ku": "kur", "kv": "kom", "kw": "cor", "ky": "kir",
	"la": "lat", "lb": "ltz", "lg": "lug", "li": "lim", "ln": "lin", "lo": "lao", "lt": "lit", "lu": "lub",
	"lv": "lav",
	"mg": "mlg", "mh": "mah", "mi": "mri", "mk": "mkd", "ml": "mal", "mn": "mon", "mr": "mar", "ms": "msa",
	"mt": "mlt", "my": "mya",
	"na": "nau", "nb": "nob", "nd": "nde", "ne": "nep", "ng": "ndo", "nl": "nld", "nn": "nno", "no": "nor",
	"nr": "nbl", "nv": "nav", "ny": "nya",
	"oc": "oci", "oj": "oji", "om": "orm", "or": "ori", "os": "oss",
	"pa": "pan", "pi": "pli", "pl": "pol", "ps": "pus", "pt": "por",
	"qu": "que",
	"rm": "roh", "rn": "run", "ro": "ron", "ru": "rus", "rw": "kin",
	"sa": "san", "sc": "srd", "sd": "snd", "se": "sme", "sg": "sag", "si": "sin", "sk": "slk", "sl": "slv",
	"sm": "smo", "sn": "sna", "so": "som", "sq": "sqi", "sr": "srp", "ss": "ssw", "st": "sot", "su": "sun",
	"sv": "swe", "sw": "swa",
	"ta": "tam", "te": "tel", "tg": "tgk", "th": "tha", "ti": "tir", "tk": "tuk", "tl": "tgl", "tn": "tsn",
	"to": "ton", "tr": "tur", "ts": "tso", "tt": "tat", "tw": "twi", "ty": "tah",
	"ug": "uig", "uk": "ukr", "ur": "urd", "uz": "uzb",
	"ve": "ven", "vi": "vie", "vo": "vol",
	"wa": "wln", "wo": "wol",
	"xh": "xho",
	"yi": "yid", "yo": "yor",
	"za": "zha", "zh": "zho", "zu": "zul",
	// withdrawn, and still in file names
	"iw": "heb", "in": "ind", "ji": "yid", "jw": "jav", "mo": "ron",
}
