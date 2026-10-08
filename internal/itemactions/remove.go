package itemactions

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// RemoveItem implements graph.Remover: delete an item from the catalog (a
// series cascades to its episodes) and optionally clean its files off disk.
// reason, which may be empty, is kept in the deletion log.
//
// Order of operations is deliberate:
//  1. Collect file paths / package roots BEFORE the rows go (they are the only
//     record of where the files live).
//  2. Delete the catalog rows in ONE transaction, which also records every
//     item it removes in the deletion log, attributed to the caller — the
//     catalog is the source of truth; from here the item is gone even if a file
//     removal later fails (failures are reported in Errors for the operator to
//     retry by hand), and the log tells a later verification that a folder left
//     behind belongs to an item the catalog deleted, not one it lost.
//  3. Remove media files (only with deleteFiles) and packaged dirs (only with
//     deletePackages), every path validated to live UNDER its configured root —
//     a corrupted path row must never turn into an rm outside the library.
//     The items' extras go with them: their files under the media root or
//     EXTRAS_ROOT (never a library record's, which is written once), their
//     packages in packages/extras/ and the transcoder's handoffs left in the
//     inbox. With the library's v2 layout the files are the originals the
//     items still have, where they arrived (presentArrivals) or in their
//     versions' folders (recordedOriginals), and the packages are the
//     items' folders in the record and their entries in the work folder
//     (libraryFolders), and the package store's folders of before. An
//     original in its version's folder is a file of its title, never a
//     package's: without deleteFiles it is put back where it arrived before
//     its folder goes, and a folder whose original cannot be put back stays
//     (so does one whose original's source keeps no place it arrived at, as
//     a source keeps none once its record is written).
//  4. Emit stube.catalog.item.removed so live-refresh surfaces drop the item.
//
// One file of several episodes is its holder's (migration 045): an episode
// it covers has no file of its own, so removing one touches no file, its
// holder's file and package staying as they are (the holder's file covers
// one episode fewer); removing the holder keeps the episodes its file
// covered besides it, unlinked, with no file now, and the result says which
// (Unlinked).
func (s *Service) RemoveItem(ctx context.Context, id string, deleteFiles, deletePackages bool, reason string) (graph.RemoveResult, error) {
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
	extras, err := s.st.ExtrasOfItems(ctx, ids)
	if err != nil {
		return res, err
	}
	var mediaFiles []string
	// stops are where the folders a removed file leaves empty stop being
	// pruned: the root it lies in.
	stops := map[string]string{}
	v2 := s.v2(ctx)
	var recorded []recordedOriginal
	if v2 {
		if recorded, err = s.recordedOriginals(ctx, ids); err != nil {
			return res, err
		}
	}
	if deleteFiles && v2 {
		if mediaFiles, err = s.presentArrivals(ctx, ids, extras, stops); err != nil {
			return res, err
		}
		for _, o := range recorded {
			mediaFiles = append(mediaFiles, o.path)
			stops[o.path] = filepath.Dir(o.path) // nothing is pruned in the record
		}
	} else if deleteFiles {
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
				stops[p] = s.cfg.NFSRoot
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		for _, x := range extras {
			if x.SourcePath == nil {
				continue
			}
			for _, root := range []string{s.cfg.NFSRoot, s.cfg.Roots(false).Extras} {
				if p := *x.SourcePath; underRoot(root, p) && stops[p] == "" {
					mediaFiles = append(mediaFiles, p)
					stops[p] = root
				}
			}
		}
	}
	var pkgRoots []string
	// pkgStops are where the folders a removed package leaves empty stop
	// being pruned: the package store, or the library's.
	pkgStops := map[string]string{}
	if deletePackages {
		seen := map[string]bool{}
		add := func(root string) {
			if root != "" && underRoot(s.cfg.PackagesRoot, root) && !seen[root] {
				seen[root] = true
				pkgRoots = append(pkgRoots, root)
				pkgStops[root] = s.cfg.PackagesRoot
			}
		}
		if v2 {
			folders, err := s.libraryFolders(ctx, ids, extras)
			if err != nil {
				return res, err
			}
			for dir, stop := range folders {
				if !seen[dir] {
					seen[dir] = true
					pkgRoots = append(pkgRoots, dir)
					pkgStops[dir] = stop
				}
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
		// The extras' packages, wherever their rows say they are and where
		// the packager puts them, and their handoffs in the inbox.
		for _, x := range extras {
			if x.PackagePath != nil {
				add(filepath.Clean(*x.PackagePath))
			}
			add(extraPackageRoot(s.cfg.PackagesRoot, x.ID))
			add(filepath.Join(s.cfg.PackagesRoot, "_inbox", "extra-"+x.ID))
		}
	}

	// 2. Catalog rows go first, atomically, and are recorded as they go. The
	// episodes the file of an item removed covered besides it stay, unlinked,
	// with no file now.
	n, unlinked, err := s.st.DeleteItemsAndUnlink(ctx, ids, store.Deletion{By: auth.Actor(ctx, "katalog-manager"), Reason: reason})
	if err != nil {
		return res, err
	}
	res.Deleted = n > 0
	res.ItemsRemoved = int32(n)
	res.Unlinked = unlinked
	if len(unlinked) > 0 {
		log.Printf("removed item %s: the episodes its file covered besides it stay, unlinked, with no file now: %s", id,
			strings.Join(unlinked, ", "))
	}

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
		pruneEmptyDirs(filepath.Dir(f), stops[f])
	}
	if v2 {
		sort.Strings(pkgRoots) // a series' folder before its episodes'
	}
	for _, root := range pkgRoots {
		if _, err := os.Stat(root); err != nil {
			continue // never packaged / already gone
		}
		if why := putBack(root, recorded, deleteFiles); why != "" {
			res.Errors = append(res.Errors, "package: "+why)
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			res.Errors = append(res.Errors, "package: "+err.Error())
			continue
		}
		res.PackagesRemoved++
		pruneEmptyDirs(filepath.Dir(root), pkgStops[root])
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

// presentArrivals are the files a removal with the v2 layout deletes: the
// originals the items still have (present sources, and primary assets of no
// source) and their extras' files, where they arrived (ARRIVALS_ROOT,
// EXTRAS_ROOT) or under the media root before the library (NFS_ROOT). Never
// a file in the record, nor an original being retired: the retire job has
// it. stops gets the root each lies in.
func (s *Service) presentArrivals(ctx context.Context, ids []string, extras []*model.Extra, stops map[string]string) ([]string, error) {
	p := library.PathsOf(s.cfg)
	roots := []string{p.Arrivals, p.Extras, s.cfg.NFSRoot, s.cfg.Roots(false).Extras}
	rows, err := s.st.Pool().Query(ctx, `
		SELECT arrivalpath FROM com_nalet_katalog_itemsources
		WHERE item_id = ANY($1) AND state = 'present' AND arrivalpath IS NOT NULL
		UNION
		SELECT path FROM com_nalet_katalog_playbackassets
		WHERE item_id = ANY($1) AND isprimary = true AND sourceid IS NULL AND path IS NOT NULL
		ORDER BY 1`, ids)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return nil, err
		}
		paths = append(paths, path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, x := range extras {
		if x.SourcePath != nil {
			paths = append(paths, *x.SourcePath)
		}
	}
	var out []string
	for _, path := range paths {
		for _, root := range roots {
			if underRoot(root, path) && stops[path] == "" && !inRecord(p, path) {
				out = append(out, path)
				stops[path] = root
				break
			}
		}
	}
	return out, nil
}

// recordedOriginal is an original a title has in its version's folder, and
// where it arrived, which the library keeps of it.
type recordedOriginal struct{ path, arrival string }

// recordedOriginals are the originals the items still have in their
// versions' folders (present sources, and those being retired), each with
// where it arrived ("" when the catalog does not say): those the catalog has
// there, and those a run renamed into a version's folder whose handover was
// not taken (library.UnrecordedOriginal), which go back where the catalog
// has them.
func (s *Service) recordedOriginals(ctx context.Context, ids []string) ([]recordedOriginal, error) {
	p := library.PathsOf(s.cfg)
	pool := s.st.Pool()
	var out []recordedOriginal
	for _, id := range ids {
		sources, err := library.SourcesOf(ctx, pool, id)
		if err != nil {
			return nil, err
		}
		for _, src := range sources {
			if (src.State != library.SourcePresent && src.State != library.SourceRetiring) || src.ArrivalPath == nil {
				continue
			}
			if inRecord(p, *src.ArrivalPath) {
				if library.IsOriginalName(filepath.Base(*src.ArrivalPath)) {
					out = append(out, recordedOriginal{path: filepath.Clean(*src.ArrivalPath), arrival: p.ArrivalOf(src)})
				}
				continue
			}
			if _, err := os.Lstat(*src.ArrivalPath); err == nil {
				continue // where the catalog has it
			}
			pl, err := library.PlaceOf(ctx, pool, id)
			if err != nil {
				continue
			}
			found, err := library.UnrecordedOriginal(ctx, pool, p.ItemDir(pl), src)
			if err != nil {
				return nil, err
			}
			if found != "" {
				out = append(out, recordedOriginal{path: found, arrival: filepath.Clean(*src.ArrivalPath)})
			}
		}
	}
	return out, nil
}

// putBack puts each original of recorded that lies under root, a folder a
// removal deletes, back where it arrived, unless the removal deletes the
// files: the title is removed, its file is kept, as a removal without
// deleteFiles keeps a title's files. It says why root stays, "" when it may
// go: an original whose place is unknown or taken, or that cannot be moved,
// keeps its folder, and so does any other file in a version's folder under
// root that may be an original (library.MayBeOriginals).
func putBack(root string, recorded []recordedOriginal, deleteFiles bool) string {
	if deleteFiles {
		return ""
	}
	if why := putBackRecorded(root, recorded); why != "" {
		return why
	}
	var kept string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || kept != "" {
			return nil
		}
		switch d.Name() {
		case "hls", "subs", "trickplay", "trailers", "metadata":
			return fs.SkipDir
		}
		if filepath.Base(filepath.Dir(path)) != "versions" || !library.ValidID(d.Name()) {
			return nil
		}
		if names, err := library.MayBeOriginals(path); err != nil || len(names) > 0 {
			kept = fmt.Sprintf("%s holds %s, which may be an original the catalog does not know: %s stays", path,
				strings.Join(names, ", "), root)
		}
		return fs.SkipDir
	})
	return kept
}

// putBackRecorded puts each original of recorded under root back where it
// arrived (see putBack).
func putBackRecorded(root string, recorded []recordedOriginal) string {
	for _, o := range recorded {
		if !underRoot(root, o.path) {
			continue
		}
		if _, err := os.Lstat(o.path); err != nil {
			continue // deleted with the files, or gone
		}
		switch _, err := os.Lstat(o.arrival); {
		case o.arrival == "":
			return fmt.Sprintf("the original %s is kept, the catalog naming no place it arrived at: %s stays", o.path, root)
		case err == nil:
			return fmt.Sprintf("the original %s is kept, a file lying where it arrived (%s): %s stays", o.path, o.arrival, root)
		}
		if err := library.MkdirAll(filepath.Dir(o.arrival)); err != nil {
			return fmt.Sprintf("the original %s is kept (%v): %s stays", o.path, err, root)
		}
		if err := os.Rename(o.path, o.arrival); err != nil {
			return fmt.Sprintf("the original %s is kept (%v): %s stays", o.path, err, root)
		}
		log.Printf("removed: the original %s is kept, put back where it arrived, %s", o.path, o.arrival)
	}
	return ""
}

// inRecord reports whether path lies in the library's record.
func inRecord(p library.Paths, path string) bool {
	for _, d := range []string{library.MoviesDir, library.SeriesDir, library.PeopleDir} {
		if underRoot(filepath.Join(p.Root, d), path) {
			return true
		}
	}
	return false
}

// libraryFolders are what a removal with the v2 layout deletes of the items'
// packages, each with where pruning the folders it leaves empty stops: each
// item's folder in the record (a series' holds its episodes'), and the
// items' and their extras' entries in the work folder (the transcoder's
// handoffs in .work/inbox, the packager's builds in .work/staging). The
// legacy package store's folders are the caller's.
func (s *Service) libraryFolders(ctx context.Context, ids []string, extras []*model.Extra) (map[string]string, error) {
	p := library.PathsOf(s.cfg)
	out := map[string]string{}
	pool := s.st.Pool()
	for _, id := range ids {
		pl, err := library.PlaceOf(ctx, pool, id)
		var unplaced *library.Unplaced
		if errors.As(err, &unplaced) || errors.Is(err, library.ErrNoItem) {
			continue
		}
		if err != nil {
			return nil, err
		}
		dir := p.ItemDir(pl)
		stop := filepath.Join(p.Root, library.MoviesDir)
		if pl.Type != "movie" {
			stop = filepath.Join(p.Root, library.SeriesDir)
		}
		if underRoot(stop, dir) {
			out[dir] = stop
		}
		out[p.InboxDir(id)] = p.Work
	}
	rows, err := pool.Query(ctx, `SELECT id FROM com_nalet_katalog_itemversions WHERE item_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var vid string
		if err := rows.Scan(&vid); err != nil {
			rows.Close()
			return nil, err
		}
		out[p.StagingDir(vid)] = p.Work
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, x := range extras {
		out[p.ExtraInboxDir(x.ID)] = p.Work
		out[p.ExtraStagingDir(x.ID)] = p.Work
	}
	for dir := range out {
		if !underRoot(p.Root, dir) {
			delete(out, dir)
		}
	}
	return out, nil
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

// extraPackageRoot is where the packager puts the package of the extra id:
// packages/extras/<the id's first two characters>/<id>, whatever its title's
// type.
func extraPackageRoot(packagesRoot, id string) string {
	shard := "00"
	if len(id) >= 2 {
		shard = id[:2]
	}
	return filepath.Join(packagesRoot, "extras", shard, id)
}

// isNoRows is defined in service.go for pgx.ErrNoRows; keep a local alias so
// this file stands alone if that helper ever moves.
var _ = pgx.ErrNoRows
