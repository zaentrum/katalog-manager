package chapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// night is inside the backfill's default window (22:00-17:00 UTC).
var night = time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)

// uid is a title's or an original's id, a lower-case UUID as the library
// names folders by: kind a hex digit, a for a movie, b an original, c a
// retired title, d a version, e an original of no title, f a series.
func uid(kind byte, n int) string {
	return fmt.Sprintf("%c%07d-0000-4000-8000-%012d", kind, n, n)
}

// lib is a catalog's titles with their originals and source records on disk,
// under one library root.
type lib struct {
	t     *testing.T
	st    *store.Store
	root  string
	paths library.Paths
}

func newLib(t *testing.T) lib {
	root := t.TempDir()
	return lib{t, storetest.Open(t), root, library.PathsOf(config.Config{LibraryRoot: root})}
}

func (l lib) cfg() config.Config {
	return config.Config{LibraryRoot: l.root, PackagesRoot: filepath.Join(l.root, "packages")}
}

// opts says how a title is made: whether the compliance check's record of its
// original says it has chapters (chapters, unless records says otherwise:
// "none" no compliance row, "null" records unread, "odd" no boolean, "other"
// of another original), whether its original is gone from disk.
type opts struct {
	chapters bool
	records  string
	gone     bool
}

// title makes the movie id with its original source as its primary asset,
// on disk unless gone, and the compliance check's record of it; it returns
// the original's path.
func (l lib) title(id, source string, o opts) string {
	l.t.Helper()
	storetest.AddItem(l.t, l.st, id, "movie", "Title "+id[:8], "")
	path := filepath.Join(l.root, "incoming", id, "original.mkv")
	if !o.gone {
		l.file(path, "a film")
	}
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ($1, $2, 'original.mkv', 6, 'present')`, source, id)
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
		VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', $3)`, id, path, source)
	key, rec := "records/2 s:"+source+"@1.5 ", fmt.Sprintf(`{"srcEssence": {"chapters": %t}}`, o.chapters)
	switch o.records {
	case "none":
		return path
	case "null":
		rec = ""
	case "odd":
		rec = `{"srcEssence": {"chapters": "yes"}}`
	case "other":
		key = "records/2 s:" + uid('e', 9) + "@1.5 "
	}
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, ruleshash, recordkey, records)
		VALUES ($1, 'movie', 'h', $2, NULLIF($3, '')::jsonb)`, id, key, rec)
	return path
}

func (l lib) file(path, content string) {
	l.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

// record writes the source record of the movie id's original source: its
// probe, with the chapter marks given ([start, end, title] each, in seconds;
// an end below 0 is none) and the file's duration, and its source.json, which
// names that probe (its sha256) and an original of 6 bytes (the catalog's).
func (l lib) record(id, source string, duration float64, marks ...[3]any) string {
	l.t.Helper()
	chs := []string{}
	for _, m := range marks {
		end := ""
		if e, _ := m[1].(float64); e >= 0 {
			end = fmt.Sprintf(`, "end_time": "%f"`, e)
		}
		chs = append(chs, fmt.Sprintf(`{"start_time": "%f"%s, "tags": {"title": %q}}`, m[0], end, m[2]))
	}
	path := filepath.Join(l.recordDir(id, source), ProbeFile)
	probe := fmt.Sprintf(`{"streams": [{"codec_type": "video"}], "chapters": [%s], "format": {"duration": "%f"}}`,
		strings.Join(chs, ", "), duration)
	l.file(path, probe)
	l.sourceJSON(id, source, sha(probe), 6)
	return path
}

// recordDir is the folder of the source record of the movie id's source.
func (l lib) recordDir(id, source string) string {
	return library.SourceDir(l.paths.MovieDir(id), source)
}

// sourceJSON writes the source.json of the movie id's original source: it
// names the probe of the sha256 sum, of an original of size bytes.
func (l lib) sourceJSON(id, source, sum string, size int64) {
	l.t.Helper()
	l.file(filepath.Join(l.recordDir(id, source), "source.json"),
		fmt.Sprintf(`{"file": {"sizeBytes": %d}, "probe": {"sha256": %q}}`, size, sum))
}

// sha is the sha256 a source.json names the probe s by.
func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// packaged gives the item a complete version made of its source.
func (l lib) packaged(id, source string) {
	l.t.Helper()
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir, sourceids)
		VALUES ('d' || substring($1::varchar from 2), $1, 'complete', $2, ARRAY[$3]::varchar[])`, id,
		filepath.Join(l.root, "versions", id), source)
}

// rows writes chapter rows of the item as a read before the reads were kept
// did.
func (l lib) rows(id string, chs ...Chapter) {
	l.t.Helper()
	for _, c := range chs {
		storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemchapters (id, item_id, startms, endms, title, ordinal)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5)`, id, c.StartMs, c.EndMs, c.Title, c.Ordinal)
	}
}

func (l lib) count(id string) int {
	return storetest.Count(l.t, l.st, `SELECT count(*) FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, id)
}

// chapters are the title's chapters in the catalog.
func (l lib) chapters(id string) []Chapter {
	l.t.Helper()
	chs, err := Of(context.Background(), l.st.Pool(), id)
	if err != nil {
		l.t.Fatal(err)
	}
	return chs
}

// read is the title's last read.
func (l lib) read(id string) *Read {
	l.t.Helper()
	r, err := ReadOf(context.Background(), l.st.Pool(), id)
	if err != nil || r == nil {
		l.t.Fatalf("the read of %s: %+v, %v", id[:8], r, err)
	}
	return r
}

