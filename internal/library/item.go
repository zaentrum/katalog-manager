package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ItemFile is an item's identity record, written once.
const ItemFile = "item.json"

// Blocked says why an item cannot be recorded: its record would not hold.
// A worker record names it, and the packager fails its step with it.
type Blocked struct{ Reason string }

func (e *Blocked) Error() string { return e.Reason }

// creatorOf is who created an item as its record names it when the catalog
// does not say: the scanner, which creates every item it finds.
const creatorOf = "katalog-manager/scanner"

var (
	tmdbIDRE = regexp.MustCompile(`^[0-9]+$`)
	imdbRE   = regexp.MustCompile(`^tt[0-9]+$`)
)

// ItemRecord is what an item's record says, read from the catalog.
type ItemRecord struct {
	Place
	Title         string
	CreatedAt     string // RFC 3339, UTC, to the second
	CreatedBy     string
	SeasonNumber  *int32
	EpisodeNumber *int32
	ExternalIDs   [][2]string // (source, id) as the catalog holds them
	Recorded      bool        // items.recordedat is set
}

// ExternalIDs are an item's reference ids as a record names them (item.json,
// and metadata.json's current ones): TMDB's under the key of the item's
// type, IMDb's and TheTVDB's when they have their form; any other source has
// no field, and a TMDB id that is no number is none.
func ExternalIDs(typ string, ids [][2]string) Doc {
	got := map[string]string{}
	for _, e := range ids {
		src, val := strings.ToLower(e[0]), oneLine(e[1])
		if val == "" {
			continue
		}
		switch src {
		case "tmdb", "themoviedb":
			if typ == "movie" {
				got["tmdbMovie"] = val
			} else {
				got["tmdbTv"] = val
			}
		case "tmdb-episode", "tmdbepisode":
			if typ == "episode" {
				got["tmdbEpisode"] = val
			}
		case "tmdb-season", "tmdbseason":
			if typ != "movie" {
				got["tmdbSeason"] = val
			}
		case "imdb":
			if imdbRE.MatchString(val) {
				got["imdb"] = val
			}
		case "tvdb":
			if tmdbIDRE.MatchString(val) {
				got["tvdb"] = val
			}
		}
	}
	for _, k := range []string{"tmdbMovie", "tmdbTv", "tmdbSeason", "tmdbEpisode"} {
		if v, ok := got[k]; ok && !tmdbIDRE.MatchString(v) {
			delete(got, k)
		}
	}
	// The order a record lists them in: the order the catalog's ids came in,
	// as the tool that writes a migrated tree keeps it.
	out := Doc{}
	seen := map[string]bool{}
	for _, e := range ids {
		for _, k := range keysOf(typ, strings.ToLower(e[0])) {
			if v, ok := got[k]; ok && !seen[k] {
				seen[k] = true
				out = append(out, Field{k, v})
			}
		}
	}
	return out
}

func keysOf(typ, src string) []string {
	switch src {
	case "tmdb", "themoviedb":
		if typ == "movie" {
			return []string{"tmdbMovie"}
		}
		return []string{"tmdbTv"}
	case "tmdb-episode", "tmdbepisode":
		return []string{"tmdbEpisode"}
	case "tmdb-season", "tmdbseason":
		return []string{"tmdbSeason"}
	case "imdb", "tvdb":
		return []string{src}
	}
	return nil
}

// oneLine is a record's one-line text: line breaks are spaces, the spaces
// around it go.
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
}

