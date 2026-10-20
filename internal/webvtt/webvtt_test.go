package webvtt

import (
	"errors"
	"strings"
	"testing"
)

// SubRip becomes WebVTT: its header first, each block's number gone, its
// times WebVTT's, the styling of other formats gone, "-->" in its text
// "->", a block that runs into the next without its blank line split from
// it, and a block without text left out. A byte order mark and CR LF go.
func TestSubRipBecomesWebVTT(t *testing.T) {
	srt := "\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:02,500\r\n{\\an8}<font color=\"#ffff00\">Hello</font>\r\n<i>there</i>\r\n\r\n" +
		"2\r\n00:00:03,000 --> 00:00:04,000\r\nA --> B\r\n3\r\n00:01:05,250 --> 00:01:06,000\r\nNo blank line\r\n\r\n" +
		"4\r\n00:01:07,000 --> 00:01:08,000\r\n\r\n" +
		"5\r\n1:00:00,5 --> 1:00:01,25\r\n1984\r\n\r\n" +
		"6\r\n01:00:02,000 --> 01:00:03,000\r\n{\\c&b0f0f0&} {\\fs16}两行\r\n{\\c&ffffff&} {\\fs14}Two lines \r\n"
	vtt, cues, err := Normalize([]byte(srt), "")
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\nHello\n<i>there</i>\n\n" +
		"00:00:03.000 --> 00:00:04.000\nA -> B\n\n00:01:05.250 --> 00:01:06.000\nNo blank line\n\n" +
		"01:00:00.500 --> 01:00:01.250\n1984\n\n01:00:02.000 --> 01:00:03.000\n两行\nTwo lines\n"
	if string(vtt) != want || cues != 5 {
		t.Errorf("%d cues:\n%s\nwant 5:\n%s", cues, vtt, want)
	}
}

// WebVTT stays WebVTT, its blocks as they are, but each timing line as
// WebVTT spells times, its cue settings kept, and one line end at its end.
func TestWebVTTKeepsItsBlocks(t *testing.T) {
	in := "WEBVTT - a title\n\nNOTE made by hand\n\nSTYLE\n::cue { color: yellow }\n\n" +
		"one\n00:01.000 --> 00:02,5 align:start line:10%\nHello\n\n00:00:03.000 --> 00:00:04.000\nThere\n\n\n"
	vtt, cues, err := Normalize([]byte(in), FormatWebVTT)
	if err != nil {
		t.Fatal(err)
	}
	want := "WEBVTT - a title\n\nNOTE made by hand\n\nSTYLE\n::cue { color: yellow }\n\n" +
		"one\n00:00:01.000 --> 00:00:02.500 align:start line:10%\nHello\n\n00:00:03.000 --> 00:00:04.000\nThere\n"
	if string(vtt) != want || cues != 2 {
		t.Errorf("%d cues:\n%q\nwant 2:\n%q", cues, vtt, want)
	}
	if got, _, err := Normalize([]byte(in), ""); err != nil || string(got) != want {
		t.Errorf("the text says it is WebVTT: %q %v", got, err)
	}
}

// What is no subtitle is refused, saying why.
func TestNormalizeRefusesWhatIsNoSubtitle(t *testing.T) {
	for name, c := range map[string]struct {
		text, format, says string
	}{
		"not UTF-8":             {"1\n00:00:01,000 --> 00:00:02,000\n\xe4\xfc\xf6 \xff\n", "", "not UTF-8"},
		"read as UTF-8 badly":   {"1\n00:00:01,000 --> 00:00:02,000\nGr\ufffd\ufffde\n", "", "could not be read"},
		"WebVTT without header": {"00:00:01.000 --> 00:00:02.000\nHello\n", FormatWebVTT, "WEBVTT header"},
		"no cue":                {"WEBVTT\n\nNOTE nothing\n", "", "no cue"},
		"no cue in SubRip":      {"just some text\n", FormatSRT, "no cue"},
		"backwards":             {"1\n00:00:05,000 --> 00:00:02,000\nHello\n", "", "ends before it starts"},
		"a broken timing":       {"WEBVTT\n\n00:00:01.000 --> soon\nHello\n", "", "no cue timing"},
		"seconds past 59":       {"1\n00:00:61,000 --> 00:01:02,000\nHello\n", "", "no time"},
	} {
		_, _, err := Normalize([]byte(c.text), c.format)
		var bad *Invalid
		if !errors.As(err, &bad) || !strings.Contains(bad.Reason, c.says) {
			t.Errorf("%s: %v, want it refused saying %q", name, err, c.says)
		}
	}
}

func TestFormatOf(t *testing.T) {
	for name, want := range map[string]string{"": "", "webvtt": FormatWebVTT, "VTT": FormatWebVTT, "srt": FormatSRT, "SubRip": FormatSRT} {
		if got, ok := FormatOf(name); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", name, got, ok, want)
		}
	}
	if _, ok := FormatOf("ass"); ok {
		t.Error("ass is taken")
	}
}
