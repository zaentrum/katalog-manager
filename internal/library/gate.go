package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/languages"
)

// What an original carries that a package can fail to carry, in the terms of
// the essence both a source record and a package record hold, as the
// validator's deletion gate names them. Chapters are not here (a version
// keeps the marks, not the file), nor is interlacing (a deinterlaced picture
// is no poorer one).
var (
	lossFlags = []string{"surround", "losslessAudio", "objectAudio", "hdr10Metadata", "dolbyVision", "stereo3d",
		"imageSubtitles", "styledSubtitles", "fonts", "closedCaptions"}
	lossCounts = []string{"maxAudioChannels", "maxVideoHeight", "videoBitDepth", "subtitleTracks",
		"commentaryTracks", "commentarySubtitles", "audioDescriptionTracks"}
	lossLanguages = []string{"audioLanguages", "subtitleLanguages", "sdhSubtitleLanguages", "forcedSubtitleLanguages"}
)

// gateLanguage is a language of a language list as the gate names it.
var gateLanguage = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// IsGateName reports whether n is a name the deletion gate gives: a flag or a
// count it compares (the validator's, or GateFlags), or a language list's
// name and one of its languages after a colon ("subtitleLanguages:de").
func IsGateName(n string) bool {
	if list, lang, ok := strings.Cut(n, ":"); ok {
		if list == trackNames["audio"] || list == trackNames["subtitle"] {
			// a stream's index, or a language a track of which is lost
			return gateLanguage.MatchString(lang)
		}
		return slices.Contains(lossLanguages, list) && gateLanguage.MatchString(lang)
	}
	return slices.Contains(lossFlags, n) || slices.Contains(lossCounts, n) || slices.Contains(GateFlags, n) || n == "hdr"
}

// DeletionGate is what deleting a version's originals costs: the essence of
// its sources minus the essence of its package, each property the package is
// poorer in or lacks, and each language of a language list it lacks
// ("subtitleLanguages:de"), sorted. It is the port of the validator's
// deletion_gate (validate-library-v2.py), and reads its values as Python
// does: a flag is lost when a source has it and the package has not, a count
// when the package's is missing or below the most of the sources'.
func DeletionGate(sources []Doc, pkg Doc) []string {
	lost := map[string]bool{}
	for _, key := range lossFlags {
		had := false
		for _, s := range sources {
			if v, _ := s.Get(key); truthy(v) {
				had = true
			}
		}
		if v, _ := pkg.Get(key); had && !truthy(v) {
			lost[key] = true
		}
	}
	for _, key := range lossCounts {
		var had []float64
		for _, s := range sources {
			if v, ok := s.Get(key); ok && v != nil {
				if n, ok := toFloat(v); ok {
					had = append(had, n)
				}
			}
		}
		if len(had) == 0 {
			continue
		}
		most := had[0]
		for _, n := range had[1:] {
			most = max(most, n)
		}
		v, _ := pkg.Get(key)
		kept, ok := toFloat(v)
		if v == nil || !ok || kept < most {
			lost[key] = true
		}
	}
	for _, key := range lossLanguages {
		v, _ := pkg.Get(key)
		kept := map[string]bool{}
		for _, l := range stringsOf(v) {
			kept[l] = true
		}
		for _, s := range sources {
			v, _ := s.Get(key)
			for _, l := range stringsOf(v) {
				if !kept[l] {
					lost[key+":"+l] = true
				}
			}
		}
	}
	out := make([]string, 0, len(lost))
	for k := range lost {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// truthy is whether Python takes v for true: true, a number but 0, a string
// or a list or an object that is not empty.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case Doc:
		return len(x) > 0
	}
	n, ok := toFloat(v)
	return ok && n != 0
}

