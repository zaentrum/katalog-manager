package itemactions

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/library"
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

// BusyTeller tells, in a transaction, why a re-encode would leave a title
// alone now — its transcode or package running, or waiting for its worker,
// within the step's timeout —, "" when it would not, the title's steps
// locked until the transaction ends (retry.Service). A replace asked with
// ifIdle asks it of its Reencoder.
type BusyTeller interface {
	BusyIn(ctx context.Context, tx pgx.Tx, itemID string) (string, error)
}

var _ graph.SourceReplacer = (*Service)(nil)

// The GraphQL codes of a replaceSource refusal (graph.SourceRefused).
const (
	codeSourceRefused  = "SOURCE_REFUSED"
	codeSourceConflict = "SOURCE_CONFLICT"
	codeSourceBusy     = "SOURCE_BUSY"
	codeSourceCovered  = "SOURCE_COVERED"
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
//     path of the file replaced. One there is not is NOT_FOUND. An episode
//     another episode's file covers (migration 045) has no file of its own:
//     it is refused (SOURCE_COVERED, naming the holder, whose file is the one
//     to replace).
//   - The new file is an absolute path of an existing regular file of its
//     own — no link — under the media root and outside the package store, as
//     written and with the links of its folders followed, that a scan takes
//     for a title's file (scanner.NoTitleFile). A path that is the title's
//     file already changes nothing.
//   - It is no file of the catalog yet, as written nor as its folders' links
//     lead: not another asset row's, of this title or another, nor a live
//     extra's, nor an original's the library keeps (SOURCE_CONFLICT, naming
//     it). A scan finds a file's title by its path, so a file is one title's.
//   - Asked with a key (an addon's id of what it hands over), a replace of
//     the title asked with that key once already changes nothing: the answer
//     is what that one did (repeated), so that one asked again, its first
//     answer lost, never replaces twice.
//
// Asked with IfIdle, a title one of whose steps that read an original
// (processing.OriginalSteps) runs, or waits for its worker, is given no file
// (SOURCE_BUSY): that run reads the old file, and what it reports would be
// taken for the new one's. Its steps stay locked until the replace's
// transaction ends, so none starts meanwhile.
//
// A refusal that a later ask may not meet — the title's original being
// retired, a version of it placed and its handover not recorded, a file or
// the catalog's settings that could not be read, a migration not in place —
// is SOURCE_BUSY; anything else refused is SOURCE_REFUSED, and nothing
// changes. In one transaction the title's source asset row moves to the new
// file, which it describes as a scan does a new one (newSource), and what
// described the old file goes (forgetTheOldFile). Everything else of the
// title stays as it is: the item and its external ids, credits, genres, tags,
// artwork, extras, segments, chapters, package, subtitles and steps, and the
// languages an admin set for its tracks; the item is modified (modifiedat,
// modifiedby), as by any change of it. What is kept elsewhere by the item's
// id (everyone's progress, what they rated) is so kept too.
//
// The lock ingest and the scanner take on the path of a title's file is held
// on the new file's path for all of it, its transaction and the moves of its
// files: a scan or an ingest of it waits, then finds the title's.
//
// Then, the title having its new file, the old file is deleted when asked
// (dropOldFile, the legacy layout), and the title is encoded again from the
// new file when asked, as reencodeItem does: a title whose transcode or
// package is running is left alone (busy), and without an event bus nothing
// is encoded. Neither fails the call: the answer says what they did.
//
// With the setting library.layout=v2 the new file lies under ARRIVALS_ROOT or
// in .work/replace/, from where it is taken into the arrivals: a link of its
// name there is made under the lock, which fails on a name there already (no
// file is ever written over), and the name in .work/replace goes once the
// transaction is committed; one left by an ask stopped before is taken as it
// is. It is a source of the title of its own (a new sourceId). The old
// original stays where it lies, whatever is asked: asked to go
// (deleteOldFile), it is superseded (library.Supersede), and the retire job
// retires it once a version of the title made without it is complete and
// verified, through the trash, unless the title holds its originals; not
// asked, it stays a source of the title, as it is. A source of the old
// original that is being retired refuses the replace as busy. The title's
// folder records a note of what became of the old original
// (library.NoteReplaced). A title whose original was retired (it has no file)
// may be given one: it gets its file anew. The re-encode is then a new
// version of the title, which supersedes the one there is.
func (s *Service) ReplaceSource(ctx context.Context, in graph.ReplaceSourceRequest) (res graph.ReplaceSourceResult, err error) {
	defer func() { s.logReplace(ctx, in, res, err) }()
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
	set, err := library.ReadSettings(ctx, s.st.Pool())
	if err != nil {
		return res, graph.RefuseSource(codeSourceBusy, "the library's settings could not be read: %v", err)
	}
	// An addon replaces a title's file when the catalog names it.
	if err := s.addonMay(ctx, set); err != nil {
		return res, err
	}
	// An episode another episode's file covers has no file of its own: its
	// holder's is the one to replace.
	if itemID != "" {
		holder, err := library.HolderOf(ctx, s.st.Pool(), itemID)
		if err != nil {
			return res, graph.RefuseSource(codeSourceBusy, "whether another episode's file covers item %s could not be read: %v", itemID, err)
		}
		if holder != "" {
			r := graph.RefuseSource(codeSourceCovered, "item %s has no file of its own: the file of episode %s covers it, "+
				"and is the one to replace", itemID, holder)
			r.ItemID = holder
			return res, r
		}
	}
	return s.replaceSource(ctx, in, set, path, itemID, itemPath)
}

// addonMay refuses a replace asked by an addon's service account whose
// client the setting library.replace.clients does not name (FORBIDDEN): an
// addon replaces a title's file, and has its old original go, only once the
// catalog names it. An admin, and the platform's service account, are no
// addon.
func (s *Service) addonMay(ctx context.Context, set library.Settings) error {
	p, _ := auth.PrincipalFrom(ctx)
	pol := s.cfg.Policy()
	if p == nil || p.Unrestricted || pol.IsAdmin(p) || pol.IsService(p) || !pol.IsAddon(p) || set.MayReplace(p.Client) {
		return nil
	}
	named := "names none"
	if len(set.ReplaceClients) > 0 {
		named = "names " + strings.Join(set.ReplaceClients, ", ")
	}
	return &auth.Forbidden{Operation: "replaceSource", Role: pol.AdminRole,
		Requires: "the " + pol.AdminRole + " role, or an addon whose client the setting " + library.SettingReplaceClients +
			" names (it " + named + ", and not " + orDash(p.Client) + ")"}
}

// logReplace logs a replace, whatever came of it: who asked it — the token's
// subject, the client it was issued to and what the catalog takes it for —,
// the title, its old file and the new one, and what it came to (what it did,
// or why it was refused). An addon's replaces can be read back so.
func (s *Service) logReplace(ctx context.Context, in graph.ReplaceSourceRequest, res graph.ReplaceSourceResult, err error) {
	item, old, cur := res.ItemID, res.OldPath, res.Path
	if item == "" {
		item = strings.TrimSpace(in.ItemID)
	}
	if old == "" {
		old = strings.TrimSpace(in.ItemPath)
	}
	if cur == "" {
		cur = strings.TrimSpace(in.Path)
	}
	title := "?"
	if item != "" {
		var t, typ string
		if s.st.Pool().QueryRow(context.WithoutCancel(ctx), `SELECT title, type FROM com_nalet_katalog_items WHERE id = $1`, item).
			Scan(&t, &typ) == nil {
			title = fmt.Sprintf("%q, %s", t, typ)
		}
	}
	outcome := "replaced: " + res.Message
	var refused *graph.SourceRefused
	var forbidden *auth.Forbidden
	switch {
	case errors.As(err, &refused):
		outcome = "refused (" + refused.Code + "): " + refused.Message
	case errors.As(err, &forbidden):
		outcome = "refused (FORBIDDEN): " + forbidden.Error()
	case err != nil:
		outcome = "failed: " + err.Error()
	case !res.Replaced:
		outcome = "nothing changed: " + res.Message
	}
	log.Printf("replaceSource asked by %s: item %s (%s), old file %s, new file %s, deleteOldFile %v, reencode %v, ifIdle %v: %s",
		s.callerOf(ctx), orDash(item), title, orDash(old), orDash(cur), in.DeleteOldFile, in.Reencode, in.IfIdle, outcome)
}

// callerOf names who asks a replace: the token's subject, the client it was
// issued to, and what the catalog takes it for.
func (s *Service) callerOf(ctx context.Context) string {
	p, _ := auth.PrincipalFrom(ctx)
	if p == nil {
		return "nobody signed in"
	}
	pol := s.cfg.Policy()
	role := "a viewer"
	switch {
	case p.Unrestricted:
		role = "auth off"
	case pol.IsAdmin(p):
		role = "an admin (" + pol.AdminRole + ")"
	case pol.IsService(p):
		role = "the platform's service account"
	case pol.IsAddon(p):
		role = "an addon (" + pol.AddonRole + ")"
	}
	return fmt.Sprintf("%s (client %s, %s)", orDash(p.Subject), orDash(p.Client), role)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// replaceSource is ReplaceSource of the title named by itemID or itemPath,
// which no other's file covers, and the new file at path, absolute and clean.
func (s *Service) replaceSource(ctx context.Context, in graph.ReplaceSourceRequest, set library.Settings, path, itemID,
	itemPath string) (graph.ReplaceSourceResult, error) {
	var res graph.ReplaceSourceResult
	pool := s.st.Pool()
	v2 := set.V2()
	key := strings.TrimSpace(in.Key)
	switch {
	case key != "" && !v2:
		return res, graph.RefuseSource(codeSourceRefused, "a replace's key is kept with the library's v2 layout alone: ask without it")
	case len(key) > 255:
		return res, graph.RefuseSource(codeSourceRefused, "a replace's key is 255 characters at most")
	}
	// With the v2 layout an old original asked to go is kept superseded, and
	// a key kept, which migration 056 lets a source be and hold.
	if v2 && (in.DeleteOldFile || key != "") {
		if ready, err := library.SupersedeReady(ctx, pool); err != nil || !ready {
			return res, graph.RefuseSource(codeSourceBusy, "the catalog cannot keep an old original superseded or a replace's key yet "+
				"(migration db/migrations/056_superseded_sources.sql is not in place%s): the title is given no other file now", errNote(err))
		}
	}
	// With the v2 layout a file handed over in .work/replace/ is taken into
	// the arrivals first: the title's file is the one there.
	arrival := path
	handed := v2 && library.Within(library.PathsOf(s.cfg).ReplaceDir(), path)
	if handed {
		arrival = filepath.Join(library.PathsOf(s.cfg).Arrivals, filepath.Base(path))
	}

	// The path's lock, on a connection of its own that the transaction runs
	// on, held until the files are where they go.
	conn, unlock, err := s.lockPath(ctx, arrival)
	if err != nil {
		return res, err
	}
	defer unlock()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The title's lock, which the retire job holds while it claims and
	// retires its originals, first: its rows are locked after it.
	id, err := itemOf(ctx, tx, itemID, itemPath)
	if err != nil {
		return res, err
	}
	if err := library.LockItem(ctx, tx, id); err != nil {
		return res, err
	}
	if key != "" {
		if done, err := repeatedOf(ctx, tx, id, key); err != nil || done != nil {
			if done != nil {
				s.finishMove(path, done.Path)
				return *done, nil
			}
			return res, err
		}
	}
	src, err := sourceOf(ctx, tx, itemID, itemPath, v2)
	if err != nil {
		return res, err
	}
	if src.itemID != id {
		return res, graph.RefuseSource(codeSourceBusy, "the file %s changed hands meanwhile: ask again", itemPath)
	}
	res.ItemID, res.OldPath, res.Path = src.itemID, src.path, arrival
	if src.assetID != "" && filepath.Clean(src.path) == arrival {
		// The title's file already. A file handed over that is it — the
		// name an ask stopped before left in .work/replace — goes; another
		// is no file of this title's.
		if handed {
			if same, err := sameFile(path, arrival); err == nil && !same {
				return res, graph.RefuseSource(codeSourceConflict, "%s is the title's file already, and the file handed over, %s, "+
					"is another", arrival, path)
			}
			s.finishMove(path, arrival)
		}
		res.Message = arrival + " is the title's file already: nothing changed"
		return res, nil
	}
	if v2 {
		if why, err := s.handoverLost(ctx, tx, src.itemID); err != nil {
			return res, graph.RefuseSource(codeSourceBusy, "whether the title's version is in the library could not be read: %v", err)
		} else if why != "" {
			return res, graph.RefuseSource(codeSourceBusy, "%s", why)
		}
	}
	size, resolved, refused := s.sourceFile(path, v2)
	if refused != nil {
		return res, refused
	}
	for _, p := range []string{arrival, resolved} {
		if err := takenAlready(ctx, tx, src, p, v2); err != nil {
			return res, err
		}
	}
	if in.IfIdle {
		if err := s.idle(ctx, tx, src.itemID); err != nil {
			return res, err
		}
	}
	sidecars, err := s.oldSidecars(ctx, tx, src)
	if err != nil {
		return res, err
	}
	var old *library.Source // the old original's source, with the v2 layout
	if v2 && src.assetID != "" {
		if old, err = s.oldSourceOf(ctx, tx, src); err != nil {
			return res, err
		}
		// The retire job has an original being retired: it is no title's
		// file to replace meanwhile.
		if old != nil && old.State == library.SourceRetiring {
			return res, graph.RefuseSource(codeSourceBusy, "%v: the title is given no other file now; ask again once it is",
				&library.ErrNotPresent{ID: old.ID, State: old.State})
		}
	}
	var fsize int64
	var qh1 string
	if v2 {
		if fsize, qh1, err = library.QH1(path); err != nil {
			return res, graph.RefuseSource(codeSourceBusy, "%s cannot be read: %v", path, err)
		}
	}
	// The file handed over gets its name in the arrivals now, under the
	// path's lock; until the transaction is committed, it goes again on any
	// failure.
	var mv *move
	if handed {
		if mv, err = reserve(path, arrival); err != nil {
			return res, err
		}
	}
	committed := false
	defer func() {
		if mv != nil && !committed {
			mv.undo()
		}
	}()
	by := auth.Actor(ctx, "katalog-manager")
	if src.assetID == "" {
		// A title whose original was retired is given a file anew.
		if err := tx.QueryRow(ctx, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, true, 'primary') RETURNING id`, src.itemID, arrival, size).
			Scan(&src.assetID); err != nil {
			return res, fmt.Errorf("give item %s the file %s: %w", src.itemID, arrival, err)
		}
	} else if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET `+newSource+` WHERE id = $1`,
		src.assetID, arrival, size); err != nil {
		return res, fmt.Errorf("give item %s the file %s: %w", src.itemID, arrival, err)
	}
	if v2 {
		// The new file is a source of the title of its own; the old one, if
		// it is still there, stays one, and is retired after its version.
		ns, err := library.PathsOf(s.cfg).AddSource(ctx, tx, src.itemID, arrival, fsize, qh1)
		if err != nil {
			return res, fmt.Errorf("give item %s the file %s: %w", src.itemID, arrival, err)
		}
		if key != "" {
			if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET replacekey = $2 WHERE id = $1`, ns.ID, key); err != nil {
				return res, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $2 WHERE id = $1`, src.assetID, ns.ID); err != nil {
			return res, err
		}
		// The old original asked to go is superseded by the new one, kept
		// where it lies for the retire job.
		if old != nil && in.DeleteOldFile {
			var busy *library.ErrNotPresent
			if err := library.Supersede(ctx, tx, old.ID, ns.ID); errors.As(err, &busy) {
				return res, graph.RefuseSource(codeSourceBusy, "%v: the title is given no other file now; ask again once it is", err)
			} else if err != nil {
				return res, err
			}
			res.OldFileSuperseded = true
		}
	}
	if err := forgetTheOldFile(ctx, tx, src.itemID); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now(), modifiedby = left($2, 255) WHERE id = $1`,
		src.itemID, by); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		// Whether it was committed is read again: a commit whose answer was
		// lost may have been.
		switch done, rerr := s.hasFile(src.itemID, arrival); {
		case rerr != nil:
			committed = true // unknown: both names stay, for the next ask to find out
			log.Printf("replace: whether item %s has the file %s could not be read after its commit failed (%v): %v; "+
				"the file handed over keeps both its names", src.itemID, arrival, err, rerr)
			return res, err
		case !done:
			return res, err
		}
		log.Printf("replace: the commit of item %s's file %s failed (%v), and the catalog has it: it is replaced", src.itemID, arrival, err)
	}
	committed = true
	if mv != nil {
		mv.done()
	}
	res.Replaced, res.OldSidecars = true, int32(len(sidecars))

	var said string
	if v2 {
		said = s.keptOld(ctx, src, old, arrival, res.OldFileSuperseded)
	} else {
		res.OldFileDeleted, said = s.dropOldFile(ctx, src.path, arrival, in.DeleteOldFile)
	}
	if in.Reencode {
		res.Reencode = s.encodeAgain(ctx, src.itemID)
	}
	res.Message = replacedMessage(res, said, sidecars)
	return res, nil
}

// lockPath takes the lock ingest and the scanner take on the path of a
// title's file, on a connection of its own, held until the answer's unlock:
// the replace's transaction runs on that connection, and its files are moved
// before it is let go. A connection whose lock could not be let go is
// closed, never handed back holding it.
func (s *Service) lockPath(ctx context.Context, path string) (*pgxpool.Conn, func(), error) {
	conn, err := s.st.Pool().Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	const key = `hashtext('com_nalet_katalog_playbackassets:' || $1)`
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+key+`)`, path); err != nil {
		conn.Release()
		return nil, nil, err
	}
	return conn, func() {
		bg := context.WithoutCancel(ctx)
		if _, err := conn.Exec(bg, `SELECT pg_advisory_unlock(`+key+`)`, path); err != nil {
			_ = conn.Conn().Close(bg)
		}
		conn.Release()
	}, nil
}

// move is a file handed over in .work/replace/ given its name in the
// arrivals (reserve): linked there, its name in .work/replace going once the
// replace is committed; or, on a filesystem that links no file, renamed,
// renamed back on a failure.
type move struct {
	from, to string
	renamed  bool
}

// reserve gives the file handed over at from its name in the arrivals, to,
// under the path's lock: a hard link, which fails on a name there already, so
// that no file is ever written over. A name there that is the file itself —
// a link an ask stopped before left — is taken as it is; another is refused
// (SOURCE_CONFLICT). A filesystem that links no file has it renamed, the name
// looked at again right before.
func reserve(from, to string) (*move, error) {
	if err := library.MkdirAll(filepath.Dir(to)); err != nil {
		return nil, graph.RefuseSource(codeSourceBusy, "the arrivals cannot take %s: %v", from, err)
	}
	err := os.Link(from, to)
	switch {
	case err == nil:
		return &move{from: from, to: to}, nil
	case errors.Is(err, os.ErrExist):
		if same, serr := sameFile(from, to); serr == nil && same {
			return &move{from: from, to: to}, nil
		}
		return nil, graph.RefuseSource(codeSourceConflict, "%s cannot be taken into the arrivals: %s is there already", from, to)
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.EMLINK):
		if _, err := os.Lstat(to); err == nil {
			return nil, graph.RefuseSource(codeSourceConflict, "%s cannot be taken into the arrivals: %s is there already", from, to)
		}
		if err := os.Rename(from, to); err != nil {
			return nil, graph.RefuseSource(codeSourceBusy, "%s cannot be taken into the arrivals: %v", from, err)
		}
		return &move{from: from, to: to, renamed: true}, nil
	}
	return nil, graph.RefuseSource(codeSourceBusy, "%s cannot be taken into the arrivals: %v", from, err)
}

// undo gives the arrivals back what the move took: its link goes, while it is
// the file handed over still; a renamed file goes back.
func (m *move) undo() {
	if m.renamed {
		if err := os.Rename(m.to, m.from); err != nil {
			log.Printf("replace: %s could not be handed back to %s after the replace failed: %v", m.to, m.from, err)
		}
		return
	}
	if same, err := sameFile(m.from, m.to); err == nil && same {
		if err := os.Remove(m.to); err != nil {
			log.Printf("replace: the link %s could not be removed after the replace failed: %v", m.to, err)
		}
	}
}

// done ends the move once the replace is committed: the name in
// .work/replace goes, while the arrivals' is the same file.
func (m *move) done() {
	if !m.renamed {
		finishMove(m.from, m.to)
	}
}

// finishMove removes the name from of a file handed over whose name in the
// arrivals to is the same file: the move of a replace committed (one stopped
// before it removed the name is finished so by the next ask).
func finishMove(from, to string) {
	if same, err := sameFile(from, to); err == nil && same {
		if err := os.Remove(from); err != nil {
			log.Printf("replace: %s could not be removed once %s took it: %v", from, to, err)
		}
	}
}

// finishMove is finishMove of a file handed over in .work/replace/, of the
// v2 layout alone.
func (s *Service) finishMove(from, to string) {
	if from != to && library.Within(library.PathsOf(s.cfg).ReplaceDir(), from) {
		finishMove(from, to)
	}
}

// sameFile reports whether a and b are the same file (one of two links), as
// os.SameFile tells it; false with an error when either cannot be read.
func sameFile(a, b string) (bool, error) {
	fa, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Lstat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// hasFile reads again, after a commit that failed, whether the title has the
// file at path as its own: whether the commit went through.
func (s *Service) hasFile(itemID, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var has bool
	err := s.st.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND path = $2
		AND isprimary = true AND COALESCE(kind, 'primary') = 'primary')`, itemID, path).Scan(&has)
	return has, err
}

