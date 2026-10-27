package library

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// (keep, its default) and stays as it is. A dry run of the titles an admin
// names checks each, and says whether it would retire its original, touching
// nothing but the run's own rows; a run retires the originals of the titles a
// dry run found it would (fromRun) and nothing else, each held to what the
// dry run found of it (its original, its version, and what retiring it gives
// up: review), within library.retire.list.maxAge, through the retire job's
// own path (retireOf: its claim, its checks of the original and of the
// version, its event, the trash and its grace, the source's row, what the
// package lacks of it recorded), on the word of the admin who asked. The
// setting library.retire.list says what a list may do: off, dry-run (the
// default) or on.
//
// A title's original is retired only when every condition holds (check): it
// is checked once the run may act on it (its pace kept, the retire job's lock
// held), and the retire path asks the list again (its authority's Recheck)
// before it resumes a retirement whose event is not recorded and right before
// it records the event, after every wait of its own. Else the title is
// skipped, saying why.
//
// One run runs at a time (idx_retireruns_running), in one instance at a time
// (an advisory lock), in the background: it goes on when the admin goes away,
// and a run a restart interrupted goes on where it stopped - a retirement it
// began is resumed (or, called off meanwhile, its title is checked again). An
// admin stops it after the title it is at (stopRetireRun), and so does the
// setting. A title an error stops is tried again a few times, a while apart,
// then failed; a run whose titles fail so several in a row stops, failed. It
// retires one original at a time, each while the retire job runs no pass (the
// job's own lock), at most library.retire.rate a minute (the job's pace).
//
// The library's name of an original is all it retires: a file with more than
// one name (a hard link another tree shares) is retired by that name, moved
// into the trash, and its other names are none of the library's.

// The states of a run: it runs, it is done (every title has a result), it
// stopped (an admin stopped it, or library.retire.list doesn't let it go on),
// or it failed: it could not go on. error says why of the last two.
const (
	RunRunning = "running"
	RunDone    = "done"
	RunStopped = "stopped"
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

// titleTries is how often a run tries a title an error stops before it fails
// it, a while apart (backoff, doubled each time); runErrors is how many
// titles in a row it fails so before it stops itself, failed.
const (
	titleTries = 3
	runErrors  = 5
)

// listPhaseRetiring marks a title whose retirement a run began: a run that
// resumes goes on with it.
const listPhaseRetiring = "retiring"

// RetireListedBy is who retires an original of an admin's list, as its event
// and its row say: by, the admin.
func RetireListedBy(by string) string { return "katalog-manager (retireOriginals, by " + by + ")" }

// RetireListedReason is why, as the event says.
const RetireListedReason = "an admin listed it to be retired (retireOriginals): the package is the record"

// ListRun is a run of an admin's list: its state (RunRunning, RunDone,
// RunStopped, RunFailed), whether it is a dry run, the dry run whose titles
// it retires, who asked, when it began and ended, why it stopped or failed,
// its titles by result (Pending those still to come), and the size of the
// originals it retired (of a dry run, would retire).
type ListRun struct {
	ID, State, RequestedBy                        string
	DryRun                                        bool
	FromRun                                       *string
	StartedAt                                     time.Time
	FinishedAt                                    *time.Time
	Error                                         *string
	Total, Pending, Retired, WouldRetire, Skipped int
	Failed                                        int
	Bytes                                         int64
}

// ListItem is a title of a run's list: its place in it (from 1), its id and
// its name when the run began, the original the run retires of it and the
// version it checked it against, its result (nil while it waits), why (of a
// title a dry run would retire, what its package doesn't carry of the
// original and the files that go with it), its original's size, and when it
// got its result.
type ListItem struct {
	Position                   int
	ItemID                     string
	Title, SourceID, VersionID *string
	Result, Reason             *string
	Bytes                      *int64
	At                         *time.Time
}

// ListRequest is what an admin asks of a list: a dry run of the titles they
// name (Items), or a run of the titles a dry run found it would retire
// (FromRun, the dry run's id), retired unless DryRun. By is the admin.
type ListRequest struct {
	Items   []string
	FromRun string
	DryRun  bool
	By      string
}

// ListRefused is why a list is not run: Code (UNAVAILABLE, LEGACY, OFF,
// DRY_RUN_ONLY, NAMED_TITLES, BOTH, EMPTY, BAD_ID, TOO_MANY, FROM_RUN,
// RUNNING) and a sentence.
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
	// sleep waits d, false when ctx ends first: the pace, the wait for the
	// retire job's pass, and the wait after an error.
	sleep func(ctx context.Context, d time.Duration) bool
	pokes chan struct{}
	tick  time.Duration
	// lockWait is how long it waits before it tries the retire job's lock
	// again, and backoff how long after a first error on a title.
	lockWait, backoff time.Duration
	// tries are how often an error stopped the title it is at
	// ("<run>/<position>"), and inRow how many titles in a row of the run
	// rowOf it failed so.
	tries       map[string]int
	inRow       int
	rowOf, said string
}

