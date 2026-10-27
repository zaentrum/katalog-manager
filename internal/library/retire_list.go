package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// An admin's list of titles whose originals are retired (GraphQL
// retireOriginals, migration 052). library.originals keeps every original
// (keep, its default) and stays as it is: a list retires exactly the
// originals of the titles it names, each through the retire job's own path
// (retireOf: its claim, its checks of the original and of the version, its
// event, the trash and its grace, the source's row, what the package lacks of
// it recorded), on the word of the admin who asked. A title's original is
// retired only when every condition holds (check); else the title is
// skipped, saying why. A dry run, the default, checks every title and retires
// none, touching nothing but the run's own rows.
//
// One run runs at a time (idx_retireruns_running), in one instance at a time
// (an advisory lock), in the background: it goes on when the admin goes away,
// and a run a restart interrupted goes on where it stopped - a retirement it
// began is resumed (or, called off meanwhile, its title is checked again).
// It retires one original at a time, each while the retire job runs no pass
// (the job's own lock), at most library.retire.rate a minute (the job's pace).
//
// The library's name of an original is all it retires: a file with more than
// one name (a hard link another tree shares) is retired by that name, moved
// into the trash, and its other names are none of the library's.

// The states of a run: it runs, it is done (every title has a result), or it
// failed: it could not go on (error says why).
const (
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
)

// What a run did with a title: its original retired, the title skipped (a
// condition does not hold), its retirement failed (the retire path refused
// it, saying why: nothing is deleted before its event is recorded), or, in a
// dry run, every condition holds.
const (
	ListRetired     = "retired"
	ListSkipped     = "skipped"
	ListFailed      = "failed"
	ListWouldRetire = "would-retire"
)

// MaxList is the most titles one list names.
const MaxList = 10000

// listPhaseRetiring marks a title whose retirement a run began: a run that
// resumes goes on with it.
const listPhaseRetiring = "retiring"

// RetireListedBy is who retires an original of an admin's list, as its event
// and its row say: by, the admin.
func RetireListedBy(by string) string { return "katalog-manager (retireOriginals, by " + by + ")" }

// RetireListedReason is why, as the event says.
const RetireListedReason = "an admin listed it to be retired (retireOriginals): the package is the record"

// ListRun is a run of an admin's list: its state (RunRunning, RunDone,
// RunFailed), whether it is a dry run, who asked, when it began and ended,
// why a failed run stopped, its titles by result (Pending those still to
// come), and the size of the originals it retired (of a dry run, would
// retire).
type ListRun struct {
	ID, State, RequestedBy                        string
	DryRun                                        bool
	StartedAt                                     time.Time
	FinishedAt                                    *time.Time
	Error                                         *string
	Total, Pending, Retired, WouldRetire, Skipped int
	Failed                                        int
	Bytes                                         int64
}

// ListItem is a title of a run's list: its place in it (from 1), its id and
// its name when the run began, the original the run retires of it, its
// result (nil while it waits), why, its original's size, and when it got its
// result.
type ListItem struct {
	Position        int
	ItemID          string
	Title, SourceID *string
	Result, Reason  *string
	Bytes           *int64
	At              *time.Time
}

// ListRefused is why a list is not run: Code (UNAVAILABLE, LEGACY, EMPTY,
// TOO_MANY, RUNNING) and a sentence.
type ListRefused struct{ Code, Msg string }

func (e *ListRefused) Error() string { return e.Msg }

// ComplianceGate is the library's compliance check, as a list asks it of
// each title before it retires its original (implemented by compliance):
// "" of a title it found within every rule under the rules the workers
// report now, else why not.
type ComplianceGate interface {
	Passed(ctx context.Context, ids []string) (map[string]string, error)
}

// RetireList runs admins' lists of titles whose originals are retired.
type RetireList struct {
	pool *pgxpool.Pool
	cfg  config.Config
	r    *Retirer // its own: the job's notes nothing of a list's
	gate ComplianceGate
	now  func() time.Time
	// sleep waits d, false when ctx ends first: the pace, and the wait for
	// the retire job's pass.
	sleep func(ctx context.Context, d time.Duration) bool
	pokes chan struct{}
	tick  time.Duration
	// lockWait is how long it waits before it tries the retire job's lock
	// again.
	lockWait time.Duration
	said     string
}