// toFloat is v as a number: a JSON number, an integer or a float; a boolean
// is 1 or 0, as Python counts it.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// stringsOf are the strings of a JSON list.
func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Essence is the essence a record holds (source.json, package.json,
// extra.json): {} when it holds none.
func Essence(path string) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ed, err := EssenceOf(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ed, nil
}

// EssenceOf is the essence of the record b, a record read already: {} when
// it holds none.
func EssenceOf(b []byte) (Doc, error) {
	d, err := DecodeDoc(b)
	if err != nil {
		return nil, err
	}
	return essenceOf(d), nil
}

// GateOf is the deletion gate of the version versionID of the item folder
// itemDir for the sources sourceIDs, from their records:
// sources/<id>/source.json and versions/<id>/package.json. Where the records
// can't tell what the package carries of the sources' subtitles
// (SubtitlesUnknown) it is a *GateUnknown, which no retirement gets past: an
// original is not let go on what its records leave unknown.
func GateOf(itemDir, versionID string, sourceIDs ...string) ([]string, error) {
	g, err := ReadGate(itemDir, versionID, sourceIDs...)
	if err != nil {
		return nil, err
	}
	if g.Unknown != "" {
		return nil, &GateUnknown{Why: g.Unknown}
	}
	return g.Lost, nil
}

// Gate is the deletion gate of a version for its sources, as their records
// tell it (ReadGate).
type Gate struct {
	// Lost is what the package doesn't carry of the sources: the
	// validator's gate of their essences (DeletionGate), what the library
	// compares beside it (GateFlags; hdr: an HDR picture packaged SDR), and,
	// of the version's own original, its tracks one by one (Tracks).
	Lost []string
	// Unknown says why the records can't tell what the package carries of
	// the sources, "" when they can: of their subtitles (SubtitlesUnknown),
	// of a stream no track says it was made from (Tracks), of the tracks of
	// a package that don't say their streams counted (TrackCounts), of the
	// originals of a version made of several.
	Unknown string
	// Scan says why an original in fields is kept (inFields), "" unless it
	// is: what marks it, or found it to hold them.
	Scan string
	// Captions says, of an original that carries closed captions
	// (captionsRule), where (its windows that show them, or its record), ""
	// unless it does.
	Captions string
	// Labels say of each track whose label is no label of its stream's
	// language both labels (Tracks), that an admin sees what to correct, and
	// of a language the package has fewer tracks of how many each has
	// (TrackCounts).
	Labels []string
	// captionsUnknown is why the captions rule can't tell yet, "" when it
	// can (OnlyCaptionsUnknown).
	captionsUnknown string
	sources         []Doc // the sources' records
	pkg             Doc   // the package's record
}

// Dropped are the names of g.Lost the master spec drops by design
// (SpecDropped), as the package keeps what it must for them (SpecDrops).
func (g Gate) Dropped() map[string]bool { return specDrops(SpecDropped, g.Lost, g.sources, g.pkg) }

// Beyond is what of g.Lost the master spec doesn't drop by design, in its
// order: anything it names keeps the original until the title is packaged
// again (ADR-0015, decision 2).
func (g Gate) Beyond() []string { return g.beyondOf(SpecDropped) }

// beyondOf is what of g.Lost the list of what is dropped by design drops not.
func (g Gate) beyondOf(list []string) []string {
	dropped := specDrops(list, g.Lost, g.sources, g.pkg)
	var out []string
	for _, n := range g.Lost {
		if !dropped[n] {
			out = append(out, n)
		}
	}
	return out
}

// ReadGate is the deletion gate of the version versionID of the item folder
// itemDir for the sources sourceIDs, read from their records.
func ReadGate(itemDir, versionID string, sourceIDs ...string) (Gate, error) {
	return ReadGateWith(itemDir, versionID, GateFacts{CaptionsNotAsked: true}, sourceIDs...)
}

// GateFacts are what the catalog knows of an original that its record
// doesn't say (FactsOf): the admins' corrections of its track languages, and
// what a read of its frames (the captions backfill's) found of its closed
// captions, window by window. A retirement reads them; ReadGate, which reads
// the records alone, holds no original to the captions rule and is for what
// the gate names, not whether it lets go (CaptionsNotAsked): the zero facts
// hold every original to it, a read missing keeping it.
type GateFacts struct {
	Corrections Corrections
	// Captions is what a read that decoded the original's frames found of
	// its closed captions; nil when none did, CaptionsFailed then saying why
	// the last read was not taken ("" when there was none). The captions
	// rule holds (captionsRule) unless CaptionsNotAsked says the gate is
	// asked what the records name alone (ReadGate), never whether an
	// original goes: a caller that sets nothing holds it.
	Captions         *CaptionsRead
	CaptionsFailed   string
	CaptionsNotAsked bool
	// CaptionsFinds is what the last read, which failed, found of the file
	// before it stopped (its streams of captions of their own, what of it
	// is off the closed lists), nil when nothing.
	CaptionsFinds *CaptionsRead
	// Master says the version asked of is the title's master made of
	// another original (a superseded original's gate), not one the catalog
	// says is made of the original: its package's picture, scan and record
	// are that original's.
	Master bool
}

// offListSaid is what the reads of f say is off the closed lists.
func (f GateFacts) offListSaid() []string {
	var out []string
	for _, r := range []*CaptionsRead{f.Captions, f.CaptionsFinds} {
		if r != nil {
			out = append(out, r.OffList...)
		}
	}
	return out
}

// FactsOf are the facts the catalog knows of the original s.
func FactsOf(ctx context.Context, q Querier, s *Source) (GateFacts, error) {
	corr, err := TrackCorrections(ctx, q, s)
	if err != nil {
		return GateFacts{}, err
	}
	captions, failed, finds, err := SourceCaptions(ctx, q, s.ID)
	return GateFacts{Corrections: corr, Captions: captions, CaptionsFailed: failed, CaptionsFinds: finds}, err
}

// ReadGateWith is ReadGate with the facts f of the source the catalog knows:
// the admins' corrections of its track languages (TrackCorrections), and its
// closed captions (captionsRule): an original that carries them is kept for
// good whatever its package carries, closedCaptions named and where said
// (Captions); one whose captions can't be told yet is kept until a read
// tells (Unknown).
func ReadGateWith(itemDir, versionID string, f GateFacts, sourceIDs ...string) (Gate, error) {
	corr := f.Corrections
	pkg, err := readRecordDoc(filepath.Join(VersionDir(itemDir, versionID), PackageFile))
	if err != nil {
		return Gate{}, err
	}
	if len(sourceIDs) == 0 {
		// no original to tell of: nothing it lets go
		return Gate{Unknown: "the gate is asked of no original", pkg: pkg}, nil
	}
	var records, essences, probes []Doc
	var unreadable []string
	recordUnread, recordGap := "", ""
	for _, sid := range sourceIDs {
		rec, err := readRecordDoc(filepath.Join(SourceDir(itemDir, sid), "source.json"))
		if err != nil {
			return Gate{}, err
		}
		if why := recordShape(rec); why != "" && recordUnread == "" {
			recordUnread = "its source record can't be read: " + why
		}
		records = append(records, rec)
		essences = append(essences, essenceOf(rec))
		// its stored probe, where there is one: one that can't be read, or
		// is no probe of a file (no object, no streams), keeps it, as what it
		// says can't be told
		probe, err := readRecordDoc(filepath.Join(SourceDir(itemDir, sid), ProbeFile))
		if err == nil {
			if why := probeShape(probe); why != "" {
				err = errors.New(why)
			} else if list, _ := listOf(probe, "streams"); len(probeFilms(list)) == 0 {
				// a probe that gives no film (film_of) gives no plan and no
				// field order of one, whatever the policy for an absent field
				// order says; one whose record lists a picture is of another
				// file, or cut short
				if len(filmsOf(rec, false, nil)) > 0 {
					err = errors.New("it lists no picture, and its record lists one")
				} else {
					err = errors.New("it lists no picture (a video stream that is no cover): it gives no film")
				}
			}
		}
		switch {
		case err == nil:
			probes = append(probes, probe)
		case !errors.Is(err, os.ErrNotExist):
			unreadable = append(unreadable, "its stored probe can't be read: "+err.Error())
			probes = append(probes, nil)
		default:
			probes = append(probes, nil)
		}
	}
	for i, rec := range records {
		if recordShape(rec) != "" {
			continue
		}
		if why := recordGaps(rec, probes[i]); why != "" && recordGap == "" {
			recordGap = "its source record doesn't tell what the gate compares: " + why
		}
	}
	if why := packageFlags(essenceOf(pkg)); why != "" && recordGap == "" {
		recordGap = why
	}
	lost := DeletionGate(essences, essenceOf(pkg))
	// the version the catalog says is made of the original: its record says
	// so too, and its package's own scan and picture are the original's
	var ownPkg Doc
	versionUnread, copied := "", ""
	if !f.Master {
		ownPkg = pkg
		listed, ok := versionSourceList(itemDir, versionID)
		for _, sid := range sourceIDs {
			if !ok || !slices.Contains(listed, sid) {
				versionUnread = "the record of its version " + versionID + " (version.json) doesn't name it among the " +
					"originals the version was made of (a list of sourceIds), as the catalog does"
				break
			}
		}
		copied = copyHolds(pkg)
	}
	scan := ""
	fields, framesUnknown := inFields(itemDir, versionID, ownPkg, sourceIDs, records, probes, f.Captions)
	if len(unreadable) > 0 && fields == "" {
		fields = unreadable[0]
	}
	if fields != "" {
		lost = append(lost, "interlaced")
		scan = FieldsKept + ": " + fields
	}
	if hdrLost(records, pkg) {
		lost = append(lost, "hdr")
	}
	sort.Strings(lost)
	unknown := SubtitlesUnknown(records, pkg, VersionDir(itemDir, versionID))
	// track by track, of the version's own original: a package track says
	// the stream it was made from, not of which original
	made := versionSources(itemDir, versionID)
	own := len(sourceIDs) > 0
	for _, sid := range sourceIDs {
		own = own && slices.Contains(made, sid)
	}
	var labels []string
	switch {
	case own && len(made) == 1:
		var tracks string
		lost, tracks, labels = Tracks(lost, records[0], pkg, corr)
		if unknown == "" {
			unknown = tracks
		}
	case own && unknown == "" && slices.ContainsFunc(records, hasTracks):
		unknown = fmt.Sprintf("its version is made of %d originals, and its package's tracks don't say which original "+
			"they were made from: what it carries of each original's tracks can't be told", len(made))
	case !own && len(records) == 1:
		// a master of another original: its track indexes are that file's,
		// so their languages are counted; a path names a stream of the
		// master's own original
		var pkgSrc Doc
		if len(made) == 1 {
			pkgSrc, _ = readRecordDoc(filepath.Join(SourceDir(itemDir, made[0]), "source.json"))
		}
		var counts string
		lost, counts, labels = TrackCounts(lost, records[0], pkg, pkgSrc, nil)
		if unknown == "" {
			unknown = counts
		}
	}
	// what no read lets go of comes before the captions rule's why: what the
	// record doesn't tell, a version record that doesn't name it, the copy
	// rule
	other := ""
	for _, why := range []string{recordGap, versionUnread, copied} {
		if why != "" && other == "" {
			other = why
		}
	}
	if other != "" && unknown == "" {
		unknown = other
	}
	captions, captionsUnknown := "", ""
	if !f.CaptionsNotAsked {
		lost = slices.DeleteFunc(lost, func(n string) bool { return n == "closedCaptions" })
		captions, captionsUnknown = captionsRule(records, probes, f)
		if captionsUnknown == "" && framesUnknown != "" {
			captionsUnknown = framesUnknown
		}
		switch {
		case captions != "":
			lost = append(lost, "closedCaptions")
			sort.Strings(lost)
		case captionsUnknown != "" && unknown == "":
			unknown = captionsUnknown
		}
	}
	if captions != "" && other != "" && unknown == other {
		// carried: kept for good, which its why says
		unknown = ""
	}
	if recordUnread != "" && captions == "" {
		unknown = recordUnread
	}
	// the closing rule: what of the file the reads were not made for leaves
	// its captions and its fields not known, whether the captions are read
	// or not
	if off := offListOf(records, probes, f.offListSaid()); len(off) > 0 && unknown == "" && captions == "" {
		unknown = offListWhy(off)
	}
	return Gate{Lost: lost, Unknown: unknown, Labels: labels, Scan: scan, Captions: captions,
		captionsUnknown: captionsUnknown, sources: records, pkg: pkg}, nil
}

// OnlyCaptionsUnknown reports whether all that keeps the original of g is
// that a read of its frames hasn't told its closed captions (or its fields)
// yet: the captions rule's unknown, of those a read lets go of
// (readReleases), and nothing else unknown nor lost beyond what the spec
// drops. A read of them may let it go; what the closing rule leaves not
// known (closedNotKnown) no read does, nor a finding of a length that is
// not its record's (CaptionsLengthMismatch), final for its file and record,
// nor records that give no length or no plan of its picture.
func (g Gate) OnlyCaptionsUnknown() bool {
	return g.captionsUnknown != "" && g.Unknown == g.captionsUnknown && len(g.Beyond()) == 0 &&
		!strings.HasPrefix(g.Unknown, CaptionsLengthMismatch) && !strings.HasPrefix(g.Unknown, CaptionsNoLength) &&
		!strings.HasPrefix(g.Unknown, CaptionsNoPlan) &&
		!strings.HasPrefix(g.Unknown, CaptionsUnknown+": "+PictureNotFilm) &&
		!strings.HasPrefix(g.Unknown, CaptionsUnscanned) &&
		slices.ContainsFunc(readReleases, func(p string) bool { return strings.HasPrefix(g.Unknown, p) })
}

// CopyUnproven begins why the gate keeps an original whose package copies
// its picture (its top video rendition's encoder copy): what a copy carries
// of it is the original's own stream, and the catalog keeps no record yet
// that proves a copy carries it whole, so the copy rule holds it.
const CopyUnproven = "its package's picture is its original's own stream, copied"

// copyHolds says why the package record pkg holds its original by the copy
// rule: its top video rendition's encoder is copy (CopyUnproven), or it says
// none (no rendition, no encoder, one that is no word) or a word that is no
// encoder the transcoder writes (TranscoderEncoders), so whether it copies
// can't be told; "" when it states one of the transcoder's encoders.
func copyHolds(pkg Doc) string {
	v0 := topVideo(pkg)
	if v0 == nil {
		return "its package lists no video rendition, so whether it copies its original's picture can't be told"
	}
	e, _ := v0.Get("encoder")
	w, ok := e.(string)
	switch {
	case !ok || strings.TrimSpace(w) == "":
		return "its package's top video rendition states no encoder, so whether it copies its original's picture " +
			"can't be told"
	case strings.TrimSpace(w) == "copy":
		return CopyUnproven + " (encoder copy), and no record that proves the copy is read yet"
	case !slices.Contains(TranscoderEncoders, w):
		return fmt.Sprintf("its package's top video rendition says encoder %q, no word the transcoder writes of an "+
			"encode (%s) nor copy: whether it copies its original's picture can't be told", w,
			strings.Join(TranscoderEncoders, ", "))
	}
	return ""
}

// TranscoderEncoders are the words the transcoder writes of a rendition it
// encoded (and copy of one it copied): an encoder word of no other kind, or
// one the gate doesn't know, holds the original as one whose copy can't be
// told.
//
// They are the transcoder's own, as its RungPlan.encoder says them at
// src/transcoder/decision.py:825 of zaentrum/transcoder at cf2c15f
// (Encoders and CPU_ENCODERS just above it name the four). A word the
// transcoder adds or renames holds its originals here until this list
// takes it; TestTheTranscodersEncoderWordsAreTheGates pins the list, so a
// change of it is made on purpose, against that file.
var TranscoderEncoders = []string{"hevc_nvenc", "h264_nvenc", "libx265", "libx264"}

// versionSourceList is what the record of the version versionID of the item
// folder itemDir says it was made of (version.json's sourceIds), and whether
// it says it: a record that can be read whose sourceIds is a list of
// strings.
func versionSourceList(itemDir, versionID string) ([]string, bool) {
	d, err := readRecordDoc(filepath.Join(VersionDir(itemDir, versionID), VersionFile))
	if err != nil {
		return nil, false
	}
	return sourceIDList(d)
}

// sourceIDList is the version record ver's sourceIds, and whether it is a
// list of strings.
func sourceIDList(ver Doc) ([]string, bool) {
	v, _ := ver.Get("sourceIds")
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// topVideo is the package record pkg's first video rendition, nil when it
// lists none.
func topVideo(pkg Doc) Doc {
	ren, _ := pkg.Get("renditions")
	rd, _ := ren.(Doc)
	video, _ := listOf(rd, "video")
	if len(video) == 0 {
		return nil
	}
	v0, _ := video[0].(Doc)
	return v0
}

// GateFlags are the names the library's gate gives beside the validator's
// (DeletionGate) of a flag: interlaced, which the validator leaves out (a
// deinterlaced picture is no poorer one) and the master spec's list doesn't
// name (ADR-0015, decision 2), so that it keeps the original: an original
// in fields is not retired for now (inFields). Naming interlaced in
// SpecDropped would let every one go.
var GateFlags = []string{"interlaced"}

// FieldsKept begins why the gate keeps an original in fields: whatever was
// done to make its package progressive, it is not retired for now.
const FieldsKept = "an original in fields is not retired for now"

// NothingFound is the one word of a package's scan.found that says its
// original was found to hold no fields: progressive. Every other word it
// may say (fields, film, telecine, mixed, field-pictures, unknown, or one
// the gate doesn't know) counts as fields found.
const NothingFound = "progressive"

// ProbeFile is a source's stored probe beside its record: ffprobe's own
// JSON of the file, sources/<id>/ffprobe.json.
const ProbeFile = "ffprobe.json"

// FieldOrderGroup is a group of originals the policy for an absent field
// order (AbsentFieldOrder) decides alike: by the codec of their picture
// stream and their container (container.format), as their record says them
// ("" when it doesn't).
type FieldOrderGroup struct{ Codec, Container string }

// AbsentFieldOrder says whether an original of the group g whose record and
// stored probe state no field order for its picture counts as in fields. For
// now every group does: an absent field order keeps the original, as an
// unknown one does, whatever a package's scan says (no word of the
// transcoder's releases an original: its record is informational). What may
// let such originals go later is an examination of the whole original
// itself, or a proof that its package copies it, which the catalog doesn't
// read yet; a later decision lets a group go the ordinary way here, and
// nowhere else. Nothing in it reads a scan.
var AbsentFieldOrder = func(g FieldOrderGroup) bool { return true }

// absentFieldOrderWhy is why an original whose field order no record states
// is kept (no examination of the original itself is read yet).
const absentFieldOrderWhy = "its field order is not stated and it was never examined"

// inFields says why the original of the sources sourceIDs of the item folder
// itemDir (their records records, their stored probes probes, nil where
// there is none) is in fields, in the widest reading, "" when it is not; and
// why its fields can't be told yet of the read of its frames r (unknown).
// Its picture is its first video stream that is no attached picture; a
// record or a probe that has more than one such keeps it, as which is the
// picture can't be told. In fields: a record says interlaced, or its
// picture a field order other than progressive (tt, bb, tb, bt, unknown,
// ...); its stored probe gives the picture such a field_order; the scan of a
// package version made of it (version.json's sourceIds) found anything but
// NothingFound, or a package or version record of it can't be read; a read
// of its frames found interlaced or repeated frames in a window; or neither
// its record nor its probe states a field order, where the policy for its
// group says so (AbsentFieldOrder: for now, every group), whatever a
// package's scan says. A read that says nothing of interlaced or repeated
// frames (an older one) leaves its fields not known. No word of a scan lets
// one go, neither what it says was done (scan.done) nor that it found
// NothingFound: a package called whole has been found not to be.
func inFields(itemDir, versionID string, ownPkg Doc, sourceIDs []string, records, probes []Doc,
	r *CaptionsRead) (fields, unknown string) {
	for i, rec := range records {
		switch v, has := essenceOf(rec).Get("interlaced"); {
		case !has || v == nil || v == false:
		case v == true:
			return "its record says interlaced", ""
		default:
			return "its record's interlaced is no boolean", ""
		}
		stated := false
		var probe Doc
		if i < len(probes) {
			probe = probes[i]
		}
		pictures := filmsOf(rec, false, probe)
		if len(pictures) > 1 {
			return fmt.Sprintf("its record lists %d picture streams, so which is its picture can't be told", len(pictures)), ""
		}
		if len(pictures) == 1 {
			if o := strings.ToLower(strings.TrimSpace(str(pictures[0], "fieldOrder"))); o != "" {
				stated = true
				if o != "progressive" {
					return "its record's field order is " + o, ""
				}
			}
		}
		if why := pictureNotFilm(rec, probe); why != "" {
			return why, ""
		}
		if i < len(probes) && probes[i] != nil {
			pp := filmsOf(probes[i], true, nil)
			if len(pp) > 1 {
				return fmt.Sprintf("its probe lists %d picture streams, so which is its picture can't be told", len(pp)), ""
			}
			if len(pp) == 1 {
				if o := strings.ToLower(strings.TrimSpace(str(pp[0], "field_order"))); o != "" {
					stated = true
					if o != "progressive" {
						return "its probe's field order is " + o, ""
					}
				}
			}
		}
		if ownPkg != nil {
			// the asked version's own package, read whatever its
			// version.json says
			if why := scanHolds(ownPkg); why != "" {
				return "the scan of its package of version " + versionID + " " + why, ""
			}
		}
		if i < len(sourceIDs) {
			if why := scansOf(itemDir, sourceIDs[i]); why != "" {
				return why, ""
			}
		}
		if !stated {
			g := FieldOrderGroup{}
			if c, _ := rec.Get("container"); c != nil {
				if cd, ok := c.(Doc); ok {
					g.Container = strings.ToLower(strings.TrimSpace(str(cd, "format")))
				}
			}
			if len(pictures) > 0 {
				g.Codec = strings.ToLower(strings.TrimSpace(str(pictures[0], "codec")))
			}
			if AbsentFieldOrder(g) {
				return absentFieldOrderWhy, ""
			}
		}
	}
	if r != nil {
		var interlaced, repeated, silent, unread []float64
		for _, w := range r.Windows {
			// what a window counted in fields counts, whatever else of it
			// failed (the worst a read says is kept)
			switch {
			case w.InterlacedFrames != nil && *w.InterlacedFrames > 0:
				interlaced = append(interlaced, w.At)
			case w.RepeatFrames != nil && *w.RepeatFrames > 0:
				repeated = append(repeated, w.At)
			case w.CountsUnreadable:
				unread = append(unread, w.At)
			case w.Frames <= 0:
			case w.InterlacedFrames == nil || w.RepeatFrames == nil:
				silent = append(silent, w.At)
			}
		}
		switch {
		case len(interlaced) > 0:
			return "a read of its frames found interlaced ones at " + percents(interlaced), ""
		case len(repeated) > 0:
			return "a read of its frames found ones that repeat a field at " + percents(repeated), ""
		case len(unread) > 0:
			return "a read of its frames gave a count of interlaced or repeated frames that can't be read at " +
				percents(unread), ""
		case len(silent) > 0:
			return "", FramesUnknown + ": a read of its original says nothing of interlaced or repeated frames at " +
				percents(silent) + "; the captions backfill reads it again (captions.backfill)"
		}
	}
	return "", ""
}

// scansOf is why the scans of the packages made of the source sourceID (the
// versions of the item folder itemDir whose version.json names it in
// sourceIds) keep it in fields: one found anything but NothingFound; a
// version whose record can't be read, or says no list of sourceIds, or
// whose package's record can't be read; a version without a record whose
// package's scan holds (which original it was made of can't be told); or a
// folder of versions that can't be listed. A version folder with neither
// record says nothing. "" when none does. A scan that found NothingFound
// lets nothing go.
func scansOf(itemDir, sourceID string) string {
	entries, err := os.ReadDir(filepath.Join(itemDir, "versions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "the versions of its title can't be listed: " + err.Error()
	}
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() {
			// a link, a file: what it holds can't be told from where it lies
			return "the versions of its title hold " + id + ", which is no folder of its own (a link, a file)"
		}
		dir := filepath.Join(itemDir, "versions", id)
		ver, err := readRecordDoc(filepath.Join(dir, VersionFile))
		unnamed := errors.Is(err, os.ErrNotExist)
		switch {
		case unnamed:
		case err != nil:
			return "the record of version " + id + " can't be read: " + err.Error()
		default:
			ids, ok := sourceIDList(ver)
			if !ok {
				return "the record of version " + id + " says no list of the originals it was made of (sourceIds)"
			}
			if !slices.Contains(ids, sourceID) {
				continue
			}
		}
		pkg, err := readRecordDoc(filepath.Join(dir, PackageFile))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "its package of version " + id + " can't be read: " + err.Error()
		}
		if why := scanFoundHolds(pkg); why != "" {
			if unnamed {
				return "the scan of the package of version " + id + ", whose record (version.json) is not there to say " +
					"which original it was made of, " + why
			}
			return "the scan of its package of version " + id + " " + why
		}
	}
	return ""
}

// scanFoundHolds says why the scan of the package record pkg of a version of
// the original holds it in fields - it found anything but NothingFound, or
// found NothingFound with counts of its own that say fields or can't be
// read (scanCountsHold) -, "" when it found NothingFound or says nothing
// found (no scan, a scan of null, no found): what an older version's scan
// lacks is for the version the original is retired from to state
// (scanHolds), not a hold for good.
func scanFoundHolds(pkg Doc) string {
	for _, d := range []Doc{pkg, essenceOf(pkg)} {
		v, _ := d.Get("scan")
		x, ok := v.(Doc)
		if !ok {
			continue
		}
		found, has := x.Get("found")
		if !has || found == nil {
			continue
		}
		if found == NothingFound {
			if why := scanCountsHold(x); why != "" {
				return why
			}
			continue
		}
		if f, ok := found.(string); ok {
			return "found " + f
		}
		return fmt.Sprintf("found %v, no word", found)
	}
	return ""
}

// gateCount is v where it is a whole JSON number from 0, as the record
// logic's gate_count takes a count (no boolean, no float, no text), and
// whether it is one.
func gateCount(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case json.Number:
		// a float's text ("5.0", "1e3") is no whole number's
		n, ok := new(big.Int).SetString(x.String(), 10)
		return n, ok && n.Sign() >= 0
	case int:
		return big.NewInt(int64(x)), x >= 0
	case int64:
		return big.NewInt(x), x >= 0
	}
	return nil, false
}

