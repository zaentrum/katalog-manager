package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// The adopt (platform-library/1, 7.2 phase 3): unit by unit, a series before
// its episodes, under the item's lock, the guards are checked (the old
// package is the one the plan saw), the plan's renames made, each journaled,
// the originals gone before the library was recorded given their
// original-deleted event, and the database changed in one transaction as
// packaging-complete leaves it (7.3 db). A failure puts the unit's renames
// back in reverse. The staged people are put in place after the items.
// Adopting again skips what is adopted. A plan moves an original into its
// version's folder (versions/<versionId>/original.<ext>), the title's first
// version; a title nothing packaged gets one too, taken in (its original and
// no package), which the catalog records taken (migration 044). A plan of
// before moves originals to the arrivals. The episodes a source's one file
// covers besides the item (db.sources[].covers, the item first) are linked
// to it in the same transaction (migration 045), and a revert puts their
// links back as they were.
//
// The revert replays an adoption backwards, the last adopted first: the
// originals the retire job deleted since come back from the trash, every
// rename is undone, the published records go back into staging, and the
// database is as the journal says it was. It is refused for an item that
// changed since (another version, another file, a new extra), whose original
// is being retired, or whose original's trash was purged.

// MigratedBy is who acts in a migration, as its events say.
const MigratedBy = "katalog-manager (migration)"

// GoneBefore is why the original of an item that was gone before the library
// recorded it is deleted, as its event says.
const GoneBefore = "gone before the library was recorded"

// ErrNoRun says there is no such migration run.
var ErrNoRun = errors.New("no such migration run")

// ErrMigrationBusy says another adopt, revert or names of the run runs.
var ErrMigrationBusy = errors.New("another adopt, revert or names of the run runs")

// Migration adopts and reverts a run.
type Migration struct {
	pool *pgxpool.Pool
	cfg  config.Config
	p    Paths
	Run  string
	Dir  string
	now  func() time.Time
}

// NewMigration is the run of the catalog's pool at the paths cfg configures:
// ErrNoRun when its folder is not there.
func NewMigration(pool *pgxpool.Pool, cfg config.Config, run string) (*Migration, error) {
	if !ValidRun(run) {
		return nil, fmt.Errorf("%q is no run's name: letters, digits, dots, dashes and underscores", run)
	}
	p := PathsOf(cfg)
	m := &Migration{pool: pool, cfg: cfg, p: p, Run: run, Dir: p.MigrationDir(run), now: time.Now}
	if fi, err := os.Stat(m.Dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNoRun, m.Dir)
	}
	return m, nil
}

// UnitResult is what became of a unit.
type UnitResult struct {
	ItemID string `json:"itemId"`
	Type   string `json:"type"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// MigrationReport is what an adopt or a revert did.
type MigrationReport struct {
	Run      string       `json:"run"`
	Adopted  int          `json:"adopted"`
	Reverted int          `json:"reverted"`
	Skipped  int          `json:"skipped"`
	Stale    int          `json:"stale"`
	Busy     int          `json:"busy"`
	Refused  int          `json:"refused"`
	Failed   int          `json:"failed"`
	People   int          `json:"people"`
	Stopped  string       `json:"stopped,omitempty"`
	Units    []UnitResult `json:"units"`
}

func (r *MigrationReport) add(u UnitResult) {
	switch u.State {
	case StateAdopted:
		r.Adopted++
	case StateReverted:
		r.Reverted++
	case "skipped":
		r.Skipped++
	case StateStale:
		r.Stale++
	case StateBusy:
		r.Busy++
	case StateRefused:
		r.Refused++
	default:
		r.Failed++
	}
	r.Units = append(r.Units, u)
}

// lock holds the run's lock, one adopt, revert or names at a time, until
// release.
func (m *Migration) lock(ctx context.Context) (release func(), err error) {
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	key := "library-migration:" + m.Run
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, err
	}
	if !locked {
		conn.Release()
		return nil, ErrMigrationBusy
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, key)
		conn.Release()
	}, nil
}

// open reads the run's units and opens its journal, and puts right what a
// crash stopped: an adoption whose database changed is adopted, one whose
// did not has its renames put back.
func (m *Migration) open(ctx context.Context) ([]*Unit, *Journal, error) {
	units, err := ReadUnits(m.Dir)
	if err != nil {
		return nil, nil, err
	}
	j, err := OpenJournal(filepath.Join(m.Dir, "journal.jsonl"), m.now)
	if err != nil {
		return nil, nil, err
	}
	byID := map[string]*Unit{}
	for _, u := range units {
		byID[u.ItemID] = u
	}
	for id, ij := range j.items() {
		if len(ij.open) == 0 || ij.state() == StateAdopted || byID[id] == nil {
			continue // nothing open, or an open revert: the next revert goes on with it
		}
		var recorded *time.Time
		if err := m.pool.QueryRow(ctx, `SELECT recordedat FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&recorded); err != nil &&
			!errors.Is(err, pgx.ErrNoRows) {
			_ = j.Close()
			return nil, nil, err
		}
		if recorded != nil && (attempt{entries: ij.open}).db(StateDone) != nil {
			if err := j.Add(JournalEntry{ItemID: id, Op: OpUnit, State: StateAdopted,
				Reason: "a crash stopped the journal after the database changed"}); err != nil {
				_ = j.Close()
				return nil, nil, err
			}
			continue
		}
		var done []Move
		for _, e := range (attempt{entries: ij.open}).moves(StateDone) {
			done = append(done, Move{Kind: e.Kind, From: e.From, To: e.To})
		}
		if un := unprojectionOf(ij.open); un != nil {
			if _, err := os.Stat(byID[id].ItemDir); err == nil {
				_ = unproject(byID[id].ItemDir, un)
			}
		}
		m.undo(j, id, done)
		if err := j.Add(JournalEntry{ItemID: id, Op: OpUnit, State: StateFailed,
			Reason: "a crash stopped its adoption: its renames are put back"}); err != nil {
			_ = j.Close()
			return nil, nil, err
		}
	}
	return units, j, nil
}

