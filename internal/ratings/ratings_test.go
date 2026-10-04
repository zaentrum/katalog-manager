package ratings

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// rows is the mapping table as the tests hold it: each certification of each
// country, as TMDB spells it, and the age it means. Every row is a case of
// TestEveryRowMeansItsAge, and TestTheTableIsTheRows fails while the table and
// these rows differ, so no row is in the table without a test.
var rows = []struct {
	country, cert string
	age           int
}{
	{"AU", "P", 0}, {"AU", "C", 0}, {"AU", "G", 0}, {"AU", "PG", 10}, {"AU", "M", 15},
	{"AU", "MA 15+", 15}, {"AU", "AV 15+", 15}, {"AU", "R 18+", 18}, {"AU", "X 18+", 18}, {"AU", "RC", 18},

	{"BR", "L", 0}, {"BR", "10", 10}, {"BR", "12", 12}, {"BR", "14", 14}, {"BR", "16", 16}, {"BR", "18", 18},

	{"CA", "C", 0}, {"CA", "C8", 8}, {"CA", "G", 0}, {"CA", "PG", 10}, {"CA", "14A", 14}, {"CA", "14+", 14},
	{"CA", "18A", 18}, {"CA", "18+", 18}, {"CA", "R", 18}, {"CA", "A", 18},

	{"CH", "0", 0}, {"CH", "6", 6}, {"CH", "8", 8}, {"CH", "10", 10}, {"CH", "12", 12}, {"CH", "14", 14},
	{"CH", "16", 16}, {"CH", "18", 18},

	{"DE", "0", 0}, {"DE", "6", 6}, {"DE", "12", 12}, {"DE", "16", 16}, {"DE", "18", 18},

	{"DK", "A", 0}, {"DK", "7", 7}, {"DK", "11", 11}, {"DK", "15", 15},

	{"ES", "A", 0}, {"ES", "Ai", 0}, {"ES", "ERI", 0}, {"ES", "TP", 0}, {"ES", "7", 7}, {"ES", "7i", 7},
	{"ES", "10", 10}, {"ES", "12", 12}, {"ES", "13", 13}, {"ES", "16", 16}, {"ES", "18", 18}, {"ES", "X", 18},

	{"FI", "S", 0}, {"FI", "K-7", 7}, {"FI", "K7", 7}, {"FI", "K-12", 12}, {"FI", "K12", 12},
	{"FI", "K-16", 16}, {"FI", "K16", 16}, {"FI", "K-18", 18}, {"FI", "K18", 18}, {"FI", "KK", 18},

	{"FR", "TP", 0}, {"FR", "10", 10}, {"FR", "12", 12}, {"FR", "16", 16}, {"FR", "18", 18},

	{"GB", "U", 0}, {"GB", "PG", 8}, {"GB", "12A", 12}, {"GB", "12", 12}, {"GB", "15", 15}, {"GB", "18", 18},
	{"GB", "R18", 18},

	{"IE", "G", 0}, {"IE", "PG", 12}, {"IE", "12A", 12}, {"IE", "12", 12}, {"IE", "15A", 15}, {"IE", "15", 15},
	{"IE", "16", 16}, {"IE", "18", 18},

	{"IT", "T", 0}, {"IT", "BA", 10}, {"IT", "6+", 6}, {"IT", "VM12", 12}, {"IT", "14+", 14}, {"IT", "VM14", 14},
	{"IT", "18+", 18}, {"IT", "VM18", 18},

	{"JP", "G", 0}, {"JP", "PG12", 12}, {"JP", "R15+", 15}, {"JP", "R18+", 18},

	{"KR", "All", 0}, {"KR", "7", 7}, {"KR", "12", 12}, {"KR", "15", 15}, {"KR", "18", 18}, {"KR", "19", 19},
	{"KR", "Restricted Screening", 18},

	{"LU", "EA", 0}, {"LU", "6", 6}, {"LU", "12", 12}, {"LU", "16", 16}, {"LU", "18", 18},

	{"MX", "AA", 0}, {"MX", "A", 0}, {"MX", "B", 12}, {"MX", "B-15", 15}, {"MX", "C", 18}, {"MX", "D", 18},

	{"NL", "AL", 0}, {"NL", "6", 6}, {"NL", "9", 9}, {"NL", "12", 12}, {"NL", "14", 14}, {"NL", "16", 16},
	{"NL", "18", 18},

	{"NO", "A", 0}, {"NO", "6", 6}, {"NO", "9", 9}, {"NO", "12", 12}, {"NO", "15", 15}, {"NO", "18", 18},

	{"NZ", "G", 0}, {"NZ", "PG", 10}, {"NZ", "M", 16}, {"NZ", "R13", 13}, {"NZ", "RP13", 13}, {"NZ", "R15", 15},
	{"NZ", "16", 16}, {"NZ", "RP16", 16}, {"NZ", "R16", 16}, {"NZ", "18", 18}, {"NZ", "R18", 18},
	{"NZ", "RP18", 18}, {"NZ", "R", 18},

	{"PT", "Públicos", 0}, {"PT", "T", 0}, {"PT", "M/3", 3}, {"PT", "M/6", 6}, {"PT", "10AP", 10},
	{"PT", "M/12", 12}, {"PT", "12AP", 12}, {"PT", "M/14", 14}, {"PT", "M/16", 16}, {"PT", "16", 16},
	{"PT", "M/18", 18}, {"PT", "18", 18}, {"PT", "P", 18},

	{"RU", "0+", 0}, {"RU", "6+", 6}, {"RU", "12+", 12}, {"RU", "16+", 16}, {"RU", "18+", 18},

	{"SE", "Btl", 0}, {"SE", "7", 7}, {"SE", "Från 7 år", 7}, {"SE", "11", 11}, {"SE", "Från 11 år", 11},
	{"SE", "15", 15}, {"SE", "Från 15 år", 15},

	{"SG", "G", 0}, {"SG", "PG", 10}, {"SG", "PG13", 13}, {"SG", "NC16", 16}, {"SG", "M18", 18}, {"SG", "R21", 21},

	{"US", "G", 0}, {"US", "PG", 10}, {"US", "PG-13", 13}, {"US", "R", 17}, {"US", "NC-17", 18},
	{"US", "TV-Y", 0}, {"US", "TV-Y7", 7}, {"US", "TV-G", 0}, {"US", "TV-PG", 10}, {"US", "TV-14", 14},
	{"US", "TV-MA", 17},
}

