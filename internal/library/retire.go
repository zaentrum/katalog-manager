package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// The retire job (platform-library/1, section 4). With library.originals set
// to delete-after-package, katalog-manager deletes a title's original once a
// version made of it is complete for library.retire.delay, unless the title
// is held (items.retirehold), when every step that reads the original is
// over: none waits or runs, and a failed one has no attempt left.
//
//  1. Claim (tx A): the source is retiring; the id and the moment of its
//     event and its trash folder are noted, and the retire step runs.
//  2. Verify: the original is the file recorded (its size and quick hash),
//     the version's chain holds (every file's hash too, unless a full
//     verification within library.verify.maxAge did that), and so does the
//     source's record, which keeps the copies of its sidecars.
//  3. Record the original-deleted event in the item's folder, unless it is:
//     what the package does not carry of the original (the deletion gate)
//     is what the deletion accepts.
//  4. Move the original and its sidecars into the trash, .work/trash/<day>/
//     <sourceId>/ (a trash grace of 0 unlinks them), and prune the arrival
//     folders left empty.
//  5. tx B: the source is deleted; its playback row is an original's, which
//     points at its record; its sidecars' subtitle rows point at the
//     package's renditions made of them, or at their copies in the record,
//     their ids kept; the retire step is done, saying what was lost.
//
// A mismatch before the event fails the retire step with the file's name
// and keeps the original: nothing is deleted, and the source is present
// again. A source left retiring by a crash resumes where it stopped: an
// event recorded is not written again, files in the trash are not moved
// again. Before its event is recorded, a retirement is called off when the
// policy is keep again, the title is held, or its pipeline reads the
// original again.
//
// The same job deletes an extra's original once its folder is recorded and
// verified (extras get no event), removes a superseded version after
// library.superseded.grace (cleanup.go), moves a legacy package folder a
// version replaced to .work/legacy/, and deletes the trash's and the legacy
// folder's days once their grace is over. A pass runs at most once a minute,
// in one instance at a time (an advisory lock), and retires at most
// library.retire.rate originals. It does nothing while library.layout is
// legacy.

// RetiredBy is who deletes an original after packaging, as its event and
// its row say.
const RetiredBy = "katalog-manager (library.originals=delete-after-package)"

// RetireReason is why, as the event says.
const RetireReason = "originals are not kept: the package is the record"

// retireLock is the advisory lock a pass holds.
const retireLock = "library-retire"

// SurroundHeld is why an original whose title's current package has no 5.1
// of the surround it had is kept (owner decision 2026-10-07): the package
// must carry it before the original goes.
const SurroundHeld = "its package has no 5.1 of the source's surround; re-encode it first"

// heldFor marks, in the details of a retire step held for its surround, the
// version whose package lacked it: the job looks at the original again once
// another version is complete.
const heldFor = "held for version "

// errHeld says an original is kept for its surround.
var errHeld = errors.New(SurroundHeld)

// Retirer runs the retire job.
type Retirer struct {
	pool  *pgxpool.Pool
	cfg   config.Config
	steps *processing.Steps
	now   func() time.Time
	every time.Duration
	last  time.Time
	// legacyAt and legacyID are how far the sweep of the legacy package
	// folders got in this process: the versions completed up to then.
	legacyAt time.Time
	legacyID string
	said     map[string]string
}

// NewRetirer is the retire job of the catalog's pool, at the paths cfg
// configures, reporting the retire step through steps.
func NewRetirer(pool *pgxpool.Pool, cfg config.Config, steps *processing.Steps) *Retirer {
	return &Retirer{pool: pool, cfg: cfg, steps: steps, now: time.Now, every: time.Minute, said: map[string]string{}}
}

// Report is what a pass did.
type Report struct {
	Originals int // originals deleted
	Extras    int // extras' originals deleted
	Versions  int // superseded versions removed
	Legacy    int // legacy package folders moved aside
	Purged    int // days of the trash and of the legacy folder deleted
	Held      int // originals kept: their package has no 5.1 of their surround
	Failed    int // originals, extras and versions that could not be
}

// String says what the pass did; "" when it did nothing.
func (r Report) String() string {
	var out []string
	for _, c := range []struct {
		n    int
		what string
	}{{r.Originals, "originals deleted"}, {r.Extras, "extras' originals deleted"}, {r.Versions, "superseded versions removed"},
		{r.Legacy, "legacy package folders moved aside"}, {r.Purged, "days of the trash and the legacy folder deleted"},
		{r.Held, "originals kept for their surround"}, {r.Failed, "failed"}} {
		if c.n > 0 {
			out = append(out, fmt.Sprintf("%d %s", c.n, c.what))
		}
	}
	return strings.Join(out, ", ")
}

