package languages

import "testing"

// A language tag is spelled as people's software reads one: the ISO 639-1
// code of a language that has one, whatever ISO 639 form it came in, its
// script in title case, its region in capitals; what names no language a
// subtitle can be in, or is no tag, is refused.
func TestTag(t *testing.T) {
	for in, want := range map[string]string{
		"de": "de", "deu": "de", "ger": "de", " DE ": "de", "en": "en", "eng": "en",
		"zh": "zh", "chi": "zh", "zho": "zh", "zh-hans": "zh-Hans", "zh_CN": "zh-CN", "zh-Hant-TW": "zh-Hant-TW",
		"pt-br": "pt-BR", "es-419": "es-419", "iw": "he", "fil": "fil", "sr-latn": "sr-Latn",
	} {
		if got, ok := Tag(in); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "und", "zxx", "mul", "mis", "x", "german", "de-", "de-DEU-x", "zh-Hans-Hant",
		"zh-Hant-TW-x", "en-GB-oxendict"} {
		if got, ok := Tag(in); ok {
			t.Errorf("%q: %q, want it refused", in, got)
		}
	}
}
