package itemactions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/scanner"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Reencoder encodes a title again with the pipeline's current settings, as
// reencodeItem does (retry.Service).
type Reencoder interface {
	ReencodeItem(ctx context.Context, id string) (graph.ReencodeResult, error)
}

// WithReencoder has ReplaceSource encode a title again through r once the
// title has its new file.
func (s *Service) WithReencoder(r Reencoder) *Service {
	s.reencoder = r
	return s
}

var _ graph.SourceReplacer = (*Service)(nil)

// The GraphQL codes of a replaceSource refusal (graph.SourceRefused).
const (
	codeSourceRefused  = "SOURCE_REFUSED"
	codeSourceConflict = "SOURCE_CONFLICT"
	codeNotFound       = "NOT_FOUND"
)

// newSource is what a title's source asset says of the file it is given: its
// path and its size, as a scan records a new file, and nothing that described
// the old one: no hash, no probe (codec, resolution, bit rate, duration), no
// audio and no track counts, which the workers report of the file they read.
const newSource = `path = $2, sizebytes = $3, hash = NULL, codec = NULL, resolution = NULL, bitratekbps = NULL,
	durationms = NULL, audiocodec = NULL, audiolanguage = NULL, audiochannels = NULL, audiobitratekbps = NULL,
	audiotrackcount = NULL, subtitletrackcount = NULL`

// source is the file a title is given another in place of: its asset row and
// its path, and the title.
type source struct {
	assetID, path      string
	itemID, typ, title string
}

