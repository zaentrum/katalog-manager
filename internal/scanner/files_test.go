package scanner

import (
	"path/filepath"
	"testing"
)

// A file is a title's to a scan when it is a video by its extension, not
// hidden, no disc image, and no extra by the convention as it reads with
// extras.scan on: a file named as a title's extra beside it, one in a folder
// of extras, one named as a kind alone, one the scanner always took for a
// trailer. A file named as another file beside it, as an upgrade of an
// episode is, is a title's.
func TestNoTitleFile(t *testing.T) {
	root := media(t, "BigBuckBunny_320x180.mp4", "Big Buck Bunny (2008).mov", "Sintel/Sintel.mkv",
		"Sintel/Sintel - Behind the Scenes.mkv", "Sintel/Sintel-trailer.mkv", "Sintel/extras/Score.mkv",
		"trailers/Sintel.mkv", "Making Of.mkv", "Short.mkv", "series/Pioneer One/Pioneer.One.S01E01.mp4",
		"series/Pioneer One/Pioneer.One.S01E01.mkv", ".Sintel.mkv", "notes.txt", "clip.ts", "song.flac", "Disc (2001).ISO")
	for _, c := range []struct{ rel, why string }{
		{"BigBuckBunny_320x180.mp4", ""},
		{"Big Buck Bunny (2008).mov", ""},
		{"Sintel/Sintel.mkv", ""},
		{"Short.mkv", ""},
		{"series/Pioneer One/Pioneer.One.S01E01.mkv", ""},
		{"Sintel/Sintel - Behind the Scenes.mkv", "it is an extra by the extras convention (behind-the-scenes)"},
		{"Sintel/Sintel-trailer.mkv", "it is an extra by the extras convention (trailer)"},
		{"Sintel/extras/Score.mkv", "it is an extra by the extras convention (other)"},
		{"trailers/Sintel.mkv", "it is an extra by the extras convention (trailer)"},
		{"Making Of.mkv", "it is an extra by the extras convention (making-of)"},
		{".Sintel.mkv", "its name begins with a dot, and a scan passes over hidden files"},
		{"notes.txt", "it is no video file a scan takes (.avi, .m4v, .mkv, .mov, .mp4, .webm)"},
		{"clip.ts", "it is no video file a scan takes (.avi, .m4v, .mkv, .mov, .mp4, .webm)"},
		{"song.flac", "it is no video file a scan takes (.avi, .m4v, .mkv, .mov, .mp4, .webm)"},
		{"Disc (2001).ISO", "it is a disc image: convert it to a single file"},
	} {
		if got := NoTitleFile(filepath.Join(root, c.rel)); got != c.why {
			t.Errorf("%s: %q, want %q", c.rel, got, c.why)
		}
	}
}

// A subtitle file is a video's when a scan pairs it with the video: in its
// folder, named as the video, a language between maybe, letter case aside.
func TestSidecarOf(t *testing.T) {
	for _, c := range []struct {
		video, sub string
		is         bool
	}{
		{"/m/Film.mkv", "/m/Film.en.srt", true},
		{"/m/Film.mkv", "/m/Film.srt", true},
		{"/m/Film.mkv", "/m/film.PT-br.ASS", true},
		{"/m/Film.mkv", "/m/./Film.de.vtt", true},
		{"/m/Pioneer.One.S01E01.mp4", "/m/Pioneer.One.S01E01.en.srt", true},
		{"/m/Pioneer.One.S01E01.mkv", "/m/Pioneer.One.S01E01.en.srt", true},
		{"/m/Big Buck Bunny (2008).mov", "/m/BigBuckBunny_320x180.en.srt", false},
		{"/m/Film.mkv", "/m/Film.en.nfo", false},
		{"/m/Film.mkv", "/m/Film.director.srt", false},
		{"/m/Film.mkv", "/m/Other.en.srt", false},
		{"/m/Film.mkv", "/m/subs/Film.en.srt", false},
		{"/m/a/Film.mkv", "/m/Film.en.srt", false},
	} {
		if got := SidecarOf(c.video, c.sub); got != c.is {
			t.Errorf("%s beside %s: %v, want %v", c.sub, c.video, got, c.is)
		}
	}
}
