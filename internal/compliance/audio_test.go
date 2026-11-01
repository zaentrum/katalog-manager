package compliance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// A title that says its track is no commentary makes it none, as the record
// logic reads it (record logic 9; schemas' tools/test-library-v2-tools.py,
// its cases, a film's name in them made up): its commentary word is taken
// back by a negation right before it, or past articles and possessives
// alone, three at most; a negation of anything else — a film's name, a word
// in between — leaves the commentary one, as one after it does.
func TestATitleThatSaysItsTrackIsNoCommentary(t *testing.T) {
	nones := []string{"No commentary", "Without commentary", "w/o commentary", "W/o commentary", "Ohne Kommentar",
		"Kein Kommentar", "Keine Kommentare", "Kommentarlos", "Kommentarfrei", "Sans commentaire",
		"Sans commentaires", "Sin comentarios", "Senza commento", "Zonder commentaar", "Geen commentaar",
		"Non commentary", "Non-commentary", "English (No Commentary)", "English - no director's commentary",
		"Without the director's audio commentary", "Commentary-free", "Deutsch (ohne Kommentar)"}
	ones := []string{"Commentary", "Kommentar", "Commentaire du réalisateur", "Comentario del director",
		"No-spoiler commentary", "No spoilers commentary", "No 2 commentary", "No Zebra commentary",
		"Sin Pelota commentary", "Sans Bateau commentary", "Without Lanterns - Commentary",
		"Commentary - not for kids", "Non-English commentary", "Director's commentary (no spoilers)",
		"Commentary with no music", "Kommentar ohne Spoiler", "Commentaire sans spoilers",
		"Comentario sin spoilers", "Commentary — no subtitles", "No. 2: Commentary",
		"No a the an audio commentary"}
	for _, x := range nones {
		if got := (audioTrack{title: x}).purpose(); got != purposeMain {
			t.Errorf("%q: %s, want the main audio", x, got)
		}
	}
	for _, x := range ones {
		if got := (audioTrack{title: x}).purpose(); got != purposeCommentary {
			t.Errorf("%q: %s, want a commentary", x, got)
		}
	}
	for _, x := range []string{"", "No comments", "Commentaries"} {
		if saysCommentary(x) {
			t.Errorf("%q says it is a commentary", x)
		}
	}
}

// What a track is for, as the record logic reads it (audio_purpose): a
// commentary by its disposition, an audio description by its own, then by
// its title — a commentary's words as TestATitleThatSaysItsTrackIsNoCommentary
// reads them, an audio description's where it says no commentary — else the
// film's main audio, which alone gets a 5.1 companion of a packager that
// makes one of the tracks shown. Python's \b and \s are Unicode's. Of a
// packager that doesn't report which tracks get one, the check takes a
// track for a commentary as it took one before (oldCommentary): every title
// that holds "comment" or "kommentar", a negation's too, and no audio
// description.
func TestWhatATrackIsFor(t *testing.T) {
	for _, tc := range []struct {
		title                    string
		comment, impaired, descr bool
		purpose                  string
		oldTaken                 bool
	}{
		{title: "English", purpose: purposeMain},
		{title: "", purpose: purposeMain},
		{title: "Director's Commentary", purpose: purposeCommentary, oldTaken: true},
		{title: "Kommentar des Regisseurs", purpose: purposeCommentary, oldTaken: true},
		{title: "Director comments", purpose: purposeMain, oldTaken: true},
		{title: "English", comment: true, purpose: purposeCommentary, oldTaken: true},
		{title: "No commentary", purpose: purposeMain, oldTaken: true},
		{title: "without the director’s commentary", purpose: purposeMain, oldTaken: true},
		{title: "kommentar-frei", purpose: purposeMain, oldTaken: true},
		{title: "«No commentary»", purpose: purposeMain, oldTaken: true},
		// a no-break space is a space, as Python's \s takes one
		{title: "No commentary", purpose: purposeMain, oldTaken: true},
		// a negation begins a word: a letter before it, of any script, is
		// none, as Python's \b reads it
		{title: "Pianino commentary", purpose: purposeCommentary, oldTaken: true},
		{title: "Éno commentary", purpose: purposeCommentary, oldTaken: true},
		{title: "Comentario del director", purpose: purposeCommentary},
		{title: "Audio Description", purpose: purposeDescription},
		{title: "Deutsch Audiodeskription", purpose: purposeDescription},
		{title: "Descriptive audio", purpose: purposeDescription},
		{title: "English", impaired: true, purpose: purposeDescription},
		{title: "English", descr: true, purpose: purposeDescription},
		{title: "Commentary", impaired: true, purpose: purposeDescription, oldTaken: true},
		{title: "Audio description (no commentary)", purpose: purposeDescription, oldTaken: true},
		{title: "Commentary with audio description", purpose: purposeCommentary, oldTaken: true},
		{title: "Audio Description", comment: true, purpose: purposeCommentary, oldTaken: true},
	} {
		a := audioTrack{title: tc.title, comment: tc.comment, impaired: tc.impaired, description: tc.descr}
		if a.purpose() != tc.purpose || a.forTheFilm() != (tc.purpose == purposeMain) || a.oldCommentary() != tc.oldTaken {
			t.Errorf("%q (comment %v, visual_impaired %v, descriptions %v): %s, for the film %v, taken for a commentary "+
				"before %v; want %s, %v", tc.title, tc.comment, tc.impaired, tc.descr, a.purpose(), a.forTheFilm(),
				a.oldCommentary(), tc.purpose, tc.oldTaken)
		}
	}
}

