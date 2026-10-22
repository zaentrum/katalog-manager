package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/languages"
)

// A title taken in (migration 044) plays the original its version's folder
// holds, and has no package: the subtitles it has of its own are the
// subtitle streams embedded in that file, which chino-stream extracts when a
// player asks for one. The catalog keeps no row of them. The original's
// source record (sources/<sourceId>/source.json, zaentrum.library.source/2),
// written once when the original was taken in, lists every stream of the
// file as the probe found it, and the attach seam lists each subtitle stream
// there as one of the title's own subtitles (EmbeddedSubtitles), so that no
// client makes or fetches again a subtitle the file has. Once a package is
// added to the version, the rows of the package's subtitles are the title's
// own instead.

// EmbeddedPrefix begins the id of a subtitle stream embedded in an original:
// source-<sourceId>-<index>, <index> the stream's index in the file
// (EmbeddedID). No row has such an id.
const EmbeddedPrefix = "source-"

// EmbeddedID is the id of the stream index of the original of the source
// sourceID, as the attach seam lists it: the same as long as the source
// record is, which is written once.
func EmbeddedID(sourceID string, index int) string {
	return EmbeddedPrefix + sourceID + "-" + strconv.Itoa(index)
}

// ParseEmbeddedID is the source and the stream index an id EmbeddedID made
// names; false for any other id.
func ParseEmbeddedID(id string) (string, int, bool) {
	rest, ok := strings.CutPrefix(id, EmbeddedPrefix)
	i := strings.LastIndexByte(rest, '-')
	if !ok || i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n < 0 || strconv.Itoa(n) != rest[i+1:] {
		return "", 0, false
	}
	return rest[:i], n, true
}

// The purposes of a subtitle stream (the schema's subtitlePurpose) the
// attach seam reads: what its flags may not say.
const (
	purposeForced     = "forced"
	purposeSignsSongs = "signs-songs"
	purposeSDH        = "sdh"
	purposeCommentary = "commentary"
	purposeLyrics     = "lyrics"
)

