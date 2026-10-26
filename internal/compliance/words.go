package compliance

import (
	"fmt"
	"regexp"
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
// said again. A title's file is its original, never its "source"; a height
// has its p ("2160p"); a bit rate is two words; and what a tool said (an
// exit code, a codec id, an event) is a detail, never in the text.

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

// audioCodecName names the codec of a track the packager makes as people
// know it: "E-AC-3", "AAC".
func audioCodecName(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "eac3", "ec-3":
		return "E-AC-3"
	case "ac3", "ac-3":
		return "AC-3"
	case "aac", "mp4a":
		return "AAC"
	case "opus":
		return "Opus"
	}
	return codec
}

// languageName names a language in English as the clients name one: the
// language, and the script and the region a tag names in brackets
// ("German", "Portuguese (Brazil)", "Chinese (Simplified)"), never a
// dialect's own name ("Brazilian Portuguese"); "an unknown language" for und
// or a code it doesn't know.
func languageName(code string) string {
	c := strings.TrimSpace(code)
	if c == "" || strings.EqualFold(c, "und") {
		return "an unknown language"
	}
	tag, err := language.Parse(c)
	if err != nil {
		return code
	}
	base, _ := tag.Base()
	name := display.English.Languages().Name(language.Make(base.String()))
	if name == "" || strings.EqualFold(name, "unknown language") {
		return code
	}
	var more []string
	if script, conf := tag.Script(); conf == language.Exact {
		more = append(more, scriptName(script))
	}
	if region, conf := tag.Region(); conf == language.Exact {
		more = append(more, display.English.Regions().Name(region))
	}
	if len(more) > 0 {
		name += " (" + strings.Join(more, ", ") + ")"
	}
	return name
}

// scriptName names a script as a language's brackets do: Chinese's
// "Simplified" and "Traditional", any other by its name ("Latin").
func scriptName(s language.Script) string {
	switch s.String() {
	case "Hans":
		return "Simplified"
	case "Hant":
		return "Traditional"
	}
	return display.English.Scripts().Name(s)
}

// The findings, each the reason a title breaks its rule.

// bitrateWords is what a title whose video is above the transcoder's limit is
// found to be: its bit rate, about one when it was read off the package's
// size, and the limit the transcoder allows for its kind and bucket.
func bitrateWords(rate float64, approx bool, limit float64, bucket int, kind string) (string, *string, *string) {
	measured := rateText(rate)
	if approx {
		measured = "about " + measured
	}
	text := fmt.Sprintf("The video runs at %s, above the %s the transcoder allows for a %s %s.", measured,
		rateText(limit), heightText(bucket), kind)
	return text, strp(measured), strp(rateText(limit))
}

// heightText is a height as the clients write one: "2160p".
func heightText(lines int) string { return strconv.Itoa(lines) + "p" }

// sizeWords is what a movie whose original is larger than the transcoder
// allows, and whose package copied its video, is found to be. The
// original's size is the report's column, not said again.
func sizeWords(limit float64) (string, *string, *string) {
	text := fmt.Sprintf("The original is larger than the %s the transcoder allows for a movie, and the package copied its video.",
		sizeText(limit))
	return text, nil, strp(sizeText(limit))
}

// surroundTrack names the track the packager makes of a surround original,
// as it reports it: "5.1 E-AC-3", or "E-AC-3 surround" when it reports no
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

// surroundWords is what a package that lacks its original's surround is
// found to be: the channels of both, and the track the packager makes,
// missing.
func surroundWords(had, kept float64, hadOK, keptOK bool, p *PackagerRules) (string, *string, *string) {
	track := surroundTrack(p)
	switch {
	case hadOK && had > 2 && keptOK:
		return fmt.Sprintf("The package has %d audio %s, and the original has %d. The %s track the packager makes is missing.",
			int(kept), plural(int(kept), "channel", "channels"), int(had), track), strp(channels(kept)), strp(track)
	case hadOK && had > 2:
		return fmt.Sprintf("The original has %d-channel sound. The %s track the packager makes is missing from the package.",
			int(had), track), nil, strp(track)
	}
	return fmt.Sprintf("The original has surround sound. The %s track the packager makes is missing from the package.", track),
		nil, strp(track)
}

