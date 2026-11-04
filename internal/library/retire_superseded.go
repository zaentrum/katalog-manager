package library

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/processing"
)

// The retire job retires an original another file took the place of as its
// title's (superseded, superseded.go) once the title's master no longer needs
// it: once a version of the title made without it is complete — its master,
// the version that plays — and verified as a retirement verifies a version.
// It is the replace's word that has it go (deleteOldFile), so it does
// whatever library.originals says; the rest holds it as an original is held,
// the same path, the same checks:
//
//  1. Due: the source is superseded; the title does not hold its originals
//     (retirehold); none of its steps that read an original waits or runs;
//     its master is not made of it and has been complete for
//     library.retire.delay. One whose retirement failed is looked at again an
//     hour later. Each counts against library.retire.rate.
//  2. Held: its package must carry the surround the original had, as of an
//     original retired by the policy (SurroundHeld): one that has none is
//     kept, superseded, saying why on its row.
//  3. Claim (tx A, under the title's lock): retiring, its event's id and
//     moment and its trash noted; supersededby stays, which tells this
//     retirement from another's.
//  4. Check: the file is where the catalog says, under a root the retire job
//     deletes under or in its version's folder, outside the package store,
//     no file of a title any more, and the one recorded (its size and quick
//     hash); its record verifies, when it has one; the master verifies.
//  5. Its original-deleted event, when the source has its record in the
//     title's folder: naming the version made of it (the one whose folder
//     keeps it, else the latest), what that version's package does not carry
//     of it accepted (the deletion gate), its reason the master that took its
//     place. A source with no record — an original replaced before any
//     version was made of it — has no event: nothing in the title's folder
//     names it, and its row records its deletion.
//  6. Move it into the trash, .work/trash/<day>/<sourceId>/ (deleteFiles;
//     unlinked with a trash grace of 0), alone: the subtitle files beside it
//     stay the title's where they are. The retire job purges the day once
//     library.trash.grace is over after it.
//  7. tx B: the source is deleted, by the retire job; the title's retire step
//     is left alone, the new original's to come.
//
// Called off before its event — the title holds its originals, its pipeline
// reads an original again, its master changed — it is superseded again; one
// that fails before its event is too, saying why. One whose event is
// recorded, or, with no record, whose file left its place, is finished by
// the next pass. Nothing in it is a replace's own: an original superseded
// however is retired so once its title's master is verified.

// SupersededRetiredBy is who deletes a superseded original, as its event and
// its row say.
const SupersededRetiredBy = "katalog-manager (a superseded original, retired once its title's master is verified)"

// supersededReason is why a superseded original is deleted, as its event
// says: the source that took its place, and the title's master.
func supersededReason(by, master string) string {
	return fmt.Sprintf("another file took its place as the title's (source %s), and version %s of the title, made without it, "+
		"is complete and verified", by, master)
}

// supersededRetry is how long a superseded original whose retirement failed,
// or was held, waits before it is looked at again.
const supersededRetry = time.Hour

