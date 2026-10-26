package compliance

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/language"
	"golang.org/x/text/language/display"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// The words the check writes for the person who runs the server: what it
// found of a title, what it cannot check, where a title stands in the queue,
// why processing it again cannot help, and what a request to process titles
// again did. Each is a sentence in sentence case that gives the reason, never
// an instruction, and states only what the running system holds: every limit
// is the one a worker reports, read when the text is built and named by the
// worker that allows it, and every value is one a record or a row gives. A
// size the report's columns show (the original's, the package's) is not
// said again.

// nbsp keeps a number on the line of its unit: "1.4 GB", "73 Mbit/s".
const nbsp = " "

// Decimal units, as a phone's storage and the clients' downloads count them.
const (
	megabyte = 1_000_000
	gigabyte = 1_000_000_000
)

// sizeText writes a size as the clients write one: GB with one decimal from
// 1 GB up, without a trailing zero ("1.4 GB", "18 GB"), MB below it ("820
// MB"), decimal units; something there is never "0 MB".
func sizeText(bytes float64) string {
	b := max(int64(bytes+0.5), 0)
	mb := (b + megabyte/2) / megabyte
	if b >= gigabyte || mb >= 1000 {
		tenths := (b*10 + gigabyte/2) / gigabyte
		if tenths%10 == 0 {
			return strconv.FormatInt(tenths/10, 10) + nbsp + "GB"
		}
		return strconv.FormatInt(tenths/10, 10) + "." + strconv.FormatInt(tenths%10, 10) + nbsp + "GB"
	}
	if b > 0 {
		mb = max(mb, 1)
	}
	return strconv.FormatInt(mb, 10) + nbsp + "MB"
}

// rateText writes a bit rate: Mbit/s with one decimal without a trailing
// zero ("73 Mbit/s", "8.4 Mbit/s"), kbit/s below 1 Mbit/s ("448 kbit/s").
func rateText(bps float64) string {
	if bps >= 1e6 {
		return strconv.FormatFloat(roundTo(bps/1e6, 1), 'f', -1, 64) + nbsp + "Mbit/s"
	}
	return strconv.FormatFloat(roundTo(bps/1e3, 0), 'f', -1, 64) + nbsp + "kbit/s"
}

// countText writes a count with a thousands separator: "2,500".
func countText(n int) string {
	if n < 0 {
		return "-" + countText(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// plural is one when n is 1, else several.
func plural(n int, one, several string) string {
	if n == 1 {
		return one
	}
	return several
}

// titlesText is a count of titles: "1 title", "2,500 titles".
func titlesText(n int) string {
	if n == 1 {
		return "1 title"
	}
	return countText(n) + " titles"
}

// joinAnd joins words as a sentence lists them: "A", "A and B", "A, B and C".
func joinAnd(words []string) string { return join(words, "and") }

// joinOr joins words as "A", "A or B", "A, B or C".
func joinOr(words []string) string { return join(words, "or") }

func join(words []string, last string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	}
	return strings.Join(words[:len(words)-1], ", ") + " " + last + " " + words[len(words)-1]
}

// capital starts a sentence with a capital.
func capital(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// sentence ends s as a sentence: a full stop, unless it has one.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "?") || strings.HasSuffix(s, "!") {
		return s
	}
	return s + "."
}

// codecName names a video codec as people know it, from a package's codec
// string or a library's name for it: "H.264", "HEVC", "AV1".
func codecName(codec string) string {
	c := strings.ToLower(strings.TrimSpace(codec))
	for _, f := range []struct{ prefix, name string }{{"avc1", "H.264"}, {"avc3", "H.264"}, {"h264", "H.264"},
		{"hev1", "HEVC"}, {"hvc1", "HEVC"}, {"dvh1", "HEVC"}, {"dvhe", "HEVC"}, {"hevc", "HEVC"}, {"h265", "HEVC"},
		{"av01", "AV1"}, {"av1", "AV1"}, {"vp09", "VP9"}, {"vp9", "VP9"}, {"mp4v", "MPEG-4"}, {"mpeg2", "MPEG-2"}} {
		if strings.HasPrefix(c, f.prefix) {
			return f.name
		}
	}
	return codec
}

// formName names a form of image subtitles: "PGS", "VobSub".
func formName(form string) string {
	switch strings.ToLower(strings.TrimSpace(form)) {
	case "pgs", "sup":
		return "PGS"
	case "vobsub":
		return "VobSub"
	case "dvb":
		return "DVB"
	case "xsub":
		return "XSUB"
	}
	return form
}

// formNames names each form of forms.
func formNames(forms []string) []string {
	out := make([]string, 0, len(forms))
	for _, f := range forms {
		out = append(out, formName(f))
	}
	return out
}

// audioCodecName names the codec of the packager's 5.1 track: "E-AC-3".
func audioCodecName(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "eac3", "ec-3":
		return "E-AC-3"
	case "ac3", "ac-3":
		return "AC-3"
	}
	return codec
}

