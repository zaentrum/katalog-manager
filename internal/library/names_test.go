package library

import "testing"

// An original's name in its version folder is the record logic's: original,
// -<part> for a version split in parts, and the extension it arrived with,
// lower-cased, when that is 1 to 8 letters a-z and digits, else bin. The
// extension is Python's splitext's: a name that is only dots before its last
// dot has none. These are the examples the schemas' record logic shares.
func TestAnOriginalsNameInItsVersionFolder(t *testing.T) {
	for _, c := range []struct {
		name string
		part int
		want string
	}{
		{"Some Film (2024) Bluray-1080p.MKV", 0, "original.mkv"},
		{"Some Film (2024)", 0, "original.bin"},
		{"00001.m2ts", 0, "original.m2ts"},
		{"BDMV/STREAM/00002.M2TS", 0, "original.m2ts"},
		{".m2ts", 0, "original.bin"},
		{"..mkv", 0, "original.bin"},
		{"...a.mkv", 0, "original.mkv"},
		{"Film.", 0, "original.bin"},
		{"Film.part1.mp4", 2, "original-2.mp4"},
		{"Film.ts", 1, "original-1.ts"},
		{"Film.12345678", 0, "original.12345678"},
		{"Film.123456789", 0, "original.bin"},
		{"Film.mkv~", 0, "original.bin"},
		{"Film.m k v", 0, "original.bin"},
		{"Film.ÄVI", 0, "original.bin"},
		{"Film.\u0130SO", 0, "original.bin"}, // İ lower-cases to i and a combining dot, as Python's lower does
		{"Film.\u212Av", 0, "original.kv"},   // the Kelvin sign lower-cases to k
		{"Film.tar.GZ", 0, "original.gz"},
	} {
		got := OriginalName(c.name, c.part)
		if got != c.want {
			t.Errorf("OriginalName(%q, %d) = %q, want %q", c.name, c.part, got, c.want)
		}
		if !IsOriginalName(got) {
			t.Errorf("%q is not an original's name", got)
		}
	}
	for _, name := range []string{"original", "original.MKV", "Original.mkv", "original-0.mkv", "original-01.mkv",
		"original.mkv.part", "a.mkv", "original..mkv", "original.123456789"} {
		if IsOriginalName(name) {
			t.Errorf("%q is taken for an original's name", name)
		}
	}
}
