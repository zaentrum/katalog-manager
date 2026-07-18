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