// imageSubtitleCodecs are the codecs of a subtitle stream of pictures, not
// text (a record says so in form; one that does not is read by its codec).
var imageSubtitleCodecs = map[string]bool{"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true, "xsub": true}

// recordedStream is a stream of an original as its source record lists it,
// in what the attach seam reads of a subtitle stream.
type recordedStream struct {
	Index        *int    `json:"index"`
	Type         string  `json:"type"`
	Codec        *string `json:"codec"`
	Language     *string `json:"language"`
	LanguageRaw  *string `json:"languageRaw"`
	Title        *string `json:"title"`
	Dispositions struct {
		Forced          bool `json:"forced"`
		HearingImpaired bool `json:"hearingImpaired"`
		Commentary      bool `json:"commentary"`
		Lyrics          bool `json:"lyrics"`
	} `json:"dispositions"`
	Form    *string `json:"form"`
	Variant *string `json:"variant"`
	Purpose *string `json:"purpose"`
}

// EmbeddedSubtitles lists the subtitle streams embedded in the original of
// the source sourceID of the item folder itemDir as the source's record
// lists them, in the file's order, each as one of the title's own subtitles:
// its id EmbeddedID's, its language its tag (embeddedLanguage), its label
// the stream's title, its format the stream's codec, forced as the file
// flags it or its purpose is (forced, signs and songs), for the deaf and
// hard of hearing as the file flags it or its purpose is (sdh); no origin,
// not attached, embedded, with its index in the file (stream), its form
// (text) and its purpose. A stream of a commentary or of song lyrics alone
// is left out: it is no subtitle of the dialogue. So is a stream of pictures
// (PGS, VobSub, DVB, XSUB): an original's subtitle streams are served as
// text, which a picture is not without OCR, so no player is served it and it
// is no subtitle the title has. An error says why
// the record tells nothing of them: it is not there, it cannot be read, it
// is no source record, or it lists none of the original's streams (the
// original was never probed).
func EmbeddedSubtitles(itemDir, sourceID string) ([]Subtitle, error) {
	rel := "sources/" + sourceID + "/source.json"
	b, err := os.ReadFile(filepath.Join(SourceDir(itemDir, sourceID), "source.json"))
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("its source record %s is not there", rel)
	case errors.As(err, &pe):
		return nil, fmt.Errorf("its source record %s cannot be read: %v", rel, pe.Err)
	case err != nil:
		return nil, fmt.Errorf("its source record %s cannot be read: %v", rel, err)
	}
	var rec struct {
		Streams []recordedStream `json:"streams"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("its source record %s is no source record: %v", rel, err)
	}
	if len(rec.Streams) == 0 {
		return nil, fmt.Errorf("its source record %s lists none of the original's streams", rel)
	}
	out := []Subtitle{}
	for _, s := range rec.Streams {
		if sub, ok := s.subtitle(sourceID); ok {
			out = append(out, sub)
		}
	}
	return out, nil
}

// subtitle is the stream s of the original of the source sourceID as one of
// the title's own subtitles (EmbeddedSubtitles); false for a stream that is
// none: no subtitle stream, one without an index, one of a commentary or of
// lyrics alone, and one that is no text — its form the record's, else its
// codec's, and a stream the record gives neither is none either.
func (s recordedStream) subtitle(sourceID string) (Subtitle, bool) {
	purpose := strings.ToLower(strings.TrimSpace(deref(s.Purpose)))
	if s.Type != "subtitle" || s.Index == nil || *s.Index < 0 || purpose == purposeCommentary ||
		purpose == purposeLyrics || s.Dispositions.Commentary || s.Dispositions.Lyrics {
		return Subtitle{}, false
	}
	index := *s.Index
	codec := strings.ToLower(strings.TrimSpace(deref(s.Codec)))
	form := strings.ToLower(strings.TrimSpace(deref(s.Form)))
	if form == "" && codec != "" {
		form = "text"
		if imageSubtitleCodecs[codec] {
			form = "image"
		}
	}
	if form != "text" {
		return Subtitle{}, false
	}
	var label *string
	if t := strings.TrimSpace(deref(s.Title)); t != "" {
		label = &t
	}
	return Subtitle{
		ID:       EmbeddedID(sourceID, index),
		Language: embeddedLanguage(s.Language, s.LanguageRaw, s.Variant),
		Label:    label,
		Format:   codec,
		Forced:   s.Dispositions.Forced || purpose == purposeForced || purpose == purposeSignsSongs,
		SDH:      s.Dispositions.HearingImpaired || purpose == purposeSDH,
		Embedded: true,
		Stream:   &index,
		Form:     form,
		Purpose:  purpose,
	}, true
}

// The words of a stream's variant (read from its title: the schema's
// subtitleStream.variant) a language tag says: the script of Chinese text,
// and a region where the word names one cleanly. Any other (Castilian,
// European, Cantonese, Hong Kong, …) adds nothing to the tag: the stream's
// title, its label, says it.
var (
	variantScripts = map[string]string{"simplified": "Hans", "traditional": "Hant"}
	variantRegions = map[string]string{"latin american": "419", "latinamerican": "419", "latam": "419",
		"brazilian": "BR", "canadian": "CA"}
)

// embeddedLanguage is the language tag of a subtitle stream: its language as
// languages.Tag spells it (else the code its file carried; "" for a stream
// of no language, as a row of none is listed), with the script its variant
// names for Chinese, and the region it names, where the tag has none of its
// own.
func embeddedLanguage(language, raw, variant *string) string {
	tag := ""
	for _, code := range []*string{language, raw} {
		if code == nil {
			continue
		}
		if t, ok := languages.Tag(*code); ok {
			tag = t
			break
		}
	}
	if tag == "" || variant == nil {
		return tag
	}
	word := strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(*variant, "-", " "))), " ")
	parts := strings.Split(tag, "-")
	primary, rest := parts[0], parts[1:]
	var script, region string
	if len(rest) > 0 && len(rest[0]) == 4 && !isDigit(rest[0][0]) {
		script, rest = rest[0], rest[1:]
	}
	if len(rest) > 0 && (len(rest[0]) == 2 || len(rest[0]) == 3 && isDigit(rest[0][0])) {
		region, rest = rest[0], rest[1:]
	}
	if s, ok := variantScripts[word]; ok && script == "" && primary == "zh" {
		script = s
	}
	if r, ok := variantRegions[word]; ok && region == "" {
		region = r
	}
	out := []string{primary}
	for _, p := range []string{script, region} {
		if p != "" {
			out = append(out, p)
		}
	}
	if t, ok := languages.Tag(strings.Join(append(out, rest...), "-")); ok {
		return t
	}
	return tag
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// embeddedIn reports whether sub is the id of a subtitle stream embedded in
// an original of the item id (EmbeddedID: one of its sources'), a subtitle
// of its own, which no caller removes from the file.
func embeddedIn(ctx context.Context, q Querier, id, sub string) (bool, error) {
	source, _, ok := ParseEmbeddedID(sub)
	if !ok {
		return false, nil
	}
	var own bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources WHERE id = $1 AND item_id = $2)`,
		source, id).Scan(&own)
	return own, err
}