// A language's key is the packager's: the primary subtag in lower case, a
// three-letter code by the packager's own table, anything else as it is.
func TestThePackagersLanguageKeys(t *testing.T) {
	for tag, want := range map[string]string{
		"ger": "de", "deu": "de", "de": "de", "DE": "de", "de-CH": "de", "en_US": "en", " eng ": "en", "chi": "zh",
		"zh-Hans": "zh", "gsw": "gsw", "arm": "arm", "hye": "hye", "iw": "iw", "": "und", " ": "und", "-x": "und",
		"und": "und", "zxx": "zxx",
	} {
		if got := languageKey(tag); got != want {
			t.Errorf("%q: %q, want %q", tag, got, want)
		}
	}
}

// tracksOf are tracks of the keys given, each "key", "key:51" (six
// channels), with ":ad" an audio description, ":c" a commentary, ":d"
// flagged default.
func tracksOf(specs ...string) []audioTrack {
	out := make([]audioTrack, 0, len(specs))
	for _, s := range specs {
		parts := strings.Split(s, ":")
		a := audioTrack{key: parts[0], channels: 2}
		for _, p := range parts[1:] {
			switch p {
			case "51":
				a.channels = 6
			case "ad":
				a.title = "Audio Description"
			case "c":
				a.comment = true
			case "d":
				a.flagged = true
			}
		}
		out = append(out, a)
	}
	return out
}

func shownOf(shown []bool) string {
	var out []string
	for i, s := range shown {
		if s {
			out = append(out, fmt.Sprint(i))
		}
	}
	return strings.Join(out, " ")
}

// The tracks shown are the packager's: every one without a whitelist; one of
// no language or of one the whitelist names, by its key; and of a title of a
// single language the whitelist hides, every one, while
// keep_original_if_single holds.
func TestTheTracksShown(t *testing.T) {
	for _, tc := range []struct {
		tracks    []audioTrack
		whitelist []string
		keep      bool
		want      string
	}{
		{tracksOf("en", "ja", "de"), nil, true, "0 1 2"},
		{tracksOf("en", "ja", "de"), []string{"en", "de"}, true, "0 2"},
		{tracksOf("en", "ja", "de"), []string{"ger"}, true, "2"},
		{tracksOf("ja", "und"), []string{"en"}, true, "1"},
		{tracksOf("ja", "zxx"), []string{"en"}, false, "1"},
		{tracksOf("ja", "ja"), []string{"en"}, true, "0 1"},
		{tracksOf("ja", "ja"), []string{"en"}, false, ""},
		{tracksOf("ja", "ko"), []string{"en"}, true, ""},
	} {
		if got := shownOf(shownTracks(tc.tracks, languageSettings{whitelist: tc.whitelist, keepOriginal: tc.keep})); got != tc.want {
			t.Errorf("%v of %v, keep %v: shown %q, want %q", tc.whitelist, tc.tracks, tc.keep, got, tc.want)
		}
	}
}