// repeatedOf is, in tx, what the replace of the title asked with key did
// once already, as its answer says it (repeated); nil when none of the
// title's sources was given its file by one asked with key.
func repeatedOf(ctx context.Context, tx pgx.Tx, itemID, key string) (*graph.ReplaceSourceResult, error) {
	var sourceID, filename string
	var at time.Time
	var arrival *string
	err := tx.QueryRow(ctx, `SELECT id, filename, arrivalpath, createdat FROM com_nalet_katalog_itemsources
		WHERE item_id = $1 AND replacekey = $2`, itemID, key).Scan(&sourceID, &filename, &arrival, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res := &graph.ReplaceSourceResult{ItemID: itemID, Replaced: true, Repeated: true}
	if arrival != nil {
		res.Path = *arrival
	}
	var oldPath *string
	switch err := tx.QueryRow(ctx, `SELECT arrivalpath FROM com_nalet_katalog_itemsources WHERE item_id = $1 AND supersededby = $2
		ORDER BY supersededat DESC, id LIMIT 1`, itemID, sourceID).Scan(&oldPath); {
	case err == nil:
		res.OldFileSuperseded = true
		if oldPath != nil {
			res.OldPath = *oldPath
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	where := "where it is now"
	if arrival != nil {
		where = *arrival
	}
	res.Message = fmt.Sprintf("a replace asked with the key %q gave the title its file %s at %s already (source %s, %s): "+
		"nothing changed", key, filename, library.Timestamp(at), sourceID, where)
	return res, nil
}

// idle refuses, in tx, a replace asked only while the title is idle when its
// transcode or package runs or waits for its worker (SOURCE_BUSY), its
// re-encoder's word (BusyTeller), the title's steps locked until tx ends. A
// service whose re-encoder cannot tell refuses it too, rather than guess.
func (s *Service) idle(ctx context.Context, tx pgx.Tx, itemID string) error {
	bt, ok := s.reencoder.(BusyTeller)
	if !ok {
		return graph.RefuseSource(codeSourceRefused,
			"ifIdle is asked, and nothing here tells whether the title's transcode or package runs: ask without it")
	}
	why, err := bt.BusyIn(ctx, tx, itemID)
	if err != nil {
		return fmt.Errorf("whether the title's transcode or package runs could not be read: %w", err)
	}
	if why != "" {
		return graph.RefuseSource(codeSourceBusy, "the title is given no other file while it is encoded (ifIdle): %s", why)
	}
	return nil
}

// handoverLost says why, with the v2 layout, the title is given no other
// file now, "" when it may be: the version the pipeline builds for it is in
// the library already (a run placed its folder, with the original it
// renamed into it) and its handover is not recorded. The run taken again
// reports that version as it is, which another file would make another
// version's.
func (s *Service) handoverLost(ctx context.Context, q library.Querier, itemID string) (string, error) {
	v, err := library.Building(ctx, q, itemID)
	if err != nil || v == nil {
		return "", err
	}
	pl, err := library.PlaceOf(ctx, q, itemID)
	if err != nil {
		return "", nil
	}
	if !library.Placed(library.PathsOf(s.cfg).ItemDir(pl), v.ID) {
		return "", nil
	}
	return fmt.Sprintf("the title's version %s is in the library, and its handover is not recorded yet: retry its package "+
		"(or takein) step, which reports it again, then give it another file", v.ID), nil
}

// sourceOf finds, in tx, the file the title named one way is given another
// in place of, and locks its asset row: the item's source asset by the item's
// id (one: a title with two names the one by itemPath), or the source asset
// at itemPath. The title is a movie or an episode.
func sourceOf(ctx context.Context, tx pgx.Tx, itemID, itemPath string, v2 bool) (source, error) {
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
		if v2 {
			return src, nil // its original was retired: it is given one anew
		}
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
// source may be, and answers its size and the path its folders' links lead
// to: an existing regular file of its own — no link — inside the media root
// and outside the package store, as written and with the links of its
// folders followed (a folder inside the media root that leads out of it is
// refused, as for an extra's file), that a scan takes for a title's file. A
// file that could not be read is refused as busy: a later ask may read it.
func (s *Service) sourceFile(path string, v2 bool) (int64, string, *graph.SourceRefused) {
	roots, named := []string{s.cfg.NFSRoot}, "the media root"
	if v2 {
		p := library.PathsOf(s.cfg)
		roots, named = []string{p.Arrivals, p.ReplaceDir()}, "ARRIVALS_ROOT or .work/replace"
	}
	inside := func(p string, resolve bool) bool {
		pkgs := s.cfg.PackagesRoot
		if resolve {
			pkgs = resolvedRoot(pkgs)
		}
		if underRoot(pkgs, p) {
			return false
		}
		for _, r := range roots {
			if resolve {
				r = resolvedRoot(r)
			}
			if underRoot(r, p) {
				return true
			}
		}
		return false
	}
	if !inside(path, false) {
		return 0, "", graph.RefuseSource(codeSourceRefused, "%s is not under %s (or is under the package store)", path, named)
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, "", graph.RefuseSource(codeSourceRefused, "there is no file at %s", path)
	case err != nil:
		return 0, "", graph.RefuseSource(codeSourceBusy, "%s cannot be read: %v", path, err)
	case fi.Mode()&os.ModeSymlink != 0:
		return 0, "", graph.RefuseSource(codeSourceRefused, "%s is a link: a title's file is a file of its own", path)
	case !fi.Mode().IsRegular():
		return 0, "", graph.RefuseSource(codeSourceRefused, "%s is no file", path)
	}
	target, err := filepath.EvalSymlinks(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 0, "", graph.RefuseSource(codeSourceRefused, "there is no file at %s", path)
	case err != nil:
		return 0, "", graph.RefuseSource(codeSourceBusy, "%s cannot be read: %v", path, err)
	case !inside(target, true):
		return 0, "", graph.RefuseSource(codeSourceRefused, "%s leads out of %s", path, named)
	}
	if why := scanner.NoTitleFile(path); why != "" {
		return 0, "", graph.RefuseSource(codeSourceRefused, "%s is no title's file to a scan: %s", path, why)
	}
	return fi.Size(), target, nil
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
// another asset row's, of this title or another, a live extra's, or, with the
// v2 layout, an original's the library keeps there (present, superseded or
// being retired).
func takenAlready(ctx context.Context, tx pgx.Tx, src source, path string, v2 bool) error {
	var owner string
	if v2 {
		var state string
		err := tx.QueryRow(ctx, `SELECT item_id, state FROM com_nalet_katalog_itemsources WHERE arrivalpath = $1
			AND state IN ('present', 'retiring', 'superseded') ORDER BY item_id LIMIT 1`, path).Scan(&owner, &state)
		switch {
		case err == nil:
			r := graph.RefuseSource(codeSourceConflict, "%s is an original of item %s already (%s)", path, owner, state)
			r.ItemID = owner
			return r
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
	}
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
	if src.path == "" {
		return nil, nil
	}
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
// when asked, with the legacy layout: as RemoveItem deletes a title's files,
// only under the media root (its root guard) and never under the package
// store, and only one nothing of the catalog is any more (no asset row, no
// live extra) and the new file does not lead to (a link to it); the folders
// it leaves empty go, up to the root. It says what became of the file, also
// when it was not asked to go.
func (s *Service) dropOldFile(ctx context.Context, old, cur string, asked bool) (bool, string) {
	root := s.cfg.NFSRoot
	if old == "" {
		return false, "the title had no file"
	}
	media := underRoot(root, old)
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
	pruneEmptyDirs(filepath.Dir(old), root)
	return true, "the old file is deleted"
}

// keptOld says, with the v2 layout, what became of the title's old original,
// which stays where it lies (old: its source, nil when it had none), and
// records a note of it in the title's folder: superseded, retired by the
// retire job once a version of the title made without it is complete and
// verified — unless the title holds its originals —, or a source of the title
// still, as it was.
func (s *Service) keptOld(ctx context.Context, src source, old *library.Source, cur string, superseded bool) string {
	var said string
	switch {
	case src.path == "":
		return "the title had no file: its original was retired"
	case old == nil:
		said = "the old file was gone already"
	case superseded:
		said = "the old file stays where it lies, superseded: the retire job moves it into the library's trash once a version " +
			"of the title made of the new file is complete and verified"
		var held bool
		if err := s.st.Pool().QueryRow(ctx, `SELECT retirehold FROM com_nalet_katalog_items WHERE id = $1`, src.itemID).
			Scan(&held); err == nil && held {
			said += ", and not while the title holds its originals (holdOriginal)"
		}
	default:
		said = "the old file stays where it lies, a source of the title as it was"
	}
	pl, err := library.PlaceOf(ctx, s.st.Pool(), src.itemID)
	if err != nil {
		return said
	}
	note, err := library.NoteReplaced(library.PathsOf(s.cfg).ItemDir(pl), library.ReplacedBy(s.callerOf(ctx)), old,
		filepath.Base(cur), superseded, time.Now())
	switch {
	case err != nil:
		log.Printf("replace: the note of what became of the old file of item %s could not be recorded: %v", src.itemID, err)
		said += "; the title's folder records no note of it: " + err.Error()
	case note != "":
		said += "; the title's folder records it (note " + note + ")"
	}
	return said
}

// itemOf is, in tx, the title named one way, by its id or by the path of its
// file, as sourceOf finds it, without locking anything: the item whose lock
// the replace takes first. One there is not is answered as sourceOf answers
// it.
func itemOf(ctx context.Context, tx pgx.Tx, itemID, itemPath string) (string, error) {
	if itemPath == "" {
		return itemID, nil
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets
		WHERE isprimary = true AND COALESCE(kind, 'primary') = 'primary' AND path = $1 ORDER BY item_id, id LIMIT 1`, itemPath).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", graph.RefuseSource(codeNotFound, "no item has the file %s", itemPath)
	}
	return id, err
}

// oldSourceOf is, in tx, the source of the title's old file with the v2
// layout: the one its asset names, or the one whose original lies at its
// path; one the catalog has none of yet (a row from before migration 040) is
// taken in as one now, as library.EnsureSource does. nil when the old file is
// gone, and there is nothing to keep of it. One that cannot be read refuses
// the replace as busy: its file stays the title's.
func (s *Service) oldSourceOf(ctx context.Context, tx pgx.Tx, src source) (*library.Source, error) {
	var named *string
	if err := tx.QueryRow(ctx, `SELECT sourceid FROM com_nalet_katalog_playbackassets WHERE id = $1`, src.assetID).
		Scan(&named); err != nil {
		return nil, err
	}
	if named != nil {
		old, err := library.SourceByID(ctx, tx, *named)
		if err != nil || old != nil {
			return old, err
		}
	}
	old, err := library.SourceAt(ctx, tx, src.path)
	if err != nil || old != nil {
		return old, err
	}
	if _, err := os.Lstat(src.path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	size, qh1, err := library.QH1(src.path)
	if err != nil {
		return nil, graph.RefuseSource(codeSourceBusy, "the title's old file %s cannot be read: %v; it stays the title's", src.path, err)
	}
	old, err = library.PathsOf(s.cfg).AddSource(ctx, tx, src.itemID, src.path, size, qh1)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $2 WHERE id = $1`, src.assetID, old.ID); err != nil {
		return nil, err
	}
	return old, nil
}

// errNote is ": err" of an error, nothing of none.
func errNote(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
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
