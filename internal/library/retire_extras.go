package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// An extra's original is deleted only where the deletion gate lets it go
// (ExtraGate), as a title's is: once its folder is recorded (state ready,
// recordedat) for library.retire.delay and verifies, its title not held, the
// file still of the size and quick hash it was taken in with goes to the
// trash, .work/trash/<day>/extra-<extraId>/ (a trash grace of 0 unlinks it),
// and the extra keeps no original (sourcepath NULL, sourcedeletedat). An
// extra has no record of its original the gate can read (its folder holds
// its package and extra.json, which says what it was packaged from, never
// the original's essence and streams), so the gate keeps every one today,
// and the pass names them kept, by why; a rule that reads such a record
// later lets them go through the gate, in ExtraGate. Extras get no event:
// their folder never held the original. A removed extra is left to its
// removal. What stops a deletion is said in the log, once.

// ExtraNoRecord is why the deletion gate keeps an extra's original: there
// is no record of it the gate can read.
const ExtraNoRecord = "no record the deletion gate can read"

// ExtrasGated says the retire job's extras step goes through the deletion
// gate (ExtraGate): a client (the deploy tooling's guard) asks it of
// captionReads before it lets originals be deleted.
const ExtrasGated = true

// ExtraGate is the deletion gate of the original of the extra whose folder
// is dir: what keeps it (Unknown, or what it names beyond the spec), nothing
// when it may go. An extra's folder holds no record of its original (no
// essence, no streams), so the gate can't tell what deleting it gives up:
// it keeps every one, Unknown ExtraNoRecord. A rule that reads a record of
// an extra's original reads it here.
func ExtraGate(dir string) (Gate, error) {
	return Gate{Unknown: ExtraNoRecord}, nil
}

// extraKept is why the gate g keeps an extra's original, "" when it lets it
// go.
func extraKept(g Gate) string {
	switch {
	case g.Unknown != "":
		return g.Unknown
	case len(g.Beyond()) > 0:
		return SpecHeldOf(g)
	}
	return ""
}

// maxExtrasLooked is how many due extras a pass looks at at most: those the
// gate keeps take no turn of library.retire.rate, so that one kept never
// stands before one it lets go. The bound is taken before the gate, of the
// oldest due: where more than it are kept, a pass reaches none after them,
// and one the gate would let go waits until fewer before it are kept. While
// every extra is kept (ExtraNoRecord) that holds back nothing; a gate that
// lets extras go takes the kept ones out of the query (as the retire job's
// holds are) before it relies on the bound.
const maxExtrasLooked = 1000

// pendingExtra is an extra whose original is due.
type pendingExtra struct {
	id, itemID, path, dir string
	size                  *int64
	qh1                   *string
}

