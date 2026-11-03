package picture

// The backfill: the titles that have no picture area of what they play now
// are handed out to the analyzer (POST /api/picture/claim), at the pace the
// settings allow, never in the quiet hours, a title at a time by default. A
// title handed out is claimed until its report comes back (Record) or its
// lease ends: one whose run never reported back counts as a failure and is
// handed out again after its backoff, one given back unmeasured after its
// release backoff, the titles after it first. Everything it keeps is in the
// catalog, so it resumes where it was after a restart of either side. A
// movie or an episode is measured of its original while it has one (its
// primary asset, on disk), else of its package: an original has a keyframe
// at its cuts, a package's encoded rung one every few seconds and none at a
// cut, which the analyzer can't tell an area from. The titles that still
// have their original go first (before a deletion wave retires it; see
// NeedsOriginal), the titles never measured next (movies, then episodes,
// then extras), then those measured again: of an older method than the
// analyzer's that asks, one it could not tell of a month ago
// (UnknownAgain) but not a package it could not tell for its keyframes'
// gaps (that answer stays until the package changes), one a package could
// not tell while the original is there, one whose area is of another file
// than the title plays now. No title is measured again within a day of its
// last measurement (MeasureAgainAfter), whatever it plays.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The backfill's settings, read on every claim: an instance switches it on,
// slows it down or moves its hours by setting them, without a restart.
const (
	// SettingBackfill is on or off (the default): whether titles are handed
	// out at all.
	SettingBackfill = "picture.backfill"
	// SettingWindow is when, "HH:MM-HH:MM" in UTC (22:00-17:00, which
	// crosses midnight: never from 17:00 to 22:00); empty, any time.
	SettingWindow = "picture.backfill.window"
	// SettingInflight is how many titles may be handed out and not reported
	// at once (1).
	SettingInflight = "picture.backfill.inflight"
	// SettingRate is how many titles are handed out an hour at most (240); 0
	// hands out none.
	SettingRate = "picture.backfill.rate"
	// SettingLease is how long a title handed out is the worker's before it
	// is taken for lost (6h: the whole measurement of a 3-hour 4K remux, with
	// room to spare).
	SettingLease = "picture.backfill.lease"
)

// The settings' defaults.
const (
	DefaultWindow   = "22:00-17:00"
	DefaultInflight = 1
	DefaultRate     = 240
	DefaultLease    = 6 * time.Hour
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
	w, _ := ParseWindow(DefaultWindow)
	return Settings{Window: w, Inflight: DefaultInflight, Rate: DefaultRate, Lease: DefaultLease}
}

// ReadSettings reads the backfill's settings, each key without the spaces
// around it, and of a key the table holds twice the row of the lowest id. A
// value that cannot be read is its default (a window that cannot be read
// hands out nothing), said in Problems.
func ReadSettings(ctx context.Context, q library.Querier) (Settings, error) {
	s := DefaultSettings()
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (btrim(key)) btrim(key), valuetext FROM com_nalet_katalog_settings
		WHERE btrim(key) LIKE 'picture.backfill%' ORDER BY btrim(key), id`)
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
		w, err := ParseWindow(v)
		if err != nil {
			s.Window = Window{never: true}
			bad("a window, HH:MM-HH:MM in UTC, as 22:00-17:00; no title is handed out until it is one")
			return
		}
		s.Window = w
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

// Window is a daily span of UTC: from its start to its end, which may be past
// midnight. A zero Window is any time; one that never holds is no window that
// could be read.
type Window struct {
	set, never bool
	start, end int // minutes since midnight, UTC
}

var windowRE = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)-([01]\d|2[0-3]):([0-5]\d)$`)

// ParseWindow reads "HH:MM-HH:MM" (UTC); empty is any time.
func ParseWindow(v string) (Window, error) {
	v = strings.ReplaceAll(strings.TrimSpace(v), " ", "")
	if v == "" {
		return Window{}, nil
	}
	m := windowRE.FindStringSubmatch(v)
	if m == nil {
		return Window{}, fmt.Errorf("%q is no window: HH:MM-HH:MM in UTC, as 22:00-17:00", v)
	}
	at := func(h, mm string) int {
		hh, _ := strconv.Atoi(h)
		mi, _ := strconv.Atoi(mm)
		return hh*60 + mi
	}
	return Window{set: true, start: at(m[1], m[2]), end: at(m[3], m[4])}, nil
}

