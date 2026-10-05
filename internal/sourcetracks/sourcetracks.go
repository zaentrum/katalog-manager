// Package sourcetracks reads what a package says of its source's tracks: the
// audio and subtitle streams the packager found in the source, as the
// manifest it writes beside the package (and sends with packaging-complete)
// lists them.
//
//   - renditions.audio has one entry per audio stream of the source, in
//     ffprobe's order: an entry's ordinal is its idx, the stream's place among
//     the source's audio streams (its place in the list when it has none). The
//     file the packager packages carries every audio stream of the source, so
//     the two count alike.
//   - subtitles has one entry per subtitle stream the packager extracted, id
//     sub<N>, N its place among the subtitle streams of the file it packaged.
//     That is the source's ordinal when the file carries every subtitle stream
//     of the source: the source itself, or an encode of one whose subtitles
//     all stream-copy into Matroska (any Matroska source's). An encode leaves
//     out a stream it cannot copy (mov_text), and the streams after it count
//     one lower there than in the source. An entry that names its ordinal
//     among the source's streams (ordinal) is at that ordinal, whatever its
//     id. An entry marked external is no stream of the source: the
//     packager's rendition of a subtitle file beside it (the worker record's
//     subtitleFiles), whose id counts on from the streams'.
//
// A kind the manifest does not list at all is not reported: a manifest without
// renditions.audio says nothing of the source's audio, one with an empty list
// that it has none.
package sourcetracks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
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
// position-th of its list: the ordinal it names, else its idx, each a whole
// number of 0 or more, else its position. ok is false for an ordinal or an idx
// that is no such number.
func AudioOrdinal(rendition map[string]any, position int) (int32, bool) {
	for _, key := range []string{"ordinal", "idx"} {
		if raw, present := rendition[key]; present && raw != nil {
			return ordinalOf(raw)
		}
	}
	return int32(position), position >= 0
}

// ordinalOf reads an ordinal: a whole number of 0 or more.
func ordinalOf(raw any) (int32, bool) {
	n, ok := whole(raw)
	if !ok || n < 0 || n > 1<<20 {
		return 0, false
	}
	return int32(n), true
}

var streamID = regexp.MustCompile(`^sub([0-9]{1,6})$`)

// SubtitleOrdinal is the ordinal of a subtitle of a manifest that is one of
// the source's streams: the ordinal it names, else N of its id sub<N>. ok is
// false for a subtitle file's rendition (external) and for any other id.
func SubtitleOrdinal(subtitle map[string]any) (int32, bool) {
	if external, _ := subtitle["external"].(bool); external {
		return 0, false
	}
	if raw, present := subtitle["ordinal"]; present && raw != nil {
		return ordinalOf(raw)
	}
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

// Result is what a Backfill run did: the packaged titles it read the
// manifest of, those whose tracks it recorded and the tracks of each kind,
// and those whose manifest it could not read or record (Errors says why, the
// first maxErrors of them).
type Result struct {
	Titles         int32
	Recorded       int32
	AudioTracks    int32
	SubtitleTracks int32
	Failed         int32
	Errors         []string
}

// maxErrors is how many failures a Result names.
const maxErrors = 20

// maxManifest bounds what Backfill reads of a manifest: a package's is a few
// kilobytes.
const maxManifest = 8 << 20

// Backfill records the tracks of every packaged title from its package's
// manifest on disk, the one its packaged asset names, as packaging-complete
// records them from the manifest the packager sends: for the packages written
// before the catalog kept their tracks. Running it again records what the
// manifests say again. It refuses to run on a catalog without migration 037.
func Backfill(ctx context.Context, st *store.Store) (Result, error) {
	var res Result
	if ready, err := st.TrackLanguagesReady(ctx); err != nil {
		return res, err
	} else if !ready {
		return res, errors.New("the track languages migration (db/migrations/037_track_languages.sql) is not applied")
	}
	rows, err := st.Pool().Query(ctx, `SELECT DISTINCT ON (a.item_id) a.item_id, a.path
		FROM com_nalet_katalog_playbackassets a JOIN com_nalet_katalog_items i ON i.id = a.item_id
		WHERE a.kind = 'packaged' ORDER BY a.item_id, a.path`)
	if err != nil {
		return res, err
	}
	type pkg struct{ item, manifest string }
	var pkgs []pkg
	for rows.Next() {
		var p pkg
		if err := rows.Scan(&p.item, &p.manifest); err != nil {
			rows.Close()
			return res, err
		}
		pkgs = append(pkgs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}

	fail := func(item string, err error) {
		res.Failed++
		if len(res.Errors) < maxErrors {
			res.Errors = append(res.Errors, item+": "+err.Error())
		}
	}
	for _, p := range pkgs {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Titles++
		manifest, err := readManifest(p.manifest)
		if err != nil {
			fail(p.item, err)
			continue
		}
		tracks := FromManifest(manifest)
		if len(tracks) == 0 {
			fail(p.item, fmt.Errorf("its manifest %s lists no tracks", p.manifest))
			continue
		}
		if _, err := st.RecordSourceTracks(ctx, p.item, tracks); err != nil {
			fail(p.item, err)
			continue
		}
		res.Recorded++
		res.AudioTracks += int32(len(tracks[model.TrackAudio]))
		res.SubtitleTracks += int32(len(tracks[model.TrackSubtitle]))
	}
	return res, nil
}

// readManifest reads a package's manifest as packaging-complete decodes the
// one it is sent.
func readManifest(path string) (map[string]any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("its packaged asset names no manifest")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read its manifest: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxManifest))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("its manifest %s is no manifest: %w", path, err)
	}
	if m == nil {
		return nil, fmt.Errorf("its manifest %s is no manifest: null", path)
	}
	return m, nil
}
