package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// What became of an original (com_nalet_katalog_itemsources.state): it is
// there (present), its deletion is claimed (retiring), it was deleted after
// packaging (deleted), or it is no original of its item after all (removed).
const (
	SourcePresent  = "present"
	SourceRetiring = "retiring"
	SourceDeleted  = "deleted"
	SourceRemoved  = "removed"
)

// Source is an original a title was given: a row of
// com_nalet_katalog_itemsources (migration 040), its id the sourceId.
type Source struct {
	ID, ItemID, Filename string
	ArrivalPath          *string // absolute, while the original exists
	LibraryPath          *string // relative to ARRIVALS_ROOT
	SizeBytes            int64
	QH1                  *string
	State                string
	RecordedAt           *time.Time // sources/<id>/ written
	RecordDir            *string
	Sidecars             []byte // [{subtitleAssetId, rendition, path}]
	RetireEventID        *string
	RetireEventAt        *time.Time
	TrashPath            *string
	DeletedAt            *time.Time
	DeletedBy            *string
	Lost                 []byte
	Error                *string
}

const sourceCols = `id, item_id, filename, arrivalpath, librarypath, sizebytes, qh1, state, recordedat, recorddir,
	sidecars, retireeventid, retireeventat, trashpath, deletedat, deletedby, lost, error`

func scanSource(row pgx.Row, s *Source) error {
	return row.Scan(&s.ID, &s.ItemID, &s.Filename, &s.ArrivalPath, &s.LibraryPath, &s.SizeBytes, &s.QH1, &s.State,
		&s.RecordedAt, &s.RecordDir, &s.Sidecars, &s.RetireEventID, &s.RetireEventAt, &s.TrashPath, &s.DeletedAt,
		&s.DeletedBy, &s.Lost, &s.Error)
}

