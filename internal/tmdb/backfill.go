package tmdb

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// RefreshPeople is the operator's backfill of people (graph.PeopleRefresher).
// It reads the TMDB credits of titles again, which replace the titles' credits
// (replaceCredits): the people they credit get their TMDB ids (a person known
// only by name is matched by it once), a credit TMDB no longer lists goes, and
// a person no title credits any more is deleted and logged. Then it reads
// people's details and profiles, honouring their locks.
//
// With all it reads every movie and series that has a TMDB id and every
// person who has one; without, only the titles crediting someone who has no
// TMDB id yet and the people never read, so running it again picks up what a
// failure left. The run goes on even when the caller stops waiting (its
// counts are logged), and only one runs at a time.
func (s *Service) RefreshPeople(ctx context.Context, all bool) (graph.PeopleRefreshResult, error) {
	var res graph.PeopleRefreshResult
	if !s.tmdb.enabled() {
		return res, errors.New("TMDB API key not configured")
	}
	if !s.peopleReady(ctx) {
		return res, errors.New("the people migration (db/migrations/030_people.sql) is not applied")
	}
	if !s.refreshing.CompareAndSwap(false, true) {
		return res, errors.New("a people refresh is already running")
	}
	defer s.refreshing.Store(false)
	ctx = context.WithoutCancel(ctx)
	res.StartedAt = time.Now().UTC()

	titles, err := s.titlesToRead(ctx, all)
	if err != nil {
		return res, err
	}
	for _, t := range titles {
		var c *tmdbCredits
		var ok bool
		if t.typ == "movie" {
			c, ok = s.tmdb.getCredits(ctx, t.tmdbID)
		} else {
			_, c, ok = s.tmdb.getTv(ctx, t.tmdbID) // a series' credits come with its details
			ok = ok && c != nil
		}
		if !ok {
			res.TitlesFailed++
			log.Printf("tmdb: people refresh: the credits of %s %s (TMDB %d) could not be read", t.typ, t.id, t.tmdbID)
			continue
		}
		ch, err := s.replaceCredits(ctx, t.id, c)
		if err != nil {
			res.TitlesFailed++
			log.Printf("tmdb: people refresh: the credits of %s %s (TMDB %d) could not be stored: %v", t.typ, t.id, t.tmdbID, err)
			continue
		}
		res.TitlesRead++
		if ch.locked {
			res.TitlesLocked++
		}
		res.PeopleMatched += int32(ch.matched)
		res.PeopleCreated += int32(ch.created)
		res.CreditsAdded += int32(ch.added)
		res.CreditsUpdated += int32(ch.updated)
		res.CreditsDropped += int32(ch.dropped)
		res.CreditsRelinked += int32(ch.relinked)
		res.PeopleDeleted += int32(ch.deleted)
	}

	where := `tmdbfetchedat IS NULL`
	if all {
		where = `true`
	}
	people, err := s.peopleToRead(ctx, where)
	if err != nil {
		return res, err
	}
	for _, p := range people {
		out, err := s.refreshPerson(ctx, p.id, p.tmdbID, nil)
		switch {
		case err != nil:
			res.PeopleFailed++
			log.Printf("tmdb: people refresh: person %s (TMDB %d): %v", p.id, p.tmdbID, err)
		case out == personLocked:
			res.PeopleLocked++
		case out == personGone:
			res.PeopleNotFound++
		default:
			res.PeopleFetched++
		}
	}

	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM com_nalet_katalog_people WHERE tmdbpersonid IS NULL`).
		Scan(&res.PeopleWithoutTmdbID); err != nil {
		return res, err
	}
	res.FinishedAt = time.Now().UTC()
	log.Printf("tmdb: people refresh all=%v: titles read=%d failed=%d locked=%d; credits added=%d updated=%d "+
		"dropped=%d relinked=%d; people matched=%d created=%d deleted=%d fetched=%d locked=%d notFound=%d failed=%d "+
		"withoutTmdbId=%d (%s)", all,
		res.TitlesRead, res.TitlesFailed, res.TitlesLocked, res.CreditsAdded, res.CreditsUpdated, res.CreditsDropped,
		res.CreditsRelinked,
		res.PeopleMatched, res.PeopleCreated, res.PeopleDeleted, res.PeopleFetched, res.PeopleLocked,
		res.PeopleNotFound, res.PeopleFailed, res.PeopleWithoutTmdbID,
		res.FinishedAt.Sub(res.StartedAt).Round(time.Millisecond))
	return res, nil
}

// titleRef is a movie or series and its TMDB id.
type titleRef struct {
	id, typ string
	tmdbID  int64
}

// titlesToRead lists the movies and series with a TMDB id: all of them, or
// only those crediting someone who has no TMDB id yet.
func (s *Service) titlesToRead(ctx context.Context, all bool) ([]titleRef, error) {
	rows, err := s.pool.Query(ctx, `SELECT i.id, i.type, e.externalid
		FROM com_nalet_katalog_items i
		JOIN com_nalet_katalog_itemexternalids e ON e.item_id = i.id AND e.source = 'tmdb'
		WHERE i.type IN ('movie', 'series')
		  AND ($1 OR EXISTS (SELECT 1 FROM com_nalet_katalog_itempeople ip
		                     JOIN com_nalet_katalog_people p ON p.id = ip.person_id
		                     WHERE ip.item_id = i.id AND p.tmdbpersonid IS NULL))
		ORDER BY i.type, i.id`, all)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []titleRef
	for rows.Next() {
		var t titleRef
		var ext string
		if err := rows.Scan(&t.id, &t.typ, &ext); err != nil {
			return nil, err
		}
		if t.tmdbID, err = strconv.ParseInt(strings.TrimSpace(ext), 10, 64); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