// ReplaceSource implements graph.SourceReplacer: it gives a title another
// file, its source, and keeps the title, so that a title upgraded to a better
// file (a 320×180 copy to its 1080p original) stays the title it was.
//
//   - The title is a movie or an episode with a file (its source asset: its
//     primary asset that is no package), named one way, by its id or by the
//     path of the file replaced. One there is not is NOT_FOUND.
//   - The new file is an absolute path of an existing regular file under the
//     media root and outside the package store, as written and with its links
//     followed, that a scan takes for a title's file (scanner.NoTitleFile). A
//     path that is the title's file already changes nothing.
//   - It is no file of the catalog yet: not another asset row's, of this
//     title or another, nor a live extra's (SOURCE_CONFLICT, naming it). A
//     scan finds a file's title by its path, so a file is one title's; one
//     replace onto a file runs at a time.
//
// Anything else refused is SOURCE_REFUSED. In one transaction the title's
// source asset row moves to the new file, which it describes as a scan does
// a new one (newSource), and what described the old file goes
// (forgetTheOldFile). Everything else of the title stays as it is: the item
// and its external ids, credits, genres, tags, artwork, extras, segments,
// chapters, package, subtitles and steps, and the languages an admin set for
// its tracks; the item is modified (modifiedat, modifiedby), as by any change
// of it. What is kept elsewhere by the item's id (everyone's progress, what
// they rated) is so kept too.
//
// Then, the title having its new file, the old file is deleted when asked
// (dropOldFile), and the title is encoded again from the new file when asked,
// as reencodeItem does: a title whose transcode or package is running is left
// alone (busy), and without an event bus nothing is encoded. Neither fails the
// call: the answer says what they did.
func (s *Service) ReplaceSource(ctx context.Context, in graph.ReplaceSourceRequest) (graph.ReplaceSourceResult, error) {
	var res graph.ReplaceSourceResult
	path := strings.TrimSpace(in.Path)
	switch {
	case path == "":
		return res, graph.RefuseSource(codeSourceRefused, "a title is given a file by its path: path is required")
	case !filepath.IsAbs(path):
		return res, graph.RefuseSource(codeSourceRefused, "%s is no absolute path", path)
	}
	path = filepath.Clean(path)
	itemID, itemPath := strings.TrimSpace(in.ItemID), strings.TrimSpace(in.ItemPath)
	if (itemID == "") == (itemPath == "") {
		return res, graph.RefuseSource(codeSourceRefused,
			"name the title one way: itemId, or itemPath (the path of the file replaced)")
	}
	// Once begun, a replace runs to its end, also when the caller stops
	// waiting: no title is given its file without its old file's deletion and
	// its re-encode.
	ctx = context.WithoutCancel(ctx)

	tx, err := s.st.Pool().Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('com_nalet_katalog_playbackassets:' || $1))`, path); err != nil {
		return res, err
	}
	src, err := sourceOf(ctx, tx, itemID, itemPath)
	if err != nil {
		return res, err
	}
	res.ItemID, res.OldPath, res.Path = src.itemID, src.path, path
	if filepath.Clean(src.path) == path {
		res.Message = path + " is the title's file already: nothing changed"
		return res, nil
	}
	size, refused := s.sourceFile(path)
	if refused != nil {
		return res, refused
	}
	if err := takenAlready(ctx, tx, src, path); err != nil {
		return res, err
	}
	sidecars, err := s.oldSidecars(ctx, tx, src)
	if err != nil {
		return res, err
	}
	by := auth.Actor(ctx, "katalog-manager")
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET `+newSource+` WHERE id = $1`,
		src.assetID, path, size); err != nil {
		return res, fmt.Errorf("give item %s the file %s: %w", src.itemID, path, err)
	}
	if err := forgetTheOldFile(ctx, tx, src.itemID); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now(), modifiedby = left($2, 255) WHERE id = $1`,
		src.itemID, by); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	res.Replaced, res.OldSidecars = true, int32(len(sidecars))

	var said string
	res.OldFileDeleted, said = s.dropOldFile(ctx, src.path, path, in.DeleteOldFile)
	if in.Reencode {
		res.Reencode = s.encodeAgain(ctx, src.itemID)
	}
	res.Message = replacedMessage(res, said, sidecars)
	log.Printf("replaced the file of item %s (%q, %s), by %s: %s", src.itemID, src.title, src.typ, by, res.Message)
	return res, nil
}

// sourceOf finds, in tx, the file the title named one way is given another
// in place of, and locks its asset row: the item's source asset by the item's
// id (one: a title with two names the one by itemPath), or the source asset
// at itemPath. The title is a movie or an episode.
func sourceOf(ctx context.Context, tx pgx.Tx, itemID, itemPath string) (source, error) {
	const sql = `SELECT p.id, p.path, i.id, i.type, i.title FROM com_nalet_katalog_playbackassets p
		JOIN com_nalet_katalog_items i ON i.id = p.item_id
		WHERE p.isprimary = true AND COALESCE(p.kind, 'primary') = 'primary' AND `
	var src source
	if itemPath != "" {
		found, err := sources(tx.Query(ctx, sql+`p.path = $1 ORDER BY i.id, p.id FOR UPDATE OF p`, itemPath))
		if err != nil {
			return src, err
		}
		switch len(found) {
		case 0:
			return src, graph.RefuseSource(codeNotFound, "no item has the file %s", itemPath)
		case 1:
			src = found[0]
		default:
			var of []string
			for _, f := range found {
				of = append(of, "asset "+f.assetID+" of item "+f.itemID)
			}
			return src, graph.RefuseSource(codeSourceConflict, "the file %s is more than one title's file: %s", itemPath,
				strings.Join(of, ", "))
		}
	} else {
		err := tx.QueryRow(ctx, `SELECT id, type, title FROM com_nalet_katalog_items WHERE id = $1`, itemID).
			Scan(&src.itemID, &src.typ, &src.title)
		if errors.Is(err, pgx.ErrNoRows) {
			return src, graph.RefuseSource(codeNotFound, "unknown item: %s", itemID)
		}
		if err != nil {
			return src, err
		}
	}
	switch strings.ToLower(src.typ) {
	case "movie", "episode":
	case "series":
		return src, graph.RefuseSource(codeSourceRefused,
			"item %s is a series, and a series has no file: its episodes have, each replaced on its own", src.itemID)
	default:
		return src, graph.RefuseSource(codeSourceRefused, "item %s is a %s: only a movie's or an episode's file is replaced",
			src.itemID, src.typ)
	}
	if itemPath != "" {
		return src, nil
	}
	files, err := sources(tx.Query(ctx, sql+`p.item_id = $1 ORDER BY p.id FOR UPDATE OF p`, src.itemID))
	if err != nil {
		return src, err
	}
	switch len(files) {
	case 0:
		return src, graph.RefuseSource(codeSourceRefused, "item %s has no file to replace", src.itemID)
	case 1:
		src.assetID, src.path = files[0].assetID, files[0].path
		return src, nil
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.path)
	}
	return src, graph.RefuseSource(codeSourceRefused, "item %s has %d files (%s): name the one to replace by itemPath",
		src.itemID, len(files), strings.Join(paths, ", "))
}

// sources reads the rows of a query of sources.
func sources(rows pgx.Rows, err error) ([]source, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []source
	for rows.Next() {
		var x source
		if err := rows.Scan(&x.assetID, &x.path, &x.itemID, &x.typ, &x.title); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// sourceFile checks that path, absolute and clean, names a file a title's
// source may be, and answers its size: an existing regular file inside the
// media root and outside the package store, as written and with its links
// followed (a link inside the media root that leads out of it is refused, as
// for an extra's file), that a scan takes for a title's file.
func (s *Service) sourceFile(path string) (int64, *graph.SourceRefused) {
	inside := func(p string, resolve bool) bool {
		media, pkgs := s.cfg.NFSRoot, s.cfg.PackagesRoot
		if resolve {
			media, pkgs = resolvedRoot(media), resolvedRoot(pkgs)
		}
		return underRoot(media, p) && !underRoot(pkgs, p)
	}
	if !inside(path, false) {
		return 0, graph.RefuseSource(codeSourceRefused, "%s is not under the media root (or is under the package store)", path)
	}
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, graph.RefuseSource(codeSourceRefused, "there is no file at %s", path)
	}
	if err != nil {
		return 0, graph.RefuseSource(codeSourceRefused, "%s cannot be read: %v", path, err)
	}
	if !inside(target, true) {
		return 0, graph.RefuseSource(codeSourceRefused, "%s leads out of the media root", path)
	}
	fi, err := os.Stat(target)
	if err != nil {
		return 0, graph.RefuseSource(codeSourceRefused, "%s cannot be read: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return 0, graph.RefuseSource(codeSourceRefused, "%s is no file", path)
	}
	if why := scanner.NoTitleFile(path); why != "" {
		return 0, graph.RefuseSource(codeSourceRefused, "%s is no title's file to a scan: %s", path, why)
	}
	return fi.Size(), nil
}

// resolvedRoot is root with its links followed, root itself when they cannot
// be (a root that does not exist yet).
func resolvedRoot(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		return r
	}
	return filepath.Clean(root)
}

// takenAlready refuses, in tx, a file that is a file of the catalog already:
// another asset row's, of this title or another, or a live extra's.
func takenAlready(ctx context.Context, tx pgx.Tx, src source, path string) error {
	var owner string
	err := tx.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 AND id <> $2
		ORDER BY isprimary DESC NULLS LAST, item_id LIMIT 1`, path, src.assetID).Scan(&owner)
	switch {
	case err == nil:
		r := graph.RefuseSource(codeSourceConflict, "%s is a file of item %s already", path, owner)
		r.ItemID = owner
		return r
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	var extras bool // a catalog older than migration 039 keeps no extra
	if err := tx.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_itemextras') IS NOT NULL`).Scan(&extras); err != nil || !extras {
		return err
	}
	var extra string
	err = tx.QueryRow(ctx, `SELECT id, item_id FROM com_nalet_katalog_itemextras WHERE sourcepath = $1 AND removedat IS NULL
		ORDER BY id LIMIT 1`, path).Scan(&extra, &owner)
	switch {
	case err == nil:
		r := graph.RefuseSource(codeSourceConflict, "%s is extra %s of item %s already", path, extra, owner)
		r.ItemID, r.ExtraID = owner, extra
		return r
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	}
	return err
}