// extras deletes the originals of the extras that are due and the deletion
// gate lets go, at most n, and names those it keeps (Kept, KeptExtra): it
// answers how many were deleted and how many failed.
func (r *Retirer) extras(ctx context.Context, p Paths, set Settings, now time.Time, n int) (done, failed int, err error) {
	rows, err := r.pool.Query(ctx, `SELECT x.id, x.item_id, x.sourcepath, COALESCE(x.recordpath, ''), x.sourcesize, x.sourceqh1
		FROM com_nalet_katalog_itemextras x
		WHERE x.state = 'ready' AND x.recordedat IS NOT NULL AND x.sourcepath IS NOT NULL AND x.removedat IS NULL
		  AND x.recordedat + make_interval(secs => $1::float8) <= $2::timestamptz
		  AND NOT COALESCE((SELECT i.retirehold FROM com_nalet_katalog_items i WHERE i.id = x.item_id), false)
		ORDER BY x.recordedat, x.id
		LIMIT $3`, set.RetireDelay.Seconds(), now, maxExtrasLooked)
	if err != nil {
		return 0, 0, fmt.Errorf("the extras' originals due: %w", err)
	}
	var due []pendingExtra
	for rows.Next() {
		var x pendingExtra
		if err := rows.Scan(&x.id, &x.itemID, &x.path, &x.dir, &x.size, &x.qh1); err != nil {
			rows.Close()
			return 0, 0, err
		}
		due = append(due, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var errs []error
	kept := map[string]int{}
	for _, x := range due {
		if ctx.Err() != nil || done+failed >= n {
			break
		}
		// the deletion gate first: what keeps it, nothing of it touched
		gate := r.extraGate
		if gate == nil {
			gate = ExtraGate
		}
		g, err := gate(x.dir)
		if err == nil {
			if why := extraKept(g); why != "" {
				r.kept = append(r.kept, Kept{SourceID: x.id, ItemID: x.itemID, Why: why, For: KeptExtra})
				kept[why]++
				continue
			}
		}
		if err == nil {
			err = r.retireExtra(ctx, p, set, now, x)
		}
		if err != nil {
			failed++
			r.say("extra:"+x.id, fmt.Sprintf("the original of extra %s is not deleted: %v", x.id, err))
			errs = append(errs, fmt.Errorf("extra %s: %w", x.id, err))
			continue
		}
		r.say("extra:"+x.id, "")
		done++
	}
	for why, k := range kept {
		r.say("extras:"+why, fmt.Sprintf("%d extras' originals are kept: %s", k, why))
	}
	return done, failed, errors.Join(errs...)
}

// retireExtra deletes the original of the extra x: verified first, unless
// it is in the trash already (a pass before stopped short of noting it).
// beforeExtraMove is a test's seam: called right before an extra's original
// is checked against its record the second time and moved; nil in the
// service.
var beforeExtraMove func()

func (r *Retirer) retireExtra(ctx context.Context, p Paths, set Settings, now time.Time, x pendingExtra) error {
	root, err := r.rootOf(p, x.path)
	if err != nil {
		return err
	}
	_, statErr := os.Lstat(x.path)
	switch {
	case errors.Is(statErr, os.ErrNotExist):
		// Moved into the trash or unlinked by a pass that stopped short, or
		// gone by itself: what is left of it is its folder, which must hold.
		if _, err := VerifyExtra(x.dir, set.Verify == VerifyFull); err != nil {
			return fmt.Errorf("its original is gone, and its folder does not verify: %v", err)
		}
	case statErr != nil:
		return statErr
	default:
		if x.size == nil {
			return fmt.Errorf("its original %s was taken in without its size; nothing is deleted", x.path)
		}
		if err := isRecordedOriginal(x.path, *x.size, x.qh1); err != nil {
			return err
		}
		if x.dir == "" {
			return errors.New("the catalog does not say where its folder is; nothing is deleted")
		}
		if _, err := VerifyExtra(x.dir, set.Verify == VerifyFull); err != nil {
			return fmt.Errorf("its folder does not verify (%s): %v; the original is kept", set.Verify, err)
		}
		// its identity again, right before it is moved or unlinked: a file
		// put in its place while its folder was verified is not the original
		if beforeExtraMove != nil {
			beforeExtraMove()
		}
		if err := isRecordedOriginal(x.path, *x.size, x.qh1); err != nil {
			return err
		}
		if set.TrashGrace == 0 {
			if err := os.Remove(x.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s cannot be deleted: %v", x.path, err)
			}
		} else {
			trash := p.ExtraTrashDir(now, x.id)
			if err := MkdirAll(trash); err != nil {
				return fmt.Errorf("the trash %s cannot be made: %v", trash, err)
			}
			if err := os.Rename(x.path, filepath.Join(trash, filepath.Base(x.path))); err != nil {
				return fmt.Errorf("%s cannot be moved into the trash %s: %v", x.path, trash, err)
			}
		}
		pruneEmpty(filepath.Dir(x.path), root)
	}
	_, err = r.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemextras SET sourcepath = NULL, sourcedeletedat = $3, modifiedat = now()
		WHERE id = $1 AND sourcepath = $2`, x.id, x.path, now)
	return err
}