// NewRetireList runs the lists of the catalog's pool, at the paths cfg
// configures, the retire step through steps, each title's compliance asked
// of gate.
func NewRetireList(pool *pgxpool.Pool, cfg config.Config, steps *processing.Steps, gate ComplianceGate) *RetireList {
	return &RetireList{pool: pool, cfg: cfg, r: NewRetirer(pool, cfg, steps), gate: gate, now: time.Now, sleep: sleepCtx,
		pokes: make(chan struct{}, 1), tick: 30 * time.Second, lockWait: 2 * time.Second}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ready reports whether migration 052 is in place, and 040 it reads by.
func (l *RetireList) ready(ctx context.Context) (bool, error) {
	var ok bool
	err := l.pool.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_retireruns') IS NOT NULL
		AND to_regclass('com_nalet_katalog_retirerunitems') IS NOT NULL
		AND to_regclass('com_nalet_katalog_itemsources') IS NOT NULL`).Scan(&ok)
	return ok, err
}

// Start runs a list of the titles ids, a dry run or not, on the word of by:
// it is noted, and runs in the background (Run). It answers the run as it
// begins; ListRefused while one runs already, of no title or of more than
// MaxList, with library.layout legacy, or without migration 052.
func (l *RetireList) Start(ctx context.Context, ids []string, dryRun bool, by string) (ListRun, error) {
	if ok, err := l.ready(ctx); err != nil {
		return ListRun{}, err
	} else if !ok {
		return ListRun{}, &ListRefused{"UNAVAILABLE", "migration 052 (db/migrations/052_retire_runs.sql) is not applied: " +
			"no list of originals is retired"}
	}
	set, err := ReadSettings(ctx, l.pool)
	if err != nil {
		return ListRun{}, err
	}
	if !set.V2() {
		return ListRun{}, &ListRefused{"LEGACY", "library.layout is legacy: the library keeps no records of originals and " +
			"packages, so no original is retired"}
	}
	var list []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			list = append(list, id)
		}
	}
	switch {
	case len(list) == 0:
		return ListRun{}, &ListRefused{"EMPTY", "no title is listed"}
	case len(list) > MaxList:
		return ListRun{}, &ListRefused{"TOO_MANY", fmt.Sprintf("%d titles are listed, and a list names %d at most", len(list),
			MaxList)}
	}
	names, err := TitlesOf(ctx, l.pool, list)
	if err != nil {
		return ListRun{}, err
	}
	named := make([]*string, len(list))
	for i, id := range list {
		if n, ok := names[id]; ok {
			n := clip(n, 500)
			named[i] = &n
		}
	}
	id := NewID()
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return ListRun{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_retireruns (id, dryrun, requestedby) VALUES ($1, $2, $3)`,
		id, dryRun, clip(by, 255)); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return ListRun{}, l.refusedRunning(ctx)
		}
		return ListRun{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_retirerunitems (run_id, position, item_id, title)
		SELECT $1, x.position, x.id, t.title
		FROM unnest($2::text[]) WITH ORDINALITY AS x(id, position)
		JOIN unnest($3::text[]) WITH ORDINALITY AS t(title, position) ON t.position = x.position`, id, list, named); err != nil {
		return ListRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ListRun{}, err
	}
	l.poke()
	run, err := l.runOf(ctx, id)
	if err != nil || run == nil {
		return ListRun{}, errors.Join(err, errors.New("the run was not noted"))
	}
	return *run, nil
}

// refusedRunning is the refusal of a list while another runs.
func (l *RetireList) refusedRunning(ctx context.Context) error {
	run, err := l.Latest(ctx)
	if err != nil {
		return err
	}
	msg := "a list of originals is being retired already: wait for it to finish"
	if run != nil && run.State == RunRunning {
		kind := "a list"
		if run.DryRun {
			kind = "a dry run of a list"
		}
		msg = fmt.Sprintf("%s of %s runs since %s, %d of them still to come: wait for it to finish", kind, titles(run.Total),
			Timestamp(run.StartedAt), run.Pending)
	}
	return &ListRefused{"RUNNING", msg}
}

// titles counts titles: "1 title", "12 titles".
func titles(n int) string {
	if n == 1 {
		return "1 title"
	}
	return fmt.Sprintf("%d titles", n)
}

// poke wakes the background runner, without waiting.
func (l *RetireList) poke() {
	select {
	case l.pokes <- struct{}{}:
	default:
	}
}

const runCols = `r.id, r.state, r.requestedby, r.dryrun, r.startedat, r.finishedat, r.error,
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id),
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id AND i.result IS NULL),
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id AND i.result = 'retired'),
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id AND i.result = 'would-retire'),
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id AND i.result = 'skipped'),
	(SELECT count(*) FROM com_nalet_katalog_retirerunitems i WHERE i.run_id = r.id AND i.result = 'failed'),
	(SELECT COALESCE(sum(i.bytes), 0) FROM com_nalet_katalog_retirerunitems i
		WHERE i.run_id = r.id AND i.result IN ('retired', 'would-retire'))`

func scanRun(row pgx.Row) (*ListRun, error) {
	var r ListRun
	err := row.Scan(&r.ID, &r.State, &r.RequestedBy, &r.DryRun, &r.StartedAt, &r.FinishedAt, &r.Error, &r.Total, &r.Pending,
		&r.Retired, &r.WouldRetire, &r.Skipped, &r.Failed, &r.Bytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (l *RetireList) runOf(ctx context.Context, id string) (*ListRun, error) {
	return scanRun(l.pool.QueryRow(ctx, `SELECT `+runCols+` FROM com_nalet_katalog_retireruns r WHERE r.id = $1`, id))
}

// Latest is the run begun last, running or not; nil when none ever ran (or
// without migration 052).
func (l *RetireList) Latest(ctx context.Context) (*ListRun, error) {
	if ok, err := l.ready(ctx); err != nil || !ok {
		return nil, err
	}
	return scanRun(l.pool.QueryRow(ctx, `SELECT `+runCols+` FROM com_nalet_katalog_retireruns r
		ORDER BY r.startedat DESC, r.id DESC LIMIT 1`))
}

// Items are the titles of the run id after the place after, at most first
// (1 to 500), in the list's order: of result alone when it is one ("" any;
// "pending" those still to come).
func (l *RetireList) Items(ctx context.Context, id string, first, after int, result string) ([]ListItem, error) {
	first = min(max(first, 1), 500)
	rows, err := l.pool.Query(ctx, `SELECT position, item_id, title, source_id, result, reason, bytes, at
		FROM com_nalet_katalog_retirerunitems
		WHERE run_id = $1 AND position > $2
		  AND ($3 = '' OR ($3 = 'pending' AND result IS NULL) OR result = $3)
		ORDER BY position LIMIT $4`, id, max(after, 0), result, first)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ListItem{}
	for rows.Next() {
		var it ListItem
		if err := rows.Scan(&it.Position, &it.ItemID, &it.Title, &it.SourceID, &it.Result, &it.Reason, &it.Bytes, &it.At); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Run runs the lists in the background until ctx ends: the one that runs,
// when it is woken (Start) or every tick, a run a restart interrupted too.
func (l *RetireList) Run(ctx context.Context) {
	t := time.NewTicker(l.tick)
	defer t.Stop()
	for {
		if err := l.Work(ctx); err != nil && ctx.Err() == nil {
			l.say("library: a list of originals to retire stopped: " + err.Error() + "; it goes on at the next try")
		} else if err == nil {
			l.say("")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.pokes:
		}
	}
}

func (l *RetireList) say(msg string) {
	if msg != l.said && msg != "" {
		log.Print(msg)
	}
	l.said = msg
}

// listLock is the advisory lock of the instance that runs the lists, the
// catalog's (its schema's).
const listLock = `hashtext('library-retire-list/' || current_schema())`

// Work runs the run that runs, title by title, until every title has its
// result, in one instance at a time: another instance at it, it does
// nothing. It answers once the run is done (or there is none), or ctx ended.
func (l *RetireList) Work(ctx context.Context) error {
	if ok, err := l.ready(ctx); err != nil || !ok {
		return err
	}
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+listLock+`)`).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+listLock+`)`) }()
	var p pace
	var at runAt
	for ctx.Err() == nil {
		run, err := l.current(ctx)
		if err != nil || run == nil {
			return err
		}
		if run.id != at.id {
			at = runAt{id: run.id}
		}
		set, err := ReadSettings(ctx, l.pool)
		if err != nil {
			return err
		}
		if !set.V2() {
			return l.finish(ctx, run.id, RunFailed, "library.layout is legacy: the library keeps no records of originals "+
				"and packages, so no original is retired")
		}
		it, err := l.next(ctx, run.id, at.after)
		if err != nil {
			return err
		}
		if it == nil {
			if err := l.finish(ctx, run.id, RunDone, ""); err != nil {
				return err
			}
			continue
		}
		res, err := l.title(ctx, conn, run, it, set, &p)
		if err != nil {
			return err
		}
		if res.result == "" {
			return ctx.Err() // ended before it had a result: the next run of Work goes on with it
		}
		if err := l.note(context.WithoutCancel(ctx), run.id, it.position, res); err != nil {
			return err
		}
		at.after = it.position
	}
	return ctx.Err()
}

// running is the run that runs, as Work reads it: its id, whether it is a
// dry run, and who asked.
type running struct {
	id, by string
	dry    bool
}

// runAt is how far Work got in the run id: the titles up to after have
// their results.
type runAt struct {
	id    string
	after int
}

// current is the run that runs; nil when none does.
func (l *RetireList) current(ctx context.Context) (*running, error) {
	var r running
	err := l.pool.QueryRow(ctx, `SELECT id, requestedby, dryrun FROM com_nalet_katalog_retireruns
		WHERE state = 'running' ORDER BY startedat, id LIMIT 1`).Scan(&r.id, &r.by, &r.dry)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// pace counts the retirements of the minute: at most library.retire.rate.
type pace struct {
	start time.Time
	n     int
}

// paced waits, when the minute's retirements are as many as rate, for the
// next minute; false when ctx ended first.
func (l *RetireList) paced(ctx context.Context, p *pace, rate int) bool {
	now := l.now()
	if p.start.IsZero() || now.Sub(p.start) >= time.Minute {
		p.start, p.n = now, 0
	}
	if p.n >= max(rate, 1) {
		if !l.sleep(ctx, p.start.Add(time.Minute).Sub(now)) {
			return false
		}
		p.start, p.n = l.now(), 0
	}
	p.n++
	return true
}

// listItem is a title of the run, as it waits for its result.
type listItem struct {
	position int
	itemID   string
	sourceID *string
	phase    *string
}

// next is the run's first title after the place after that waits for its
// result; nil when none does.
func (l *RetireList) next(ctx context.Context, run string, after int) (*listItem, error) {
	var it listItem
	err := l.pool.QueryRow(ctx, `SELECT position, item_id, source_id, phase FROM com_nalet_katalog_retirerunitems
		WHERE run_id = $1 AND position > $2 AND result IS NULL ORDER BY position LIMIT 1`, run, after).
		Scan(&it.position, &it.itemID, &it.sourceID, &it.phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// outcome is a title's result: what, why, its original and its size.
type outcome struct {
	result, reason string
	source         *string
	bytes          *int64
}

func (l *RetireList) note(ctx context.Context, run string, position int, o outcome) error {
	var reason *string
	if o.reason != "" {
		r := clip(o.reason, 1000)
		reason = &r
	}
	_, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retirerunitems SET result = $3, reason = $4, bytes = $5,
			source_id = COALESCE($6, source_id), phase = NULL, at = now()
		WHERE run_id = $1 AND position = $2`, run, position, o.result, reason, o.bytes, o.source)
	return err
}

