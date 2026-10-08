package library

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// The neutral names (the schemas' library-v2-neutral-names.py) give a tree
// written before 2026-10-08 the names the library gives its files: an
// original kept in a version's or an extra's folder becomes original.<ext>,
// the copy of a subtitle file in its source's record
// subtitle-<n>.<lang>[.forced][.sdh].<ext>, every other copy (NFO text, an
// image) leaves the record for the run's removed/, and the records that
// named them are written again. The tool changes no database. Names points
// the catalog at the files where the tool left them, by the run's journal
// (.work/migration/<run>/journal.jsonl): {seq, at, item, op, …}, an item's
// folder and every path relative to the library's root, first the item's
// plan ({"op": "plan", "steps": [...]}, each step a rename {from, to}, a
// removal {from, to} into the run's folder, or a record written again
// {path}), then a line for each step as it is made, and {"op": "done"} once
// all are.

// ErrNamesRefused says a journal is not one Names takes: it names a file
// outside the folder of its item, or holds a line the neutral names do not
// write. Nothing is changed.
var ErrNamesRefused = errors.New("the journal is refused")

// NamesSkip is an item Names left as it was, and why.
type NamesSkip struct {
	ItemID string `json:"itemId"`
	Reason string `json:"reason"`
}

// NamesReport is what Names did: how many of the journal's items it took,
// how many of the catalog's rows it changed (none when it is called again),
// and the items it left, each with why.
type NamesReport struct {
	Run     string      `json:"run"`
	Items   int         `json:"items"`
	Rows    int         `json:"rows"`
	Skipped []NamesSkip `json:"skipped"`
	Stopped string      `json:"stopped,omitempty"`
}

// namesStep is a step of an item's plan: a rename (from, to), a removal
// (from, and to in the run's folder: removed/<from>), or a record written
// again (path).
type namesStep struct {
	Op   string `json:"op"`
	From string `json:"from"`
	To   string `json:"to"`
	Path string `json:"path"`
}

// namesLine is a line of the journal.
type namesLine struct {
	Item  *string     `json:"item"`
	Op    string      `json:"op"`
	Steps []namesStep `json:"steps"`
	From  string      `json:"from"`
	To    string      `json:"to"`
	Path  string      `json:"path"`
}

// namesItem is what the journal says of an item's folder: the steps whose
// effect is in the tree, in their order (every step of a plan that is done,
// and of one the tool planned again before it was done, those journaled as
// made), and whether its last plan is done.
type namesItem struct {
	rel    string
	made   []namesStep
	plan   []namesStep
	logged []namesStep // journaled as made since its last plan
	done   bool
}

// namesItemRE is an item's folder as the journal names it: a movie's, a
// series', or an episode's in its series' folder.
var namesItemRE = regexp.MustCompile(`^(movies/[^/]+/[^/]+|series/[^/]+/[^/]+(/episodes/[^/]+)?)$`)

