package chapters

import (
	"context"
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

// record writes the source record's probe of the movie id's original source,
// with the chapter marks given ([start, end, title] each, in seconds; an end
// below 0 is none) and the file's duration.
func (l lib) record(id, source string, duration float64, marks ...[3]any) string {
	l.t.Helper()
	var chs []string
	for _, m := range marks {
		end := ""
		if e, _ := m[1].(float64); e >= 0 {
			end = fmt.Sprintf(`, "end_time": "%f"`, e)
		}
		chs = append(chs, fmt.Sprintf(`{"start_time": "%f"%s, "tags": {"title": %q}}`, m[0], end, m[2]))
	}
	path := filepath.Join(library.SourceDir(l.paths.MovieDir(id), source), ProbeFile)
	l.file(path, fmt.Sprintf(`{"streams": [{"codec_type": "video"}], "chapters": [%s], "format": {"duration": "%f"}}`,
		strings.Join(chs, ", "), duration))
	return path
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

	// its record's marks have no length: the none is confirmed, and the
	// chapters an earlier read wrote are not deleted
	m2, s2 := uid('a', 2), uid('b', 2)
	orig2 := l.title(m2, s2, opts{chapters: true})
	l.record(m2, s2, 2900, [3]any{3600.0, 3600.0, "Chapter 01"}, [3]any{3605.0, 3605.0, "Intro"})
	l.rows(m2, Chapter{StartMs: 0, EndMs: 9000, Title: str("Old"), Ordinal: 1})
	r = l.record_(m2, header(orig2, []Chapter{}, 2), at)
	if r.State != StateNone || !r.Confirmed || deref(r.RecordSourceID) != s2 || needs(t, l.st, m2) || l.count(m2) != 1 {
		t.Fatalf("an empty read its record agrees with: %+v, %d chapters", r, l.count(m2))
	}
	// read again while its record can't be read (the compliance check's word
	// is that it has chapter marks): the confirmed none stands
	if err := os.Remove(filepath.Join(library.SourceDir(l.paths.MovieDir(m2), s2), ProbeFile)); err != nil {
		t.Fatal(err)
	}
	if r = l.record_(m2, header(orig2, []Chapter{}, 2), at.Add(time.Hour)); r.State != StateNone || !r.Confirmed ||
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
	accepted, refused, err := Accept(ctx, l.st, []string{m1, uid('a', 9)}, "an-admin", at)
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
	if accepted, refused, _ := Accept(ctx, l.st, []string{m2}, "an-admin", at); len(accepted) != 0 ||
		!strings.Contains(refused[0], "is failed, not unreadable") {
		t.Errorf("accepting a failed title: %v %v", accepted, refused)
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
	m4, s4 := uid('a', 4), uid('b', 4) // chapters of an earlier read, not the record's
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
	if r, _ := ReadOf(ctx, l.st.Pool(), m4); !strings.Contains(deref(r.Reason), "in place of 1 chapters") {
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
		p.RecordsReadLastHour != 6 {
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
