package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/config"
)

// The path rules (platform-library/1, 2.1). The migration's tool places an
// item by the same rules.
//
//	ItemDir(movie)   = <LIB>/movies/<id[0:2]>/<id>
//	ItemDir(series)  = <LIB>/series/<id[0:2]>/<id>
//	ItemDir(episode) = ItemDir(its series) + "/episodes/<id>"
//	SourceDir        = ItemDir + "/sources/<sourceId>"
//	VersionDir       = ItemDir + "/versions/<versionId>"
//	ExtraDir         = ItemDir(movie|series) + "/extras/<extraId>"
//	EventDir         = ItemDir + "/events/<YYYYMMDDTHHMMSSZ>-<eventId[0:8]>-<kind>"
//	PersonDir        = <LIB>/people/<id[0:2]>/<id>
//	InboxDir         = <WORK>/inbox/<itemId>, <WORK>/inbox/extra-<extraId>
//	StagingDir       = <WORK>/staging/<versionId>, <WORK>/staging/extra-<extraId>
//
// An episode's series is its parent when that is the series, else its
// parent's parent: an episode under a season sits in its series' folder.

// Paths are where the library and its work folder lie.
type Paths struct {
	Root     string // LIBRARY_ROOT: the record's movies/, series/ and people/
	Work     string // WORK_ROOT
	Arrivals string // ARRIVALS_ROOT: the files that arrive, the scan root
	Extras   string // EXTRAS_ROOT: the extras' files taken in by the API
}

// PathsOf are the paths cfg configures.
func PathsOf(cfg config.Config) Paths {
	return Paths{Root: cfg.LibraryRoot, Work: cfg.WorkRoot, Arrivals: cfg.ArrivalsRoot, Extras: cfg.ExtrasRoot}
}

// The library's top folders: the record of movies, of series (and their
// episodes) and of people.
const (
	MoviesDir = "movies"
	SeriesDir = "series"
	PeopleDir = "people"
)

// uuidRE is an id as the record names one: a lower-case UUID.
var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidID reports whether id may name a folder of the record: a lower-case
// UUID.
func ValidID(id string) bool { return uuidRE.MatchString(id) }

// shard is the folder an id sits in: its first two characters.
func shard(id string) string {
	if len(id) < 2 {
		return id
	}
	return id[:2]
}

// MovieDir is the folder of the movie id.
func (p Paths) MovieDir(id string) string { return filepath.Join(p.Root, MoviesDir, shard(id), id) }

// SeriesDir is the folder of the series id.
func (p Paths) SeriesDir(id string) string { return filepath.Join(p.Root, SeriesDir, shard(id), id) }

// EpisodeDir is the folder of the episode id of the series seriesID.
func (p Paths) EpisodeDir(seriesID, id string) string {
	return filepath.Join(p.SeriesDir(seriesID), "episodes", id)
}

// PersonDir is the folder of the person id.
func (p Paths) PersonDir(id string) string { return filepath.Join(p.Root, PeopleDir, shard(id), id) }

// SourceDir is the folder of an item's source record.
func SourceDir(itemDir, sourceID string) string { return filepath.Join(itemDir, "sources", sourceID) }

// VersionDir is the folder of an item's version.
func VersionDir(itemDir, versionID string) string {
	return filepath.Join(itemDir, "versions", versionID)
}

// ExtraDir is the folder of a movie's or a series' extra.
func ExtraDir(itemDir, extraID string) string { return filepath.Join(itemDir, "extras", extraID) }

// EventStamp is the moment an event's folder is named by: UTC, to the second.
func EventStamp(at time.Time) string { return at.UTC().Format("20060102T150405Z") }

// EventDir is the folder of an item's event: its moment, the first eight
// characters of its id and its kind.
func EventDir(itemDir string, at time.Time, eventID, kind string) string {
	id8 := eventID
	if len(id8) > 8 {
		id8 = id8[:8]
	}
	return filepath.Join(itemDir, "events", EventStamp(at)+"-"+id8+"-"+kind)
}

// InboxDir is where the transcoder hands the item's encode to the packager.
func (p Paths) InboxDir(itemID string) string { return filepath.Join(p.Work, WorkInbox, itemID) }

// ExtraInboxDir is where the transcoder hands the extra's encode to the
// packager.
func (p Paths) ExtraInboxDir(extraID string) string {
	return filepath.Join(p.Work, WorkInbox, "extra-"+extraID)
}

