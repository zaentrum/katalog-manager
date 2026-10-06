package library

import (
	"encoding/json"
	"strconv"
	"strings"
)

// PackagedRow is what the catalog's packaged playback row says of a package,
// from its record, package.json, as packaging-complete writes it and the
// migration's adopt does (platform-library/1, 2.5): the codec and resolution
// of its top video rendition, its peak bandwidth in kbit/s, its size, the
// default audio rendition's codec, language, channels and bitrate (the first
// one's without a default), how many audio and subtitle renditions it has,
// and its duration.
type PackagedRow struct {
	Codec, Resolution                  *string
	BitrateKbps, SizeBytes, DurationMs *int64
	AudioCodec, AudioLanguage          *string
	AudioChannels, AudioBitrateKbps    *int
	AudioTracks, SubtitleTracks        int
}

// PackagedRowOf reads the packaged row of the package pkg, package.json
// decoded with its numbers as json.Number.
func PackagedRowOf(pkg map[string]any) PackagedRow {
	var r PackagedRow
	ren := jsonMap(pkg["renditions"])
	video, audio := jsonMaps(ren["video"]), jsonMaps(ren["audio"])
	if len(video) > 0 {
		r.Codec = jsonString(video[0]["codec"])
		if wd, ok := jsonInt(video[0]["width"]); ok {
			if ht, ok := jsonInt(video[0]["height"]); ok {
				s := strconv.FormatInt(wd, 10) + "x" + strconv.FormatInt(ht, 10)
				r.Resolution = &s
			}
		}
	}
	if peak, ok := jsonInt(pkg["peakBandwidthBps"]); ok && peak > 0 {
		k := peak / 1000
		r.BitrateKbps = &k
	}
	if n, ok := jsonInt(pkg["sizeBytes"]); ok {
		r.SizeBytes = &n
	}
	if d, ok := jsonInt(pkg["durationMs"]); ok {
		r.DurationMs = &d
	}
	var primary map[string]any
	for _, a := range audio {
		if def, _ := a["default"].(bool); def {
			primary = a
			break
		}
	}
	if primary == nil && len(audio) > 0 {
		primary = audio[0]
	}
	if primary != nil {
		r.AudioCodec, r.AudioLanguage = jsonString(primary["codec"]), jsonString(primary["language"])
		if ch, ok := jsonInt(primary["channels"]); ok {
			c := int(ch)
			r.AudioChannels = &c
		}
		if bps, ok := jsonInt(primary["bitrateBps"]); ok && bps > 0 {
			k := int(bps / 1000)
			r.AudioBitrateKbps = &k
		}
	}
	r.AudioTracks, r.SubtitleTracks = len(audio), len(jsonMaps(pkg["subtitles"]))
	return r
}

func jsonMap(o any) map[string]any {
	if m, ok := o.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func jsonMaps(o any) []map[string]any {
	list, _ := o.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// jsonString is a JSON value as text: a string, a number as written, a
// boolean; nil for anything else.
func jsonString(o any) *string {
	var s string
	switch v := o.(type) {
	case string:
		s = v
	case json.Number:
		s = v.String()
	case bool:
		s = strconv.FormatBool(v)
	default:
		return nil
	}
	return &s
}

// jsonInt is a JSON value as an integer: a number (a fraction cut off), or a
// string of an integer.
func jsonInt(o any) (int64, bool) {
	switch v := o.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
		f, err := v.Float64()
		return int64(f), err == nil
	case float64:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	}
	return 0, false
}
