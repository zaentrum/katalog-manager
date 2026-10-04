// Package ratings says whom a title's age certification is for: the minimum
// age, in years, that each certification TMDB gives a title means, country by
// country, and which of a title's certifications the catalog keeps.
//
// A kid's account is capped at an age, the max_rating claim of its access
// token. katalog-api leaves out of what it serves such a viewer every title
// rated above the cap, and every unrated one unless ratings.unrated_for_capped
// says show; chino-api answers 404 for one asked for by id. This package only
// says what the ages are.
package ratings

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// The settings the catalog's ratings follow (com_nalet_katalog_settings).
const (
	// CountriesSetting lists the countries whose certifications rate a title,
	// in the order they are asked (Countries).
	CountriesSetting = "ratings.countries"
	// DefaultCountries is CountriesSetting when it names none.
	DefaultCountries = "CH,DE,US"
	// UnratedSetting says whether a viewer with a cap is served the titles
	// nothing rates: show, or hide (the default, and anything else).
	UnratedSetting = "ratings.unrated_for_capped"
)

// MaxAge is the highest minimum age a certification means (Singapore's R21).
const MaxAge = 21

// minAges is the table: per country (ISO 3166-1 alpha-2, as TMDB names it),
// the minimum age each certification TMDB gives there means, for its films
// (release_dates) and its series (content_ratings) alike: where a country's
// two lists share a word, it means the same in both. A certification is
// matched without regard to case or spaces ("MA 15+" is "MA15+").
//
// The table is conservative, so a capped viewer is never served more than a
// rating board allows a child alone:
//
//   - A certification that names an age means that age.
//   - One that admits younger children only with an adult ("12A", "15A",
//     "14A", "14+" in Italy, "PG12") means the age it names: a child of a
//     capped account watches alone.
//   - Parental guidance without an age means the age the board names for it
//     where it names one (Ireland's PG: guidance under 12), else 10, as the
//     US PG ("may not be suitable for children under 10"), and the BBFC's PG
//     8 ("should not unsettle a child aged around eight or older").
//   - Mature without an age means the board's next restricted age (MA 15+ in
//     Australia, 16 in New Zealand). A restricted, refused or banned title (R
//     in New Zealand, RC, KK, adult-only) means 18.
//   - Not rated (NR) and exemptions (E, Exempt, F in Denmark) are no rating:
//     they are not in the table, and the title's next country is asked.
//
// TMDB's lists of certifications are its /certification/movie/list and
// /certification/tv/list. A country it lists none for (Austria) is not here.
var minAges = map[string]map[string]int{
	// Australia: ACB (films), the ACMA's TV classification (series).
	"AU": {"P": 0, "C": 0, "G": 0, "PG": 10, "M": 15, "MA 15+": 15, "AV 15+": 15,
		"R 18+": 18, "X 18+": 18, "RC": 18},
	// Brazil: ClassInd, Livre (L) and the age.
	"BR": {"L": 0, "10": 10, "12": 12, "14": 14, "16": 16, "18": 18},
	// Canada: the provincial film boards (films), the CRTC's TV ratings
	// (series). R and A are restricted to adults.
	"CA": {"C": 0, "C8": 8, "G": 0, "PG": 10, "14A": 14, "14+": 14, "18A": 18, "18+": 18, "R": 18, "A": 18},
	// Switzerland: the age a film is admitted from.
	"CH": {"0": 0, "6": 6, "8": 8, "10": 10, "12": 12, "14": 14, "16": 16, "18": 18},
	// Germany: FSK (films) and the FSF (series).
	"DE": {"0": 0, "6": 6, "12": 12, "16": 16, "18": 18},
	// Denmark: Medierådet, A for all.
	"DK": {"A": 0, "7": 7, "11": 11, "15": 15},
	// Spain: ICAA (films), the TV code (series); Ai, ERI and TP are for all,
	// X for adults.
	"ES": {"A": 0, "Ai": 0, "ERI": 0, "TP": 0, "7": 7, "7i": 7, "10": 10, "12": 12, "13": 13,
		"16": 16, "18": 18, "X": 18},
	// Finland: KAVI, S for all; KK is banned.
	"FI": {"S": 0, "K-7": 7, "K7": 7, "K-12": 12, "K12": 12, "K-16": 16, "K16": 16,
		"K-18": 18, "K18": 18, "KK": 18},
	// France: CNC (films, TP for all), Arcom's signs (series).
	"FR": {"TP": 0, "10": 10, "12": 12, "16": 16, "18": 18},
	// United Kingdom: BBFC; R18 is for licensed premises, adults only.
	"GB": {"U": 0, "PG": 8, "12A": 12, "12": 12, "15": 15, "18": 18, "R18": 18},
	// Ireland: IFCO; PG recommends guidance under 12.
	"IE": {"G": 0, "PG": 12, "12A": 12, "12": 12, "15A": 15, "15": 15, "16": 16, "18": 18},
	// Italy: the ministry's ages (films), the TV signs (series); T for all,
	// BA parental guidance.
	"IT": {"T": 0, "BA": 10, "6+": 6, "VM12": 12, "14+": 14, "VM14": 14, "18+": 18, "VM18": 18},
	// Japan: Eirin, PG12 guidance under 12.
	"JP": {"G": 0, "PG12": 12, "R15+": 15, "R18+": 18},
	// South Korea: KMRB (films), the KCSC (series).
	"KR": {"All": 0, "7": 7, "12": 12, "15": 15, "18": 18, "19": 19, "Restricted Screening": 18},
	// Luxembourg: EA (enfants admis) for all.
	"LU": {"EA": 0, "6": 6, "12": 12, "16": 16, "18": 18},
	// Mexico: RTC, AA and A for all, B for 12 and up, C and D for adults.
	"MX": {"AA": 0, "A": 0, "B": 12, "B-15": 15, "C": 18, "D": 18},
	// Netherlands: Kijkwijzer, AL (alle leeftijden) for all.
	"NL": {"AL": 0, "6": 6, "9": 9, "12": 12, "14": 14, "16": 16, "18": 18},
	// Norway: Medietilsynet, A for all.
	"NO": {"A": 0, "6": 6, "9": 9, "12": 12, "15": 15, "18": 18},
	// New Zealand: the Classification Office; M is for 16 and up, R for a
	// class of persons only.
	"NZ": {"G": 0, "PG": 10, "M": 16, "R13": 13, "RP13": 13, "R15": 15, "16": 16, "RP16": 16, "R16": 16,
		"18": 18, "R18": 18, "RP18": 18, "R": 18},
	// Portugal: CCE (films, P is pornography), the TV signs (series, AP the
	// age it is advised from).
	"PT": {"Públicos": 0, "T": 0, "M/3": 3, "M/6": 6, "10AP": 10, "M/12": 12, "12AP": 12, "M/14": 14,
		"M/16": 16, "16": 16, "M/18": 18, "18": 18, "P": 18},
	// Russia: the age a title is for.
	"RU": {"0+": 0, "6+": 6, "12+": 12, "16+": 16, "18+": 18},
	// Sweden: Statens medieråd, Btl (barntillåten) for all.
	"SE": {"Btl": 0, "7": 7, "Från 7 år": 7, "11": 11, "Från 11 år": 11, "15": 15, "Från 15 år": 15},
	// Singapore: IMDA; R21 is for 21 and up.
	"SG": {"G": 0, "PG": 10, "PG13": 13, "NC16": 16, "M18": 18, "R21": 21},
	// United States: the MPA's ratings (films) and the TV Parental
	// Guidelines (series). R admits under 17 only with an adult.
	"US": {"G": 0, "PG": 10, "PG-13": 13, "R": 17, "NC-17": 18,
		"TV-Y": 0, "TV-Y7": 7, "TV-G": 0, "TV-PG": 10, "TV-14": 14, "TV-MA": 17},
}