// String writes the window as it is set ("22:00-17:00 UTC"), "any time".
func (w Window) String() string {
	switch {
	case w.never:
		return "never (no window that could be read)"
	case !w.set || w.start == w.end:
		return "any time"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d UTC", w.start/60, w.start%60, w.end/60, w.end%60)
}

// Contains reports whether t lies in the window.
func (w Window) Contains(t time.Time) bool {
	if w.never {
		return false
	}
	if !w.set || w.start == w.end {
		return true
	}
	u := t.UTC()
	m := u.Hour()*60 + u.Minute()
	if w.start < w.end {
		return m >= w.start && m < w.end
	}
	return m >= w.start || m < w.end
}

// Closes is when the window that holds at t ends; the zero time when it never
// ends (any time).
func (w Window) Closes(t time.Time) time.Time {
	if !w.set || w.start == w.end || w.never {
		return time.Time{}
	}
	u := t.UTC()
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	end := day.Add(time.Duration(w.end) * time.Minute)
	if !end.After(u) {
		end = end.Add(24 * time.Hour)
	}
	return end
}

// Opens is when the window next opens after t (t itself when it holds).
func (w Window) Opens(t time.Time) time.Time {
	if w.Contains(t) || w.never {
		return t
	}
	u := t.UTC()
	day := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	start := day.Add(time.Duration(w.start) * time.Minute)
	if !start.After(u) {
		start = start.Add(24 * time.Hour)
	}
	return start
}

// Job is a title the backfill hands out: what to measure (Path, an original
// or a package's video rendition, its HLS playlist), how long it is, and the
// token its run reports with (Report.Token). CostlyRuns are its runs in a
// row that had their whole lease and gave it back; Last says this run has
// its whole lease too and is the last: one that does not finish in it
// reports the title too costly to measure (unknown, sampling.doubt "cost")
// rather than give it back.
type Job struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	ItemID     string `json:"itemId"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	DurationMs *int64 `json:"durationMs"`
	Token      string `json:"token"`
	CostlyRuns int32  `json:"costlyRuns"`
	Last       bool   `json:"last"`
}

// UnknownAgain is how long an area the analyzer could not tell of stands
// before the title is measured again; MeasureAgainAfter the least time
// between two measurements of a title, whatever it plays since.
const (
	UnknownAgain      = 30 * 24 * time.Hour
	MeasureAgainAfter = 24 * time.Hour
)

// Claim is what a claim hands out: its titles and the time their runs end by
// (the window's end, or the lease's), or none and why, and when to ask again.
type Claim struct {
	Items             []Job      `json:"items"`
	Deadline          *time.Time `json:"deadline,omitempty"`
	Idle              string     `json:"idle,omitempty"`
	RetryAfterSeconds int        `json:"retryAfterSeconds,omitempty"`
}

// Service is the backfill, over the catalog of st and the library of cfg.
type Service struct {
	st    *store.Store
	paths library.Paths
	now   func() time.Time
}

// New is the backfill of the catalog st and the library cfg.
func New(st *store.Store, cfg config.Config) *Service {
	return &Service{st: st, paths: library.PathsOf(cfg), now: time.Now}
}

// WithClock is s with now as its clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Of reads a title's picture area (Of).
func (s *Service) Of(ctx context.Context, kind, id string) (*Area, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return Of(ctx, s.st.Pool(), kind, id)
}

// Record keeps a report of the analyzer's (Record).
func (s *Service) Record(ctx context.Context, kind, id string, r Report) (*Area, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	return Record(ctx, s.st, kind, id, r, s.now())
}

func (s *Service) ready(ctx context.Context) error {
	ok, err := s.st.PictureAreasReady(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotReady
	}
	return nil
}

// claimLock is the advisory lock a claim holds, the catalog's (its
// schema's): claims are counted against the pace one at a time.
const claimLock = `hashtext('picture-backfill/' || current_schema())`

// candidates is how many titles a claim looks at for each it hands out: one
// whose file can't be found fails (and waits its backoff), and the next goes.
const candidates = 8

// Claim hands out up to limit titles to the worker (1 at least, at most what
// the pace allows), or none and why: the backfill is off, outside its window,
// titles enough are in flight, the hour's rate is reached, or every title has
// a picture area of what it plays now. method is the analyzer's measurement
// ("" for any): an area of another is measured again. A claim of a lease that
// ended first counts as a failure of its title.
func (s *Service) Claim(ctx context.Context, worker string, limit int, method string) (Claim, error) {
	if err := s.ready(ctx); err != nil {
		return Claim{}, err
	}
	now := s.now().UTC()
	pool := s.st.Pool()
	set, err := ReadSettings(ctx, pool)
	if err != nil {
		return Claim{}, err
	}
	if !set.On {
		return idle(SettingBackfill+" is off", 15*time.Minute), nil
	}
	if !set.Window.Contains(now) {
		return idle("outside "+SettingWindow+" ("+set.Window.String()+")", set.Window.Opens(now).Sub(now)), nil
	}
	if set.Rate == 0 {
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
		FROM `+store.PictureAreasTable, now.Add(-time.Hour)).Scan(&inflight, &recent, &oldest); err != nil {
		return Claim{}, err
	}
	if inflight >= set.Inflight {
		return idle(fmt.Sprintf("%d titles are being measured (%s %d)", inflight, SettingInflight, set.Inflight),
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

	rows, err := tx.Query(ctx, candidatesSQL, now, n*candidates, clip(strings.TrimSpace(method), 32),
		now.Add(-UnknownAgain), now.Add(-MeasureAgainAfter))
	if err != nil {
		return Claim{}, err
	}
	type candidate struct {
		kind, id, itemID, typ string
		duration              *int64
		costly                int32
	}
	var cs []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.kind, &c.id, &c.itemID, &c.typ, &c.duration, &c.costly); err != nil {
			rows.Close()
			return Claim{}, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Claim{}, err
	}
	// the runs' deadline: the lease's end, or the window's when it comes
	// first (a run cut short so is no measure of its cost)
	deadline := now.Add(set.Lease)
	whole := true
	if closes := set.Window.Closes(now); !closes.IsZero() && closes.Before(deadline) {
		deadline, whole = closes, false
	}
	out := Claim{Items: []Job{}}
	for _, c := range cs {
		if len(out.Items) == n {
			break
		}
		path, why, err := s.fileOf(ctx, tx, c.kind, c.id)
		if err != nil {
			return Claim{}, err
		}
		if path == "" {
			if err := fail(ctx, tx, c.kind, c.id, c.itemID, "nothing to measure: "+why, now); err != nil {
				return Claim{}, err
			}
			continue
		}
		var token string
		if err := tx.QueryRow(ctx, `INSERT INTO `+store.PictureAreasTable+`
				(id, kind, item_id, state, claimedat, claimedby, claimtoken, lastclaimedat, attempts, createdat, modifiedat)
			VALUES ($1, $2, $3, 'pending', $4, $5, gen_random_uuid()::varchar, $4, 1, $4, $4)
			ON CONFLICT (id) DO UPDATE SET claimedat = $4, claimedby = $5, claimtoken = gen_random_uuid()::varchar,
				lastclaimedat = $4, attempts = `+store.PictureAreasTable+`.attempts + 1, modifiedat = $4
			RETURNING claimtoken`,
			c.id, c.kind, c.itemID, now, clip(worker, 255)).Scan(&token); err != nil {
			return Claim{}, err
		}
		out.Items = append(out.Items, Job{Kind: c.kind, ID: c.id, ItemID: c.itemID, Type: c.typ, Path: path,
			DurationMs: c.duration, Token: token, CostlyRuns: c.costly, Last: whole && c.costly+1 >= CostlyRuns})
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, err
	}
	if len(out.Items) == 0 {
		if len(cs) == 0 {
			return idle("every title has a picture area of what it plays now", time.Hour), nil
		}
		return idle("the titles looked at had nothing to measure", time.Minute), nil
	}
	out.Deadline = &deadline
	return out, nil
}

