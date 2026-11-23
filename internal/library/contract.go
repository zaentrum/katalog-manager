package library

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The contract's deletion gate: the record logic's gate block (the schemas'
// tools/libv2_records.py at 71999959, sha256 5109334f..., "the deletion gate,
// shared"), function for function and line for line, which the validator and
// the rebuild run as the same text. The gate (ReadGateWith) runs it beside
// its own rules on the records it reads and the catalog's read of the
// original as the analyzer reported it, and keeps an original either keeps:
// what the contract holds, the gate holds; what the gate holds beyond it is
// the gate's own (the differential test, TestTheGateHoldsWhatTheRecordLogicHolds,
// holds the two to it, input by input, and lists the kinds the gate holds
// alone).
//
// It reads JSON values as Python's json gives them: an object (Doc), a list
// ([]any), text, a boolean, null, and a number (json.Number: an int where it
// is written with no point and no exponent, a float else). Where the record
// logic raises (an object's .get of a value that is no object, a loop over a
// number, a comparison of values Python can't order) it holds both, as a
// reader that can't tell keeps the original; where its exact behaviour is
// hard to follow (a float written as Python's float() alone reads it), it
// reads less, which tells less and holds more, never the other way.

// contractNever are the keys no deletion gives up (the record logic's
// NEVER_ACCEPTED): an original that has closed captions or whose captions
// are not known, and one in fields, are kept.
var contractNever = []string{"closedCaptions", "interlaced"}

// raised is the panic the port raises where the record logic raises.
type raised struct{ why string }

func pyRaise(format string, args ...any) { panic(raised{fmt.Sprintf(format, args...)}) }

// pyGet is d.get(key) of an object d (nil: of none).
func pyGet(d Doc, key string) any {
	v, _ := d.Get(key)
	return v
}

// pyHas is key in d.
func pyHas(d Doc, key string) bool {
	_, ok := d.Get(key)
	return ok
}

// pyDict is v if isinstance(v, dict) else {}.
func pyDict(v any) Doc {
	d, _ := v.(Doc)
	return d
}

// pyOr is (v or {}) of a value whose .get follows: {} of one Python takes for
// false, v of an object, and a raise of anything else.
func pyOr(v any, what string) Doc {
	if !truthy(v) {
		return nil
	}
	d, ok := v.(Doc)
	if !ok {
		pyRaise("%s is %s, which has no .get", what, pyStr(v))
	}
	return d
}

// pyIter is what `for x in v or []` loops over: a list's items, an object's
// keys, a text's characters; a raise of any other value Python takes for
// true (a number, true).
func pyIter(v any, what string) []any {
	if !truthy(v) {
		return nil
	}
	switch x := v.(type) {
	case []any:
		return x
	case Doc:
		out := make([]any, 0, len(x))
		for _, f := range x {
			out = append(out, f.Key)
		}
		return out
	case string:
		out := []any{}
		for _, r := range x {
			out = append(out, string(r))
		}
		return out
	}
	pyRaise("%s is %s, which can't be looped over", what, pyStr(v))
	return nil
}

// pyIn is v in names, of a value as given: text equal to one of them.
func pyIn(v any, names []string) bool {
	s, ok := v.(string)
	return ok && slices.Contains(names, s)
}

// pyOrText is str(v or alt), as names are read.
func pyOrText(v any, alt string) string {
	if !truthy(v) {
		return alt
	}
	return pyStr(v)
}

// contractInt is v as Python's json gives an int (no bool), and whether it is one.
func contractInt(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return nil, false
		}
		return new(big.Int).SetString(x.String(), 10)
	case int:
		return big.NewInt(int64(x)), true
	case int64:
		return big.NewInt(x), true
	}
	return nil, false
}

// pyIntFloat is float(n) of an int, as Python converts one (the nearest
// float); a raise (OverflowError) of one too large for a float.
func pyIntFloat(n *big.Int) float64 {
	f, _ := new(big.Float).SetInt(n).Float64()
	if math.IsInf(f, 0) {
		pyRaise("an int too large for a float")
	}
	return f
}

// contractGateNumber is the record logic's gate_number: v where it is a
// finite number (no boolean, no text, no NaN or infinity, no whole number
// too large for a float), as a float, and whether it is one.
func contractGateNumber(v any) (float64, bool) {
	if n, ok := contractInt(v); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, !math.IsInf(f, 0)
	}
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = strconv.ParseFloat(x.String(), 64); err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
	case float64:
		f = x
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

// contractGateCount is the record logic's gate_count: v where it is a whole
// number from 0 (to top, where top is not nil), and whether it is one.
func contractGateCount(v any, top *big.Int) (*big.Int, bool) {
	n, ok := contractInt(v)
	if !ok || n.Sign() < 0 || (top != nil && n.Cmp(top) > 0) {
		return nil, false
	}
	return n, true
}

// pySecondsText is a text float() reads as a decimal number, spaces about it
// (fewer forms than float() takes: no underscores, no digits of other
// scripts; a text it reads that this doesn't is a time not stated, which
// gives no plan and holds).
var pySecondsText = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)