// table is minAges keyed as a certification is looked up: by country, then by
// the certification's key (key).
var table = func() map[string]map[string]int {
	out := make(map[string]map[string]int, len(minAges))
	for country, certs := range minAges {
		if !countryRE.MatchString(country) {
			panic(fmt.Sprintf("ratings: %q is no ISO 3166-1 alpha-2 code", country))
		}
		out[country] = make(map[string]int, len(certs))
		for cert, age := range certs {
			k := key(cert)
			if _, dup := out[country][k]; dup {
				panic(fmt.Sprintf("ratings: %s lists %q twice", country, cert))
			}
			if age < 0 || age > MaxAge {
				panic(fmt.Sprintf("ratings: %s %q is %d, not an age of 0 to %d", country, cert, age, MaxAge))
			}
			out[country][k] = age
		}
	}
	return out
}()

// key is how a certification is matched: upper case, without spaces.
func key(cert string) string {
	return strings.ToUpper(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, cert))
}

// MinAge is the minimum age certification means in country; ok is false for
// one the table does not rate there: not rated (NR), an exemption, a word the
// country does not use, or a country the table has no row for.
func MinAge(country, certification string) (age int, ok bool) {
	age, ok = table[strings.ToUpper(strings.TrimSpace(country))][key(certification)]
	return age, ok
}

// Rating is the certification the catalog keeps of a title: as TMDB gives it
// in a country, the country, and the minimum age it means.
type Rating struct {
	Certification string // as TMDB gives it: "12", "PG-13", "TV-MA"
	Country       string // ISO 3166-1 alpha-2: "DE"
	MinAge        int
}

// Choose picks a title's rating from its certifications, by country (as tmdb
// reads them from TMDB): the first of countries that has one the table rates
// wins, and of that country's certifications (a film's several releases) the
// strictest, the one with the highest age, the first of them on a tie. ok is
// false when none of countries rates the title.
func Choose(countries []string, certs map[string][]string) (Rating, bool) {
	byCountry := make(map[string][]string, len(certs))
	for c, list := range certs {
		c = strings.ToUpper(strings.TrimSpace(c))
		byCountry[c] = append(byCountry[c], list...)
	}
	for _, country := range countries {
		var best Rating
		found := false
		for _, cert := range byCountry[country] {
			age, ok := MinAge(country, cert)
			if !ok || (found && age <= best.MinAge) {
				continue
			}
			best, found = Rating{Certification: strings.TrimSpace(cert), Country: country, MinAge: age}, true
		}
		if found {
			return best, true
		}
	}
	return Rating{}, false
}

var countryRE = regexp.MustCompile(`^[A-Z]{2}$`)

// Countries reads the setting ratings.countries: ISO 3166-1 alpha-2 codes
// separated by commas or spaces, in the order they are asked ("CH, de us"
// is CH, DE, US). A word that is no such code is left out, and so is a
// country named twice after its first place; a setting that names none is
// DefaultCountries.
func Countries(setting string) []string {
	if out := countries(setting); len(out) > 0 {
		return out
	}
	return countries(DefaultCountries)
}

func countries(setting string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(setting, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		c := strings.ToUpper(w)
		if !countryRE.MatchString(c) || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// ShowUnrated reads the setting ratings.unrated_for_capped: true for show,
// whatever its case and spaces; false (hide, the default) for hide, for an
// absent setting and for a value the setting does not know.
func ShowUnrated(setting string) bool {
	return strings.EqualFold(strings.TrimSpace(setting), "show")
}
