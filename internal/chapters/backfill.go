package chapters

// The backfill: the titles that miss their chapters
// (com_nalet_katalog_chaptersmissing: a present original whose record says
// it has chapters, none in the catalog, no read of that original that
// settled it) are handed out to the analyzer (POST /api/chapters/claim), at
// the pace the settings allow, never in the quiet hours, a title at a time by
// default. The analyzer reads each original's header (ffprobe's chapters,
// nothing decoded) and reports what it found with the claim's token
// (Record). A title handed out is claimed until its report comes back or its
// lease ends: one whose run never reported back counts as a failure and is
// handed out again after its backoff. A title whose read failed waits its
// backoff; one whose reads failed MaxFailures times in a row is settled as
// unreadable. The titles whose version is complete go first, movies before
// episodes: their originals are the deletion waves' next. Everything it
// keeps is in the catalog, so it resumes where it was after a restart of
// either side, and it hands out nothing once no title misses its chapters.

import (
	"context"
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
	// SettingBackfill is on or off (the default): whether titles are handed
	// out at all.
	SettingBackfill = "chapters.backfill"
	// SettingWindow is when, "HH:MM-HH:MM" in UTC (22:00-17:00, which
	// crosses midnight: never from 17:00 to 22:00); empty, any time.
	SettingWindow = "chapters.backfill.window"
	// SettingInflight is how many titles may be handed out and not reported
	// at once (1).
	SettingInflight = "chapters.backfill.inflight"
	// SettingRate is how many titles are handed out an hour at most (120); 0
	// hands out none.
	SettingRate = "chapters.backfill.rate"
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
	DefaultLease    = 30 * time.Minute
)

// Settings are the backfill's settings as they are read.
type Settings struct {
	On       bool
	Window   Window
	Inflight int
	Rate     int
	Lease    time.Duration
	// Problems says of each setting that could not be read what it held and
	// what is used instead.
	Problems []string
}

// DefaultSettings are the settings of a catalog that sets none.
func DefaultSettings() Settings {
	w, _ := picture.ParseWindow(DefaultWindow)
	return Settings{Window: Window{Window: w}, Inflight: DefaultInflight, Rate: DefaultRate, Lease: DefaultLease}
}

// Window is when the backfill hands out titles: a daily span of UTC, as the
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
// hands out nothing), said in Problems.
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
			bad("a window, HH:MM-HH:MM in UTC, as 22:00-17:00; no title is handed out until it is one")
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
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.Rate = n
		} else if v != "" {
			bad(fmt.Sprintf("a number of titles an hour, 0 or more; it is %d", DefaultRate))
		}
	case SettingLease:
		if d, err := library.ParseDuration(v); err == nil && d >= time.Minute {
			s.Lease = d
		} else if v != "" {
			bad("a duration of a minute or more, as 30m or 2h; it is 30m")
		}
	}
}

// Job is a title the backfill hands out: its original to read (Path, of the
// source SourceID), how long the title is (the analyzer's fallback for the
// end of a last chapter whose header says none), and the token its report
// names (Report.Token).
type Job struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	SourceID   string `json:"sourceId"`
	DurationMs *int64 `json:"durationMs"`
	Token      string `json:"token"`
}

// Claim is what a claim hands out: its titles and the time their reads end by
// (the window's end, or the lease's), or none and why, and when to ask again.
type Claim struct {
	Items             []Job      `json:"items"`
	Deadline          *time.Time `json:"deadline,omitempty"`
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
	return Record(ctx, s.st, itemID, r, s.now())
}

// Title is a title's chapters, its last read and whether it misses its
// chapters (NeedsOriginal, of any of its originals).
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

// claimLock is the advisory lock a claim holds, the catalog's (its
// schema's): claims are counted against the pace one at a time.
const claimLock = `hashtext('chapters-backfill/' || current_schema())`

// candidates is how many titles a claim looks at for each it hands out: one
// whose original can't be found fails (and waits its backoff), and the next
// goes.
const candidates = 8

