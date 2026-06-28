package scanner

import "testing"

// TestExtractTitle covers the bare-year strip parity with Java's global
// replaceAll("\\s+(?:19|20)\\d{2}(?=\\s|$)","") — including the mid-string case
// the end-anchored version missed.
func TestExtractTitle(t *testing.T) {
	cases := []struct{ in, typ, want string }{
		{"Some 2020 Thing Here.mp4", "movie", "Some Thing Here"}, // mid-string year stripped (global) — the fix
		{"The Matrix 1999.mkv", "movie", "The Matrix"},          // trailing year stripped
		{"Blade Runner 2049.mkv", "movie", "Blade Runner"},      // faithful to Java even when the year is part of the title
		{"1917 (2019).mkv", "movie", "1917"},                    // paren-year removed, numeric title kept
		{"Tears of Steel.mov", "movie", "Tears of Steel"},      // no year, case preserved
	}
	for _, c := range cases {
		if got := extractTitle(c.in, c.typ); got != c.want {
			t.Errorf("extractTitle(%q,%q) = %q, want %q", c.in, c.typ, got, c.want)
		}
	}
}