// languageName names a language in English, as the clients name one:
// "German", "Chinese"; "an unknown language" for und or a code it does not
// know.
func languageName(code string) string {
	if code == "" || code == "und" {
		return "an unknown language"
	}
	tag, err := language.Parse(code)
	if err != nil {
		return code
	}
	if name := display.English.Languages().Name(tag); name != "" && !strings.EqualFold(name, "unknown language") {
		return name
	}
	return code
}

// The findings, each the reason a title breaks its rule.

// bitrateWords is what a title whose video is above the transcoder's limit is
// found to be: its bit rate, approximate when it was read off the package's
// size, and the limit the transcoder allows for its kind and bucket.
func bitrateWords(rate float64, approx bool, limit float64, bucket int, kind string) (string, *string, *string) {
	about := ""
	if approx {
		about = "about "
	}
	text := fmt.Sprintf("The video runs at %s%s, above the %s the transcoder allows for a %d %s.", about, rateText(rate),
		rateText(limit), bucket, kind)
	return text, strp(rateText(rate)), strp(rateText(limit))
}

// sizeWords is what a movie whose original is larger than the transcoder
// allows, and whose package keeps its video as it is, is found to be. The
// original's size is the report's column, not said again.
func sizeWords(limit float64) (string, *string, *string) {
	text := fmt.Sprintf("The original is larger than the %s the transcoder allows for a movie, and the package keeps its video as it is.",
		sizeText(limit))
	return text, nil, strp(sizeText(limit))
}

// surroundTrack names the track the packager makes of a surround source, as
// it reports it: "5.1 E-AC-3", or "E-AC-3 surround" when it reports no
// layout.
func surroundTrack(p *PackagerRules) string {
	codec := ""
	if p.SurroundAudio != nil {
		codec = audioCodecName(*p.SurroundAudio)
	}
	if p.SurroundLayout != nil && strings.TrimSpace(*p.SurroundLayout) != "" {
		return strings.TrimSpace(strings.TrimSpace(*p.SurroundLayout) + " " + codec)
	}
	return strings.TrimSpace(codec + " surround")
}

// surroundWords is what a package that lacks its source's surround is found
// to be: the channels of both, and the track the packager makes, missing.
func surroundWords(had, kept float64, hadOK, keptOK bool, p *PackagerRules) (string, *string, *string) {
	track := surroundTrack(p)
	switch {
	case hadOK && had > 2 && keptOK:
		return fmt.Sprintf("The package has %d audio %s, and the source has %d. The %s track the packager makes is missing.",
			int(kept), plural(int(kept), "channel", "channels"), int(had), track), strp(channels(kept)), strp(track)
	case hadOK && had > 2:
		return fmt.Sprintf("The source has %d-channel sound. The %s track the packager makes is missing from the package.",
			int(had), track), nil, strp(track)
	}
	return fmt.Sprintf("The source has surround sound. The %s track the packager makes is missing from the package.", track),
		nil, strp(track)
}

