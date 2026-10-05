package scanner

import (
	"fmt"
	"testing"
)

// A kind is read from the first words of a name, the longest phrase first,
// in any case and with any separators; the label is what follows, as
// written.
func TestParseKind(t *testing.T) {
	for in, want := range map[string]string{
		"trailer":                   "trailer ",
		"Trailer-2":                 "trailer 2",
		"TEASER":                    "teaser ",
		"behind-the-scenes":         "behind-the-scenes ",
		"Behind The Scenes - Music": "behind-the-scenes Music",
		"behindthescenes":           "behind-the-scenes ",
		"making of":                 "making-of ",
		"Making-Of_the_film":        "making-of the_film",
		"deleted scene 01":          "deleted-scene 01",
		"deleted.scenes":            "deleted-scene ",
		"deleted":                   "deleted-scene ",
		"gag reel":                  "gag-reel ",
		"bloopers":                  "gag-reel ",
		"featurette - The Score":    "featurette The Score",
		"interview":                 "interview ",
		"short":                     "short ",
		"extra":                     "other ",
		"other":                     "other ",
		"Official Trailer":          "-",
		"trailers":                  "-",
		"Making":                    "-",
		"":                          "-",
	} {
		got := "-"
		if kind, label, ok := parseKind(in); ok {
			got = kind + " " + label
		}
		if got != want {
			t.Errorf("parseKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// A file names the title whose file's stem it begins with, a separator, a
// kind: the longest such stem wins, in any case; a stem that runs on into a
// word, or a name that says no kind after it, names none.
func TestNameOfAnExtra(t *testing.T) {
	mains := []string{"Sintel", "Movie", "Movie 2", "Été"}
	for in, want := range map[string]string{
		"Sintel-trailer":                     "Sintel trailer ",
		"Sintel - Behind the Scenes - Music": "Sintel behind-the-scenes Music",
		"sintel.TEASER.2":                    "Sintel teaser 2",
		"Sintel_deleted_scene_01":            "Sintel deleted-scene 01",
		"Movie 2-trailer":                    "Movie 2 trailer ",
		"Movie-trailer":                      "Movie trailer ",
		"été - making of":                    "Été making-of ",
		"Sintel2-trailer":                    "-",
		"Sintel - Official Trailer":          "-",
		"Sintel":                             "-",
		"Sintel-":                            "-",
		"Other-trailer":                      "-",
	} {
		got := "-"
		if main, kind, label, ok := nameOfAnExtra(in, mains); ok {
			got = main + " " + kind + " " + label
		}
		if got != want {
			t.Errorf("nameOfAnExtra(%q) = %q, want %q", in, got, want)
		}
	}
}

// The folders of extras, by their names in any case and spelling, each with
// its kind; a folder of any other name holds none (shorts/ and other/ may be
// a library's folders of titles).
func TestFolderKind(t *testing.T) {
	for in, want := range map[string]string{
		"trailers": "trailer", "Trailers": "trailer", "Teasers": "teaser", "featurettes": "featurette",
		"Behind The Scenes": "behind-the-scenes", "behind-the-scenes": "behind-the-scenes", "BehindTheScenes": "behind-the-scenes",
		"Making Of": "making-of", "Deleted Scenes": "deleted-scene", "deleted_scenes": "deleted-scene", "Interviews": "interview",
		"Bloopers": "gag-reel", "gag reels": "gag-reel", "Extras": "other", "extras": "other",
		"Shorts": "-", "Other": "-", "Trailer": "-", "Season 01": "-", "Sintel (2010)": "-",
	} {
		got := "-"
		if k, ok := folderKind(in); ok {
			got = k
		}
		if got != want {
			t.Errorf("folderKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// A season's folder names its season; Specials is season 0.
func TestSeasonOf(t *testing.T) {
	for in, want := range map[string]string{
		"Season 01": "1", "season.2": "2", "SEASON_10": "10", "S03": "3", "s1": "1", "Specials": "0", "Special": "0",
		"Season": "-", "Staffel 1": "-", "Season One": "-", "extras": "-",
	} {
		got := "-"
		if n, ok := seasonOf(in); ok {
			got = fmt.Sprint(n)
		}
		if got != want {
			t.Errorf("seasonOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// An extra is called by its label when the label says something, by its
// kind and the label when the label is a number, by its kind without one.
func TestExtraTitle(t *testing.T) {
	for _, c := range [][3]string{
		{"trailer", "", "Trailer"}, {"trailer", "2", "Trailer 2"}, {"featurette", "Music", "Music"},
		{"behind-the-scenes", " ", "Behind the Scenes"}, {"deleted-scene", "01", "Deleted Scene 01"},
	} {
		if got := extraTitle(c[0], c[1]); got != c[2] {
			t.Errorf("extraTitle(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	if got := nameTitle("Pioneer One - Trailer"); got != "Pioneer One Trailer" {
		t.Errorf("nameTitle: %q", got)
	}
}

// A file in a folder of extras is told by its name first: as an extra of a
// title's file beside the folder, as a kind, by the kind its last words
// name; else it is of the folder's kind, titled by its words.
func TestFolderExtra(t *testing.T) {
	mains := []string{"Sintel (2010)"}
	for in, want := range map[string]string{
		"Sintel (2010)-trailer":        "trailer Trailer",
		"Sintel (2010) - Featurette 2": "featurette Featurette 2",
		"Sintel (2010) - The Score":    "trailer The Score",
		"sintel (2010).2":              "trailer Trailer 2",
		"teaser":                       "teaser Teaser",
		"Pioneer One - Trailer":        "trailer Trailer",
		"Official":                     "trailer Official",
		"The Making of Sintel":         "trailer The Making of Sintel",
		"...":                          "trailer Trailer",
	} {
		if k, title := folderExtra(in, "trailer", mains); k+" "+title != want {
			t.Errorf("folderExtra(%q) = %s %s, want %s", in, k, title, want)
		}
	}
}