func idle(why string, after time.Duration) Claim {
	return Claim{Items: []Job{}, Idle: why, RetryAfterSeconds: max(1, int(after.Round(time.Second)/time.Second))}
}

// candidatesSQL are the titles to measure, $2 of them at most, in the order
// they are handed out: movies, episodes (none another's file covers, none of a
// disc image), extras (packaged, not removed), each with a file, without a
// picture area of what it plays now (measured, or unknown: the analyzer could
// not tell) of the method $3 ("" for any), one unknown since before $4 (a
// month ago) counting as none unless a package could not tell for its
// keyframes' gaps or the title was too costly to measure (sampling.doubt
// "cost": those answers stay until the file or the method changes), one a
// package could not tell counting as none while the title has its original,
// not claimed now, not waiting out a failure's or a release's backoff ($1 is
// now), and not measured since $5 (a day ago); the titles with their original
// first, then those never measured; with each, its costly runs in a row.
var candidatesSQL = `
	SELECT t.kind, t.id, t.item_id, t.type, t.durationms, COALESCE(pa.costlyruns, 0)
	FROM (
		SELECT 'item' AS kind, i.id, i.id AS item_id, i.type, i.durationms,
		       CASE i.type WHEN 'movie' THEN 0 ELSE 1 END AS rank,
		       EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
		               WHERE a.item_id = i.id AND a.isprimary AND COALESCE(a.kind, 'primary') = 'primary') AS original
		FROM com_nalet_katalog_items i
		WHERE i.type IN ('movie', 'episode') AND i.coveredby IS NULL AND NOT ` + processing.DiscImageOf("i.id") + `
		  AND (EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
		               WHERE v.item_id = i.id AND v.state = 'complete' AND v.dir IS NOT NULL)
		    OR EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
		               WHERE a.item_id = i.id AND ((a.isprimary AND COALESCE(a.kind, 'primary') = 'primary')
		                                          OR a.kind = 'packaged')))
		UNION ALL
		SELECT 'extra', x.id, x.item_id, 'extra', x.durationms, 2, false
		FROM com_nalet_katalog_itemextras x
		WHERE x.removedat IS NULL AND x.packagedat IS NOT NULL AND x.packagepath IS NOT NULL
	) t
	LEFT JOIN ` + store.PictureAreasTable + ` pa ON pa.id = t.id
	WHERE NOT EXISTS (SELECT 1 FROM ` + store.CurrentPictureAreasView + ` c
	                  WHERE c.id = t.id AND c.state IN ('measured', 'unknown')
	                    AND ($3 = '' OR c.method = $3)
	                    AND (c.state = 'measured' OR c.measuredat > $4
	                      OR (c.measuredon = 'package' AND c.sampling->'doubt' @> '["gap"]'::jsonb)
	                      OR c.sampling->'doubt' @> '["cost"]'::jsonb)
	                    AND NOT (t.original AND c.state = 'unknown' AND c.measuredon IS DISTINCT FROM 'original'))
	  AND (pa.id IS NULL OR (pa.claimedat IS NULL AND (pa.nextattemptat IS NULL OR pa.nextattemptat <= $1)
	                         AND (pa.measuredat IS NULL OR pa.measuredat <= $5)))
	ORDER BY t.original DESC, pa.measuredat IS NOT NULL, t.rank, t.id
	LIMIT $2`