func (l *RetireList) finish(ctx context.Context, run, state, why string) error {
	var e *string
	if why != "" {
		e = &why
	}
	_, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retireruns SET state = $2, finishedat = now(), error = $3
		WHERE id = $1 AND state = 'running'`, run, state, e)
	return err
}

// title takes a title of the run to its result: a title whose retirement the
// run began before it was interrupted goes on with it (its original deleted
// meanwhile retired, still retiring resumed, present again checked anew);
// any other is checked (check), and, every condition holding, retired, or in
// a dry run would be. An empty result: ctx ended first.
func (l *RetireList) title(ctx context.Context, conn *pgxpool.Conn, run *running, it *listItem, set Settings,
	p *pace) (outcome, error) {
	auth := Authority{By: RetireListedBy(run.by), Reason: RetireListedReason}
	if it.phase != nil && *it.phase == listPhaseRetiring && it.sourceID != nil {
		s, err := SourceByID(ctx, l.pool, *it.sourceID)
		if err != nil {
			return outcome{}, err
		}
		switch {
		case s == nil:
			return outcome{result: ListFailed, reason: "its original is no original of the title any more", source: it.sourceID}, nil
		case s.State == SourceDeleted:
			size := s.SizeBytes
			return outcome{result: ListRetired, source: &s.ID, bytes: &size}, nil
		case s.State == SourceRetiring:
			return l.retire(ctx, conn, auth, set, s, p)
		case s.State != SourcePresent:
			return outcome{result: ListSkipped, reason: "its original is " + s.State + " meanwhile", source: &s.ID}, nil
		}
		// present again: its retirement was called off meanwhile; checked anew
	}
	s, why, err := l.check(ctx, it.itemID, set)
	if err != nil {
		return outcome{}, err
	}
	var src *string
	var size *int64
	if s != nil {
		src, size = &s.ID, &s.SizeBytes
	}
	switch {
	case why != "":
		return outcome{result: ListSkipped, reason: why, source: src, bytes: size}, nil
	case run.dry:
		return outcome{result: ListWouldRetire, source: src, bytes: size}, nil
	}
	if _, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retirerunitems SET phase = $3, source_id = $4
		WHERE run_id = $1 AND position = $2`, run.id, it.position, listPhaseRetiring, s.ID); err != nil {
		return outcome{}, err
	}
	return l.retire(ctx, conn, auth, set, s, p)
}

