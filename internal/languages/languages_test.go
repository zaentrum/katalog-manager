package languages

import "testing"

// A track's language is set as three lowercase letters: zxx and und too, an
// ISO 639-1 code, a capital, a region or a word are not.
func TestIsCode(t *testing.T) {
	for _, ok := range []string{"eng", "ger", "deu", "zxx", "und", "jpn"} {
		if !IsCode(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "en", "ENG", "Eng", "english", "en-US", "e1g", " eng", "eng ", "ëng"} {
		if IsCode(bad) {
			t.Errorf("%q taken", bad)
		}
	}
}

// A track plays as an admin's language, else its source's, else und.
func TestEffective(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		override, source *string
		want             string
	}{
		{s("zxx"), s("und"), "zxx"},
		{s("eng"), nil, "eng"},
		{nil, s("ger"), "ger"},
		{nil, s(" "), "und"},
		{nil, nil, "und"},
		{s(""), s("fra"), "fra"},
	} {
		if got := Effective(c.override, c.source); got != c.want {
			t.Errorf("Effective(%v, %v) = %q, want %q", deref(c.override), deref(c.source), got, c.want)
		}
	}
}

// A code a file name gives becomes an ISO 639-2 code: three letters as they
// are, two letters (ISO 639-1, a withdrawn one too) as their terminology
// code; a region does not count; anything else is und.
func TestISO6392(t *testing.T) {
	for in, want := range map[string]string{
		"eng": "eng", "ger": "ger", "DEU": "deu", "en": "eng", "de": "deu", "DE": "deu", "fr": "fra",
		"nl": "nld", "zh": "zho", "pt-br": "por", "pt_BR": "por", "en-US": "eng", "iw": "heb", "zxx": "zxx",
		"": "und", "x": "und", "qq": "und", "english": "und", "e1": "und", " es ": "spa",
	} {
		if got := ISO6392(in); got != want {
			t.Errorf("ISO6392(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every ISO 639-1 code has its ISO 639-2 code, three lowercase letters.
func TestEveryISO6391CodeIsMapped(t *testing.T) {
	if len(fromISO6391) < 183 {
		t.Errorf("%d codes mapped; ISO 639-1 has 183", len(fromISO6391))
	}
	for two, three := range fromISO6391 {
		if len(two) != 2 || !IsCode(three) {
			t.Errorf("%q maps to %q", two, three)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
