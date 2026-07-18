package tmdb

import "testing"

// TestBestFanart covers the ranking: English wins over other languages, likes
// break ties, empty URLs are skipped, and an empty set yields "".
func TestBestFanart(t *testing.T) {
	cases := []struct {
		name string
		in   []fanartImage
		want string
	}{
		{"empty", nil, ""},
		{"skip blank url", []fanartImage{{URL: "", Lang: "en", Likes: "99"}}, ""},
		{
			"english beats other language even with fewer likes",
			[]fanartImage{{URL: "de", Lang: "de", Likes: "50"}, {URL: "en", Lang: "en", Likes: "1"}},
			"en",
		},
		{
			"among english, most likes wins",
			[]fanartImage{{URL: "low", Lang: "en", Likes: "2"}, {URL: "high", Lang: "en", Likes: "30"}},
			"high",
		},
		{
			"textless (00) preferred over a foreign language",
			[]fanartImage{{URL: "fr", Lang: "fr", Likes: "5"}, {URL: "textless", Lang: "00", Likes: "0"}},
			"textless",
		},
		{
			"likes capped so a foreign pile can't beat english",
			[]fanartImage{{URL: "foreign", Lang: "ru", Likes: "9999"}, {URL: "en", Lang: "en", Likes: "0"}},
			"en",
		},
	}
	for _, c := range cases {
		if got := bestFanart(c.in); got != c.want {
			t.Errorf("%s: bestFanart = %q, want %q", c.name, got, c.want)
		}
	}
}
