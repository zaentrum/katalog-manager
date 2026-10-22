package library

import "testing"

// A stream embedded in an original is in its language as a language tag
// spells it, else in the code its file carried, else in none: Chinese with
// the script its variant names, any language with the region its variant
// names cleanly, where the tag has none of its own.
func TestTheLanguageOfAStreamEmbeddedInAnOriginal(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, c := range []struct {
		language, raw, variant *string
		want                   string
	}{
		{s("en"), s("eng"), nil, "en"},
		{s("zh"), s("chi"), s("Simplified"), "zh-Hans"},
		{s("zh"), s("chi"), s("traditional"), "zh-Hant"},
		{s("zh-tw"), nil, s("Traditional"), "zh-Hant-TW"},
		{s("zh-hant"), nil, s("Simplified"), "zh-Hant"},
		{s("zh"), s("chi"), s("Cantonese"), "zh"},
		{s("en"), s("eng"), s("Simplified"), "en"},
		{s("es"), s("spa"), s("Latin American"), "es-419"},
		{s("es"), s("spa"), s("Latin-American"), "es-419"},
		{s("es"), s("spa"), s("LatAm"), "es-419"},
		{s("es-mx"), nil, s("Latin American"), "es-MX"},
		{s("es"), s("spa"), s("Castilian"), "es"},
		{s("pt"), s("por"), s("Brazilian"), "pt-BR"},
		{s("fr"), s("fre"), s("Canadian"), "fr-CA"},
		{nil, s("ger"), nil, "de"},
		{s("und"), s("zh_hans"), nil, "zh-Hans"},
		{s("und"), s("english"), nil, ""},
		{s("zxx"), nil, nil, ""},
		{nil, nil, s("Simplified"), ""},
	} {
		if got := embeddedLanguage(c.language, c.raw, c.variant); got != c.want {
			t.Errorf("%v %v %v: %q, want %q", deref(c.language), deref(c.raw), deref(c.variant), got, c.want)
		}
	}
}

// The id of a stream embedded in an original names its source and its index
// in the file, and is read back; no other id is one.
func TestTheIDOfAStreamEmbeddedInAnOriginal(t *testing.T) {
	const source = "0b6c0000-0000-4000-8000-000000000003"
	id := EmbeddedID(source, 12)
	if id != "source-"+source+"-12" {
		t.Errorf("the id: %s", id)
	}
	if s, n, ok := ParseEmbeddedID(id); !ok || s != source || n != 12 {
		t.Errorf("read back: %s %d %v", s, n, ok)
	}
	if s, n, ok := ParseEmbeddedID(EmbeddedID(source, 0)); !ok || s != source || n != 0 {
		t.Errorf("stream 0: %s %d %v", s, n, ok)
	}
	for _, id := range []string{"", "source-", "source-12", "source--12", "source-" + source, "source-" + source + "-",
		"source-" + source + "-012", "source-" + source + "-+1", "source-" + source + "-x", "sub-" + source + "-1",
		"6a1c4e2e-0000-4000-8000-000000000001"} {
		if s, n, ok := ParseEmbeddedID(id); ok {
			t.Errorf("%q: %s %d, want no stream's", id, s, n)
		}
	}
}