// scanCountsHold says why the scan scan, which found NothingFound, holds its
// title's originals in fields by its own counts, as the record logic's
// scan_holds reads them: its read-through (verified) counted chunks in
// fields (chunksInFields, 0 where it gives none), or gives a count of them
// that can't be read (gateCount); its examination (idet), where it gives
// one, is no object, gives a count of top-field, bottom-field or
// progressive pictures that can't be read (each 0 where it gives none), or
// counted more top- and bottom-field pictures together than progressive
// ones. "" when its counts don't say otherwise.
func scanCountsHold(scan Doc) string {
	if ver, _ := scan.Get("verified"); ver != nil {
		if vd, ok := ver.(Doc); ok {
			if v, has := vd.Get("chunksInFields"); has {
				if n, ok := gateCount(v); !ok || n.Sign() > 0 {
					return "found " + NothingFound + ", but its read-through counted chunks in fields (chunksInFields " +
						pyStr(v) + ")"
				}
			}
		}
	}
	idet, has := scan.Get("idet")
	if !has || idet == nil {
		return ""
	}
	id, ok := idet.(Doc)
	if !ok {
		return "found " + NothingFound + ", but its examination (idet) is no object"
	}
	counts := map[string]*big.Int{}
	for _, k := range []string{"tff", "bff", "progressive"} {
		counts[k] = new(big.Int)
		if v, has := id.Get(k); has {
			n, ok := gateCount(v)
			if !ok {
				return "found " + NothingFound + ", but its examination's " + k + " is no count (" + pyStr(v) + ")"
			}
			counts[k] = n
		}
	}
	if new(big.Int).Add(counts["tff"], counts["bff"]).Cmp(counts["progressive"]) > 0 {
		return fmt.Sprintf("found %s, but its examination counted more fields than frames (tff %s, bff %s, "+
			"progressive %s)", NothingFound, counts["tff"], counts["bff"], counts["progressive"])
	}
	return ""
}

