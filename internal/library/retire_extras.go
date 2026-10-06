package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// An extra's original is deleted once its folder is recorded (state ready,
// recordedat) for library.retire.delay and verifies, its title not held: the
// file still of the size and quick hash it was taken in with goes to the
// trash, .work/trash/<day>/extra-<extraId>/ (a trash grace of 0 unlinks it),
// and the extra keeps no original (sourcepath NULL, sourcedeletedat). Extras
// get no event: their folder never held the original, and extra.json's
// packagedFrom says what it was. A removed extra is left to its removal.
// What stops a deletion is said in the log, once.

// pendingExtra is an extra whose original is due.
type pendingExtra struct {
	id, itemID, path, dir string
	size                  *int64
	qh1                   *string
}

// extras deletes the originals of the extras that are due, at most n: it
// answers how many were deleted and how many failed.
func (r *Retirer) extras(ctx context.Context, p Paths, set Settings, now time.Time, n int) (done, failed int, err error) {
	rows, err := r.pool.Query(ctx, `SELECT x.id, x.item_id, x.sourcepath, COALESCE(x.recordpath, ''), x.sourcesize, x.sourceqh1
		FROM com_nalet_katalog_itemextras x
		WHERE x.state = 'ready' AND x.recordedat IS NOT NULL AND x.sourcepath IS NOT NULL AND x.removedat IS NULL
		  AND x.recordedat + make_interval(secs => $1::float8) <= $2::timestamptz
		  AND NOT COALESCE((SELECT i.retirehold FROM com_nalet_katalog_items i WHERE i.id = x.item_id), false)
		ORDER BY x.recordedat, x.id
		LIMIT $3`, set.RetireDelay.Seconds(), now, n)
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
	for _, x := range due {
		if ctx.Err() != nil {
			break
		}
		if err := r.retireExtra(ctx, p, set, now, x); err != nil {
			failed++
			r.say("extra:"+x.id, fmt.Sprintf("the original of extra %s is not deleted: %v", x.id, err))
			errs = append(errs, fmt.Errorf("extra %s: %w", x.id, err))
			continue
		}
		r.say("extra:"+x.id, "")
		done++
	}
	return done, failed, errors.Join(errs...)
}

// retireExtra deletes the original of the extra x: verified first, unless
// it is in the trash already (a pass before stopped short of noting it).
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