// retireLockKey is the retire job's advisory lock (retireLock), which a list
// holds while it retires an original: the job runs no pass meanwhile.
const retireLockKey = `hashtext('` + retireLock + `')`

// retire retires the original s through the retire job's own path, at the
// pace, while the job runs no pass.
func (l *RetireList) retire(ctx context.Context, conn *pgxpool.Conn, auth Authority, set Settings, s *Source,
	p *pace) (outcome, error) {
	if !l.paced(ctx, p, set.RetireRate) {
		return outcome{}, nil
	}
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+retireLockKey+`)`).Scan(&locked); err != nil {
			return outcome{}, err
		}
		if locked {
			break
		}
		if !l.sleep(ctx, l.lockWait) {
			return outcome{}, nil
		}
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+retireLockKey+`)`) }()
	src, size := s.ID, s.SizeBytes
	rt := &retirement{src: s, auth: auth}
	deleted, err := l.r.retireOf(ctx, PathsOf(l.cfg), set, l.now().UTC(), rt)
	switch {
	case errors.Is(err, errHeld):
		return outcome{result: ListSkipped, reason: SurroundHeld, source: &src}, nil
	case err != nil && ctx.Err() != nil:
		return outcome{}, nil
	case err != nil:
		why := rt.why
		if why == "" {
			why = err.Error()
		}
		return outcome{result: ListFailed, reason: why, source: &src}, nil
	case deleted:
		return outcome{result: ListRetired, source: &src, bytes: &size}, nil
	}
	why := rt.why
	if why == "" {
		why = "its retirement was called off"
	}
	return outcome{result: ListSkipped, reason: why, source: &src}, nil
}

