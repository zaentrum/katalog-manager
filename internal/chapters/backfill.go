package chapters

// The backfill: the titles that miss their chapters
// (com_nalet_katalog_chaptersmissing: an original present on disk, the
// title's current one, that no read settled) are read, at the pace the
// settings allow, never in the quiet hours. Each claim (POST
// /api/chapters/claim, the analyzer's) first has the catalog read the source
// records of titles whose record it has not read yet for their current
// original (a small JSON file each, no media; FromRecord): a record that
// holds chapters settles the title with them, one that holds none with a
// length settles it as a confirmed none. Only a title whose record can't say
// (none written, no chapters list in it, an error of the storage) is handed
// out to the analyzer, which reads the original's header (ffprobe's chapters,
// nothing decoded) and reports what it found with the claim's token (Record).
// A title handed out is claimed until its report comes back or its lease
// ends; one whose run never reported back, or whose original is not on disk
// when it comes up, waits a backoff, which counts as no failure of the title.
// The titles whose version is complete go first, movies before episodes:
// their originals are the deletion waves' next. Titles whose original was
// retired with no chapters in the catalog have them restored from their
// records, after those. Everything it keeps is in the catalog, so it resumes
// where it was after a restart of either side.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/picture"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The backfill's settings, read on every claim: an instance switches it on,
// slows it down or moves its hours by setting them, without a restart.
const (
	// SettingBackfill is on or off (the default): whether titles are read
	// at all.
	SettingBackfill = "chapters.backfill"
	// SettingWindow is when, "HH:MM-HH:MM" in UTC (22:00-17:00, which
	// crosses midnight: never from 17:00 to 22:00); empty, any time.
	SettingWindow = "chapters.backfill.window"
	// SettingInflight is how many titles may be handed out and not reported
	// at once (1).
	SettingInflight = "chapters.backfill.inflight"
	// SettingRate is how many titles are handed out an hour at most (120);
	// 0 hands out none.
	SettingRate = "chapters.backfill.rate"
	// SettingRecords is how many source records the catalog reads an hour
	// at most (3600); 0 reads none, and every title goes to the analyzer.
	SettingRecords = "chapters.backfill.records"
	// SettingLease is how long a title handed out is the worker's before it
	// is taken for lost (30m: a header read takes seconds, the analyzer gives
	// one two minutes).
	SettingLease = "chapters.backfill.lease"
)

// The settings' defaults.
const (
	DefaultWindow   = "22:00-17:00"
	DefaultInflight = 1
	DefaultRate     = 120
	DefaultRecords  = 3600
	DefaultLease    = 30 * time.Minute
)

// Settings are the backfill's settings as they are read.
type Settings struct {
	On       bool
	Window   Window
	Inflight int
	Rate     int
	Records  int
	Lease    time.Duration
	// Problems says of each setting that could not be read what it held and
	// what is used instead.
	Problems []string
}

// DefaultSettings are the settings of a catalog that sets none.
func DefaultSettings() Settings {
	w, _ := picture.ParseWindow(DefaultWindow)
	return Settings{Window: Window{Window: w}, Inflight: DefaultInflight, Rate: DefaultRate, Records: DefaultRecords,
		Lease: DefaultLease}
}

// Window is when the backfill reads titles: a daily span of UTC, as the
// picture backfill's (picture.Window), or never, a setting that is no window.
type Window struct {
	picture.Window
	Never bool
}

// Contains reports whether t lies in the window.
func (w Window) Contains(t time.Time) bool { return !w.Never && w.Window.Contains(t) }

// String writes the window as it is set ("22:00-17:00 UTC", "any time").
func (w Window) String() string {
	if w.Never {
		return "never (no window that could be read)"
	}
	return w.Window.String()
}

// Closes is when the window that holds at t ends; the zero time when it never
// ends (any time).
func (w Window) Closes(t time.Time) time.Time {
	if w.Never {
		return time.Time{}
	}
	return w.Window.Closes(t)
}

