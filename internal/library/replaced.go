package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An original a title was given another file in place of (replaceSource
// with deleteOldFile, the library's v2 layout). Once the title has its new
// file, the old original is no file of the title's any more. One that lies in
// its version's folder (versions/<versionId>/original.<ext>) goes the way the
// retire job takes an original (retire.go), with the replace's word for it
// in place of the deletion gate's, under the retire job's lock (HoldRetire):
//
//  1. Claim (tx A): its source is retiring, the id and the moment of its
//     event and its trash folder noted, as the retire job claims one — but
//     not held back by what holds a retirement back (a complete version that
//     holds it, the title held, its pipeline): the title has its new file.
//  2. Record its original-deleted event in the title's folder, its reason
//     ReplacedReason, by whoever asked the replace, what the version's
//     package does not carry of it accepted as the gate reads it.
//  3. Move it into the trash, .work/trash/<day>/<sourceId>/ (deleteFiles: a
//     trash grace of 0 unlinks it), where the retire job purges the day once
//     library.trash.grace is over after it, and from where it can be put
//     back until then.
//  4. tx B: its source is deleted, by whoever asked (deleted).
//
// The subtitle files that came with it stay where they are, the title's as
// they were, and the title's retire step is left alone: it is the new
// original's to come. A retirement left retiring once its event is recorded
// is finished by the retire job the same way, as the event's reason says
// (fromEvent).

// ReplacedReason is why the original a title was given another file in place
// of is deleted, as its event says.
const ReplacedReason = "the title was given another file in its place (replaceSource with deleteOldFile)"

// ReplacedBy is who deletes such an original, as its event and its source's
// row say: replaceSource, asked by caller.
func ReplacedBy(caller string) string { return clip(replacedByPrefix+caller, 255) }

// ErrRetireBusy says the retire job's lock is held — a pass, or an admin's
// list, retires originals — and was not given up within the wait.
var ErrRetireBusy = errors.New("the library's retire job is running")

// retirePoll is how often HoldRetire tries the retire job's lock.
var retirePoll = 250 * time.Millisecond

// HoldRetire takes the retire job's lock (the one a pass and an admin's list
// hold), waiting at most wait for whoever holds it, and answers its release.
// ErrRetireBusy when it is not given up within wait.
func HoldRetire(ctx context.Context, pool *pgxpool.Pool, wait time.Duration) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		locked, err := tryRetireLocks(ctx, conn)
		if err != nil {
			conn.Release()
			return nil, err
		}
		if locked {
			return func() {
				unlockRetire(ctx, conn, len(retireLocks))
				conn.Release()
			}, nil
		}
		if !time.Now().Before(deadline) {
			conn.Release()
			return nil, ErrRetireBusy
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(retirePoll):
		}
	}
}

