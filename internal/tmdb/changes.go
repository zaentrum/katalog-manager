package tmdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Keeping people and titles fresh from TMDB's change lists. TMDB lists, per
// kind (person, movie, tv), the ids of everything that changed in a range of
// days. Each list has a cursor in com_nalet_katalog_referencesync: a run reads
// the list from the cursor up to today, refreshes what of it the catalog holds
// (people by their TMDB id; movies and series by their TMDB external id, by
// running their enrichment again) and only then moves the cursor to today.
// The cursor's own day is read again by the next run, as it may have changed
// after this one. A list without a cursor starts today: no historical sweep.

// changeKinds are TMDB's change lists, in the order a run reads them.
var changeKinds = []string{"person", "movie", "tv"}

const (
	// maxChangeDays is the longest range TMDB serves in one call.
	maxChangeDays = 14
	// maxChangePages is the last page TMDB serves of a list.
	maxChangePages = 500
	// changeSyncLock is the advisory lock that lets one instance run at a time.
	// Its second key is the catalog's schema: catalogs in other schemas of the
	// same database, such as tests, do not wait for each other.
	changeSyncLock = int32(0x6b6d746d) // "kmtm"
)

// errSyncBusy says another instance of the service is running the change lists.
var errSyncBusy = errors.New("another instance is reading TMDB's change lists")

// window is a range of days, both ends included.
type window struct{ from, to time.Time }

func (w window) days() int { return int(w.to.Sub(w.from).Hours()/24) + 1 }

// changeWindows covers from..to with windows of at most maxChangeDays days. A
// window starts on the day the one before it ended, so no day falls between
// two whichever end TMDB counts.
func changeWindows(from, to time.Time) []window {
	var out []window
	for start := from; !start.After(to); {
		end := start.AddDate(0, 0, maxChangeDays-1)
		if end.After(to) {
			end = to
		}
		out = append(out, window{start, end})
		if !end.Before(to) {
			break
		}
		start = end
	}
	return out
}

// utcDay is the day t falls on in UTC, at midnight.
func utcDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// syncRun is what one run did with one change list.
type syncRun struct {
	kind                                         string
	from, to                                     time.Time
	changes, matched, refreshed, skipped, failed int
	err                                          error  // the run did not get through, so the cursor stays
	note                                         string // something worth knowing that did not stop it
}

// SyncChanges reads TMDB's person, movie and tv change lists, each from its
// cursor up to the day now falls on (UTC), and refreshes what of them the
// catalog holds. One instance of the service runs it at a time.
func (s *Service) SyncChanges(ctx context.Context, now time.Time) ([]syncRun, error) {
	if !s.tmdb.enabled() {
		return nil, errors.New("TMDB API key not configured")
	}
	if !s.peopleReady(ctx) {
		return nil, errors.New("the people migration (db/migrations/030_people.sql) is not applied")
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var mine bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, hashtext(current_schema()))`, changeSyncLock).
		Scan(&mine); err != nil || !mine {
		conn.Release()
		if err == nil {
			err = errSyncBusy
		}
		return nil, err
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, hashtext(current_schema()))`,
			changeSyncLock); err != nil {
			conn.Conn().Close(context.Background()) // never hand back a session that may still hold the lock
		}
		conn.Release()
	}()

	today := utcDay(now)
	var runs []syncRun
	for _, kind := range changeKinds {
		run := s.syncKind(ctx, kind, today)
		runs = append(runs, run)
		msg := ""
		if run.err != nil {
			msg = "; the cursor stays: " + run.err.Error()
		} else if run.note != "" {
			msg = "; " + run.note
		}
		log.Printf("tmdb: %s changes %s..%s: %d listed, %d held, %d refreshed, %d skipped, %d failed%s",
			kind, run.from.Format(time.DateOnly), run.to.Format(time.DateOnly),
			run.changes, run.matched, run.refreshed, run.skipped, run.failed, msg)
	}
	return runs, nil
}