// contractGateSeconds is the record logic's gate_seconds: a time or a length
// as ffprobe gives one ("12.345000") or a record keeps one (a number), as
// seconds, and whether it is one: no boolean, finite.
func contractGateSeconds(v any) (float64, bool) {
	switch x := v.(type) {
	case bool, nil:
		return 0, false
	case string:
		t := pyStrip(x)
		if !pySecondsText.MatchString(t) {
			return 0, false
		}
		f, err := strconv.ParseFloat(t, 64)
		if err != nil && !math.IsInf(f, 0) {
			return 0, false
		}
		return f, !math.IsNaN(f) && !math.IsInf(f, 0)
	}
	if _, ok := v.([]any); ok {
		return 0, false
	}
	if _, ok := v.(Doc); ok {
		return 0, false
	}
	return contractGateNumber(v)
}

// pyRound3 is Python's round(x, 3) of a float: the float nearest x rounded
// to three decimals (ties to even, of the exact binary value).
func pyRound3(x float64) float64 {
	f, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 3, 64), 64)
	if err != nil {
		return x
	}
	return f
}

// The plan's numbers as the record logic reads them (CAPTION_PLAN).
var (
	contractWindows    = append([]float64{0}, PlannedWindows...)
	contractMinFrames  = big.NewInt(MinWindowFrames)
	contractLeadingTop = big.NewInt(WholeLeadingMax)
)

// contractWindowOf is one entry of a read's windows as the record logic's
// window_of reads it, with the window as a record keeps it (doc).
type contractWindowOf struct {
	at, start, end float64
	frames         *big.Int
	captions       bool
	doc            Doc // as a record keeps it
	whole          bool
	packets        *big.Int
	leading        any // its leading as given; nil: none
	hasLeading     bool
	keyed          bool
}

// contractWindow is the record logic's window_of: one entry of a picture's or
// a read's caption windows as every reader reads it, (window, sound).
func contractWindow(w any) (contractWindowOf, bool) {
	d, isDoc := w.(Doc)
	at, okAt := contractGateNumber(pyGet(d, "at"))
	start, okStart := contractGateNumber(pyGet(d, "start"))
	end, okEnd := contractGateNumber(pyGet(d, "end"))
	frames, okFrames := contractGateCount(pyGet(d, "frames"), nil)
	captions := pyGet(d, "captions")
	var counts []Field
	for _, k := range []string{"interlacedFrames", "repeatFrames"} {
		if v, has := d.Get(k); has {
			counts = append(counts, Field{k, v})
		}
	}
	var whole []Field
	for _, k := range []string{"whole", "packets", "leading"} {
		if v, has := d.Get(k); has {
			whole = append(whole, Field{k, v})
		}
	}
	wholeOf := Doc(whole)
	cb, capBool := captions.(bool)
	sound := isDoc && okAt && 0 <= at && at <= 1 && okStart && okEnd && end >= start && okFrames
	if sound {
		sound = (capBool && !(frames.Sign() == 0 && cb)) || (captions == nil && frames.Sign() == 0)
	}
	if sound {
		for _, c := range counts {
			if _, ok := contractGateCount(c.Value, frames); !ok {
				sound = false
			}
		}
	}
	if sound && pyHas(d, "keyframeBefore") {
		if _, ok := pyGet(d, "keyframeBefore").(bool); !ok {
			sound = false
		}
	}
	var packets *big.Int
	if sound && len(whole) > 0 {
		var okPackets bool
		packets, okPackets = contractGateCount(pyGet(wholeOf, "packets"), nil)
		sound = pyGet(wholeOf, "whole") == true && at == 0 && okPackets && packets.Cmp(frames) >= 0
		if sound && pyHas(wholeOf, "leading") {
			_, ok := contractGateCount(pyGet(wholeOf, "leading"), contractLeadingTop)
			sound = ok
		}
	}
	if !sound {
		a := 0.0
		if okAt && 0 <= at && at <= 1 {
			a = at
		}
		p := 0.0
		if okStart {
			p = start
		}
		out := contractWindowOf{at: pyRound3(a), start: pyRound3(p), end: pyRound3(p), frames: new(big.Int)}
		out.doc = Doc{{"at", out.at}, {"start", out.start}, {"end", out.end}, {"frames", 0}, {"captions", false}}
		return out, false
	}
	out := contractWindowOf{at: pyRound3(at), start: pyRound3(start), end: pyRound3(end), frames: frames,
		captions: captions == true, packets: packets}
	out.doc = Doc{{"at", out.at}, {"start", out.start}, {"end", out.end}, {"frames", json.Number(frames.String())},
		{"captions", captions == true}}
	out.doc = append(out.doc, counts...)
	out.doc = append(out.doc, whole...)
	if len(whole) > 0 {
		out.whole = pyGet(wholeOf, "whole") == true
		out.leading, out.hasLeading = wholeOf.Get("leading")
	}
	if pyGet(d, "keyframeBefore") == true {
		out.keyed = true
		out.doc = append(out.doc, Field{"keyframeBefore", true})
	}
	return out, true
}

// contractWindowsShow is the record logic's windows_show: whether a window of
// windows (as given) is one that decoded frames and shows captions.
func contractWindowsShow(windows any) bool {
	list, ok := windows.([]any)
	if !ok {
		return false
	}
	for _, w := range list {
		if o, sound := contractWindow(w); sound && o.captions {
			return true
		}
	}
	return false
}

// contractPlan is the plan's span the record logic's plan_of gives:
// {start, end, step}; nil: no plan.
type contractPlan struct{ start, end, step float64 }

