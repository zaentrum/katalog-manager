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
	"time"

	"github.com/jackc/pgx/v5"
)

// MendsJournal is the journal of a run of the schemas' library-v2-mend.py,
// in the run's folder: a mend (undo) line before a version's files are
// replaced, a mended (undone) line once they are, with the .complete it left.
const MendsJournal = "mends.jsonl"

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

// readMended reads the versions a mends journal says were mended or put
// back, each once, as its last line has it; a version whose last line is a
// mend or an undo the run did not finish is left out (the tool finishes it
// first). A line a crash cut short is not one.
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
		if l.Op == "mended" || l.Op == "undone" {
			out = append(out, l)
		}
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
// passes. Any other is skipped, with why. Calling it again changes nothing:
// a version verified in full since the step is counted unchanged. only
// narrows it to those items. A run whose folder holds no such journal notes
// nothing.
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
	for _, l := range lines {
		if len(only) > 0 && !slices.Contains(only, l.ItemID) {
			continue
		}
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the versions left are not noted"
			return rep, nil
		}
		noted, why, err := m.noteMended(context.WithoutCancel(ctx), l)
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
func (m *Migration) noteMended(ctx context.Context, l mendedLine) (bool, string, error) {
	if !ValidID(l.ItemID) || !ValidID(l.VersionID) {
		return false, "the journal names no item and version by their ids", nil
	}
	at, err := time.Parse(time.RFC3339, l.At)
	if err != nil {
		return false, "the journal does not say when it was " + l.Op, nil
	}
	dir := m.inRoot(l.Dir)
	if !Within(m.p.Root, dir) {
		return false, l.Dir + " lies outside the library", nil
	}
	var state string
	var keptIn, pkg, level *string
	var verified *time.Time
	err = m.pool.QueryRow(ctx, `SELECT state, dir, packageid, verifiedlevel, verifiedat FROM com_nalet_katalog_itemversions
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
	case deref(level) == VerifyFull && verified != nil && !verified.Before(at):
		return false, "", nil // verified in full since the step: nothing to note
	}
	c, err := VerifyVersion(dir, true)
	switch {
	case err != nil:
		return false, "its chain does not hold: " + err.Error(), nil
	case c.Pending:
		return false, "its checksums are pending", nil
	case l.Complete == "" || c.Complete != l.Complete:
		return false, "its package.json is not the one the run left: it changed since it was " + l.Op, nil
	}
	tag, err := m.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = now(), verifiedlevel = $3,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND state = 'complete'`, l.VersionID, l.ItemID, VerifyFull)
	if err != nil {
		return false, "", err
	}
	return tag.RowsAffected() == 1, "", nil
}