// Claim hands out up to limit titles to the worker (1 at least, at most what
// the pace allows), or none and why: the backfill is off, outside its window,
// titles enough are in flight, the hour's rate is reached, or no title misses
// its chapters. A claim whose lease ended first counts as a failure of its
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
	case set.Rate == 0:
		return idle(SettingRate+" is 0", 15*time.Minute), nil
	}
	tx, err := pool.Begin(ctx)
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
	var inflight, recent int
	var oldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE claimedat IS NOT NULL),
			count(*) FILTER (WHERE lastclaimedat > $1), min(lastclaimedat) FILTER (WHERE lastclaimedat > $1)
		FROM `+store.ChapterReadsTable, now.Add(-time.Hour)).Scan(&inflight, &recent, &oldest); err != nil {
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

	rows, err := tx.Query(ctx, candidatesSQL, now, n*candidates)
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
			if err := fail(ctx, tx, c.id, c.source, "nothing to read: "+why, now); err != nil {
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
		out.Items = append(out.Items, Job{ID: c.id, Type: c.typ, Path: path, SourceID: c.source,
			DurationMs: c.duration, Token: token})
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, err
	}
	if len(out.Items) == 0 {
		if len(cs) == 0 {
			return idle("no title misses its chapters", time.Hour), nil
		}
		return idle("the titles looked at had no original to read", time.Minute), nil
	}
	out.Deadline = &deadline
	return out, nil
}

func idle(why string, after time.Duration) Claim {
	return Claim{Items: []Job{}, Idle: why, RetryAfterSeconds: max(1, int(after.Round(time.Second)/time.Second))}
}

// candidatesSQL are the titles to read, $2 of them at most, in the order they
// are handed out: those that miss their chapters (a movie or an episode no
// other's file covers, none of a disc image), not claimed now and not waiting
// out a failure's or a release's backoff ($1 is now); the titles whose
// version is complete first (their originals are the deletion waves' next),
// movies before episodes; each once, with the original to read and its
// length.
var candidatesSQL = `
	SELECT t.item_id, t.type, t.sourceid, t.durationms FROM (
		SELECT DISTINCT ON (m.item_id) m.item_id, i.type, m.sourceid, i.durationms,
		       EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
		               WHERE v.item_id = m.item_id AND v.state = 'complete') AS packaged
		FROM ` + store.ChaptersMissingView + ` m
		JOIN com_nalet_katalog_items i ON i.id = m.item_id
		LEFT JOIN ` + store.ChapterReadsTable + ` r ON r.item_id = m.item_id
		WHERE i.type IN ('movie', 'episode') AND i.coveredby IS NULL AND NOT ` + processing.DiscImageOf("i.id") + `
		  AND (r.item_id IS NULL OR (r.claimedat IS NULL AND (r.nextattemptat IS NULL OR r.nextattemptat <= $1)))
		ORDER BY m.item_id, m.sourceid
	) t
	ORDER BY t.packaged DESC, t.type = 'movie' DESC, t.item_id
	LIMIT $2`

// expire takes the claims whose lease ended for failures of their titles:
// their runs never reported back (a title that misses no chapters since, its
// chapters read meanwhile, only has its claim ended). A claim's token is kept
// as the lapsed one: its run's failure, reported late, is counted already.
func expire(ctx context.Context, tx pgx.Tx, now time.Time, lease time.Duration) error {
	rows, err := tx.Query(ctx, `SELECT r.item_id, r.failures, r.state, `+settledSQL+`
		FROM `+store.ChapterReadsTable+` r WHERE r.claimedat IS NOT NULL AND r.claimedat <= $1 FOR UPDATE OF r`,
		now.Add(-lease))
	if err != nil {
		return err
	}
	type lost struct {
		id, state string
		failures  int32
		settled   bool
	}
	var ls []lost
	for rows.Next() {
		var l lost
		if err := rows.Scan(&l.id, &l.failures, &l.state, &l.settled); err != nil {
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
		if l.settled && l.state != StatePending && l.state != StateFailed {
			if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET claimedat = NULL, claimedby = NULL,
				lapsedtoken = claimtoken, claimtoken = NULL, modifiedat = $2 WHERE item_id = $1`, l.id, now); err != nil {
				return err
			}
			continue
		}
		f := l.failures + 1
		state, next, reason := failure(f, why, now)
		if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET claimedat = NULL, claimedby = NULL,
			lapsedtoken = claimtoken, claimtoken = NULL, failures = $2, state = $3, nextattemptat = $4, reason = $5,
			modifiedat = $6 WHERE item_id = $1`, l.id, f, state, next, reason, now); err != nil {
			return err
		}
	}
	return nil
}

// fail counts a failure of a title the backfill could not hand out (it has no
// original to read), which waits its backoff; the MaxFailures-th settles it
// as unreadable.
func fail(ctx context.Context, tx pgx.Tx, itemID, sourceID, why string, now time.Time) error {
	var failures int32
	if err := tx.QueryRow(ctx, `SELECT failures FROM `+store.ChapterReadsTable+` WHERE item_id = $1`, itemID).
		Scan(&failures); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	f := failures + 1
	state, next, reason := failure(f, why, now)
	_, err := tx.Exec(ctx, `INSERT INTO `+store.ChapterReadsTable+`
			(item_id, state, sourceid, reason, failures, nextattemptat, readat, createdat, modifiedat)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $7, $7)
		ON CONFLICT (item_id) DO UPDATE SET state = EXCLUDED.state, sourceid = EXCLUDED.sourceid,
			reason = EXCLUDED.reason, failures = EXCLUDED.failures, nextattemptat = EXCLUDED.nextattemptat,
			readat = EXCLUDED.readat, modifiedat = EXCLUDED.modifiedat`,
		itemID, state, sourceID, reason, f, next, now)
	return err
}

// originalOf is the file the backfill reads of a title: its original of the
// source sourceID, its primary asset, where it lies (there, or in a version's
// folder where a run left it and its handover was lost); "" and why when it
// is not on disk.
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