func querySources(ctx context.Context, q Querier, sql string, args ...any) ([]*Source, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		var s Source
		if err := scanSource(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

func sourceRow(ctx context.Context, q Querier, sql string, args ...any) (*Source, error) {
	var s Source
	err := scanSource(q.QueryRow(ctx, sql, args...), &s)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SourceByID reads the source id; nil when there is none.
func SourceByID(ctx context.Context, q Querier, id string) (*Source, error) {
	return sourceRow(ctx, q, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources WHERE id = $1`, id)
}

// SourceAt reads the source whose original lies at path; nil when none does.
func SourceAt(ctx context.Context, q Querier, path string) (*Source, error) {
	return sourceRow(ctx, q, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources WHERE arrivalpath = $1`, path)
}

// SourcesByFixity lists the sources whose original had size and quick hash
// qh1, whatever became of it: present first, then by when it was deleted.
func SourcesByFixity(ctx context.Context, q Querier, size int64, qh1 string) ([]*Source, error) {
	return querySources(ctx, q, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources
		WHERE sizebytes = $1 AND qh1 = $2 ORDER BY state <> 'present', deletedat NULLS FIRST, id`, size, qh1)
}

// SourcesOf lists the item's sources, the oldest first.
func SourcesOf(ctx context.Context, q Querier, itemID string) ([]*Source, error) {
	return querySources(ctx, q, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources
		WHERE item_id = $1 ORDER BY createdat, id`, itemID)
}

// LibraryPathOf is path as the record names an original's place: relative to
// the arrivals' root, its folders joined by "/"; "" when it lies outside.
func (p Paths) LibraryPathOf(path string) string {
	if !Within(p.Arrivals, path) {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(p.Arrivals), filepath.Clean(path))
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

// AddSource takes the original at path in as a source of the item: a new
// sourceId, present, with its name, its place (absolute, and as the record
// names it), its size and its quick hash.
func (p Paths) AddSource(ctx context.Context, q Querier, itemID, path string, size int64, qh1 string) (*Source, error) {
	var lib *string
	if l := p.LibraryPathOf(path); l != "" {
		lib = &l
	}
	return sourceRow(ctx, q, `INSERT INTO com_nalet_katalog_itemsources
		(id, item_id, filename, arrivalpath, librarypath, sizebytes, qh1, state)
		VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, 'present')
		RETURNING `+sourceCols, itemID, filepath.Base(path), path, lib, size, qh1)
}

// MoveSource notes that the original of the source s lies at path now: a
// moved arrival. While its record is not written, its name and its place as
// the record will name them follow.
func (p Paths) MoveSource(ctx context.Context, q Querier, s *Source, path string) error {
	var lib *string
	if l := p.LibraryPathOf(path); l != "" {
		lib = &l
	}
	_, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET arrivalpath = $2,
			filename = CASE WHEN recordedat IS NULL THEN $3 ELSE filename END,
			librarypath = CASE WHEN recordedat IS NULL THEN $4 ELSE librarypath END, modifiedat = now()
		WHERE id = $1`, s.ID, path, filepath.Base(path), lib)
	return err
}

// Refix notes the size and quick hash of the original of a source whose
// record is not written yet: the file at its place changed before it was
// recorded.
func Refix(ctx context.Context, q Querier, sourceID string, size int64, qh1 string) error {
	_, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET sizebytes = $2, qh1 = $3, modifiedat = now()
		WHERE id = $1 AND recordedat IS NULL AND (sizebytes <> $2 OR qh1 IS DISTINCT FROM $3)`, sourceID, size, qh1)
	return err
}

// primaryAsset is a title's own file: its primary playback asset that is no
// package, and the source it is of.
type primaryAsset struct {
	ID, Path string
	SourceID *string
}

// primaryOf reads the item's primary asset, nil when it has none (a series,
// or a title whose original was retired).
func primaryOf(ctx context.Context, q Querier, itemID string) (*primaryAsset, error) {
	var a primaryAsset
	err := q.QueryRow(ctx, `SELECT id, path, sourceid FROM com_nalet_katalog_playbackassets
		WHERE item_id = $1 AND isprimary = true AND COALESCE(kind, 'primary') = 'primary' ORDER BY id LIMIT 1`, itemID).
		Scan(&a.ID, &a.Path, &a.SourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// PrimarySource is the source the item's primary asset names, as it is; nil
// when the asset names none, or the item has no primary asset.
func PrimarySource(ctx context.Context, q Querier, itemID string) (*Source, error) {
	a, err := primaryOf(ctx, q, itemID)
	if err != nil || a == nil || a.SourceID == nil {
		return nil, err
	}
	return SourceByID(ctx, q, *a.SourceID)
}

// EnsureSource is the source behind the item's primary asset: the one it
// names, or the one whose original lies at its path; a row from before
// migration 040, or one the scanner took in before the v2 layout, gets its
// source now, its size and quick hash read from the file, and names it. nil
// when the item has no primary asset. A source no longer present behind it
// (its original retired) is answered as it is.
func (p Paths) EnsureSource(ctx context.Context, q Querier, itemID string) (*Source, error) {
	a, err := primaryOf(ctx, q, itemID)
	if err != nil || a == nil {
		return nil, err
	}
	if a.SourceID != nil {
		s, err := SourceByID(ctx, q, *a.SourceID)
		if err != nil || s != nil {
			return s, err
		}
	}
	s, err := SourceAt(ctx, q, a.Path)
	if err != nil {
		return nil, err
	}
	if s == nil {
		size, qh1, err := QH1(a.Path)
		if err != nil {
			return nil, fmt.Errorf("the original of item %s at %s cannot be read: %w", itemID, a.Path, err)
		}
		if s, err = p.AddSource(ctx, q, itemID, a.Path, size, qh1); err != nil {
			return nil, err
		}
	}
	if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $2 WHERE id = $1`, a.ID, s.ID); err != nil {
		return nil, err
	}
	return s, nil
}

// isUndefinedTable reports whether err is Postgres saying a table does not
// exist (42P01): a catalog older than migration 040.
func isUndefinedTable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "42P01"
}

// statOK reports whether a file is at path.
func statOK(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