// Sweep runs a pass when the last one began a minute ago or longer: the
// retry sweep calls it on each of its rounds. It says what the pass did.
func (r *Retirer) Sweep(ctx context.Context) (string, error) {
	now := r.now()
	if !r.last.IsZero() && now.Sub(r.last) < r.every {
		return "", nil
	}
	r.last = now
	rep, err := r.Pass(ctx)
	return rep.String(), err
}

// Pass runs the job once, with library.layout=v2, in one instance at a time:
// another instance's pass running, it does nothing.
func (r *Retirer) Pass(ctx context.Context) (Report, error) {
	var rep Report
	set, err := ReadSettings(ctx, r.pool)
	if err != nil {
		return rep, fmt.Errorf("the library's settings: %w", err)
	}
	if !set.V2() {
		return rep, nil
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return rep, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, retireLock).Scan(&locked); err != nil || !locked {
		return rep, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, retireLock)
	}()
	p := PathsOf(r.cfg)
	now := r.now().UTC()
	var errs []error
	n, held, failed, err := r.originals(ctx, p, set, now, set.RetireRate)
	rep.Originals, rep.Held, rep.Failed = n, held, failed
	errs = append(errs, err)
	if set.DeleteOriginals() && n+held+failed < set.RetireRate {
		n, failed, err = r.extras(ctx, p, set, now, set.RetireRate-n-held-failed)
		rep.Extras, rep.Failed = n, rep.Failed+failed
		errs = append(errs, err)
	}
	n, failed, err = r.removeVersions(ctx, p, set, now, set.RetireRate)
	rep.Versions, rep.Failed = n, rep.Failed+failed
	errs = append(errs, err)
	rep.Legacy, err = r.legacyPackages(ctx, p, set, now)
	errs = append(errs, err)
	rep.Purged, err = r.purge(p, set, now)
	errs = append(errs, err)
	return rep, errors.Join(errs...)
}

// say logs msg of the subject key once, until it says something else.
func (r *Retirer) say(key, msg string) {
	if r.said[key] == msg {
		return
	}
	if msg == "" {
		delete(r.said, key)
		return
	}
	r.said[key] = msg
	log.Printf("library: %s", msg)
}