// check holds the title id against every condition its original is retired
// on, in this order, and answers its original and "", or why not:
//
//   - it is a movie or an episode of the catalog, whose file is its own (an
//     episode another's file covers is that episode's), and it is not held
//     (holdOriginal);
//   - it has a complete version, verified once at least, made of one
//     original, which is present;
//   - no step that reads the original waits, runs or is to be retried, and
//     it is not in the re-encode queue (queued, or sent);
//   - the compliance check found it within every rule under the rules both
//     workers report now (the gate);
//   - its package has the surround of its original (the retire job's own
//     condition, held to before it claims);
//   - its original is where the catalog says, of the size it recorded, and
//     the trash is on its filesystem (unless library.trash.grace is 0);
//   - the episodes its file covers are not held, and none has a step at work
//     or is in the re-encode queue.
//
// The retire path checks more before it deletes anything: the original's
// quick hash, the source's record and the version's checksums, the sidecars.
func (l *RetireList) check(ctx context.Context, id string, set Settings) (*Source, string, error) {
	covers, err := CoversReady(ctx, l.pool)
	if err != nil {
		return nil, "", err
	}
	var typ string
	var held bool
	var holder *string
	if err := l.pool.QueryRow(ctx, `SELECT lower(COALESCE(i.type, '')), i.retirehold, to_jsonb(i)->>'coveredby'
		FROM com_nalet_katalog_items i WHERE i.id = $1`, id).Scan(&typ, &held, &holder); errors.Is(err, pgx.ErrNoRows) {
		return nil, "it is not in the catalog", nil
	} else if err != nil {
		return nil, "", err
	}
	switch {
	case typ != "movie" && typ != "episode":
		return nil, "a " + typ + " has no original of its own: a list names movies and episodes", nil
	case covers && holder != nil:
		name, err := l.nameOf(ctx, *holder)
		if err != nil {
			return nil, "", err
		}
		return nil, "another episode’s file covers it: its original is that of " + name + " (" + *holder + "), which a list " +
			"names by its own id", nil
	case held:
		return nil, "its originals are held (holdOriginal)", nil
	}
	v, err := Current(ctx, l.pool, id)
	if err != nil {
		return nil, "", err
	}
	if v == nil {
		return nil, "it has no complete version", nil
	}
	if v.VerifiedAt == nil {
		return nil, "its complete version " + v.ID + " was never verified", nil
	}
	s, why, err := l.originalOf(ctx, v)
	if err != nil || why != "" {
		return s, why, err
	}
	if why, err := l.atWork(ctx, id); err != nil || why != "" {
		return s, why, err
	}
	passed, err := l.gate.Passed(ctx, []string{id})
	if err != nil {
		return s, "", err
	}
	if why, ok := passed[id]; !ok {
		return s, "the compliance check said nothing of it", nil
	} else if why != "" {
		return s, why, nil
	}
	pl, err := PlaceOf(ctx, l.pool, id)
	if err != nil {
		return s, "the title has no folder in the library: " + err.Error(), nil
	}
	itemDir := PathsOf(l.cfg).ItemDir(pl)
	if lost, err := surroundLost(itemDir, v.ID, s.ID); err != nil {
		return s, fmt.Sprintf("the essences of its original's and its package's records can’t be read: %v", err), nil
	} else if lost {
		return s, SurroundHeld, nil
	}
	if why := l.fileOf(s, set); why != "" {
		return s, why, nil
	}
	if covers {
		if why, err := l.coveredOf(ctx, id); err != nil || why != "" {
			return s, why, err
		}
	}
	return s, "", nil
}

