package library

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Where a version stands (com_nalet_katalog_itemversions.state): the
// pipeline works on it (building), its folder holds the original it was
// taken in from and no package (taken, migration 044: the title plays from
// that original until a package is added to the folder), its package is
// recorded and plays (complete, one per item at most), a newer one took over
// (superseded), or it is gone from the item (removed).
const (
	VersionBuilding   = "building"
	VersionTaken      = "taken"
	VersionComplete   = "complete"
	VersionSuperseded = "superseded"
	VersionRemoved    = "removed"
)

// Version is a package run of a title: a row of com_nalet_katalog_itemversions
// (migration 040), its id the versionId, the name of its folder.
type Version struct {
	ID, ItemID       string
	SourceIDs        []string
	State            string
	PackageID        *string
	Dir              *string
	CompletedAt      *time.Time
	VerifiedAt       *time.Time
	VerifiedLevel    *string
	SupersededBy     *string
	SupersededAt     *time.Time
	SupersedeEventID *string
	RemovedAt        *time.Time
	RemoveEventID    *string
	CreatedAt        time.Time
}

const versionCols = `id, item_id, sourceids, state, packageid, dir, completedat, verifiedat, verifiedlevel,
	supersededby, supersededat, supersedeeventid, removedat, removeeventid, createdat`

func scanVersion(row pgx.Row, v *Version) error {
	return row.Scan(&v.ID, &v.ItemID, &v.SourceIDs, &v.State, &v.PackageID, &v.Dir, &v.CompletedAt, &v.VerifiedAt,
		&v.VerifiedLevel, &v.SupersededBy, &v.SupersededAt, &v.SupersedeEventID, &v.RemovedAt, &v.RemoveEventID, &v.CreatedAt)
}

func versionRow(ctx context.Context, q Querier, sql string, args ...any) (*Version, error) {
	var v Version
	err := scanVersion(q.QueryRow(ctx, sql, args...), &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// VersionsOf lists the item's versions, the newest first.
func VersionsOf(ctx context.Context, q Querier, itemID string) ([]*Version, error) {
	rows, err := q.Query(ctx, `SELECT `+versionCols+` FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 ORDER BY createdat DESC, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Version
	for rows.Next() {
		var v Version
		if err := scanVersion(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// VersionByID reads the version id; nil when there is none.
func VersionByID(ctx context.Context, q Querier, id string) (*Version, error) {
	return versionRow(ctx, q, `SELECT `+versionCols+` FROM com_nalet_katalog_itemversions WHERE id = $1`, id)
}

// Current is the item's complete version, the one that plays; nil when it
// has none.
func Current(ctx context.Context, q Querier, itemID string) (*Version, error) {
	return versionRow(ctx, q, `SELECT `+versionCols+` FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 AND state = 'complete'`, itemID)
}

// Building is the version the pipeline works on for the item; nil when it
// has none.
func Building(ctx context.Context, q Querier, itemID string) (*Version, error) {
	return versionRow(ctx, q, `SELECT `+versionCols+` FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 AND state = 'building'`, itemID)
}

// EnsureBuilding is the version the pipeline works on for the item, made of
// sourceIDs: the one there is, which keeps its id across retries until a
// package of it is recorded (it is made of sourceIDs from now on when it was
// of others: the title was given another file), or a new one.
func EnsureBuilding(ctx context.Context, q Querier, itemID string, sourceIDs []string) (*Version, error) {
	if sourceIDs == nil {
		sourceIDs = []string{}
	}
	for range 2 { // a second time when another caller made it meanwhile
		v, err := Building(ctx, q, itemID)
		if err != nil {
			return nil, err
		}
		if v != nil {
			if !slices.Equal(v.SourceIDs, sourceIDs) && len(sourceIDs) > 0 {
				if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET sourceids = $2, modifiedat = now()
					WHERE id = $1 AND state = 'building'`, v.ID, sourceIDs); err != nil {
					return nil, err
				}
				v.SourceIDs = sourceIDs
			}
			return v, nil
		}
		if v, err = versionRow(ctx, q, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state)
			VALUES (gen_random_uuid()::varchar, $1, $2, 'building')
			ON CONFLICT (item_id) WHERE state = 'building' DO NOTHING
			RETURNING `+versionCols, itemID, sourceIDs); err != nil || v != nil {
			return v, err
		}
	}
	return nil, errors.New("the version being built for item " + itemID + " could not be found nor made")
}