// NewRetireList runs the lists of the catalog's pool, at the paths cfg
// configures, the retire step through steps, each title's compliance asked
// of gate.
func NewRetireList(pool *pgxpool.Pool, cfg config.Config, steps *processing.Steps, gate ComplianceGate) *RetireList {
	return &RetireList{pool: pool, cfg: cfg, r: NewRetirer(pool, cfg, steps), gate: gate, now: time.Now, sleep: sleepCtx,
		pokes: make(chan struct{}, 1), tick: 30 * time.Second, lockWait: 2 * time.Second, backoff: 10 * time.Second,
		tries: map[string]int{}}
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

// Start runs a list as req asks, on the word of req.By: it is noted, and
// runs in the background (Run). It answers the run as it begins, or
// ListRefused: without migration 052, with library.layout legacy, with
// library.retire.list off, a run (no dry run) unless it is on, a run of
// titles named (a run takes a dry run's), of titles and of a dry run both, of
// no title, of an id that is no title's, of more than MaxList, of a run that
// is no dry run done with titles it would retire, or while one runs already.
func (l *RetireList) Start(ctx context.Context, req ListRequest) (ListRun, error) {
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
	switch {
	case !set.V2():
		return ListRun{}, &ListRefused{"LEGACY", "library.layout is legacy: the library keeps no records of originals and " +
			"packages, so no original is retired"}
	case set.RetireList == RetireListOff:
		return ListRun{}, &ListRefused{"OFF", "library.retire.list is off: no list runs; set it to dry-run to see what a " +
			"list would retire"}
	case !req.DryRun && set.RetireList != RetireListOn:
		return ListRun{}, &ListRefused{"DRY_RUN_ONLY", "library.retire.list is dry-run: a list only says what it would " +
			"retire; set it to on to retire the titles a dry run found it would"}
	}
	from := strings.TrimSpace(req.FromRun)
	var list []string
	var reviews []*review
	switch {
	case len(req.Items) > 0 && from != "":
		return ListRun{}, &ListRefused{"BOTH", "a list names titles, or the dry run whose titles it retires, and not both"}
	case from != "":
		if list, reviews, err = l.fromRun(ctx, from, set); err != nil {
			return ListRun{}, err
		}
	case !req.DryRun:
		return ListRun{}, &ListRefused{"NAMED_TITLES", "a list of titles named is a dry run: retire the titles a dry run " +
			"found it would, with fromRun (its id)"}
	default:
		seen := map[string]bool{}
		var bad []string
		for _, id := range req.Items {
			id = strings.TrimSpace(id)
			switch {
			case id == "" || seen[id]:
			case !ValidID(id):
				bad = append(bad, strconv.Quote(id))
			default:
				seen[id] = true
				list = append(list, id)
			}
		}
		if len(bad) > 0 {
			if len(bad) > 3 {
				bad = append(bad[:3], fmt.Sprintf("and %d more", len(bad)-3))
			}
			return ListRun{}, &ListRefused{"BAD_ID", "an id of a title is a lower-case UUID, and these are not: " +
				strings.Join(bad, ", ")}
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
	rsrc, rver, rdig := make([]*string, len(list)), make([]*string, len(list)), make([]*string, len(list))
	rat := make([]*time.Time, len(list))
	for i, id := range list {
		if n, ok := names[id]; ok {
			n := clip(n, 500)
			named[i] = &n
		}
		if i < len(reviews) && reviews[i] != nil {
			rv := reviews[i]
			rsrc[i], rver[i], rdig[i], rat[i] = &rv.source, &rv.version, &rv.digest, &rv.at
		}
	}
	var fromRun *string
	if from != "" {
		fromRun = &from
	}
	id := NewID()
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return ListRun{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_retireruns (id, dryrun, fromrun, requestedby) VALUES ($1, $2, $3, $4)`,
		id, req.DryRun, fromRun, clip(req.By, 255)); err != nil {
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" {
			return ListRun{}, l.refusedRunning(ctx)
		}
		return ListRun{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_retirerunitems (run_id, position, item_id, title,
			reviewed_source_id, reviewed_version_id, reviewed_digest, reviewed_at)
		SELECT $1, x.position, x.id, x.title, x.rsrc, x.rver, x.rdig, x.rat
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::timestamptz[])
			WITH ORDINALITY AS x(id, title, rsrc, rver, rdig, rat, position)`, id, list, named, rsrc, rver, rdig, rat); err != nil {
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

// fromRun are the titles the dry run id found it would retire, in its order,
// and what it found of each (review; nil of one it left no word on, which a
// run skips); ListRefused of a run that is none, no dry run, a dry run not
// done, one that finished longer ago than library.retire.list.maxAge, or one
// that found none.
func (l *RetireList) fromRun(ctx context.Context, id string, set Settings) ([]string, []*review, error) {
	var state string
	var dry, fresh bool
	err := l.pool.QueryRow(ctx, `SELECT state, dryrun, COALESCE(finishedat >= now() - make_interval(secs => $2::float8), false)
		FROM com_nalet_katalog_retireruns WHERE id = $1`, id, set.RetireListMaxAge.Seconds()).Scan(&state, &dry, &fresh)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil, &ListRefused{"FROM_RUN", "there is no run " + id}
	case err != nil:
		return nil, nil, err
	case !dry:
		return nil, nil, &ListRefused{"FROM_RUN", "run " + id + " is no dry run: a run retires the titles a dry run found it would"}
	case state != RunDone:
		return nil, nil, &ListRefused{"FROM_RUN", "dry run " + id + " is " + state + ": a run takes the titles of a dry run done"}
	case !fresh:
		return nil, nil, &ListRefused{"FROM_RUN", "dry run " + id + " finished more than library.retire.list.maxAge (" +
			DurationText(set.RetireListMaxAge) + ") ago: what it found holds no more"}
	}
	rows, err := l.pool.Query(ctx, `SELECT item_id, source_id, version_id, digest, at FROM com_nalet_katalog_retirerunitems
		WHERE run_id = $1 AND result = $2 ORDER BY position`, id, ListWouldRetire)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var out []string
	var reviews []*review
	for rows.Next() {
		var item string
		var src, ver, digest *string
		var at *time.Time
		if err := rows.Scan(&item, &src, &ver, &digest, &at); err != nil {
			return nil, nil, err
		}
		var rv *review
		if src != nil && ver != nil && digest != nil && at != nil {
			rv = &review{source: *src, version: *ver, digest: *digest, at: *at}
		}
		out, reviews = append(out, item), append(reviews, rv)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		return nil, nil, &ListRefused{"FROM_RUN", "dry run " + id + " found no title whose original it would retire"}
	}
	return out, reviews, nil
}

// review is what a dry run found of a title a run retires: the original it
// checked, the version it checked it against, the digest of what retiring it
// gives up (reviewDigest), and when it checked it.
type review struct {
	source, version, digest string
	at                      time.Time
}

// changed says how the title the dry run reviewed as rv changed since, its
// original s and its version v now; "" when it did not, or of no review.
func (rv *review) changed(s *Source, v *Version) string {
	switch {
	case rv == nil:
		return ""
	case s.ID != rv.source:
		return "it changed since the dry run: its original is " + s.ID + " now, and the dry run checked " + rv.source
	case v.ID != rv.version:
		return "it changed since the dry run: its version is " + v.ID + " now, and the dry run checked " + rv.version
	}
	return ""
}

// changedLoss is why a title whose retirement would give up other than its
// dry run showed is not retired: what it gives up now (preview).
func changedLoss(preview string) string {
	return "it changed since the dry run: what retiring it gives up is not what the dry run showed; now " + preview
}

// reviewDigest is the sha256 (hex) of what retiring an original gives up:
// what its package doesn't carry of it (accepted) and the files that go with
// it into the trash (moving), whatever their order.
func reviewDigest(accepted, moving []string) string {
	h := sha256.New()
	for _, part := range [][]string{accepted, moving} {
		sorted := slices.Clone(part)
		slices.Sort(sorted)
		for _, x := range sorted {
			h.Write([]byte(x))
			h.Write([]byte{0})
		}
		h.Write([]byte{1})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// refusedRunning is the refusal of a list while another runs.
func (l *RetireList) refusedRunning(ctx context.Context) error {
	run, err := l.Latest(ctx)
	if err != nil {
		return err
	}
	msg := "a list of originals is being retired already: wait for it to finish, or stop it (stopRetireRun)"
	if run != nil && run.State == RunRunning {
		kind := "a list"
		if run.DryRun {
			kind = "a dry run of a list"
		}
		msg = fmt.Sprintf("%s of %s runs since %s, %d of them still to come: wait for it to finish, or stop it "+
			"(stopRetireRun)", kind, titles(run.Total), Timestamp(run.StartedAt), run.Pending)
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

// Stop stops the run that runs once the title it is at has its result, on
// the word of by, and answers it; nil when none runs.
func (l *RetireList) Stop(ctx context.Context, by string) (*ListRun, error) {
	if ok, err := l.ready(ctx); err != nil || !ok {
		return nil, err
	}
	var id string
	err := l.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_retireruns SET state = $1, finishedat = now(), error = $2
		WHERE state = $3 RETURNING id`, RunStopped, clip("stopped by "+by+" (stopRetireRun)", 500), RunRunning).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return l.runOf(ctx, id)
}

// poke wakes the background runner, without waiting.
func (l *RetireList) poke() {
	select {
	case l.pokes <- struct{}{}:
	default:
	}
}

const runCols = `r.id, r.state, r.requestedby, r.dryrun, r.fromrun, r.startedat, r.finishedat, r.error,
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
	err := row.Scan(&r.ID, &r.State, &r.RequestedBy, &r.DryRun, &r.FromRun, &r.StartedAt, &r.FinishedAt, &r.Error, &r.Total,
		&r.Pending, &r.Retired, &r.WouldRetire, &r.Skipped, &r.Failed, &r.Bytes)
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
	rows, err := l.pool.Query(ctx, `SELECT position, item_id, title, source_id, version_id, result, reason, bytes, at
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
		if err := rows.Scan(&it.Position, &it.ItemID, &it.Title, &it.SourceID, &it.VersionID, &it.Result, &it.Reason, &it.Bytes,
			&it.At); err != nil {
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
// nothing. It answers once the run is done, stopped or failed (or there is
// none), or ctx ended.
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
		if run.id != l.rowOf {
			l.rowOf, l.inRow = run.id, 0
		}
		set, err := ReadSettings(ctx, l.pool)
		if err != nil {
			return err
		}
		if state, why := goesOn(set, run); why != "" {
			return l.finish(ctx, run.id, state, why)
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
		res, err := l.title(ctx, conn, run, it, &p)
		key := run.id + "/" + strconv.Itoa(it.position)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if l.tries[key]++; l.tries[key] < titleTries {
				l.say(fmt.Sprintf("library: title %d of the list of originals to retire stopped: %v; it is tried again",
					it.position, err))
				if !l.sleep(ctx, l.backoff<<(l.tries[key]-1)) {
					return ctx.Err()
				}
				continue
			}
			delete(l.tries, key)
			l.inRow++
			res = outcome{result: ListFailed, reason: fmt.Sprintf("an error stopped it %d times, the last: %v", titleTries, err),
				source: it.sourceID}
		case res.result == "" && ctx.Err() != nil:
			return ctx.Err() // ended before the title had a result: it stays to come
		case res.result == "":
			continue // stopped, or the settings no longer let the run go on: the title stays to come, and the run ends
		default:
			delete(l.tries, key)
			l.inRow = 0
		}
		if err := l.note(context.WithoutCancel(ctx), run.id, it.position, res); err != nil {
			return err
		}
		at.after = it.position
		if l.inRow >= runErrors {
			return l.finish(ctx, run.id, RunFailed, fmt.Sprintf("errors stopped %d titles in a row, the last: %s", runErrors,
				res.reason))
		}
	}
	return ctx.Err()
}

// goesOn says why the run can't go on, "" when it can, and what it becomes
// then: the layout is legacy (failed), library.retire.list is off, or a run
// that retires finds it is no longer on (stopped).
func goesOn(set Settings, run *running) (string, string) {
	switch {
	case !set.V2():
		return RunFailed, "library.layout is legacy: the library keeps no records of originals and packages, so no " +
			"original is retired"
	case set.RetireList == RetireListOff:
		return RunStopped, "library.retire.list is off: the run stopped"
	case !run.dry && set.RetireList != RetireListOn:
		return RunStopped, "library.retire.list is " + set.RetireList + ": no original is retired, and the run stopped"
	}
	return "", ""
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
	// review is, of a run's title, what its dry run found of it; nil of a
	// dry run's, and of a run's whose dry run left no word on it
	review *review
}

// next is the run's first title after the place after that waits for its
// result; nil when none does.
func (l *RetireList) next(ctx context.Context, run string, after int) (*listItem, error) {
	var it listItem
	var rsrc, rver, rdig *string
	var rat *time.Time
	err := l.pool.QueryRow(ctx, `SELECT position, item_id, source_id, phase, reviewed_source_id, reviewed_version_id,
			reviewed_digest, reviewed_at
		FROM com_nalet_katalog_retirerunitems
		WHERE run_id = $1 AND position > $2 AND result IS NULL ORDER BY position LIMIT 1`, run, after).
		Scan(&it.position, &it.itemID, &it.sourceID, &it.phase, &rsrc, &rver, &rdig, &rat)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rsrc != nil && rver != nil && rdig != nil && rat != nil {
		it.review = &review{source: *rsrc, version: *rver, digest: *rdig, at: *rat}
	}
	return &it, nil
}

// outcome is a title's result: what, why, its original and its size, the
// version it was checked against, and of a title a dry run would retire the
// digest of what retiring it gives up.
type outcome struct {
	result, reason  string
	source          *string
	bytes           *int64
	version, digest *string
}

func (l *RetireList) note(ctx context.Context, run string, position int, o outcome) error {
	var reason *string
	if o.reason != "" {
		r := clip(o.reason, 1000)
		reason = &r
	}
	_, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retirerunitems SET result = $3, reason = $4, bytes = $5,
			source_id = COALESCE($6, source_id), version_id = COALESCE($7, version_id), digest = $8, phase = NULL, at = now()
		WHERE run_id = $1 AND position = $2`, run, position, o.result, reason, o.bytes, o.source, o.version, o.digest)
	return err
}

func (l *RetireList) finish(ctx context.Context, run, state, why string) error {
	var e *string
	if why != "" {
		why = clip(why, 500)
		e = &why
	}
	_, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retireruns SET state = $2, finishedat = now(), error = $3
		WHERE id = $1 AND state = 'running'`, run, state, e)
	return err
}

// retireLockKey is the retire job's advisory lock (retireLock), which a list
// holds while it checks and retires an original: the job runs no pass
// meanwhile.
const retireLockKey = `hashtext('` + retireLock + `')`

// lockRetire waits for the retire job's lock on conn and answers how to let
// it go; nil when ctx ended first.
func (l *RetireList) lockRetire(ctx context.Context, conn *pgxpool.Conn) (func(), error) {
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+retireLockKey+`)`).Scan(&locked); err != nil {
			return nil, err
		}
		if locked {
			return func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+retireLockKey+`)`) }, nil
		}
		if !l.sleep(ctx, l.lockWait) {
			return nil, nil
		}
	}
}

// stillRuns reports whether the run still runs, and the settings let it go
// on: an admin may have stopped it, or the setting changed, while it waited.
func (l *RetireList) stillRuns(ctx context.Context, run *running) (bool, Settings, error) {
	var state string
	if err := l.pool.QueryRow(ctx, `SELECT state FROM com_nalet_katalog_retireruns WHERE id = $1`, run.id).Scan(&state); err != nil {
		return false, Settings{}, err
	}
	set, err := ReadSettings(ctx, l.pool)
	if err != nil {
		return false, set, err
	}
	_, why := goesOn(set, run)
	return state == RunRunning && why == "", set, nil
}

// title takes a title of the run to its result. A run that retires keeps its
// pace and holds the retire job's lock first, then finds it still runs: what
// it then checks holds while it acts. It holds a title to what its dry run
// found of it (its review), in check and again as the retire path asks right
// before the event. A title whose retirement the run began before it was
// interrupted goes on with it: its original deleted meanwhile on the run's
// word is retired (on another's, skipped), one still retiring resumed, one
// present again checked anew. Any other is checked (check) and, every
// condition holding, retired, or in a dry run would be. An empty result: ctx
// ended, or the run stopped, first.
func (l *RetireList) title(ctx context.Context, conn *pgxpool.Conn, run *running, it *listItem, p *pace) (outcome, error) {
	set, err := ReadSettings(ctx, l.pool)
	if err != nil {
		return outcome{}, err
	}
	if !run.dry {
		if !l.paced(ctx, p, set.RetireRate) {
			return outcome{}, nil
		}
		unlock, err := l.lockRetire(ctx, conn)
		if err != nil || unlock == nil {
			return outcome{}, err
		}
		defer unlock()
		ok, now, err := l.stillRuns(ctx, run)
		if err != nil || !ok {
			return outcome{}, err
		}
		set = now
	}
	rv := it.review
	if !run.dry && rv == nil {
		return outcome{result: ListSkipped, reason: "its dry run left no word on what retiring it gives up"}, nil
	}
	if run.dry {
		rv = nil
	}
	auth := Authority{By: RetireListedBy(run.by), Reason: RetireListedReason,
		Recheck: func(ctx context.Context, s *Source) (string, error) { return l.recheck(ctx, s, rv) }}
	if it.phase != nil && *it.phase == listPhaseRetiring && it.sourceID != nil {
		s, err := SourceByID(ctx, l.pool, *it.sourceID)
		if err != nil {
			return outcome{}, err
		}
		switch {
		case s == nil:
			return outcome{result: ListFailed, reason: "its original is no original of the title any more", source: it.sourceID}, nil
		case s.State == SourceDeleted && deref(s.DeletedBy) == clip(auth.By, 255):
			size := s.SizeBytes
			return outcome{result: ListRetired, source: &s.ID, bytes: &size}, nil
		case s.State == SourceDeleted:
			return outcome{result: ListSkipped, reason: "its original was deleted meanwhile, by " + deref(s.DeletedBy),
				source: &s.ID}, nil
		case s.State == SourceRetiring:
			return l.retire(ctx, auth, set, s)
		case s.State != SourcePresent:
			return outcome{result: ListSkipped, reason: "its original is " + s.State + " meanwhile", source: &s.ID}, nil
		}
		// present again: its retirement was called off meanwhile; checked anew
	}
	f, why, err := l.check(ctx, it.itemID, rv, set)
	if err != nil {
		return outcome{}, err
	}
	var o outcome
	if f.src != nil {
		o.source, o.bytes = &f.src.ID, &f.src.SizeBytes
	}
	if f.ver != nil {
		o.version = &f.ver.ID
	}
	switch {
	case why != "":
		o.result, o.reason = ListSkipped, why
		return o, nil
	case run.dry:
		o.result, o.reason, o.digest = ListWouldRetire, f.preview, &f.digest
		return o, nil
	}
	if _, err := l.pool.Exec(ctx, `UPDATE com_nalet_katalog_retirerunitems SET phase = $3, source_id = $4, version_id = $5
		WHERE run_id = $1 AND position = $2`, run.id, it.position, listPhaseRetiring, f.src.ID, f.ver.ID); err != nil {
		return outcome{}, err
	}
	res, err := l.retire(ctx, auth, set, f.src)
	res.version = o.version
	return res, err
}

// retire retires the original s through the retire job's own path, on the
// list's word (auth): its result, and why when it is not retired; an error of
// the catalog's, which Work tries again a while later (the title resumes
// where the retirement stopped). A failure once the event is recorded says
// so: the retire job finishes it.
func (l *RetireList) retire(ctx context.Context, auth Authority, set Settings, s *Source) (outcome, error) {
	src, size := s.ID, s.SizeBytes
	rt := &retirement{src: s, auth: auth}
	deleted, err := l.r.retireOf(ctx, PathsOf(l.cfg), set, l.now().UTC(), rt)
	var refused *retireFailed
	switch {
	case errors.Is(err, errHeld):
		return outcome{result: ListSkipped, reason: SurroundHeld, source: &src}, nil
	case err != nil && ctx.Err() != nil:
		return outcome{}, nil
	case err != nil && !errors.As(err, &refused):
		return outcome{}, err
	case err != nil:
		why := rt.why
		if why == "" {
			why = err.Error()
		}
		if recorded, _, _ := l.r.eventOf(rt); recorded != "" {
			why = "its original-deleted event is recorded, and the rest of its retirement failed: " + why +
				"; the retire job finishes it"
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

// found is what check found of a title: its original, the version it
// checked it against, and of one whose original it would retire what
// retiring it gives up, as a dry run says it (preview), and its digest
// (reviewDigest).
type found struct {
	src             *Source
	ver             *Version
	preview, digest string
}

// check holds the title id against every condition its original is retired
// on, in this order, and answers what it found of it, or why not:
//
//   - of a run's title, its dry run checked it within library.retire.list.maxAge
//     (rv, its review);
//   - it is a movie or an episode of the catalog, whose file is its own (an
//     episode another episode's file covers is that episode's), and it is not
//     held (holdOriginal);
//   - it has a complete version, verified once at least, made of exactly one
//     original, which is present; of a run's title, the original and the
//     version its dry run checked;
//   - no step that reads the original waits, runs or is to be retried, and
//     it is not in the re-encode queue (queued, or sent);
//   - the compliance check found it within every rule under the rules both
//     workers report now (the gate);
//   - its version's folder is there, and its package has the surround of
//     its original (the retire job's own condition, held to before it
//     claims);
//   - its original is a file where the catalog says, under a root the
//     retire path deletes under, of the size it recorded, and the trash is
//     on its filesystem (unless library.trash.grace is 0);
//   - what the retire path reads before it records the event holds, of the
//     small files: its version's checksums are not pending, the source's
//     record verifies, and no file beside the original would be lost with it
//     (a sidecar paired after packaging); of a run's title, retiring it gives
//     up what its dry run showed, no more and no other;
//   - the episodes its file covers are not held, and none has a step at work
//     or is in the re-encode queue.
//
// The retire path checks the rest before it deletes anything: the original's
// quick hash and the version's checksums.
func (l *RetireList) check(ctx context.Context, id string, rv *review, set Settings) (found, string, error) {
	if rv != nil && l.now().Sub(rv.at) > set.RetireListMaxAge {
		return found{}, "its dry run checked it more than library.retire.list.maxAge (" + DurationText(set.RetireListMaxAge) +
			") ago", nil
	}
	if why, err := l.itemOf(ctx, id); err != nil || why != "" {
		return found{}, why, err
	}
	v, s, why, err := l.versionOf(ctx, id, "")
	f := found{src: s, ver: v}
	if err != nil || why != "" {
		return f, why, err
	}
	if why := rv.changed(s, v); why != "" {
		return f, why, nil
	}
	if why, err := l.atWork(ctx, id); err != nil || why != "" {
		return f, why, err
	}
	if why, err := l.passed(ctx, id); err != nil || why != "" {
		return f, why, err
	}
	pl, err := PlaceOf(ctx, l.pool, id)
	if err != nil && !notPlaced(err) {
		return f, "", err
	}
	if err != nil {
		return f, "the title has no folder in the library: " + err.Error(), nil
	}
	itemDir := PathsOf(l.cfg).ItemDir(pl)
	dir := deref(v.Dir)
	if dir == "" {
		return f, "the catalog doesn’t say where its version’s folder is", nil
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return f, "its version’s folder isn’t there: " + dir, nil
	}
	if lost, err := surroundLost(itemDir, v.ID, s.ID); err != nil {
		return f, fmt.Sprintf("the essences of its original's and its package's records can’t be read: %v", err), nil
	} else if lost {
		return f, SurroundHeld, nil
	}
	if why := l.fileOf(s, itemDir, set); why != "" {
		return f, why, nil
	}
	f.preview, f.digest, why, err = l.preflight(ctx, itemDir, dir, v, s)
	if err != nil || why != "" {
		return f, why, err
	}
	if rv != nil && f.digest != rv.digest {
		return f, changedLoss(f.preview), nil
	}
	if why, err := l.coveredOf(ctx, id); err != nil || why != "" {
		return f, why, err
	}
	return f, "", nil
}

// recheck is the list's word on the retirement of s, as the retire path asks
// it again once its own waits are over (Authority.Recheck): the compliance
// check still finds it within every rule, and then, read last, the title is
// still a movie or an episode of its own and not held, its complete version
// is still made of s alone, nothing reads it, and the episodes its file
// covers hold too; of a run's title, its version is still the one its dry
// run checked (rv), and retiring it still gives up what the dry run showed.
func (l *RetireList) recheck(ctx context.Context, s *Source, rv *review) (string, error) {
	if why, err := l.passed(ctx, s.ItemID); err != nil || why != "" {
		return why, err
	}
	if why, err := l.itemOf(ctx, s.ItemID); err != nil || why != "" {
		return why, err
	}
	v, _, why, err := l.versionOf(ctx, s.ItemID, s.ID)
	if err != nil || why != "" {
		return why, err
	}
	if why := rv.changed(s, v); why != "" {
		return why, nil
	}
	if why, err := l.atWork(ctx, s.ItemID); err != nil || why != "" {
		return why, err
	}
	if why, err := l.coveredOf(ctx, s.ItemID); err != nil || why != "" || rv == nil {
		return why, err
	}
	pl, err := PlaceOf(ctx, l.pool, s.ItemID)
	if err != nil && !notPlaced(err) {
		return "", err
	}
	if err != nil {
		return "the title has no folder in the library: " + err.Error(), nil
	}
	preview, digest, why, err := l.preflight(ctx, PathsOf(l.cfg).ItemDir(pl), deref(v.Dir), v, s)
	if err != nil || why != "" {
		return why, err
	}
	if digest != rv.digest {
		return changedLoss(preview), nil
	}
	return "", nil
}

// itemOf says why the title id is none whose original a list retires, ""
// when it is one: it is no movie or episode of the catalog, another
// episode's file covers it (that episode's original it is), or it is held.
func (l *RetireList) itemOf(ctx context.Context, id string) (string, error) {
	covers, err := CoversReady(ctx, l.pool)
	if err != nil {
		return "", err
	}
	var typ string
	var held bool
	var holder *string
	if err := l.pool.QueryRow(ctx, `SELECT lower(btrim(COALESCE(i.type, ''))), i.retirehold, to_jsonb(i)->>'coveredby'
		FROM com_nalet_katalog_items i WHERE i.id = $1`, id).Scan(&typ, &held, &holder); errors.Is(err, pgx.ErrNoRows) {
		return "it is not in the catalog", nil
	} else if err != nil {
		return "", err
	}
	switch {
	case typ == "":
		return "it is an item of no type: a list names movies and episodes", nil
	case typ != "movie" && typ != "episode":
		return "a " + typ + " has no original of its own: a list names movies and episodes", nil
	case covers && holder != nil:
		name, err := l.nameOf(ctx, *holder)
		if err != nil {
			return "", err
		}
		return "another episode’s file covers it: its original is that of " + name + " (" + *holder + "), which a list " +
			"names by its own id", nil
	case held:
		return "its originals are held (holdOriginal)", nil
	}
	return "", nil
}

// passed is the compliance check's word on the title id: "" when it found it
// within every rule, else why not.
func (l *RetireList) passed(ctx context.Context, id string) (string, error) {
	passed, err := l.gate.Passed(ctx, []string{id})
	if err != nil {
		return "", err
	}
	why, ok := passed[id]
	if !ok {
		return "the compliance check said nothing of it", nil
	}
	return why, nil
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

// versionOf is the complete version of the title id and the one original it
// was made of, and "", or why it has none a list retires: no complete
// version, one never verified, one made of more than one original or of none
// the library holds, or (claimed "") an original that is not present; of the
// original claimed for its retirement, one made of another.
func (l *RetireList) versionOf(ctx context.Context, id, claimed string) (*Version, *Source, string, error) {
	v, err := Current(ctx, l.pool, id)
	switch {
	case err != nil:
		return nil, nil, "", err
	case v == nil:
		return nil, nil, "it has no complete version", nil
	case v.VerifiedAt == nil:
		return v, nil, "its complete version " + v.ID + " was never verified", nil
	case len(v.SourceIDs) != 1:
		return v, nil, fmt.Sprintf("its version %s was made of %d originals, and a list retires the original of a version "+
			"made of exactly one", v.ID, len(v.SourceIDs)), nil
	}
	s, err := SourceByID(ctx, l.pool, v.SourceIDs[0])
	switch {
	case err != nil:
		return v, nil, "", err
	case s == nil:
		return v, nil, "its version " + v.ID + " names no original the library holds", nil
	case claimed != "" && s.ID != claimed:
		return v, s, "its version " + v.ID + " is made of another original now", nil
	case claimed != "":
		return v, s, "", nil
	}
	switch s.State {
	case SourcePresent:
		return v, s, "", nil
	case SourceDeleted:
		return v, s, "its original was retired already", nil
	case SourceRetiring:
		return v, s, "its original is being retired", nil
	}
	return v, s, "its original is " + s.State, nil
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
// is in the re-encode queue. Without migration 045 a file covers no other.
func (l *RetireList) coveredOf(ctx context.Context, holder string) (string, error) {
	if covers, err := CoversReady(ctx, l.pool); err != nil || !covers {
		return "", err
	}
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

// fileOf says why the original s of the title whose folder is itemDir can't
// be retired as it lies, "" when it can: the catalog says no place of it, it
// lies outside every root the retire path deletes under, it is not there, no
// file of its own (a link, a folder) or of another size than recorded, or
// the trash, where it goes, is on another filesystem (unless
// library.trash.grace is 0, which deletes it at once). Its other names, of a
// file with more than one, are none of the library's.
func (l *RetireList) fileOf(s *Source, itemDir string, set Settings) string {
	if s.ArrivalPath == nil || *s.ArrivalPath == "" {
		return "the catalog doesn’t say where its original lies"
	}
	if _, err := l.r.originalRootOf(PathsOf(l.cfg), &retirement{src: s, itemDir: itemDir}, *s.ArrivalPath); err != nil {
		return err.Error()
	}
	fi, err := os.Lstat(*s.ArrivalPath)
	switch {
	case err != nil:
		return "its original isn’t where the catalog says: " + *s.ArrivalPath
	case !fi.Mode().IsRegular():
		return "its original is no file of its own (a link, a folder): " + *s.ArrivalPath
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

// preflight checks what the retire path reads before it records the event
// of the original s of the version v (its folder dir, which is there) that
// reads small files alone (not the original's bytes, not the package's), and
// answers what retiring it gives up, as a dry run says it (preview) and its
// digest (reviewDigest), or why the retirement would be refused: the
// version's checksums are not pending, the source's record verifies, and no
// file beside the original would be lost with it. What it gives up is what
// the package doesn't carry of the original (the deletion gate) and the files
// that go with it into the trash.
func (l *RetireList) preflight(ctx context.Context, itemDir, dir string, v *Version, s *Source) (preview, digest, why string,
	err error) {
	if pending, err := checksumsPending(dir); err != nil {
		return "", "", fmt.Sprintf("its version’s package.json can’t be read: %v", err), nil
	} else if pending {
		return "", "", "its version’s checksums are pending: no file of its package was hashed yet, so nothing proves it", nil
	}
	record, err := VerifySource(SourceDir(itemDir, s.ID))
	if err != nil {
		return "", "", fmt.Sprintf("the source’s record does not verify: %v", err), nil
	}
	rt := &retirement{src: s, itemDir: itemDir, ver: v, record: record}
	sidecars, err := l.r.sidecarsOf(ctx, rt, true)
	var refused *sidecarRefused
	if errors.As(err, &refused) {
		return "", "", refused.msg, nil
	} else if err != nil {
		return "", "", "", err
	}
	accepted, err := GateOf(itemDir, v.ID, s.ID)
	if err != nil {
		return "", "", fmt.Sprintf("the deletion gate can’t be read: %v", err), nil
	}
	lacks := "nothing"
	if len(accepted) > 0 {
		lacks = strings.Join(accepted, ", ")
	}
	var moving []string
	for _, sc := range sidecars {
		if !sc.kept {
			moving = append(moving, sc.path)
		}
	}
	goes := "none"
	if len(moving) > 0 {
		goes = strings.Join(moving, ", ")
	}
	return "what its package doesn’t carry of it: " + lacks + "; the files that go with it: " + goes,
		reviewDigest(accepted, moving), "", nil
}

// checksumsPending reports whether the package.json of the version folder
// dir says its checksums are pending: no file of the package was hashed yet.
func checksumsPending(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, PackageFile))
	if err != nil {
		return false, err
	}
	doc, err := DecodeDoc(b)
	if err != nil {
		return false, err
	}
	cs, _ := doc.Get("checksums")
	sums, _ := cs.(Doc)
	p, _ := sums.Get("pending")
	return p == true, nil
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