// readNamesJournal reads the journal at path by item folder; none when it is
// not there. A line a crash cut short is passed over, as the tool passes
// over it. A journal that names a file outside the folder of its item, or
// holds a line the neutral names do not write, is refused whole.
func readNamesJournal(path string) (map[string]*namesItem, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	items := map[string]*namesItem{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for n := 1; sc.Scan(); n++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var l namesLine
		if err := json.Unmarshal([]byte(text), &l); err != nil {
			continue // a line a crash cut short: its step is found again from the files
		}
		refuse := func(format string, args ...any) error {
			return fmt.Errorf("%w: line %d: %s", ErrNamesRefused, n, fmt.Sprintf(format, args...))
		}
		if l.Item == nil || !namesItemRE.MatchString(*l.Item) || !cleanRel(*l.Item) {
			return nil, refuse("it names no item's folder, as a journal of the neutral names does")
		}
		it := items[*l.Item]
		if it == nil {
			it = &namesItem{rel: *l.Item}
			items[*l.Item] = it
		}
		switch l.Op {
		case "plan":
			for _, s := range l.Steps {
				if why := stepRefusal(it.rel, s); why != "" {
					return nil, refuse("its plan holds %s", why)
				}
			}
			if !it.done {
				it.made = append(it.made, it.logged...) // a plan left: what it made
			}
			it.plan, it.logged, it.done = l.Steps, nil, false
		case "rename", "remove", "rewrite":
			s := namesStep{Op: l.Op, From: l.From, To: l.To, Path: l.Path}
			if why := stepRefusal(it.rel, s); why != "" {
				return nil, refuse("it holds %s", why)
			}
			if it.plan == nil {
				return nil, refuse("a step of %s that no plan holds", it.rel)
			}
			it.logged = append(it.logged, s)
		case "done":
			if it.plan == nil {
				return nil, refuse("%s done with no plan", it.rel)
			}
			if !it.done {
				it.made = append(it.made, it.plan...)
			}
			it.logged, it.done = nil, true
		default:
			return nil, refuse("%q is no line the neutral names write", l.Op)
		}
	}
	return items, sc.Err()
}

// stepRefusal says why the journal may not hold step s for the item whose
// folder is rel: "" when it names files of that folder alone (a series'
// episodes are items of their own), a removal moving its file into the
// run's removed/ under the path it had.
func stepRefusal(rel string, s namesStep) string {
	switch s.Op {
	case "rename":
		if !ownFile(rel, s.From) || !ownFile(rel, s.To) {
			return fmt.Sprintf("a rename of %q to %q, which is not within the folder of %s", s.From, s.To, rel)
		}
	case "remove":
		if !ownFile(rel, s.From) || s.To != "removed/"+s.From {
			return fmt.Sprintf("a removal of %q to %q, which is not from the folder of %s into the run's removed/",
				s.From, s.To, rel)
		}
	case "rewrite":
		if !ownFile(rel, s.Path) {
			return fmt.Sprintf("%q written again, which is not within the folder of %s", s.Path, rel)
		}
	default:
		return fmt.Sprintf("a step %q, which the neutral names do not make", s.Op)
	}
	return ""
}

// ownFile reports whether name, a path relative to the library's root, is a
// file in the folder of the item at rel and in no episode's folder in it.
func ownFile(rel, name string) bool {
	if !cleanRel(name) || !strings.HasPrefix(name, rel+"/") {
		return false
	}
	series := strings.HasPrefix(rel, SeriesDir+"/") && strings.Count(rel, "/") == 2
	return !series || !strings.HasPrefix(name, rel+"/episodes/")
}

// cleanRel reports whether name is a path relative to the library's root
// as the tool writes one: its folders joined by "/", nothing to clean.
func cleanRel(name string) bool {
	return name != "" && !strings.ContainsAny(name, "\\\x00") && !strings.HasPrefix(name, "/") &&
		pathpkg.Clean(name) == name && name != ".." && !strings.HasPrefix(name, "../")
}

// namesColumns are the catalog's columns that name a file in an item's
// folder, of the item's own rows (by item_id), and whether a row's
// modifiedat says when it changed.
var namesColumns = []struct {
	table, column string
	stamped       bool
}{
	{"com_nalet_katalog_playbackassets", "path", false},        // the title's file, its package
	{"com_nalet_katalog_subtitleassets", "path", false},        // a subtitle file, a copy, a rendition
	{"com_nalet_katalog_itemsources", "arrivalpath", true},     // where an original lies
	{"com_nalet_katalog_itemextras", "sourcepath", true},       // an extra's original
	{"com_nalet_katalog_itemdiagnostics", "sourcepath", false}, // the file its diagnostics are of
}

