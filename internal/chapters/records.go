package chapters

// A title's source record: sources/<sourceId>/ffprobe.json beside its
// source.json, ffprobe's whole probe of the original as the library recorded
// it (-show_format -show_streams -show_chapters), the chapters among it. It is
// a small JSON file, never media, and it stays when the original is retired.
// A record is taken only when it is the one its source.json names (the probe's
// sha256) and of the original the catalog holds (its size, as source.json, the
// probe and the catalog give it). The catalog reads its chapters as the
// analyzer reads a header: the marks in their order, each from its start to
// its end (ffprobe probed the streams, so a mark the file gives no end has the
// one ffprobe worked out), a mark without a length left out. Marks that all
// lie past the file's end, as a broadcast's from 01:00:00 do, are read an hour
// earlier when every one of them fits the file so, in order; otherwise the
// record holds the title for an admin, and is never taken for one without
// chapters. The record settles a title without a read of the media, and is
// the other reader a read of the header that finds no chapter is held
// against.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ProbeFile is the source record's probe, beside its source.json.
const ProbeFile = "ffprobe.json"

// maxProbe is the most of a record file the catalog reads: one larger is no
// record it reads, said so.
const maxProbe = 16 << 20

// hour is how far a broadcast's marks lie past their places (from
// 01:00:00).
const hour = int64(3600_000)

// RecordRead is what a source record says of its original's chapters: the
// chapter marks its probe holds (Found) and those with a length (Chapters),
// what it left out or how it read them (Note); or that it can't say, and why
// (Why): there is no probe, it holds no chapters list, it is no probe, it is
// not the probe its source.json names or not of the original the catalog
// holds, or it could not be read (Err, an error of the storage); or that an
// admin decides (Held): its marks lie past the file's end and do not fit it
// an hour earlier.
type RecordRead struct {
	Path     string
	Found    int32
	Chapters []Chapter
	Note     string
	Held     string
	Why      string
	Err      error
}

// Says reports whether the record says what its original's chapters are.
func (r RecordRead) Says() bool { return r.Why == "" && r.Err == nil && r.Held == "" }

// RecordOf reads what the source record of the item itemID's source sourceID
// says of its chapters (ReadRecord), held against the catalog's size of that
// original. Its files are read in a goroutine of their own, waited for
// FileWait at most (read): a storage that does not answer in time is an Err
// (ErrStorageSlow, ErrStorageBusy), which says nothing of the record.
func RecordOf(ctx context.Context, q library.Querier, p library.Paths, itemID, sourceID string) RecordRead {
	return recordOf(ctx, q, p, itemID, sourceID, nil, time.Time{})
}

// recordOf is RecordOf, its files read as one of the reads rs of a claim
// (nil: of none), never past until (the zero time: no limit).
func recordOf(ctx context.Context, q library.Querier, p library.Paths, itemID, sourceID string, rs *reads,
	until time.Time) RecordRead {
	if !library.ValidID(sourceID) {
		return RecordRead{Why: "its source record can't be found: a source is named by a lower-case UUID"}
	}
	pl, err := library.PlaceOf(ctx, q, itemID)
	if err != nil {
		return RecordRead{Why: "its source record can't be found: " + err.Error()}
	}
	var size *int64
	if err := q.QueryRow(ctx, `SELECT sizebytes FROM com_nalet_katalog_itemsources WHERE id = $1 AND item_id = $2`,
		sourceID, itemID).Scan(&size); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RecordRead{Err: err}
	}
	dir := library.SourceDir(p.ItemDir(pl), sourceID)
	rec, err := read(ctx, rs, until, func() RecordRead { return ReadRecord(dir, size) })
	if err != nil {
		return RecordRead{Path: filepath.Join(dir, ProbeFile), Err: err}
	}
	return rec
}

// ReadRecord reads the chapters of the source record in dir, the original's
// size the catalog holds size (nil: unknown).
func ReadRecord(dir string, size *int64) RecordRead {
	path := filepath.Join(dir, ProbeFile)
	probe, why, err := readFile(path)
	if err != nil || why != "" {
		if why == "absent" {
			why = "its source record has no probe (" + ProbeFile + ")"
		}
		return RecordRead{Path: path, Why: why, Err: err}
	}
	src, why, err := readFile(filepath.Join(dir, "source.json"))
	if err != nil || why != "" {
		if why == "absent" {
			why = "its source record has no source.json"
		}
		return RecordRead{Path: path, Why: why, Err: err}
	}
	r := interpret(probe)
	r.Path = path
	if r.Why == "" {
		r.Why = verify(probe, src, size)
	}
	return r
}

// readFile reads a record's file, at most maxProbe of it; why is "absent" for
// none there, and says why of one that is no record.
func readFile(path string) ([]byte, string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "absent", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxProbe+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxProbe {
		return nil, fmt.Sprintf("its source record's %s is larger than %d MiB", filepath.Base(path), maxProbe>>20), nil
	}
	return b, "", nil
}