// ScanDone are the words of a scan's done that say nothing was done to the
// picture: any other (deinterlaced, fields kept, inverse telecine, a word the
// gate doesn't know) holds the original.
var ScanDone = []string{NothingFound, "none"}

// scanHolds says why the scan of the package record pkg (scan, of the record
// or of its essence) holds the original in fields, "" when it holds it not
// or the package says no scan (one from before the scan was written): a
// scan that is no object (null among them); a found that is not exactly
// NothingFound (a word, one the gate doesn't know, blank, of another type);
// a done that is not one of ScanDone's (none, null among them); a
// verification (verified) that is not ok and complete, or none; counts of
// its own that say fields or can't be read (scanCountsHold). A scan that
// found NothingFound lets nothing go.
func scanHolds(pkg Doc) string {
	for _, d := range []Doc{pkg, essenceOf(pkg)} {
		v, has := d.Get("scan")
		if !has {
			continue
		}
		x, ok := v.(Doc)
		if !ok {
			return "is no object"
		}
		found, _ := x.Get("found")
		if f, ok := found.(string); !ok || f != NothingFound {
			switch {
			case found == nil:
				return "found nothing"
			case ok && strings.TrimSpace(f) == "":
				return "found a blank"
			case ok:
				return "found " + f
			}
			return fmt.Sprintf("found %v, no word", found)
		}
		done, _ := x.Get("done")
		if w, ok := done.(string); !ok || !slices.Contains(ScanDone, w) {
			if done == nil {
				return "says nothing of what was done to the picture (done)"
			}
			return "did " + fmt.Sprint(done)
		}
		ver, _ := x.Get("verified")
		vd, _ := ver.(Doc)
		ok1, _ := vd.Get("ok")
		complete, _ := vd.Get("complete")
		if ok1 != true || complete != true {
			return "found " + NothingFound + " with no verification that is ok and complete"
		}
		if why := scanCountsHold(x); why != "" {
			return why
		}
	}
	return ""
}

// pictureStreams are the picture streams of the record d (a probe's, by
// ffprobe's names, typeKey codec_type, or a source record's): its films,
// the video streams that are no attached picture (filmsOf).
func pictureStreams(d Doc, list, typeKey, dispositions, attached string) []Doc {
	return filmsOf(d, typeKey == "codec_type", nil)
}

// recordShape says why the source record rec is none the gate can read (no
// essence object, streams that are no list of objects), "" when it is one.
func recordShape(rec Doc) string {
	if e, has := rec.Get("essence"); !has {
		return "it has no essence"
	} else if _, ok := e.(Doc); !ok {
		return "its essence is no object"
	}
	if st, has := rec.Get("streams"); has && st != nil {
		list, ok := st.([]any)
		if !ok {
			return "its streams are no list"
		}
		for _, e := range list {
			if _, ok := e.(Doc); !ok {
				return "a stream of it is no object"
			}
		}
	}
	return ""
}

// recordGaps says what the source record rec (its stored probe probe, nil:
// none), one the gate can read (recordShape), doesn't tell that the gate
// compares, "" when it tells it all: a list of its file's streams (absent,
// null or empty: it lists none), and each fact of its essence the gate
// compares, of its type as the record logic writes it (libv2_records'
// source_essence): a flag a boolean (lossFlags), a count a whole number, 0
// or more (lossCounts), a language list a list of strings (lossLanguages).
// A fact it doesn't state, or states of another type or null, is no
// "nothing to lose". The record logic writes null for maxVideoHeight and
// videoBitDepth alone, of a picture it couldn't measure or of a file with
// no picture: null says none only of a record that lists no picture.
func recordGaps(rec, probe Doc) string {
	ed := essenceOf(rec)
	films := len(filmsOf(rec, false, probe))
	var lacks, typed []string
	for _, k := range lossFlags {
		switch v, has := ed.Get(k); {
		case !has:
			lacks = append(lacks, k)
		case v != true && v != false:
			typed = append(typed, k)
		}
	}
	for _, k := range lossCounts {
		v, has := ed.Get(k)
		switch {
		case !has:
			lacks = append(lacks, k)
		case v == nil && films == 0 && (k == "maxVideoHeight" || k == "videoBitDepth"):
		case !wholeCount(v):
			typed = append(typed, k)
		}
	}
	for _, k := range lossLanguages {
		v, has := ed.Get(k)
		list, ok := v.([]any)
		switch {
		case !has:
			lacks = append(lacks, k)
		case !ok:
			typed = append(typed, k)
		default:
			for _, e := range list {
				if s, ok := e.(string); !ok || strings.TrimSpace(s) == "" {
					typed = append(typed, k)
					break
				}
			}
		}
	}
	switch {
	case len(lacks) > 0:
		return "its essence doesn't state " + strings.Join(lacks, ", ")
	case len(typed) > 0:
		return "its essence states " + strings.Join(typed, ", ") + " as no value of its kind (a flag a boolean, a " +
			"count a whole number, a language list a list of languages; null a picture's height or bit depth of a " +
			"file with no picture alone)"
	}
	if list, _ := listOf(rec, "streams"); len(list) == 0 {
		return "it lists no streams of its file"
	}
	return ""
}