// contractPlanOf is the record logic's plan_of.
func contractPlanOf(probe any, durationMs any) *contractPlan {
	p, ok := probe.(Doc)
	if !ok {
		return nil
	}
	if _, ok := pyGet(p, "streams").([]any); !ok {
		return nil
	}
	format := pyDict(pyGet(p, "format"))
	picture := contractFilmOf(p)
	if picture == nil {
		return nil
	}
	start, okStart := contractGateSeconds(pyGet(picture, "start_time"))
	if !okStart {
		start, okStart = contractGateSeconds(pyGet(format, "start_time"))
	}
	length, okLength := contractGateSeconds(pyGet(picture, "duration"))
	if !okLength || length <= 0 {
		if ms, ok := contractGateNumber(durationMs); ok && ms > 0 {
			length, okLength = ms/1000, true
		} else {
			length, okLength = contractGateSeconds(pyGet(format, "duration"))
		}
	}
	step, okStep := frameStepOf(picture)
	if !okStart || !okLength || length <= 0 || !okStep {
		return nil
	}
	return &contractPlan{start: start, end: start + length, step: step}
}

// contractFilmOf is the record logic's film_of: the probe's one video stream
// that is no cover by the closed lists' rule, nil where there is none or two
// could each be it.
func contractFilmOf(probe Doc) Doc {
	streams := pyIter(pyGet(probe, "streams"), "a probe's streams")
	films := probeFilms(streams)
	if len(films) == 1 {
		return films[0]
	}
	return nil
}

// contractTolerance is the record logic's caption_tolerance.
func contractTolerance(plan *contractPlan) float64 { return 1*plan.step + SpanSlack }

// contractLands is the record logic's window_lands.
func contractLands(start, place, gop float64, plan *contractPlan, keyed bool) bool {
	if gop == 0 {
		return math.Abs(start-place) <= contractTolerance(plan)
	}
	return math.Abs(start-place) <= math.Max(WindowPlaceSlack, gop+WindowGopPlus) ||
		(keyed && place-WindowKeyframeSlack <= start && start <= place)
}

// contractPlaces is the record logic's plan_places.
func contractPlaces(plan *contractPlan) []float64 {
	out := make([]float64, 0, len(contractWindows))
	for _, at := range contractWindows {
		if at == 0 {
			out = append(out, plan.start)
			continue
		}
		// the product rounded before it is added, as Python adds it (no fused
		// multiply-add: shortReach)
		out = append(out, math.Max(plan.start, math.Min(plan.start+float64(at*(plan.end-plan.start)), plan.end-WindowTail)))
	}
	return out
}

// contractReach is how far a window reaches in the record logic's
// plan_short: its packets at the frame step, the gop and 0.5 s, the product
// rounded before it is added (no fused multiply-add: shortReach).
func contractReach(plan *contractPlan, gop float64) float64 {
	return float64(float64(WindowPackets)*plan.step) + gop + ShortReachSlack
}

// contractShort is the record logic's plan_short.
func contractShort(plan *contractPlan, gop float64) bool {
	reach := contractReach(plan, gop)
	places := contractPlaces(plan)
	sort.Float64s(places)
	for i := 1; i < len(places); i++ {
		if places[i]-places[i-1] < reach {
			return true
		}
	}
	return false
}

// contractWholeComplete is the record logic's whole_complete.
func contractWholeComplete(w contractWindowOf) bool {
	leading, ok := contractGateCount(w.leading, contractLeadingTop)
	if !w.hasLeading || !ok || w.frames.Sign() <= 0 {
		return false
	}
	return new(big.Int).Sub(w.packets, leading).Cmp(w.frames) <= 0 && w.frames.Cmp(w.packets) <= 0
}

// contractTellsNone is the record logic's windows_tell_none: whether a read
// ({windows, span, gop}) tells that its picture carries no closed captions,
// by the plan over its span.
func contractTellsNone(read any, plan *contractPlan) bool {
	r, ok := read.(Doc)
	if plan == nil || !ok {
		return false
	}
	list, ok := pyGet(r, "windows").([]any)
	if !ok || len(list) == 0 {
		return false
	}
	var ws []contractWindowOf
	for _, w := range list {
		o, sound := contractWindow(w)
		if !sound {
			return false
		}
		ws = append(ws, o)
	}
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].at < ws[j].at })
	for _, w := range ws {
		if w.captions {
			return false
		}
	}
	tol := contractTolerance(plan)
	span := pyDict(pyGet(r, "span"))
	first, okF := contractGateNumber(pyGet(span, "start"))
	last, okL := contractGateNumber(pyGet(span, "end"))
	if !okF || !okL || math.Abs(first-plan.start) > tol || math.Abs(last-plan.end) > tol {
		return false
	}
	gop := 0.0
	if g := pyGet(r, "gop"); g != nil {
		var okG bool
		if gop, okG = contractGateNumber(g); !okG {
			return false
		}
	}
	if gop < 0 || gop > float64(GopPackets)*plan.step {
		return false
	}
	for _, w := range ws {
		if w.whole {
			w := ws[0]
			return len(ws) == 1 && w.whole && contractWholeComplete(w) &&
				pyIntFloat(w.packets)*plan.step >= plan.end-plan.start-tol &&
				math.Abs(w.start-plan.start) <= tol && math.Abs(w.end-plan.end) <= tol
		}
	}
	if contractShort(plan, gop) || len(ws) != len(contractWindows) {
		return false
	}
	places := contractPlaces(plan)
	for i, w := range ws {
		if math.Abs(w.at-contractWindows[i]) > 1e-3 || w.frames.Cmp(contractMinFrames) < 0 ||
			!contractLands(w.start, places[i], gop, plan, w.keyed) {
			return false
		}
	}
	for i := 1; i < len(ws); i++ {
		if !(ws[i].start >= ws[i-1].end && ws[i].start > ws[i-1].start) {
			return false
		}
	}
	return true
}