// imageSubtitleWords is what a package without the original's image subtitles
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
			part = "The original’s " + part
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
// the transcoder makes every video HEVC; the codec's id, when it isn't its
// name, is the detail.
func codecWords(codec string) (text string, measured, limit, detail *string) {
	name := codecName(codec)
	text = fmt.Sprintf("The video is %s, and the transcoder makes every video HEVC.", name)
	if name != codec {
		detail = strp(codec)
	}
	return text, strp(name), strp("HEVC"), detail
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
// is found to be: which worker, and what it did; the worker's own reason is
// the detail, unless the words say it (a reason they know, as knownReasons
// tells). A step that failed with a retry really scheduled says that it is
// tried again.
func stepWords(worker, status, reason string, retry bool) (string, *string) {
	r := strings.TrimSpace(reason)
	var detail *string
	if r != "" {
		detail = strp(r)
	}
	if status == "skipped" {
		if clause, ok := knownReason(r); ok {
			return "The " + worker + " skipped it: " + clause + ".", nil
		}
		return "The " + worker + " skipped it.", detail
	}
	if head, ok := strings.CutSuffix(r, keptTheOriginal); ok {
		head = strings.TrimSpace(head)
		if clause, ok := knownReason(head); ok {
			return "The " + worker + " left it as it is: " + clause + ".", nil
		}
		return "The " + worker + " left it as it is.", strp(head)
	}
	if retry {
		return "The " + worker + " failed, and tries again by itself.", detail
	}
	return "The " + worker + " failed.", detail
}

// keptTheOriginal ends the reason of a transcode the transcoder refused, and
// left the title as it is.
const keptTheOriginal = "; kept the original"

// knownReasons are reasons a worker gives that the words say in the
// reader's own: a Dolby Vision profile only a tone-mapping encode plays
// right, an original without video.
var knownReasons = []struct {
	re    *regexp.Regexp
	words func(m []string) string
}{
	{regexp.MustCompile(`^Dolby Vision (profile [0-9]+(?:\.[0-9]+)?) needs a tone-mapping encode$`),
		func(m []string) string { return "Dolby Vision " + m[1] + " needs a tone-mapping encode" }},
	{regexp.MustCompile(`(?i)^(?:(?:the )?(?:source|original|file) has )?no video(?: stream)?$`),
		func([]string) string { return "the original has no video" }},
}

// knownReason is reason in the reader's words, for a reason the words know.
func knownReason(reason string) (string, bool) {
	for _, k := range knownReasons {
		if m := k.re.FindStringSubmatch(reason); m != nil {
			return k.words(m), true
		}
	}
	return "", false
}

// Why a title can't be processed again, or why the queue didn't take one, of
// each kind of skip, as a clause: what is true of the title. The console
// says a skip's reason after a colon ("1 title wasn’t queued: its file is a
// disc image, which the transcoder doesn’t encode."); a row's reworkBlocked
// is the clause as a sentence.
var skipClauses = map[string]string{
	graph.SkipDeleted:    "its original was deleted after packaging",
	graph.SkipDeleting:   "its original is being deleted after packaging",
	graph.SkipNoFile:     "it has no file",
	graph.SkipDiscImage:  "its file is a disc image, which the transcoder doesn’t encode",
	graph.SkipNoEpisodes: "the series has no episodes",
	graph.SkipUnknown:    "it is not in the catalog",
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
// the reason the queue keeps of a title waiting or failed, and the reason's
// technical part, its detail (what a worker said, when its worker last
// reported, an event); nil for a reason of no shape the queue writes (and
// for none).
func queueWords(state, reason string) (text, detail *string) {
	r := strings.TrimSpace(reason)
	if r == "" {
		return nil, nil
	}
	after := func(prefix string) (string, bool) {
		rest, ok := strings.CutPrefix(r, prefix)
		return strings.TrimSpace(strings.TrimPrefix(rest, ":")), ok
	}
	some := func(s string) *string {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return &s
	}
	switch state {
	case "queued":
		for _, w := range []struct{ worker, step string }{{"transcoder", "transcode"}, {"packager", "package"}} {
			if rest, ok := after(w.step + " is running"); ok {
				detail := some(rest)
				if m := runningRE.FindStringSubmatch(r); m != nil {
					detail = strp("its worker last reported at " + m[1] + "; timeout " + m[2])
				}
				return strp("The " + w.worker + " is working on it."), detail
			}
			if rest, ok := after(w.step + " is waiting for its worker"); ok {
				detail := some(rest)
				if m := waitingRE.FindStringSubmatch(r); m != nil {
					detail = strp("waiting since " + m[1] + "; timeout " + m[2])
				}
				return strp("It is waiting for the " + w.worker + "."), detail
			}
		}
		if rest, ok := after("the re-encode could not be sent"); ok {
			return strp("It couldn’t be handed to the transcoder."), some(rest)
		}
	case "failed":
		if rest, ok := after("transcode failed"); ok {
			text, detail := stepWords("transcoder", "failed", noErrorGiven(rest), false)
			return &text, detail
		}
		if rest, ok := after("package failed"); ok {
			text, detail := stepWords("packager", "failed", noErrorGiven(rest), false)
			return &text, detail
		}
		if rest, ok := after("package skipped"); ok {
			text, detail := stepWords("packager", "skipped", rest, false)
			return &text, detail
		}
		if rest, ok := after("package not applicable"); ok {
			return strp("The packager found nothing to package."), some(rest)
		}
		switch {
		case strings.HasPrefix(r, "its transcode and package are gone"):
			return strp("Its transcode and package steps were removed."), nil
		case r == "unknown item":
			return strp(asSentence(skipReason(graph.SkipUnknown))), nil
		case strings.Contains(r, "is being deleted after packaging"):
			return strp(deletingText), eventOf(r)
		case strings.Contains(r, "deleted after packaging"):
			return strp(deletedText), eventOf(r)
		case strings.HasSuffix(r, "has no file to encode"):
			return strp(noFileText), nil
		case strings.HasPrefix(r, "its file is a disc image"):
			return strp(discText), nil
		}
	}
	return nil, nil
}

// The shapes of the reasons the queue keeps of a title it holds back while
// its step runs or waits for its worker (retry's busy reasons).
var (
	runningRE = regexp.MustCompile(`^(?:transcode|package) is running: its worker last reported at (\S+), .*timeout of (\S+)$`)
	waitingRE = regexp.MustCompile(`^(?:transcode|package) is waiting for its worker since (\S+), .*timeout of (\S+)$`)
	eventRE   = regexp.MustCompile(`\((event [^)]*)\)`)
)

// noErrorGiven is a step's reason as the queue keeps it, none when it says
// its step gave none.
func noErrorGiven(reason string) string {
	if reason == "no error given" {
		return ""
	}
	return reason
}

// eventOf is the event a reason names of an original deleted after
// packaging ("event <id>, <when>"), nil for none.
func eventOf(reason string) *string {
	if m := eventRE.FindStringSubmatch(reason); m != nil {
		return strp(m[1])
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
		graph.SkipDeleted:    "their originals were deleted after packaging",
		graph.SkipDeleting:   "their originals are being deleted after packaging",
		graph.SkipNoFile:     "they have no file",
		graph.SkipDiscImage:  "their files are disc images, which the transcoder doesn’t encode",
		graph.SkipNoEpisodes: "the series have no episodes",
		graph.SkipUnknown:    "they are not in the catalog",
		graph.SkipType:       "they aren’t movies, episodes or series",
		skipOther:            "they can’t be processed again",
	}
	skipSome = map[string][2]string{ // of 1, of %s
		graph.SkipDeleted:    {"the original of 1 was deleted after packaging", "the originals of %s were deleted after packaging"},
		graph.SkipDeleting:   {"the original of 1 is being deleted after packaging", "the originals of %s are being deleted after packaging"},
		graph.SkipNoFile:     {"1 has no file", "%s have no file"},
		graph.SkipDiscImage:  {"1 is a disc image", "%s are disc images"},
		graph.SkipNoEpisodes: {"1 is a series without episodes", "%s are series without episodes"},
		graph.SkipUnknown:    {"1 is not in the catalog", "%s are not in the catalog"},
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