// Adopt adopts the run's units, those of only when it names any, and then
// its staged people.
func (m *Migration) Adopt(ctx context.Context, only []string) (MigrationReport, error) {
	rep := MigrationReport{Run: m.Run, Units: []UnitResult{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	units, j, err := m.open(ctx)
	if err != nil {
		return rep, err
	}
	defer j.Close()
	items := j.items()
	for _, u := range units {
		if len(only) > 0 && !slices.Contains(only, u.ItemID) {
			continue
		}
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the units left are not adopted"
			return rep, nil
		}
		rep.add(m.adoptUnit(context.WithoutCancel(ctx), j, items[u.ItemID], u))
	}
	if len(only) == 0 {
		n, err := m.adoptPeople(context.WithoutCancel(ctx), j)
		rep.People = n
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// adoptUnit adopts one unit.
func (m *Migration) adoptUnit(ctx context.Context, j *Journal, ij *itemJournal, u *Unit) UnitResult {
	res := UnitResult{ItemID: u.ItemID, Type: u.Type}
	end := func(state, reason string) UnitResult {
		res.State, res.Reason = state, reason
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpUnit, State: state, Reason: reason}); err != nil {
			res.State, res.Reason = StateFailed, reason+"; and the journal could not say so: "+err.Error()
		}
		return res
	}
	if ij.state() == StateAdopted {
		res.State, res.Reason = "skipped", "adopted already"
		return res
	}
	if statOK(filepath.Join(u.ItemDir, ItemFile)) {
		res.State, res.Reason = "skipped", "recorded already: "+u.ItemDir+" holds its item.json"
		return res
	}
	if err := m.checkPlan(ctx, u); err != nil {
		return end(StateRefused, err.Error())
	}
	if err := m.checkCovers(ctx, u); err != nil {
		return end(StateRefused, err.Error())
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return end(StateFailed, err.Error())
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := LockItem(ctx, tx, u.ItemID); err != nil {
		return end(StateFailed, err.Error())
	}
	var typ string
	var complete bool
	err = tx.QueryRow(ctx, `SELECT i.type, EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
			WHERE v.item_id = i.id AND v.state IN ('complete', 'taken', 'building'))
		FROM com_nalet_katalog_items i WHERE i.id = $1`, u.ItemID).Scan(&typ, &complete)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return end(StateRefused, "the catalog does not hold the item any more")
	case err != nil:
		return end(StateFailed, err.Error())
	case typ != u.Type && !(u.Type == "series" && typ == "series"):
		return end(StateRefused, fmt.Sprintf("the catalog holds it as a %s, and the plan as a %s: stage it again", typ, u.Type))
	case complete:
		return end(StateRefused, "the catalog holds a version of it already: it was packaged in the library meanwhile")
	}
	if why, err := m.stale(u); err != nil {
		return end(StateStale, "stale plan, re-stage: "+err.Error())
	} else if why != "" {
		return end(StateStale, "stale plan, re-stage: "+why)
	}
	if why, err := busyItem(ctx, tx, u.ItemID); err != nil {
		return end(StateFailed, err.Error())
	} else if why != "" {
		return end(StateBusy, why)
	}
	before, err := m.snapshot(ctx, tx, u)
	if err != nil {
		return end(StateFailed, err.Error())
	}
	var done []Move
	var unproj *unprojection
	fail := func(why string) UnitResult {
		if unproj != nil {
			if err := unproject(u.ItemDir, unproj); err != nil {
				why += "; and its staged metadata.json could not be put back: " + err.Error()
			}
		}
		m.undo(j, u.ItemID, done)
		return end(StateFailed, why)
	}
	for _, mv := range u.Moves {
		if err := renameInto(mv.From, mv.To); err != nil {
			return fail(fmt.Sprintf("the %s move of %s to %s: %v; its renames are put back", mv.Kind, mv.From, mv.To, err))
		}
		done = append(done, mv)
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpMove, Kind: mv.Kind, From: mv.From, To: mv.To, State: StateDone}); err != nil {
			return fail("the journal cannot be written: " + err.Error())
		}
	}
	gone, err := m.goneBefore(u)
	if err != nil {
		return fail(err.Error())
	}
	if err := m.apply(ctx, tx, u, gone); err != nil {
		return fail("the database: " + err.Error())
	}
	if unproj, err = m.recordItem(ctx, tx, u); err != nil {
		return fail("the item's record: " + err.Error())
	}
	if unproj != nil {
		pb, err := json.Marshal(unproj)
		if err != nil {
			return fail(err.Error())
		}
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpProjection, Before: pb, State: StateDone,
			Reason: "the item changed since its projection was staged: projected again"}); err != nil {
			return fail("the journal cannot be written: " + err.Error())
		}
	}
	b, err := json.Marshal(before)
	if err != nil {
		return fail(err.Error())
	}
	if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpDB, Before: b, State: StateDone}); err != nil {
		return fail("the journal cannot be written: " + err.Error())
	}
	if err := tx.Commit(ctx); err != nil {
		return fail("the database: " + err.Error())
	}
	m.prune(u.Moves, false)
	return end(StateAdopted, "")
}

// prune removes the folders a unit's renames left empty, up to the root
// each lies in: the store before the library's after an adoption, the
// arrivals' and the record's after a revert (back).
func (m *Migration) prune(moves []Move, back bool) {
	roots := []string{m.cfg.NFSRoot, m.cfg.Roots(false).Extras, m.cfg.PackagesRoot}
	if back {
		roots = []string{m.p.Arrivals, m.p.Extras, filepath.Join(m.p.Root, MoviesDir), filepath.Join(m.p.Root, SeriesDir),
			filepath.Join(m.Dir, "legacy")}
	}
	for _, mv := range moves {
		dir := filepath.Dir(mv.From)
		if back {
			dir = filepath.Dir(mv.To)
		}
		for _, root := range roots {
			if Within(root, dir) {
				pruneEmpty(dir, root)
				break
			}
		}
	}
}

// renameInto renames from to to, the folder to goes in made first.
func renameInto(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%s is there already", to)
	}
	if err := MkdirAll(filepath.Dir(to)); err != nil {
		return err
	}
	return os.Rename(from, to)
}

// undo puts the renames done back, the last first, journaled: each whose
// target is there and whose origin is not.
func (m *Migration) undo(j *Journal, itemID string, done []Move) {
	for i := len(done) - 1; i >= 0; i-- {
		mv := done[i]
		if _, err := os.Lstat(mv.To); err != nil {
			continue
		}
		if _, err := os.Lstat(mv.From); err == nil {
			continue
		}
		if err := renameInto(mv.To, mv.From); err != nil {
			_ = j.Add(JournalEntry{ItemID: itemID, Op: OpMove, Kind: mv.Kind, From: mv.To, To: mv.From, State: StateFailed,
				Reason: err.Error()})
			continue
		}
		_ = j.Add(JournalEntry{ItemID: itemID, Op: OpMove, Kind: mv.Kind, From: mv.To, To: mv.From, State: StateUndone})
	}
}