// contractCountsFields is the record logic's windows_count_fields: whether
// windows (as given) put their picture in fields.
func contractCountsFields(windows any) bool {
	list, ok := windows.([]any)
	if !ok {
		return true
	}
	for _, w := range list {
		d, ok := w.(Doc)
		if !ok {
			continue
		}
		frames, okFrames := contractGateCount(pyGet(d, "frames"), nil)
		top := frames
		if !okFrames {
			top = nil
		}
		for _, k := range []string{"interlacedFrames", "repeatFrames"} {
			if v, has := d.Get(k); has {
				// gate_count(v, None) of frames that are none: no top
				if _, ok := contractGateCount(v, top); !ok {
					return true
				}
			}
		}
		if n, ok := contractGateCount(pyGet(d, "interlacedFrames"), top); ok && n.Sign() > 0 {
			return true
		}
	}
	return false
}

// contractPictureOf is the record logic's picture_of: the first video stream
// of a record's streams no dispositions mark attachedPic; nil: none.
func contractPictureOf(streams any) Doc {
	for _, x := range pyIter(streams, "a record's streams") {
		d, ok := x.(Doc)
		if !ok || pyGet(d, "type") != "video" {
			continue
		}
		if !truthy(pyGet(pyOr(pyGet(d, "dispositions"), "a stream's dispositions"), "attachedPic")) {
			return d
		}
	}
	return nil
}

// contractCaptionRead is the record logic's caption_read_of.
func contractCaptionRead(picture Doc) Doc {
	return Doc{{"windows", pyGet(picture, "captionWindows")}, {"span", pyGet(picture, "captionSpan")},
		{"gop", pyGet(picture, "captionGop")}}
}

// contractCaptionsKnown is the record logic's captions_known.
func contractCaptionsKnown(streams []any, off []string, plan *contractPlan) bool {
	picture := contractPictureOf(streams)
	for _, x := range streams {
		d, ok := x.(Doc)
		if ok && pyIn(pyGet(d, "type"), []string{"subtitle", "data"}) && pyGet(d, "closedCaptions") == true {
			return true
		}
	}
	if pyGet(picture, "captionSignature") == true || contractWindowsShow(pyGet(picture, "captionWindows")) {
		return true
	}
	return contractTellsNone(contractCaptionRead(picture), plan) && len(off) == 0 &&
		(!pyIn(pyGet(picture, "codec"), ScannedCodecs) || pyGet(picture, "captionSignature") == false)
}

// contractFieldOrder is the record logic's field_order_word: as written,
// "unknown" where none is given.
func contractFieldOrder(v any) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return "unknown"
}

// contractReadThrough is the record logic's read_through.
func contractReadThrough(verified any) bool {
	v, ok := verified.(Doc)
	if !ok || pyGet(v, "ok") != true || pyGet(v, "complete") != true {
		return false
	}
	frames, ok1 := contractGateCount(pyGet(v, "frames"), nil)
	packets, ok2 := contractGateCount(pyGet(v, "packets"), nil)
	leading, ok3 := contractGateCount(pyGet(v, "leading"), nil)
	return ok1 && ok2 && ok3 && frames.Cmp(new(big.Int).Sub(packets, leading)) == 0
}

// contractScanHolds is the record logic's scan_holds: why the scan of the
// package record pkg keeps its title's originals in fields, "" when it says
// nothing or lets them go.
func contractScanHolds(pkg any) string {
	p, ok := pkg.(Doc)
	if !ok {
		return "a package record of the title can't be read"
	}
	scanV := pyGet(p, "scan")
	if scanV == nil {
		return ""
	}
	scan, ok := scanV.(Doc)
	if !ok {
		return "the scan of a package of the title is no object"
	}
	found, done, verified := pyGet(scan, "found"), pyGet(scan, "done"), pyGet(scan, "verified")
	if found != NothingFound {
		return "the scan of a package of the title found " + pyStr(found)
	}
	if !contractReadThrough(verified) {
		return "the scan of a package of the title found progressive without its whole title read through"
	}
	if !pyIn(done, []string{"progressive", "none"}) {
		return "the scan of a package of the title says " + pyStr(done) + " was done"
	}
	vd := verified.(Doc)
	chunks := any(json.Number("0"))
	if v, has := vd.Get("chunksInFields"); has {
		chunks = v
	}
	if n, ok := contractGateCount(chunks, nil); !ok || n.Sign() > 0 {
		return "the scan of a package of the title found progressive, but its read-through counted chunks in fields"
	}
	if idet, has := scan.Get("idet"); has && idet != nil {
		id, isDoc := idet.(Doc)
		counts := map[string]*big.Int{}
		for _, k := range []string{"tff", "bff", "progressive"} {
			v := any(json.Number("0"))
			if isDoc {
				if x, has := id.Get(k); has {
					v = x
				}
			}
			n, ok := contractGateCount(v, nil)
			if !isDoc || !ok {
				return "the scan of a package of the title found progressive, but its examination counted more " +
					"fields than frames"
			}
			counts[k] = n
		}
		if new(big.Int).Add(counts["tff"], counts["bff"]).Cmp(counts["progressive"]) > 0 {
			return "the scan of a package of the title found progressive, but its examination counted more " +
				"fields than frames"
		}
	}
	return ""
}

