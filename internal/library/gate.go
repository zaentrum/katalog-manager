package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	d, err := DecodeDoc(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	e, _ := d.Get("essence")
	ed, _ := e.(Doc)
	return ed, nil
}

// GateOf is the deletion gate of the version versionID of the item folder
// itemDir for the sources sourceIDs, from their records:
// sources/<id>/source.json and versions/<id>/package.json.
func GateOf(itemDir, versionID string, sourceIDs ...string) ([]string, error) {
	pkg, err := Essence(filepath.Join(VersionDir(itemDir, versionID), PackageFile))
	if err != nil {
		return nil, err
	}
	var sources []Doc
	for _, sid := range sourceIDs {
		e, err := Essence(filepath.Join(SourceDir(itemDir, sid), "source.json"))
		if err != nil {
			return nil, err
		}
		sources = append(sources, e)
	}
	return DeletionGate(sources, pkg), nil
}