// oldSidecars are, in tx, the title's subtitle files beside its old file and
// named after it, as a scan pairs them (scanner.SidecarOf), its package's
// aside. They stay the title's, as they are.
func (s *Service) oldSidecars(ctx context.Context, tx pgx.Tx, src source) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT path FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY path`, src.itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if !underRoot(s.cfg.PackagesRoot, p) && scanner.SidecarOf(src.path, p) {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// forgetTheOldFile removes, in tx, what described the title's old file
// besides its asset row: the tracks a package reported of it (migration 037),
// so that the new file's package records the new file's (packaging-complete
// replaces a kind of track only when its manifest lists the kind), and its
// diagnostics, a snapshot of the old file (its path, size, ffprobe and
// folder), which backfillSourceProbes would read as the new one's.
//
// The languages an admin set for the title's tracks stay. They name a track
// as the worker record does, by its kind and its ordinal, its place among the
// source's streams of the kind in ffprobe's order, and the packager labels
// the track at that place in the file it packages: they apply to the new
// file by ordinal, and one for a track the new file does not have is passed
// over.
func forgetTheOldFile(ctx context.Context, tx pgx.Tx, itemID string) error {
	var tracks bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('com_nalet_katalog_itemtracks') IS NOT NULL`).Scan(&tracks); err != nil {
		return err
	}
	if tracks {
		if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemtracks WHERE item_id = $1`, itemID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemdiagnostics WHERE item_id = $1`, itemID)
	return err
}

