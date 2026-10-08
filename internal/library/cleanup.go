package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// What the retire job cleans up besides originals (platform-library/1, 1.5
// and 4.7):
//   - a superseded version once library.superseded.grace is over: its
//     removal is noted first (removedat, the event's id), then the
//     version-removed event is recorded, its folder deleted, and it is
//     removed. A version whose folder still holds an original (one the
//     catalog has there, present or being retired, or any file beside its
//     record and its package's folders) is never removed: it waits until
//     its original is retired;
//   - a legacy package folder (PACKAGES_ROOT/<category>/<aa>/<itemId>) of an
//     item whose v2 version is complete for the same grace: moved to
//     .work/legacy/<day>/packages/, never in the record, so no event;
//   - the trash's days (.work/trash/<YYYYMMDD>) once library.trash.grace is
//     over after their day, and the legacy folder's days once
//     library.superseded.grace is.

// RemovedBy is who removes a superseded version, as its event says.
const RemovedBy = "katalog-manager (library.superseded.grace)"

// legacyBatch is how many complete versions a pass looks at for a legacy
// package folder.
const legacyBatch = 500

// errHoldsOriginal says a superseded version is kept: its folder holds an
// original.
var errHoldsOriginal = errors.New("its folder holds an original, which is retired first")

// removeVersions removes the superseded versions whose grace is over, and
// goes on with those whose removal began, at most n: it answers how many
// were removed and how many failed. A version whose folder holds an
// original the catalog has there waits, unread, and so does one whose
// folder holds any other file that may be an original (MayBeOriginals),
// said in the log, while others are removed past it.
func (r *Retirer) removeVersions(ctx context.Context, p Paths, set Settings, now time.Time, n int) (done, failed int, err error) {
	rows, err := r.pool.Query(ctx, `SELECT `+versionCols+` FROM com_nalet_katalog_itemversions v
		WHERE state = 'superseded'
		  AND (removedat IS NOT NULL OR supersededat + make_interval(secs => $1::float8) <= $2::timestamptz)
		  AND NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources s
			WHERE s.item_id = v.item_id AND s.state IN ('present', 'retiring') AND v.dir IS NOT NULL
			  AND left(s.arrivalpath, length(v.dir) + 1) = v.dir || '/')
		ORDER BY removedat NULLS LAST, supersededat, id
		LIMIT $3`, set.SupersededGrace.Seconds(), now, 4*n)
	if err != nil {
		return 0, 0, fmt.Errorf("the superseded versions due: %w", err)
	}
	var due []*Version
	for rows.Next() {
		var v Version
		if err := scanVersion(rows, &v); err != nil {
			rows.Close()
			return 0, 0, err
		}
		due = append(due, &v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, v := range due {
		if ctx.Err() != nil || done+failed >= n {
			break
		}
		if err := r.removeVersion(ctx, p, now, v); errors.Is(err, errHoldsOriginal) {
			r.say("version:"+v.ID, fmt.Sprintf("superseded version %s of item %s is kept: %v", v.ID, v.ItemID, err))
			continue
		} else if err != nil {
			failed++
			r.say("version:"+v.ID, fmt.Sprintf("superseded version %s of item %s is not removed: %v", v.ID, v.ItemID, err))
			errs = append(errs, fmt.Errorf("version %s: %w", v.ID, err))
			continue
		}
		r.say("version:"+v.ID, "")
		done++
	}
	return done, failed, errors.Join(errs...)
}

// removeVersion removes the superseded version v: its removal noted (under
// the item's lock, as it still is superseded), the version-removed event
// recorded, its folder deleted, and the version removed.
func (r *Retirer) removeVersion(ctx context.Context, p Paths, now time.Time, v *Version) error {
	pl, err := PlaceOf(ctx, r.pool, v.ItemID)
	if errors.Is(err, ErrNoItem) {
		return nil
	}
	if err != nil {
		return err
	}
	itemDir := p.ItemDir(pl)
	dir := deref(v.Dir)
	if dir == "" {
		dir = VersionDir(itemDir, v.ID)
	}
	if filepath.Base(dir) != v.ID || filepath.Base(filepath.Dir(dir)) != "versions" || !Within(p.Root, dir) || Within(p.Work, dir) {
		return fmt.Errorf("its folder %s is no version folder of the library: nothing is deleted", dir)
	}
	if names, err := MayBeOriginals(dir); err != nil {
		return fmt.Errorf("its folder %s cannot be read: %v", dir, err)
	} else if len(names) > 0 {
		return fmt.Errorf("%w (%s)", errHoldsOriginal, strings.Join(names, ", "))
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, v.ItemID); err != nil {
		return err
	}
	var at time.Time
	var id string
	err = tx.QueryRow(ctx, `UPDATE com_nalet_katalog_itemversions SET removedat = COALESCE(removedat, $2),
			removeeventid = COALESCE(removeeventid, $3), modifiedat = now()
		WHERE id = $1 AND state = 'superseded'
		RETURNING removedat, removeeventid`, v.ID, now.Truncate(time.Second), NewID()).Scan(&at, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not superseded any more
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	reason := "superseded by version " + deref(v.SupersededBy) + ", and library.superseded.grace is over"
	if _, err := WriteEvent(itemDir, Event{ID: id, At: at, By: RemovedBy, Kind: EventVersionRemoved, VersionID: v.ID,
		PackageID: deref(v.PackageID), Reason: &reason}); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("its folder %s cannot be deleted: %v", dir, err)
	}
	_ = os.Remove(filepath.Dir(dir)) // versions/, when it was the last
	_, err = r.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET state = 'removed', modifiedat = now()
		WHERE id = $1 AND state = 'superseded'`, v.ID)
	return err
}

// LegacyPackageDir is the folder of the item's package in the package store
// before the library: PACKAGES_ROOT/<movies|shows|music|items>/<aa>/<id>, by
// the item's type, as the packager's legacy layout places it.
func LegacyPackageDir(packagesRoot, typ, id string) string {
	category := "items"
	switch strings.ToLower(typ) {
	case "movie":
		category = "movies"
	case "episode":
		category = "shows"
	case "track":
		category = "music"
	}
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(packagesRoot, category, shard, id)
}

// legacyPackages moves aside the legacy package folders of the items whose
// v2 version has been complete for library.superseded.grace: each version is
// looked at once in a process, a batch a pass. It answers how many folders
// it moved.
func (r *Retirer) legacyPackages(ctx context.Context, p Paths, set Settings, now time.Time) (int, error) {
	root := filepath.Clean(r.cfg.PackagesRoot)
	if r.cfg.PackagesRoot == "" || !filepath.IsAbs(root) {
		return 0, nil
	}
	rows, err := r.pool.Query(ctx, `SELECT v.id, v.item_id, i.type, v.completedat
		FROM com_nalet_katalog_itemversions v JOIN com_nalet_katalog_items i ON i.id = v.item_id
		WHERE v.state = 'complete' AND v.completedat + make_interval(secs => $1::float8) <= $2::timestamptz
		  AND (v.completedat, v.id) > ($3::timestamptz, $4::text)
		ORDER BY v.completedat, v.id
		LIMIT $5`, set.SupersededGrace.Seconds(), now, r.legacyAt, r.legacyID, legacyBatch)
	if err != nil {
		return 0, fmt.Errorf("the versions to look for a legacy package folder of: %w", err)
	}
	type seen struct {
		id, item, typ string
		at            time.Time
	}
	var batch []seen
	for rows.Next() {
		var s seen
		if err := rows.Scan(&s.id, &s.item, &s.typ, &s.at); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	moved := 0
	var errs []error
	for _, s := range batch {
		dir := LegacyPackageDir(root, s.typ, s.item)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			rel, _ := filepath.Rel(root, dir)
			to := filepath.Join(p.LegacyDay(now), "packages", rel)
			if _, err := os.Stat(to); err == nil {
				to += "-" + NewID()[:8]
			}
			if err := MkdirAll(filepath.Dir(to)); err != nil {
				errs = append(errs, err)
				break
			}
			if err := os.Rename(dir, to); err != nil {
				if errors.Is(err, syscall.EXDEV) {
					r.say("legacy", fmt.Sprintf("the legacy package folders are on another filesystem than the legacy folder %s: "+
						"they are left where they are", p.LegacyDir()))
					return moved, nil
				}
				errs = append(errs, fmt.Errorf("the legacy package folder %s cannot be moved aside: %w", dir, err))
				break
			}
			pruneEmpty(filepath.Dir(dir), root)
			moved++
		}
		r.legacyAt, r.legacyID = s.at, s.id
	}
	return moved, errors.Join(errs...)
}

// purge deletes the days of the trash whose library.trash.grace is over
// after their day, and of the legacy folder whose
// library.superseded.grace is: everything in a day folder went there that
// day. It answers how many it deleted.
func (r *Retirer) purge(p Paths, set Settings, now time.Time) (int, error) {
	purged := 0
	var errs []error
	for _, w := range []struct {
		dir   string
		grace time.Duration
	}{{filepath.Join(p.Work, WorkTrash), set.TrashGrace}, {p.LegacyDir(), set.SupersededGrace}} {
		entries, err := os.ReadDir(w.dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range entries {
			day, err := time.Parse("20060102", e.Name())
			if err != nil || !e.IsDir() {
				continue
			}
			if day.Add(24 * time.Hour).Add(w.grace).After(now) {
				continue
			}
			if err := os.RemoveAll(filepath.Join(w.dir, e.Name())); err != nil {
				errs = append(errs, fmt.Errorf("%s cannot be deleted: %w", filepath.Join(w.dir, e.Name()), err))
				continue
			}
			purged++
		}
	}
	return purged, errors.Join(errs...)
}