// nameOf is the title id as a list names it, else its id.
func (l *RetireList) nameOf(ctx context.Context, id string) (string, error) {
	names, err := TitlesOf(ctx, l.pool, []string{id})
	if err != nil {
		return "", err
	}
	if n, ok := names[id]; ok {
		return n, nil
	}
	return id, nil
}

// originalOf is the original the version v was made of, and "", or why
// there is none to retire: it names none the library holds, or more than
// one, or it is not present.
func (l *RetireList) originalOf(ctx context.Context, v *Version) (*Source, string, error) {
	sources, err := querySources(ctx, l.pool, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources
		WHERE id = ANY($1) ORDER BY createdat, id`, v.SourceIDs)
	if err != nil {
		return nil, "", err
	}
	var present []*Source
	for _, s := range sources {
		if s.State == SourcePresent {
			present = append(present, s)
		}
	}
	switch {
	case len(present) == 1:
		return present[0], "", nil
	case len(present) > 1:
		return nil, fmt.Sprintf("its version was made of %d originals, and a list retires one a title", len(present)), nil
	case len(sources) == 0:
		return nil, "its version names no original the library holds", nil
	}
	s := sources[0]
	switch s.State {
	case SourceDeleted:
		return s, "its original was retired already", nil
	case SourceRetiring:
		return s, "its original is being retired", nil
	}
	return s, "its original is " + s.State, nil
}

// atWork says why the title id is at work, "" when it is not: a step that
// reads its original waits, runs or is to be retried, or it is in the
// re-encode queue.
func (l *RetireList) atWork(ctx context.Context, id string) (string, error) {
	var step, status string
	err := l.pool.QueryRow(ctx, `SELECT step, status FROM com_nalet_katalog_itemprocessingsteps
		WHERE item_id = $1 AND step = ANY($2::text[])
		  AND (status IN ('pending', 'in_progress') OR (status = 'failed' AND nextretryat IS NOT NULL))
		ORDER BY array_position($2::text[], step) LIMIT 1`, id, processing.OriginalSteps).Scan(&step, &status)
	switch {
	case err == nil:
		what := map[string]string{"pending": "waits", "in_progress": "runs", "failed": "failed and is to be tried again"}[status]
		return "its " + step + " step " + what, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	var queued bool
	if err := l.pool.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_reencodequeue') IS NOT NULL`).Scan(&queued); err != nil ||
		!queued {
		return "", err
	}
	var state string
	err = l.pool.QueryRow(ctx, `SELECT state FROM com_nalet_katalog_reencodequeue WHERE item_id = $1 AND state IN ('queued', 'sent')
		LIMIT 1`, id).Scan(&state)
	switch {
	case err == nil:
		return "it is in the re-encode queue (" + state + ")", nil
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	}
	return "", err
}