// Doc is the item's record, item.json: its identity as the catalog holds it
// when the item is recorded, written once.
func (r ItemRecord) Doc() (Doc, error) {
	title := oneLine(r.Title)
	if title == "" {
		return nil, &Blocked{Reason: "an item needs a title before it is recorded: its record carries the one it was created with"}
	}
	by := oneLine(r.CreatedBy)
	if by == "" {
		by = creatorOf
	}
	doc := Doc{{"schema", "zaentrum.library.item/2"}, {"itemId", r.ID}, {"type", r.Type}, {"title", title},
		{"externalIds", ExternalIDs(r.Type, r.ExternalIDs)}, {"createdAt", r.CreatedAt}, {"createdBy", by}}
	if r.Type == "episode" {
		if r.SeasonNumber == nil || r.EpisodeNumber == nil {
			return nil, &Blocked{Reason: "an episode needs its season and episode numbers before it is recorded"}
		}
		if *r.SeasonNumber < 0 || *r.EpisodeNumber < 0 {
			return nil, &Blocked{Reason: fmt.Sprintf("an episode is recorded with numbers of 0 or more, not S%dE%d",
				*r.SeasonNumber, *r.EpisodeNumber)}
		}
		doc = append(doc, Field{"seriesId", r.SeriesID}, Field{"seasonNumber", int64(*r.SeasonNumber)},
			Field{"episodeNumber", int64(*r.EpisodeNumber)},
			Field{"episodeCode", fmt.Sprintf("S%02dE%02d", *r.SeasonNumber, *r.EpisodeNumber)})
	}
	return doc, nil
}

// ReadItemRecord reads what the item id's record says from the catalog. An
// item the tree holds no folder for is Unplaced, one there is not ErrNoItem.
func ReadItemRecord(ctx context.Context, q Querier, id string) (ItemRecord, error) {
	var r ItemRecord
	pl, err := PlaceOf(ctx, q, id)
	if err != nil {
		return r, err
	}
	r.Place = pl
	var createdBy *string
	if err := q.QueryRow(ctx, `SELECT title, COALESCE(to_char(createdat, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			to_char(now() AT TIME ZONE 'utc', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')),
			createdby, seasonnumber, episodenumber, recordedat IS NOT NULL
		FROM com_nalet_katalog_items WHERE id = $1`, id).Scan(&r.Title, &r.CreatedAt, &createdBy, &r.SeasonNumber,
		&r.EpisodeNumber, &r.Recorded); err != nil {
		return r, err
	}
	r.CreatedBy = deref(createdBy)
	rows, err := q.Query(ctx, `SELECT source, externalid FROM com_nalet_katalog_itemexternalids WHERE item_id = $1
		ORDER BY id`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var e [2]string
		if err := rows.Scan(&e[0], &e[1]); err != nil {
			return r, err
		}
		r.ExternalIDs = append(r.ExternalIDs, e)
	}
	return r, rows.Err()
}

// EnsureItemRecord records the item id in the library: its folder gets its
// item.json and the checksums.sha256 that lists exactly it, the checksums
// last, and the catalog notes when (items.recordedat). A series is recorded
// before its first episode. An item recorded already is left as it is, and
// so is a folder whose record is complete (its checksums written): a record
// is written once. Its content is the catalog's, the moment it was created
// among it, so a write cut short and done again writes the same bytes.
//
// It answers the item's folder, also when the item cannot be recorded
// (Blocked) but has a place in the tree; one that has none is Blocked with no
// folder, one there is not ErrNoItem.
func (p Paths) EnsureItemRecord(ctx context.Context, q Querier, id string) (string, error) {
	r, err := ReadItemRecord(ctx, q, id)
	var unplaced *Unplaced
	if errors.As(err, &unplaced) {
		return "", &Blocked{Reason: unplaced.Reason}
	}
	if err != nil {
		return "", err
	}
	dir := p.ItemDir(r.Place)
	if r.Recorded {
		return dir, nil
	}
	if r.Type == "episode" {
		if _, err := p.EnsureItemRecord(ctx, q, r.SeriesID); err != nil {
			var blocked *Blocked
			if errors.As(err, &blocked) {
				return dir, &Blocked{Reason: "its series " + r.SeriesID + " is not recorded: " + blocked.Reason}
			}
			return "", err
		}
	}
	doc, err := r.Doc()
	if err != nil {
		return dir, err
	}
	if _, err := os.Stat(filepath.Join(dir, SumsFile)); errors.Is(err, os.ErrNotExist) {
		b, err := Encode(doc)
		if err != nil {
			return "", err
		}
		if err := WriteCovered(dir, map[string][]byte{ItemFile: b}); err != nil {
			return "", fmt.Errorf("record item %s in %s: %w", id, dir, err)
		}
	} else if err != nil {
		return "", err
	}
	if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_items SET recordedat = now() WHERE id = $1 AND recordedat IS NULL`, id); err != nil {
		return "", err
	}
	return dir, nil
}