// ReadSettings reads the backfill's settings, each key without the spaces
// around it, and of a key the table holds twice the row of the lowest id. A
// value that cannot be read is its default (a window that cannot be read
// reads nothing), said in Problems.
func ReadSettings(ctx context.Context, q library.Querier) (Settings, error) {
	s := DefaultSettings()
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (btrim(key)) btrim(key), valuetext FROM com_nalet_katalog_settings
		WHERE btrim(key) LIKE 'chapters.backfill%' ORDER BY btrim(key), id`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		s.set(key, value)
	}
	return s, rows.Err()
}

func (s *Settings) set(key, value string) {
	v := strings.TrimSpace(value)
	bad := func(what string) {
		s.Problems = append(s.Problems, fmt.Sprintf("%s is %q, which is not %s", key, value, what))
	}
	count := func(into *int, def int, what string) {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			*into = n
		} else if v != "" {
			bad(fmt.Sprintf("a number of %s an hour, 0 or more; it is %d", what, def))
		}
	}
	switch key {
	case SettingBackfill:
		switch strings.ToLower(v) {
		case "on":
			s.On = true
		case "off", "":
			s.On = false
		default:
			bad("on or off; the backfill is off")
		}
	case SettingWindow:
		w, err := picture.ParseWindow(v)
		if err != nil {
			s.Window = Window{Never: true}
			bad("a window, HH:MM-HH:MM in UTC, as 22:00-17:00; no title is read until it is one")
			return
		}
		s.Window = Window{Window: w}
	case SettingInflight:
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			s.Inflight = n
		} else if v != "" {
			bad(fmt.Sprintf("a number of titles, 1 or more; it is %d", DefaultInflight))
		}
	case SettingRate:
		count(&s.Rate, DefaultRate, "titles")
	case SettingRecords:
		count(&s.Records, DefaultRecords, "records")
	case SettingLease:
		if d, err := library.ParseDuration(v); err == nil && d >= time.Minute {
			s.Lease = d
		} else if v != "" {
			bad("a duration of a minute or more, as 30m or 2h; it is 30m")
		}
	}
}

// Job is a title the backfill hands out: its original to read (Path, of the
// source SourceID, which its report names), how long the title is (the
// analyzer's fallback for the end of a last chapter whose header says none),
// and the token its report names (Report.Token).
type Job struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	SourceID   string `json:"sourceId"`
	DurationMs *int64 `json:"durationMs"`
	Token      string `json:"token"`
}

// Claim is what a claim hands out: its titles and the time their reads end by
// (the window's end, or the lease's), or none and why, and when to ask again;
// Settled says how many titles the claim settled from their records first.
type Claim struct {
	Items             []Job      `json:"items"`
	Deadline          *time.Time `json:"deadline,omitempty"`
	Settled           int        `json:"settled,omitempty"`
	Idle              string     `json:"idle,omitempty"`
	RetryAfterSeconds int        `json:"retryAfterSeconds,omitempty"`
}

// Service is the titles' chapters and their backfill, over the catalog of st
// and the library of cfg.
type Service struct {
	st    *store.Store
	paths library.Paths
	now   func() time.Time
}

// New is the chapters of the catalog st and the library cfg.
func New(st *store.Store, cfg config.Config) *Service {
	return &Service{st: st, paths: library.PathsOf(cfg), now: time.Now}
}

// WithClock is s with now as its clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func (s *Service) ready(ctx context.Context) error {
	ok, err := s.st.ChapterReadsReady(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotReady
	}
	return nil
}

// Record keeps a read the analyzer reported (Record).
func (s *Service) Record(ctx context.Context, itemID string, r Report) (*Read, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return Record(ctx, s.st, s.paths, itemID, r, s.now())
}

// Accept settles, for an admin, the unreadable and held titles of items
// (Accept).
func (s *Service) Accept(ctx context.Context, items []string, take, by string) ([]string, []string, error) {
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
	}
	return Accept(ctx, s.st, s.paths, items, take, by, s.now())
}

// Revert puts back, for an admin, the chapters a read replaced (Revert).
func (s *Service) Revert(ctx context.Context, items []string, by string) ([]string, []string, error) {
	if err := s.ready(ctx); err != nil {
		return nil, nil, err
	}
	return Revert(ctx, s.st, items, by, s.now())
}

// Title is a title's chapters, its last read and whether it misses its
// chapters (NeedsOriginal, of its current original).
type Title struct {
	Chapters []Chapter
	Read     *Read
	Missing  bool
}

// Of reads the title itemID's chapters and its last read; ErrNoTitle for no
// such item.
func (s *Service) Of(ctx context.Context, itemID string) (*Title, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	pool := s.st.Pool()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items WHERE id = $1)`, itemID).
		Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNoTitle
	}
	var t Title
	var err error
	if t.Chapters, err = Of(ctx, pool, itemID); err != nil {
		return nil, err
	}
	if t.Read, err = ReadOf(ctx, pool, itemID); err != nil {
		return nil, err
	}
	if t.Missing, err = NeedsOriginal(ctx, pool, itemID, ""); err != nil {
		return nil, err
	}
	return &t, nil
}

// claimLock is the advisory lock a claim's hand-out holds, the catalog's
// (its schema's): claims are counted against the pace one at a time.
const claimLock = `hashtext('chapters-backfill/' || current_schema())`