// originals resumes the retirements left retiring, then, with the policy
// on, retires the originals that are due, at most budget in all: it
// answers how many were deleted, how many were kept for their surround and
// how many failed.
func (r *Retirer) originals(ctx context.Context, p Paths, set Settings, now time.Time, budget int) (done, held, failed int, err error) {
	todo, err := querySources(ctx, r.pool, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources
		WHERE state = 'retiring' ORDER BY retireeventat, id LIMIT $1`, budget)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("the originals being retired: %w", err)
	}
	if set.DeleteOriginals() && len(todo) < budget {
		ids, err := r.dueSources(ctx, set, now, budget-len(todo))
		if err != nil {
			return 0, 0, 0, fmt.Errorf("the originals due: %w", err)
		}
		for _, id := range ids {
			s, err := SourceByID(ctx, r.pool, id)
			if err != nil {
				return 0, 0, 0, err
			}
			if s != nil {
				todo = append(todo, s)
			}
		}
	}
	var errs []error
	for _, s := range todo {
		if ctx.Err() != nil {
			break
		}
		deleted, err := r.retire(ctx, p, set, now, s)
		switch {
		case errors.Is(err, errHeld):
			held++
		case err != nil:
			failed++
			errs = append(errs, err)
		case deleted:
			done++
		}
	}
	return done, held, failed, errors.Join(errs...)
}

// originalStepsOver holds when no step of the item $item that reads its
// original waits or runs, and none failed with an attempt left; $steps are
// those steps.
func originalStepsOver(item, steps string) string {
	return `NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps p
		WHERE p.item_id = ` + item + ` AND p.step = ANY(` + steps + `::text[])
		  AND (p.status IN ('pending', 'in_progress') OR (p.status = 'failed' AND p.nextretryat IS NOT NULL)))`
}

// dueSources are the present originals whose retirement is due, the ones of
// the versions completed first first: of a complete version for the delay,
// the title not held, its steps that read the original over, and its retire
// step not failed (unless its retry is due, or it was kept for its surround
// in another version than the one complete now).
func (r *Retirer) dueSources(ctx context.Context, set Settings, now time.Time, n int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT s.id FROM com_nalet_katalog_itemsources s
		JOIN com_nalet_katalog_items i ON i.id = s.item_id
		JOIN com_nalet_katalog_itemversions v ON v.item_id = s.item_id AND v.state = 'complete' AND s.id = ANY(v.sourceids)
		WHERE s.state = 'present' AND NOT i.retirehold
		  AND v.completedat + make_interval(secs => $1::float8) <= $2::timestamptz
		  AND `+originalStepsOver("s.item_id", "$3")+`
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps r
			WHERE r.item_id = s.item_id AND r.step = 'retire' AND r.status = 'failed'
			  AND (r.nextretryat IS NULL OR r.nextretryat > $2::timestamptz)
			  AND NOT (COALESCE(r.details, '') LIKE '`+heldFor+`%' AND r.details <> '`+heldFor+`' || v.id))
		ORDER BY v.completedat, s.id
		LIMIT $4`, set.RetireDelay.Seconds(), now, processing.OriginalSteps, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// retirement is what retiring an original takes: the source, its item's
// folder, the version that holds it, and its sidecars.
type retirement struct {
	src      *Source
	itemDir  string
	ver      *Version
	record   Doc       // the source's record, source.json
	sidecars []sidecar // its subtitle rows and the files the record lists beside it
	accepted []string
}

// sidecar is a file beside an original that goes with it: a subtitle file
// the scanner paired (its row, and what the row points at once the original
// is gone), or one the source's record lists as copied.
type sidecar struct {
	path   string // beside the original
	asset  string // the subtitle row's id; "" for a file only the record lists
	to     string // what the row points at after
	webvtt bool   // a rendition of the package, which is WebVTT
}

// retire takes the source s through what is left of its retirement. It
// answers whether the original is deleted now; an error when it is not and
// should be: the step and the row say why.
func (r *Retirer) retire(ctx context.Context, p Paths, set Settings, now time.Time, s *Source) (bool, error) {
	rt := &retirement{src: s}
	pl, err := PlaceOf(ctx, r.pool, s.ItemID)
	if errors.Is(err, ErrNoItem) {
		return false, nil // deleted with its item meanwhile
	}
	if err != nil {
		return false, r.fail(ctx, rt, fmt.Sprintf("the title has no folder in the library: %v; nothing is deleted", err))
	}
	rt.itemDir = p.ItemDir(pl)
	recorded, _, err := r.eventOf(rt)
	if err != nil {
		return false, r.fail(ctx, rt, err.Error())
	}
	if recorded != "" {
		if err := r.fromEvent(ctx, rt, recorded); err != nil {
			return false, r.fail(ctx, rt, err.Error())
		}
	} else {
		if s.State == SourceRetiring {
			if why, err := r.calledOff(ctx, set, s.ItemID); err != nil {
				return false, err
			} else if why != "" {
				return false, r.callOff(ctx, rt, why)
			}
		}
		if err := r.prepare(ctx, p, set, now, rt); err != nil || rt.src == nil {
			return false, err // errHeld: kept for its surround
		}
	}
	if err := r.deleteFiles(p, set, rt); err != nil {
		return false, r.fail(ctx, rt, err.Error())
	}
	if err := r.deleted(ctx, now, rt); err != nil {
		return false, r.fail(ctx, rt, fmt.Sprintf("the catalog could not note the deleted original: %v", err))
	}
	r.say("source:"+s.ID, "")
	return true, nil
}

// eventOf finds the event of a retiring source, in the folder its id and
// moment name: recorded (its checksums written), or begun and not recorded;
// both "" when there is none.
func (r *Retirer) eventOf(rt *retirement) (recorded, begun string, err error) {
	s := rt.src
	if s.State != SourceRetiring || s.RetireEventID == nil || s.RetireEventAt == nil || rt.itemDir == "" {
		return "", "", nil
	}
	dir := EventDir(rt.itemDir, *s.RetireEventAt, *s.RetireEventID, EventOriginalDeleted)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	} else if err != nil {
		return "", "", fmt.Errorf("the event folder %s cannot be read: %v", dir, err)
	}
	if statOK(filepath.Join(dir, SumsFile)) {
		return dir, "", nil
	}
	return "", dir, nil
}

// fromEvent reads what a recorded event says: the version, and what the
// deletion accepted.
func (r *Retirer) fromEvent(ctx context.Context, rt *retirement, dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, EventFile))
	if err != nil {
		return err
	}
	ev, err := DecodeDoc(b)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, EventFile), err)
	}
	vid, _ := ev.Get("versionId")
	id, _ := vid.(string)
	if rt.ver, err = VersionByID(ctx, r.pool, id); err != nil {
		return err
	}
	if rt.ver == nil {
		return fmt.Errorf("the event %s names version %s, which the catalog does not hold", dir, id)
	}
	acc, _ := ev.Get("accepted")
	rt.accepted = stringsOf(acc)
	rt.record, err = readSourceRecord(SourceDir(rt.itemDir, rt.src.ID))
	if err != nil {
		return err
	}
	rt.sidecars, err = r.sidecarsOf(ctx, rt, false)
	return err
}