// StagingDir is where the packager builds a version.
func (p Paths) StagingDir(versionID string) string {
	return filepath.Join(p.Work, WorkStaging, versionID)
}

// ExtraStagingDir is where the packager builds an extra's folder.
func (p Paths) ExtraStagingDir(extraID string) string {
	return filepath.Join(p.Work, WorkStaging, "extra-"+extraID)
}

// TrashDay is the trash's folder of the day t: <WORK>/trash/<YYYYMMDD>.
func (p Paths) TrashDay(t time.Time) string {
	return filepath.Join(p.Work, WorkTrash, t.UTC().Format("20060102"))
}

// TrashDir is where a retired original and its sidecars wait out their
// grace: <WORK>/trash/<YYYYMMDD>/<sourceId>.
func (p Paths) TrashDir(t time.Time, sourceID string) string {
	return filepath.Join(p.TrashDay(t), sourceID)
}

// ExtraTrashDir is where an extra's retired original waits out its grace.
func (p Paths) ExtraTrashDir(t time.Time, extraID string) string {
	return filepath.Join(p.TrashDay(t), "extra-"+extraID)
}

// ReplaceDir is where a file handed to replaceSource waits.
func (p Paths) ReplaceDir() string { return filepath.Join(p.Work, WorkReplace) }

// MigrationDir is the folder of a migration's run.
func (p Paths) MigrationDir(run string) string { return filepath.Join(p.Work, WorkMigration, run) }

// LegacyDir is where what is left of the old layout waits to be deleted.
func (p Paths) LegacyDir() string { return filepath.Join(p.Work, WorkLegacy) }

// Within reports whether path lies strictly inside root, both cleaned.
func Within(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || root == "." || root == "/" || !filepath.IsAbs(root) {
		return false
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// ErrNoItem is the answer for an item there is not.
var ErrNoItem = errors.New("no such item")

// Unplaced says why an item has no folder in the tree.
type Unplaced struct{ Reason string }

func (e *Unplaced) Error() string { return e.Reason }

// Place is what puts an item in the tree: its id and type, and an episode's
// series.
type Place struct {
	ID, Type string
	SeriesID string // an episode's series
}

// PlaceOf reads where the item id goes in the tree: a movie, a series, or an
// episode of a series, under its season maybe. Anything else is Unplaced, an
// item there is not ErrNoItem.
func PlaceOf(ctx context.Context, q Querier, id string) (Place, error) {
	var pl Place
	var parent, parentType, grandparent, grandType *string
	err := q.QueryRow(ctx, `SELECT i.id, i.type, p.id, p.type, g.id, g.type FROM com_nalet_katalog_items i
		LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
		LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id
		WHERE i.id = $1`, id).Scan(&pl.ID, &pl.Type, &parent, &parentType, &grandparent, &grandType)
	if errors.Is(err, pgx.ErrNoRows) {
		return pl, ErrNoItem
	}
	if err != nil {
		return pl, err
	}
	pl.Type = strings.ToLower(strings.TrimSpace(pl.Type))
	switch pl.Type {
	case "movie", "series":
	case "episode":
		switch {
		case parent != nil && strings.EqualFold(deref(parentType), "series"):
			pl.SeriesID = *parent
		case grandparent != nil && strings.EqualFold(deref(grandType), "series"):
			pl.SeriesID = *grandparent
		case parent == nil:
			return pl, &Unplaced{Reason: "an episode needs its series before it is recorded"}
		default:
			return pl, &Unplaced{Reason: fmt.Sprintf("an episode is recorded in its series' folder, and its parent %s "+
				"(a %s) is no series nor a season of one", *parent, deref(parentType))}
		}
	default:
		return pl, &Unplaced{Reason: fmt.Sprintf("a %s is not recorded in the library: only a movie, a series and an episode are",
			pl.Type)}
	}
	if !ValidID(pl.ID) || (pl.SeriesID != "" && !ValidID(pl.SeriesID)) {
		return pl, &Unplaced{Reason: fmt.Sprintf("item %s is not recorded: the library names a folder by a lower-case UUID", pl.ID)}
	}
	return pl, nil
}

// ItemDir is the folder of the item pl places.
func (p Paths) ItemDir(pl Place) string {
	switch pl.Type {
	case "movie":
		return p.MovieDir(pl.ID)
	case "series":
		return p.SeriesDir(pl.ID)
	}
	return p.EpisodeDir(pl.SeriesID, pl.ID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
