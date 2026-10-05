package scanner

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// The names of extras, as the scanner's convention reads them (extras.go).
//
// A file is an extra of the title beside it when its name is the title's
// file's, then a kind, then a label maybe:
//
//	<main stem><sep><kind>[<sep><label>].<ext>
//
// in the same folder as the title's file; the separators are -, ., _ and
// space, runs of them too ("Sintel - Trailer"); the kinds are the words of
// kindPhrases. The longest stem a file names wins.

// extraSeps are the separators of the words in a file's name.
const extraSeps = "-._ "

func isExtraSep(r rune) bool { return strings.ContainsRune(extraSeps, r) }

// kindPhrases are the words that name a kind, each phrase in lower case.
// "behind-the-scenes" and "making-of" are the words behind, the, scenes and
// making, of: a hyphen separates words.
var kindPhrases = []struct {
	words []string
	kind  string
}{
	{[]string{"behind", "the", "scenes"}, "behind-the-scenes"},
	{[]string{"behindthescenes"}, "behind-the-scenes"},
	{[]string{"making", "of"}, "making-of"},
	{[]string{"makingof"}, "making-of"},
	{[]string{"deleted", "scenes"}, "deleted-scene"},
	{[]string{"deleted", "scene"}, "deleted-scene"},
	{[]string{"deletedscenes"}, "deleted-scene"},
	{[]string{"deletedscene"}, "deleted-scene"},
	{[]string{"deleted"}, "deleted-scene"},
	{[]string{"gag", "reel"}, "gag-reel"},
	{[]string{"gagreel"}, "gag-reel"},
	{[]string{"bloopers"}, "gag-reel"},
	{[]string{"trailer"}, "trailer"},
	{[]string{"teaser"}, "teaser"},
	{[]string{"featurette"}, "featurette"},
	{[]string{"interview"}, "interview"},
	{[]string{"short"}, "short"},
	{[]string{"other"}, "other"},
	{[]string{"extra"}, "other"},
}

func init() {
	// The longest phrase first, so that "deleted scene" wins over "deleted".
	sort.SliceStable(kindPhrases, func(i, j int) bool { return len(kindPhrases[i].words) > len(kindPhrases[j].words) })
}

// word is a word of a name and where it is in it.
type word struct {
	text       string
	start, end int
}