// wholeCount reports whether v is a count as a record writes one: a whole
// JSON number, 0 or more.
func wholeCount(v any) bool {
	if _, isBool := v.(bool); isBool {
		return false
	}
	n, ok := toFloat(v)
	return ok && n >= 0 && n == math.Trunc(n) && !math.IsInf(n, 0)
}

// packageFlags says why the package essence pkg can't be read for what it
// carries: a flag the gate compares that it states as no boolean (a string,
// a number, null), which would be taken for carried or not as it happens;
// "" when it states none so.
func packageFlags(pkg Doc) string {
	var typed []string
	for _, k := range lossFlags {
		if v, has := pkg.Get(k); has && v != true && v != false {
			typed = append(typed, k)
		}
	}
	if len(typed) == 0 {
		return ""
	}
	return "its package's essence states " + strings.Join(typed, ", ") + " as no boolean"
}

// probeShape says why the stored probe probe is no probe of a file (no
// streams, streams that are no list of objects, a format that is no
// object), "" when it is one.
func probeShape(probe Doc) string {
	st, has := probe.Get("streams")
	list, ok := st.([]any)
	switch {
	case !has || st == nil:
		return "it lists no streams"
	case !ok:
		return "its streams are no list"
	case len(list) == 0:
		return "it lists no streams"
	}
	for _, e := range list {
		if _, ok := e.(Doc); !ok {
			return "a stream of it is no object"
		}
	}
	if f, has := probe.Get("format"); has && f != nil {
		if _, ok := f.(Doc); !ok {
			return "its format is no object"
		}
	}
	return ""
}

// saidOr is s, or none when s is empty.
func saidOr(s, none string) string {
	if s == "" {
		return none
	}
	return s
}

// hasTracks reports whether the source record rec lists an audio or a
// subtitle stream.
func hasTracks(rec Doc) bool {
	return len(sourceStreams(rec, "audio", nil)) > 0 || len(sourceStreams(rec, "subtitle", nil)) > 0
}

// SpecDropped are what the master spec drops by design (ADR-0015, decision
// 2), in the deletion gate's names: surround and the channels above the
// package's, lossless and object-based audio, Dolby Vision where the package
// keeps the HDR, 3D. A retirement gives up these and nothing else; a setting
// may narrow them (library.superseded.accept), never widen them. It is the
// one list: a name added here is dropped wherever an original is retired.
var SpecDropped = []string{"surround", "maxAudioChannels", "losslessAudio", "objectAudio", "dolbyVision", "stereo3d"}

// SpecDrops are the names of lost, a deletion gate of the package whose
// record is pkg for the sources whose records are sources, that the master
// spec drops by design (SpecDropped), as the package keeps what it must for
// them: the channels above the package's while it keeps a stereo track (the
// spec folds surround down to stereo), Dolby Vision while it keeps the HDR
// (KeepsHDR). Every other name keeps the original.
func SpecDrops(lost []string, sources []Doc, pkg Doc) map[string]bool {
	return specDrops(SpecDropped, lost, sources, pkg)
}

// specDrops is SpecDrops of the names list drops by design.
func specDrops(list, lost []string, sources []Doc, pkg Doc) map[string]bool {
	out := map[string]bool{}
	for _, n := range lost {
		if !slices.Contains(list, n) {
			continue
		}
		switch n {
		case "maxAudioChannels":
			out[n] = keepsStereo(essenceOf(pkg))
		case "dolbyVision":
			out[n] = KeepsHDR(sources, pkg)
		default:
			out[n] = true
		}
	}
	return out
}

// The tracks a package carries one by one of its original's streams: audio
// (its stereo renditions and their 5.1 companions) and subtitles (but those
// made of a file beside the original, fromSidecar), with the gate's names of
// a stream of the kind the package has no track of ("audioTrack:2") and of
// the language lists a track counts for.
var (
	trackNames    = map[string]string{"audio": "audioTrack", "subtitle": "subtitleTrack"}
	trackLists    = map[string][]string{"audio": {"audioLanguages"}, "subtitle": {"subtitleLanguages", "sdhSubtitleLanguages", "forcedSubtitleLanguages"}}
	keptLabels    = []string{"zxx", "mul"} // a label any track may take: no linguistic content, several languages
	unknownLabels = []string{"", "und"}    // a source track's label any label may replace: the catalog learned it
)

// stream is a stream of an original, or a track of a package, as the gate
// holds them track by track: the stream it is (or was made from: from, named
// when the track says it), its place among the streams of its kind (ordinal,
// an original's), its primary language as the gate reads it (an admin's
// correction of it, else its label) and its label, what it is for.
type stream struct {
	from, ordinal               int
	named                       bool
	lang, label, purpose, codec string
	forced                      bool
}

// sdh reports whether the stream is a subtitle for the hard of hearing, and
// forcedKind whether it is a forced one (forced, or signs and songs).
func (s stream) sdh() bool { return s.purpose == "sdh" }
func (s stream) forcedKind() bool {
	return s.forced || s.purpose == "forced" || s.purpose == "signs-songs"
}

// Correction is an admin's language of a track of a title's source
// (setTrackLanguage, com_nalet_katalog_itemtracklanguages): who set it and
// when. For the gate it is the language of that stream of the title's
// source.
type Correction struct {
	Language, By string
	At           time.Time
}

// Corrections are the corrections of a title's source, by kind (audio,
// subtitle) and by the track's place among its streams of the kind (0
// first).
type Corrections map[string]map[int]Correction