// imageSubtitleWords is what a package without the source's image subtitles
// in some languages is found to be: each form's languages missing, and the
// forms the packager keeps of them; measured are the languages, the limit
// every form the packager keeps.
func imageSubtitleWords(byForm map[string][]string, langs, kept []string) (string, *string, *string) {
	forms := make([]string, 0, len(byForm))
	for f := range byForm {
		forms = append(forms, f)
	}
	sort.Strings(forms)
	var parts []string
	for i, f := range forms {
		names := make([]string, 0, len(byForm[f]))
		for _, l := range byForm[f] {
			names = append(names, languageName(l))
		}
		part := formName(f) + " subtitles in " + joinAnd(names)
		if i == 0 {
			part = "The source’s " + part
		} else {
			part = "its " + part
		}
		parts = append(parts, part)
	}
	subject := parts[0]
	if len(parts) > 1 {
		subject = strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1] + ","
	}
	text := subject + " are missing from the package. The packager keeps " + joinAnd(formNames(forms)) + " subtitles."
	names := make([]string, 0, len(langs))
	for _, l := range langs {
		names = append(names, languageName(l))
	}
	return text, strp(strings.Join(names, ", ")), strp(strings.Join(formNames(kept), ", "))
}

// codecWords is what a package whose video is not HEVC is found to be, where
// the transcoder makes every video HEVC.
func codecWords(codec string) (string, *string, *string) {
	text := fmt.Sprintf("The video is %s, and the transcoder makes every video HEVC.", codecName(codec))
	measured := codecName(codec)
	if measured != codec {
		measured += " (" + codec + ")"
	}
	return text, strp(measured), strp("HEVC")
}

// What a title without a package is found to be.
const (
	notPackagedText = "It has no package, and none is being made."
	takenInText     = "It was taken in without a package and plays from its original."
	finishedText    = "Its transcode and packaging finished, and no package is recorded."
)

// discImageText is what a title whose file is a disc image is found to be.
var discImageText = asSentence(skipClauses[graph.SkipDiscImage])

// stepWords is what a title whose transcode or package failed or was skipped
// is found to be: who did, why, and whether a retry is scheduled.
func stepWords(worker, status, reason string, retry bool) string {
	var text string
	switch status {
	case "skipped":
		text = "The " + worker + " skipped it"
	default:
		text = "The " + worker + " failed"
	}
	if r := strings.TrimSpace(reason); r != "" {
		text += ": " + r
	}
	text = sentence(text)
	if retry {
		text += " A retry is scheduled."
	}
	return text
}

// Why a title can't be processed again, or why the queue didn't take one, of
// each kind of skip, as a clause: what is true of the title. The console
// says a skip's reason after a colon ("1 title wasn’t queued: its file is a
// disc image, which the transcoder doesn’t encode."); a row's reworkBlocked
// is the clause as a sentence.
var skipClauses = map[string]string{
	graph.SkipDeleted:    "its original was deleted from disk after packaging",
	graph.SkipDeleting:   "its original is being deleted from disk after packaging",
	graph.SkipNoFile:     "it has no file",
	graph.SkipDiscImage:  "its file is a disc image, which the transcoder doesn’t encode",
	graph.SkipNoEpisodes: "the series has no episodes",
	graph.SkipUnknown:    "it’s no longer in the catalog",
	graph.SkipType:       "it isn’t a movie, an episode or a series",
}

// skipOther is a skip of a kind the words don't know.
const skipOther = "other"

// skipReason is why a title of kind wasn't queued, a clause; of a kind the
// words don't know, only that it can't be.
func skipReason(kind string) string {
	if c, ok := skipClauses[kind]; ok {
		return c
	}
	return "it can’t be processed again"
}

// asSentence is a clause as a sentence of its own.
func asSentence(clause string) string { return sentence(capital(clause)) }

// The reasons processing a title again can't help, as a row says them.
var (
	deletedText  = asSentence(skipClauses[graph.SkipDeleted])
	deletingText = asSentence(skipClauses[graph.SkipDeleting])
	noFileText   = asSentence(skipClauses[graph.SkipNoFile])
	discText     = discImageText
)