// OriginalInVersion says whether path is the title itemID's original in one
// of its version folders (versions/<versionId>/original.<ext>), and of which
// version.
func OriginalInVersion(ctx context.Context, q Querier, p Paths, itemID, path string) (string, bool, error) {
	pl, err := PlaceOf(ctx, q, itemID)
	if notPlaced(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	vid, ok := VersionFolderOf(p.ItemDir(pl), path)
	if !ok || !IsOriginalName(filepath.Base(path)) {
		return "", false, nil
	}
	return vid, true, nil
}

// Trashed is what TrashReplaced did with the original: its source, the
// version whose folder it lay in, its event, the trash folder it lies in
// (none when a trash grace of 0 unlinked it), and from when the retire job
// purges it.
type Trashed struct {
	SourceID, VersionID, EventID string
	Trash                        string
	PurgedFrom                   time.Time
}

// TrashReplaced takes the original at path — the title itemID's old file,
// which replaceSource gave the title another in place of, in its version's
// folder — into the library's trash as the retire job takes an original
// (see the top of this file), by: who asked the replace (ReplacedBy). The
// caller holds the retire job's lock (HoldRetire). An error says why it is
// not in the trash, and whether its source was claimed: a failure once its
// event is recorded leaves it retiring, for the retire job to finish.
func (r *Retirer) TrashReplaced(ctx context.Context, itemID, path, by string) (Trashed, error) {
	var out Trashed
	set, err := ReadSettings(ctx, r.pool)
	if err != nil {
		return out, fmt.Errorf("the library's settings could not be read: %w", err)
	}
	if !set.V2() {
		return out, errors.New("the library's layout is legacy: it keeps no originals in version folders")
	}
	p := PathsOf(r.cfg)
	pl, err := PlaceOf(ctx, r.pool, itemID)
	if err != nil {
		return out, fmt.Errorf("the title has no folder in the library: %v", err)
	}
	itemDir := p.ItemDir(pl)
	vid, ok := VersionFolderOf(itemDir, path)
	if !ok || !IsOriginalName(filepath.Base(path)) {
		return out, fmt.Errorf("%s is no original in a version folder of the title", path)
	}
	src, err := sourceRow(ctx, r.pool, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources
		WHERE item_id = $1 AND arrivalpath = $2 AND state = 'present' ORDER BY id LIMIT 1`, itemID, path)
	if err != nil {
		return out, err
	}
	if src == nil {
		return out, fmt.Errorf("no present source of the title is %s", path)
	}
	out.SourceID, out.VersionID = src.ID, vid
	now := r.now().UTC()
	rt := &retirement{src: src, auth: Authority{By: by, Reason: ReplacedReason}, itemDir: itemDir, replaced: true}
	claimed, err := r.claimReplaced(ctx, p, now, rt)
	if err != nil {
		return out, err
	}
	if !claimed {
		return out, fmt.Errorf("the source %s of %s changed meanwhile: it is no present original of the title any more", src.ID, path)
	}
	s := rt.src
	out.EventID = deref(s.RetireEventID)
	if err := r.recordReplaced(ctx, rt); err != nil {
		return out, err
	}
	if err := r.deleteFiles(p, set, rt); err != nil {
		return out, r.fail(ctx, rt, err.Error())
	}
	if err := r.deleted(ctx, now, rt); err != nil {
		return out, r.fail(ctx, rt, fmt.Sprintf("the catalog could not note the deleted original: %v", err))
	}
	if set.TrashGrace > 0 {
		out.Trash = deref(s.TrashPath)
	}
	at := s.RetireEventAt.UTC()
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
	out.PurgedFrom = day.Add(24 * time.Hour).Add(set.TrashGrace)
	r.say("source:"+s.ID, "")
	return out, nil
}

// recordReplaced is step 2 of a replaced original claimed (rt.src retiring):
// its original-deleted event recorded in the title's folder, its reason
// ReplacedReason, by whoever asked the replace (rt.auth.By), with what the
// package of the version whose folder it lies in does not carry of it, as
// the gate reads it — the record's word on what went, no condition here. A
// failure gives the source back as present, its original kept (fail).
func (r *Retirer) recordReplaced(ctx context.Context, rt *retirement) error {
	s := rt.src
	rt.replaced = true
	vid, ok := VersionFolderOf(rt.itemDir, deref(s.ArrivalPath))
	if !ok || !IsOriginalName(filepath.Base(deref(s.ArrivalPath))) {
		return r.fail(ctx, rt, fmt.Sprintf("%s is no original in a version folder of the title; it is kept", deref(s.ArrivalPath)))
	}
	if accepted, err := GateOf(rt.itemDir, vid, s.ID); err == nil {
		rt.accepted = accepted
	}
	reason := ReplacedReason
	if _, err := WriteEvent(rt.itemDir, Event{ID: *s.RetireEventID, At: *s.RetireEventAt, By: rt.auth.By, Kind: EventOriginalDeleted,
		VersionID: vid, SourceID: s.ID, Reason: &reason, Accepted: rt.accepted}); err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the event cannot be recorded: %v; the original is kept", err))
	}
	rt.by = rt.auth.By
	return nil
}

// replacedClaim reports whether a retiring source was claimed for a replace
// (claimReplaced): its row names, from the claim on, the replace that deletes
// it.
func replacedClaim(s *Source) bool {
	return s.State == SourceRetiring && strings.HasPrefix(deref(s.DeletedBy), replacedByPrefix)
}

// replacedByPrefix starts every ReplacedBy.
const replacedByPrefix = "replaceSource, asked by "

// claimReplaced is tx A of a replaced original: under the item's lock, its
// source is retiring while it is still present at its path, its event's id
// and moment and its trash noted, as claim notes them for the retire job,
// and whoever asked the replace (deletedby): a retire pass that resumes it
// before its event is recorded knows it for a replaced one by that.
func (r *Retirer) claimReplaced(ctx context.Context, p Paths, now time.Time, rt *retirement) (bool, error) {
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
	claimed, err := sourceRow(ctx, tx, `UPDATE com_nalet_katalog_itemsources SET state = 'retiring', retireeventid = $2,
			retireeventat = $3, trashpath = $4, deletedby = $6, error = NULL, modifiedat = now()
		WHERE id = $1 AND state = 'present' AND arrivalpath = $5
		RETURNING `+sourceCols, s.ID, NewID(), at, p.TrashDir(at, s.ID), deref(s.ArrivalPath), clip(rt.auth.By, 255))
	if err != nil || claimed == nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	rt.src = claimed
	return true, nil
}