// candidates is how many titles a claim looks at for each it hands out: one
// whose original can't be found waits its backoff, and the next goes.
const candidates = 8

// recordsPerClaim and recordsFor bound the records a claim reads before it
// hands out: so many, for so long at most.
const (
	recordsPerClaim = 50
	recordsFor      = 10 * time.Second
)

// Claim reads, of the titles that miss their chapters, the source records
// the settings' pace allows (FromRecord), and hands out up to limit titles
// whose records can't say to the worker (1 at least, at most what the pace
// allows); or none and why: the backfill is off, outside its window, titles
// enough are in flight, the hour's rate is reached, or no title waits. A
// claim whose lease ended first waits its backoff, as no failure of its
// title.
func (s *Service) Claim(ctx context.Context, worker string, limit int) (Claim, error) {
	if err := s.ready(ctx); err != nil {
		return Claim{}, err
	}
	now := s.now().UTC()
	pool := s.st.Pool()
	set, err := ReadSettings(ctx, pool)
	if err != nil {
		return Claim{}, err
	}
	switch {
	case !set.On:
		return idle(SettingBackfill+" is off", 15*time.Minute), nil
	case set.Window.Never:
		return idle(SettingWindow+" is no window", 15*time.Minute), nil
	case !set.Window.Contains(now):
		return idle("outside "+SettingWindow+" ("+set.Window.String()+")", set.Window.Opens(now).Sub(now)), nil
	}
	settled, err := s.settleFromRecords(ctx, set, now)
	if err != nil {
		return Claim{}, err
	}
	out, err := s.handOut(ctx, worker, limit, set, now)
	out.Settled = settled
	if err != nil || len(out.Items) > 0 || settled == 0 {
		return out, err
	}
	// records settled titles and none is handed out: ask again at once
	return Claim{Items: []Job{}, Settled: settled, Idle: fmt.Sprintf("%d titles were settled from their records", settled),
		RetryAfterSeconds: 1}, nil
}