// TrackCorrections are the admins' corrections of the track languages of
// the original s, when it is its title's source, the file its primary asset
// plays, which the corrections are of: none of another original, none a
// catalog without migration 037 holds, none that doesn't say who set it, and
// none set before the original was recorded (or, with no record, taken in),
// which was of the file the title had then.
func TrackCorrections(ctx context.Context, q Querier, s *Source) (Corrections, error) {
	if ok, err := relationReady(ctx, q, "com_nalet_katalog_itemtracklanguages"); err != nil || !ok {
		return nil, err
	}
	var current bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND isprimary AND COALESCE(kind, 'primary') = 'primary' AND sourceid = $2)`, s.ItemID, s.ID).
		Scan(&current); err != nil || !current {
		return nil, err
	}
	// a correction older than the original was made for the file the title
	// had before: none of this one's (the table doesn't say which file)
	rows, err := q.Query(ctx, `SELECT kind, ordinal, language, modifiedby, modifiedat FROM com_nalet_katalog_itemtracklanguages
		WHERE item_id = $1 AND COALESCE(btrim(modifiedby), '') <> ''
		  AND modifiedat >= (SELECT COALESCE(recordedat, createdat) FROM com_nalet_katalog_itemsources WHERE id = $2)`,
		s.ItemID, s.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := Corrections{}
	for rows.Next() {
		var kind string
		var ordinal int
		var c Correction
		if err := rows.Scan(&kind, &ordinal, &c.Language, &c.By, &c.At); err != nil {
			return nil, err
		}
		if out[kind] == nil {
			out[kind] = map[int]Correction{}
		}
		out[kind][ordinal] = c
	}
	return out, rows.Err()
}

// sourceStreams are the streams of kind (audio, subtitle) the source record
// rec lists, each of the language corr corrects it to where it does.
func sourceStreams(rec Doc, kind string, corr Corrections) []stream {
	list, _ := listOf(rec, "streams")
	var out []stream
	for _, e := range list {
		d, ok := e.(Doc)
		if !ok || str(d, "type") != kind {
			continue
		}
		i, named := toFloat(valueOf(d.Get("index")))
		disp, _ := d.Get("dispositions")
		dd, _ := disp.(Doc)
		forced, _ := dd.Get("forced")
		st := stream{from: int(i), ordinal: len(out), named: named, label: primaryLanguage(str(d, "language")),
			purpose: str(d, "purpose"), codec: strings.ToLower(str(d, "codec")), forced: truthy(forced)}
		st.lang = st.label
		if c, ok := corr[kind][st.ordinal]; ok {
			st.lang = primaryLanguage(c.Language)
		}
		out = append(out, st)
	}
	return out
}

// subtitleOrdinal reads the convention by which a package written before
// its tracks said their streams names a subtitle of its original's: the
// n-th subtitle stream's is subs/<n>.<ext>.
var subtitleOrdinal = regexp.MustCompile(`^subs/([0-9]+)\.[A-Za-z0-9]+$`)

// The forms of picture subtitles: of a package's subtitle by its format, of
// an original's stream by its codec. A text subtitle has none.
var (
	entryForms  = map[string]string{"pgs": "pgs", "sup": "pgs", "vobsub": "vobsub", "dvb": "dvb", "xsub": "xsub"}
	streamForms = map[string]string{"hdmv_pgs_subtitle": "pgs", "dvd_subtitle": "vobsub", "dvb_subtitle": "dvb", "xsub": "xsub"}
	// textLeftOut are the text codecs an encode to Matroska leaves out, so
	// that no package subtitle is made of them by its place
	textLeftOut = []string{"mov_text"}
)

// madeOf reports whether a package subtitle of format, written before its
// tracks said their streams, can be the one made of the original's stream
// st its path names: a picture of the same form, or text of a text stream
// an encode keeps (a stream whose codec isn't said is none).
func madeOf(format string, st stream) bool {
	if st.codec == "" {
		return false
	}
	form, sf := entryForms[strings.ToLower(strings.TrimSpace(format))], streamForms[st.codec]
	return form == sf && (sf != "" || !slices.Contains(textLeftOut, st.codec))
}

// packageStreams are the tracks of kind the package record pkg lists: of
// audio its stereo renditions and, but where stereo alone counts (one track
// a stream, as tracks are counted), their 5.1 companions; of subtitles none
// made of a file beside the original that says so (fromSidecar). Counted (of
// a record whose subtitles don't say their streams), a subtitle that says no
// stream counts only when its path, subs/<n>, names the n-th subtitle stream
// of orig, the original the package was made of, of a kind it can be made of
// (madeOf): a file beside the original numbered after the packaged file's
// subtitles, or named otherwise (subs/legacy_0.vtt), is none of its streams.
func packageStreams(pkg Doc, kind string, stereoAlone bool, orig []stream, counted bool) []stream {
	var list []any
	if kind == "audio" {
		ren, _ := pkg.Get("renditions")
		rd, _ := ren.(Doc)
		audio, _ := listOf(rd, "audio")
		list = append(list, audio...)
		if !stereoAlone {
			surround, _ := listOf(rd, "audioSurround")
			list = append(list, surround...)
		}
	} else {
		list, _ = listOf(pkg, "subtitles")
	}
	var out []stream
	for _, e := range list {
		d, ok := e.(Doc)
		if !ok {
			continue
		}
		i, named := toFloat(valueOf(d.Get("sourceStreamIndex")))
		if kind == "subtitle" {
			if sc, _ := d.Get("fromSidecar"); truthy(sc) {
				continue
			}
			if counted && !named {
				m := subtitleOrdinal.FindStringSubmatch(str(d, "path"))
				if m == nil {
					continue
				}
				n, _ := strconv.Atoi(m[1])
				if n >= len(orig) || !madeOf(str(d, "format"), orig[n]) {
					continue
				}
			}
		}
		forced, _ := d.Get("forced")
		out = append(out, stream{from: int(i), named: named, lang: primaryLanguage(str(d, "language")),
			label: primaryLanguage(str(d, "language")), purpose: str(d, "purpose"), forced: truthy(forced)})
	}
	return out
}

// labelKept reports whether a package track labelled to carries the
// language of its original's stream labelled from: the same language, a
// stream the original didn't label (und: the catalog learned it), or a label
// any track may take (zxx, mul). Any other change, to und among them, is a
// difference.
func labelKept(from, to string) bool {
	return from == to || slices.Contains(unknownLabels, from) || slices.Contains(keptLabels, to)
}

// Tracks is the deletion gate lost held track by track (ADR-0015: the master
// carries every track of its original; a missing language is one the
// original carries on a track the package has no track of). Of a version made
// of one original, the source record rec, whose package record is pkg, every
// audio stream and every subtitle stream needs a package track that says it
// was made from it (sourceStreamIndex), whatever the other tracks are
// labelled. A stream's language is an admin's correction of it where corr
// holds one (who and when recorded), else its label:
//
//   - a stream with none is lost ("audioTrack:2"), and so is its language
//     where no other stream of it is carried;
//   - a track whose label is no label of its stream's language (labelKept)
//     loses that language ("audioLanguages:en"), said in labels with both;
//     a subtitle that is no longer for the hard of hearing, or forced, loses
//     that too;
//   - a stream with none, where a package track of the kind says no stream,
//     can't be told (unknown).
//
// For those streams the gate's names of their language lists are these, in
// place of the essences': a language carried under another label it may take
// is no loss. A package none of whose tracks of a kind says its stream (a
// record written before tracks said them) is held by its tracks' languages
// counted (TrackCounts).
func Tracks(lost []string, rec, pkg Doc, corr Corrections) (kept []string, unknown string, labels []string) {
	names := map[string]bool{}
	for _, n := range lost {
		names[n] = true
	}
	for _, kind := range []string{"audio", "subtitle"} {
		src := sourceStreams(rec, kind, corr)
		if len(src) == 0 {
			continue
		}
		tracks := packageStreams(pkg, kind, false, nil, false)
		var named, unnamed []stream
		for _, t := range tracks {
			if t.named {
				named = append(named, t)
			} else {
				unnamed = append(unnamed, t)
			}
		}
		if len(named) == 0 && len(unnamed) > 0 {
			// no track says its stream: their languages counted
			var why string
			lostOf, said := trackCounts(kind, src, packageStreams(pkg, kind, true, src, true), &why)
			for n := range lostOf {
				names[n] = true
			}
			labels = append(labels, said...)
			if unknown == "" {
				unknown = why
			}
			continue
		}
		// track by track: the names of the lists of this kind are the tracks'
		srcLangs := map[string]bool{}
		for _, st := range src {
			srcLangs[st.lang], srcLangs[st.label] = true, true
		}
		for n := range names {
			for _, list := range trackLists[kind] {
				if l, ok := strings.CutPrefix(n, list+":"); ok && srcLangs[primaryLanguage(l)] {
					delete(names, n)
				}
			}
		}
		carried := map[string]bool{}
		var missing []stream
		for _, st := range src {
			var made []stream
			for _, t := range named {
				if st.named && t.from == st.from {
					made = append(made, t)
				}
			}
			if len(made) == 0 {
				missing = append(missing, st)
				continue
			}
			labelled := true
			for _, t := range made {
				if !labelKept(st.lang, t.lang) {
					labelled = false
					labels = append(labels, fmt.Sprintf("its %s track %d (stream %d) is %s in its original and %s in its "+
						"package", kind, st.ordinal, st.from, orUnd(st.lang), orUnd(t.lang)))
				}
				if kind == "subtitle" && st.sdh() && !t.sdh() && !slices.Contains(unknownLabels, st.lang) {
					names["sdhSubtitleLanguages:"+st.lang] = true
				}
				if kind == "subtitle" && st.forcedKind() && !t.forcedKind() && !slices.Contains(unknownLabels, st.lang) {
					names["forcedSubtitleLanguages:"+st.lang] = true
				}
			}
			if labelled {
				carried[st.lang] = true
			} else {
				names[trackLists[kind][0]+":"+st.lang] = true
			}
		}
		for _, st := range missing {
			if len(unnamed) > 0 {
				if unknown == "" {
					unknown = fmt.Sprintf("its package has %s track that doesn't say which of its original's streams it was "+
						"made from, and its original's stream %d (%s) has no track of its that does: whether it is that one "+
						"can't be told", article(kind), st.from, orUnd(st.lang))
				}
				continue
			}
			names[trackNames[kind]+":"+strconv.Itoa(st.from)] = true
			if !carried[st.lang] && !slices.Contains(unknownLabels, st.lang) {
				names[trackLists[kind][0]+":"+st.lang] = true
				if kind == "subtitle" && st.sdh() {
					names["sdhSubtitleLanguages:"+st.lang] = true
				}
				if kind == "subtitle" && st.forcedKind() {
					names["forcedSubtitleLanguages:"+st.lang] = true
				}
			}
		}
	}
	for n := range names {
		kept = append(kept, n)
	}
	sort.Strings(kept)
	return kept, unknown, labels
}

// TrackCounts holds a package against an original by its tracks' languages
// counted, where no track of a kind says which stream it was made from (a
// record written before tracks said them, and a master against an original
// it wasn't made of): lost, the names it adds to the gate's lost of the
// original's record rec against the package's pkg, and why the counts can't
// tell, "" when they can. The package's tracks of a kind are counted as
// packageStreams counts them, against the streams of the record (of pkgSrc,
// the record of the original the package was made of; nil: not told) a
// subtitle path names.
func TrackCounts(lost []string, rec, pkg, pkgSrc Doc, corr Corrections) (kept []string, unknown string, counts []string) {
	names := map[string]bool{}
	for _, n := range lost {
		names[n] = true
	}
	for _, kind := range []string{"audio", "subtitle"} {
		src := sourceStreams(rec, kind, corr)
		if len(src) == 0 {
			continue
		}
		var why string
		lostOf, said := trackCounts(kind, src, packageStreams(pkg, kind, true, sourceStreams(pkgSrc, kind, nil), true), &why)
		for n := range lostOf {
			names[n] = true
		}
		counts = append(counts, said...)
		if unknown == "" {
			unknown = why
		}
	}
	for n := range names {
		kept = append(kept, n)
	}
	sort.Strings(kept)
	return kept, unknown, counts
}

// trackCounts are the names an original's streams src of kind lose against
// the package's tracks, counted language by language: a language of which
// the package has fewer tracks than the original loses a track of it
// ("audioTrack:de"), and the language where it has none ("audioLanguages:de").
// Where a track the package has more of could be one of them under another
// label it may take (any, of an und stream; zxx or mul, of any), the counts
// can't tell: why says so.
func trackCounts(kind string, src, tracks []stream, why *string) (map[string]bool, []string) {
	had, has := map[string]int{}, map[string]int{}
	for _, st := range src {
		had[st.lang]++
	}
	for _, t := range tracks {
		has[t.lang]++
	}
	var surplus []string
	for l, n := range has {
		if n > had[l] {
			surplus = append(surplus, l)
		}
	}
	sort.Strings(surplus)
	var langs []string
	for l := range had {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	out := map[string]bool{}
	var said []string
	for _, l := range langs {
		if has[l] >= had[l] {
			continue
		}
		relabel := slices.ContainsFunc(surplus, func(to string) bool { return labelKept(l, to) })
		if relabel {
			if *why == "" {
				*why = fmt.Sprintf("its package's %s tracks don't say which of its original's streams they were made from, "+
					"and it has fewer tracks of %s than its original (%d of %d) and more of %s: whether they are those "+
					"under another label can't be told", kind, orUnd(l), has[l], had[l], strings.Join(surplus, ", "))
			}
			continue
		}
		out[trackNames[kind]+":"+orUnd(l)] = true
		if has[l] == 0 && !slices.Contains(unknownLabels, l) {
			out[trackLists[kind][0]+":"+l] = true
		}
		said = append(said, fmt.Sprintf("its original has %d %s %s in %s, and its package %d", had[l], kind,
			plural(had[l], "track", "tracks"), orUnd(l), has[l]))
	}
	return out, said
}

func article(kind string) string {
	return map[string]string{"audio": "an audio", "subtitle": "a subtitle"}[kind]
}

func orUnd(lang string) string {
	if lang == "" {
		return "und"
	}
	return lang
}

// hdrRanges are the source pictures of a high dynamic range, as its record's
// first video stream says it (hdr.format).
var hdrRanges = []string{"hdr10", "hdr10plus", "hlg", "dolby-vision"}

// hdrLost reports whether a source of the records sources has a picture of a
// high dynamic range (HDR10, HLG, Dolby Vision; with or without static
// metadata) and the package whose record is pkg keeps none: its top video
// rendition is not PQ or HLG.
func hdrLost(sources []Doc, pkg Doc) bool {
	if r := topVideoRange(pkg); r == "PQ" || r == "HLG" {
		return false
	}
	for _, rec := range sources {
		if v := firstVideo(rec); v != nil {
			h, _ := v.Get("hdr")
			hd, _ := h.(Doc)
			if slices.Contains(hdrRanges, str(hd, "format")) {
				return true
			}
		}
	}
	return false
}

// firstVideo is the first video stream of the source record rec that is no
// attached picture; nil when it lists none.
func firstVideo(rec Doc) Doc {
	streams, _ := listOf(rec, "streams")
	for _, e := range streams {
		st, ok := e.(Doc)
		if !ok || str(st, "type") != "video" {
			continue
		}
		if disp, _ := st.Get("dispositions"); disp != nil {
			if dd, ok := disp.(Doc); ok {
				if v, _ := dd.Get("attachedPic"); truthy(v) {
					continue
				}
			}
		}
		return st
	}
	return nil
}

// versionSources are the sources the record of the version versionID of the
// item folder itemDir says it was made of (version.json's sourceIds); nil
// when it can't be read.
func versionSources(itemDir, versionID string) []string {
	d, err := readRecordDoc(filepath.Join(VersionDir(itemDir, versionID), VersionFile))
	if err != nil {
		return nil
	}
	v, _ := d.Get("sourceIds")
	return stringsOf(v)
}

// valueOf is the value of a Doc.Get.
func valueOf(v any, _ bool) any { return v }

// KeepsHDR reports whether the package whose record is pkg keeps the HDR of
// the Dolby Vision of the sources whose records are sources, as the master
// spec makes it: its top video rendition is HDR10 (PQ), or HLG where every
// Dolby Vision source has an HLG base. A package that says no range, an SDR
// one, and an HLG one of another base keep none.
func KeepsHDR(sources []Doc, pkg Doc) bool {
	switch topVideoRange(pkg) {
	case "PQ":
		return true
	case "HLG":
		for _, rec := range sources {
			if v, _ := essenceOf(rec).Get("dolbyVision"); truthy(v) && !hlgBase(rec) {
				return false
			}
		}
		return len(sources) > 0
	}
	return false
}

// topVideoRange is the range of the top video rendition of the package
// record pkg, the first it lists, as the master playlist gives it (PQ, HLG,
// SDR), or as its dynamic range says it; "" when it says neither.
func topVideoRange(pkg Doc) string {
	ren, _ := pkg.Get("renditions")
	rd, _ := ren.(Doc)
	video, _ := listOf(rd, "video")
	if len(video) == 0 {
		return ""
	}
	v0, _ := video[0].(Doc)
	if r := strings.ToUpper(str(v0, "videoRange")); r != "" {
		return r
	}
	switch str(v0, "dynamicRange") {
	case "hdr10", "hdr10plus":
		return "PQ"
	case "hlg":
		return "HLG"
	case "sdr":
		return "SDR"
	}
	return ""
}

// hlgBase reports whether the picture of the source record rec, its first
// video stream that is no attached picture, is HLG, or Dolby Vision whose
// base layer is (its compatibility id 4).
func hlgBase(rec Doc) bool {
	st := firstVideo(rec)
	if st == nil {
		return false
	}
	h, _ := st.Get("hdr")
	hd, _ := h.(Doc)
	if str(hd, "format") == "hlg" {
		return true
	}
	dv, _ := hd.Get("dolbyVision")
	dd, _ := dv.(Doc)
	id, _ := dd.Get("blCompatibilityId")
	n, ok := toFloat(id)
	return ok && n == 4
}

// readRecordDoc is the record at path, decoded, as the gate reads one: a
// record an object of which gives a key twice is none it can read (which of
// the two a reader takes is a reader's choice, and the gate's would be one);
// nor is one that gives a number no reader holds (unheldNumber), as the
// schemas' loaders refuse it.
func readRecordDoc(path string) (Doc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	d, err := DecodeDoc(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if k := repeatedKey(d); k != "" {
		return nil, fmt.Errorf("%s: its key %q is given twice in one object", path, k)
	}
	if n := unheldNumber(d); n != "" {
		return nil, fmt.Errorf("%s: %s", path, n)
	}
	return d, nil
}

// unheldNumber says of a number of the decoded value v that no reader holds,
// "" when none: one written with a fraction or an exponent that a float
// can't hold (1e999, which Python's json reads as Infinity, and the schemas'
// loaders refuse as they refuse Infinity and NaN - no JSON, which the
// decoder refuses already), and a whole number of more digits than Python
// reads (PyIntDigits: its json can't load the record).
func unheldNumber(v any) string {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			if f, err := strconv.ParseFloat(s, 64); err != nil && math.IsInf(f, 0) {
				return fmt.Sprintf("it gives %.20s..., a number past what a float holds (as Infinity)", s)
			}
			return ""
		}
		if len(strings.TrimPrefix(s, "-")) > PyIntDigits {
			return fmt.Sprintf("it gives a whole number of %d digits, more than Python reads (%d)",
				len(strings.TrimPrefix(s, "-")), PyIntDigits)
		}
	case Doc:
		for _, f := range x {
			if n := unheldNumber(f.Value); n != "" {
				return n
			}
		}
	case []any:
		for _, e := range x {
			if n := unheldNumber(e); n != "" {
				return n
			}
		}
	}
	return ""
}

// repeatedKey is a key an object of the decoded value v gives twice, ""
// when none does.
func repeatedKey(v any) string {
	switch x := v.(type) {
	case Doc:
		seen := make(map[string]bool, len(x))
		for _, f := range x {
			if seen[f.Key] {
				return f.Key
			}
			seen[f.Key] = true
			if k := repeatedKey(f.Value); k != "" {
				return k
			}
		}
	case []any:
		for _, e := range x {
			if k := repeatedKey(e); k != "" {
				return k
			}
		}
	}
	return ""
}

// essenceOf is the essence a decoded record holds: none when it holds none.
func essenceOf(record Doc) Doc {
	e, _ := record.Get("essence")
	ed, _ := e.(Doc)
	return ed
}

// GateUnknown is a deletion gate the records can't tell, saying why: what
// of the original they leave unknown is no loss to accept, and its original
// is kept.
type GateUnknown struct{ Why string }

func (e *GateUnknown) Error() string { return e.Why }

// The facts of an essence about subtitles, as the deletion gate compares
// them: counts, flags and language lists.
var (
	subtitleCounts = []string{"subtitleTracks", "commentarySubtitles"}
	subtitleFlags  = []string{"imageSubtitles", "styledSubtitles"}
	subtitleLists  = []string{"subtitleLanguages", "sdhSubtitleLanguages", "forcedSubtitleLanguages"}
)

// subtitleKeys are those facts, in the order the gate names them.
var subtitleKeys = slices.Concat(subtitleCounts, subtitleFlags, subtitleLists)

// SubtitlesUnknown says why what the package whose record is pkg
// (package.json, in the folder pkgDir) carries of the subtitles of the sources
// whose records are sources (source.json) can't be told, "" when it can. The
// gate reads an essence's missing fact as nothing: of a source as nothing to
// lose, of a package as nothing kept, a loss the retirement accepts; and it
// reads the package's essence alone, which its writer derives from the
// subtitles its record lists. Unknown is not let go, so the records can't
// tell:
//
//   - of a source whose record lists no stream at all and whose essence says
//     nothing of subtitles (an original never probed);
//   - of a source whose record lists subtitle streams and whose essence lacks
//     one of the subtitle facts (subtitleKeys).
//
// For a source with subtitles - an essence that says it has some, or a
// record that lists a subtitle stream - they can't tell either:
//
//   - where the package's record lists no subtitles at all (no subtitles
//     array), whatever its essence claims;
//   - where its essence claims of what the source has more than the
//     subtitles its record lists back, of those whose files are in pkgDir
//     (all of them, with pkgDir ""): more tracks or commentaries, a
//     language, an image or a styled subtitle, a forced or an SDH language
//     none of them is;
//   - where its record lists subtitles and its essence lacks a subtitle fact
//     a source has.
//
// A package whose record lists none (an empty subtitles array) and whose
// essence claims none of them carries none: what the sources had of them is
// lost, and the gate names it (subtitleTracks, subtitleLanguages:de, ...).
func SubtitlesUnknown(sources []Doc, pkg Doc, pkgDir string) string {
	had := map[string]bool{}
	hadLangs := map[string]map[string]bool{} // a language list's -> the primary languages the sources have
	streams := 0
	for _, rec := range sources {
		ess := essenceOf(rec)
		n := subtitleStreams(rec)
		streams = max(streams, n)
		if n > 0 {
			if missing := missingFacts(ess, subtitleKeys); len(missing) > 0 {
				return fmt.Sprintf("its original's record lists %d subtitle %s, and its essence says nothing of %s: what its "+
					"package carries of them can't be told", n, plural(n, "track", "tracks"), strings.Join(missing, ", "))
			}
		}
		if streamList, _ := listOf(rec, "streams"); len(streamList) == 0 && len(missingFacts(ess, subtitleKeys)) == len(subtitleKeys) {
			return "its original's record lists no stream, and its essence says nothing of its subtitles (it was never " +
				"probed): what its package carries of them can't be told"
		}
		for _, k := range subtitleKeys {
			v, _ := ess.Get(k)
			if hasFact(k, v) {
				had[k] = true
			}
			if slices.Contains(subtitleLists, k) {
				for _, l := range stringsOf(v) {
					if hadLangs[k] == nil {
						hadLangs[k] = map[string]bool{}
					}
					hadLangs[k][primaryLanguage(l)] = true
				}
			}
		}
	}
	if len(had) == 0 && streams == 0 {
		return ""
	}
	has := "its original has subtitles"
	if n := max(streams, mostOf(sources, "subtitleTracks")); n > 0 {
		has = fmt.Sprintf("its original has %d subtitle %s", n, plural(n, "track", "tracks"))
	}
	listed, isList := listOf(pkg, "subtitles")
	if !isList {
		return "its package's record says nothing about its subtitles (" + has + "): what its package carries of them " +
			"can't be told"
	}
	ess := essenceOf(pkg)
	subs := listedSubtitles(listed, pkgDir)
	if v, _ := ess.Get("subtitleTracks"); v != nil {
		if n, ok := toFloat(v); ok && n > float64(len(subs)) {
			return fmt.Sprintf("its package's essence says %s subtitle tracks, and its record lists %d (%s): what its "+
				"package carries of them can't be told", strconvNumber(n), len(subs), has)
		}
	}
	if unbacked := unbackedClaims(ess, subs, had, hadLangs); len(unbacked) > 0 {
		return "its package's essence says " + strings.Join(unbacked, ", ") + ", and its record lists no such subtitle (" +
			has + "): what its package carries of them can't be told"
	}
	if len(listed) == 0 {
		// it carries none, and says so: the gate names what is lost
		return ""
	}
	var wanted []string
	for _, k := range subtitleKeys {
		if had[k] {
			wanted = append(wanted, k)
		}
	}
	if missing := missingFacts(ess, wanted); len(missing) > 0 {
		return "its package's essence says nothing of " + strings.Join(missing, ", ") + " (" + has + "): what its " +
			"package carries of them can't be told"
	}
	return ""
}

// listedSubtitle is a subtitle a package's record lists, as the gate holds
// its essence against it: its primary language, whether it is a picture, a
// styled one (it keeps its ASS original), for what it is (sdh, forced,
// signs-songs, commentary) and whether it is flagged forced.
type listedSubtitle struct {
	lang, purpose        string
	image, styled, force bool
}

// packageImageForms are the formats of a package's picture subtitles, as its
// writer counts them for imageSubtitles.
var packageImageForms = []string{"pgs", "sup", "vobsub", "dvb", "xsub"}

// listedSubtitles are the subtitles of the package record's list, those whose
// file is in the folder dir (all of them, with dir "").
func listedSubtitles(list []any, dir string) []listedSubtitle {
	var out []listedSubtitle
	for _, e := range list {
		d, ok := e.(Doc)
		if !ok {
			continue
		}
		if dir != "" {
			path := str(d, "path")
			if path == "" || !filepath.IsLocal(filepath.FromSlash(path)) {
				continue
			}
			if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil || !fi.Mode().IsRegular() {
				continue
			}
		}
		styled, _ := d.Get("styled")
		forced, _ := d.Get("forced")
		out = append(out, listedSubtitle{lang: primaryLanguage(str(d, "language")), purpose: str(d, "purpose"),
			image:  slices.Contains(packageImageForms, strings.ToLower(strings.TrimSpace(str(d, "format")))),
			styled: truthy(styled), force: truthy(forced)})
	}
	return out
}

// unbackedClaims are what the package essence ess claims of what the sources
// have (had, hadLangs) that none of the subtitles subs it lists is: in the
// gate's names, a count as its name.
func unbackedClaims(ess Doc, subs []listedSubtitle, had map[string]bool, hadLangs map[string]map[string]bool) []string {
	var out []string
	if had["commentarySubtitles"] {
		v, _ := ess.Get("commentarySubtitles")
		if n, ok := toFloat(v); ok {
			c := 0
			for _, sub := range subs {
				if sub.purpose == "commentary" {
					c++
				}
			}
			if n > float64(c) {
				out = append(out, "commentarySubtitles")
			}
		}
	}
	for _, k := range subtitleFlags {
		if v, _ := ess.Get(k); !had[k] || !truthy(v) {
			continue
		}
		if !slices.ContainsFunc(subs, func(sub listedSubtitle) bool {
			return k == "imageSubtitles" && sub.image || k == "styledSubtitles" && sub.styled
		}) {
			out = append(out, k)
		}
	}
	for _, k := range subtitleLists {
		v, _ := ess.Get(k)
		claimed := map[string]bool{}
		for _, l := range stringsOf(v) {
			claimed[primaryLanguage(l)] = true
		}
		var langs []string
		for l := range hadLangs[k] {
			if claimed[l] {
				langs = append(langs, l)
			}
		}
		sort.Strings(langs)
		for _, l := range langs {
			if !slices.ContainsFunc(subs, func(sub listedSubtitle) bool {
				if sub.lang != l {
					return false
				}
				switch k {
				case "sdhSubtitleLanguages":
					return sub.purpose == "sdh"
				case "forcedSubtitleLanguages":
					return sub.purpose == "forced" || sub.purpose == "signs-songs" || sub.force
				}
				return true
			}) {
				out = append(out, k+":"+l)
			}
		}
	}
	return out
}

// primaryLanguage is the primary language of a language code or tag ("de"
// of "de", "deu", "ger", "de-CH"), the code in lower case where it names no
// language the gate knows.
func primaryLanguage(code string) string {
	if t, ok := languages.Tag(code); ok {
		primary, _, _ := strings.Cut(t, "-")
		return strings.ToLower(primary)
	}
	return strings.ToLower(strings.TrimSpace(code))
}

// hasFact reports whether the value v of the subtitle fact key says a source
// has some: a count above 0, a flag that is true, a language list that is
// not empty.
func hasFact(key string, v any) bool {
	if slices.Contains(subtitleLists, key) {
		return len(stringsOf(v)) > 0
	}
	if slices.Contains(subtitleCounts, key) {
		n, ok := toFloat(v)
		return ok && n > 0
	}
	return truthy(v)
}

// missingFacts are the keys of keys the essence ess lacks or holds as null.
func missingFacts(ess Doc, keys []string) []string {
	var out []string
	for _, k := range keys {
		if v, _ := ess.Get(k); v == nil {
			out = append(out, k)
		}
	}
	return out
}

// subtitleStreams is how many subtitle streams the source record rec lists.
func subtitleStreams(rec Doc) int {
	v, _ := rec.Get("streams")
	list, _ := v.([]any)
	n := 0
	for _, e := range list {
		if d, ok := e.(Doc); ok && str(d, "type") == "subtitle" {
			n++
		}
	}
	return n
}

// mostOf is the most of the count key the essences of the records say, 0
// when none says one.
func mostOf(records []Doc, key string) int {
	most := 0
	for _, rec := range records {
		v, _ := essenceOf(rec).Get(key)
		if n, ok := toFloat(v); ok {
			most = max(most, int(n))
		}
	}
	return most
}

// listOf is the list d holds at key, and whether it holds one: a JSON array,
// empty or not; not one when the key is missing or null.
func listOf(d Doc, key string) ([]any, bool) {
	v, _ := d.Get(key)
	list, ok := v.([]any)
	return list, ok
}

// strconvNumber is n as a record writes a count: without a fraction when it
// has none.
func strconvNumber(n float64) string {
	if n == float64(int64(n)) {
		return fmt.Sprintf("%d", int64(n))
	}
	return fmt.Sprintf("%g", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