// The default track is the packager's: of the first language of the
// whitelist a track is of, else of all of them, the first shown, for the
// film, flagged default, in that order.
func TestTheDefaultTrack(t *testing.T) {
	for _, tc := range []struct {
		tracks    []audioTrack
		whitelist []string
		keep      bool
		want      int
	}{
		{tracksOf("en", "de"), []string{"de", "en"}, true, 1},
		{tracksOf("de:c:d", "de"), []string{"de"}, true, 1},
		{tracksOf("en", "de:d"), nil, true, 1},
		{tracksOf("en:c", "en:ad", "en"), nil, true, 2},
		{tracksOf("en:c", "en:ad"), nil, true, 0},
		{tracksOf("ja", "en"), []string{"de"}, true, 0}, // none shown: the first
		{tracksOf("ja:c", "ja", "ko:d"), []string{"en"}, true, 2},
		{tracksOf("ja:c", "ja", "ko"), []string{"en"}, true, 1},
		{nil, []string{"en"}, true, -1},
	} {
		l := languageSettings{whitelist: tc.whitelist, keepOriginal: tc.keep}
		if got := defaultTrack(tc.tracks, shownTracks(tc.tracks, l), l.whitelist); got != tc.want {
			t.Errorf("%v of %v: the default %d, want %d", tc.whitelist, tc.tracks, got, tc.want)
		}
	}
}

// The tracks the packager makes a 5.1 companion of: of a packager that
// doesn't report which, the first track of more than two channels of each
// language that is no commentary; of one that makes one of the tracks shown,
// the first such track of each language that is for the film and shown or
// the default one — of the languages it prefers first, else of the
// whitelist's — and where none is, the first such track for the film,
// hidden though it is, alone; none of a hidden one where another gets one,
// and never one of a commentary or an audio description.
func TestTheTracksThePackagerMakesACompanionOf(t *testing.T) {
	en := []string{"en"}
	for _, tc := range []struct {
		name      string
		tracks    []audioTrack
		which     string
		whitelist []string
		keep      bool
		preferred []string
		want      string
	}{
		{"every surround track", tracksOf("en:51", "de:51", "en:51", "fr:51:c", "it"), tracksEvery, en, true, nil, "0 1"},
		{"every surround track, an audio description's too", tracksOf("en", "en:51:ad"), tracksEvery, en, true, nil, "1"},
		{"the tracks shown: the English, the German hidden", tracksOf("en:51", "de:51"), tracksShown, en, true, nil, "0"},
		{"none shown surround: a German 5.1 hidden keeps its own", tracksOf("en", "de:51"), tracksShown, en, true, nil, "1"},
		{"none shown surround: the first hidden one alone", tracksOf("en", "fr:51", "ja:51"), tracksShown, en, true, nil, "1"},
		{"no whitelist: every language", tracksOf("en", "de:51", "ja:51"), tracksShown, nil, true, nil, "1 2"},
		{"an audio description gets none", tracksOf("en:51:ad", "en"), tracksShown, en, true, nil, ""},
		{"an audio description first in its language", tracksOf("en:51:ad", "en:51"), tracksShown, en, true, nil, "1"},
		{"a commentary gets none", tracksOf("en:51:c", "en"), tracksShown, en, true, nil, ""},
		{"a hidden commentary gets none either", tracksOf("en", "ja:51:c"), tracksShown, en, true, nil, ""},
		{"a track of no language is shown", tracksOf("und:51", "zxx:51"), tracksShown, en, true, nil, "0 1"},
		// the default is a shown track before a hidden one: a hidden 5.1
		// beside a shown one gets none
		{"a hidden 5.1, a shown 5.1 of no language the default", tracksOf("ja:51", "und:51"), tracksShown, en, true, nil, "1"},
		{"a hidden 5.1, a shown stereo track of no language", tracksOf("ja:51", "und"), tracksShown, en, true, nil, "0"},
		{"a single language the whitelist hides, kept", tracksOf("ja:51", "ja"), tracksShown, en, true, nil, "0"},
		{"the same, not kept: the default one, hidden", tracksOf("ja", "ja:51:d"), tracksShown, en, false, nil, "1"},
		{"two languages hidden: the default one's", tracksOf("ja:51", "ko:51:d"), tracksShown, en, true, nil, "1"},
		{"two languages hidden, a stereo default: the first surround track's", tracksOf("ja:d", "ko:51"), tracksShown, en, true,
			nil, "1"},
		// the languages the packager prefers pick the default first
		{"Japanese preferred, the whitelist hiding it: its default keeps its own", tracksOf("en:51", "ja:51"), tracksShown, en,
			true, []string{"ja"}, "0 1"},
		{"English preferred, of the same", tracksOf("en:51", "ja:51"), tracksShown, en, true, []string{"en"}, "0"},
		{"a preferred language no track is of: the whitelist's", tracksOf("ja:51", "en:51"), tracksShown, en, true,
			[]string{"ko"}, "1"},
	} {
		l := languageSettings{whitelist: tc.whitelist, keepOriginal: tc.keep}
		p := PackagerRules{PreferredLanguages: tc.preferred}
		got := companionTracks(tc.tracks, tc.which, l, p.preferredOf(l))
		if s := strings.Trim(fmt.Sprint(got), "[]"); s != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, s, tc.want)
		}
	}
}