// calledOff says why a retirement whose event is not recorded is called
// off: the policy keeps originals again, the title is held, or its pipeline
// reads the original again; "" when it goes on.
func (r *Retirer) calledOff(ctx context.Context, set Settings, itemID string) (string, error) {
	if !set.DeleteOriginals() {
		return "library.originals is keep: the original is kept", nil
	}
	var held, busy bool
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT retirehold FROM com_nalet_katalog_items WHERE id = $1), false),
		NOT `+originalStepsOver("$1", "$2"), itemID, processing.OriginalSteps).Scan(&held, &busy)
	switch {
	case err != nil:
		return "", err
	case held:
		return "the title holds its originals (retirehold): the original is kept", nil
	case busy:
		return "the title's pipeline reads its original again: the retirement waits for it", nil
	}
	return "", nil
}

// callOff calls a retirement off before its event is recorded: the source is
// present again, and the retire step waits.
func (r *Retirer) callOff(ctx context.Context, rt *retirement, why string) error {
	s := rt.src
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, s.ItemID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET state = 'present', retireeventid = NULL,
			retireeventat = NULL, trashpath = NULL, error = NULL, modifiedat = now()
		WHERE id = $1 AND state = 'retiring'`, s.ID); err != nil {
		return err
	}
	if err := r.steps.UpsertIn(ctx, tx, s.ItemID, processing.StepRetire, processing.StatusPending, nil, &why); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if _, begun, _ := r.eventOf(rt); begun != "" {
		_ = os.RemoveAll(begun)
	}
	r.say("source:"+s.ID, fmt.Sprintf("the retirement of original %s of item %s is called off: %s", s.ID, s.ItemID, why))
	return nil
}

// prepare claims a present source (tx A), checks it, verifies its version
// and records its event. On a mismatch it fails the retire step and keeps
// the original; rt.src is nil when the source was not claimed after all, or
// its retirement was called off.
func (r *Retirer) prepare(ctx context.Context, p Paths, set Settings, now time.Time, rt *retirement) error {
	ver, err := r.versionOf(ctx, rt.src)
	if err != nil {
		return err
	}
	if ver == nil {
		return r.fail(ctx, rt, "no complete version holds the original any more; nothing is deleted")
	}
	rt.ver = ver
	if lacks, err := surroundLost(rt.itemDir, ver.ID, rt.src.ID); err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the essences of the source and of version %s cannot be read: %v; nothing is deleted",
			ver.ID, err))
	} else if lacks {
		if err := r.hold(ctx, rt); err != nil {
			return err
		}
		rt.src = nil
		return errHeld
	}
	if rt.src.State == SourcePresent {
		claimed, err := r.claim(ctx, p, now, rt)
		if err != nil || !claimed {
			rt.src = nil
			return err
		}
	} else if rt.src.RetireEventID == nil || rt.src.RetireEventAt == nil {
		return r.fail(ctx, rt, "its retirement was claimed without its event's id and moment; nothing is deleted")
	}
	s := rt.src
	if s.ArrivalPath == nil {
		return r.fail(ctx, rt, "the catalog does not say where the original lies; nothing is deleted")
	}
	if _, err := r.rootOf(p, *s.ArrivalPath); err != nil {
		return r.fail(ctx, rt, err.Error()+"; nothing is deleted")
	}
	if err := isRecordedOriginal(*s.ArrivalPath, s.SizeBytes, s.QH1); err != nil {
		return r.fail(ctx, rt, err.Error())
	}
	if rt.record, err = VerifySource(SourceDir(rt.itemDir, s.ID)); err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the source's record does not verify: %v; the original is kept", err))
	}
	if rt.sidecars, err = r.sidecarsOf(ctx, rt, true); err != nil {
		return r.fail(ctx, rt, err.Error())
	}
	level, err := r.verify(ctx, set, now, rt.ver)
	if err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("version %s does not verify (%s): %v; the original is kept", rt.ver.ID, level, err))
	}
	if why, err := r.calledOff(ctx, set, s.ItemID); err != nil {
		return err
	} else if why != "" {
		err := r.callOff(ctx, rt, why)
		rt.src = nil
		return err
	}
	if rt.accepted, err = GateOf(rt.itemDir, rt.ver.ID, s.ID); err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the deletion gate cannot be read: %v; the original is kept", err))
	}
	reason := RetireReason
	if _, err := WriteEvent(rt.itemDir, Event{ID: *s.RetireEventID, At: *s.RetireEventAt, By: RetiredBy,
		Kind: EventOriginalDeleted, VersionID: rt.ver.ID, SourceID: s.ID, Reason: &reason, Accepted: rt.accepted}); err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the event cannot be recorded: %v; the original is kept", err))
	}
	return nil
}

// surroundLost reports whether the package of the version vid lacks the
// surround the source sid had (owner decision 2026-10-07), from the essences
// of their records, as the deletion gate reads them (the package's counts
// its 5.1 companions): the source had surround and the package has none, or
// the package carries fewer channels than a 5.1 companion of the source's
// would (a 7.1 source's 5.1 is enough, a 5.1 source's stereo is not).
func surroundLost(itemDir, vid, sid string) (bool, error) {
	src, err := Essence(filepath.Join(SourceDir(itemDir, sid), "source.json"))
	if err != nil {
		return false, err
	}
	pkg, err := Essence(filepath.Join(VersionDir(itemDir, vid), PackageFile))
	if err != nil {
		return false, err
	}
	srcSurround, _ := src.Get("surround")
	pkgSurround, _ := pkg.Get("surround")
	if truthy(srcSurround) && !truthy(pkgSurround) {
		return true, nil
	}
	had, _ := src.Get("maxAudioChannels")
	kept, _ := pkg.Get("maxAudioChannels")
	n, ok := toFloat(had)
	k, _ := toFloat(kept)
	return ok && n > 2 && k < min(n, 6), nil
}