// handOut hands out up to limit titles whose records can't say, in the
// claim's lock.
func (s *Service) handOut(ctx context.Context, worker string, limit int, set Settings, now time.Time) (Claim, error) {
	if set.Rate == 0 {
		return idle(SettingRate+" is 0", 15*time.Minute), nil
	}
	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return Claim{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+claimLock+`)`); err != nil {
		return Claim{}, err
	}
	if err := expire(ctx, tx, now, set.Lease); err != nil {
		return Claim{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+store.ChapterClaimsTable+` WHERE claimedat < $1`,
		now.Add(-48*time.Hour)); err != nil {
		return Claim{}, err
	}
	var inflight, recent int
	var oldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+store.ChapterReadsTable+` WHERE claimedat IS NOT NULL),
			count(*), min(claimedat) FROM `+store.ChapterClaimsTable+` WHERE claimedat > $1`, now.Add(-time.Hour)).
		Scan(&inflight, &recent, &oldest); err != nil {
		return Claim{}, err
	}
	if inflight >= set.Inflight {
		return idle(fmt.Sprintf("%d titles are being read (%s %d)", inflight, SettingInflight, set.Inflight),
			time.Minute), tx.Commit(ctx)
	}
	if recent >= set.Rate {
		wait := time.Minute
		if oldest != nil {
			wait = oldest.Add(time.Hour).Sub(now)
		}
		return idle(fmt.Sprintf("%d titles were handed out in the last hour (%s %d)", recent, SettingRate, set.Rate),
			wait), tx.Commit(ctx)
	}
	n := min(max(limit, 1), set.Inflight-inflight, set.Rate-recent)

	rows, err := tx.Query(ctx, handOutSQL, now, n*candidates, set.Records == 0)
	if err != nil {
		return Claim{}, err
	}
	type candidate struct {
		id, typ, source string
		duration        *int64
	}
	var cs []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.typ, &c.source, &c.duration); err != nil {
			rows.Close()
			return Claim{}, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Claim{}, err
	}
	// the reads' deadline: the lease's end, or the window's when it comes
	// first (the backfill reads nothing in the quiet hours)
	deadline := now.Add(set.Lease)
	if closes := set.Window.Closes(now); !closes.IsZero() && closes.Before(deadline) {
		deadline = closes
	}
	out := Claim{Items: []Job{}}
	for _, c := range cs {
		if len(out.Items) == n {
			break
		}
		path, why, err := s.originalOf(ctx, tx, c.id, c.source)
		if err != nil {
			return Claim{}, err
		}
		if path == "" {
			if err := transient(ctx, tx, c.id, "nothing to read: "+why, now); err != nil {
				return Claim{}, err
			}
			continue
		}
		var token string
		if err := tx.QueryRow(ctx, `INSERT INTO `+store.ChapterReadsTable+`
				(item_id, state, claimedat, claimedby, claimtoken, lastclaimedat, attempts, createdat, modifiedat)
			VALUES ($1, 'pending', $2, $3, gen_random_uuid()::varchar, $2, 1, $2, $2)
			ON CONFLICT (item_id) DO UPDATE SET claimedat = $2, claimedby = $3, claimtoken = gen_random_uuid()::varchar,
				lastclaimedat = $2, attempts = `+store.ChapterReadsTable+`.attempts + 1, modifiedat = $2
			RETURNING claimtoken`, c.id, now, clip(worker, 255)).Scan(&token); err != nil {
			return Claim{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+store.ChapterClaimsTable+` (item_id, claimedat) VALUES ($1, $2)`,
			c.id, now); err != nil {
			return Claim{}, err
		}
		out.Items = append(out.Items, Job{ID: c.id, Type: c.typ, Path: path, SourceID: c.source,
			DurationMs: c.duration, Token: token})
	}
	var missing int
	if len(out.Items) == 0 {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+store.ChaptersMissingView).Scan(&missing); err != nil {
			return Claim{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, err
	}
	if len(out.Items) == 0 {
		switch {
		case missing == 0:
			return idle("no title misses its chapters", time.Hour), nil
		case len(cs) > 0:
			return idle("the titles looked at had no original to read", time.Minute), nil
		}
		return idle(fmt.Sprintf("%d titles miss their chapters, none to hand out now (their records not read yet, "+
			"claimed, waiting their backoff, unreadable, or held for an admin)", missing), 15*time.Minute), nil
	}
	out.Deadline = &deadline
	return out, nil
}

func idle(why string, after time.Duration) Claim {
	return Claim{Items: []Job{}, Idle: why, RetryAfterSeconds: max(1, int(after.Round(time.Second)/time.Second))}
}

// missingSQL are the titles that miss their chapters, a movie or an episode
// no other's file covers, none of a disc image, each with its current
// original and its length, the read the catalog keeps of it (aliased r; the
// original it read, readsource) and whether its version is complete.
var missingSQL = `
	SELECT m.item_id, i.type, m.sourceid, i.durationms,
	       EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
	               WHERE v.item_id = m.item_id AND v.state = 'complete') AS packaged,
	       r.recordsourceid, r.state AS readstate, r.sourceid AS readsource, r.claimedat, r.nextattemptat
	FROM ` + store.ChaptersMissingView + ` m
	JOIN com_nalet_katalog_items i ON i.id = m.item_id
	LEFT JOIN ` + store.ChapterReadsTable + ` r ON r.item_id = m.item_id
	WHERE i.coveredby IS NULL AND NOT ` + processing.DiscImageOf("i.id")

// handOutSQL are the titles to hand out, $2 of them at most, in their order:
// of those that miss their chapters, those whose current original's record
// the catalog read and could not settle them with (any, when $3: the
// catalog reads no records), neither unreadable nor held of that original,
// not claimed now and not waiting out a backoff ($1 is now); the titles whose
// version is complete first, movies before episodes.
var handOutSQL = `
	SELECT t.item_id, t.type, t.sourceid, t.durationms FROM (` + missingSQL + `) t
	WHERE ($3 OR t.recordsourceid = t.sourceid) AND t.readstate IS DISTINCT FROM 'unreadable'
	  AND (t.readstate IS DISTINCT FROM 'held' OR t.readsource IS DISTINCT FROM t.sourceid)
	  AND t.claimedat IS NULL AND (t.nextattemptat IS NULL OR t.nextattemptat <= $1)
	ORDER BY t.packaged DESC, t.type = 'movie' DESC, t.item_id
	LIMIT $2`

// recordSQL are the titles whose source record the catalog reads next, $2 of
// them at most: those that miss their chapters whose current original's
// record it has not read (not claimed now, not held of that original, not
// waiting out a backoff; $1 is now), the packaged first, movies before
// episodes; then the titles whose original was retired with no chapter in the
// catalog, of their last original, unless the compliance check's record of it
// says it holds none, and unless that original's record was read already.
var recordSQL = `
	SELECT item_id, sourceid FROM (
		SELECT t.item_id, t.sourceid, 0 AS rank, t.packaged, t.type FROM (` + missingSQL + `) t
		WHERE t.recordsourceid IS DISTINCT FROM t.sourceid AND t.claimedat IS NULL
		  AND (t.readstate IS DISTINCT FROM 'held' OR t.readsource IS DISTINCT FROM t.sourceid)
		  AND (t.nextattemptat IS NULL OR t.nextattemptat <= $1)
		UNION ALL
		SELECT i.id, last.id, 1, true, i.type
		FROM com_nalet_katalog_items i
		JOIN LATERAL (SELECT s.id FROM com_nalet_katalog_itemsources s WHERE s.item_id = i.id
		              ORDER BY s.deletedat DESC NULLS LAST, s.createdat DESC, s.id LIMIT 1) last ON true
		LEFT JOIN ` + store.ChapterReadsTable + ` r ON r.item_id = i.id
		LEFT JOIN com_nalet_katalog_compliance c ON c.item_id = i.id
		WHERE i.type IN ('movie', 'episode') AND i.coveredby IS NULL
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources p WHERE p.item_id = i.id AND p.state = 'present')
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemchapters h WHERE h.item_id = i.id)
		  AND r.recordsourceid IS DISTINCT FROM last.id AND (r.nextattemptat IS NULL OR r.nextattemptat <= $1)
		  AND NOT COALESCE(substring(c.recordkey FROM 's:([0-9a-f-]{36})@') = last.id
		                   AND jsonb_typeof(c.records->'srcEssence'->'chapters') = 'boolean'
		                   AND NOT (c.records->'srcEssence'->>'chapters')::boolean, false)
	) x
	ORDER BY rank, packaged DESC, type = 'movie' DESC, item_id
	LIMIT $2`

// settleFromRecords reads the source records of the titles recordSQL names,
// as many as the hour's pace of the setting allows (recordsPerClaim, for
// recordsFor at most), and settles each it can (FromRecord); it answers how
// many it settled.
func (s *Service) settleFromRecords(ctx context.Context, set Settings, now time.Time) (int, error) {
	if set.Records == 0 {
		return 0, nil
	}
	pool := s.st.Pool()
	var recent int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+store.ChapterReadsTable+` WHERE recordat > $1`,
		now.Add(-time.Hour)).Scan(&recent); err != nil {
		return 0, err
	}
	n := min(set.Records-recent, recordsPerClaim)
	if n <= 0 {
		return 0, nil
	}
	rows, err := pool.Query(ctx, recordSQL, now, n)
	if err != nil {
		return 0, err
	}
	type title struct{ id, source string }
	var ts []title
	for rows.Next() {
		var t title
		if err := rows.Scan(&t.id, &t.source); err != nil {
			rows.Close()
			return 0, err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	settled, until := 0, time.Now().Add(recordsFor)
	for _, t := range ts {
		if time.Now().After(until) {
			break
		}
		rec := RecordOf(ctx, pool, s.paths, t.id, t.source)
		ok, err := FromRecord(ctx, s.st, t.id, t.source, rec, now)
		if err != nil {
			return settled, err
		}
		if ok {
			settled++
		}
	}
	return settled, nil
}

// FromRecord settles the title itemID of its original sourceID from that
// original's source record, rec, where the record says, as take says: the
// record's chapters in place of those of another original, and of those of
// no original known when they are as many or more (those it replaces kept to
// undo); those in the catalog kept as they are when they are the record's;
// a confirmed none, those of another original going with it. Fewer, or none,
// than those of no original known hold the title for an admin, as marks that
// lie past the file's end and do not fit it an hour earlier do (of an
// original still on disk): a record never deletes chapters of no original
// known. A title claimed now, held of that original, or that a read of that
// original settled since, is left as it is. Where the record can't say, why
// is kept, and the backfill hands the title to the analyzer. It answers
// whether the record settled the title.
func FromRecord(ctx context.Context, st *store.Store, itemID, sourceID string, rec RecordRead, now time.Time) (bool, error) {
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO `+store.ChapterReadsTable+` (item_id, state, createdat, modifiedat)
		VALUES ($1, 'pending', $2, $2) ON CONFLICT (item_id) DO NOTHING`, itemID, now); err != nil {
		return false, err
	}
	kept, err := scanRead(tx.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.ChapterReadsTable+` r
		WHERE r.item_id = $1 FOR UPDATE OF r`, itemID))
	if err != nil {
		return false, err
	}
	have, err := Of(ctx, tx, itemID)
	if err != nil {
		return false, err
	}
	if kept.ClaimedAt != nil || kept.heldOf(&sourceID) || kept.settles(&sourceID, len(have) > 0) {
		return false, tx.Commit(ctx)
	}
	// an original no longer on disk (a retired one's record restores its
	// chapters) is held for no admin: there is nothing left to keep
	var present bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources
		WHERE id = $1 AND item_id = $2 AND state = 'present')`, sourceID, itemID).Scan(&present); err != nil {
		return false, err
	}
	// noted keeps that the record was read and why it settled nothing; held
	// holds the title for an admin besides
	noted := func(why string, held bool) (bool, error) {
		held = held && present
		if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET recordsourceid = $2, recordat = $3,
			recordnote = $4, state = CASE WHEN $5 THEN 'held' ELSE state END,
			sourceid = CASE WHEN $5 THEN $2 ELSE sourceid END, reason = CASE WHEN $5 THEN $4 ELSE reason END,
			nextattemptat = CASE WHEN $5 THEN NULL ELSE nextattemptat END, modifiedat = $3
			WHERE item_id = $1`, itemID, sourceID, now, clip(why, 500), held); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	switch {
	case rec.Held != "":
		return noted(rec.Held, true)
	case !rec.Says():
		why := rec.Why
		if rec.Err != nil {
			why = "its source record could not be read: " + rec.Err.Error()
		}
		return noted(why, false)
	}
	how, why := take(have, rec.Chapters, kept.RowsSourceID, &sourceID)
	reason := rec.Note
	switch how {
	case takeHold:
		return noted("its source record: "+why, true)
	case takeStay:
		return noted("its source record holds no chapter; the chapters read before of this original stay", false)
	case takeKeep:
		if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET rowssourceid = $2 WHERE item_id = $1`,
			itemID, sourceID); err != nil {
			return false, err
		}
		reason = "the chapters in the catalog are those of its source record"
	default:
		if err := put(ctx, tx, itemID, have, rec.Chapters, kept.RowsSourceID, &sourceID, ReadFromRecord, now); err != nil {
			return false, err
		}
		switch {
		case len(rec.Chapters) > 0 && reason == "":
			reason = "read from its source record"
		case len(rec.Chapters) == 0 && reason == "":
			reason = "its source record holds no chapter mark"
		}
		if len(have) > 0 {
			reason = replacedNote(reason, len(have))
		}
	}
	state, confirmed := StateRead, false
	if len(rec.Chapters) == 0 {
		state, confirmed = StateNone, true
	}
	_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET state = $2, readfrom = 'record', confirmed = $3,
		sourceid = $4, path = $5, filesize = NULL, found = $6, written = $7, probe = 'source record', seconds = NULL,
		reason = NULLIF($8, ''), readat = $9, recordsourceid = $4, recordat = $9, recordnote = NULL, failures = 0,
		transients = 0, nextattemptat = $10, modifiedat = $9 WHERE item_id = $1`, itemID, state, confirmed, sourceID,
		clip(rec.Path, 2048), rec.Found, int32(len(rec.Chapters)), clip(reason, 500), now, now.Add(SettledWait))
	if err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// expire takes the claims whose lease ended back: their runs never reported
// back, which waits a backoff and counts as no failure of the title (the
// worker, or the storage, failed). A claim's token and when it was handed
// out are kept as the lapsed one's: its run's report, late, is told for one.
func expire(ctx context.Context, tx pgx.Tx, now time.Time, lease time.Duration) error {
	rows, err := tx.Query(ctx, `SELECT item_id, transients FROM `+store.ChapterReadsTable+`
		WHERE claimedat IS NOT NULL AND claimedat <= $1 FOR UPDATE`, now.Add(-lease))
	if err != nil {
		return err
	}
	type lost struct {
		id string
		t  int32
	}
	var ls []lost
	for rows.Next() {
		var l lost
		if err := rows.Scan(&l.id, &l.t); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	why := "its run did not report back within " + library.DurationText(lease)
	for _, l := range ls {
		t := l.t + 1
		if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET claimedat = NULL, claimedby = NULL,
			lapsedtoken = claimtoken, lapsedclaimedat = claimedat, claimtoken = NULL, transients = $2,
			nextattemptat = $3, state = CASE WHEN state = 'pending' THEN 'failed' ELSE state END,
			reason = CASE WHEN state IN ('pending', 'failed') THEN $4 ELSE reason END, modifiedat = $5
			WHERE item_id = $1`, l.id, t, now.Add(backoff(t)), why, now); err != nil {
			return err
		}
	}
	return nil
}