// Names points the catalog at the files of the run of the neutral names
// where the tool left them. The journal is read whole first: one that names
// a file outside the folder of its item (or in one of its episodes') is
// refused, ErrNamesRefused, and nothing changes. Then, for each item the
// journal finished (of only, when it names any), in one transaction under
// the item's lock: every row of the item that names a file the run renamed
// or took out of the record names it where it is now (the asset of the
// title's file, its sources' places, its subtitle rows, its extras'
// originals, its diagnostics), and each of its recorded sources has the
// library's name for its file (its record's) and no place among the
// arrivals. A row is changed only when it names a file where it was, so
// calling it again changes nothing. An item the journal did not finish (its
// last plan has no done line), one the catalog does not keep where the
// journal has it, and one a row of which would name a file that is not
// there are skipped, each with why.
func (m *Migration) Names(ctx context.Context, only []string) (NamesReport, error) {
	rep := NamesReport{Run: m.Run, Skipped: []NamesSkip{}}
	release, err := m.lock(ctx)
	if err != nil {
		return rep, err
	}
	defer release()
	items, err := readNamesJournal(filepath.Join(m.Dir, "journal.jsonl"))
	if err != nil {
		return rep, err
	}
	rels := make([]string, 0, len(items))
	found := map[string]bool{}
	for rel := range items {
		id := pathpkg.Base(rel)
		if len(only) > 0 && !slices.Contains(only, id) {
			continue
		}
		rels = append(rels, rel)
		found[id] = true
	}
	sort.Strings(rels) // a series before its episodes
	for _, id := range only {
		if !found[id] {
			rep.Skipped = append(rep.Skipped, NamesSkip{ItemID: id, Reason: "the run's journal names no folder of it"})
		}
	}
	for _, rel := range rels {
		if ctx.Err() != nil {
			rep.Stopped = "the caller went away: the items left are not taken"
			return rep, nil
		}
		it, id := items[rel], pathpkg.Base(rel)
		if !it.done {
			rep.Skipped = append(rep.Skipped, NamesSkip{ItemID: id, Reason: "the run did not finish " + rel +
				" (its plan has no done line): call this again once the tool has"})
			continue
		}
		rows, why, err := m.names(context.WithoutCancel(ctx), it)
		switch {
		case err != nil:
			rep.Skipped = append(rep.Skipped, NamesSkip{ItemID: id, Reason: "failed: " + err.Error()})
		case why != "":
			rep.Skipped = append(rep.Skipped, NamesSkip{ItemID: id, Reason: why})
		default:
			rep.Items++
			rep.Rows += rows
		}
	}
	return rep, nil
}