// checkPlan refuses a plan the path rules or the share's roots do not
// allow: the item's folder is not where the rules put it, a version's
// folder not where they put it in the item's, a series' episode comes
// before its series, a move leaves the share's roots, or the plan puts an
// original in the record where no move of it goes: an original goes into
// the record only directly into the folder of a version of the plan (the
// staged one, or the item's), named as the library names an original.
func (m *Migration) checkPlan(ctx context.Context, u *Unit) error {
	if u.Run != m.Run {
		return fmt.Errorf("the plan is of run %s", u.Run)
	}
	if !ValidID(u.ItemID) {
		return fmt.Errorf("the item's id %q names no folder", u.ItemID)
	}
	if _, ok := unitOrder[u.Type]; !ok {
		return fmt.Errorf("an item of type %s has no folder in the library", u.Type)
	}
	pl, err := PlaceOf(ctx, m.pool, u.ItemID)
	if err != nil {
		return err
	}
	if want := m.p.ItemDir(pl); filepath.Clean(u.ItemDir) != want {
		return fmt.Errorf("the plan puts the item at %s, and the path rules at %s", u.ItemDir, want)
	}
	staged := filepath.Join(m.Dir, "staged", u.ItemID, "item")
	if filepath.Clean(u.StagedDir) != staged {
		return fmt.Errorf("the plan stages the item at %s, and the run at %s", u.StagedDir, staged)
	}
	if u.Type == "episode" && !statOK(filepath.Join(filepath.Dir(filepath.Dir(u.ItemDir)), ItemFile)) {
		return errors.New("its series is not in the library: adopt the series first")
	}
	inShare := func(path string, roots ...string) bool {
		for _, r := range roots {
			if Within(r, path) {
				return true
			}
		}
		return false
	}
	old := []string{m.cfg.PackagesRoot}
	media := []string{m.cfg.NFSRoot, m.cfg.Roots(false).Extras, m.p.Arrivals, m.p.Extras}
	publish := 0
	for _, mv := range u.Moves {
		if !filepath.IsAbs(mv.From) || !filepath.IsAbs(mv.To) || filepath.Clean(mv.From) != mv.From || filepath.Clean(mv.To) != mv.To {
			return fmt.Errorf("the %s move of %q to %q names no clean absolute paths", mv.Kind, mv.From, mv.To)
		}
		ok := false
		switch mv.Kind {
		case MovePackage:
			ok = inShare(mv.From, old...) && Within(staged, mv.To)
		case MovePublish:
			ok = mv.From == staged && mv.To == filepath.Clean(u.ItemDir)
			publish++
		case MoveOriginal:
			ok = inShare(mv.From, media...) && (inShare(mv.To, m.p.Arrivals, m.p.Extras) || intoVersion(u, staged, mv.To))
		case MoveSidecar:
			ok = inShare(mv.From, media...) && inShare(mv.To, m.p.Arrivals, m.p.Extras)
		case MoveLegacy:
			ok = inShare(mv.From, old...) && Within(filepath.Join(m.Dir, "legacy"), mv.To)
		}
		if !ok {
			return fmt.Errorf("the %s move of %s to %s leaves what such a move may touch", mv.Kind, mv.From, mv.To)
		}
	}
	if publish != 1 {
		return fmt.Errorf("the plan publishes the item %d times", publish)
	}
	for _, v := range u.DB.Versions {
		if !ValidID(v.VersionID) || filepath.Clean(v.Dir) != VersionDir(u.ItemDir, v.VersionID) {
			return fmt.Errorf("the plan puts version %s at %s, and the path rules at %s", v.VersionID, v.Dir,
				VersionDir(u.ItemDir, v.VersionID))
		}
	}
	// An original the database is to find in the record is one a move puts
	// there.
	moved := map[string]bool{}
	for _, mv := range u.Moves {
		if mv.Kind == MoveOriginal && intoVersion(u, staged, mv.To) {
			moved[published(u, staged, mv.To)] = true
		}
	}
	for _, s := range u.DB.Sources {
		if s.ArrivalPath != nil && m.p.InRecord(*s.ArrivalPath) && !moved[filepath.Clean(*s.ArrivalPath)] {
			return fmt.Errorf("the plan has source %s's original at %s, where no move of it goes", s.SourceID, *s.ArrivalPath)
		}
	}
	for _, a := range u.DB.Assets {
		if a.SourceID != nil && m.p.InRecord(a.Path) && !Within(filepath.Join(u.ItemDir, "sources"), a.Path) &&
			!moved[filepath.Clean(a.Path)] {
			return fmt.Errorf("the plan has playback row %s at %s, where no move of an original goes", a.ID, a.Path)
		}
	}
	return nil
}