// A track's language is the one an admin set for it, else the code its file
// carried, as the packager reads its tag — not the record's language read
// off it: "arm" is a key of its own to the packager, which a whitelist of
// "hy" doesn't show, where the record reads "hy" of it.
func TestATracksLanguageIsTheCodeItsFileCarried(t *testing.T) {
	var src sourceRecord
	if err := json.Unmarshal([]byte(`{"streams": [
		{"type": "audio", "codec": "ac3", "channels": 6, "language": "hy", "languageRaw": "arm"},
		{"type": "audio", "codec": "ac3", "channels": 6, "language": "en", "languageRaw": "eng"},
		{"type": "audio", "codec": "aac", "channels": 2, "language": "hy"}]}`), &src); err != nil {
		t.Fatal(err)
	}
	r := records{src: &src}
	l := languageSettings{whitelist: []string{"hy", "en"}, keepOriginal: true}
	tracks := audioTracks(title{}, r)
	if got := shownOf(shownTracks(tracks, l)); got != "1 2" {
		t.Errorf("shown %q, want the English and the stereo track tagged hy, not the one its file tags arm", got)
	}
	if got := companionTracks(tracks, tracksShown, l, l.whitelist); fmt.Sprint(got) != "[1]" {
		t.Errorf("the companions %v, want the English one's alone", got)
	}
	// an admin's language wins
	tracks = audioTracks(title{AudioLanguages: map[int]string{0: "hye", 1: "jpn"}}, r)
	if got := []string{tracks[0].key, tracks[1].key}; got[0] != "hye" || got[1] != "ja" {
		t.Errorf("the keys an admin's languages give: %v", got)
	}
}

// Which tracks the packager makes a 5.1 companion of, as it reports it: a
// report that names none, or a blank, is a packager planned as the check
// planned it before; "shown" in any case and spaces is shown; anything else
// is no word the check knows.
func TestTheTracksAPackagerReports(t *testing.T) {
	for _, tc := range []struct {
		report string
		want   string
	}{
		{packagerReport, tracksEvery},
		{strings.Replace(packagerShown, `"shown"`, `null`, 1), tracksEvery},
		{strings.Replace(packagerShown, `"shown"`, `""`, 1), tracksEvery},
		{strings.Replace(packagerShown, `"shown"`, `"  "`, 1), tracksEvery},
		{packagerShown, tracksShown},
		{strings.Replace(packagerShown, `"shown"`, `" Shown "`, 1), tracksShown},
		{strings.Replace(packagerShown, `"shown"`, `"every"`, 1), ""},
	} {
		var p PackagerRules
		if err := json.Unmarshal([]byte(tc.report), &p); err != nil {
			t.Fatal(err)
		}
		if got := p.tracks(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.report, got, tc.want)
		}
	}
}

// The language settings are read as the packager reads them: the whitelist
// split at its commas, each language trimmed and in lower case, a blank one
// none; keep_original_if_single true where it is not set, and false of
// anything but true, 1 or yes, a blank too; of a key held twice, its row of
// the lowest id, the spaces around a key aside.
func TestTheLanguageSettingsAsThePackagerReadsThem(t *testing.T) {
	of := func(rows ...[3]string) languageSettings {
		var ss []*model.Setting
		for _, r := range rows {
			ss = append(ss, &model.Setting{ID: r[0], Key: r[1], ValueText: r[2]})
		}
		return languageSettingsOf(model.WorkerSettings(ss))
	}
	for _, tc := range []struct {
		name string
		got  languageSettings
		want string
	}{
		{"none set", of(), "whitelist= keepOriginal=true"},
		{"a whitelist", of([3]string{"a", settingWhitelist, " EN, de ,,zh\t"}), "whitelist=en,de,zh keepOriginal=true"},
		{"kept: yes", of([3]string{"a", settingKeepOriginal, " Yes "}), "whitelist= keepOriginal=true"},
		{"kept: 1", of([3]string{"a", settingKeepOriginal, "1"}), "whitelist= keepOriginal=true"},
		{"not kept: off", of([3]string{"a", settingKeepOriginal, "off"}), "whitelist= keepOriginal=false"},
		{"not kept: blank", of([3]string{"a", settingKeepOriginal, ""}), "whitelist= keepOriginal=false"},
		{"a key held twice, the spaces around one", of([3]string{"b", settingWhitelist, "fr"},
			[3]string{"a", " " + settingWhitelist + " ", "it"}), "whitelist=it keepOriginal=true"},
	} {
		if got := tc.got.key(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