// replace makes source, a new original of 6 bytes, the movie id's current
// one (its primary asset's); its first one, old, retired.
func (l lib) replace(id, old, source string) {
	l.t.Helper()
	storetest.Exec(l.t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now() WHERE id = $1`,
		old)
	storetest.Exec(l.t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ($1, $2, 'original.mkv', 6, 'present')`, source, id)
	storetest.Exec(l.t, l.st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $1 WHERE item_id = $2`, source, id)
}

// undo is the chapters a read replaced, as the read row keeps them.
type undo struct {
	By       string    `json:"by"`
	SourceID *string   `json:"sourceId"`
	Chapters []Chapter `json:"chapters"`
}

func undoOf(t *testing.T, r *Read) undo {
	t.Helper()
	var u undo
	if err := json.Unmarshal(r.Replaced, &u); err != nil {
		t.Fatalf("the replaced chapters of %s: %s, %v", r.ItemID[:8], r.Replaced, err)
	}
	return u
}

func setting(t *testing.T, st *store.Store, key, value string) {
	t.Helper()
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings WHERE key = $1`, key)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, $1, $2)`,
		key, value)
}

func three() []Chapter {
	return []Chapter{{StartMs: 0, EndMs: 60_000, Title: str("Opening"), Ordinal: 1},
		{StartMs: 60_000, EndMs: 1_200_000, Title: str("Act 1"), Ordinal: 2},
		{StartMs: 1_200_000, EndMs: 1_300_000, Title: nil, Ordinal: 3}}
}

// threeMarks are the record's marks of three(), in seconds.
func threeMarks() [][3]any {
	return [][3]any{{0.0, 60.0, "Opening"}, {60.0, 1200.0, "Act 1"}, {1200.0, 1300.0, ""}}
}

func header(path string, chs []Chapter, marks int32) Report {
	return Report{Read: ReadReport{Path: path, Found: &marks, Probe: "ffprobe version 4.4.2", Seconds: f64(0.4)},
		Chapters: chs}
}

func failed(path, why string, transient bool) Report {
	return Report{Read: ReadReport{Path: path, Error: why, Transient: transient}}
}

func f64(v float64) *float64 { return &v }

func needs(t *testing.T, st *store.Store, id string) bool {
	t.Helper()
	n, err := NeedsOriginal(context.Background(), st.Pool(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (l lib) record_(id string, r Report, now time.Time) *Read {
	l.t.Helper()
	got, err := Record(context.Background(), l.st, l.paths, id, r, now)
	if err != nil {
		l.t.Fatal(err)
	}
	return got
}

func service(l lib, now *time.Time) *Service {
	return New(l.st, l.cfg()).WithClock(func() time.Time { return *now })
}

// A read of the header that found chapters writes them and settles the title;
// one of its original that finds none later leaves them (an empty read never
// replaces chapters read before of the same original).
func TestAReadWritesTheChaptersAndAnEmptyOneLeavesThem(t *testing.T) {
	l := newLib(t)
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	if !needs(t, l.st, m1) {
		t.Fatal("a title whose original has chapters and none in the catalog: it needs its original")
	}
	r := l.record_(m1, header(orig, three(), 4), night)
	if r.State != StateRead || deref(r.ReadFrom) != ReadFromHeader || deref(r.SourceID) != s1 || *r.Found != 4 ||
		*r.Written != 3 || *r.FileSize != 6 || !r.NextAttemptAt.Equal(night.Add(SettledWait)) || l.count(m1) != 3 {
		t.Fatalf("the read: %+v", r)
	}
	if needs(t, l.st, m1) {
		t.Error("needs its original once its chapters are in the catalog")
	}
	// the header read finds none now (a read error the reader did not see)
	r = l.record_(m1, header(orig, []Chapter{}, 0), night.Add(time.Hour))
	if r.State != StateRead || l.count(m1) != 3 || !strings.Contains(deref(r.Reason), "stay") {
		t.Fatalf("an empty read after chapters were read of the original: %+v, %d chapters", r, l.count(m1))
	}
}

// A read of the header that found none is held against the original's record:
// where the record holds chapters they are written; where its marks have no
// length the none is confirmed; where the record can't be read the compliance
// check's word decides (no chapters: confirmed; chapters: a failure, nothing
// deleted); where nothing can say, the none settles nothing.
func TestAnEmptyReadIsHeldAgainstTheRecord(t *testing.T) {
	l := newLib(t)
	at := night

	// its record holds three chapters: they are written, read from it
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	r := l.record_(m1, header(orig, []Chapter{}, 0), at)
	if r.State != StateRead || deref(r.ReadFrom) != ReadFromRecord || *r.Written != 3 || l.count(m1) != 3 ||
		!strings.Contains(deref(r.Reason), "its source record holds 3") || needs(t, l.st, m1) {
		t.Fatalf("an empty read whose record holds chapters: %+v, %d chapters", r, l.count(m1))
	}

	// its record's one mark has no length: the none is confirmed
	m2, s2 := uid('a', 2), uid('b', 2)
	orig2 := l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 1418, [3]any{0.0, 0.0, "Part A"})
	r = l.record_(m2, header(orig2, []Chapter{}, 1), at)
	if r.State != StateNone || !r.Confirmed || deref(r.RecordSourceID) != s2 || needs(t, l.st, m2) || l.count(m2) != 0 {
		t.Fatalf("an empty read its record agrees with: %+v, %d chapters", r, l.count(m2))
	}
	// read again while its record can't be read (the compliance check's word
	// is that it has chapter marks): the confirmed none stands
	if err := os.Remove(filepath.Join(l.recordDir(m2, s2), ProbeFile)); err != nil {
		t.Fatal(err)
	}
	if r = l.record_(m2, header(orig2, []Chapter{}, 1), at.Add(time.Hour)); r.State != StateNone || !r.Confirmed ||
		r.Failures != 0 || needs(t, l.st, m2) {
		t.Fatalf("an empty read of a confirmed none whose record can't be read: %+v", r)
	}

	// no record, and the compliance check's says chapters: a failure, the
	// chapters an earlier read wrote stay
	m3, s3 := uid('a', 3), uid('b', 3)
	orig3 := l.title(m3, s3, opts{chapters: true})
	l.rows(m3, three()...)
	r = l.record_(m3, header(orig3, []Chapter{}, 0), at)
	if r.State != StateFailed || r.Failures != 1 || l.count(m3) != 3 || !needs(t, l.st, m3) ||
		!strings.Contains(deref(r.Reason), "says it holds some") {
		t.Fatalf("an empty read the record contradicts: %+v, %d chapters", r, l.count(m3))
	}

	// no record, and the compliance check's says no chapters: confirmed
	m4, s4 := uid('a', 4), uid('b', 4)
	orig4 := l.title(m4, s4, opts{chapters: false})
	if r = l.record_(m4, header(orig4, []Chapter{}, 0), at); r.State != StateNone || !r.Confirmed {
		t.Fatalf("an empty read the compliance record agrees with: %+v", r)
	}

	// nothing can say: settles nothing, and its record is read again
	m5, s5 := uid('a', 5), uid('b', 5)
	orig5 := l.title(m5, s5, opts{records: "none"})
	r = l.record_(m5, header(orig5, []Chapter{}, 0), at)
	if r.State != StateNone || r.Confirmed || r.RecordSourceID != nil || !needs(t, l.st, m5) ||
		!r.NextAttemptAt.Equal(at.Add(MaxBackoff)) {
		t.Fatalf("an empty read nothing confirms: %+v", r)
	}
}

// A failure of the storage or the worker (no file, no answer in time, a lease
// that ended, an original not on disk) counts for nothing but a backoff; a
// read that ran on the file and could not read its chapters counts, once a
// backoff, and the fifth makes the title unreadable, which settles nothing:
// it is handed out no more, and an admin's acceptance settles it.
func TestOnlyAFileThatCantBeReadCountsAndUnreadableSettlesNothing(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	at := night
	for i := 1; i <= 10; i++ {
		r := l.record_(m1, failed(orig, "ffprobe did not answer within 120 s", true), at)
		if r.State != StateFailed || r.Failures != 0 || r.Transients != int32(i) {
			t.Fatalf("transient %d: %+v", i, r)
		}
		at = *r.NextAttemptAt
	}
	waits := []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour}
	for i, wait := range waits {
		r := l.record_(m1, failed(orig, "ffprobe exited 1: Invalid data found when processing input", false), at)
		if r.State != StateFailed || r.Failures != int32(i+1) || r.Transients != 0 || !r.NextAttemptAt.Equal(at.Add(wait)) {
			t.Fatalf("failure %d: %+v", i+1, r)
		}
		// the step's retries of the pass within the backoff: counted once
		if r = l.record_(m1, failed(orig, "again", false), at.Add(time.Minute)); r.Failures != int32(i+1) {
			t.Fatalf("a retry within failure %d's backoff counted: %+v", i+1, r)
		}
		at = at.Add(wait)
	}
	r := l.record_(m1, failed(orig, "exit 1: moov atom not found", false), at)
	if r.State != StateUnreadable || r.Failures != MaxFailures ||
		deref(r.Reason) != "5 reads in a row failed, the last: exit 1: moov atom not found" {
		t.Fatalf("the fifth failure: %+v", r)
	}
	if !needs(t, l.st, m1) {
		t.Fatal("an unreadable title settled itself")
	}
	accepted, refused, err := Accept(ctx, l.st, l.paths, []string{m1, uid('a', 9)}, TakeCatalog, "an-admin", at)
	if err != nil || len(accepted) != 1 || accepted[0] != m1 || len(refused) != 1 ||
		!strings.Contains(refused[0], "no such title") {
		t.Fatalf("accept: %v %v %v", accepted, refused, err)
	}
	if needs(t, l.st, m1) {
		t.Error("an accepted unreadable title still needs its original")
	}
	// a failed title is no unreadable one
	m2, s2 := uid('a', 2), uid('b', 2)
	orig2 := l.title(m2, s2, opts{chapters: true})
	l.record_(m2, failed(orig2, "exit 1", false), at)
	if accepted, refused, _ := Accept(ctx, l.st, l.paths, []string{m2}, TakeCatalog, "an-admin", at); len(accepted) != 0 ||
		!strings.Contains(refused[0], "is failed, neither unreadable nor held") {
		t.Errorf("accepting a failed title: %v %v", accepted, refused)
	}
	// an unreadable title is taken as the catalog holds it, not from its record
	if accepted, refused, _ := Accept(ctx, l.st, l.paths, []string{m1}, TakeRecord, "an-admin", at); len(accepted) != 0 ||
		!strings.Contains(refused[0], "an unreadable title is accepted as the catalog holds it") {
		t.Errorf("accepting an unreadable title from its record: %v %v", accepted, refused)
	}
}

// A report of a claim whose title a newer read settled meanwhile changes
// nothing but its claim: a late report never replaces a newer read, empty or
// not.
func TestALateReportNeverReplacesANewerRead(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	c, err := s.Claim(ctx, "analyzer-0", 1) // its record has no probe: handed out
	if err != nil || len(c.Items) != 1 || c.Settled != 0 {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	job := c.Items[0]
	now = now.Add(time.Minute)
	l.record_(m1, header(orig, three(), 3), now) // the analyzer's pass, meanwhile
	now = now.Add(time.Minute)
	// the claim's own report, late: one chapter, older than the pass's read;
	// then reports naming that claim again, which ended
	for i, late := range [][]Chapter{{{StartMs: 0, EndMs: 5000, Ordinal: 1}}, {{StartMs: 0, EndMs: 5000, Ordinal: 1}}, {}} {
		r, err := s.Record(ctx, m1, Report{Read: ReadReport{Path: job.Path, SourceID: job.SourceID, Found: i32(1)},
			Chapters: late, Token: job.Token})
		if err != nil || r.State != StateRead || l.count(m1) != 3 || r.ClaimedAt != nil || *r.Written != 3 {
			t.Fatalf("late report %d of %d chapters: %+v, %v, %d chapters", i+1, len(late), r, err, l.count(m1))
		}
	}
}

// The view fails closed: a title whose compliance record of its current
// original is missing, unread, no boolean or of another original misses its
// chapters until a read settles it; one whose record of that original says no
// chapters needs nothing.
func TestTheViewFailsClosed(t *testing.T) {
	l := newLib(t)
	for i, tc := range []struct {
		o       opts
		missing bool
	}{
		{opts{chapters: true}, true},
		{opts{records: "none"}, true},
		{opts{records: "null"}, true},
		{opts{records: "odd"}, true},
		{opts{records: "other"}, true},
		{opts{chapters: false}, false},
		{opts{chapters: false, records: "other"}, true},
	} {
		id := uid('a', i+1)
		l.title(id, uid('b', i+1), tc.o)
		if got := needs(t, l.st, id); got != tc.missing {
			t.Errorf("%+v: missing %v, want %v", tc.o, got, tc.missing)
		}
	}
}

// Chapters count as in the catalog only when a read of the title's current
// original wrote them: a title whose original was replaced misses them again,
// and of a title with two originals present the current one is the primary
// asset's, read once.
func TestChaptersAreOfTheCurrentOriginal(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	m1, s1, s2 := uid('a', 1), uid('b', 1), uid('b', 2)
	orig := l.title(m1, s1, opts{chapters: true})
	l.record_(m1, header(orig, three(), 3), night)
	if needs(t, l.st, m1) {
		t.Fatal("read of its original")
	}
	// replaced: a new original, the primary asset's now; the old one present
	storetest.Exec(t, l.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, createdat)
		VALUES ($1, $2, 'original.mkv', 9, 'present', now() - interval '1 day')`, s2, m1)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $1 WHERE item_id = $2`, s2, m1)
	if !needs(t, l.st, m1) {
		t.Fatal("the chapters of the replaced original count for the new one")
	}
	if n := storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_chaptersmissing WHERE item_id = $1`, m1); n != 1 {
		t.Fatalf("a title with two originals present is missing %d times", n)
	}
	if ok, _ := NeedsOriginal(ctx, l.st.Pool(), m1, s2); !ok {
		t.Error("the current original is the primary asset's")
	}
	// a read the claim names the new original of settles it, whatever its
	// path says (one where a run left the original)
	r := l.record_(m1, Report{Read: ReadReport{Path: "/elsewhere/original.mkv", SourceID: s2, Found: i32(3)},
		Chapters: three()}, night.Add(time.Hour))
	if deref(r.SourceID) != s2 || needs(t, l.st, m1) {
		t.Errorf("a read of the new original: %+v", r)
	}
	// one that names no original of the title is of its file's
	if r = l.record_(m1, Report{Read: ReadReport{Path: orig, SourceID: uid('e', 3), Found: i32(3)}, Chapters: three()},
		night.Add(2*time.Hour)); deref(r.SourceID) != s2 {
		t.Errorf("a read naming another title's original: %+v", r)
	}
}

