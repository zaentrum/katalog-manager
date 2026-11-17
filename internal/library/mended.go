package library

import (
	"bufio"
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
)

// MendsJournal is the journal of a run of the schemas' library-v2-mend.py,
// in the run's folder: a mend (undo) line before a version's files are
// replaced, a mended (undone) line once they are, with the .complete it left.
const MendsJournal = "mends.jsonl"

// MendingSuffix ends the name of the file library-v2-mend.py writes a
// file's new bytes to beside it before it renames it into place: one in a
// package's folder is a mend stopped mid-way, or one under way.
const MendingSuffix = ".mending"

// MendsNoted is the catalog's own record, in the run's folder beside its
// journal, of the versions it noted from it: by folder, the .complete the
// full verification it noted held and the verifiedat it wrote then (the
// database's clock). It decides "noted already" on the bytes, not on the
// moments two clocks gave.
const MendsNoted = "mends-noted.json"

// mendNote is one version's entry in MendsNoted.
type mendNote struct {
	Complete   string    `json:"complete"`
	VerifiedAt time.Time `json:"verifiedAt"`
}

// readNotes reads the run's MendsNoted; none is no notes.
func (m *Migration) readNotes() map[string]mendNote {
	out := map[string]mendNote{}
	if b, err := os.ReadFile(filepath.Join(m.Dir, MendsNoted)); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// writeNotes writes the run's MendsNoted whole, in one step.
func (m *Migration) writeNotes(notes map[string]mendNote) error {
	b, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(m.Dir, MendsNoted)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// mendedLine is a line of the mends journal.
type mendedLine struct {
	At        string `json:"at"`
	Op        string `json:"op"`
	Dir       string `json:"dir"`
	ItemID    string `json:"itemId"`
	VersionID string `json:"versionId"`
	PackageID string `json:"packageId"`
	Complete  string `json:"complete"` // "sha256:<hex>" of the package.json the step left
}

// readMended reads the versions a mends journal names, each once, as its
// last line has it: mended or undone where the run finished the step, mend
// or undo where it stopped mid-way (the tool finishes it on its next run of
// the same name). A line a crash cut short is not one.
func readMended(path string) ([]mendedLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	last := map[string]mendedLine{} // dir → its last line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var l mendedLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Op {
		case "mend", "mended", "undo", "undone":
			last[l.Dir] = l
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]mendedLine, 0, len(last))
	for _, l := range last {
		out = append(out, l)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Dir < out[k].Dir })
	return out, nil
}

// Mended is the catalog's side of the schemas' library-v2-mend.py, which
// changes boxes of a version's init segments in place, closes the chain over
// them again (the files' lines in checksums.sha256, package.json with its
// mends, .complete) and changes no database: by the run's mends journal,
// every version it mended or put back is verified again, in full — its
// mends held to the checksums — and noted verified at that moment,
// so no verification of the version from before the change stands for the
// bytes it has now (the retire job reads it before an original goes).
//
// A version is noted only while it is what the tool left: the catalog keeps
// it, complete and of that package, in the folder the journal names, its
// chain holds from the package.json the tool wrote, and a full verification
// passes. Any other is skipped, with why; one the run left mid-way (its last
// line a mend or an undo), and one whose full verification fails, has the
// verification the catalog noted for it taken away too (verifiedat and
// verifiedlevel cleared), as it stood for bytes the version no longer has
// — the retire job then verifies it in full before an original goes, and
// refuses it while it does not hold. Calling it again changes nothing: a
// version this call's run noted already, whose .complete is still the one
// the noted verification held and whose row nothing wrote since, is counted
// unchanged (MendsNoted) — decided on the bytes, so no verification of the
// bytes from before the step counts, whatever the clocks of the tool's host
// and the database. only narrows it to those items. A run whose folder
// holds no such journal notes nothing.
func (m *Migration) Mended(ctx context.Context, only []string) (FilledReport, error) {
	rep := FilledReport{Run: m.Run, Skipped: []FilledSkip{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	lines, err := readMended(filepath.Join(m.Dir, MendsJournal))
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil // the tool mended nothing in this run: nothing to note
	}
	if err != nil {
		return rep, err
	}
	notes := m.readNotes()
	for _, l := range lines {
		if len(only) > 0 && !slices.Contains(only, l.ItemID) {
			continue
		}
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the versions left are not noted"
			return rep, nil
		}
		noted, why, err := m.noteMended(context.WithoutCancel(ctx), l, notes)
		switch {
		case err != nil:
			rep.Skipped = append(rep.Skipped, FilledSkip{ItemID: l.ItemID, VersionID: l.VersionID, Reason: "failed: " + err.Error()})
		case why != "":
			rep.Skipped = append(rep.Skipped, FilledSkip{ItemID: l.ItemID, VersionID: l.VersionID, Reason: why})
		case noted:
			rep.Versions++
		default:
			rep.Unchanged++
		}
	}
	return rep, nil
}