// queueWords is where a title stands in the re-encode queue, in words, from
// the reason the queue keeps of a title waiting or failed; nil for a reason
// of no shape the queue writes (and for none).
func queueWords(state, reason string) *string {
	r := strings.TrimSpace(reason)
	if r == "" {
		return nil
	}
	after := func(prefix string) (string, bool) {
		rest, ok := strings.CutPrefix(r, prefix)
		return strings.TrimSpace(strings.TrimPrefix(rest, ":")), ok
	}
	switch state {
	case "queued":
		for _, w := range []struct{ prefix, text string }{
			{"transcode is running", "The transcoder is working on it."},
			{"package is running", "The packager is working on it."},
			{"transcode is waiting for its worker", "It is waiting for the transcoder to start it."},
			{"package is waiting for its worker", "It is waiting for the packager to start it."},
			{"the re-encode could not be sent", "It couldn’t be handed to the transcoder."},
		} {
			if strings.HasPrefix(r, w.prefix) {
				return strp(w.text)
			}
		}
	case "failed":
		if rest, ok := after("transcode failed"); ok {
			return strp(stepWords("transcoder", "failed", rest, false))
		}
		if rest, ok := after("package failed"); ok {
			return strp(stepWords("packager", "failed", rest, false))
		}
		if rest, ok := after("package skipped"); ok {
			return strp(stepWords("packager", "skipped", rest, false))
		}
		switch {
		case strings.HasPrefix(r, "package not applicable"):
			return strp("The packager found nothing to package.")
		case strings.HasPrefix(r, "its transcode and package are gone"):
			return strp("Its transcode and package steps were removed.")
		case r == "unknown item":
			return strp(asSentence(skipReason(graph.SkipUnknown)))
		case strings.Contains(r, "is being deleted after packaging"):
			return strp(deletingText)
		case strings.Contains(r, "deleted after packaging"):
			return strp(deletedText)
		case strings.HasSuffix(r, "has no file to encode"):
			return strp(noFileText)
		case strings.HasPrefix(r, "its file is a disc image"):
			return strp(discText)
		}
	}
	return nil
}

// skipKinds are the kinds of skip, in the order a request's message says
// them.
var skipKinds = []string{graph.SkipDeleted, graph.SkipDeleting, graph.SkipNoFile, graph.SkipDiscImage,
	graph.SkipNoEpisodes, graph.SkipUnknown, graph.SkipType, skipOther}

// skipAll is why the titles of a request's message were not queued when all
// of them weren't for one reason; skipSome says it of a number of them,
// among others. Of one title alone, the message says its skipReason.
var (
	skipAll = map[string]string{
		graph.SkipDeleted:    "their originals were deleted from disk after packaging",
		graph.SkipDeleting:   "their originals are being deleted from disk after packaging",
		graph.SkipNoFile:     "they have no file",
		graph.SkipDiscImage:  "their files are disc images, which the transcoder doesn’t encode",
		graph.SkipNoEpisodes: "the series have no episodes",
		graph.SkipUnknown:    "they’re no longer in the catalog",
		graph.SkipType:       "they aren’t movies, episodes or series",
		skipOther:            "they can’t be processed again",
	}
	skipSome = map[string][2]string{ // of 1, of %s
		graph.SkipDeleted:    {"the original of 1 was deleted from disk after packaging", "the originals of %s were deleted from disk after packaging"},
		graph.SkipDeleting:   {"the original of 1 is being deleted from disk after packaging", "the originals of %s are being deleted from disk after packaging"},
		graph.SkipNoFile:     {"1 has no file", "%s have no file"},
		graph.SkipDiscImage:  {"1 is a disc image", "%s are disc images"},
		graph.SkipNoEpisodes: {"1 is a series without episodes", "%s are series without episodes"},
		graph.SkipUnknown:    {"1 is no longer in the catalog", "%s are no longer in the catalog"},
		graph.SkipType:       {"1 isn’t a movie, an episode or a series", "%s aren’t movies, episodes or series"},
		skipOther:            {"1 can’t be processed again", "%s can’t be processed again"},
	}
)