// Each claim first settles the titles whose source record says their chapters
// (from it, no media read): chapters written (those in the catalog kept as
// they are when they are the record's), or a confirmed none; a title whose
// record can't say is handed out to the analyzer. A retired title without
// chapters has them restored from its record; one whose compliance record
// says none is not read.
func TestTheBackfillSettlesFromTheRecordsFirst(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")

	m1, s1 := uid('a', 1), uid('b', 1) // its record holds chapters
	l.title(m1, s1, opts{chapters: true})
	l.packaged(m1, s1)
	l.record(m1, s1, 1300, threeMarks()...)
	m2, s2 := uid('a', 2), uid('b', 2) // its record's marks have no length
	l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 1418, [3]any{0.0, 0.0, "Part A"})
	m3, s3 := uid('a', 3), uid('b', 3) // chapters of an earlier read, the record's
	l.title(m3, s3, opts{chapters: true})
	l.record(m3, s3, 1300, threeMarks()...)
	l.rows(m3, three()...)
	m4, s4 := uid('a', 4), uid('b', 4) // fewer chapters of an earlier read than the record's
	l.title(m4, s4, opts{chapters: true})
	l.record(m4, s4, 1300, threeMarks()...)
	l.rows(m4, Chapter{StartMs: 0, EndMs: 9000, Title: str("Old"), Ordinal: 1})
	m5, s5 := uid('a', 5), uid('b', 5) // no probe in its record: the analyzer's
	l.title(m5, s5, opts{chapters: true})
	r1, sr1 := uid('c', 1), uid('b', 6) // retired, its record holds chapters
	l.title(r1, sr1, opts{chapters: true})
	l.record(r1, sr1, 1300, threeMarks()...)
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now() WHERE id = $1`, sr1)
	r2, sr2 := uid('c', 2), uid('b', 7) // retired, its compliance record says none
	l.title(r2, sr2, opts{chapters: false})
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now() WHERE id = $1`, sr2)
	ids := func(id string) string {
		var s string
		if err := l.st.Pool().QueryRow(ctx, `SELECT string_agg(id, ',' ORDER BY id) FROM com_nalet_katalog_itemchapters
			WHERE item_id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	m3rows := ids(m3)

	c, err := s.Claim(ctx, "analyzer-0", 1)
	if err != nil || c.Settled != 5 || len(c.Items) != 1 || c.Items[0].ID != m5 || c.Items[0].SourceID != s5 {
		t.Fatalf("the first claim: %+v, %v", c, err)
	}
	for _, tc := range []struct {
		id, state, from string
		confirmed       bool
		count           int
	}{
		{m1, StateRead, ReadFromRecord, false, 3},
		{m2, StateNone, ReadFromRecord, true, 0},
		{m3, StateRead, ReadFromRecord, false, 3},
		{m4, StateRead, ReadFromRecord, false, 3},
		{r1, StateRead, ReadFromRecord, false, 3},
	} {
		r, err := ReadOf(ctx, l.st.Pool(), tc.id)
		if err != nil || r == nil || r.State != tc.state || deref(r.ReadFrom) != tc.from || r.Confirmed != tc.confirmed ||
			l.count(tc.id) != tc.count {
			t.Errorf("%s: %+v, %v, %d chapters", tc.id[:8], r, err, l.count(tc.id))
		}
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), m3); !strings.Contains(deref(r.Reason), "are those of its source record") ||
		ids(m3) != m3rows {
		t.Errorf("m3's chapters were written again: %+v", r)
	}
	if r := l.read(m4); !strings.Contains(deref(r.Reason), "the 1 chapters it had before replaced, kept to undo") ||
		len(undoOf(t, r).Chapters) != 1 || deref(r.RowsSourceID) != s4 {
		t.Errorf("m4: %+v", r)
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), r2); r != nil {
		t.Errorf("a retired title whose record says no chapters was read: %+v", r)
	}
	if r, _ := ReadOf(ctx, l.st.Pool(), m5); !strings.Contains(deref(r.RecordNote), "has no probe") {
		t.Errorf("m5's record: %+v", r)
	}
	for _, id := range []string{m1, m2, m3, m4} {
		if needs(t, l.st, id) {
			t.Errorf("%s still misses its chapters", id[:8])
		}
	}
	p, err := s.Progress(ctx)
	if err != nil || p.Missing != 1 || p.FromRecord != 4 || p.None != 1 || p.InFlight != 1 || p.HandedOutLastHour != 1 ||
		p.RecordsReadLastHour != 6 || p.Replaced != 1 || p.Held != 0 {
		t.Errorf("progress: %+v, %v", p, err)
	}
}

// The backfill reads nothing while it is off, nor outside its window (not a
// record either); an original not on disk waits a backoff as no failure of
// its title, and the next goes; a claim whose lease ended waits a backoff as
// no failure, and its run's report, late, ends nothing; a claim made near the
// window's end must be read by it.
func TestTheBackfillsPace(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	gone, sg := uid('a', 1), uid('b', 1)
	l.title(gone, sg, opts{chapters: true, gone: true})
	m2, s2 := uid('a', 2), uid('b', 2)
	l.title(m2, s2, opts{chapters: true})

	if c, err := s.Claim(ctx, "analyzer-0", 1); err != nil || c.Idle != "chapters.backfill is off" || c.RetryAfterSeconds != 900 {
		t.Fatalf("off: %+v, %v", c, err)
	}
	setting(t, l.st, SettingBackfill, "on")
	now = time.Date(2026, 11, 4, 18, 0, 0, 0, time.UTC)
	if c, err := s.Claim(ctx, "analyzer-0", 1); err != nil || c.Idle != "outside chapters.backfill.window (22:00-17:00 UTC)" ||
		c.RetryAfterSeconds != 4*3600 || c.Settled != 0 {
		t.Fatalf("in the quiet hours: %+v, %v", c, err)
	}
	if n := storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_chapterreads WHERE recordat IS NOT NULL`); n != 0 {
		t.Fatalf("%d records read in the quiet hours", n)
	}
	now = night
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 || c.Items[0].ID != m2 || c.Items[0].SourceID != s2 {
		t.Fatalf("m2: %+v, %v", c, err)
	}
	rg, _ := ReadOf(ctx, l.st.Pool(), gone)
	if rg == nil || rg.State != StateFailed || rg.Failures != 0 || rg.Transients != 1 ||
		!strings.HasPrefix(deref(rg.Reason), "nothing to read: its original is not at ") {
		t.Fatalf("an original not on disk: %+v", rg)
	}
	lost := c.Items[0]
	// its lease ends: a backoff, no failure
	now = night.Add(DefaultLease + time.Second)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 0 {
		t.Fatalf("after the lease: %+v, %v", c, err)
	}
	r, _ := ReadOf(ctx, l.st.Pool(), m2)
	if r.State != StateFailed || r.Failures != 0 || r.Transients != 1 || deref(r.LapsedToken) != lost.Token ||
		!r.NextAttemptAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("m2 after its lease: %+v", r)
	}
	if r, err = s.Record(ctx, m2, Report{Read: ReadReport{Path: lost.Path, Error: "killed", Transient: true},
		Token: lost.Token}); err != nil || r.Failures != 0 || r.Transients != 1 || r.LapsedToken != nil ||
		deref(r.Reason) != "killed" {
		t.Errorf("a late failure of a lapsed claim: %+v, %v", r, err)
	}
	// at 16:50 a claim's deadline is the window's end
	now = time.Date(2026, 11, 4, 16, 50, 0, 0, time.UTC)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 ||
		!c.Deadline.Equal(time.Date(2026, 11, 4, 17, 0, 0, 0, time.UTC)) {
		t.Fatalf("near the window's end: %+v, %v", c, err)
	}
	setting(t, l.st, SettingWindow, "after dinner")
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || c.Idle != "chapters.backfill.window is no window" {
		t.Errorf("no window: %+v, %v", c, err)
	}
}

