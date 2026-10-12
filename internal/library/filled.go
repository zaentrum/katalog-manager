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

// ChecksumsJournal is the journal of a run of the schemas'
// library-v2-fill-checksums.py, in the run's folder: a fill line before a
// folder's package.json is written again, a filled line once its chain is
// closed.
const ChecksumsJournal = "checksums.jsonl"

// FilledSkip is a version the journal says was filled that the catalog did
// not note, and why.
type FilledSkip struct {
	ItemID    string `json:"itemId"`
	VersionID string `json:"versionId"`
	Reason    string `json:"reason"`
}

// FilledReport is what Filled did: the versions it noted verified in full,
// those it had noted already or that were verified since, and those it
// left, each with why.
type FilledReport struct {
	Run       string       `json:"run"`
	Versions  int          `json:"versions"`
	Unchanged int          `json:"unchanged"`
	Skipped   []FilledSkip `json:"skipped"`
	Stopped   string       `json:"stopped,omitempty"`
}

// filledLine is a line of the checksums journal.
type filledLine struct {
	At        string `json:"at"`
	Op        string `json:"op"`
	Dir       string `json:"dir"`
	After     string `json:"after"`
	ItemID    string `json:"itemId"`
	VersionID string `json:"versionId"`
	PackageID string `json:"packageId"`
}

// filledVersion is a version the journal says was filled: where, when, and
// the package.json the tool closed its chain over.
type filledVersion struct {
	filledLine
	complete string // "sha256:<hex>" of the package.json it wrote
}

// readFilled reads the versions a checksums journal says were filled, each
// once, as its last filled line has it. A line a crash cut short is not one.
func readFilled(path string) ([]filledVersion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	wrote := map[string]string{} // dir → the package.json its last fill line wrote
	byVersion := map[string]filledVersion{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var l filledLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Op {
		case "fill":
			wrote[l.Dir] = l.After
		case "filled":
			if l.VersionID != "" {
				byVersion[l.VersionID] = filledVersion{filledLine: l, complete: wrote[l.Dir]}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	out := make([]filledVersion, 0, len(byVersion))
	for _, v := range byVersion {
		out = append(out, v)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Dir < out[k].Dir })
	return out, nil
}

// Filled is the catalog's side of the schemas' library-v2-fill-checksums.py,
// which closes the chains of the packages a migration adopted with their
// checksums pending and changes no database: by the run's checksums journal,
// every version it filled is noted verified in full at the moment it was —
// the tool hashed every file of its package to write its checksums — so the
// retire job does not read them all again within library.verify.maxAge.
//
// A version is noted only while it is what the tool left: the catalog keeps
// it, complete and of that package, in the folder the journal names, and its
// chain holds from the package.json the tool wrote. Any other is skipped,
// with why. Calling it again changes nothing: a version noted already, or
// verified since, is counted unchanged. only narrows it to those items. A
// run whose folder holds no such journal notes nothing.
func (m *Migration) Filled(ctx context.Context, only []string) (FilledReport, error) {
	rep := FilledReport{Run: m.Run, Skipped: []FilledSkip{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	filled, err := readFilled(filepath.Join(m.Dir, ChecksumsJournal))
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil // the tool filled nothing in this run: nothing to note
	}
	if err != nil {
		return rep, err
	}
	for _, v := range filled {
		if len(only) > 0 && !slices.Contains(only, v.ItemID) {
			continue
		}
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the versions left are not noted"
			return rep, nil
		}
		noted, why, err := m.noteFilled(context.WithoutCancel(ctx), v)
		switch {
		case err != nil:
			rep.Skipped = append(rep.Skipped, FilledSkip{ItemID: v.ItemID, VersionID: v.VersionID, Reason: "failed: " + err.Error()})
		case why != "":
			rep.Skipped = append(rep.Skipped, FilledSkip{ItemID: v.ItemID, VersionID: v.VersionID, Reason: why})
		case noted:
			rep.Versions++
		default:
			rep.Unchanged++
		}
	}
	return rep, nil
}

// noteFilled notes one version the journal says was filled: whether its row
// changed, or why it was left.
func (m *Migration) noteFilled(ctx context.Context, v filledVersion) (bool, string, error) {
	if !ValidID(v.ItemID) || !ValidID(v.VersionID) {
		return false, "the journal names no item and version by their ids", nil
	}
	at, err := time.Parse(time.RFC3339, v.At)
	if err != nil {
		return false, "the journal does not say when it was filled", nil
	}
	dir := m.inRoot(v.Dir)
	if !Within(m.p.Root, dir) {
		return false, v.Dir + " lies outside the library", nil
	}
	var state string
	var keptIn, pkg *string
	err = m.pool.QueryRow(ctx, `SELECT state, dir, packageid FROM com_nalet_katalog_itemversions WHERE id = $1 AND item_id = $2`,
		v.VersionID, v.ItemID).Scan(&state, &keptIn, &pkg)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, "the catalog holds no such version of the item", nil
	case err != nil:
		return false, "", err
	case state != VersionComplete:
		return false, "the catalog holds the version as " + state + ", not complete", nil
	case keptIn == nil || filepath.Clean(*keptIn) != dir:
		return false, fmt.Sprintf("the catalog keeps the version in %s, not in %s", m.relOf(deref(keptIn)), v.Dir), nil
	case deref(pkg) != v.PackageID:
		return false, "the catalog holds another package of the version than the one that was filled", nil
	}
	c, err := VerifyVersion(dir, false)
	switch {
	case err != nil:
		return false, "its chain does not hold: " + err.Error(), nil
	case c.Pending:
		return false, "its checksums are pending still", nil
	case v.complete == "" || c.Complete != v.complete:
		return false, "its package.json is not the one the run wrote: it changed since it was filled", nil
	}
	tag, err := m.pool.Exec(ctx, `UPDATE com_nalet_katalog_itemversions SET verifiedat = $3::timestamptz, verifiedlevel = $4,
			modifiedat = now()
		WHERE id = $1 AND item_id = $2 AND state = 'complete' AND (verifiedat IS NULL OR verifiedat < $3::timestamptz)`,
		v.VersionID, v.ItemID, at, VerifyFull)
	if err != nil {
		return false, "", err
	}
	return tag.RowsAffected() == 1, "", nil
}