// queuedWords is what a request to process titles again did: how many it
// queued, how many were queued already, how many it skipped and why; with
// none at all, that no title was named, that none fits the filter, or why
// none of those that fit it is queued.
func queuedWords(queued, already int, skipped map[string]int, by graph.QueuedBy) string {
	total := 0
	for _, n := range skipped {
		total += n
	}
	if queued == 0 && already == 0 && total == 0 {
		if !by.Matching {
			return "No title is named, so none is queued."
		}
		return leftOutWords(int(by.InQueue), int(by.Blocked))
	}
	parts := []string{"No title queued to be processed again."}
	if queued > 0 {
		parts[0] = titlesText(queued) + " queued to be processed again."
	}
	switch {
	case already == 1:
		parts = append(parts, "1 title is queued already.")
	case already > 1:
		parts = append(parts, titlesText(already)+" are queued already.")
	}
	if total > 0 {
		var kinds []string
		for _, k := range skipKinds {
			if skipped[k] > 0 {
				kinds = append(kinds, k)
			}
		}
		var why string
		if len(kinds) == 1 {
			why = skipAll[kinds[0]]
			if total == 1 {
				why = skipReason(kinds[0])
			}
		} else {
			var some []string
			for _, k := range kinds {
				if n := skipped[k]; n == 1 {
					some = append(some, skipSome[k][0])
				} else {
					some = append(some, fmt.Sprintf(skipSome[k][1], countText(n)))
				}
			}
			why = joinAnd(some)
		}
		parts = append(parts, titlesText(total)+" "+plural(total, "wasn’t", "weren’t")+" queued: "+why+".")
	}
	return strings.Join(parts, " ")
}

// leftOutWords is why a request by a filter queued no title: none fits it,
// or each that fits it is queued already (inQueue) or can't be processed
// again (blocked).
func leftOutWords(inQueue, blocked int) string {
	const none = "No title queued to be processed again. "
	switch n := inQueue + blocked; {
	case n == 0:
		return "No title fits the filter, so none is queued."
	case blocked == 0 && n == 1:
		return none + "The title that fits the filter is queued already."
	case blocked == 0:
		return none + "The " + titlesText(n) + " that fit the filter are queued already."
	case inQueue == 0 && n == 1:
		return none + "The title that fits the filter can’t be processed again."
	case inQueue == 0:
		return none + "The " + titlesText(n) + " that fit the filter can’t be processed again."
	}
	return none + "Of the " + titlesText(inQueue+blocked) + " that fit the filter, " + countText(inQueue) + " " +
		plural(inQueue, "is", "are") + " queued already and " + countText(blocked) + " can’t be processed again."
}

// titleText names a title as a list of them does: a movie or a series by its
// title and year ("A Film (1991)"), an episode by its series, its number and
// its title ("A Show · S01E02 · Its Title"), a season by its series and its
// number ("A Show · S01"). A part the catalog doesn't hold is left out, and a
// title of which it holds nothing is its id.
func titleText(id string, typ, title, series *string, year, season, episode *int32) string {
	str := func(s *string) string {
		if s == nil {
			return ""
		}
		return strings.TrimSpace(*s)
	}
	name := str(title)
	var parts []string
	add := func(p string) {
		if p != "" {
			parts = append(parts, p)
		}
	}
	switch str(typ) {
	case "episode":
		add(str(series))
		switch {
		case season != nil && episode != nil:
			add(fmt.Sprintf("S%02dE%02d", *season, *episode))
		case episode != nil:
			add(fmt.Sprintf("E%02d", *episode))
		}
		add(name)
	case "season":
		add(str(series))
		if season != nil {
			add(fmt.Sprintf("S%02d", *season))
		} else {
			add(name)
		}
	default:
		if name != "" && year != nil && *year > 0 {
			name += " (" + strconv.Itoa(int(*year)) + ")"
		}
		add(name)
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " · ")
}
