package store

import (
	"context"
	"log"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/events"
)

// A title's identities outside any one library, its ids in the reference
// databases, are rows of com_nalet_katalog_itemexternalids, one for each
// source: a movie's and a series' TMDB id under tmdb, an episode's own under
// tmdb-episode, and the IMDb and TheTVDB ids under imdb and tvdb. Enrichment
// writes them, and POST /api/ingest those of a title it is handed by its
// identity (ADR-0012).

// IdentitySources are the sources a title is handed over and found by, in
// the order a lookup tries them: the first that finds a title decides.
var IdentitySources = []string{"tmdb", "tvdb", "imdb"}

// EpisodeSource is the source the catalog keeps an episode's own id of the
// reference database src under: TMDB's under tmdb-episode (an episode's
// record names its series' TV id by tmdb), the others under their names.
func EpisodeSource(src string) string {
	if src == "tmdb" {
		return "tmdb-episode"
	}
	return src
}

// Querier runs statements: the pool or a transaction.
type Querier = querier

// Identity is what the catalog holds of an item's identity: its type, its
// own reference ids (each source to its id), and of an episode its series
// (its parent, or its parent's parent when that is the series), the series'
// ids and the episode's numbers.
type Identity struct {
	Type              string
	ExternalIDs       map[string]string
	SeriesID          string
	SeriesExternalIDs map[string]string
	SeasonNumber      *int32
	EpisodeNumber     *int32
}

// Event is what an item event names of the identity (events.Identity): the
// item's ids, and an episode's series' ids and its numbers.
func (i Identity) Event() events.Identity {
	ev := events.Identity{ExternalIDs: i.ExternalIDs}
	if i.Type == "episode" {
		ev.SeriesExternalIDs, ev.SeasonNumber, ev.EpisodeNumber = i.SeriesExternalIDs, i.SeasonNumber, i.EpisodeNumber
	}
	return ev
}