// contractInFields is the record logic's in_fields: why the originals are in
// fields as the deletion gate reads it, "" where nothing says so.
func contractInFields(sources, packages, probes, reads []any) string {
	for _, sv := range sources {
		s, ok := sv.(Doc)
		if !ok {
			return "a source record of it can't be read"
		}
		if pyGet(pyOr(pyGet(s, "essence"), "a record's essence"), "interlaced") == true {
			return "its record is marked interlaced"
		}
		for _, x := range pyIter(pyGet(s, "streams"), "a record's streams") {
			d, ok := x.(Doc)
			if !ok || pyGet(d, "type") != "video" ||
				truthy(pyGet(pyOr(pyGet(d, "dispositions"), "a stream's dispositions"), "attachedPic")) {
				continue
			}
			if o := contractFieldOrder(pyGet(d, "fieldOrder")); o != NothingFound {
				return "its record's field order is " + strconv.Quote(o)
			}
			if pyHas(d, "captionWindows") && contractCountsFields(pyGet(d, "captionWindows")) {
				return "a caption window of its picture counted interlaced frames, or a count it can't be read for"
			}
		}
	}
	for _, pv := range probes {
		p, ok := pv.(Doc)
		if !ok {
			continue
		}
		list, _ := pyGet(p, "streams").([]any)
		for _, x := range list {
			d, ok := x.(Doc)
			if ok && pyGet(d, "codec_type") == "video" && !probeCover(d) {
				if o := contractFieldOrder(pyGet(d, "field_order")); o != NothingFound {
					return "its probe's field order is " + strconv.Quote(o)
				}
			}
		}
	}
	for _, r := range reads {
		d, ok := r.(Doc)
		if !ok || contractCountsFields(pyGet(d, "windows")) {
			return "the read its deletion carries counted interlaced frames, or can't be read"
		}
	}
	for _, pkg := range packages {
		if why := contractScanHolds(pkg); why != "" {
			return why
		}
	}
	return ""
}

// contractCaptionStream is the record logic's caption_stream, of a probe's
// stream.
func contractCaptionStream(s Doc) bool {
	return slices.Contains(CaptionStreamCodecs, pyLower(pyOrText(pyGet(s, "codec_name"), ""))) ||
		slices.Contains(CaptionStreamTags, pyLower(pyOrText(pyGet(s, "codec_tag_string"), "")))
}

// contractChapterStream is the record logic's chapter_stream.
func contractChapterStream(st any) bool {
	d, ok := st.(Doc)
	return ok && pyGet(d, "codec_type") == "data" && pyGet(d, "codec_name") == "bin_data" &&
		pyGet(d, "codec_tag_string") == "text"
}

// contractChapterTrack is the record logic's chapter_track.
func contractChapterTrack(st Doc, probe Doc) bool {
	format := pyDict(pyGet(probe, "format"))
	marks := 0
	if list, ok := pyGet(probe, "chapters").([]any); ok {
		marks = len(list)
	}
	n, ok := gateDigits(pyGet(st, "nb_frames"))
	counted := ok && n.Cmp(big.NewInt(int64(marks))) == 0
	if pyGet(format, "format_name") != "mov,mp4,m4a,3gp,3g2,mj2" || !contractChapterStream(st) || marks <= 0 || !counted {
		return false
	}
	streams := 0
	for _, x := range pyIter(pyGet(probe, "streams"), "a probe's streams") {
		if contractChapterStream(x) {
			streams++
		}
	}
	return streams == 1
}

// pyVBI is x in VBI_HEIGHTS: a number equal to one of them.
func pyVBI(v any) bool {
	n, ok := pyExact(v)
	if !ok {
		return false
	}
	for _, h := range VBIHeights {
		if n.Cmp(new(big.Rat).SetFloat64(h)) == 0 {
			return true
		}
	}
	return false
}

// contractOffList is the record logic's off_list of a probe.
func contractOffList(probe Doc) []string {
	format := pyDict(pyGet(probe, "format"))
	var off []string
	var films []Doc
	name := pyOrText(pyGet(format, "format_name"), "none")
	if !slices.Contains(CaptionContainers, name) {
		off = append(off, "container:"+name)
	}
	for _, x := range pyIter(pyGet(probe, "streams"), "a probe's streams") {
		st, ok := x.(Doc)
		if !ok {
			continue
		}
		kind, codec := pyOrText(pyGet(st, "codec_type"), "none"), pyOrText(pyGet(st, "codec_name"), "none")
		tag := pyLower(pyOrText(pyGet(st, "codec_tag_string"), ""))
		if codec == "unknown" {
			codec = "none"
		}
		what := codec
		if codec == "none" && tag != "" && !strings.HasPrefix(tag, "[") {
			what = tag
		}
		if slices.Contains(CaptionStreamCodecs, codec) || slices.Contains(CaptionStreamTags, tag) {
			continue
		}
		switch {
		case kind == "video":
			if !probeCover(st) {
				films = append(films, st)
			}
		case kind == "audio" || kind == "attachment" || (kind == "subtitle" && slices.Contains(SubtitleCodecs, codec)) ||
			(kind == "data" && (slices.Contains(DataCodecs, codec) || slices.Contains(DataTags, tag) ||
				contractChapterTrack(st, probe))):
		default:
			off = append(off, "stream:"+kind+":"+what)
		}
	}
	if len(films) > 1 {
		off = append(off, fmt.Sprintf("video-streams:%d", len(films)))
	}
	for _, st := range films {
		codec := pyOrText(pyGet(st, "codec_name"), "none")
		if !slices.Contains(CaptionCodecs, codec) && !slices.Contains(off, "codec:"+codec) {
			off = append(off, "codec:"+codec)
		}
	}
	for _, st := range films {
		if pyVBI(pyGet(st, "height")) {
			off = append(off, "picture:vbi-lines")
			break
		}
	}
	return off
}