// transient keeps a failure of the storage, or of the worker, of a title the
// backfill could not hand out (its original is not on disk): it waits its
// backoff, and counts as no failure of the title.
func transient(ctx context.Context, tx pgx.Tx, itemID, why string, now time.Time) error {
	var t int32
	if err := tx.QueryRow(ctx, `SELECT transients FROM `+store.ChapterReadsTable+` WHERE item_id = $1`, itemID).
		Scan(&t); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	t++
	_, err := tx.Exec(ctx, `INSERT INTO `+store.ChapterReadsTable+`
			(item_id, state, reason, transients, nextattemptat, createdat, modifiedat)
		VALUES ($1, 'failed', $2, $3, $4, $5, $5)
		ON CONFLICT (item_id) DO UPDATE SET reason = EXCLUDED.reason, transients = EXCLUDED.transients,
			nextattemptat = EXCLUDED.nextattemptat, modifiedat = EXCLUDED.modifiedat,
			state = CASE WHEN `+store.ChapterReadsTable+`.state = 'pending' THEN 'failed'
			             ELSE `+store.ChapterReadsTable+`.state END`,
		itemID, clip(why, 500), t, now.Add(backoff(t)), now)
	return err
}

// originalOf is the file the backfill reads of a title: its original of the
// source sourceID, its primary asset of that source (else its primary
// asset), where it lies (there, or in a version's folder where a run left it
// and its handover was lost); "" and why when it is not on disk.
func (s *Service) originalOf(ctx context.Context, q pgx.Tx, itemID, sourceID string) (string, string, error) {
	var path *string
	err := q.QueryRow(ctx, `SELECT path FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND isprimary AND COALESCE(kind, 'primary') = 'primary'
		ORDER BY sourceid IS NOT DISTINCT FROM $2 DESC, path LIMIT 1`, itemID, sourceID).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && path == nil) {
		return "", "it has no primary asset", nil
	}
	if err != nil {
		return "", "", err
	}
	at := s.originalAt(ctx, q, itemID, *path)
	if fi, err := os.Stat(at); err != nil || !fi.Mode().IsRegular() {
		return "", "its original is not at " + at, nil
	}
	return at, "", nil
}