// dropOldFile deletes the title's old file once the title has its new one,
// when asked: as RemoveItem deletes a title's files, only under the media
// root (its root guard) and never under the package store, and only one
// nothing of the catalog is any more (no asset row, no live extra) and the
// new file does not lead to (a link to it); the folders it leaves empty go,
// up to the root. It says what became of the file, also when it was not
// asked to go.
func (s *Service) dropOldFile(ctx context.Context, old, cur string, asked bool) (bool, string) {
	media := underRoot(s.cfg.NFSRoot, old)
	switch {
	case !asked && media:
		return false, "the old file stays, under the media root: the next scan takes it in as a title of its own unless it is moved out of it or deleted"
	case !asked:
		return false, "the old file stays"
	case !media:
		return false, "the old file is kept: it is not under the media root"
	case underRoot(s.cfg.PackagesRoot, old):
		return false, "the old file is kept: it is under the package store"
	}
	if why, err := s.stillHeld(ctx, old); err != nil {
		return false, "the old file is kept: whether the catalog holds it still could not be read: " + err.Error()
	} else if why != "" {
		return false, "the old file is kept: " + why
	}
	if a, err := filepath.EvalSymlinks(cur); err == nil {
		if b, err := filepath.EvalSymlinks(old); err == nil && a == b {
			return false, "the old file is kept: the new one leads to it"
		}
	}
	fi, err := os.Lstat(old)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, "the old file was gone already"
	case err != nil:
		return false, "the old file is kept: " + err.Error()
	case fi.IsDir():
		return false, "the old file is kept: it is a folder"
	}
	if err := os.Remove(old); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "the old file was gone already"
		}
		return false, "the old file could not be deleted: " + err.Error()
	}
	pruneEmptyDirs(filepath.Dir(old), s.cfg.NFSRoot)
	return true, "the old file is deleted"
}

// stillHeld says what of the catalog the file at path is still, "" when
// nothing is: an asset row's file, or a live extra's.
func (s *Service) stillHeld(ctx context.Context, path string) (string, error) {
	var item string
	err := s.st.Pool().QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1
		ORDER BY item_id LIMIT 1`, path).Scan(&item)
	switch {
	case err == nil:
		return "it is a file of item " + item + " still", nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", err
	}
	x, err := s.st.ExtraAt(ctx, path)
	switch {
	case errors.Is(err, store.ErrNoExtras):
		return "", nil
	case err != nil:
		return "", err
	case x != nil:
		return "it is extra " + x.ID + " of item " + x.ItemID, nil
	}
	return "", nil
}

// encodeAgain encodes the title again from its new file, as reencodeItem
// does; a re-encode that cannot run says why in its message.
func (s *Service) encodeAgain(ctx context.Context, itemID string) *graph.ReencodeResult {
	if s.reencoder == nil {
		return &graph.ReencodeResult{ItemID: itemID, Message: "cannot re-encode: nothing encodes a title again here"}
	}
	r, err := s.reencoder.ReencodeItem(ctx, itemID)
	if err != nil {
		r.ItemID, r.Message = itemID, err.Error()
	}
	return &r
}

// replacedMessage says what a replace did: the title's files, what became of
// the old one (said) and of the subtitle files named after it, and what
// encoding it again did.
func replacedMessage(res graph.ReplaceSourceResult, said string, sidecars []string) string {
	parts := []string{fmt.Sprintf("the title's file is %s now, in place of %s", res.Path, res.OldPath), said}
	if n := len(sidecars); n > 0 {
		paired := 0
		for _, p := range sidecars {
			if scanner.SidecarOf(res.Path, p) {
				paired++
			}
		}
		switch old, cur := filepath.Dir(res.OldPath), filepath.Dir(res.Path); {
		case paired == n:
			parts = append(parts, fmt.Sprintf("the subtitle files named after the old file (%d) pair with the new one, as they did", n))
		case old == cur || underRoot(cur, old):
			parts = append(parts, fmt.Sprintf("the subtitle files named after the old file (%d) stay the title's as they are; "+
				"the next scan pairs those named after the new one", n))
		default:
			parts = append(parts, fmt.Sprintf("the subtitle files named after the old file (%d) stay the title's as they are, "+
				"and the packager no longer takes them, outside the new file's folder; the next scan pairs those named after the new one", n))
		}
	}
	switch r := res.Reencode; {
	case r == nil:
		parts = append(parts, "it is not encoded again: a package it has is the old file's until reencodeItem encodes the new one")
	case r.Busy > 0:
		parts = append(parts, r.Message+"; that run is the old file's: encode the title again (reencodeItem) once it is done")
	default:
		parts = append(parts, r.Message)
	}
	return strings.Join(parts, "; ")
}