// coveredOf says why an episode the file of the holder id covers keeps the
// holder's original, "" when none does: one is held, has a step at work, or
// is in the re-encode queue.
func (l *RetireList) coveredOf(ctx context.Context, holder string) (string, error) {
	rows, err := l.pool.Query(ctx, `SELECT id, retirehold FROM com_nalet_katalog_items WHERE coveredby = $1
		ORDER BY seasonnumber NULLS LAST, episodenumber NULLS LAST, id`, holder)
	if err != nil {
		return "", err
	}
	type covered struct {
		id   string
		held bool
	}
	var list []covered
	for rows.Next() {
		var c covered
		if err := rows.Scan(&c.id, &c.held); err != nil {
			rows.Close()
			return "", err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for _, c := range list {
		why := ""
		if c.held {
			why = "its originals are held (holdOriginal)"
		} else if why, err = l.atWork(ctx, c.id); err != nil {
			return "", err
		}
		if why != "" {
			name, err := l.nameOf(ctx, c.id)
			if err != nil {
				return "", err
			}
			return "an episode its file covers keeps it, " + name + ": " + why, nil
		}
	}
	return "", nil
}

// fileOf says why the original s can't be retired as it lies, "" when it
// can: the catalog says no place of it, it is not there or of another size
// than recorded, or the trash, where it goes, is on another filesystem
// (unless library.trash.grace is 0, which deletes it at once). Its other
// names, of a file with more than one, are none of the library's.
func (l *RetireList) fileOf(s *Source, set Settings) string {
	if s.ArrivalPath == nil || *s.ArrivalPath == "" {
		return "the catalog doesn’t say where its original lies"
	}
	fi, err := os.Stat(*s.ArrivalPath)
	switch {
	case err != nil:
		return "its original isn’t where the catalog says: " + *s.ArrivalPath
	case !fi.Mode().IsRegular():
		return "its original is no file: " + *s.ArrivalPath
	case fi.Size() != s.SizeBytes:
		return fmt.Sprintf("its original is %d bytes, and the catalog recorded %d: %s", fi.Size(), s.SizeBytes, *s.ArrivalPath)
	}
	if set.TrashGrace == 0 {
		return ""
	}
	trash := PathsOf(l.cfg).TrashDay(l.now())
	if same, err := sameFilesystem(*s.ArrivalPath, trash); err != nil {
		return fmt.Sprintf("where the trash is can’t be told (%v): %s", err, trash)
	} else if !same {
		return "the trash (WORK_ROOT) is on another filesystem than its original: put the trash on the share its original " +
			"is on, or set library.trash.grace to 0 to delete originals at once"
	}
	return ""
}

// sameFilesystem reports whether the file at path and the folder dir (or,
// while it is not there, the nearest folder above it that is) lie on one
// filesystem, as a rename between them needs.
func sameFilesystem(path, dir string) (bool, error) {
	a, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		b, err := os.Stat(d)
		if err == nil {
			sa, okA := a.Sys().(*syscall.Stat_t)
			sb, okB := b.Sys().(*syscall.Stat_t)
			if !okA || !okB {
				return true, nil // no device to tell them by: the rename says
			}
			return sa.Dev == sb.Dev, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(d) == d {
			return false, err
		}
	}
}
