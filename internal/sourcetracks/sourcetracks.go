// Package sourcetracks reads what a package says of its source's tracks: the
// audio and subtitle streams the packager found in the source, as the
// manifest it writes beside the package (and sends with packaging-complete)
// lists them.
//
//   - renditions.audio has one entry per audio stream of the source, in
//     ffprobe's order: an entry's ordinal is its idx, the stream's place among
//     the source's audio streams (its place in the list when it has none).
//   - subtitles has one entry per subtitle stream the packager extracted, id
//     sub<N>, N the stream's place among the source's subtitle streams. An
//     entry with another id is no stream of the source: the packager's
//     rendition of a subtitle file beside the source (the worker record's
//     subtitleFiles).
//
// A kind the manifest does not list at all is not reported: a manifest without
// renditions.audio says nothing of the source's audio, one with an empty list
// that it has none.
package sourcetracks

import (
	"encoding/json"
	"regexp"
	"strconv"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// FromManifest is what manifest (a manifest decoded with json.Number numbers,
// or with float64 ones) reports of the source's tracks, by kind: a kind it
// does not list is not a key.
func FromManifest(manifest map[string]any) map[string][]model.SourceTrack {
	out := map[string][]model.SourceTrack{}
	if renditions, ok := manifest["renditions"].(map[string]any); ok {
		if list, ok := renditions["audio"].([]any); ok {
			tracks := []model.SourceTrack{}
			for i, e := range list {
				a, ok := e.(map[string]any)
				if !ok {
					continue
				}
				ordinal, ok := AudioOrdinal(a, i)
				if !ok {
					continue
				}
				tracks = append(tracks, model.SourceTrack{Kind: model.TrackAudio, Ordinal: ordinal,
					Language: str(a["language"]), Title: str(a["title"])})
			}
			out[model.TrackAudio] = tracks
		}
	}
	if list, ok := manifest["subtitles"].([]any); ok {
		tracks := []model.SourceTrack{}
		for _, e := range list {
			s, ok := e.(map[string]any)
			if !ok {
				continue
			}
			ordinal, ok := SubtitleOrdinal(s)
			if !ok {
				continue
			}
			forced, _ := s["forced"].(bool)
			tracks = append(tracks, model.SourceTrack{Kind: model.TrackSubtitle, Ordinal: ordinal,
				Language: str(s["language"]), Title: str(s["title"]), Format: str(s["format"]), Forced: forced})
		}
		out[model.TrackSubtitle] = tracks
	}
	return out
}

// AudioOrdinal is the ordinal of an audio rendition of a manifest, the
// position-th of its list: its idx, a whole number of 0 or more, else its
// position. ok is false for an idx that is no such number.
func AudioOrdinal(rendition map[string]any, position int) (int32, bool) {
	raw, present := rendition["idx"]
	if !present || raw == nil {
		return int32(position), position >= 0
	}
	n, ok := whole(raw)
	if !ok || n < 0 || n > 1<<20 {
		return 0, false
	}
	return int32(n), true
}

var streamID = regexp.MustCompile(`^sub([0-9]{1,6})$`)

// SubtitleOrdinal is the ordinal of a subtitle of a manifest that is one of
// the source's streams, from its id sub<N>; ok is false for any other id, a
// subtitle file's rendition.
func SubtitleOrdinal(subtitle map[string]any) (int32, bool) {
	id, _ := subtitle["id"].(string)
	m := streamID.FindStringSubmatch(id)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 32)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

// whole reads a JSON number that is a whole number.
func whole(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// str is a JSON string, nil for anything else.
func str(v any) *string {
	s, ok := v.(string)
	if !ok {
		return nil
	}
	return &s
}