// The hour's rate counts claims, not titles: a title handed out and given
// back twice within the hour reaches a rate of two.
func TestTheRateCountsClaims(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingRate, "2")
	m1, s1 := uid('a', 1), uid('b', 1)
	l.title(m1, s1, opts{chapters: true})
	for i := 0; i < 2; i++ {
		c, err := s.Claim(ctx, "analyzer-0", 1)
		if err != nil || len(c.Items) != 1 {
			t.Fatalf("claim %d: %+v, %v", i+1, c, err)
		}
		if _, err := s.Record(ctx, m1, Report{Released: true, Token: c.Items[0].Token}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(ReleaseWait)
	}
	if c, err := s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 0 ||
		c.Idle != "2 titles were handed out in the last hour (chapters.backfill.rate 2)" {
		t.Fatalf("the hour's rate: %+v, %v", c, err)
	}
}

// Without records read (chapters.backfill.records 0) every title goes to the
// analyzer; an unreadable title is handed out no more.
func TestWithoutRecordsAndAnUnreadableTitle(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingRecords, "0")
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	c, err := s.Claim(ctx, "analyzer-0", 1)
	if err != nil || len(c.Items) != 1 || c.Settled != 0 {
		t.Fatalf("records off: %+v, %v", c, err)
	}
	if _, err := s.Record(ctx, m1, Report{Released: true, Token: c.Items[0].Token}); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_chapterreads SET state = 'unreadable', failures = 5, sourceid = $2,
		nextattemptat = NULL WHERE item_id = $1`, m1, s1)
	now = now.Add(time.Hour)
	if c, err = s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 0 || !strings.Contains(c.Idle, "unreadable") {
		t.Errorf("an unreadable title: %+v, %v", c, err)
	}
	_ = orig
}

// A report that can't be taken says why; one of no such title, or of a
// series, is refused.
func TestAReportThatCantBeTaken(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	storetest.AddItem(t, l.st, uid('f', 1), "series", "A Show", "")
	var bad *BadReport
	if _, err := Record(ctx, l.st, l.paths, uid('f', 1), header("/a.mkv", three(), 3), night); !errors.As(err, &bad) {
		t.Errorf("a series: %v", err)
	}
	if _, err := Record(ctx, l.st, l.paths, uid('a', 9), header(orig, three(), 3), night); !errors.Is(err, ErrNoTitle) {
		t.Errorf("no such title: %v", err)
	}
	if _, err := Record(ctx, l.st, l.paths, m1, Report{Read: ReadReport{Path: orig, Error: "x"}, Chapters: three()},
		night); !errors.As(err, &bad) {
		t.Errorf("a failed read with chapters: %v", err)
	}
	if n := storetest.Count(t, l.st, `SELECT count(*) FROM com_nalet_katalog_chapterreads`); n != 0 {
		t.Errorf("reports refused left %d reads", n)
	}
}

// A deleted title's read and claims go with it; without migration 055 the
// backfill and the reads say so, and NeedsOriginal keeps the original.
func TestADeletedTitleAndNoMigration(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	m1, s1 := uid('a', 1), uid('b', 1)
	l.title(m1, s1, opts{chapters: true})
	if c, err := s.Claim(ctx, "analyzer-0", 1); err != nil || len(c.Items) != 1 {
		t.Fatalf("claim: %+v, %v", c, err)
	}
	if _, err := l.st.DeleteItems(ctx, []string{m1}, store.Deletion{By: "a test"}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{store.ChapterReadsTable, store.ChapterClaimsTable} {
		if n := storetest.Count(t, l.st, `SELECT count(*) FROM `+table+` WHERE item_id = $1`, m1); n != 0 {
			t.Errorf("a deleted title left %d rows in %s", n, table)
		}
	}

	base := storetest.OpenBase(t)
	sb := New(base, config.Config{}).WithClock(func() time.Time { return now })
	if _, err := sb.Claim(ctx, "analyzer-0", 1); !errors.Is(err, ErrNotReady) {
		t.Errorf("a claim: %v", err)
	}
	if _, err := sb.Record(ctx, m1, header("/m.mkv", three(), 3)); !errors.Is(err, ErrNotReady) {
		t.Errorf("a report: %v", err)
	}
	if n, err := NeedsOriginal(ctx, base.Pool(), m1, s1); !n || !errors.Is(err, ErrNotReady) {
		t.Errorf("NeedsOriginal: %v, %v", n, err)
	}
}

// A source record is taken only when it is the one its source.json names and
// of the original the catalog holds: a probe written over since its
// source.json, a source.json of a file of another size than the catalog's
// original, a probe of a file of another size than its source.json's, or no
// source.json can't say. The title goes to the analyzer, its chapters as they
// are, and a header read that finds none is not confirmed by such a record.
func TestARecordIsTakenOnlyOfItsOriginal(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingInflight, "8")

	m1, s1 := uid('a', 1), uid('b', 1) // its probe written over since: no chapters in it now
	l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	l.file(filepath.Join(l.recordDir(m1, s1), ProbeFile), `{"chapters": [], "format": {"duration": "1300.000000"}}`)
	l.rows(m1, Chapter{StartMs: 0, EndMs: 9000, Title: str("Old"), Ordinal: 1})
	m2, s2 := uid('a', 2), uid('b', 2) // its source.json of a file of 7 bytes, the catalog's original of 6
	orig2 := l.title(m2, s2, opts{chapters: true})
	probe, err := os.ReadFile(l.record(m2, s2, 1300, threeMarks()...))
	if err != nil {
		t.Fatal(err)
	}
	l.sourceJSON(m2, s2, sha(string(probe)), 7)
	m3, s3 := uid('a', 3), uid('b', 3) // no source.json
	l.title(m3, s3, opts{chapters: true})
	l.record(m3, s3, 1300, threeMarks()...)
	if err := os.Remove(filepath.Join(l.recordDir(m3, s3), "source.json")); err != nil {
		t.Fatal(err)
	}
	m4, s4 := uid('a', 4), uid('b', 4) // its probe of a file of 7 bytes, its source.json of 6
	l.title(m4, s4, opts{chapters: true})
	p4 := `{"chapters": [{"start_time": "0.000000", "end_time": "60.000000"}], "format": {"duration": "1300.000000", "size": "7"}}`
	l.file(filepath.Join(l.recordDir(m4, s4), ProbeFile), p4)
	l.sourceJSON(m4, s4, sha(p4), 6)

	c, err := s.Claim(ctx, "analyzer-0", 8)
	if err != nil || c.Settled != 0 || len(c.Items) != 4 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	for _, tc := range []struct{ id, source, why string }{
		{m1, s1, "its probe is not the one its source.json names (sha256:"},
		{m2, s2, "its source.json is of a file of 7 bytes, the catalog's original of 6"},
		{m3, s3, "its source record has no source.json"},
		{m4, s4, "its probe is of a file of 7 bytes, its source.json of 6"},
	} {
		if r := l.read(tc.id); !strings.HasPrefix(deref(r.RecordNote), tc.why) || deref(r.RecordSourceID) != tc.source ||
			!needs(t, l.st, tc.id) {
			t.Errorf("%s: %+v", tc.id[:8], r)
		}
	}
	if l.count(m1) != 1 || l.count(m2) != 0 || l.count(m4) != 0 {
		t.Errorf("records that can't say wrote chapters: %d, %d, %d", l.count(m1), l.count(m2), l.count(m4))
	}
	// the analyzer's read of m2's header finds none: its record can't confirm
	// that, and the compliance check's says chapters
	if r := l.record_(m2, header(orig2, []Chapter{}, 0), now); r.State != StateFailed || r.Confirmed || !needs(t, l.st, m2) {
		t.Errorf("an empty read held against a record of another file: %+v", r)
	}
}

// The chapters a read replaces are kept to undo: the record's three in place
// of one written before the reads were kept, the header's four in place of
// the record's three; an admin's Revert puts the replaced ones back as the
// title's, of its original, and keeps the read's to undo in turn.
func TestAReplacementIsKeptToUndo(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	m1, s1 := uid('a', 1), uid('b', 1)
	orig := l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, threeMarks()...)
	l.rows(m1, Chapter{StartMs: 0, EndMs: 9000, Title: str("Old"), Ordinal: 1})
	if c, err := s.Claim(ctx, "analyzer-0", 1); err != nil || c.Settled != 1 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	r := l.read(m1)
	if u := undoOf(t, r); u.By != ReadFromRecord || u.SourceID != nil || len(u.Chapters) != 1 ||
		deref(u.Chapters[0].Title) != "Old" || deref(r.RowsSourceID) != s1 || l.count(m1) != 3 || needs(t, l.st, m1) {
		t.Fatalf("the record's three in place of one: %+v, %+v", r, u)
	}
	four := append(three(), Chapter{StartMs: 1_300_000, EndMs: 1_400_000, Title: str("Credits"), Ordinal: 4})
	r = l.record_(m1, header(orig, four, 4), now.Add(time.Hour))
	if u := undoOf(t, r); u.By != ReadFromHeader || deref(u.SourceID) != s1 || len(u.Chapters) != 3 || l.count(m1) != 4 ||
		!strings.Contains(deref(r.Reason), "the 3 chapters it had before replaced, kept to undo") {
		t.Fatalf("the header's four in place of the record's three: %+v, %+v", r, u)
	}

	reverted, refused, err := Revert(ctx, l.st, []string{m1, uid('a', 9)}, "an-admin", now.Add(2*time.Hour))
	if err != nil || len(reverted) != 1 || reverted[0] != m1 || len(refused) != 1 ||
		!strings.Contains(refused[0], "no read replaced its chapters") {
		t.Fatalf("revert: %v %v %v", reverted, refused, err)
	}
	r = l.read(m1)
	if u := undoOf(t, r); r.State != StateRead || deref(r.ReadFrom) != ReadFromCatalog || deref(r.AcceptedBy) != "an-admin" ||
		deref(r.RowsSourceID) != s1 || l.count(m1) != 3 || len(u.Chapters) != 4 || u.By != "admin" || needs(t, l.st, m1) {
		t.Fatalf("reverted: %+v, %+v, %d chapters", r, u, l.count(m1))
	}
	if reverted, _, err = Revert(ctx, l.st, []string{m1}, "an-admin", now.Add(3*time.Hour)); err != nil ||
		len(reverted) != 1 || l.count(m1) != 4 {
		t.Errorf("reverted again: %v, %v, %d chapters", reverted, err, l.count(m1))
	}
}

// Fewer chapters than the catalog holds of no original known, or than an
// earlier read of the same original wrote, hold the title for an admin, its
// chapters as they are: it still needs its original, and is neither handed
// out nor read from its record again. An admin's Accept takes the catalog's
// (now of its current original) or the record's (those in the catalog kept to
// undo); a record that can't say can't be taken.
func TestFewerChaptersHoldTheTitle(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingInflight, "8")
	one := [3]any{0.0, 60.0, "Opening"}

	m1, s1 := uid('a', 1), uid('b', 1) // three of no original known; its record one
	l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1300, one)
	l.rows(m1, three()...)
	m2, s2 := uid('a', 2), uid('b', 2) // the same
	l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 1300, one)
	l.rows(m2, three()...)
	m3, s3 := uid('a', 3), uid('b', 3) // three of no original known; its header two
	orig3 := l.title(m3, s3, opts{chapters: true})
	l.rows(m3, three()...)
	m4, s4 := uid('a', 4), uid('b', 4) // three of a read of its original; then one
	orig4 := l.title(m4, s4, opts{chapters: true})

	c, err := s.Claim(ctx, "analyzer-0", 8)
	if err != nil || c.Settled != 0 || len(c.Items) != 2 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	l.record_(m3, header(orig3, three()[:2], 2), now)
	l.record_(m4, header(orig4, three(), 3), now)
	l.record_(m4, header(orig4, three()[:1], 1), now.Add(time.Minute))
	for id, why := range map[string]string{
		m1: "its source record: it holds 1 chapters, the catalog 3 of an earlier read: an admin decides",
		m2: "its source record: it holds 1 chapters, the catalog 3 of an earlier read: an admin decides",
		m3: "it holds 2 chapters, the catalog 3 of an earlier read: an admin decides",
		m4: "it holds 1 chapters, the catalog 3 of an earlier read of this original: an admin decides",
	} {
		if r := l.read(id); r.State != StateHeld || deref(r.Reason) != why || l.count(id) != 3 || len(r.Replaced) != 0 ||
			!needs(t, l.st, id) {
			t.Errorf("%s: %+v, %d chapters", id[:8], r, l.count(id))
		}
	}
	// a header read of a held title that fails leaves it held, and counts nothing
	if r := l.record_(m3, failed(orig3, "ffprobe exited 1: Invalid data found when processing input", false),
		now.Add(time.Minute)); r.State != StateHeld || r.Failures != 0 || l.count(m3) != 3 {
		t.Errorf("a failed read of a held title: %+v", r)
	}
	// m5, held by the analyzer's pass before its record was read: its record,
	// of the catalog's three, is not read either
	m5, s5 := uid('a', 5), uid('b', 5)
	orig5 := l.title(m5, s5, opts{chapters: true})
	l.rows(m5, three()...)
	if r := l.record_(m5, header(orig5, three()[:2], 2), now); r.State != StateHeld || r.RecordSourceID != nil {
		t.Fatalf("m5: %+v", r)
	}
	l.record(m5, s5, 1300, threeMarks()...)
	now = now.Add(2 * SettledWait)
	if c, err = s.Claim(ctx, "analyzer-0", 8); err != nil || len(c.Items) != 0 || c.Settled != 0 ||
		!strings.Contains(c.Idle, "held for an admin") || l.read(m5).State != StateHeld {
		t.Fatalf("held titles were handed out or read again: %+v, %v", c, err)
	}
	if p, err := s.Progress(ctx); err != nil || p.Held != 5 || p.Missing != 5 {
		t.Errorf("progress: %+v, %v", p, err)
	}

	accepted, refused, err := Accept(ctx, l.st, l.paths, []string{m1, m3}, TakeCatalog, "an-admin", now)
	if err != nil || len(accepted) != 2 || len(refused) != 0 {
		t.Fatalf("the catalog's: %v %v %v", accepted, refused, err)
	}
	accepted, refused, err = Accept(ctx, l.st, l.paths, []string{m2, m4}, TakeRecord, "an-admin", now)
	if err != nil || len(accepted) != 1 || accepted[0] != m2 || len(refused) != 1 ||
		refused[0] != m4+": its source record can't say: its source record has no probe (ffprobe.json)" {
		t.Fatalf("the record's: %v %v %v", accepted, refused, err)
	}
	if r := l.read(m1); r.State != StateRead || deref(r.ReadFrom) != ReadFromCatalog || deref(r.RowsSourceID) != s1 ||
		deref(r.AcceptedBy) != "an-admin" || l.count(m1) != 3 || len(r.Replaced) != 0 || needs(t, l.st, m1) {
		t.Errorf("m1, the catalog's taken: %+v", r)
	}
	if r := l.read(m2); r.State != StateRead || deref(r.ReadFrom) != ReadFromRecord || l.count(m2) != 1 ||
		len(undoOf(t, r).Chapters) != 3 || needs(t, l.st, m2) {
		t.Errorf("m2, the record's taken: %+v", r)
	}
	if needs(t, l.st, m3) || !needs(t, l.st, m4) || l.read(m4).State != StateHeld {
		t.Errorf("m3 taken, m4 held still: %v %v", needs(t, l.st, m3), needs(t, l.st, m4))
	}
}

// A record never deletes chapters of no original known (written before the
// reads were kept): where it holds no chapter with a length, or no mark at
// all, the title is held for an admin and its chapters stay, whether the
// record is read first or a header read that found none is held against it;
// so too where the compliance check's record says the original holds none.
func TestARecordNeverDeletesChapters(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingInflight, "8")

	m1, s1 := uid('a', 1), uid('b', 1) // its record's one mark has no length
	l.title(m1, s1, opts{chapters: true})
	l.record(m1, s1, 1418, [3]any{0.0, 0.0, "Part A"})
	l.rows(m1, three()...)
	m2, s2 := uid('a', 2), uid('b', 2) // its record holds no mark
	l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 1418)
	l.rows(m2, three()...)
	if c, err := s.Claim(ctx, "analyzer-0", 8); err != nil || c.Settled != 0 || len(c.Items) != 0 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	m3, s3 := uid('a', 3), uid('b', 3) // a header read finds none; its record's one mark has no length
	orig3 := l.title(m3, s3, opts{chapters: true})
	l.record(m3, s3, 1418, [3]any{0.0, 0.0, "Part A"})
	l.rows(m3, three()...)
	l.record_(m3, header(orig3, []Chapter{}, 1), now)
	m4, s4 := uid('a', 4), uid('b', 4) // a header read finds none; no record, the compliance check's says none
	orig4 := l.title(m4, s4, opts{chapters: false})
	l.rows(m4, three()...)
	l.record_(m4, header(orig4, []Chapter{}, 0), now)
	for _, id := range []string{m1, m2, m3, m4} {
		if r := l.read(id); r.State != StateHeld || r.Confirmed || l.count(id) != 3 || len(r.Replaced) != 0 ||
			!strings.Contains(deref(r.Reason), "it holds no chapter with a length, the catalog 3 of an earlier read: "+
				"an admin decides") || !needs(t, l.st, id) {
			t.Errorf("%s: %+v, %d chapters", id[:8], r, l.count(id))
		}
	}
}

// The chapters read of an original that was replaced go with it: the new
// original's record replaces them (kept to undo), a none of it its record or
// the compliance check's confirms takes them away (kept to undo); until a
// read of the new original says, they stand for it in nothing.
func TestAReplacedOriginalsChaptersGoWithIt(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingInflight, "8")
	type title struct{ id, old, cur, orig string }
	replaced := func(n int) title {
		tt := title{uid('a', n), uid('b', n), uid('b', 10+n), ""}
		tt.orig = l.title(tt.id, tt.old, opts{chapters: true})
		l.record_(tt.id, header(tt.orig, three(), 3), now)
		l.replace(tt.id, tt.old, tt.cur)
		if !needs(t, l.st, tt.id) {
			t.Fatalf("%s: the chapters of its replaced original count for the new one", tt.id[:8])
		}
		return tt
	}
	t1 := replaced(1) // the new original's record holds no chapter with a length
	l.record(t1.id, t1.cur, 1418, [3]any{0.0, 0.0, "Part A"})
	t2 := replaced(2) // the new original's record holds two other chapters
	l.record(t2.id, t2.cur, 1300, [3]any{0.0, 30.0, "Cold Open"}, [3]any{30.0, 1300.0, "Episode"})
	t3 := replaced(3) // no record; the compliance check's of the new original says none
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_compliance SET recordkey = $2,
		records = '{"srcEssence": {"chapters": false}}' WHERE item_id = $1`, t3.id, "records/2 s:"+t3.cur+"@1.5 ")
	t4 := replaced(4) // no record; nothing says
	storetest.Exec(t, l.st, `DELETE FROM com_nalet_katalog_compliance WHERE item_id = $1`, t4.id)

	now = now.Add(SettledWait)
	c, err := s.Claim(ctx, "analyzer-0", 8)
	if err != nil || c.Settled != 2 || len(c.Items) != 2 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	for _, job := range c.Items {
		if _, err := s.Record(ctx, job.ID, Report{Read: ReadReport{Path: job.Path, SourceID: job.SourceID, Found: i32(0)},
			Chapters: []Chapter{}, Token: job.Token}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		t       title
		state   string
		count   int
		missing bool
	}{
		{t1, StateNone, 0, false},
		{t2, StateRead, 2, false},
		{t3, StateNone, 0, false},
		{t4, StateNone, 3, true},
	} {
		r := l.read(tc.t.id)
		if r.State != tc.state || deref(r.SourceID) != tc.t.cur || l.count(tc.t.id) != tc.count ||
			needs(t, l.st, tc.t.id) != tc.missing {
			t.Errorf("%s: %+v, %d chapters", tc.t.id[:8], r, l.count(tc.t.id))
			continue
		}
		if tc.missing {
			if r.Confirmed || len(r.Replaced) != 0 || deref(r.RowsSourceID) != tc.t.old {
				t.Errorf("%s: a none nothing confirms took chapters away: %+v", tc.t.id[:8], r)
			}
			continue
		}
		if u := undoOf(t, r); deref(u.SourceID) != tc.t.old || len(u.Chapters) != 3 {
			t.Errorf("%s: the replaced original's chapters kept to undo: %+v", tc.t.id[:8], u)
		}
	}
	// the chapters a confirmed none took away come back, as the title's, of
	// its original; nothing is left to undo
	if reverted, _, err := Revert(ctx, l.st, []string{t1.id}, "an-admin", now); err != nil || len(reverted) != 1 {
		t.Fatalf("revert: %v, %v", reverted, err)
	}
	if r := l.read(t1.id); l.count(t1.id) != 3 || len(r.Replaced) != 0 || deref(r.RowsSourceID) != t1.cur ||
		needs(t, l.st, t1.id) {
		t.Errorf("the replaced original's chapters reverted: %+v, %d chapters", r, l.count(t1.id))
	}
}