// Each certification of the table means its age, as TMDB spells it and in any
// case and spacing.
func TestEveryRowMeansItsAge(t *testing.T) {
	for _, r := range rows {
		t.Run(r.country+" "+r.cert, func(t *testing.T) {
			if age, ok := MinAge(r.country, r.cert); !ok || age != r.age {
				t.Errorf("MinAge(%s, %q) = %d %v, want %d", r.country, r.cert, age, ok, r.age)
			}
			spaced := strings.Join(strings.Split(strings.ToLower(r.cert), ""), " ") // "ma 15+" as "m a   1 5 +"
			if age, ok := MinAge(" "+strings.ToLower(r.country)+" ", spaced); !ok || age != r.age {
				t.Errorf("MinAge(%s, %q) = %d %v, want %d", r.country, spaced, age, ok, r.age)
			}
		})
	}
}

// The table holds the rows of the tests and nothing else.
func TestTheTableIsTheRows(t *testing.T) {
	var want, got []string
	for _, r := range rows {
		want = append(want, fmt.Sprintf("%s %s %d", r.country, r.cert, r.age))
	}
	for country, certs := range minAges {
		for cert, age := range certs {
			got = append(got, fmt.Sprintf("%s %s %d", country, cert, age))
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the table and the tests' rows differ\n table %q\n rows  %q", got, want)
	}
}

// What the table does not rate has no age: not rated, an exemption, a word a
// country does not use, a country it has no row for (TMDB lists no
// certification of Austria), and nothing at all.
func TestWhatTheTableDoesNotRate(t *testing.T) {
	for _, c := range []struct{ country, cert string }{
		{"US", "NR"}, {"US", "Unrated"}, {"IT", "NR"}, {"FR", "NR"}, {"ES", "NR"}, {"DK", "NR"}, {"NO", "NR"},
		{"AU", "E"}, {"CA", "E"}, {"CA", "Exempt"}, {"DK", "F"}, {"KR", "Exempt"},
		{"DE", "FSK 12"}, {"DE", "PG-13"}, {"US", "12"}, {"GB", "12B"}, {"CH", "7"},
		{"AT", "12"}, {"XX", "PG"}, {"", "PG"}, {"DE", ""}, {"DE", " "},
	} {
		if age, ok := MinAge(c.country, c.cert); ok {
			t.Errorf("MinAge(%q, %q) = %d, want no age", c.country, c.cert, age)
		}
	}
}

// The first country of the list that rates a title wins; a country's
// strictest certification is its rating, the first of them on a tie; and a
// certification nobody rates is passed over for the next country.
func TestChoose(t *testing.T) {
	chd := Countries(DefaultCountries)
	cases := []struct {
		name  string
		certs map[string][]string
		list  []string
		want  string // "certification country age", "" for none
	}{
		{"the first country wins", map[string][]string{"US": {"R"}, "DE": {"16"}, "CH": {"12"}}, chd, "12 CH 12"},
		{"then the next", map[string][]string{"US": {"PG-13"}, "DE": {"12"}}, chd, "12 DE 12"},
		{"only the last", map[string][]string{"US": {"TV-MA"}, "FR": {"16"}}, chd, "TV-MA US 17"},
		{"a country not listed is not asked", map[string][]string{"FR": {"16"}, "GB": {"15"}}, chd, ""},
		{"not rated is passed over", map[string][]string{"CH": {"NR"}, "DE": {""}, "US": {"PG"}}, chd, "PG US 10"},
		{"the strictest of a country", map[string][]string{"DE": {"12", "16", "6"}}, chd, "16 DE 16"},
		{"a tie keeps the first", map[string][]string{"GB": {"12A", "12"}}, []string{"GB"}, "12A GB 12"},
		{"spaces and case", map[string][]string{" de ": {" 16 "}}, chd, "16 DE 16"},
		{"another order", map[string][]string{"US": {"R"}, "DE": {"16"}}, []string{"US", "DE"}, "R US 17"},
		{"none at all", nil, chd, ""},
	}
	for _, c := range cases {
		r, ok := Choose(c.list, c.certs)
		got := ""
		if ok {
			got = fmt.Sprintf("%s %s %d", r.Certification, r.Country, r.MinAge)
		}
		if got != c.want {
			t.Errorf("%s: Choose(%v, %v) = %q, want %q", c.name, c.list, c.certs, got, c.want)
		}
	}
}

// ratings.countries names countries by code, in its order; what is no code
// is left out, a repeat too, and a setting that names none is the default.
func TestCountries(t *testing.T) {
	for setting, want := range map[string][]string{
		"":                   {"CH", "DE", "US"},
		"CH,DE,US":           {"CH", "DE", "US"},
		" gb , us":           {"GB", "US"},
		"de at\tch":          {"DE", "AT", "CH"},
		"DE,DEU,Germany,us":  {"DE", "US"},
		"US,us,DE,US":        {"US", "DE"},
		"none, 12, D-E":      {"CH", "DE", "US"},
		",,,":                {"CH", "DE", "US"},
		"FR":                 {"FR"},
		"ch;de":              {"CH", "DE", "US"},
		"CH,DE,US,FR,GB,IT,": {"CH", "DE", "US", "FR", "GB", "IT"},
	} {
		if got := Countries(setting); !reflect.DeepEqual(got, want) {
			t.Errorf("Countries(%q) = %v, want %v", setting, got, want)
		}
	}
}

// ratings.unrated_for_capped shows unrated titles to capped viewers only when
// it says show.
func TestShowUnrated(t *testing.T) {
	for setting, want := range map[string]bool{
		"show": true, " Show ": true, "SHOW": true,
		"hide": false, "": false, "yes": false, "true": false, "shown": false, "show all": false,
	} {
		if got := ShowUnrated(setting); got != want {
			t.Errorf("ShowUnrated(%q) = %v, want %v", setting, got, want)
		}
	}
}