// noteMended verifies and notes one version the journal says was mended or
// put back: whether its row changed, or why it was left.
func (m *Migration) noteMended(ctx context.Context, l mendedLine, notes map[string]mendNote) (bool, string, error) {
	if !ValidID(l.ItemID) || !ValidID(l.VersionID) {
		return false, "the journal names no item and version by their ids", nil
	}
	if _, err := time.Parse(time.RFC3339, l.At); err != nil {
		return false, "the journal does not say when it was " + l.Op, nil
	}
	dir := m.inRoot(l.Dir)
	if !Within(m.p.Root, dir) {
		return false, l.Dir + " lies outside the library", nil
	}
	var state string
	var keptIn, pkg, level *string
	var verified *time.Time
	err := m.pool.QueryRow(ctx, `SELECT state, dir, packageid, verifiedlevel, verifiedat FROM com_nalet_katalog_itemversions
		WHERE id = $1 AND item_id = $2`, l.VersionID, l.ItemID).Scan(&state, &keptIn, &pkg, &level, &verified)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, "the catalog holds no such version of the item", nil
	case err != nil:
		return false, "", err
	case state != VersionComplete:
		return false, "the catalog holds the version as " + state + ", not complete", nil
	case keptIn == nil || filepath.Clean(*keptIn) != dir:
		return false, fmt.Sprintf("the catalog keeps the version in %s, not in %s", m.relOf(deref(keptIn)), l.Dir), nil
	case deref(pkg) != l.PackageID:
		return false, "the catalog holds another package of the version than the one that was " + l.Op, nil
	case l.Op == "mend" || l.Op == "undo":
		why := "the run stopped mid-way through a " + l.Op + " of it: the tool finishes it on its next run of the same name"
		return m.unverified(ctx, l, why)
	}
	if n, ok := notes[l.Dir]; ok && n.Complete == l.Complete && deref(level) == VerifyFull && verified != nil &&
		verified.Equal(n.VerifiedAt) {
		if mark, err := os.ReadFile(filepath.Join(dir, CompleteFile)); err == nil && strings.TrimSpace(string(mark)) == l.Complete {
			return false, "", nil // noted already, of these bytes, and nothing wrote the row since
		}
	}
	c, err := VerifyVersion(dir, true)
	switch {
	case err != nil:
		return m.unverified(ctx, l, "its chain does not hold: "+err.Error())
	case c.Pending:
		return false, "its checksums are pending", nil
	case l.Complete == "" || c.Complete != l.Complete:
		return false, "its package.json is not the one the run left: it changed since it was " + l.Op, nil
	}
	var at time.Time
	err = m.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = $3,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND state = 'complete' RETURNING verifiedat`, l.VersionID, l.ItemID, VerifyFull).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	notes[l.Dir] = mendNote{Complete: c.Complete, VerifiedAt: at}
	if err := m.writeNotes(notes); err != nil {
		return true, "", fmt.Errorf("noted, and its note could not be written (%v): the next call verifies it again", err)
	}
	return true, "", nil
}

// unverified takes away the verification the catalog noted for the version
// of l, which a mend left mid-way or whose bytes no longer hold: why it was
// skipped, said so.
func (m *Migration) unverified(ctx context.Context, l mendedLine, why string) (bool, string, error) {
	tag, err := m.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = NULL, verifiedlevel = NULL,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND (verifiedat IS NOT NULL OR verifiedlevel IS NOT NULL)`, l.VersionID, l.ItemID)
	if err != nil {
		return false, "", err
	}
	if tag.RowsAffected() == 1 {
		why += "; the verification noted for it is taken away"
	}
	return false, why, nil
}