// contractRecordOffList is the record logic's record_off_list.
func contractRecordOffList(record Doc) []string {
	if v, has := record.Get("offList"); has {
		list, ok := v.([]any)
		var out []string
		for _, e := range list {
			s, isText := e.(string)
			if !isText || s == "" {
				ok = false
			}
			out = append(out, s)
		}
		if !ok {
			return []string{"offList:unreadable"}
		}
		return out
	}
	nameV := pyGet(pyOr(pyGet(record, "container"), "a record's container"), "format")
	if !truthy(nameV) {
		nameV = "none"
	}
	var off []string
	if !pyIn(nameV, CaptionContainers) {
		off = append(off, "container:"+pyStr(nameV))
	}
	var films []Doc
	for _, x := range pyIter(pyGet(record, "streams"), "a record's streams") {
		d, ok := x.(Doc)
		if !ok {
			off = append(off, "stream:unreadable")
			continue
		}
		kind, codec := pyGet(d, "type"), pyGet(d, "codec")
		if !truthy(kind) {
			kind = "none"
		}
		if !truthy(codec) {
			codec = "none"
		}
		if codec == "unknown" {
			codec = "none"
		}
		if pyIn(codec, CaptionStreamCodecs) || (pyIn(kind, []string{"subtitle", "data"}) && truthy(pyGet(d, "closedCaptions"))) {
			continue
		}
		switch {
		case kind == "video":
			if !truthy(pyGet(pyOr(pyGet(d, "dispositions"), "a stream's dispositions"), "attachedPic")) {
				films = append(films, d)
			}
		case pyIn(kind, []string{"audio", "attachment"}) || (kind == "subtitle" && pyIn(codec, SubtitleCodecs)) ||
			(kind == "data" && pyIn(codec, DataCodecs)):
		default:
			off = append(off, "stream:"+pyStr(kind)+":"+pyStr(codec))
		}
	}
	if len(films) > 1 {
		off = append(off, fmt.Sprintf("video-streams:%d", len(films)))
	}
	for _, x := range films {
		codec := pyGet(x, "codec")
		if !truthy(codec) {
			codec = "none"
		}
		if !pyIn(codec, CaptionCodecs) && !slices.Contains(off, "codec:"+pyStr(codec)) {
			off = append(off, "codec:"+pyStr(codec))
		}
	}
	for _, x := range films {
		if pyVBI(pyGet(x, "height")) {
			off = append(off, "picture:vbi-lines")
			break
		}
	}
	return off
}

// contractProbeShape is the record logic's probe_shape: why the probe kept
// of a source can't be read as one, "" when it can.
func contractProbeShape(probe any) string {
	p, ok := probe.(Doc)
	if !ok {
		return "its probe can't be read"
	}
	_, okFormat := pyGet(p, "format").(Doc)
	list, okStreams := pyGet(p, "streams").([]any)
	if !okFormat || !okStreams || len(list) == 0 {
		return "its probe is no probe: no format, or no list of streams"
	}
	for _, x := range list {
		if _, ok := x.(Doc); !ok {
			return "its probe is no probe: no format, or no list of streams"
		}
	}
	return ""
}