// originalAt is where the original at path lies: there, or, where a run left
// it in a version's folder and its handover was lost, there (as the worker
// record hands it out).
func (s *Service) originalAt(ctx context.Context, q library.Querier, id, path string) string {
	if _, err := os.Lstat(path); err == nil {
		return path
	}
	pl, err := library.PlaceOf(ctx, q, id)
	if err != nil {
		return path
	}
	src, err := library.SourceAt(ctx, q, filepath.Clean(path))
	if err != nil || src == nil {
		return path
	}
	if found, err := library.UnrecordedOriginal(ctx, q, s.paths.ItemDir(pl), src); err == nil && found != "" {
		return found
	}
	return path
}

// What an admin's acceptance of a held title takes for its chapters.
const (
	TakeCatalog = "catalog" // the chapters in the catalog, now of its current original
	TakeRecord  = "record"  // its source record's (none, where its marks have no place in the file)
)

// currentSourceSQL is the current original of the title $1: its primary
// asset's source while that is present, else its newest present one (as the
// view reads it).
const currentSourceSQL = `SELECT s.id FROM com_nalet_katalog_itemsources s
	WHERE s.item_id = $1 AND s.state = 'present'
	ORDER BY s.id IS NOT DISTINCT FROM (
	           SELECT a.sourceid FROM com_nalet_katalog_playbackassets a
	           WHERE a.item_id = $1 AND a.isprimary AND COALESCE(a.kind, 'primary') = 'primary'
	             AND a.sourceid IS NOT NULL
	           ORDER BY a.id LIMIT 1) DESC,
	         s.createdat DESC, s.id
	LIMIT 1`