// supersededOriginals retires the superseded originals that are due, at most
// budget: it answers how many were deleted, how many were kept for their
// surround and how many failed.
func (r *Retirer) supersededOriginals(ctx context.Context, p Paths, set Settings, now time.Time, budget int) (done, held, failed int,
	err error) {
	ids, err := r.supersededDue(ctx, set, now, budget)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("the superseded originals due: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		s, err := SourceByID(ctx, r.pool, id)
		if err != nil {
			return done, held, failed, err
		}
		if s == nil || s.State != SourceSuperseded {
			continue
		}
		by, err := SupersededBy(ctx, r.pool, s.ID)
		if err != nil {
			return done, held, failed, err
		}
		deleted, err := r.retireOf(ctx, p, set, now, &retirement{src: s, superseded: by})
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

// supersededDue are the superseded originals whose retirement is due, those
// superseded first first, one that failed or was held an hour later: of a
// title that does not hold its originals, its steps that read an original
// over, whose master is not made of it and complete for library.retire.delay.
// None on a catalog without migration 056.
func (r *Retirer) supersededDue(ctx context.Context, set Settings, now time.Time, n int) ([]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT s.id FROM com_nalet_katalog_itemsources s
		JOIN com_nalet_katalog_items i ON i.id = s.item_id
		JOIN com_nalet_katalog_itemversions v ON v.item_id = s.item_id AND v.state = 'complete' AND NOT (s.id = ANY(v.sourceids))
		WHERE s.state = 'superseded' AND s.supersededby IS NOT NULL AND NOT i.retirehold
		  AND v.completedat + make_interval(secs => $1::float8) <= $2::timestamptz
		  AND (s.error IS NULL OR s.modifiedat + make_interval(secs => $5::float8) <= $2::timestamptz)
		  AND `+originalStepsOver("s.item_id", "$3")+`
		ORDER BY s.error IS NOT NULL, s.supersededat, s.id
		LIMIT $4`, set.RetireDelay.Seconds(), now, processing.OriginalSteps, n, supersededRetry.Seconds())
	if isUndefinedColumn(err) {
		return nil, nil
	}
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

// masterOf is the master of the title of the superseded original s that no
// longer needs it: its complete version, when that is not made of s; nil when
// there is none.
func (r *Retirer) masterOf(ctx context.Context, s *Source) (*Version, error) {
	v, err := Current(ctx, r.pool, s.ItemID)
	if err != nil || v == nil || slices.Contains(v.SourceIDs, s.ID) {
		return nil, err
	}
	return v, nil
}

// retireSuperseded takes the retirement of the superseded original of rt
// through what is left of it (see the top of this file). It answers whether
// the original is deleted now (and rt.why why not, when that is no failure);
// an error when it is not and should be: its row says why.
func (r *Retirer) retireSuperseded(ctx context.Context, p Paths, set Settings, now time.Time, rt *retirement) (bool, error) {
	s := rt.src
	rt.auth = Authority{By: SupersededRetiredBy}
	pl, err := PlaceOf(ctx, r.pool, s.ItemID)
	if errors.Is(err, ErrNoItem) {
		rt.why = "the title was deleted meanwhile"
		return false, nil
	}
	if err != nil {
		return false, r.fail(ctx, rt, fmt.Sprintf("the title has no folder in the library: %v; nothing is deleted", err))
	}
	rt.itemDir = p.ItemDir(pl)
	recorded, _, err := r.eventOf(rt)
	if err != nil {
		return false, r.fail(ctx, rt, err.Error())
	}
	switch {
	case recorded != "":
		if err := r.fromSupersededEvent(rt, recorded); err != nil {
			return false, r.fail(ctx, rt, err.Error())
		}
	case s.State == SourceRetiring && !Recorded(rt.itemDir, s) && gone(s.ArrivalPath):
		// No record, so no event: its file left its place in this
		// retirement, which goes on with tx B.
		rt.by = rt.auth.By
	default:
		if s.State == SourceRetiring {
			if why, err := r.supersededCalledOff(ctx, rt); err != nil {
				return false, err
			} else if why != "" {
				return false, r.callOff(ctx, rt, why)
			}
		}
		if err := r.prepareSuperseded(ctx, p, set, now, rt); err != nil || rt.src == nil {
			return false, err // errHeld: kept for its surround
		}
	}
	if err := r.claimedStill(ctx, rt); err != nil {
		return false, err
	}
	if err := r.deleteFiles(p, set, rt); err != nil {
		return false, r.fail(ctx, rt, err.Error())
	}
	if err := r.deleted(ctx, now, rt); err != nil {
		return false, r.fail(ctx, rt, fmt.Sprintf("the catalog could not note the deleted original: %v", err))
	}
	r.say("source:"+s.ID, "")
	where := "unlinked (library.trash.grace is 0)"
	if set.TrashGrace > 0 {
		where = "moved into the trash " + deref(rt.src.TrashPath)
	}
	master := "verified before"
	if rt.ver != nil {
		master = "version " + rt.ver.ID
	}
	log.Printf("library: superseded original %s of item %s (%s) is retired, %s; its title's master: %s", s.ID, s.ItemID,
		s.Filename, where, master)
	return true, nil
}

// gone reports whether nothing is at path any more (nil: no path).
func gone(path *string) bool {
	if path == nil {
		return true
	}
	_, err := os.Lstat(*path)
	return errors.Is(err, os.ErrNotExist)
}

// fromSupersededEvent reads what the recorded event of a superseded
// original's retirement says: what the deletion accepted, and by whom. Its
// sidecars stay where they are, and nothing else of it is needed.
func (r *Retirer) fromSupersededEvent(rt *retirement, dir string) error {
	b, err := os.ReadFile(filepath.Join(dir, EventFile))
	if err != nil {
		return err
	}
	ev, err := DecodeDoc(b)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, EventFile), err)
	}
	acc, _ := ev.Get("accepted")
	rt.accepted = stringsOf(acc)
	by, _ := ev.Get("by")
	rt.by, _ = by.(string)
	return nil
}

// supersededCalledOff says why the retirement of the superseded original of
// rt, whose event is not recorded, is called off: the title holds its
// originals, its pipeline reads an original again, or its master needs it or
// changed; "" when it goes on.
func (r *Retirer) supersededCalledOff(ctx context.Context, rt *retirement) (string, error) {
	var held, busy bool
	err := r.pool.QueryRow(ctx, `SELECT COALESCE((SELECT retirehold FROM com_nalet_katalog_items WHERE id = $1), false),
		NOT `+originalStepsOver("$1", "$2"), rt.src.ItemID, processing.OriginalSteps).Scan(&held, &busy)
	switch {
	case err != nil:
		return "", err
	case held:
		return "the title holds its originals (retirehold): the superseded original is kept", nil
	case busy:
		return "the title's pipeline reads an original again: the retirement waits for it", nil
	}
	master, err := r.masterOf(ctx, rt.src)
	switch {
	case err != nil:
		return "", err
	case master == nil:
		return "no complete version of the title made without it holds: the superseded original is kept", nil
	case rt.ver != nil && master.ID != rt.ver.ID:
		return "the title's master changed meanwhile, to version " + master.ID + ": it is looked at again", nil
	}
	return "", nil
}

// prepareSuperseded is steps 2 to 5 of the retirement of a superseded
// original (see the top of this file): held, claimed, checked, its event
// recorded. rt.src is nil when it was not claimed after all, or its
// retirement was called off.
func (r *Retirer) prepareSuperseded(ctx context.Context, p Paths, set Settings, now time.Time, rt *retirement) error {
	s := rt.src
	master, err := r.masterOf(ctx, s)
	if err != nil {
		return err
	}
	if master == nil {
		why := "no complete version of the title made without it holds: the superseded original is kept"
		if s.State == SourceRetiring {
			err := r.callOff(ctx, rt, why)
			rt.src = nil
			return err
		}
		rt.why, rt.src = why, nil
		return nil
	}
	rt.ver = master
	recorded := Recorded(rt.itemDir, s)
	if recorded {
		lacks, err := surroundLost(rt.itemDir, master.ID, s.ID)
		if err != nil {
			return r.fail(ctx, rt, fmt.Sprintf("the essences of the source and of version %s cannot be read: %v; nothing is deleted",
				master.ID, err))
		}
		if lacks {
			if lacks, err = SurroundExpected(ctx, r.pool); err != nil {
				return err
			}
		}
		if lacks {
			if err := r.holdSuperseded(ctx, rt); err != nil {
				return err
			}
			rt.src = nil
			return errHeld
		}
	}
	if s.State == SourceSuperseded {
		claimed, err := r.claimSuperseded(ctx, p, now, rt)
		if err == nil && !claimed {
			rt.why = "its retirement could not be claimed: it is no superseded original of a title whose master is complete " +
				"and made without it, not held, its pipeline idle, any more"
		}
		if err != nil || !claimed {
			rt.src = nil
			return err
		}
	} else if s.RetireEventID == nil || s.RetireEventAt == nil {
		return r.fail(ctx, rt, "its retirement was claimed without its event's id and moment; nothing is deleted")
	}
	s = rt.src
	if s.ArrivalPath == nil {
		return r.fail(ctx, rt, "the catalog does not say where the original lies; nothing is deleted")
	}
	path := *s.ArrivalPath
	if r.cfg.PackagesRoot != "" && Within(r.cfg.PackagesRoot, path) {
		return r.fail(ctx, rt, path+" is under the package store, where the retire job deletes nothing; nothing is deleted")
	}
	if _, err := r.originalRootOf(p, rt, path); err != nil {
		return r.fail(ctx, rt, err.Error()+"; nothing is deleted")
	}
	if why, err := r.fileOfATitle(ctx, path); err != nil {
		return err
	} else if why != "" {
		return r.fail(ctx, rt, path+" is "+why+"; nothing is deleted")
	}
	if err := isRecordedOriginal(path, s.SizeBytes, s.QH1); err != nil {
		return r.fail(ctx, rt, err.Error())
	}
	if recorded {
		if rt.record, err = VerifySource(SourceDir(rt.itemDir, s.ID)); err != nil {
			return r.fail(ctx, rt, fmt.Sprintf("the source's record does not verify: %v; the original is kept", err))
		}
	}
	level, err := r.verify(ctx, set, now, master)
	if err != nil {
		return r.fail(ctx, rt, fmt.Sprintf("the title's master, version %s, does not verify (%s): %v; the original is kept",
			master.ID, level, err))
	}
	if why, err := r.supersededCalledOff(ctx, rt); err != nil {
		return err
	} else if why != "" {
		err := r.callOff(ctx, rt, why)
		rt.src = nil
		return err
	}
	if recorded {
		vid, err := r.versionMadeOf(ctx, rt)
		if err != nil {
			return err
		}
		if vid != "" {
			rt.accepted = []string{}
			if _, err := os.Stat(VersionDir(rt.itemDir, vid)); err == nil {
				if rt.accepted, err = GateOf(rt.itemDir, vid, s.ID); err != nil {
					return r.fail(ctx, rt, fmt.Sprintf("the deletion gate cannot be read: %v; the original is kept", err))
				}
			}
			if err := r.claimedStill(ctx, rt); err != nil {
				return err
			}
			reason := supersededReason(rt.superseded, master.ID)
			if _, err := WriteEvent(rt.itemDir, Event{ID: *s.RetireEventID, At: *s.RetireEventAt, By: rt.auth.By,
				Kind: EventOriginalDeleted, VersionID: vid, SourceID: s.ID, Reason: &reason, Accepted: rt.accepted}); err != nil {
				return r.fail(ctx, rt, fmt.Sprintf("the event cannot be recorded: %v; the original is kept", err))
			}
		}
	}
	rt.by = rt.auth.By
	return nil
}

// versionMadeOf is the version of the title made of the superseded original
// of rt, which its event names: the one whose folder keeps it, else the
// latest made of it, one removed last; "" when none was.
func (r *Retirer) versionMadeOf(ctx context.Context, rt *retirement) (string, error) {
	s := rt.src
	if vid, ok := VersionFolderOf(rt.itemDir, deref(s.ArrivalPath)); ok && IsOriginalName(filepath.Base(deref(s.ArrivalPath))) {
		return vid, nil
	}
	var vid string
	err := r.pool.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_itemversions WHERE item_id = $1 AND $2 = ANY(sourceids)
		ORDER BY state = 'removed', createdat DESC, id DESC LIMIT 1`, s.ItemID, s.ID).Scan(&vid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return vid, err
}

// fileOfATitle says what of the catalog the file at path is still, "" when
// nothing is: a playback asset's file, of any title, or a live extra's.
func (r *Retirer) fileOfATitle(ctx context.Context, path string) (string, error) {
	var item string
	err := r.pool.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 ORDER BY item_id LIMIT 1`,
		path).Scan(&item)
	if err == nil {
		return "a file of item " + item + " still", nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var extra bool
	err = r.pool.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_itemextras') IS NOT NULL
		AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemextras WHERE sourcepath = $1 AND removedat IS NULL)`, path).Scan(&extra)
	if err != nil || !extra {
		return "", err
	}
	return "an extra's file still", nil
}

// claimSuperseded is tx A of a superseded original's retirement: under the
// title's lock, the source is retiring if it still qualifies — superseded,
// the title not holding its originals, its master rt.ver complete and not
// made of it, its steps that read an original over —, its event's id and
// moment and its trash noted. It stays superseded by the source that took
// its place.
func (r *Retirer) claimSuperseded(ctx context.Context, p Paths, now time.Time, rt *retirement) (bool, error) {
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
	claimed, err := sourceRow(ctx, tx, `UPDATE com_nalet_katalog_itemsources s SET state = 'retiring', retireeventid = $2,
			retireeventat = $3, trashpath = $4, error = NULL, modifiedat = now()
		WHERE s.id = $1 AND s.state = 'superseded' AND s.supersededby IS NOT NULL
		  AND NOT COALESCE((SELECT retirehold FROM com_nalet_katalog_items i WHERE i.id = s.item_id), true)
		  AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
			WHERE v.id = $5 AND v.item_id = s.item_id AND v.state = 'complete' AND NOT (s.id = ANY(v.sourceids)))
		  AND `+originalStepsOver("s.item_id", "$6")+`
		RETURNING `+sourceCols, s.ID, NewID(), at, p.TrashDir(at, s.ID), rt.ver.ID, processing.OriginalSteps)
	if err != nil || claimed == nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	rt.src = claimed
	return true, nil
}

// holdSuperseded keeps the superseded original of rt, whose title's master's
// package has no 5.1 of its surround: it stays superseded, its row saying
// why, and is looked at again an hour later (supersededDue) — once another
// version is complete, its master is that one.
func (r *Retirer) holdSuperseded(ctx context.Context, rt *retirement) error {
	s := rt.src
	why := SurroundHeld + " (version " + rt.ver.ID + ")"
	if _, err := r.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET error = $2, modifiedat = now()
		WHERE id = $1 AND state = 'superseded'`, s.ID, clip(why, 500)); err != nil {
		return err
	}
	r.say("source:"+s.ID, fmt.Sprintf("superseded original %s of item %s is kept: %s", s.ID, s.ItemID, why))
	return nil
}