// coveredOf are the episodes the sources of the plan of u cover besides the
// item, each once, in the order the plan lists them.
func coveredOf(u *Unit) []string {
	var out []string
	for _, s := range u.DB.Sources {
		for i, id := range s.Covers {
			if i > 0 && id != u.ItemID && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// checkCovers refuses a plan whose sources cover episodes besides the item
// (db.sources[].covers) that the catalog cannot link to it: it lacks
// migration 045; a source's covers do not begin with the item; the item is
// no episode; an episode it names twice; one that is no episode of the item's
// series, in the catalog; one with a file of its own, which wins; one another
// episode's file covers already.
func (m *Migration) checkCovers(ctx context.Context, u *Unit) error {
	for _, s := range u.DB.Sources {
		if len(s.Covers) > 0 && s.Covers[0] != u.ItemID {
			return fmt.Errorf("source %s covers %v, which do not begin with the item", s.SourceID, s.Covers)
		}
		seen := map[string]bool{}
		for _, id := range s.Covers {
			if seen[id] {
				return fmt.Errorf("source %s covers episode %s twice", s.SourceID, id)
			}
			seen[id] = true
		}
	}
	covered := coveredOf(u)
	if len(covered) == 0 {
		return nil
	}
	if ok, err := CoversReady(ctx, m.pool); err != nil {
		return err
	} else if !ok {
		return errors.New("its file covers other episodes, and migration 045 (db/migrations/045_multi_episode_files.sql) " +
			"is not applied: adopt it once it is")
	}
	holder, err := PlaceOf(ctx, m.pool, u.ItemID)
	if err != nil {
		return err
	}
	if holder.Type != "episode" {
		return fmt.Errorf("a %s covers no episodes", holder.Type)
	}
	for _, id := range covered {
		pl, err := PlaceOf(ctx, m.pool, id)
		switch {
		case errors.Is(err, ErrNoItem):
			return fmt.Errorf("its file covers episode %s, which the catalog does not hold", id)
		case err != nil:
			return fmt.Errorf("its file covers episode %s: %w", id, err)
		case pl.Type != "episode" || pl.SeriesID != holder.SeriesID:
			return fmt.Errorf("its file covers %s, which is no episode of its series", id)
		}
		var file bool
		if err := m.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets WHERE item_id = $1)`,
			id).Scan(&file); err != nil {
			return err
		}
		if file {
			return fmt.Errorf("its file covers episode %s, which has a file of its own, which wins: stage it again", id)
		}
		if by, err := HolderOf(ctx, m.pool, id); err != nil {
			return err
		} else if by != "" && by != u.ItemID {
			return fmt.Errorf("its file covers episode %s, which the file of episode %s covers already", id, by)
		}
	}
	return nil
}

// intoVersion reports whether a plan of u may move an original to to: into
// the folder of one of its versions, the staged one (under staged) or the
// item's, directly, named as the library names an original.
func intoVersion(u *Unit, staged, to string) bool {
	if !IsOriginalName(filepath.Base(to)) {
		return false
	}
	for _, base := range []string{u.ItemDir, staged} {
		vid, ok := VersionFolderOf(base, to)
		if !ok {
			continue
		}
		for _, v := range u.DB.Versions {
			if v.VersionID == vid {
				return true
			}
		}
	}
	return false
}

// published is where a file a plan of u moves to to lies once the item is
// published: in the item's folder for one moved into its staged records,
// to itself otherwise.
func published(u *Unit, staged, to string) string {
	to = filepath.Clean(to)
	if rel, err := filepath.Rel(staged, to); err == nil && Within(staged, to) {
		return filepath.Join(filepath.Clean(u.ItemDir), rel)
	}
	return to
}

// stale says which guard of the plan does not hold any more; "" when they
// all do.
func (m *Migration) stale(u *Unit) (string, error) {
	var packages []string
	var old string
	for _, mv := range u.Moves {
		switch {
		case mv.Kind == MovePackage:
			packages = append(packages, mv.From)
		case mv.Kind == MoveLegacy && old == "" && filepath.Base(mv.From) == u.ItemID:
			old = mv.From
		}
	}
	if len(packages) > 0 || u.Guards.ListingSha256 != nil {
		got, err := ListingSHA256(packages)
		if err != nil {
			return "", err
		}
		if u.Guards.ListingSha256 == nil || got != *u.Guards.ListingSha256 {
			return fmt.Sprintf("the package files are not the ones staged (their listing is %s, the plan's %s)", got,
				deref(u.Guards.ListingSha256)), nil
		}
	}
	if u.Guards.ManifestSha256 != nil {
		if old == "" {
			return "the plan names a manifest, and no old package folder", nil
		}
		got, _, err := SHA256File(filepath.Join(old, "manifest.json"))
		if err != nil {
			return "", err
		}
		if "sha256:"+got != *u.Guards.ManifestSha256 {
			return "the old package's manifest.json changed since it was staged", nil
		}
	}
	if u.Guards.CompleteMtime != nil {
		if old == "" {
			return "the plan names a .complete, and no old package folder", nil
		}
		got, err := MtimeOf(filepath.Join(old, ".complete"))
		if err != nil {
			return "", err
		}
		if !sameMtime(got, *u.Guards.CompleteMtime) {
			return fmt.Sprintf("the old package's .complete is of %s, and the plan's of %s", got, *u.Guards.CompleteMtime), nil
		}
	}
	return "", nil
}

// busyItem says why the pipeline works on the item, which an adopt must not
// move under it: its transcode runs, or one finished and its package waits
// for the packager, which the moves would take its handoff from; its take-in
// waits or runs, which moves its original; or an extra of it is in its
// packaging. "" when nothing works on it.
func busyItem(ctx context.Context, q Querier, itemID string) (string, error) {
	var title, extra bool
	err := q.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps t WHERE t.item_id = $1 AND t.step = 'transcode'
			AND (t.status = 'in_progress' OR (t.status IN ('done', 'not_applicable', 'skipped')
			  AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps p WHERE p.item_id = t.item_id AND p.step = 'package'
			              AND (p.status IN ('pending', 'in_progress') OR (p.status = 'failed' AND p.nextretryat IS NOT NULL))))))
		OR EXISTS (SELECT 1 FROM com_nalet_katalog_itemprocessingsteps k WHERE k.item_id = $1 AND k.step = 'takein'
			AND (k.status IN ('pending', 'in_progress') OR (k.status = 'failed' AND k.nextretryat IS NOT NULL))),
		EXISTS (SELECT 1 FROM com_nalet_katalog_itemextras x WHERE x.item_id = $1 AND x.removedat IS NULL
			AND x.state IN ('transcoding', 'transcoded', 'packaging'))`, itemID).Scan(&title, &extra)
	switch {
	case err != nil:
		return "", err
	case title:
		return "the pipeline works on it: its transcode runs, or its package or its take-in waits for the packager; " +
			"pause the transcoder and let the packager finish", nil
	case extra:
		return "the pipeline works on one of its extras: pause the transcoder and let the packager finish", nil
	}
	return "", nil
}

// unitBefore is what an adoption changes in the database, as it was: what a
// revert puts back.
type unitBefore struct {
	Item      json.RawMessage   `json:"item"`              // recordedat, libraryprojectedat, modifiedat
	Assets    []json.RawMessage `json:"assets"`            // every playback row of the item
	Subtitles []json.RawMessage `json:"subtitles"`         // the subtitle rows the plan points elsewhere
	Extras    []json.RawMessage `json:"extras"`            // the extras the plan records
	Sources   []json.RawMessage `json:"sources"`           // the plan's sources the catalog held already
	Versions  []string          `json:"versions"`          // the versions the adoption makes
	NewIDs    []string          `json:"sourceIds"`         // every source the adoption writes
	Covered   []json.RawMessage `json:"covered,omitempty"` // the episodes its file covers: id, coveredby
}

// snapshot reads what the adoption of u changes, as it is.
func (m *Migration) snapshot(ctx context.Context, tx pgx.Tx, u *Unit) (*unitBefore, error) {
	b := &unitBefore{}
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object('recordedat', recordedat, 'libraryprojectedat', libraryprojectedat,
			'modifiedat', modifiedat) FROM com_nalet_katalog_items WHERE id = $1`, u.ItemID).Scan(&b.Item); err != nil {
		return nil, err
	}
	rows := func(dst *[]json.RawMessage, sql string, args ...any) error {
		r, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var row json.RawMessage
			if err := r.Scan(&row); err != nil {
				return err
			}
			*dst = append(*dst, row)
		}
		return r.Err()
	}
	var subtitles, extras, sources []string
	for _, s := range u.DB.Subtitles {
		subtitles = append(subtitles, s.ID)
	}
	for _, x := range u.DB.Extras {
		extras = append(extras, x.ID)
	}
	for _, s := range u.DB.Sources {
		sources = append(sources, s.SourceID)
	}
	for _, v := range u.DB.Versions {
		b.Versions = append(b.Versions, v.VersionID)
	}
	b.NewIDs = sources
	if err := rows(&b.Assets, `SELECT to_jsonb(a) FROM com_nalet_katalog_playbackassets a WHERE a.item_id = $1 ORDER BY a.id`,
		u.ItemID); err != nil {
		return nil, err
	}
	if err := rows(&b.Subtitles, `SELECT to_jsonb(s) FROM com_nalet_katalog_subtitleassets s WHERE s.id = ANY($1) ORDER BY s.id`,
		subtitles); err != nil {
		return nil, err
	}
	if err := rows(&b.Extras, `SELECT to_jsonb(x) FROM com_nalet_katalog_itemextras x WHERE x.id = ANY($1) ORDER BY x.id`,
		extras); err != nil {
		return nil, err
	}
	if err := rows(&b.Sources, `SELECT to_jsonb(s) FROM com_nalet_katalog_itemsources s WHERE s.id = ANY($1) ORDER BY s.id`,
		sources); err != nil {
		return nil, err
	}
	if covered := coveredOf(u); len(covered) > 0 {
		if err := rows(&b.Covered, `SELECT jsonb_build_object('id', i.id, 'coveredby', to_jsonb(i)->'coveredby')
			FROM com_nalet_katalog_items i WHERE i.id = ANY($1) ORDER BY i.id`, covered); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// goneEvent is the original-deleted event of a source gone before the
// library recorded it: its moment the plan's, so that an adoption done
// again names it the same.
type goneEvent struct {
	source string
	ev     Event
}

// goneBefore records, in the published item folder, the original-deleted
// event of each source whose original was gone before the library was
// recorded (no arrival, a record): nothing was measured of it, so nothing
// is accepted.
func (m *Migration) goneBefore(u *Unit) ([]goneEvent, error) {
	at, err := time.Parse(time.RFC3339, u.DB.RecordedAt)
	if err != nil {
		return nil, fmt.Errorf("the plan's recordedAt %q: %w", u.DB.RecordedAt, err)
	}
	var out []goneEvent
	for _, s := range u.DB.Sources {
		if s.ArrivalPath != nil || s.RecordDir == nil {
			continue
		}
		vid := ""
		for _, v := range u.DB.Versions {
			if slices.Contains(v.SourceIDs, s.SourceID) {
				vid = v.VersionID
			}
		}
		if vid == "" {
			return nil, fmt.Errorf("source %s was gone before the library, and no version of the plan holds it", s.SourceID)
		}
		reason := GoneBefore
		ev := Event{ID: IDOf("original-deleted:" + s.SourceID), At: at.Truncate(time.Second), By: MigratedBy,
			Kind: EventOriginalDeleted, VersionID: vid, SourceID: s.SourceID, Reason: &reason, Accepted: []string{}}
		if _, err := WriteEvent(u.ItemDir, ev); err != nil {
			return nil, err
		}
		out = append(out, goneEvent{source: s.SourceID, ev: ev})
	}
	return out, nil
}

// apply is the adoption's database change, in tx: the sources, the
// versions, the playback rows (the packaged one as packaging-complete writes
// it from package.json, the others of the item gone), the subtitle rows'
// paths (their defaults kept), the extras recorded, and the episodes the
// item's file covers besides it linked to it (Link: their steps of a file do
// not apply, and they and the item are marked changed, so that the item is
// projected again, numbered up to the last of them). recordItem records the
// item after.
func (m *Migration) apply(ctx context.Context, tx pgx.Tx, u *Unit, gone []goneEvent) error {
	goneOf := map[string]Event{}
	for _, g := range gone {
		goneOf[g.source] = g.ev
	}
	now := m.now().UTC()
	for _, s := range u.DB.Sources {
		sidecars, err := json.Marshal(nonNilSidecars(s.Sidecars))
		if err != nil {
			return err
		}
		state, retireID, retireAt, deletedAt, deletedBy, lost := SourcePresent, (*string)(nil), (*time.Time)(nil),
			(*time.Time)(nil), (*string)(nil), (*string)(nil)
		if ev, ok := goneOf[s.SourceID]; ok {
			by, none := MigratedBy, "[]"
			state, retireID, retireAt, deletedAt, deletedBy, lost = SourceDeleted, &ev.ID, &ev.At, &now, &by, &none
		}
		// A source with a version keeps no name it arrived under: its name is
		// the library's, and the place it had among the arrivals goes.
		var recorded *time.Time
		filename, libraryPath := s.Filename, s.LibraryPath
		if s.RecordDir != nil {
			recorded = &now
			if !IsOriginalName(filename) {
				filename = OriginalName(filename, 0)
			}
			libraryPath = nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath,
				sizebytes, qh1, state, recordedat, recorddir, sidecars, retireeventid, retireeventat, deletedat, deletedby, lost)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb)
			ON CONFLICT (id) DO UPDATE SET item_id = EXCLUDED.item_id, filename = EXCLUDED.filename,
				arrivalpath = EXCLUDED.arrivalpath, librarypath = EXCLUDED.librarypath, sizebytes = EXCLUDED.sizebytes,
				qh1 = EXCLUDED.qh1, state = EXCLUDED.state, recordedat = EXCLUDED.recordedat, recorddir = EXCLUDED.recorddir,
				sidecars = EXCLUDED.sidecars, retireeventid = EXCLUDED.retireeventid, retireeventat = EXCLUDED.retireeventat,
				deletedat = EXCLUDED.deletedat, deletedby = EXCLUDED.deletedby, lost = EXCLUDED.lost, error = NULL,
				modifiedat = now()`,
			s.SourceID, u.ItemID, filename, s.ArrivalPath, libraryPath, s.SizeBytes, s.QH1, state, recorded, s.RecordDir,
			sidecars, retireID, retireAt, deletedAt, deletedBy, lost); err != nil {
			return fmt.Errorf("source %s: %w", s.SourceID, err)
		}
	}
	for _, v := range u.DB.Versions {
		if v.PackageID == "" {
			// Nothing packaged it: taken in, its original in its folder.
			if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir,
					verifiedat, verifiedlevel)
				VALUES ($1, $2, $3, 'taken', $4, $5::timestamptz, $6)`,
				v.VersionID, u.ItemID, v.SourceIDs, v.Dir, v.VerifiedAt, v.VerifiedLevel); err != nil {
				if IsTakenRefused(err) {
					return fmt.Errorf("version %s is taken in, its original and no package, and migration 044 "+
						"(db/migrations/044_library_takein.sql) is not applied: adopt it once it is", v.VersionID)
				}
				return fmt.Errorf("version %s: %w", v.VersionID, err)
			}
			continue
		}
		completed, err := time.Parse(time.RFC3339, v.CompletedAt)
		if err != nil {
			return fmt.Errorf("version %s's completedAt %q: %w", v.VersionID, v.CompletedAt, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir,
				completedat, verifiedat, verifiedlevel)
			VALUES ($1, $2, $3, 'complete', $4, $5, $6, $7::timestamptz, $8)`,
			v.VersionID, u.ItemID, v.SourceIDs, v.PackageID, v.Dir, completed, v.VerifiedAt, v.VerifiedLevel); err != nil {
			return fmt.Errorf("version %s: %w", v.VersionID, err)
		}
	}
	keep := []string{}
	for _, a := range u.DB.Assets {
		keep = append(keep, a.ID)
		var tag interface{ RowsAffected() int64 }
		var err error
		switch {
		case a.VersionID != nil:
			b, rerr := os.ReadFile(a.Path)
			if rerr != nil {
				return fmt.Errorf("the packaged row %s: %w", a.ID, rerr)
			}
			var pkg map[string]any
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.UseNumber()
			if err := dec.Decode(&pkg); err != nil {
				return fmt.Errorf("%s: %w", a.Path, err)
			}
			r := PackagedRowOf(pkg)
			tag, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, versionid = $4, kind = 'packaged',
					isprimary = false, codec = $5, resolution = $6, bitratekbps = $7, sizebytes = $8, audiocodec = $9,
					audiolanguage = $10, audiochannels = $11, audiobitratekbps = $12, audiotrackcount = $13,
					subtitletrackcount = $14, durationms = $15
				WHERE id = $1 AND item_id = $2`, a.ID, u.ItemID, a.Path, *a.VersionID, r.Codec, r.Resolution, r.BitrateKbps,
				r.SizeBytes, r.AudioCodec, r.AudioLanguage, r.AudioChannels, r.AudioBitrateKbps, r.AudioTracks, r.SubtitleTracks,
				r.DurationMs)
		case Within(filepath.Join(u.ItemDir, "sources"), a.Path):
			// the original gone before: its row is a retired one
			tag, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, sourceid = $4, kind = 'original',
					isprimary = false WHERE id = $1 AND item_id = $2`, a.ID, u.ItemID, a.Path, a.SourceID)
		default:
			tag, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, sourceid = $4
				WHERE id = $1 AND item_id = $2`, a.ID, u.ItemID, a.Path, a.SourceID)
		}
		if err != nil {
			return fmt.Errorf("playback row %s: %w", a.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("playback row %s is no row of the item", a.ID)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged'
		AND id <> ALL($2)`, u.ItemID, keep); err != nil {
		return err
	}
	for _, s := range u.DB.Subtitles {
		tag, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_subtitleassets SET path = $3 WHERE id = $1 AND item_id = $2`,
			s.ID, u.ItemID, s.Path)
		if err != nil {
			return fmt.Errorf("subtitle row %s: %w", s.ID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("subtitle row %s is no row of the item", s.ID)
		}
	}
	for _, x := range u.DB.Extras {
		var err error
		if x.Dir != nil {
			_, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_itemextras SET packagepath = $3, recordpath = $3, packageid = $4,
					recordedat = now(), sourcepath = $5, modifiedat = now()
				WHERE id = $1 AND item_id = $2`, x.ID, u.ItemID, *x.Dir, x.PackageID, x.SourcePath)
		} else {
			_, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_itemextras SET sourcepath = $3, modifiedat = now()
				WHERE id = $1 AND item_id = $2`, x.ID, u.ItemID, x.SourcePath)
		}
		if err != nil {
			return fmt.Errorf("extra %s: %w", x.ID, err)
		}
	}
	for _, id := range coveredOf(u) {
		if _, err := Link(ctx, tx, u.ItemID, id); err != nil {
			return fmt.Errorf("episode %s, which its file covers: %w", id, err)
		}
	}
	return nil
}

// unprojection is what a projection the adoption wrote replaced, which an
// undo or a revert puts back: the staged metadata.json, and the images the
// projection added beside the staged ones.
type unprojection struct {
	Metadata []byte   `json:"metadata"`
	Images   []string `json:"images"`
}

// recordItem records the item in tx (recordedat), and its projection: the
// staged metadata.json reflects the item as the export had it, to the
// second. When the item's modifiedat says otherwise (the adoption's own
// extras marked it changed, or it changed since the stage), its projection
// is written again, as the projector writes it, from the catalog as tx
// leaves it; it answers what that replaced. libraryprojectedat is the
// modifiedat the projection on storage reflects.
func (m *Migration) recordItem(ctx context.Context, tx pgx.Tx, u *Unit) (*unprojection, error) {
	recorded, err := time.Parse(time.RFC3339, u.DB.RecordedAt)
	if err != nil {
		return nil, fmt.Errorf("the plan's recordedAt %q: %w", u.DB.RecordedAt, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET recordedat = $2 WHERE id = $1`, u.ItemID, recorded); err != nil {
		return nil, err
	}
	var modified *time.Time
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT modifiedat, $2::text IS NULL OR modifiedat IS NULL
			OR date_trunc('second', modifiedat) <> ($2::timestamptz AT TIME ZONE 'UTC')
		FROM com_nalet_katalog_items WHERE id = $1`, u.ItemID, u.DB.ProjectedDatabaseUpdatedAt).Scan(&modified, &stale); err != nil {
		return nil, err
	}
	var undo *unprojection
	if stale {
		staged, err := os.ReadFile(filepath.Join(u.ItemDir, "metadata.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		pj := NewProjector(m.pool, m.cfg)
		pj.now = m.now
		pr, err := pj.projectItem(ctx, tx, u.ItemID)
		if err != nil {
			return nil, fmt.Errorf("its projection: %w", err)
		}
		if pr == nil {
			return nil, fmt.Errorf("its projection: %s holds no item.json", u.ItemDir)
		}
		undo = &unprojection{Metadata: staged, Images: pr.images}
		modified = pr.modified
	}
	_, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_items SET libraryprojectedat = COALESCE($2::timestamp, '-infinity')
		WHERE id = $1`, u.ItemID, modified)
	return undo, err
}