// expire takes the claims whose lease ended for failures of their titles:
// their runs never reported back. A claim's token is kept as the lapsed one:
// its run's failure, reported late, is counted already.
func expire(ctx context.Context, tx pgx.Tx, now time.Time, lease time.Duration) error {
	rows, err := tx.Query(ctx, `SELECT id, failures FROM `+store.PictureAreasTable+`
		WHERE claimedat IS NOT NULL AND claimedat <= $1 FOR UPDATE`, now.Add(-lease))
	if err != nil {
		return err
	}
	type lost struct {
		id       string
		failures int32
	}
	var ls []lost
	for rows.Next() {
		var l lost
		if err := rows.Scan(&l.id, &l.failures); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range ls {
		f := l.failures + 1
		if _, err := tx.Exec(ctx, `UPDATE `+store.PictureAreasTable+` SET claimedat = NULL, claimedby = NULL,
			lapsedtoken = claimtoken, claimtoken = NULL, failures = $2, nextattemptat = $3, modifiedat = $4,
			state = CASE WHEN state = 'pending' THEN 'failed' ELSE state END,
			reason = CASE WHEN state IN ('pending', 'failed') THEN $5 ELSE reason END
			WHERE id = $1`, l.id, f, now.Add(backoff(f)), now,
			"its run did not report back within "+library.DurationText(lease)); err != nil {
			return err
		}
	}
	return nil
}

// fail counts a failure of a title the backfill could not hand out (there is
// nothing to measure), which waits its backoff.
func fail(ctx context.Context, tx pgx.Tx, kind, id, itemID, why string, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO `+store.PictureAreasTable+`
			(id, kind, item_id, state, reason, failures, nextattemptat, createdat, modifiedat)
		VALUES ($1, $2, $3, 'failed', $4, 1, $5, $6, $6)
		ON CONFLICT (id) DO UPDATE SET state = 'failed', reason = $4,
			failures = `+store.PictureAreasTable+`.failures + 1,
			nextattemptat = $6::timestamptz + make_interval(secs => LEAST(
				$7 * power(2, LEAST(`+store.PictureAreasTable+`.failures, 30)), $8)),
			modifiedat = $6`,
		id, kind, itemID, clip(why, 500), now.Add(backoff(1)), now, FirstBackoff.Seconds(), MaxBackoff.Seconds())
	return err
}

// fileOf is the file the backfill measures of a title: a movie's or an
// episode's original while it has one on disk, else its package (its complete
// version's video rendition, or that of the package from before the library);
// an extra's package. "" and why when there is none to read.
func (s *Service) fileOf(ctx context.Context, q pgx.Tx, kind, id string) (string, string, error) {
	if kind == KindExtra {
		var dir, pid, src *string
		var gone *time.Time
		err := q.QueryRow(ctx, `SELECT packagepath, packageid, sourcepath, sourcedeletedat
			FROM com_nalet_katalog_itemextras WHERE id = $1`, id).Scan(&dir, &pid, &src, &gone)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "the extra is gone", nil
		}
		if err != nil {
			return "", "", err
		}
		if dir != nil {
			record := "manifest.json"
			if pid != nil {
				record = library.PackageFile
			}
			if p := videoPlaylist(*dir, record); p != "" {
				return p, "", nil
			}
		}
		return "", "its package has no video rendition to read", nil
	}
	var packaged, original *string
	err := q.QueryRow(ctx, `SELECT
			(SELECT path FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged' ORDER BY path LIMIT 1),
			(SELECT path FROM com_nalet_katalog_playbackassets
			 WHERE item_id = $1 AND isprimary AND COALESCE(kind, 'primary') = 'primary' ORDER BY path LIMIT 1)`, id).
		Scan(&packaged, &original)
	if err != nil {
		return "", "", err
	}
	missing := "it has no original"
	if original != nil {
		at := s.originalAt(ctx, q, id, *original)
		if fi, err := os.Stat(at); err == nil && fi.Mode().IsRegular() {
			return at, "", nil
		}
		missing = "its original is not at " + at
	}
	var vdir *string
	if err := q.QueryRow(ctx, `SELECT dir FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 AND state = 'complete' AND dir IS NOT NULL ORDER BY id LIMIT 1`, id).Scan(&vdir); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	if vdir != nil {
		if p := videoPlaylist(*vdir, library.PackageFile); p != "" {
			return p, "", nil
		}
	}
	if vdir == nil && packaged != nil && filepath.Base(*packaged) == "manifest.json" {
		if p := videoPlaylist(filepath.Dir(*packaged), "manifest.json"); p != "" {
			return p, "", nil
		}
	}
	return "", missing + ", and its package can't be read", nil
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

// videoPlaylist is the HLS playlist of the largest video rendition of the
// package in dir, as its record names it; "" when it names none that is
// there.
func videoPlaylist(dir, record string) string {
	b, err := os.ReadFile(filepath.Join(dir, record))
	if err != nil {
		return ""
	}
	var pkg struct {
		Renditions struct {
			Video []struct {
				Dir    string `json:"dir"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"video"`
		} `json:"renditions"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return ""
	}
	best, area := "", -1
	for _, v := range pkg.Renditions.Video {
		if v.Dir == "" || strings.Contains(v.Dir, "..") || filepath.IsAbs(v.Dir) {
			continue
		}
		if a := v.Width * v.Height; a > area {
			best, area = v.Dir, a
		}
	}
	if best == "" {
		return ""
	}
	p := filepath.Join(dir, filepath.FromSlash(best), "playlist.m3u8")
	if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return p
}