// IdentitiesOf reads the identities of the items ids, by id, as q reads the
// catalog; an item there is not has none. A source is named in lower case,
// and of a source an item holds twice the row of the lowest id counts.
func IdentitiesOf(ctx context.Context, q Querier, ids []string) (map[string]Identity, error) {
	out := map[string]Identity{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT i.id, lower(btrim(i.type)), i.seasonnumber, i.episodenumber,
			CASE WHEN lower(btrim(p.type)) = 'series' THEN p.id WHEN lower(btrim(g.type)) = 'series' THEN g.id END
		FROM com_nalet_katalog_items i
		LEFT JOIN com_nalet_katalog_items p ON p.id = i.parent_id
		LEFT JOIN com_nalet_katalog_items g ON g.id = p.parent_id
		WHERE i.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	owners := []string{}
	for rows.Next() {
		var id string
		var i Identity
		var series *string
		if err := rows.Scan(&id, &i.Type, &i.SeasonNumber, &i.EpisodeNumber, &series); err != nil {
			rows.Close()
			return nil, err
		}
		if series != nil && i.Type == "episode" {
			i.SeriesID = *series
			owners = append(owners, *series)
		}
		out[id] = i
		owners = append(owners, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs, err := externalIDsOf(ctx, q, owners)
	if err != nil {
		return nil, err
	}
	for id, i := range out {
		i.ExternalIDs = refs[id]
		if i.SeriesID != "" {
			i.SeriesExternalIDs = refs[i.SeriesID]
		}
		out[id] = i
	}
	return out, nil
}

// externalIDsOf reads the reference ids of the items ids: of each, each
// source to its id.
func externalIDsOf(ctx context.Context, q Querier, ids []string) (map[string]map[string]string, error) {
	rows, err := q.Query(ctx, `SELECT item_id, lower(btrim(source)), btrim(externalid) FROM com_nalet_katalog_itemexternalids
		WHERE item_id = ANY($1) ORDER BY item_id, id`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var item, src, id string
		if err := rows.Scan(&item, &src, &id); err != nil {
			return nil, err
		}
		if src == "" || id == "" {
			continue
		}
		if out[item] == nil {
			out[item] = map[string]string{}
		}
		if _, ok := out[item][src]; !ok {
			out[item][src] = id
		}
	}
	return out, rows.Err()
}

// IdentityOf is the identity of the item id, as the pool reads the catalog.
func (s *Store) IdentityOf(ctx context.Context, id string) (Identity, error) {
	all, err := IdentitiesOf(ctx, s.pool, []string{id})
	return all[id], err
}

// EventIdentity is what an item event of the item id names of its
// identities, read as the event is sent. It is best-effort, as the event's
// identity fields are optional: one that cannot be read is none, said in the
// log, and the event goes all the same.
func (s *Store) EventIdentity(ctx context.Context, id string) events.Identity {
	i, err := s.IdentityOf(ctx, id)
	if err != nil {
		log.Printf("catalog events: the identities of item %s could not be read, and its event names none: %v", id, err)
	}
	return i.Event()
}

// ItemsByIdentity are the items of the type typ that hold the identity ids
// (each source to its id, as the catalog keeps them), found by the first of
// IdentitySources that finds any, the one made first first; none when no
// item of the type holds any of them.
func ItemsByIdentity(ctx context.Context, q Querier, typ string, ids map[string]string) ([]string, error) {
	for _, src := range IdentitySources {
		id := ids[src]
		if id == "" {
			continue
		}
		rows, err := q.Query(ctx, `SELECT i.id FROM com_nalet_katalog_items i
			WHERE i.type = $1 AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemexternalids e
				WHERE e.item_id = i.id AND e.source = $2 AND e.externalid = $3)
			ORDER BY i.createdat NULLS LAST, i.id`, typ, src, id)
		if err != nil {
			return nil, err
		}
		var found []string
		for rows.Next() {
			var item string
			if err := rows.Scan(&item); err != nil {
				rows.Close()
				return nil, err
			}
			found = append(found, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if len(found) > 0 {
			return found, nil
		}
	}
	return nil, nil
}

// AddExternalIDs gives the item id the reference ids ids (each source to its
// id, as the catalog keeps them), a row for each source it holds none of, in
// the order of their sources' names; an empty id is none.
func AddExternalIDs(ctx context.Context, q Querier, id string, ids map[string]string) error {
	srcs := make([]string, 0, len(ids))
	for src := range ids {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		if ids[src] == "" {
			continue
		}
		if _, err := q.Exec(ctx, `INSERT INTO com_nalet_katalog_itemexternalids (id, item_id, source, externalid)
			SELECT gen_random_uuid()::varchar, $1::varchar, $2::varchar, $3::varchar
			WHERE NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itemexternalids WHERE item_id = $1 AND source = $2)`,
			id, src, ids[src]); err != nil {
			return err
		}
	}
	return nil
}

// MakeSeries makes a series as the scanner makes one it finds, in q: its
// title, its sort title the title in lower case, and so the name it is found
// by as it was made (scantitle, with migration 046), its year, and who made
// it (nil: the catalog names no one). It holds no file: enrichment gives it
// its texts and images. The caller seeds its scan step and sends its
// discovered event, which has enrichment match it. It answers its id.
func MakeSeries(ctx context.Context, q Querier, title string, year *int32, createdBy *string) (string, error) {
	byScanTitle, err := seriesScanTitleReady(ctx, q)
	if err != nil {
		return "", err
	}
	insert := `INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, year, createdby, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, 'series', $1, $2, $3, $4, now(), now())
		RETURNING id`
	if byScanTitle {
		insert = `INSERT INTO com_nalet_katalog_items (id, type, title, sorttitle, scantitle, year, createdby, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, 'series', $1, $2, $2, $3, $4, now(), now())
		RETURNING id`
	}
	var id string
	err = q.QueryRow(ctx, insert, title, strings.ToLower(title), year, createdBy).Scan(&id)
	return id, err
}