// hold keeps the original of rt, whose version's package has no 5.1 of its
// surround: the source stays present (a claimed one is present again, an
// event folder begun goes), saying why, and the retire step waits: failed,
// with no retry of its own, held for the version (heldFor). The job looks at
// the original again once another version of the title is complete, and an
// admin's retry of the step has it look now.
func (r *Retirer) hold(ctx context.Context, rt *retirement) error {
	s := rt.src
	_, begun, _ := r.eventOf(rt)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, s.ItemID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET state = 'present', retireeventid = NULL,
			retireeventat = NULL, trashpath = NULL, error = $2, modifiedat = now()
		WHERE id = $1 AND state IN ('present', 'retiring')`, s.ID, SurroundHeld); err != nil {
		return err
	}
	reason := processing.CleanError(SurroundHeld)
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemprocessingsteps
			(id, createdat, modifiedat, item_id, step, status, finishedat, attempts, error, details, failures, lasterror,
			 nextretryat, dispatchedat)
		VALUES (gen_random_uuid()::varchar, now(), now(), $1, 'retire', 'failed', now(), 0, $2::text, $3::text, 1, $2::text, NULL, NULL)
		ON CONFLICT (item_id, step) DO UPDATE SET status = 'failed', finishedat = now(), modifiedat = now(),
			error = $2::text, lasterror = $2::text, details = $3::text, nextretryat = NULL, dispatchedat = NULL,
			failures = CASE WHEN com_nalet_katalog_itemprocessingsteps.status <> 'failed'
				THEN com_nalet_katalog_itemprocessingsteps.failures + 1 ELSE com_nalet_katalog_itemprocessingsteps.failures END`,
		s.ItemID, *reason, heldFor+rt.ver.ID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if begun != "" {
		_ = os.RemoveAll(begun)
	}
	r.say("source:"+s.ID, fmt.Sprintf("original %s of item %s is kept: %s (version %s); it is retired once a version "+
		"whose package carries it is complete", s.ID, s.ItemID, SurroundHeld, rt.ver.ID))
	return nil
}

// versionOf is the complete version that holds the source s; nil when none
// does.
func (r *Retirer) versionOf(ctx context.Context, s *Source) (*Version, error) {
	v, err := Current(ctx, r.pool, s.ItemID)
	if err != nil || v == nil {
		return nil, err
	}
	for _, id := range v.SourceIDs {
		if id == s.ID {
			return v, nil
		}
	}
	return nil, nil
}

// claim is tx A: under the item's lock, the source is retiring if it still
// qualifies, its event's id and moment and its trash noted, and the retire
// step runs.
func (r *Retirer) claim(ctx context.Context, p Paths, now time.Time, rt *retirement) (bool, error) {
	s := rt.src
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, s.ItemID); err != nil {
		return false, err
	}
	at := now.Truncate(time.Second)
	id := NewID()
	trash := p.TrashDir(at, s.ID)
	claimed, err := sourceRow(ctx, tx, `UPDATE com_nalet_katalog_itemsources s SET state = 'retiring', retireeventid = $2,
			retireeventat = $3, trashpath = $4, error = NULL, modifiedat = now()
		WHERE s.id = $1 AND s.state = 'present'
		  AND NOT COALESCE((SELECT retirehold FROM com_nalet_katalog_items i WHERE i.id = s.item_id), true)
		  AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
			WHERE v.id = $5 AND v.item_id = s.item_id AND v.state = 'complete' AND s.id = ANY(v.sourceids))
		  AND `+originalStepsOver("s.item_id", "$6")+`
		RETURNING `+sourceCols, s.ID, id, at, trash, rt.ver.ID, processing.OriginalSteps)
	if err != nil || claimed == nil {
		return false, err
	}
	if err := r.steps.UpsertIn(ctx, tx, s.ItemID, processing.StepRetire, processing.StatusInProgress, nil, nil); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	rt.src = claimed
	return true, nil
}

