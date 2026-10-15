package store

import (
	"context"
	"log"

	"github.com/zaentrum/katalog-manager/internal/events"
)

// A title's identities outside any one library, its ids in the reference
// databases, are rows of com_nalet_katalog_itemexternalids, one for each
// source: a movie's and a series' TMDB id under tmdb, an episode's own under
// tmdb-episode, and the IMDb and TheTVDB ids under imdb and tvdb. Enrichment
// writes them.

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
