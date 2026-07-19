package itemactions

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
)

// RemoveItem implements graph.Remover: delete an item from the catalog (a
// series cascades to its episodes) and optionally clean its files off disk.
//
// Order of operations is deliberate:
//  1. Collect file paths / package roots BEFORE the rows go (they are the only
//     record of where the files live).
//  2. Delete the catalog rows in ONE transaction — the catalog is the source of
//     truth; from here the item is gone even if a file removal later fails
//     (failures are reported in Errors for the operator to retry by hand).
//  3. Remove media files (only with deleteFiles) and packaged dirs (only with
//     deletePackages), every path validated to live UNDER its configured root —
//     a corrupted path row must never turn into an rm outside the library.
//  4. Emit stube.catalog.item.removed so live-refresh surfaces drop the item.
func (s *Service) RemoveItem(ctx context.Context, id string, deleteFiles, deletePackages bool) (graph.RemoveResult, error) {
	var res graph.RemoveResult

	var typ, title string
	err := s.st.Pool().QueryRow(ctx,
		`SELECT type, title FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&typ, &title)
	if err != nil {
		if isNoRows(err) {
			return res, fmt.Errorf("%w: %s", ErrUnknownItem, id)
		}
		return res, err
	}

	// A series takes its episodes with it.
	ids := []string{id}
	types := map[string]string{id: typ}
	if strings.EqualFold(typ, "series") {
		rows, err := s.st.Pool().Query(ctx,
			`SELECT id, type FROM com_nalet_katalog_items WHERE parent_id = $1`, id)
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var cid, ctyp string
			if err := rows.Scan(&cid, &ctyp); err != nil {
				rows.Close()
				return res, err
			}
			ids = append(ids, cid)
			types[cid] = ctyp
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
	}

	// 1. Collect the on-disk footprint before the rows disappear.
	var mediaFiles []string
	if deleteFiles {
		rows, err := s.st.Pool().Query(ctx, `
			SELECT DISTINCT path FROM com_nalet_katalog_playbackassets
			WHERE item_id = ANY($1) AND path IS NOT NULL`, ids)
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return res, err
			}
			// Only paths inside the media root — packaged manifests live under
			// the packages root and are covered by the package-dir removal.
			if underRoot(s.cfg.NFSRoot, p) {
				mediaFiles = append(mediaFiles, p)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
	}
	var pkgRoots []string
	if deletePackages {
		seen := map[string]bool{}
		add := func(root string) {
			if root != "" && underRoot(s.cfg.PackagesRoot, root) && !seen[root] {
				seen[root] = true
				pkgRoots = append(pkgRoots, root)
			}
		}
		// The packaged asset's manifest path is AUTHORITATIVE for where the
		// package lives — an item whose type changed after packaging (e.g. a
		// movie reclassified to episode) keeps its package under the OLD
		// category, which a type-derived path would miss.
		rows, err := s.st.Pool().Query(ctx, `
			SELECT path FROM com_nalet_katalog_playbackassets
			WHERE item_id = ANY($1) AND kind = 'packaged' AND path IS NOT NULL`, ids)
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return res, err
			}
			add(filepath.Dir(p)) // …/<itemID>/manifest.json -> the package root
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		// Type-derived fallback covers partially-packaged leftovers with no
		// packaged row yet.
		for _, iid := range ids {
			add(packageRoot(s.cfg.PackagesRoot, types[iid], iid))
		}
	}

	// 2. Catalog rows go first, atomically.
	n, err := s.st.DeleteItems(ctx, ids)
	if err != nil {
		return res, err
	}
	res.Deleted = n > 0
	res.ItemsRemoved = int32(n)

	// 3. Files — failures are reported, never fatal (the catalog delete stands).
	for _, f := range mediaFiles {
		if err := os.Remove(f); err != nil {
			if os.IsNotExist(err) {
				continue // already gone — fine
			}
			res.Errors = append(res.Errors, "media: "+err.Error())
			continue
		}
		res.FilesRemoved++
		pruneEmptyDirs(filepath.Dir(f), s.cfg.NFSRoot)
	}
	for _, root := range pkgRoots {
		if _, err := os.Stat(root); err != nil {
			continue // never packaged / already gone
		}
		if err := os.RemoveAll(root); err != nil {
			res.Errors = append(res.Errors, "package: "+err.Error())
			continue
		}
		res.PackagesRemoved++
		pruneEmptyDirs(filepath.Dir(root), s.cfg.PackagesRoot)
	}

	// 4. Announce the removal (one event for the whole cascade).
	if res.Deleted {
		ev := events.NewItemEvent(id)
		ev.Type = typ
		ev.Status = "removed"
		ev.Source = "katalog-manager"
		s.events.EmitItem(ctx, events.TopicRemoved, ev)
		log.Printf("removed item %s (%q, %s): items=%d files=%d packages=%d errors=%d",
			id, title, typ, res.ItemsRemoved, res.FilesRemoved, res.PackagesRemoved, len(res.Errors))
	}
	return res, nil
}

// underRoot reports whether path (cleaned) lies strictly inside root — the
// guard that keeps a corrupted DB path from deleting anything outside the
// library mounts.
func underRoot(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || root == "/" || path == "" {
		return false
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// pruneEmptyDirs removes now-empty parent directories up to (excluding) stop —
// e.g. a series folder after its last episode file is gone. os.Remove fails on
// non-empty dirs, which ends the walk naturally.
func pruneEmptyDirs(dir, stop string) {
	stop = filepath.Clean(stop)
	for {
		dir = filepath.Clean(dir)
		if dir == stop || !underRoot(stop, dir) {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// packageRoot mirrors the packager's sharded layout (category by type, 2-char
// shard) — the same scheme rest.packageRootFor uses.
func packageRoot(packagesRoot, typ, itemID string) string {
	var category string
	switch strings.ToLower(typ) {
	case "movie":
		category = "movies"
	case "episode":
		category = "shows"
	case "track":
		category = "music"
	default:
		category = "items"
	}
	shard := "00"
	if len(itemID) >= 2 {
		shard = itemID[:2]
	}
	return filepath.Join(packagesRoot, category, shard, itemID)
}

// isNoRows is defined in service.go for pgx.ErrNoRows; keep a local alias so
// this file stands alone if that helper ever moves.
var _ = pgx.ErrNoRows