// verify checks the version's chain at the level the settings ask for, and
// notes it: full, unless a full verification within library.verify.maxAge
// did that (the migration hashed every byte), when the chain does.
func (r *Retirer) verify(ctx context.Context, set Settings, now time.Time, v *Version) (string, error) {
	level := set.Verify
	if level == VerifyFull && v.VerifiedLevel != nil && *v.VerifiedLevel == VerifyFull && v.VerifiedAt != nil &&
		!v.VerifiedAt.Before(now.Add(-set.VerifyMaxAge)) {
		level = VerifyChain
	}
	dir := deref(v.Dir)
	if dir == "" {
		return level, errors.New("the catalog does not say where its folder is")
	}
	if _, err := VerifyVersion(dir, level == VerifyFull); err != nil {
		return level, err
	}
	// A chain verified keeps a full verification within its age as it is.
	_, err := r.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = $2::timestamptz, verifiedlevel = $3::text,
			modifiedat = now()
		WHERE id = $1 AND NOT ($3::text = 'chain' AND verifiedlevel = 'full'
			AND verifiedat >= $2::timestamptz - make_interval(secs => $4::float8))`,
		v.ID, now, level, set.VerifyMaxAge.Seconds())
	return level, err
}

// isRecordedOriginal checks that the file at path is the original as it
// was recorded: its size and its quick hash.
func isRecordedOriginal(path string, size int64, qh1 *string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("the original changed since it was recorded: %s cannot be read (%v); nothing is deleted", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("the original changed since it was recorded: %s is no file; nothing is deleted", path)
	}
	if qh1 == nil {
		return fmt.Errorf("the original %s was recorded without its quick hash, so it cannot be told from another; nothing is deleted", path)
	}
	gotSize, got, err := QH1(path)
	if err != nil {
		return fmt.Errorf("the original changed since it was recorded: %s cannot be read (%v); nothing is deleted", path, err)
	}
	if gotSize != size || got != *qh1 {
		return fmt.Errorf("the original changed since it was recorded: %s is %d bytes with quick hash %s, recorded %d bytes with %s; nothing is deleted",
			path, gotSize, got, size, *qh1)
	}
	return nil
}

// readSourceRecord reads source.json of the source folder dir.
func readSourceRecord(dir string) (Doc, error) {
	b, err := os.ReadFile(filepath.Join(dir, "source.json"))
	if err != nil {
		return nil, err
	}
	d, err := DecodeDoc(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "source.json"), err)
	}
	return d, nil
}

// VerifySource checks a source's record, sources/<sourceId>/: its
// checksums.sha256 lists source.json and every file beside it (the probe,
// the copies of the sidecars), each of the hash it lists. It answers
// source.json.
func VerifySource(dir string) (Doc, error) {
	listed, err := ReadChecksums(filepath.Join(dir, SumsFile))
	if err != nil {
		return nil, broken(filepath.Join(dir, SumsFile), "%v", err)
	}
	if _, ok := listed["source.json"]; !ok {
		return nil, broken(filepath.Join(dir, SumsFile), "it does not list source.json")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, broken(dir, "%v", err)
	}
	present := 0
	for _, e := range entries {
		if e.Name() == SumsFile || osArtefacts.MatchString(e.Name()) {
			continue
		}
		if _, ok := listed[e.Name()]; !ok {
			return nil, broken(filepath.Join(dir, e.Name()), "it is not listed in checksums.sha256")
		}
		present++
	}
	for name, want := range listed {
		path := filepath.Join(dir, filepath.FromSlash(name))
		got, _, err := SHA256File(path)
		if err != nil {
			return nil, broken(path, "%v", err)
		}
		if got != want {
			return nil, broken(path, "its hash is sha256:%s, and checksums.sha256 lists %s", got, want)
		}
	}
	if present != len(listed) {
		return nil, broken(filepath.Join(dir, SumsFile), "it lists %d files, and %d are there", len(listed), present)
	}
	return readSourceRecord(dir)
}

// recordSidecar is a file a source's record lists as copied from beside its
// original.
type recordSidecar struct{ file, originalName string }

// recordSidecars are the sidecars[] of a source's record.
func recordSidecars(record Doc) []recordSidecar {
	v, _ := record.Get("sidecars")
	list, _ := v.([]any)
	var out []recordSidecar
	for _, e := range list {
		d, ok := e.(Doc)
		if !ok {
			continue
		}
		out = append(out, recordSidecar{file: str(d, "file"), originalName: str(d, "originalName")})
	}
	return out
}

// mappedSidecar is how the packager made a rendition of a sidecar
// (itemsources.sidecars): the subtitle row, and the rendition's file
// relative to the version's folder.
type mappedSidecar struct {
	SubtitleAssetID string `json:"subtitleAssetId"`
	Rendition       string `json:"rendition"`
	Path            string `json:"path"`
}

// sidecarsOf are the files that go with the original of rt: the subtitle
// rows of its sidecars (those the packager mapped, and those beside it named
// as it is), each with what it points at once the original is gone, and the
// other files its record lists as copied from beside it. A subtitle file
// that neither the package nor the record holds is refused before the event
// is recorded (strict): deleting it would lose it. Once the event is
// recorded the deletion goes on, and such a file stays where it is.
func (r *Retirer) sidecarsOf(ctx context.Context, rt *retirement, strict bool) ([]sidecar, error) {
	s := rt.src
	mapped := map[string]mappedSidecar{}
	if len(s.Sidecars) > 0 {
		var list []mappedSidecar
		if err := json.Unmarshal(s.Sidecars, &list); err != nil {
			return nil, fmt.Errorf("the catalog's map of the sidecars of source %s cannot be read: %v", s.ID, err)
		}
		for _, m := range list {
			mapped[m.SubtitleAssetID] = m
		}
	}
	copies := recordSidecars(rt.record)
	copyOf := map[string]string{}
	for _, c := range copies {
		if c.originalName != "" && c.file != "" {
			copyOf[c.originalName] = c.file
		}
	}
	original := deref(s.ArrivalPath)
	rows, err := r.pool.Query(ctx, `SELECT id, path FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY id`, s.ItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sidecar
	seen := map[string]bool{}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		m, isMapped := mapped[id]
		if !isMapped && (original == "" || !besideOriginal(original, path)) {
			continue
		}
		sc := sidecar{path: path, asset: id}
		switch {
		case isMapped:
			if rt.ver == nil || deref(rt.ver.Dir) == "" {
				return nil, fmt.Errorf("the version of the rendition of the subtitle file %s has no folder", path)
			}
			sc.to, sc.webvtt = filepath.Join(deref(rt.ver.Dir), filepath.FromSlash(m.Path)), true
		case copyOf[filepath.Base(path)] != "":
			sc.to = filepath.Join(rt.itemDir, filepath.FromSlash(copyOf[filepath.Base(path)]))
		case !strict:
			continue
		default:
			return nil, fmt.Errorf("the subtitle file %s beside the original is in neither the package of version %s "+
				"nor the source's record (it was paired after packaging): deleting the original would lose it; "+
				"package the title again to keep it, then the original is deleted", path, versionIDOf(rt.ver))
		}
		seen[filepath.Clean(path)] = true
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if original != "" {
		for _, c := range copies {
			if c.originalName == "" || strings.ContainsAny(c.originalName, `/\`) {
				continue
			}
			path := filepath.Join(filepath.Dir(original), c.originalName)
			if !seen[path] {
				seen[path] = true
				out = append(out, sidecar{path: path})
			}
		}
	}
	return out, nil
}

func versionIDOf(v *Version) string {
	if v == nil {
		return "none"
	}
	return v.ID
}

// besideOriginal reports whether the file at path lies beside the original
// and is named as it is (Film.srt, Film.en.srt beside Film.mkv): one of its
// sidecars, as the scanner pairs them.
func besideOriginal(original, path string) bool {
	original, path = filepath.Clean(original), filepath.Clean(path)
	if filepath.Dir(original) != filepath.Dir(path) || original == path {
		return false
	}
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(original), filepath.Ext(original)))
	return strings.HasPrefix(strings.ToLower(filepath.Base(path)), stem+".")
}

// rootOf is the root the original at path lies under, which the job prunes
// emptied folders up to: the arrivals' (ARRIVALS_ROOT), the extras' arrivals
// (EXTRAS_ROOT) or the media root before the library (NFS_ROOT). It deletes
// nothing anywhere else, nor in the record.
func (r *Retirer) rootOf(p Paths, path string) (string, error) {
	for _, root := range []string{p.Arrivals, p.Extras, r.cfg.NFSRoot, r.cfg.Roots(false).Extras} {
		if Within(root, path) && !r.inRecord(p, path) {
			return filepath.Clean(root), nil
		}
	}
	return "", fmt.Errorf("%s lies outside the arrivals (ARRIVALS_ROOT, EXTRAS_ROOT) and the media root (NFS_ROOT): "+
		"the retire job deletes nothing there", path)
}

// inRecord reports whether path lies in the library's record.
func (r *Retirer) inRecord(p Paths, path string) bool {
	for _, d := range []string{MoviesDir, SeriesDir, PeopleDir} {
		if Within(filepath.Join(p.Root, d), path) {
			return true
		}
	}
	return false
}

// deleteFiles moves the original of rt and its sidecars into its trash, or
// unlinks them with a trash grace of 0, and prunes the folders it emptied
// up to their root. A file not where it was is in the trash already, or was
// unlinked.
func (r *Retirer) deleteFiles(p Paths, set Settings, rt *retirement) error {
	s := rt.src
	original := deref(s.ArrivalPath)
	trash := deref(s.TrashPath)
	if trash == "" && s.RetireEventAt != nil {
		trash = p.TrashDir(*s.RetireEventAt, s.ID)
	}
	files := []string{}
	if original != "" {
		files = append(files, original)
	}
	for _, sc := range rt.sidecars {
		files = append(files, sc.path)
	}
	pruned := map[string]string{}
	for _, f := range files {
		root, err := r.rootOf(p, f)
		if err != nil {
			if f == original {
				return err
			}
			continue // a sidecar elsewhere stays where it is
		}
		fi, err := os.Lstat(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s cannot be read: %v", f, err)
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		if set.TrashGrace == 0 {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s cannot be deleted: %v", f, err)
			}
		} else {
			if trash == "" {
				return errors.New("the catalog does not say where its trash is")
			}
			if err := MkdirAll(trash); err != nil {
				return fmt.Errorf("the trash %s cannot be made: %v", trash, err)
			}
			if err := os.Rename(f, filepath.Join(trash, filepath.Base(f))); err != nil {
				if errors.Is(err, syscall.EXDEV) {
					return fmt.Errorf("%s cannot be moved into the trash %s, which is on another filesystem: put the trash "+
						"(WORK_ROOT) on the share the arrivals are on, or set library.trash.grace to 0 to delete originals at once", f, trash)
				}
				return fmt.Errorf("%s cannot be moved into the trash: %v", f, err)
			}
		}
		pruned[filepath.Dir(f)] = root
	}
	for dir, root := range pruned {
		pruneEmpty(dir, root)
	}
	return nil
}

// pruneEmpty removes dir and its parents up to (not) stop while they are
// empty.
func pruneEmpty(dir, stop string) {
	stop = filepath.Clean(stop)
	for dir = filepath.Clean(dir); dir != stop && Within(stop, dir); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			return
		}
	}
}

// deleted is tx B: the source is deleted, its playback row an original's in
// its record, its sidecars' rows repointed, and the retire step done,
// saying what the deletion gave up.
func (r *Retirer) deleted(ctx context.Context, now time.Time, rt *retirement) error {
	s := rt.src
	lost, err := json.Marshal(nonNil(rt.accepted))
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, s.ItemID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET state = 'deleted', deletedat = $2, deletedby = $3,
			arrivalpath = NULL, error = NULL, lost = $4, modifiedat = now()
		WHERE id = $1 AND state = 'retiring'`, s.ID, now, RetiredBy, lost)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // noted already
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET isprimary = false, kind = 'original', path = $3, sourceid = $1
		WHERE item_id = $2 AND COALESCE(kind, 'primary') = 'primary'
		  AND (sourceid = $1 OR (sourceid IS NULL AND path = $4))`,
		s.ID, s.ItemID, SourceDir(rt.itemDir, s.ID), deref(s.ArrivalPath)); err != nil {
		return err
	}
	for _, sc := range rt.sidecars {
		if sc.asset == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_subtitleassets SET path = $2,
				format = CASE WHEN $3 THEN 'webvtt' ELSE format END
			WHERE id = $1`, sc.asset, sc.to, sc.webvtt); err != nil {
			return err
		}
	}
	details := "lost: nothing"
	if len(rt.accepted) > 0 {
		details = "lost: " + strings.Join(rt.accepted, ", ")
	}
	if err := r.steps.UpsertIn(ctx, tx, s.ItemID, processing.StepRetire, processing.StatusDone, nil, &details); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// fail says why the original of rt was not deleted, in its row and in the