// contractGateHolds is the record logic's gate_holds: what the gate keeps the
// originals of a version for that their essences may not say, {key: why}.
func contractGateHolds(sources, packages, probes, reads []any) map[string]string {
	out := map[string]string{}
	set := func(k, why string) {
		if _, has := out[k]; !has {
			out[k] = why
		}
	}
	both := func(why string) {
		set("closedCaptions", why)
		set("interlaced", why)
	}
	if len(sources) == 0 {
		both("no source of it is known")
	}
	var good []any
	for _, s := range sources {
		if _, ok := s.(Doc); !ok {
			both("a source record of it can't be read")
		} else {
			good = append(good, s)
		}
	}
	var probeDocs []any
	for _, p := range probes {
		if _, ok := p.(Doc); ok {
			probeDocs = append(probeDocs, p)
		}
	}
	if why := contractInFields(good, packages, probeDocs, reads); why != "" {
		set("interlaced", why)
	}
	for i, sv := range sources {
		s, ok := sv.(Doc)
		if !ok {
			continue
		}
		var probe any
		if i < len(probes) {
			probe = probes[i]
		}
		streams, ok := pyGet(s, "streams").([]any)
		if !ok {
			both("its record's streams can't be read")
			streams = nil
		}
		for _, x := range streams {
			d, ok := x.(Doc)
			if ok && pyIn(pyGet(d, "type"), []string{"subtitle", "data"}) && (truthy(pyGet(d, "closedCaptions")) ||
				slices.Contains(CaptionStreamCodecs, pyLower(pyOrText(pyGet(d, "codec"), "")))) {
				set("closedCaptions", "a stream of its record is closed captions of its own")
			}
		}
		picture := contractPictureOf(streams)
		if pyIn(pyGet(picture, "codec"), ScannedCodecs) && pyGet(picture, "captionSignature") != false {
			set("closedCaptions", "its picture is "+pyStr(pyGet(picture, "codec"))+", whose packets were not read whole "+
				"for a caption signature")
		}
		if pyGet(picture, "captionSignature") == true || contractWindowsShow(pyGet(picture, "captionWindows")) {
			set("closedCaptions", "its record's picture carries closed captions")
		}
		off := contractRecordOffList(s)
		if len(off) > 0 {
			both("off the closed lists: " + strings.Join(off, ", "))
		}
		if contractProbeShape(probe) == "" {
			p := probe.(Doc)
			film := contractFilmOf(p)
			switch {
			case film == nil:
				both("its probe gives no one film: none, or two streams that could each be it")
			case picture == nil || !pyEqual(pyGet(picture, "index"), pyGet(film, "index")):
				both("its record's picture is not its probe's film")
			case pyIn(pyGet(film, "codec_name"), ScannedCodecs) && pyGet(picture, "captionSignature") != false:
				set("closedCaptions", "its probe's film is "+pyStr(pyGet(film, "codec_name"))+", whose packets were not "+
					"read whole for a caption signature")
			}
		}
		essence := pyDict(pyGet(s, "essence"))
		plan := contractPlanOf(probe, pyGet(pyDict(pyGet(s, "container")), "durationMs"))
		if pyGet(essence, "closedCaptionsKnown") == true && !contractCaptionsKnown(streams, off, plan) {
			set("closedCaptions", "its essence says it knows its closed captions, which its record's streams don't "+
				"bear out")
		}
	}
	for _, p := range probes {
		if why := contractProbeShape(p); why != "" {
			both(why)
			continue
		}
		pd := p.(Doc)
		for _, x := range pyGet(pd, "streams").([]any) {
			if contractCaptionStream(x.(Doc)) {
				set("closedCaptions", "a stream of its probe is closed captions of its own")
			}
		}
		if off := contractOffList(pd); len(off) > 0 {
			both("off the closed lists: " + strings.Join(off, ", "))
		}
	}
	for _, r := range reads {
		d, ok := r.(Doc)
		if _, isList := pyGet(d, "windows").([]any); !ok || !isList {
			both("the read its deletion carries can't be read")
		}
	}
	return out
}

// contractEssenceAsRead is the record logic's essence_as_read: the essence of
// a source as the reads tell it over the plan's span.
func contractEssenceAsRead(essence Doc, reads []any, plan *contractPlan) Doc {
	if len(reads) == 0 {
		return essence
	}
	out := append(Doc(nil), essence...)
	var windows []any
	for _, r := range reads {
		d, ok := r.(Doc)
		if !ok {
			windows = append(windows, nil)
			continue
		}
		windows = append(windows, pyGet(d, "windows"))
	}
	shows, all := false, true
	for _, w := range windows {
		shows = shows || contractWindowsShow(w)
	}
	for _, r := range reads {
		all = all && contractTellsNone(r, plan)
	}
	switch {
	case shows:
		out = out.With("closedCaptions", true).With("closedCaptionsKnown", true)
	case all:
		out = out.With("closedCaptions", pyGet(essence, "closedCaptions") != false).With("closedCaptionsKnown", true)
	}
	for _, w := range windows {
		if contractCountsFields(w) {
			out = out.With("interlaced", true)
			break
		}
	}
	return out
}

// pyHashable reports whether Python can put the value v in a set.
func pyHashable(v any) bool {
	switch v.(type) {
	case []any, Doc:
		return false
	}
	return true
}

// pyOrders reports whether Python orders a against b (a < b raises
// otherwise): two numbers, or two texts; anything else is taken for a raise
// (two lists Python may order; holding is the safe side).
func pyOrders(a, b any) bool {
	_, okA := pyExact(a)
	_, okB := pyExact(b)
	if okA && okB {
		return true
	}
	_, sa := a.(string)
	_, sb := b.(string)
	return sa && sb
}