// Marks that all lie past the file's end, as a broadcast's from 01:00:00 do,
// are read an hour earlier where every one of them fits the file so, in
// order; otherwise the title is held for an admin, its none never confirmed,
// whether its record is read first or a header read that found none is held
// against it. A single mark of no length at the start is no chapter: a
// confirmed none. An admin's Accept of the record's takes none.
func TestMarksPastTheFilesEnd(t *testing.T) {
	l := newLib(t)
	ctx := context.Background()
	now := night
	s := service(l, &now)
	setting(t, l.st, SettingBackfill, "on")
	setting(t, l.st, SettingInflight, "8")

	m1, s1 := uid('a', 1), uid('b', 1) // six marks from 01:00:00 in a file of 50 minutes
	l.title(m1, s1, opts{chapters: true})
	var six [][3]any
	for i := 0; i < 6; i++ {
		at := 3600.0 + float64(i)*300
		six = append(six, [3]any{at, at, fmt.Sprintf("Chapter %02d", i+1)})
	}
	l.record(m1, s1, 3000, six...)
	m2, s2 := uid('a', 2), uid('b', 2) // one "Credits" past the end that fits an hour earlier
	l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 2000, [3]any{3650.0, 3650.0, "Credits"})
	m3, s3 := uid('a', 3), uid('b', 3) // one "Credits" past the end that does not
	l.title(m3, s3, opts{chapters: true})
	l.record(m3, s3, 1000, [3]any{5000.0, 5000.0, "Credits"})
	m4, s4 := uid('a', 4), uid('b', 4) // one mark of no length at the start
	l.title(m4, s4, opts{chapters: true})
	l.record(m4, s4, 1418, [3]any{0.0, 0.0, "Part A"})
	r1, sr1 := uid('c', 1), uid('b', 6) // retired; its record's marks do not fit: there is nothing to hold
	l.title(r1, sr1, opts{chapters: true})
	l.record(r1, sr1, 1000, [3]any{5000.0, 5000.0, "Credits"})
	storetest.Exec(t, l.st, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = now() WHERE id = $1`, sr1)
	if c, err := s.Claim(ctx, "analyzer-0", 8); err != nil || c.Settled != 3 || len(c.Items) != 0 {
		t.Fatalf("the claim: %+v, %v", c, err)
	}
	if r := l.read(r1); r.State == StateHeld || deref(r.RecordSourceID) != sr1 ||
		!strings.Contains(deref(r.RecordNote), "do not fit it an hour earlier") {
		t.Errorf("a retired title whose record's marks do not fit: %+v", r)
	}
	m5, s5 := uid('a', 5), uid('b', 5) // a header read finds none; its record's marks don't fit
	orig5 := l.title(m5, s5, opts{chapters: true})
	l.record(m5, s5, 1000, [3]any{5000.0, 5000.0, "Credits"})
	l.record_(m5, header(orig5, []Chapter{}, 1), now)

	chs := l.chapters(m1)
	if r := l.read(m1); r.State != StateRead || len(chs) != 6 || chs[0].StartMs != 0 || chs[0].EndMs != 300_000 ||
		chs[5].StartMs != 1_500_000 || chs[5].EndMs != 3_000_000 || deref(chs[5].Title) != "Chapter 06" ||
		deref(r.Reason) != "its 6 chapter marks lay an hour past their places: read 3600 s earlier" || needs(t, l.st, m1) {
		t.Errorf("six marks an hour past their places: %+v, %+v", r, chs)
	}
	if chs := l.chapters(m2); len(chs) != 1 || chs[0].StartMs != 50_000 || chs[0].EndMs != 2_000_000 || needs(t, l.st, m2) {
		t.Errorf("a mark that fits an hour earlier: %+v", chs)
	}
	for _, id := range []string{m3, m5} {
		if r := l.read(id); r.State != StateHeld || r.Confirmed || l.count(id) != 0 || !needs(t, l.st, id) ||
			deref(r.Reason) != "its 1 chapter marks lie past the file's end, and do not fit it an hour earlier: an admin decides" {
			t.Errorf("%s, marks that do not fit: %+v", id[:8], r)
		}
	}
	if r := l.read(m4); r.State != StateNone || !r.Confirmed || needs(t, l.st, m4) {
		t.Errorf("one mark of no length at the start: %+v", r)
	}
	if accepted, refused, err := Accept(ctx, l.st, l.paths, []string{m3}, TakeRecord, "an-admin", now); err != nil ||
		len(accepted) != 1 || len(refused) != 0 {
		t.Fatalf("the record's of m3: %v %v %v", accepted, refused, err)
	}
	if r := l.read(m3); r.State != StateNone || !r.Confirmed || needs(t, l.st, m3) {
		t.Errorf("m3, the record's taken: %+v", r)
	}
}