// verify says why the probe can't be taken for the original of size the
// catalog holds, by its source.json src: "" when it can. It must be the probe
// src names (its sha256, of the bytes as written), of a file of the size src
// gives, and that the probe gives where it does, and the catalog where it
// knows one.
func verify(probe, src []byte, size *int64) string {
	var rec struct {
		File struct {
			SizeBytes *float64 `json:"sizeBytes"`
		} `json:"file"`
		Probe struct {
			SHA256 *string `json:"sha256"`
		} `json:"probe"`
	}
	if err := json.Unmarshal(src, &rec); err != nil {
		return "its source.json is no record: " + err.Error()
	}
	sum := sha256.Sum256(probe)
	if got := "sha256:" + hex.EncodeToString(sum[:]); rec.Probe.SHA256 == nil || *rec.Probe.SHA256 != got {
		return "its probe is not the one its source.json names (" + got + ")"
	}
	if rec.File.SizeBytes == nil {
		return "its source.json names no size of the original"
	}
	recorded := int64(*rec.File.SizeBytes)
	if size != nil && *size != recorded {
		return fmt.Sprintf("its source.json is of a file of %d bytes, the catalog's original of %d", recorded, *size)
	}
	var p struct {
		Format struct {
			Size json.RawMessage `json:"size"`
		} `json:"format"`
	}
	if json.Unmarshal(probe, &p) == nil {
		if s := bytes.Trim(bytes.TrimSpace(p.Format.Size), `"`); len(s) > 0 {
			if n, err := strconv.ParseInt(string(s), 10, 64); err == nil && n != recorded {
				return fmt.Sprintf("its probe is of a file of %d bytes, its source.json of %d", n, recorded)
			}
		}
	}
	return ""
}

// interpret reads the chapters of a probe (ffprobe's JSON).
func interpret(b []byte) RecordRead {
	var r RecordRead
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
	if len(r.Chapters) == 0 && len(marks) > 0 {
		switch {
		case end == nil:
			r.Held = fmt.Sprintf("its %d chapter marks have no length, and its probe gives no length of the file: "+
				"an admin decides", len(marks))
			return r
		case pastEnd(marks, *end):
			if chs, ok := shifted(marks, *end); ok {
				r.Chapters = chs
				r.Note = fmt.Sprintf("its %d chapter marks lay an hour past their places: read 3600 s earlier", len(marks))
				return r
			}
			r.Held = fmt.Sprintf("its %d chapter marks lie past the file's end, and do not fit it an hour earlier: "+
				"an admin decides", len(marks))
			return r
		}
	}
	if left := len(marks) - len(r.Chapters); left > 0 {
		r.Note = fmt.Sprintf("%d of %d chapter marks have no length, left out", left, len(marks))
	}
	return r
}

// pastEnd reports whether a mark starts at or after the file's end.
func pastEnd(marks []mark, end int64) bool {
	for _, m := range marks {
		if m.start != nil && *m.start >= end {
			return true
		}
	}
	return false
}

// shifted are the chapters of marks an hour earlier, each ending where the
// next starts, the last at the file's end: when, and only when, every mark so
// lies inside the file and they stay in order.
func shifted(marks []mark, end int64) ([]Chapter, bool) {
	out := make([]mark, len(marks))
	prev := int64(-1)
	for i, m := range marks {
		if m.start == nil {
			return nil, false
		}
		s := *m.start - hour
		if s < 0 || s >= end || s <= prev {
			return nil, false
		}
		prev = s
		out[i] = mark{start: &s, title: m.title}
	}
	chs := chaptersOf(out, &end)
	if len(chs) != len(marks) {
		return nil, false
	}
	return chs, true
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

// same reports whether the chapters in the catalog, have, are those of a read,
// got: as many, each starting within a millisecond of the read's and of its
// title, as the catalog keeps a title.
func same(have, got []Chapter) bool {
	if len(have) != len(got) {
		return false
	}
	for i := range have {
		if d := have[i].StartMs - got[i].StartMs; d < -1 || d > 1 {
			return false
		}
		a, b := CleanTitle(have[i].Title), CleanTitle(got[i].Title)
		if (a == nil) != (b == nil) || (a != nil && *a != *b) {
			return false
		}
	}
	return true
}

// OfSource reports whether the chapters the catalog holds of the title itemID
// are of its original sourceID: read of it (the reads' rowssourceid), or of
// no original known (chapters written before reads were kept, or by a writer
// that keeps none) while the title has had no other original. A version made
// of that original carries them; one of another original, a title given
// another file, carries the chapters its file holds instead. A catalog
// without migration 055 tells the second alone.
func OfSource(ctx context.Context, q library.Querier, itemID, sourceID string) (bool, error) {
	var ready bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('`+store.ChapterReadsTable+`') IS NOT NULL`).Scan(&ready); err != nil {
		return false, err
	}
	if ready {
		var rows *string
		switch err := q.QueryRow(ctx, `SELECT rowssourceid FROM `+store.ChapterReadsTable+` WHERE item_id = $1`, itemID).
			Scan(&rows); {
		case err == nil && rows != nil:
			return *rows == sourceID, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return false, err
		}
	}
	var other bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources WHERE item_id = $1 AND id <> $2)`,
		itemID, sourceID).Scan(&other)
	return !other, err
}
