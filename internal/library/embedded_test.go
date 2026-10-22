package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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

// A stream of pictures embedded in an original (PGS, VobSub, DVB, XSUB) is
// none of the title's subtitles: its form the record's, else its codec's;
// the record's form wins over the codec's, and a stream the record gives
// neither is none either. A record of pictures alone lists none, and says
// no error: the title has no subtitle of its own there.
func TestAStreamOfPicturesEmbeddedInAnOriginalIsNoSubtitleOfTheTitle(t *testing.T) {
	const source = "0b6c0000-0000-4000-8000-000000000003"
	stream := func(index int, codec, form string) map[string]any {
		s := map[string]any{"index": index, "type": "subtitle", "language": "de", "languageRaw": "ger", "purpose": "dialogue"}
		if codec != "" {
			s["codec"] = codec
		}
		if form != "" {
			s["form"] = form
		}
		return s
	}
	write := func(streams ...map[string]any) string {
		item := t.TempDir()
		dir := SourceDir(item, source)
		b, err := json.Marshal(map[string]any{"schema": "zaentrum.library.source/2", "sourceId": source, "streams": streams})
		if err == nil {
			err = os.MkdirAll(dir, 0o755)
		}
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, "source.json"), b, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	item := write(stream(2, "subrip", "text"), stream(3, "hdmv_pgs_subtitle", "image"), stream(4, "dvd_subtitle", ""),
		stream(5, "dvb_subtitle", "image"), stream(6, "xsub", ""), stream(7, "subrip", "image"), stream(8, "ass", ""),
		stream(9, "", ""))
	subs, err := EmbeddedSubtitles(item, source)
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, s := range subs {
		got = append(got, *s.Stream)
		if s.Form != "text" {
			t.Errorf("stream %d listed as %q", *s.Stream, s.Form)
		}
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 8 {
		t.Errorf("listed the streams %v, want [2 8]: the text ones", got)
	}
	subs, err = EmbeddedSubtitles(write(stream(3, "hdmv_pgs_subtitle", "image"), stream(4, "dvd_subtitle", "")), source)
	if err != nil || len(subs) != 0 {
		t.Errorf("an original of pictures alone: %v %v, want none and no error", subs, err)
	}
}