// names brings the rows of the item whose folder the journal finished in
// line, in one transaction: how many rows it changed, or why it changed
// none.
func (m *Migration) names(ctx context.Context, it *namesItem) (int, string, error) {
	id := pathpkg.Base(it.rel)
	if !ValidID(id) {
		return 0, it.rel + " is named by no item's id", nil
	}
	pl, err := PlaceOf(ctx, m.pool, id)
	var unplaced *Unplaced
	switch {
	case errors.Is(err, ErrNoItem):
		return 0, "the catalog has no item of " + it.rel, nil
	case errors.As(err, &unplaced):
		return 0, unplaced.Reason, nil
	case err != nil:
		return 0, "", err
	}
	itemDir := m.p.ItemDir(pl)
	if itemDir != m.inRoot(it.rel) {
		return 0, fmt.Sprintf("the catalog keeps the item in %s, not in %s", m.relOf(itemDir), it.rel), nil
	}
	moves, twice := m.movesOf(it.made)
	if twice != "" {
		return 0, fmt.Sprintf("the run moved two files through %s, so the rows naming it cannot be told apart", m.relOf(twice)), nil
	}
	from, to := make([]string, 0, len(moves)), make([]string, 0, len(moves))
	there := map[string]bool{}
	for _, old := range slices.Sorted(maps.Keys(moves)) {
		from, to = append(from, old), append(to, moves[old])
		_, err := os.Lstat(moves[old])
		there[moves[old]] = err == nil
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := LockItem(ctx, tx, id); err != nil {
		return 0, "", err
	}
	changed := map[string]bool{}
	for _, c := range namesColumns {
		if len(from) == 0 {
			break // nothing moved: the names alone
		}
		stamp := ""
		if c.stamped {
			stamp = ", modifiedat = now()"
		}
		rows, err := tx.Query(ctx, `UPDATE `+c.table+` t SET `+c.column+` = m.topath`+stamp+`
			FROM unnest($2::text[], $3::text[]) AS m(frompath, topath)
			WHERE t.item_id = $1 AND t.`+c.column+` = m.frompath RETURNING t.id, m.topath`, id, from, to)
		if err != nil {
			return 0, "", err
		}
		var missing string
		for rows.Next() {
			var rowID, now string
			if err := rows.Scan(&rowID, &now); err != nil {
				rows.Close()
				return 0, "", err
			}
			if !there[now] && missing == "" {
				missing = now
			}
			changed[c.table+" "+rowID] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, "", err
		}
		if missing != "" {
			return 0, fmt.Sprintf("%s, where the run left a file the catalog names, is not there: the rows of the item are "+
				"left as they are", m.relOf(missing)), nil
		}
	}
	sources, err := SourcesOf(ctx, tx, id)
	if err != nil {
		return 0, "", err
	}
	for _, s := range sources {
		if s.RecordedAt == nil && s.RecordDir == nil {
			continue // waiting for its record, under the name it arrived with
		}
		name := recordedName(SourceDir(itemDir, s.ID))
		if !IsOriginalName(name) {
			if name = s.Filename; !IsOriginalName(name) {
				name = OriginalName(name, 0)
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET filename = $2, librarypath = NULL, modifiedat = now()
			WHERE id = $1 AND (filename <> $2 OR librarypath IS NOT NULL)`, s.ID, name)
		if err != nil {
			return 0, "", err
		}
		if tag.RowsAffected() > 0 {
			changed["com_nalet_katalog_itemsources "+s.ID] = true
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return len(changed), "", nil
}

// movesOf are where the steps left each file they moved, by the path it had
// before them (and each path a step left it at on its way), absolute: a file
// renamed and renamed again where the last rename left it, one removed in
// the run's folder. twice is a path two files lay at, one after the other,
// "" when there is none: a row naming it could be of either.
func (m *Migration) movesOf(steps []namesStep) (moves map[string]string, twice string) {
	type file struct{ at []string }
	var files []*file
	now := map[string]*file{}
	for _, s := range steps {
		var from, to string
		switch s.Op {
		case "rename":
			from, to = m.inRoot(s.From), m.inRoot(s.To)
		case "remove":
			from, to = m.inRoot(s.From), filepath.Join(m.Dir, filepath.FromSlash(s.To))
		default:
			continue
		}
		f := now[from]
		if f == nil {
			f = &file{at: []string{from}}
			files = append(files, f)
		}
		delete(now, from)
		f.at = append(f.at, to)
		now[to] = f
	}
	moves = map[string]string{}
	of := map[string]*file{}
	for _, f := range files {
		last := f.at[len(f.at)-1]
		for _, p := range f.at {
			if o := of[p]; o != nil && o != f {
				return nil, p
			}
			of[p] = f
			if p != last {
				moves[p] = last
			}
		}
	}
	return moves, ""
}

// inRoot is the absolute path of name, relative to the library's root.
func (m *Migration) inRoot(name string) string {
	return filepath.Join(m.p.Root, filepath.FromSlash(name))
}

// relOf is path relative to the library's root, as the journal names one;
// path itself when it lies outside.
func (m *Migration) relOf(path string) string {
	if rel, err := filepath.Rel(m.p.Root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

// recordedName is the name the record of a source (in dir) gives its file;
// "" when it cannot be read.
func recordedName(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "source.json"))
	if err != nil {
		return ""
	}
	var rec struct {
		File struct {
			Name string `json:"name"`
		} `json:"file"`
	}
	if json.Unmarshal(b, &rec) != nil {
		return ""
	}
	return rec.File.Name
}