// contractDeletionGate is the record logic's deletion_gate of the never keys:
// which of closedCaptions and interlaced deleting the version's originals
// would give up, the essences of its sources as their reads tell them, its
// package's essence and what gate_holds keeps them for; a raise where the
// record logic's raises (a count or a language list of values it can't
// compare or hash).
func contractDeletionGate(sources []Doc, pkgEssence any, held map[string]string) map[string]string {
	out := map[string]string{}
	if len(sources) == 0 {
		out["closedCaptions"], out["interlaced"] = "no source", "no source"
	}
	pkg := pyOr(pkgEssence, "its package's essence")
	for _, s := range sources {
		if pyGet(s, "closedCaptions") != false || pyGet(s, "closedCaptionsKnown") != true {
			out["closedCaptions"] = "its essence doesn't say it knows it carries no closed captions"
		}
		if pyGet(s, "interlaced") != false {
			out["interlaced"] = "its essence doesn't say its picture is no fields"
		}
	}
	for _, key := range lossCounts {
		var had []any
		for _, s := range sources {
			if v := pyGet(s, key); v != nil {
				had = append(had, v)
			}
		}
		for i := 1; i < len(had); i++ {
			if !pyOrders(had[i], had[0]) {
				pyRaise("its %s can't be compared", key)
			}
		}
		if kept := pyGet(pkg, key); len(had) > 0 && kept != nil {
			for _, h := range had {
				if !pyOrders(kept, h) {
					pyRaise("its package's %s can't be compared with its sources'", key)
				}
			}
		}
	}
	for _, key := range lossLanguages {
		for _, x := range pyIter(pyGet(pkg, key), "its package's "+key) {
			if !pyHashable(x) {
				pyRaise("its package's %s holds a value that can't be hashed", key)
			}
		}
		for _, s := range sources {
			for _, x := range pyIter(pyGet(s, key), "its "+key) {
				if !pyHashable(x) {
					pyRaise("its %s holds a value that can't be hashed", key)
				}
			}
		}
	}
	for k, why := range held {
		if slices.Contains(contractNever, k) {
			out[k] = why
		}
	}
	return out
}

// contractRead is the read of a report ({windows, span, gop}) as the record
// logic takes a deletion's captionsRead: of the report the analyzer sent,
// loaded as the schemas' loaders load a record (a key given twice, NaN,
// Infinity or 1e999 can't be read: nil), its captionWindows, and its span
// and gop where it gives them.
func contractRead(report []byte) any {
	if report == nil {
		return nil
	}
	orig, err := DecodeDoc(report)
	if _, twice := repeatedKey(orig); err != nil || twice || unheldNumber(orig) != "" {
		return nil
	}
	r := Doc{{"windows", pyGet(orig, "captionWindows")}}
	for _, k := range []string{"span", "gop"} {
		if v, has := orig.Get(k); has {
			r = append(r, Field{k, v})
		}
	}
	return r
}

// RecordLogicRead is the read of the report as the record logic takes a
// deletion's captionsRead (contractRead), for RecordLogicHolds: nil of one
// that can't be read.
func RecordLogicRead(report []byte) any { return contractRead(report) }

// RecordLogicHolds is what the contract's deletion gate keeps the original
// of the source record src (nil: one that can't be read) for, of its stored
// probe probe (nil: none, or one that can't be read), the package records of
// every version of its title (packages, nil of one that can't be read), the
// essence of the asked version's package (pkgEssence) and the reads of it
// (reads: each a read as contractRead gives it, nil of one that can't be
// read): of closedCaptions and interlaced, why each is kept, as the
// schemas' tools say it of the same records; a raise of the record logic's
// keeps both.
func RecordLogicHolds(src any, probe any, packages []any, pkgEssence any, reads []any) (held map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			x, ok := r.(raised)
			if !ok {
				panic(r)
			}
			why := "the record logic can't read its records: " + x.why
			held = map[string]string{"closedCaptions": why, "interlaced": why}
		}
	}()
	var essences []Doc
	if s, ok := src.(Doc); ok {
		plan := contractPlanOf(probe, pyGet(pyDict(pyGet(s, "container")), "durationMs"))
		if e, ok := pyGet(s, "essence").(Doc); ok {
			essences = append(essences, contractEssenceAsRead(e, reads, plan))
		}
	}
	gate := contractGateHolds([]any{src}, packages, []any{probe}, reads)
	if len(essences) == 0 {
		for _, k := range contractNever {
			if _, has := gate[k]; !has {
				gate[k] = "a source record of it can't be read"
			}
		}
	}
	return contractDeletionGate(essences, pkgEssence, gate)
}

// CaptionsReadOf is the read r of an original's frames, the one the gate
// decided on, as an original-deleted event carries it (captionsRead, the
// contract's): its windows as the record logic keeps them (window_of), the
// span and the gop it reported where it gives them as numbers, the release
// of the ffprobe that read it, and when it was read; nil of none, of one
// whose report the catalog doesn't keep, or one with no windows or no
// moment.
func CaptionsReadOf(r *CaptionsRead) Doc {
	if r == nil || r.ReadAt == nil {
		return nil
	}
	read, ok := contractRead(r.Report).(Doc)
	if !ok {
		return nil
	}
	list, ok := pyGet(read, "windows").([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	ws := make([]any, 0, len(list))
	for _, w := range list {
		o, _ := contractWindow(w)
		ws = append(ws, o.doc)
	}
	out := Doc{{"windows", ws}}
	if sp, ok := pyGet(read, "span").(Doc); ok {
		start, okS := contractGateNumber(pyGet(sp, "start"))
		end, okE := contractGateNumber(pyGet(sp, "end"))
		if okS && okE {
			out = append(out, Field{"span", Doc{{"start", start}, {"end", end}}})
		}
	}
	if g, ok := contractGateNumber(pyGet(read, "gop")); ok && g >= 0 {
		out = append(out, Field{"gop", g})
	}
	if v := strings.TrimSpace(r.FFprobe); v != "" {
		out = append(out, Field{"ffprobe", v})
	}
	return append(out, Field{"at", Timestamp(*r.ReadAt)})
}

// RecordLogicKeeps begins why the gate keeps an original the contract's
// deletion gate keeps (RecordLogicHolds).
const RecordLogicKeeps = "the contract's deletion gate keeps it"
