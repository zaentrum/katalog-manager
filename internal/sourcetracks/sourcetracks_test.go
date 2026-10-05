package sourcetracks

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// decode reads a manifest as packaging-complete does: numbers as json.Number.
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// lines are the tracks of a report, one per line, by kind.
func lines(report map[string][]model.SourceTrack) string {
	opt := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	var out []string
	for _, kind := range []string{model.TrackAudio, model.TrackSubtitle} {
		tracks, ok := report[kind]
		if !ok {
			out = append(out, kind+" not listed")
			continue
		}
		if len(tracks) == 0 {
			out = append(out, kind+" none")
		}
		for _, t := range tracks {
			out = append(out, fmt.Sprintf("%s %d %s %s %s %v", t.Kind, t.Ordinal, opt(t.Language), opt(t.Title), opt(t.Format), t.Forced))
		}
	}
	return strings.Join(out, "\n")
}

// A manifest's audio renditions are the source's audio streams, each at its
// idx (its place in the list without one), the 5.1 companions none of them;
// its subtitles with an id sub<N> are the source's subtitle streams at N,
// any other is a subtitle file's rendition, no stream. A kind not listed is
// not reported, one listed empty reported without a track.
func TestFromManifest(t *testing.T) {
	m := decode(t, `{"version": 2,
		"renditions": {
			"audio": [
				{"id": "a0", "idx": 1, "language": "eng", "title": "Commentary"},
				{"id": "a1", "idx": 0, "language": "und", "title": ""},
				{"id": "a2", "language": "ger"},
				{"id": "a3", "idx": "x", "language": "fre"},
				"not a rendition"],
			"audioSurround": [{"id": "a4", "idx": 0, "language": "und"}]},
		"subtitles": [
			{"id": "sub0", "language": "ger", "format": "pgs", "forced": false, "path": "subs/0.sup"},
			{"id": "sub3", "language": "eng", "title": "Signs", "format": "webvtt", "forced": true},
			{"id": "file0", "language": "deu", "format": "webvtt"},
			{"id": "sub", "language": "fra"},
			{"language": "spa"}]}`)
	if got, want := lines(FromManifest(m)), `audio 1 eng Commentary - false
audio 0 und  - false
audio 2 ger - - false
subtitle 0 ger - pgs false
subtitle 3 eng Signs webvtt true`; got != want {
		t.Errorf("a v2 manifest:\n%s\nwant:\n%s", got, want)
	}
	if got, want := lines(FromManifest(decode(t, `{"renditions": {"video": [], "audio": []}, "subtitles": []}`))), "audio none\nsubtitle none"; got != want {
		t.Errorf("a source without audio and subtitles:\n%s", got)
	}
	if got, want := lines(FromManifest(decode(t, `{"renditions": {"video": []}}`))), "audio not listed\nsubtitle not listed"; got != want {
		t.Errorf("a manifest listing neither:\n%s", got)
	}
	if got := FromManifest(map[string]any{}); len(got) != 0 {
		t.Errorf("an empty manifest: %v", got)
	}
}

// An audio rendition's ordinal is its idx, a whole number of 0 or more, else
// its place; a subtitle's is N of its id sub<N>.
func TestOrdinals(t *testing.T) {
	for _, c := range []struct {
		r    map[string]any
		at   int
		want int32
		ok   bool
	}{
		{map[string]any{"idx": json.Number("2")}, 0, 2, true},
		{map[string]any{"idx": 3.0}, 0, 3, true},
		{map[string]any{}, 4, 4, true},
		{map[string]any{"idx": nil}, 1, 1, true},
		{map[string]any{"idx": json.Number("-1")}, 0, 0, false},
		{map[string]any{"idx": 1.5}, 0, 0, false},
		{map[string]any{"idx": "1"}, 0, 0, false},
	} {
		if got, ok := AudioOrdinal(c.r, c.at); got != c.want || ok != c.ok {
			t.Errorf("AudioOrdinal(%v, %d) = %d %v, want %d %v", c.r, c.at, got, ok, c.want, c.ok)
		}
	}
	for id, want := range map[any]int32{"sub0": 0, "sub12": 12} {
		if got, ok := SubtitleOrdinal(map[string]any{"id": id}); !ok || got != want {
			t.Errorf("SubtitleOrdinal(%v) = %d %v, want %d", id, got, ok, want)
		}
	}
	for _, id := range []any{"file0", "sub", "sub-1", "Sub1", "sub1a", "xsub1", 1, nil, "sub12345678"} {
		if _, ok := SubtitleOrdinal(map[string]any{"id": id}); ok {
			t.Errorf("SubtitleOrdinal(%v) took it for a stream", id)
		}
	}
}
