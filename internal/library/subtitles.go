package library

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// SubtitlesPlan is the plan a migration's subtitles phase leaves in the run's
// folder: a row for each subtitle of a package the library record holds and
// the catalog has none for (the schemas' library-v2-rebuild.py compare, a
// title's subtitle and its row being one by language and file name), each as
// the catalog writes a package's rows.
const SubtitlesPlan = "subtitles-plan.json"

// SubtitlesJournal is what Subtitles did, a line a row of the plan.
const SubtitlesJournal = "subtitles.jsonl"

// SubtitlesReport is what Subtitles answers: how many rows it wrote, how many
// the catalog held already (written before, or by the packager since), and
// the rows it left, why.
type SubtitlesReport struct {
	Run       string          `json:"run"`
	Rows      int             `json:"rows"`
	Unchanged int             `json:"unchanged"`
	Skipped   []SubtitlesSkip `json:"skipped"`
	Stopped   string          `json:"stopped,omitempty"`
}

// SubtitlesSkip is a row of the plan Subtitles left.
type SubtitlesSkip struct {
	ItemID string `json:"itemId"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// LabelWidth is how many characters the catalog keeps of a subtitle's label
// (com_nalet_katalog_subtitleassets.label).
const LabelWidth = 120

// FitLabel is a subtitle's label as the catalog keeps it: cut to LabelWidth
// characters, the last an ellipsis, when it is longer — a commentary's title
// can be a sentence or two — rather than refused with the rest of its rows.
func FitLabel(label *string) *string {
	if label == nil {
		return nil
	}
	r := []rune(*label)
	if len(r) <= LabelWidth {
		return label
	}
	cut := string(r[:LabelWidth-1]) + "…"
	return &cut
}

// subtitleRow is a row of the plan: what the catalog's row is to hold.
type subtitleRow struct {
	ItemID    string  `json:"itemId"`
	Path      string  `json:"path"`
	Format    *string `json:"format"`
	Lang      *string `json:"lang"`
	Label     *string `json:"label"`
	IsDefault bool    `json:"isdefault"`
	IsForced  bool    `json:"isforced"`
}

// packageSubtitle is what a package.json says of one of its subtitles.
type packageSubtitle struct {
	Path        string  `json:"path"`
	Format      *string `json:"format"`
	Language    *string `json:"language"`
	Name        *string `json:"name"`
	Title       *string `json:"title"`
	Forced      bool    `json:"forced"`
	FromSidecar *string `json:"fromSidecar"`
}

// Subtitles writes the rows of the run's subtitles plan the catalog still
// lacks, each as the catalog writes a package's when it takes the package:
// the path in the version's folder, the format and language the package
// gives, its name (or else its title) as the label, never the default,
// forced as the package says (a label cut to the catalog's width, FitLabel).
// Each row is looked at again first, in the
// transaction of its item under the lock every writer of the item's versions
// takes: its version is the one the catalog keeps complete in that folder, the
// package lists the subtitle there (not one made of a sidecar, whose own row
// stands for it) and says of it what the plan says, the file is there, and no
// row of the item names that file or one of that language and file name.
// Anything else leaves the row, with why; a row the catalog holds already is
// counted unchanged, so calling it again writes nothing. Each item is one
// transaction; what became of each row goes to the run's subtitles journal.
// only narrows it to those items. A run with no plan writes nothing.
func (m *Migration) Subtitles(ctx context.Context, only []string) (SubtitlesReport, error) {
	rep := SubtitlesReport{Run: m.Run, Skipped: []SubtitlesSkip{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	raw, err := os.ReadFile(filepath.Join(m.Dir, SubtitlesPlan))
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil // no plan was made for this run: nothing to write
	}
	if err != nil {
		return rep, err
	}
	var plan struct {
		Rows []subtitleRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return rep, errors.New(SubtitlesPlan + " is no plan: " + err.Error())
	}
	byItem := map[string][]subtitleRow{}
	for _, r := range plan.Rows {
		if len(only) > 0 && !slices.Contains(only, r.ItemID) {
			continue
		}
		byItem[r.ItemID] = append(byItem[r.ItemID], r)
	}
	items := make([]string, 0, len(byItem))
	for id := range byItem {
		items = append(items, id)
	}
	sort.Strings(items)
	j, err := OpenJournal(filepath.Join(m.Dir, SubtitlesJournal), m.now)
	if err != nil {
		return rep, err
	}
	defer j.Close()
	for _, id := range items {
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the items left are not written"
			return rep, nil
		}
		for _, o := range m.writeSubtitles(context.WithoutCancel(ctx), id, byItem[id]) {
			switch o.state {
			case "written":
				rep.Rows++
			case "unchanged":
				rep.Unchanged++
			default:
				rep.Skipped = append(rep.Skipped, SubtitlesSkip{ItemID: id, Path: m.relOf(o.path), Reason: o.reason})
			}
			if err := j.Add(JournalEntry{ItemID: id, Op: "subtitle", To: m.relOf(o.path), State: o.state, Reason: o.reason}); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// subtitleOutcome is what became of a row: written, unchanged or skipped.
type subtitleOutcome struct {
	path, state, reason string
}

// writeSubtitles writes one item's rows of the plan in one transaction and
// answers what became of each. When the database fails, the transaction is
// rolled back: a row it had written, or had not looked at yet, is skipped as
// failed, and one it found held or left keeps that.
func (m *Migration) writeSubtitles(ctx context.Context, itemID string, rows []subtitleRow) []subtitleOutcome {
	out := make([]subtitleOutcome, len(rows))
	for i, r := range rows {
		out[i] = subtitleOutcome{path: r.Path}
	}
	failed := func(err error) []subtitleOutcome {
		for i := range out {
			if out[i].state == "" || out[i].state == "written" {
				out[i].state, out[i].reason = "skipped", "failed: "+err.Error()
			}
		}
		return out
	}
	if !ValidID(itemID) {
		for i := range out {
			out[i].state, out[i].reason = "skipped", "the plan names no item by its id"
		}
		return out
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return failed(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if err := LockItem(ctx, tx, itemID); err != nil {
		return failed(err)
	}
	written := 0
	for i, r := range rows {
		state, why, err := m.writeSubtitle(ctx, tx, itemID, r)
		if err != nil {
			return failed(err)
		}
		out[i].state, out[i].reason = state, why
		if state == "written" {
			written++
		}
	}
	if written > 0 {
		if err := tx.Commit(ctx); err != nil {
			return failed(err)
		}
	}
	return out
}

// writeSubtitle looks at one row again and writes it: written, unchanged
// (the catalog holds it), or skipped with why.
func (m *Migration) writeSubtitle(ctx context.Context, tx pgx.Tx, itemID string, r subtitleRow) (string, string, error) {
	path := r.Path
	if !filepath.IsAbs(path) {
		path = m.inRoot(path)
	}
	path = filepath.Clean(path)
	if !Within(m.p.Root, path) {
		return "skipped", r.Path + " lies outside the library", nil
	}
	vdir := ""
	for d := filepath.Dir(path); Within(m.p.Root, d) && d != m.p.Root; d = filepath.Dir(d) {
		if filepath.Base(filepath.Dir(d)) == "versions" {
			vdir = d
			break
		}
	}
	if vdir == "" || !ValidID(filepath.Base(vdir)) {
		return "skipped", "it lies in no version's folder: the scanner writes a sidecar's row, not this", nil
	}
	rel, err := filepath.Rel(vdir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "skipped", "it lies in no version's folder", nil
	}
	var state string
	var keptIn *string
	err = tx.QueryRow(ctx, `SELECT state, dir FROM com_nalet_katalog_itemversions WHERE id = $1 AND item_id = $2`,
		filepath.Base(vdir), itemID).Scan(&state, &keptIn)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "skipped", "the catalog holds no such version of the item", nil
	case err != nil:
		return "", "", err
	case state != VersionComplete:
		return "skipped", "the catalog holds the version as " + state + ", not complete", nil
	case keptIn == nil || filepath.Clean(*keptIn) != vdir:
		return "skipped", "the catalog keeps the version in " + m.relOf(deref(keptIn)) + ", not here", nil
	}
	b, err := os.ReadFile(filepath.Join(vdir, PackageFile))
	if err != nil {
		return "skipped", "its package.json cannot be read: " + err.Error(), nil
	}
	var pkg struct {
		Subtitles []packageSubtitle `json:"subtitles"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "skipped", "its package.json cannot be read: " + err.Error(), nil
	}
	i := slices.IndexFunc(pkg.Subtitles, func(s packageSubtitle) bool { return filepath.Clean(filepath.FromSlash(s.Path)) == rel })
	if i < 0 {
		return "skipped", "its package lists no subtitle at " + filepath.ToSlash(rel), nil
	}
	s := pkg.Subtitles[i]
	if s.FromSidecar != nil && *s.FromSidecar != "" {
		return "skipped", "it is made of a sidecar, whose own row stands for it", nil
	}
	// what the catalog writes of a package's subtitle (writePackaged)
	label := s.Name
	if label == nil || strings.TrimSpace(*label) == "" {
		label = s.Title
	}
	if deref(s.Format) != deref(r.Format) || deref(s.Language) != deref(r.Lang) || deref(label) != deref(r.Label) ||
		s.Forced != r.IsForced || r.IsDefault {
		return "skipped", "its package says otherwise of it than the plan: make the plan again", nil
	}
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return "skipped", "its file is not there", nil
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
			AND (path = $2 OR (lang IS NOT DISTINCT FROM $3 AND regexp_replace(path, '^.*/', '') = $4))`,
		itemID, path, s.Language, filepath.Base(path)).Scan(&held); err != nil {
		return "", "", err
	}
	if held > 0 {
		return "unchanged", "", nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_subtitleassets
			(id, item_id, path, format, lang, label, isdefault, isforced)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, false, $6)`,
		itemID, path, s.Format, s.Language, FitLabel(label), s.Forced); err != nil {
		return "", "", err
	}
	return "written", "", nil
}
