package scanner

import "testing"

// TestExtractTitle covers the bare-year strip parity with Java's global
// replaceAll("\\s+(?:19|20)\\d{2}(?=\\s|$)","") — including the mid-string case
// the end-anchored version missed.
func TestExtractTitle(t *testing.T) {
	cases := []struct{ in, typ, want string }{
		{"Some 2020 Thing Here.mp4", "movie", "Some Thing Here"}, // mid-string year stripped (global) — the fix
		{"The Matrix 1999.mkv", "movie", "The Matrix"},           // trailing year stripped
		{"Blade Runner 2049.mkv", "movie", "Blade Runner"},       // faithful to Java even when the year is part of the title
		{"1917 (2019).mkv", "movie", "1917"},                     // paren-year removed, numeric title kept
		{"Tears of Steel.mov", "movie", "Tears of Steel"},        // no year, case preserved
	}
	for _, c := range cases {
		if got := extractTitle(c.in, c.typ); got != c.want {
			t.Errorf("extractTitle(%q,%q) = %q, want %q", c.in, c.typ, got, c.want)
		}
	}
}

// TestClassify covers episode detection: SxxEyy anywhere, and a whole-segment
// series/tv/shows folder (including a top-level one with no leading slash — the
// case the old "/series/" substring check missed).
func TestClassify(t *testing.T) {
	cases := []struct{ rel, want string }{
		{"series/Pioneer One/Pioneer.One.S01E01.mp4", "episode"}, // SxxEyy + folder
		{"tv/Some Show/some.show.s02e10.mkv", "episode"},         // lower-case token + tv folder
		{"series/Docs Only/documentary.mp4", "episode"},          // folder alone (no SxxEyy)
		{"Pioneer.One.S01E03.720p.mp4", "episode"},               // flat but SxxEyy present
		{"Big Buck Bunny (2008).mp4", "movie"},                   // plain movie
		{"Caminandes Llama Drama (2013).mp4", "movie"},           // no episode signal
	}
	for _, c := range cases {
		if got := classify(c.rel, true, false); got != c.want {
			t.Errorf("classify(%q) = %q, want %q", c.rel, got, c.want)
		}
	}
	if got := classify("music/song.flac", false, true); got != "track" {
		t.Errorf("classify(audio) = %q, want track", got)
	}
}

// TestSeriesTitleFor covers show-name derivation: folder-first, then the
// filename with the SxxEyy token (and everything after) stripped.
func TestSeriesTitleFor(t *testing.T) {
	cases := []struct{ rel, filename, want string }{
		{"series/Pioneer One/Pioneer.One.S01E01.mp4", "Pioneer.One.S01E01.mp4", "Pioneer One"},            // folder wins
		{"tv/The Show (2019)/the.show.s01e02.mkv", "the.show.s01e02.mkv", "The Show"},                     // folder, paren-year stripped
		{"Pioneer.One.S01E04.720p.x264-VODO.mp4", "Pioneer.One.S01E04.720p.x264-VODO.mp4", "Pioneer One"}, // filename fallback, tags after SxxEyy dropped
	}
	for _, c := range cases {
		if got := seriesTitleFor(c.rel, c.filename); got != c.want {
			t.Errorf("seriesTitleFor(%q,%q) = %q, want %q", c.rel, c.filename, got, c.want)
		}
	}
}

// A name numbers the episodes one file covers: S05E15 alone, or a range from
// the first to the last, written S05E15-E16, S05E15E16, S05E15-16, S05E15.E16
// or with more (S05E15-E17, S05E15E16E17), letter case aside. A number after
// a dash that a digit or a p follows is no episode (-720p, -1080p), and
// neither is one that does not come after the one before it or would cover
// more than ten episodes; a token that runs into a word is none, as before.
func TestEpisodeRanges(t *testing.T) {
	cases := []struct {
		name               string
		season, from, upTo int // from 0: no token
	}{
		{"Show.S05E15.mkv", 5, 15, 15},
		{"Show.S05E15-E16.mkv", 5, 15, 16},
		{"Show.S05E15E16.mkv", 5, 15, 16},
		{"Show.S05E15-16.mkv", 5, 15, 16},
		{"Show.S05E15-E17.mkv", 5, 15, 17},
		{"show.s05e15e16.mkv", 5, 15, 16},
		{"Show.s05E15-e16.mkv", 5, 15, 16},
		{"Show.S05E15.E16.mkv", 5, 15, 16},
		{"Show S05E15 E16 The Finale.mkv", 5, 15, 16},
		{"Show.S05E15_E16.mkv", 5, 15, 16},
		{"Show_S05E15.mkv", 0, 0, 0}, // an underscore before it is a word's, as it was
		{"Show.S05E15E16E17.mkv", 5, 15, 17},
		{"Show.S05E15-E16-E17.mkv", 5, 15, 17},
		{"Show.S05E15-16.720p.WEB.mkv", 5, 15, 16},
		{"Show.S01E01-E02.1080p.x264-GROUP.mkv", 1, 1, 2},
		{"Show.S05E15-720p.mkv", 5, 15, 15},
		{"Show.S05E15-1080p.mkv", 5, 15, 15},
		{"Show.S05E15-2160P.mkv", 5, 15, 15},
		{"Show.S05E15-576i.mkv", 5, 15, 15},
		{"Show.S05E15-264.mkv", 5, 15, 15},
		{"Show.S05E15-E14.mkv", 5, 15, 15},
		{"Show.S05E15-E26.mkv", 5, 15, 15},
		{"Show.S05E15-E24.mkv", 5, 15, 24},
		{"Show.S05E15.720p.mkv", 5, 15, 15},
		{"Show.S05E15-E16x.mkv", 5, 15, 15},
		{"Show.S05E15x264.mkv", 0, 0, 0},
		{"Show.S05E1500.mkv", 0, 0, 0},
		{"Big Buck Bunny (2008).mp4", 0, 0, 0},
	}
	for _, c := range cases {
		tok, ok := firstEpisodeToken(stripExt(c.name))
		got := [3]int{tok.season, tok.first, tok.last}
		if !ok {
			got = [3]int{}
		}
		if want := [3]int{c.season, c.from, c.upTo}; got != want {
			t.Errorf("%q numbers season, first, last %v, want %v", c.name, got, want)
		}
	}
	if s, e := episodeCoords("Show.S05E15E16.mkv"); s == nil || e == nil || *s != 5 || *e != 15 {
		t.Errorf("episodeCoords of a file of two episodes: %v %v, want its first, 5 and 15", s, e)
	}
	if got := extractTitle("Show S05E15-E16 The Finale.mkv", "episode"); got != "Show The Finale" {
		t.Errorf("the title of a file of two episodes is %q, want the name without its numbers", got)
	}
	if got := seriesTitleFor("Show.S05E15E16.Finale.mkv", "Show.S05E15E16.Finale.mkv"); got != "Show" {
		t.Errorf("the show of a file of two episodes is %q, want Show", got)
	}
	if got := classify("Show.S05E15E16.mkv", true, false); got != "episode" {
		t.Errorf("a file of two episodes is a %s", got)
	}
}
