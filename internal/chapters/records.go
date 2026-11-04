package chapters

// A title's source record: sources/<sourceId>/ffprobe.json beside its
// source.json, ffprobe's whole probe of the original as the library recorded
// it (-show_format -show_streams -show_chapters), the chapters among it. It is
// a small JSON file, never media, and it stays when the original is retired.
// The catalog reads its chapters as the analyzer reads a header: the marks in
// their order, each from its start to its end (ffprobe probed the streams, so
// a mark the file gives no end has the one ffprobe worked out), a mark
// without a length left out. The record settles a title without a read of
// the media, and is the other reader a read of the header that finds no
// chapter is held against.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// ProbeFile is the source record's probe, beside its source.json.
const ProbeFile = "ffprobe.json"

// maxProbe is the most of a probe the catalog reads: one larger is no record
// it reads, said so.
const maxProbe = 16 << 20

// RecordRead is what a source record says of its original's chapters: the
// chapter marks its probe holds (Found) and those with a length (Chapters),
// what it left out (Note); or that it can't say, and why (Why): there is no
// probe, it holds no chapters list, it is no probe, or it could not be read
// (Err, an error of the storage).
type RecordRead struct {
	Path     string
	Found    int32
	Chapters []Chapter
	Note     string
	Why      string
	Err      error
}

// Says reports whether the record says what its original's chapters are.
func (r RecordRead) Says() bool { return r.Why == "" && r.Err == nil }

// recordPath is the probe of the source record of the item itemID's source
// sourceID, in the library p.
func recordPath(ctx context.Context, q library.Querier, p library.Paths, itemID, sourceID string) (string, error) {
	if !library.ValidID(sourceID) {
		return "", fmt.Errorf("no source record of %q: a source is named by a lower-case UUID", sourceID)
	}
	pl, err := library.PlaceOf(ctx, q, itemID)
	if err != nil {
		return "", err
	}
	return filepath.Join(library.SourceDir(p.ItemDir(pl), sourceID), ProbeFile), nil
}

// RecordOf reads what the source record of the item itemID's source sourceID
// says of its chapters (ReadRecord).
func RecordOf(ctx context.Context, q library.Querier, p library.Paths, itemID, sourceID string) RecordRead {
	path, err := recordPath(ctx, q, p, itemID, sourceID)
	if err != nil {
		return RecordRead{Why: "its source record can't be found: " + err.Error()}
	}
	return ReadRecord(path)
}

// ReadRecord reads the chapters of the probe at path.
func ReadRecord(path string) RecordRead {
	r := RecordRead{Path: path}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		r.Why = "its source record has no probe (" + ProbeFile + ")"
		return r
	}
	if err != nil {
		r.Err = err
		return r
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxProbe+1))
	if err != nil {
		r.Err = err
		return r
	}
	if len(b) > maxProbe {
		r.Why = fmt.Sprintf("its source record's probe is larger than %d MiB", maxProbe>>20)
		return r
	}
	var probe struct {
		Chapters *[]struct {
			Start json.RawMessage `json:"start_time"`
			End   json.RawMessage `json:"end_time"`
			Tags  map[string]any  `json:"tags"`
		} `json:"chapters"`
		Format struct {
			Duration  json.RawMessage `json:"duration"`
			StartTime json.RawMessage `json:"start_time"`
		} `json:"format"`
		Streams []struct {
			Duration json.RawMessage `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		r.Why = "its source record's probe is no JSON object ffprobe writes: " + err.Error()
		return r
	}
	if probe.Chapters == nil {
		r.Why = "its source record's probe holds no chapters list"
		return r
	}
	marks := make([]mark, len(*probe.Chapters))
	for i, c := range *probe.Chapters {
		marks[i] = mark{start: ms(c.Start), end: ms(c.End)}
		if t, ok := c.Tags["title"].(string); ok {
			marks[i].title = &t
		}
	}
	end := ms(probe.Format.Duration)
	if end != nil && *end > 0 {
		if st := ms(probe.Format.StartTime); st != nil && *st > 0 {
			v := *end + *st
			end = &v
		}
	} else {
		end = nil
		for _, s := range probe.Streams {
			if d := ms(s.Duration); d != nil && *d > 0 && (end == nil || *d > *end) {
				end = d
			}
		}
	}
	r.Found = int32(len(marks))
	r.Chapters = chaptersOf(marks, end)
	if left := len(marks) - len(r.Chapters); left > 0 {
		r.Note = fmt.Sprintf("%d of %d chapter marks have no length, left out", left, len(marks))
	}
	return r
}

// mark is a chapter mark as a probe gives it: its start and end in ms (nil
// where it gives none) and its title.
type mark struct {
	start, end *int64
	title      *string
}

// chaptersOf are the chapters of marks with a length, in their order, each
// numbered: a mark without an end ends where the next later mark starts, else
// at end (the file's end), a start before 0 is 0, and a mark that still has
// no length is left out. The analyzer reads a header so.
func chaptersOf(marks []mark, end *int64) []Chapter {
	out := []Chapter{}
	for i, m := range marks {
		if m.start == nil {
			continue
		}
		start := *m.start
		var stop *int64
		if m.end != nil {
			stop = m.end
		} else {
			for j, n := range marks {
				if j != i && n.start != nil && *n.start > start && (stop == nil || *n.start < *stop) {
					v := *n.start
					stop = &v
				}
			}
			if end != nil && (stop == nil || *end < *stop) {
				stop = end
			}
		}
		start = max(start, 0)
		if stop == nil || *stop <= start {
			continue
		}
		out = append(out, Chapter{StartMs: start, EndMs: *stop, Title: CleanTitle(m.title), Ordinal: len(out) + 1})
	}
	return out
}

// ms is a probe's time in seconds ("12.345000", or a number) in ms, as the
// analyzer takes it (truncated); nil for none, "N/A" or one that is no time.
func ms(raw json.RawMessage) *int64 {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if raw[0] == '"' {
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
	} else {
		s = string(raw)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e12 {
		return nil
	}
	v := int64(f * 1000)
	return &v
}

// same reports whether the chapters in the catalog, have, are those of the
// record, rec: as many, each starting within a millisecond of the record's and
// of its title, as the catalog keeps a title.
func same(have, rec []Chapter) bool {
	if len(have) != len(rec) {
		return false
	}
	for i := range have {
		if d := have[i].StartMs - rec[i].StartMs; d < -1 || d > 1 {
			return false
		}
		a, b := CleanTitle(have[i].Title), CleanTitle(rec[i].Title)
		if (a == nil) != (b == nil) || (a != nil && *a != *b) {
			return false
		}
	}
	return true
}
