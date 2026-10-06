package library

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A migration run (platform-library/1, 7): library-v2-from-catalog.py
// --platform stages, under .work/migration/<run>/, the records of the items
// the catalog kept in the store before the library (originals under media/,
// packages under packages/, extras' files under extras/), and the plan of
// each, units/<itemId>.json; katalog-manager adopts them unit by unit
// (adopt.go): the planned renames, then the database as packaging-complete
// leaves it, each action a line of the run's journal, journal.jsonl, which a
// revert replays backwards and which puts right a run a crash stopped short.

// UnitSchema is the schema of a unit's plan.
const UnitSchema = "zaentrum.migration.unit/1"

// Unit is the plan of one item of a run, as the tool stages it.
type Unit struct {
	Schema    string    `json:"schema"`
	Run       string    `json:"run"`
	ItemID    string    `json:"itemId"`
	Type      string    `json:"type"`
	ItemDir   string    `json:"itemDir"`
	StagedDir string    `json:"stagedDir"`
	Moves     []Move    `json:"moves"`
	Guards    Guards    `json:"guards"`
	DB        UnitDB    `json:"db"`
	Problems  []Problem `json:"problems"`
}

// The kinds of a unit's moves, in the order a plan makes them: an old
// package's folders into the staged records, the staged item folder to its
// place, the original and the files beside it to the arrivals, and what is
// left of an old package folder to the run's legacy/.
const (
	MovePackage  = "package"
	MovePublish  = "publish"
	MoveOriginal = "original"
	MoveSidecar  = "sidecar"
	MoveLegacy   = "legacy"
)

// Move is one rename of a plan.
type Move struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Guards are what a plan saw of the old package, checked again before its
// first move: a plan whose package changed since is stale, and staged again.
type Guards struct {
	ManifestSha256 *string `json:"manifestSha256"`
	CompleteMtime  *string `json:"completeMtime"`
	ListingSha256  *string `json:"listingSha256"`
}

// UnitDB is what the adopt changes in the database.
type UnitDB struct {
	RecordedAt                 string         `json:"recordedAt"`
	ProjectedDatabaseUpdatedAt *string        `json:"projectedDatabaseUpdatedAt"`
	Sources                    []UnitSource   `json:"sources"`
	Versions                   []UnitVersion  `json:"versions"`
	Assets                     []UnitAsset    `json:"assets"`
	Subtitles                  []UnitSubtitle `json:"subtitles"`
	Extras                     []UnitExtra    `json:"extras"`
}

// UnitSource is a source of the item: its original at the arrivals, or, gone
// before the library was recorded, at none.
type UnitSource struct {
	SourceID    string          `json:"sourceId"`
	Filename    string          `json:"filename"`
	ArrivalPath *string         `json:"arrivalPath"`
	LibraryPath *string         `json:"libraryPath"`
	SizeBytes   int64           `json:"sizeBytes"`
	QH1         *string         `json:"qh1"`
	RecordDir   *string         `json:"recordDir"`
	Sidecars    []mappedSidecar `json:"sidecars"`
}

// UnitVersion is the item's version its old package becomes.
type UnitVersion struct {
	VersionID     string   `json:"versionId"`
	PackageID     string   `json:"packageId"`
	Dir           string   `json:"dir"`
	CompletedAt   string   `json:"completedAt"`
	SourceIDs     []string `json:"sourceIds"`
	VerifiedAt    *string  `json:"verifiedAt"`
	VerifiedLevel *string  `json:"verifiedLevel"`
}

// UnitAsset is a playback row of the item, where it points once adopted: the
// original's (of a source) or the packaged one (of a version).
type UnitAsset struct {
	ID        string  `json:"id"`
	Path      string  `json:"path"`
	SourceID  *string `json:"sourceId"`
	VersionID *string `json:"versionId"`
}

// UnitSubtitle is a subtitle row of the item, where it points once adopted.
type UnitSubtitle struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// UnitExtra is an extra of the title: its folder and package once adopted,
// none when it is not packaged, and where its original lies.
type UnitExtra struct {
	ID         string  `json:"id"`
	Dir        *string `json:"dir"`
	PackageID  *string `json:"packageId"`
	SourcePath *string `json:"sourcePath"`
}

// Problem is what the stage found of an item that the report says.
type Problem struct {
	Class  string `json:"class"`
	Detail string `json:"detail"`
}

// runRE is a run's name: a folder name of letters, digits, dots, dashes and
// underscores.
var runRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidRun reports whether run may name a migration's run.
func ValidRun(run string) bool { return runRE.MatchString(run) && !strings.Contains(run, "..") }

// unitOrder is the order units are adopted in: a series before its episodes,
// whose folders are in its own.
var unitOrder = map[string]int{"series": 0, "movie": 1, "episode": 2}

// ReadUnits reads the units of the run folder dir, in the order they are
// adopted: series, movies, episodes, each by id.
func ReadUnits(dir string) ([]*Unit, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "units", "*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Unit
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var u Unit
		if err := json.Unmarshal(b, &u); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if u.Schema != UnitSchema {
			return nil, fmt.Errorf("%s: its schema is %q, not %s", path, u.Schema, UnitSchema)
		}
		if strings.TrimSuffix(filepath.Base(path), ".json") != u.ItemID {
			return nil, fmt.Errorf("%s: the plan of item %s", path, u.ItemID)
		}
		out = append(out, &u)
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := unitOrder[out[i].Type], unitOrder[out[j].Type]
		if oi != oj {
			return oi < oj
		}
		return out[i].ItemID < out[j].ItemID
	})
	return out, nil
}