// item's retire step, which fails. A claimed source whose event is not
// recorded is present again, its original kept (an event folder begun is
// deleted); one whose event is recorded stays retiring, and the next pass
// goes on with it.
func (r *Retirer) fail(ctx context.Context, rt *retirement, msg string) error {
	s := rt.src
	bg := context.WithoutCancel(ctx)
	recorded, begun, _ := r.eventOf(rt)
	if _, err := r.pool.Exec(bg, `UPDATE com_nalet_katalog_itemsources SET error = $2, modifiedat = now(),
			state = CASE WHEN state = 'retiring' AND NOT $3 THEN 'present' ELSE state END,
			retireeventid = CASE WHEN state = 'retiring' AND NOT $3 THEN NULL ELSE retireeventid END,
			retireeventat = CASE WHEN state = 'retiring' AND NOT $3 THEN NULL ELSE retireeventat END,
			trashpath = CASE WHEN state = 'retiring' AND NOT $3 THEN NULL ELSE trashpath END
		WHERE id = $1 AND state IN ('present', 'retiring')`, s.ID, clip(msg, 500), recorded != ""); err != nil {
		log.Printf("library: the failure of the retirement of original %s could not be noted: %v", s.ID, err)
	} else if recorded == "" && begun != "" {
		_ = os.RemoveAll(begun)
	}
	if err := r.steps.Upsert(bg, s.ItemID, processing.StepRetire, processing.StatusFailed, &msg, nil); err != nil {
		log.Printf("library: the retire step of item %s could not be failed: %v", s.ItemID, err)
	}
	r.say("source:"+s.ID, fmt.Sprintf("original %s of item %s is not deleted: %s", s.ID, s.ItemID, msg))
	return fmt.Errorf("original %s of item %s: %s", s.ID, s.ItemID, msg)
}

// clip is s cut to n characters at most.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