// Accept settles, for the admin by, the titles of items an admin decides:
// one whose last read, of its current original, is unreadable (its original
// may go without its chapters; with take TakeCatalog only), and one held
// (take TakeCatalog: the chapters in the catalog are the title's, now of its
// current original, or none; TakeRecord: its source record's replace them,
// those kept to undo, or none, where its marks have no place in the file). It
// answers the titles accepted, and of each it refused why.
func Accept(ctx context.Context, st *store.Store, p library.Paths, items []string, take, by string,
	now time.Time) ([]string, []string, error) {
	accepted, refused := []string{}, []string{}
	if take != TakeCatalog && take != TakeRecord {
		return accepted, refused, &BadReport{"take is " + TakeCatalog + " or " + TakeRecord}
	}
	for _, id := range items {
		why, err := accept(ctx, st, p, id, take, by, now)
		if err != nil {
			return accepted, refused, err
		}
		if why != "" {
			refused = append(refused, id+": "+why)
			continue
		}
		accepted = append(accepted, id)
	}
	return accepted, refused, nil
}

// accept is Accept of one title; why it refused it, "" when it took it.
func accept(ctx context.Context, st *store.Store, p library.Paths, id, take, by string, now time.Time) (string, error) {
	pool := st.Pool()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items WHERE id = $1)`, id).
		Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "no such title", nil
	}
	var current string
	if err := pool.QueryRow(ctx, currentSourceSQL, id).Scan(&current); errors.Is(err, pgx.ErrNoRows) {
		return "it has no original on disk", nil
	} else if err != nil {
		return "", err
	}
	var rec RecordRead
	if take == TakeRecord {
		rec = RecordOf(ctx, pool, p, id, current)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	r, err := scanRead(tx.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.ChapterReadsTable+` r
		WHERE r.item_id = $1 FOR UPDATE OF r`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return "its chapters were never read", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case r.State != StateUnreadable && r.State != StateHeld:
		return "its last read is " + r.State + ", neither unreadable nor held", nil
	case r.SourceID == nil || *r.SourceID != current:
		return "its read is not of its current original", nil
	case r.State == StateUnreadable && take == TakeRecord:
		return "its source record can't say: an unreadable title is accepted as the catalog holds it", nil
	case r.State == StateUnreadable:
		_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET acceptedat = $2, acceptedby = $3, modifiedat = $2
			WHERE item_id = $1`, id, now, clip(by, 255))
		if err != nil {
			return "", err
		}
		return "", tx.Commit(ctx)
	}
	have, err := Of(ctx, tx, id)
	if err != nil {
		return "", err
	}
	got, from := have, ReadFromCatalog
	if take == TakeRecord {
		switch {
		case rec.Says():
			got = rec.Chapters
		case rec.Held != "":
			got = nil // its marks have no place in the file
		default:
			why := rec.Why
			if rec.Err != nil {
				why = "it could not be read: " + rec.Err.Error()
			}
			return "its source record can't say: " + why, nil
		}
		from = ReadFromRecord
	}
	if err := put(ctx, tx, id, have, got, r.RowsSourceID, &current, "admin", now); err != nil {
		return "", err
	}
	state, confirmed := StateRead, false
	if len(got) == 0 {
		state, confirmed = StateNone, true
	}
	_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET state = $2, readfrom = $3, confirmed = $4,
		written = $5, reason = $6, acceptedat = $7, acceptedby = $8, nextattemptat = NULL, modifiedat = $7
		WHERE item_id = $1`, id, state, from, confirmed, int32(len(got)),
		clip("an admin took the "+take+"'s chapters", 500), now, clip(by, 255))
	if err != nil {
		return "", err
	}
	return "", tx.Commit(ctx)
}

// Revert puts back, for the admin by, the chapters a read replaced last of
// each title of items, now of its current original, and keeps those it
// replaces to undo in turn (a second Revert takes the read's back). It
// answers the titles reverted, and of each it refused why.
func Revert(ctx context.Context, st *store.Store, items []string, by string, now time.Time) ([]string, []string, error) {
	reverted, refused := []string{}, []string{}
	for _, id := range items {
		why, err := revert(ctx, st, id, by, now)
		if err != nil {
			return reverted, refused, err
		}
		if why != "" {
			refused = append(refused, id+": "+why)
			continue
		}
		reverted = append(reverted, id)
	}
	return reverted, refused, nil
}

func revert(ctx context.Context, st *store.Store, id, by string, now time.Time) (string, error) {
	pool := st.Pool()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	r, err := scanRead(tx.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.ChapterReadsTable+` r
		WHERE r.item_id = $1 FOR UPDATE OF r`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(r.Replaced) == 0) {
		return "no read replaced its chapters", nil
	}
	if err != nil {
		return "", err
	}
	var undo struct {
		Chapters []Chapter `json:"chapters"`
	}
	if err := json.Unmarshal(r.Replaced, &undo); err != nil || len(undo.Chapters) == 0 {
		return "the chapters it replaced can't be read back", nil
	}
	var current *string
	if err := tx.QueryRow(ctx, currentSourceSQL, id).Scan(&current); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	have, err := Of(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if len(have) == 0 {
		// nothing in the catalog to keep to undo in turn: the undo is used
		if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET replaced = NULL WHERE item_id = $1`,
			id); err != nil {
			return "", err
		}
	}
	if err := put(ctx, tx, id, have, undo.Chapters, r.RowsSourceID, current, "admin", now); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET state = 'read', readfrom = 'catalog', confirmed = false,
		sourceid = $2, written = $3, reason = 'an admin took back the chapters a read replaced', acceptedat = $4,
		acceptedby = $5, nextattemptat = NULL, modifiedat = $4 WHERE item_id = $1`, id, current,
		int32(len(undo.Chapters)), now, clip(by, 255))
	if err != nil {
		return "", err
	}
	return "", tx.Commit(ctx)
}