// ListingSHA256 is the guard of a plan's package folders, as the tool
// computes it: sha256 over one "<size> <path>\n" line per file under each
// folder, the folders in their order, the files of one by their path within
// it, the path the folder's joined with it. A folder that is not there lists
// nothing.
func ListingSHA256(folders []string) (string, error) {
	h := sha256.New()
	for _, folder := range folders {
		var rels []string
		err := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == folder && errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if !d.IsDir() {
				rel, err := filepath.Rel(folder, path)
				if err != nil {
					return err
				}
				rels = append(rels, rel)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		sort.Strings(rels)
		for _, rel := range rels {
			p := filepath.Join(folder, rel)
			fi, err := os.Stat(p)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%d %s\n", fi.Size(), p)
		}
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// MtimeOf is a file's modification time as a plan's guard writes it: RFC
// 3339 in UTC with nine digits of its fraction.
func MtimeOf(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	return fi.ModTime().UTC().Format("2006-01-02T15:04:05.000000000Z"), nil
}

// mtimeTolerance is how far a .complete's modification time may be from the
// plan's and still be the one it saw: a client of the share may read it
// rounded to the microsecond.
const mtimeTolerance = time.Millisecond

// sameMtime reports whether two guards' times are one moment, within the
// tolerance.
func sameMtime(a, b string) bool {
	ta, err1 := time.Parse(time.RFC3339Nano, a)
	tb, err2 := time.Parse(time.RFC3339Nano, b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	d := ta.Sub(tb)
	return d < mtimeTolerance && d > -mtimeTolerance
}

// The journal's operations and their states.
const (
	OpMove       = "move"
	OpDB         = "db"
	OpProjection = "projection" // an item's projection written again: what it replaced
	OpUnit       = "unit"

	StateDone     = "done"     // a move made, the database changed
	StateUndone   = "undone"   // a move put back after its unit failed
	StateReverted = "reverted" // a move, a database change or a unit reverted
	StateRestored = "restored" // an original put back from the trash
	StateAdopted  = "adopted"
	StateStale    = "stale"
	StateBusy     = "busy"
	StateFailed   = "failed"
	StateRefused  = "refused"
)

// JournalEntry is one line of a run's journal.
type JournalEntry struct {
	Seq    int64           `json:"seq"`
	At     string          `json:"at"`
	ItemID string          `json:"itemId"`
	Op     string          `json:"op"`
	Kind   string          `json:"kind,omitempty"`
	From   string          `json:"from,omitempty"`
	To     string          `json:"to,omitempty"`
	Before json.RawMessage `json:"before,omitempty"`
	State  string          `json:"state"`
	Reason string          `json:"reason,omitempty"`
}

// Journal is a run's journal.jsonl, which only grows: a line per action,
// synced to storage before the next.
type Journal struct {
	f       *os.File
	seq     int64
	now     func() time.Time
	entries []JournalEntry
}

// OpenJournal opens the journal at path, made when missing, and reads what
// it holds.
func OpenJournal(path string, now func() time.Time) (*Journal, error) {
	if err := MkdirAll(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, FileMode)
	if err != nil {
		return nil, err
	}
	j := &Journal{f: f, now: now}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// A line a crash cut short is the last: what it was about is
			// found again from the files and the database.
			continue
		}
		j.entries = append(j.entries, e)
		j.seq = max(j.seq, e.Seq)
	}
	if err := sc.Err(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return j, nil
}

// Add appends e to the journal, numbered and timed, and syncs it.
func (j *Journal) Add(e JournalEntry) error {
	j.seq++
	e.Seq, e.At = j.seq, j.now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.entries = append(j.entries, e)
	return nil
}

// Close closes the journal.
func (j *Journal) Close() error { return j.f.Close() }

// attempt is an adoption, or a revert, of an item as the journal holds it:
// its entries up to and with the unit entry that ends it.
type attempt struct {
	entries []JournalEntry
	unit    JournalEntry
}

// moves are the attempt's moves in state.
func (a attempt) moves(state string) []JournalEntry {
	var out []JournalEntry
	for _, e := range a.entries {
		if e.Op == OpMove && e.State == state {
			out = append(out, e)
		}
	}
	return out
}

// db is the attempt's database entry in state; nil when it has none.
func (a attempt) db(state string) *JournalEntry {
	for i := range a.entries {
		if e := a.entries[i]; e.Op == OpDB && e.State == state {
			return &a.entries[i]
		}
	}
	return nil
}

// itemJournal is what the journal says of an item: its attempts, and the
// entries after the last one's unit entry, of an attempt a crash stopped.
type itemJournal struct {
	attempts []attempt
	open     []JournalEntry
}

// state is the state the item's last attempt left it in: adopted, reverted,
// stale, busy, failed, refused; "" when it has none.
func (ij *itemJournal) state() string {
	if ij == nil || len(ij.attempts) == 0 {
		return ""
	}
	return ij.attempts[len(ij.attempts)-1].unit.State
}

// adoption is the attempt that adopted the item, while it is adopted; nil
// when it is not.
func (ij *itemJournal) adoption() *attempt {
	if ij.state() != StateAdopted {
		return nil
	}
	return &ij.attempts[len(ij.attempts)-1]
}

// items reads the journal by item.
func (j *Journal) items() map[string]*itemJournal {
	out := map[string]*itemJournal{}
	for _, e := range j.entries {
		ij := out[e.ItemID]
		if ij == nil {
			ij = &itemJournal{}
			out[e.ItemID] = ij
		}
		if e.Op != OpUnit {
			ij.open = append(ij.open, e)
			continue
		}
		ij.attempts = append(ij.attempts, attempt{entries: ij.open, unit: e})
		ij.open = nil
	}
	return out
}