// unproject puts back in the item folder dir what a projection of the
// adoption replaced.
func unproject(dir string, un *unprojection) error {
	md := filepath.Join(dir, "metadata.json")
	if un.Metadata == nil {
		if err := os.Remove(md); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := WriteFile(md, un.Metadata); err != nil {
		return err
	}
	for _, name := range un.Images {
		if err := os.Remove(filepath.Join(dir, "metadata", name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// unprojectionOf is what the projection entry of entries says it replaced;
// nil when they hold none.
func unprojectionOf(entries []JournalEntry) *unprojection {
	for _, e := range entries {
		if e.Op == OpProjection && e.State == StateDone {
			var un unprojection
			if json.Unmarshal(e.Before, &un) == nil {
				return &un
			}
		}
	}
	return nil
}

func nonNilSidecars(s []mappedSidecar) []mappedSidecar {
	if s == nil {
		return []mappedSidecar{}
	}
	return s
}

// adoptPeople puts the staged people in place, after the items: those whose
// folder the library does not hold already (the projector's, else). A
// person's projection reflects them as the export had them, to the second
// (its databaseUpdatedAt): unchanged since, the person is projected as they
// are, and the projector leaves them; else it writes them again. It answers
// how many it put.
func (m *Migration) adoptPeople(ctx context.Context, j *Journal) (int, error) {
	entries, err := os.ReadDir(filepath.Join(m.Dir, "staged-people"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		pid := e.Name()
		if !e.IsDir() || !ValidID(pid) {
			continue
		}
		from, to := filepath.Join(m.Dir, "staged-people", pid), m.p.PersonDir(pid)
		if _, err := os.Lstat(to); err == nil {
			continue
		}
		var updated *string
		if b, err := os.ReadFile(filepath.Join(from, "person.json")); err == nil {
			if d, err := DecodeDoc(b); err == nil {
				if v, ok := d.Get("databaseUpdatedAt"); ok {
					if s, ok := v.(string); ok {
						updated = &s
					}
				}
			}
		}
		var before json.RawMessage
		err := m.pool.QueryRow(ctx, `SELECT jsonb_build_object('libraryprojectedat', libraryprojectedat)
			FROM com_nalet_katalog_people WHERE id = $1`, pid).Scan(&before)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return n, err
		}
		if err := renameInto(from, to); err != nil {
			return n, fmt.Errorf("person %s: %w", pid, err)
		}
		if err := j.Add(JournalEntry{ItemID: pid, Op: OpMove, Kind: "person", From: from, To: to, Before: before,
			State: StateDone}); err != nil {
			return n, err
		}
		if updated != nil {
			if _, err := m.pool.Exec(ctx, `UPDATE com_nalet_katalog_people SET libraryprojectedat =
					CASE WHEN date_trunc('second', modifiedat) = $2::timestamptz THEN modifiedat ELSE $2::timestamptz END
				WHERE id = $1`, pid, *updated); err != nil {
				return n, fmt.Errorf("person %s: %w", pid, err)
			}
		}
		if err := j.Add(JournalEntry{ItemID: pid, Op: OpUnit, Kind: "person", State: StateAdopted}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Revert reverts the run's adopted units, those of only when it names any,
// the last adopted first; and the people it put in place, once none of its
// units is adopted any more.
func (m *Migration) Revert(ctx context.Context, only []string) (MigrationReport, error) {
	rep := MigrationReport{Run: m.Run, Units: []UnitResult{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	units, j, err := m.open(ctx)
	if err != nil {
		return rep, err
	}
	defer j.Close()
	byID := map[string]*Unit{}
	for _, u := range units {
		byID[u.ItemID] = u
	}
	items := j.items()
	type adopted struct {
		id  string
		seq int64
	}
	var order []adopted
	for id, ij := range items {
		if a := ij.adoption(); a != nil && byID[id] != nil {
			order = append(order, adopted{id, a.unit.Seq})
		}
	}
	sort.Slice(order, func(i, k int) bool { return order[i].seq > order[k].seq })
	left := 0
	for _, a := range order {
		if len(only) > 0 && !slices.Contains(only, a.id) {
			left++
			continue
		}
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the units left are not reverted"
			return rep, nil
		}
		res := m.revertUnit(context.WithoutCancel(ctx), j, items[a.id], byID[a.id])
		if res.State != StateReverted {
			left++
		}
		rep.add(res)
	}
	if left == 0 && len(only) == 0 {
		n, err := m.revertPeople(context.WithoutCancel(ctx), j, items)
		rep.People = n
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// revertUnit reverts the adoption of one unit.
func (m *Migration) revertUnit(ctx context.Context, j *Journal, ij *itemJournal, u *Unit) UnitResult {
	res := UnitResult{ItemID: u.ItemID, Type: u.Type}
	end := func(state, reason string) UnitResult {
		res.State, res.Reason = state, reason
		if state != StateReverted {
			// A refused revert leaves the unit adopted, and a failed one
			// what it reverted journaled: the next revert goes on with it.
			return res
		}
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpUnit, State: state}); err != nil {
			res.State, res.Reason = StateFailed, "reverted; and the journal could not say so: "+err.Error()
		}
		return res
	}
	a := ij.adoption()
	dbEntry := a.db(StateDone)
	if dbEntry == nil {
		return end(StateRefused, "the journal does not hold the database as it was before the adoption")
	}
	var before unitBefore
	if err := json.Unmarshal(dbEntry.Before, &before); err != nil {
		return end(StateRefused, "the journal's database before the adoption cannot be read: "+err.Error())
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return end(StateRefused, err.Error())
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := LockItem(ctx, tx, u.ItemID); err != nil {
		return end(StateRefused, err.Error())
	}
	restore, events, why, err := m.canRevert(ctx, tx, u, a, ij.open)
	if err != nil {
		return end(StateRefused, err.Error())
	}
	if why != "" {
		return end(StateRefused, why)
	}
	// The events of what the revert undoes, which the records going back
	// to staging must not say: the adoption's of the originals gone before,
	// the retire job's of the originals it puts back.
	for _, dir := range events {
		if err := os.RemoveAll(dir); err != nil {
			return end(StateFailed, fmt.Sprintf("the event %s cannot be deleted: %v", dir, err))
		}
		_ = os.Remove(filepath.Dir(dir)) // events/, when it was the last
	}
	if un := unprojectionOf(a.entries); un != nil {
		dir := u.ItemDir
		if _, err := os.Stat(dir); err != nil {
			dir = u.StagedDir // its publish reverted by a revert a crash stopped
		}
		if err := unproject(dir, un); err != nil {
			return end(StateFailed, "its staged metadata.json cannot be put back: "+err.Error())
		}
	}
	for _, r := range restore {
		if err := renameInto(r.From, r.To); err != nil {
			return end(StateFailed, fmt.Sprintf("the original %s cannot be put back from the trash: %v", r.To, err))
		}
		_ = os.Remove(filepath.Dir(r.From)) // the trash's folder, when it was the last
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpMove, Kind: r.Kind, From: r.From, To: r.To, State: StateRestored}); err != nil {
			return end(StateFailed, err.Error())
		}
	}
	moves := a.moves(StateDone)
	for i := len(moves) - 1; i >= 0; i-- {
		mv := moves[i]
		_, toErr := os.Lstat(mv.To)
		_, fromErr := os.Lstat(mv.From)
		if toErr != nil && fromErr == nil {
			continue // reverted by a revert a crash stopped
		}
		if err := renameInto(mv.To, mv.From); err != nil {
			return end(StateFailed, fmt.Sprintf("the %s move of %s back to %s: %v; revert again once it is put right",
				mv.Kind, mv.To, mv.From, err))
		}
		if err := j.Add(JournalEntry{ItemID: u.ItemID, Op: OpMove, Kind: mv.Kind, From: mv.To, To: mv.From, State: StateReverted}); err != nil {
			return end(StateFailed, err.Error())
		}
	}
	if err := m.restore(ctx, tx, u, &before); err != nil {
		return end(StateFailed, "the database: "+err.Error()+"; revert again")
	}
	if err := tx.Commit(ctx); err != nil {
		return end(StateFailed, "the database: "+err.Error()+"; revert again")
	}
	_ = j.Add(JournalEntry{ItemID: u.ItemID, Op: OpDB, State: StateReverted})
	var back []Move
	for _, e := range moves {
		back = append(back, Move{Kind: e.Kind, From: e.From, To: e.To})
	}
	m.prune(back, true)
	return end(StateReverted, "")
}

// canRevert says why the adoption a of u cannot be reverted, "" when it
// can, and the originals the retire job deleted since that come back from
// the trash first. open are the journal's entries of a revert a crash
// stopped, which it goes on with.
func (m *Migration) canRevert(ctx context.Context, tx pgx.Tx, u *Unit, a *attempt, open []JournalEntry) ([]Move, []string, string, error) {
	reverting := len(open) > 0
	var versions []string
	rows, err := tx.Query(ctx, `SELECT id || ':' || state FROM com_nalet_katalog_itemversions WHERE item_id = $1 ORDER BY id`,
		u.ItemID)
	if err != nil {
		return nil, nil, "", err
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, nil, "", err
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	var want []string
	for _, v := range u.DB.Versions {
		state := VersionComplete
		if v.PackageID == "" {
			state = VersionTaken
		}
		want = append(want, v.VersionID+":"+state)
	}
	sort.Strings(want)
	if !slices.Equal(versions, want) && !(reverting && len(versions) == 0) {
		return nil, nil, fmt.Sprintf("the item changed since it was adopted: its versions are %v, and the adoption made %v",
			versions, want), nil
	}
	sources, err := querySources(ctx, tx, `SELECT `+sourceCols+` FROM com_nalet_katalog_itemsources WHERE item_id = $1 ORDER BY id`,
		u.ItemID)
	if err != nil {
		return nil, nil, "", err
	}
	planned := map[string]UnitSource{}
	for _, s := range u.DB.Sources {
		planned[s.SourceID] = s
	}
	var restore []Move
	var events []string
	event := func(id string) {
		if dir, err := FindEvent(u.ItemDir, id, EventOriginalDeleted); err == nil && dir != "" {
			events = append(events, dir)
		}
	}
	for _, s := range sources {
		p, ok := planned[s.ID]
		switch {
		case !ok:
			return nil, nil, "the item was given another file since it was adopted", nil
		case s.State == SourceRetiring:
			return nil, nil, "its original is being retired: revert once the retire job is done with it", nil
		case s.State == SourceDeleted && p.ArrivalPath == nil:
			// gone before the library: the adoption's own event goes
			event(IDOf("original-deleted:" + s.ID))
		case s.State == SourceDeleted && p.ArrivalPath != nil:
			// retired since: its files come back from the trash, where
			// the adoption put them (its original into its version's
			// folder, or to the arrivals; the files that came with it
			// from beside it), and the event of a deletion undone goes
			if s.RetireEventID != nil {
				event(*s.RetireEventID)
			}
			trash := deref(s.TrashPath)
			staged := filepath.Join(m.Dir, "staged", u.ItemID, "item")
			arrival := filepath.Clean(*p.ArrivalPath)
			beside := ""
			for _, mv := range a.moves(StateDone) {
				if mv.Kind == MoveOriginal && published(u, staged, mv.To) == arrival {
					beside = filepath.Dir(mv.From)
				}
			}
			for _, mv := range a.moves(StateDone) {
				to := published(u, staged, mv.To)
				original := mv.Kind == MoveOriginal && to == arrival
				if !original && (mv.Kind != MoveSidecar || filepath.Dir(mv.From) != beside) {
					continue
				}
				if _, err := os.Lstat(to); err == nil {
					continue
				}
				in := filepath.Join(trash, filepath.Base(to))
				if trash == "" || !statOK(in) {
					if original {
						return nil, nil, fmt.Sprintf("its original %s was deleted for good: the trash's grace is over", to), nil
					}
					continue // a sidecar gone with it: its copy is in the record that goes back to staging
				}
				restore = append(restore, Move{Kind: mv.Kind, From: in, To: to})
			}
		}
	}
	var newer bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemextras WHERE item_id = $1
		AND recordedat IS NOT NULL AND NOT (id = ANY($2)))`, u.ItemID, unitExtras(u)).Scan(&newer); err != nil {
		return nil, nil, "", err
	}
	if newer {
		return nil, nil, "an extra was recorded in its folder since it was adopted", nil
	}
	for _, x := range u.DB.Extras {
		var path *string
		var deleted *time.Time
		if err := tx.QueryRow(ctx, `SELECT sourcepath, sourcedeletedat FROM com_nalet_katalog_itemextras WHERE id = $1`, x.ID).
			Scan(&path, &deleted); err != nil {
			return nil, nil, "", err
		}
		if deleted == nil || x.SourcePath == nil {
			continue
		}
		found, _ := filepath.Glob(filepath.Join(m.p.Work, WorkTrash, "*", "extra-"+x.ID, filepath.Base(*x.SourcePath)))
		if len(found) == 0 {
			return nil, nil, fmt.Sprintf("the original of extra %s was deleted for good: the trash's grace is over", x.ID), nil
		}
		restore = append(restore, Move{Kind: MoveOriginal, From: found[0], To: *x.SourcePath})
	}
	if !reverting {
		if _, err := os.Stat(u.ItemDir); err != nil {
			return nil, nil, fmt.Sprintf("its folder %s is not there", u.ItemDir), nil
		}
	}
	if u.Type == "series" {
		if eps, _ := os.ReadDir(filepath.Join(u.ItemDir, "episodes")); len(eps) > 0 {
			return nil, nil, "its episodes are in the library: revert them first", nil
		}
	}
	return restore, events, "", nil
}

func unitExtras(u *Unit) []string {
	out := []string{}
	for _, x := range u.DB.Extras {
		out = append(out, x.ID)
	}
	return out
}

// restore puts the database back as before says it was, in tx.
func (m *Migration) restore(ctx context.Context, tx pgx.Tx, u *Unit, before *unitBefore) error {
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemversions WHERE id = ANY($1)`, before.Versions); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemsources WHERE id = ANY($1)`, before.NewIDs); err != nil {
		return err
	}
	for _, s := range before.Sources {
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itemsources
			SELECT * FROM jsonb_populate_record(NULL::com_nalet_katalog_itemsources, $1::jsonb)`, string(s)); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_playbackassets WHERE item_id = $1`, u.ItemID); err != nil {
		return err
	}
	for _, a := range before.Assets {
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_playbackassets
			SELECT * FROM jsonb_populate_record(NULL::com_nalet_katalog_playbackassets, $1::jsonb)`, string(a)); err != nil {
			return err
		}
	}
	for _, s := range before.Subtitles {
		if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_subtitleassets WHERE id = ($1::jsonb)->>'id'`, string(s)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_subtitleassets
			SELECT * FROM jsonb_populate_record(NULL::com_nalet_katalog_subtitleassets, $1::jsonb)`, string(s)); err != nil {
			return err
		}
	}
	for _, x := range before.Extras {
		if _, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemextras x SET packagepath = s.packagepath, recordpath = s.recordpath,
				packageid = s.packageid, recordedat = s.recordedat, sourcepath = s.sourcepath, sourcedeletedat = s.sourcedeletedat,
				modifiedat = s.modifiedat
			FROM jsonb_populate_record(NULL::com_nalet_katalog_itemextras, $1::jsonb) s WHERE x.id = s.id`, string(x)); err != nil {
			return err
		}
	}
	// The episodes its file covers, linked as they were: one the adoption
	// linked is unlinked, saying so, and one linked otherwise since is left.
	for _, c := range before.Covered {
		var was struct {
			ID        string  `json:"id"`
			CoveredBy *string `json:"coveredby"`
		}
		if err := json.Unmarshal(c, &was); err != nil {
			return err
		}
		now, err := HolderOf(ctx, tx, was.ID)
		switch {
		case err != nil:
			return err
		case now != u.ItemID:
		case was.CoveredBy == nil:
			if _, err := Unlink(ctx, tx, []string{was.ID}, "covers it no more: its adoption was reverted"); err != nil {
				return err
			}
		case *was.CoveredBy != u.ItemID:
			if _, err := Link(ctx, tx, *was.CoveredBy, was.ID); err != nil {
				return err
			}
		}
	}
	// Last, after the extras and the episodes it covers (whose change marks
	// the item changed): the item as it was, its modifiedat too.
	_, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_items i SET recordedat = s.recordedat, libraryprojectedat = s.libraryprojectedat,
			modifiedat = s.modifiedat
		FROM jsonb_populate_record(NULL::com_nalet_katalog_items, $2::jsonb) s WHERE i.id = $1`, u.ItemID, string(before.Item))
	return err
}

// revertPeople puts the people an adopt put in place back into staging, and
// what their projection reflected as it was.
func (m *Migration) revertPeople(ctx context.Context, j *Journal, items map[string]*itemJournal) (int, error) {
	n := 0
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := items[id].adoption()
		if a == nil || a.unit.Kind != "person" {
			continue
		}
		for _, mv := range a.moves(StateDone) {
			if _, err := os.Lstat(mv.To); err != nil {
				continue
			}
			if err := renameInto(mv.To, mv.From); err != nil {
				return n, fmt.Errorf("person %s: %w", id, err)
			}
			if err := j.Add(JournalEntry{ItemID: id, Op: OpMove, Kind: "person", From: mv.To, To: mv.From, State: StateReverted}); err != nil {
				return n, err
			}
			if len(mv.Before) > 0 {
				if _, err := m.pool.Exec(ctx, `UPDATE com_nalet_katalog_people SET libraryprojectedat = ($2::jsonb->>'libraryprojectedat')::timestamptz
					WHERE id = $1`, id, string(mv.Before)); err != nil {
					return n, fmt.Errorf("person %s: %w", id, err)
				}
			}
		}
		if err := j.Add(JournalEntry{ItemID: id, Op: OpUnit, Kind: "person", State: StateReverted}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