// syncKind runs one change list: it reads it from the cursor to today, and
// refreshes what of it the catalog holds. The cursor moves to today only when
// the whole list was read and every refresh went through; otherwise the next
// run reads the same days again.
func (s *Service) syncKind(ctx context.Context, kind string, today time.Time) syncRun {
	run := syncRun{kind: kind, from: today, to: today}
	var cursor time.Time
	err := s.pool.QueryRow(ctx, `SELECT cursor FROM com_nalet_katalog_referencesync WHERE kind = $1`, kind).Scan(&cursor)
	switch {
	case err == nil:
		if c := utcDay(cursor); c.Before(today) {
			run.from = c
		}
	case errors.Is(err, pgx.ErrNoRows):
		// first run: the cursor starts today
	default:
		run.err = fmt.Errorf("read the cursor: %w", err)
		return run // nothing to save it with
	}

	hits, err := s.readChanges(ctx, kind, &run)
	if err != nil {
		run.err = err
	} else {
		run.matched = len(hits)
		for _, h := range hits {
			switch refreshed, err := s.refreshChanged(ctx, kind, h); {
			case err != nil:
				run.failed++
				log.Printf("tmdb: %s changes: refresh %s (TMDB %d): %v", kind, h.entityID, h.tmdbID, err)
			case refreshed:
				run.refreshed++
			default:
				run.skipped++
			}
		}
		if run.failed > 0 {
			run.err = fmt.Errorf("%d of %d refreshes failed", run.failed, run.matched)
		}
	}

	next := run.from
	if run.err == nil {
		next = today
	}
	var lastErr *string
	if run.err != nil {
		e := run.err.Error()
		lastErr = &e
	} else if run.note != "" {
		lastErr = &run.note
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_referencesync
			(kind, cursor, lastrunat, lastrunchanges, lastrunmatched, lastrunrefreshed, lastrunskipped, lastrunfailed, lastrunerror)
		VALUES ($1, $2, now(), $3, $4, $5, $6, $7, $8)
		ON CONFLICT (kind) DO UPDATE SET cursor = EXCLUDED.cursor, lastrunat = EXCLUDED.lastrunat,
			lastrunchanges = EXCLUDED.lastrunchanges, lastrunmatched = EXCLUDED.lastrunmatched,
			lastrunrefreshed = EXCLUDED.lastrunrefreshed, lastrunskipped = EXCLUDED.lastrunskipped,
			lastrunfailed = EXCLUDED.lastrunfailed, lastrunerror = EXCLUDED.lastrunerror`,
		kind, next.Format(time.DateOnly), run.changes, run.matched, run.refreshed, run.skipped, run.failed, lastErr); err != nil {
		run.err = errors.Join(run.err, fmt.Errorf("save the cursor: %w", err))
	}
	return run
}

// changeHit is something a change list named that the catalog holds.
type changeHit struct {
	entityID string // the person's or the item's id
	tmdbID   int64
	day      time.Time // the last day of the window that listed it
}

// readChanges reads a change list over run's days, a window and a page at a
// time, and returns what of it the catalog holds, in order. A window with
// more pages than TMDB serves is read again a day at a time; a single day with
// more is read as far as TMDB serves it, and the run says so.
func (s *Service) readChanges(ctx context.Context, kind string, run *syncRun) ([]changeHit, error) {
	listed := map[int64]bool{}
	held := map[string]changeHit{}
	queue := changeWindows(run.from, run.to)
	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]
		ids, pages, err := s.tmdb.getChanges(ctx, kind, w.from, w.to, 1)
		if err != nil {
			return nil, fmt.Errorf("read %s..%s: %w", w.from.Format(time.DateOnly), w.to.Format(time.DateOnly), err)
		}
		if pages > s.pageCap() && w.days() > 1 {
			var days []window
			for d := w.from; !d.After(w.to); d = d.AddDate(0, 0, 1) {
				days = append(days, window{d, d})
			}
			queue = append(days, queue...)
			continue
		}
		if pages > s.pageCap() {
			run.note = fmt.Sprintf("%s lists more than %d pages; read the first %d", w.from.Format(time.DateOnly), s.pageCap(), s.pageCap())
			pages = s.pageCap()
		}
		for page := 2; page <= pages; page++ {
			more, _, err := s.tmdb.getChanges(ctx, kind, w.from, w.to, page)
			if err != nil {
				return nil, fmt.Errorf("read %s..%s page %d: %w", w.from.Format(time.DateOnly), w.to.Format(time.DateOnly), page, err)
			}
			ids = append(ids, more...)
		}
		for _, id := range ids {
			listed[id] = true
		}
		hits, err := s.heldOf(ctx, kind, ids)
		if err != nil {
			return nil, err
		}
		for _, h := range hits {
			h.day = w.to
			held[h.entityID] = h // a later window names a later day
		}
	}
	run.changes = len(listed)
	out := make([]changeHit, 0, len(held))
	for _, h := range held {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].entityID < out[j].entityID })
	return out, nil
}

// pageCap is the last page TMDB serves; a test lowers it.
func (s *Service) pageCap() int {
	if s.maxChangePages > 0 {
		return s.maxChangePages
	}
	return maxChangePages
}

// heldOf returns the people (kind person) or the movies or series (movie, tv)
// the catalog holds with one of the TMDB ids.
func (s *Service) heldOf(ctx context.Context, kind string, ids []int64) ([]changeHit, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = strconv.FormatInt(id, 10)
	}
	query, args := `SELECT id, tmdbpersonid FROM com_nalet_katalog_people WHERE tmdbpersonid = ANY($1::text[])`, []any{keys}
	if kind != "person" {
		typ := "movie"
		if kind == "tv" {
			typ = "series"
		}
		query = `SELECT i.id, btrim(e.externalid) FROM com_nalet_katalog_items i
			JOIN com_nalet_katalog_itemexternalids e ON e.item_id = i.id AND e.source = 'tmdb'
			WHERE i.type = $2 AND btrim(e.externalid) = ANY($1::text[])`
		args = append(args, typ)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []changeHit
	for rows.Next() {
		var h changeHit
		var key string
		if err := rows.Scan(&h.entityID, &key); err != nil {
			return nil, err
		}
		if h.tmdbID, err = strconv.ParseInt(key, 10, 64); err == nil {
			out = append(out, h)
		}
	}
	return out, rows.Err()
}

// refreshChanged refreshes what a change list named: a person from their TMDB
// details, a movie or series by running its enrichment again. Locks are
// honoured both ways. refreshed false without an error is a skip: the person's
// record is locked, or TMDB no longer knows what it named.
func (s *Service) refreshChanged(ctx context.Context, kind string, h changeHit) (refreshed bool, err error) {
	if kind == "person" {
		day := h.day
		out, err := s.refreshPerson(ctx, h.entityID, h.tmdbID, &day)
		return err == nil && out == personRefreshed, err
	}
	status, msg, err := s.EnrichOne(ctx, h.entityID)
	switch {
	case err != nil:
		return false, err
	case status == statusFailed:
		return false, errors.New(msg)
	}
	return status == statusDone, nil
}

// RunChangeSync keeps people and titles fresh: it runs SyncChanges once an
// interval has passed since the lists' last run, so a restart neither skips
// nor repeats a day. It waits a minute after startup, and while there is no
// TMDB key or the people migration is missing it checks back every hour. An
// interval of 0 disables it.
func (s *Service) RunChangeSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		log.Printf("tmdb: change lists: disabled (TMDB_REFRESH_INTERVAL)")
		return
	}
	retry := min(interval, time.Hour)
	wait, waiting := time.Minute, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !s.tmdb.enabled() || !s.peopleReady(ctx) {
			if !waiting {
				log.Printf("tmdb: change lists: waiting for a TMDB key and migration 030")
				waiting = true
			}
			wait = retry
			continue
		}
		waiting = false
		last, err := s.lastSyncRuns(ctx)
		if err != nil {
			log.Printf("tmdb: change lists: %v", err)
			wait = retry
			continue
		}
		if wait = dueIn(last, interval, time.Now()); wait > 0 {
			continue
		}
		if _, err := s.SyncChanges(ctx, time.Now()); err != nil {
			log.Printf("tmdb: change lists: %v", err)
			wait = retry
			continue
		}
		wait = interval
	}
}

// lastSyncRuns is when each change list last ran.
func (s *Service) lastSyncRuns(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, lastrunat FROM com_nalet_katalog_referencesync WHERE lastrunat IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var kind string
		var at time.Time
		if err := rows.Scan(&kind, &at); err != nil {
			return nil, err
		}
		out[kind] = at
	}
	return out, rows.Err()
}

// dueIn is how long until the change lists run again: an interval after the
// earliest of their last runs, at once when one never ran.
func dueIn(last map[string]time.Time, interval time.Duration, now time.Time) time.Duration {
	var earliest time.Time
	for i, kind := range changeKinds {
		at, ok := last[kind]
		if !ok {
			return 0
		}
		if i == 0 || at.Before(earliest) {
			earliest = at
		}
	}
	return max(earliest.Add(interval).Sub(now), 0)
}