// splitWords splits s at its runs of separators.
func splitWords(s string) []word {
	var out []word
	start := -1
	for i, r := range s {
		if isExtraSep(r) {
			if start >= 0 {
				out = append(out, word{s[start:i], start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, word{s[start:], start, len(s)})
	}
	return out
}

// parseKind reads "<kind>[<sep><label>]" from s: the kind its first words
// name, and the label after them as written, its separators around it
// trimmed. ok is false when s does not begin with a kind.
func parseKind(s string) (kind, label string, ok bool) {
	ws := splitWords(s)
	for _, p := range kindPhrases {
		if len(p.words) > len(ws) {
			continue
		}
		match := true
		for i, w := range p.words {
			if strings.ToLower(ws[i].text) != w {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if n := len(p.words); n < len(ws) {
			label = strings.Trim(s[ws[n].start:], extraSeps)
		}
		return p.kind, label, true
	}
	return "", "", false
}

// bareKinds are the kinds a file may be named by alone ("trailer.mkv",
// "Making Of.mkv") to be an extra: not interview, short, other and extra,
// which may be the names of titles of their own.
var bareKinds = map[string]bool{"trailer": true, "teaser": true, "featurette": true, "behind-the-scenes": true,
	"making-of": true, "deleted-scene": true, "gag-reel": true}

// kindAtEnd is the kind the last words of s name, after something else
// ("Pioneer One - Trailer"); ok is false when they name none.
func kindAtEnd(s string) (string, bool) {
	ws := splitWords(s)
	for _, p := range kindPhrases {
		n := len(p.words)
		if n >= len(ws) {
			continue
		}
		match := true
		for i, w := range p.words {
			if strings.ToLower(ws[len(ws)-n+i].text) != w {
				match = false
				break
			}
		}
		if match {
			return p.kind, true
		}
	}
	return "", false
}

// folderExtra is the kind and the title of the file stem in a folder of
// extras of kind, beside the titles' files mains: the kind and the label of
// the name of an extra of one of them ("Sintel-trailer"), else the folder's
// kind and what follows the name of one of them ("Sintel - The Score"),
// else the kind and the label of a name that is a kind ("teaser"), else the
// kind its last words name ("Pioneer One - Trailer"), else the folder's kind
// and its words as the title.
func folderExtra(stem, kind string, mains []string) (string, string) {
	if _, k, label, ok := nameOfAnExtra(stem, mains); ok {
		return k, extraTitle(k, label)
	}
	if rest, ok := afterMain(stem, mains); ok {
		return kind, extraTitle(kind, rest)
	}
	if k, label, ok := parseKind(stem); ok {
		return k, extraTitle(k, label)
	}
	if k, ok := kindAtEnd(stem); ok {
		return k, model.ExtraKindTitle(k)
	}
	if t := nameTitle(stem); t != "" {
		return kind, t
	}
	return kind, model.ExtraKindTitle(kind)
}

// afterMain is what follows the longest of mains stem begins with, and a
// separator after it, its separators trimmed; ok is false when it begins
// with none, or nothing follows.
func afterMain(stem string, mains []string) (string, bool) {
	sorted := append([]string(nil), mains...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, m := range sorted {
		if m == "" {
			continue
		}
		rest, ok := foldPrefix(stem, m)
		if !ok || rest == "" {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(rest); !isExtraSep(r) {
			continue
		}
		if rest = strings.Trim(rest, extraSeps); rest != "" {
			return rest, true
		}
	}
	return "", false
}

// foldPrefix reports whether s begins with prefix, letter case aside, and
// answers what follows it in s.
func foldPrefix(s, prefix string) (string, bool) {
	i := 0
	for _, pr := range prefix {
		if i >= len(s) {
			return "", false
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != pr && unicode.ToLower(r) != unicode.ToLower(pr) {
			return "", false
		}
		i += n
	}
	return s[i:], true
}

// nameOfAnExtra reads the stem of a file as the name of an extra of one of
// mains, the stems of the files of titles beside it: the longest of them it
// begins with, then a separator, then a kind and a label maybe. ok is false
// when it names none of them so.
func nameOfAnExtra(stem string, mains []string) (main, kind, label string, ok bool) {
	sorted := append([]string(nil), mains...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, m := range sorted {
		if m == "" || m == stem {
			continue
		}
		rest, ok := foldPrefix(stem, m)
		if !ok || rest == "" {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(rest); !isExtraSep(r) {
			continue
		}
		if k, l, ok := parseKind(strings.TrimLeft(rest, extraSeps)); ok {
			return m, k, l, true
		}
	}
	return "", "", "", false
}

// extraFolders are the folders whose files are extras of the title the
// folder is beside, by their names as normalName writes them, with the kind
// each holds.
var extraFolders = map[string]string{
	"trailers": "trailer", "teasers": "teaser", "featurettes": "featurette",
	"behind the scenes": "behind-the-scenes", "behindthescenes": "behind-the-scenes",
	"making of": "making-of", "makingof": "making-of",
	"deleted scenes": "deleted-scene", "deletedscenes": "deleted-scene",
	"interviews": "interview", "gag reels": "gag-reel", "gagreels": "gag-reel", "bloopers": "gag-reel",
	"extras": "other",
}

// normalName is a name in lower case, its words joined by a space.
func normalName(s string) string {
	var out []string
	for _, w := range splitWords(s) {
		out = append(out, strings.ToLower(w.text))
	}
	return strings.Join(out, " ")
}

// folderKind is the kind of the extras a folder named name holds; ok is
// false for a folder of no extras.
func folderKind(name string) (string, bool) {
	k, ok := extraFolders[normalName(name)]
	return k, ok
}

var (
	seasonFolder   = regexp.MustCompile(`(?i)^(?:season|s)[ ._-]*(\d{1,3})$`)
	specialsFolder = regexp.MustCompile(`(?i)^specials?$`)
)

// seasonOf reads a season's folder: "Season 01", "season.1", "S01" and
// "Specials" (season 0).
func seasonOf(name string) (int32, bool) {
	name = strings.TrimSpace(name)
	if specialsFolder.MatchString(name) {
		return 0, true
	}
	m := seasonFolder.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// extraTitle is what an extra of kind with label is called: the label when
// it says something ("Music"), the kind's name and the label when it is a
// number ("Trailer 2"), the kind's name without one ("Trailer").
func extraTitle(kind, label string) string {
	label = strings.TrimSpace(label)
	switch {
	case label == "":
		return model.ExtraKindTitle(kind)
	case strings.IndexFunc(label, unicode.IsLetter) < 0:
		return model.ExtraKindTitle(kind) + " " + label
	}
	return clipTitle(label)
}

// clipTitle is a title cut to the 255 characters a title holds.
func clipTitle(s string) string {
	if utf8.RuneCountInString(s) <= 255 {
		return s
	}
	return string([]rune(s)[:255])
}

// nameTitle is the title a file's stem gives: its words, separated by
// spaces.
func nameTitle(stem string) string {
	var out []string
	for _, w := range splitWords(stem) {
		out = append(out, w.text)
	}
	if len(out) == 0 {
		return ""
	}
	return clipTitle(strings.Join(out, " "))
}
