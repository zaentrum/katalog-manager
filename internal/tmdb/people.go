package tmdb

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The roles a credit links a person to a title in.
const (
	roleActor    = "actor"
	roleDirector = "director"
)

// peopleReady reports whether migration 030 is in place. Without it a person
// is only a name, and credits link people by name as they always did.
func (s *Service) peopleReady(ctx context.Context) bool {
	if s.peopleOK.Load() {
		return true
	}
	if s.peopleCheck == nil {
		return false
	}
	ok, err := s.peopleCheck(ctx)
	if err != nil || !ok {
		return false
	}
	s.peopleOK.Store(true)
	return true
}

// applyCredits stores a title's credits: each credited person is found by
// their TMDB id and linked to the item in their role.
func (s *Service) applyCredits(ctx context.Context, itemID string, c *tmdbCredits) {
	if !s.peopleReady(ctx) {
		for _, d := range c.Crew {
			s.upsertPersonByName(ctx, itemID, d.Name, roleDirector)
		}
		for _, a := range c.Cast {
			s.upsertPersonByName(ctx, itemID, a.Name, roleActor)
		}
		return
	}
	s.linkCredits(ctx, itemID, c)
}

// creditLinks says what storing one title's credits did.
type creditLinks struct {
	matched  int      // people without a TMDB id that a credit gave theirs
	created  int      // credited people the catalog did not hold yet
	relinked int      // links to a namesake, replaced by a link to the credited person
	people   []string // the credited people, by catalog id, each once
	failed   int      // credits that could not be stored
}

// linkCredits finds or creates every credited person by their TMDB id and links
// them to the item. A link the item has to someone else of a credited name, in
// the same role, is a credit that was once matched by name alone, to a
// namesake: it goes, the link to the credited person stays. Links to names the
// credits do not carry are left alone.
func (s *Service) linkCredits(ctx context.Context, itemID string, c *tmdbCredits) creditLinks {
	var out creditLinks
	type credited struct{ names, ids []string }
	byRole := map[string]*credited{}
	seen := map[string]bool{}
	add := func(role string, cr tmdbCredit) {
		name := oneLine(cr.Name)
		if name == "" {
			return
		}
		personID, how, err := s.findOrCreatePerson(ctx, cr.ID, name)
		if err != nil {
			log.Printf("tmdb: credit %q (TMDB person %d) of item %s: %v", name, cr.ID, itemID, err)
			out.failed++
			return
		}
		switch how {
		case personMatched:
			out.matched++
		case personCreated:
			out.created++
		}
		if err := s.linkPerson(ctx, itemID, personID, role); err != nil {
			log.Printf("tmdb: link %s as %s of item %s: %v", personID, role, itemID, err)
			out.failed++
			return
		}
		r := byRole[role]
		if r == nil {
			r = &credited{}
			byRole[role] = r
		}
		r.names, r.ids = append(r.names, name), append(r.ids, personID)
		if !seen[personID] {
			seen[personID] = true
			out.people = append(out.people, personID)
		}
	}
	for _, d := range c.Crew {
		add(roleDirector, d)
	}
	for _, a := range c.Cast {
		add(roleActor, a)
	}
	for role, r := range byRole {
		tag, err := s.pool.Exec(ctx, `DELETE FROM com_nalet_katalog_itempeople ip
			USING com_nalet_katalog_people p
			WHERE ip.item_id = $1 AND ip.role = $2 AND p.id = ip.person_id
			  AND p.name = ANY($3::text[]) AND NOT (ip.person_id = ANY($4::text[]))`,
			itemID, role, r.names, r.ids)
		if err != nil {
			log.Printf("tmdb: replace links to namesakes of item %s: %v", itemID, err)
			continue
		}
		out.relinked += int(tag.RowsAffected())
	}
	return out
}

// How findOrCreatePerson came by a person.
type personFind int

const (
	personFound   personFind = iota // by TMDB id, or by name for a credit without one
	personMatched                   // a person without a TMDB id, by name: they carry it now
	personCreated                   // new
)

// findOrCreatePerson returns the person a credit names: the one with its TMDB
// id; failing that the first person of that name who has no TMDB id yet (the
// people saved before TMDB ids were kept), who gets the id; failing that a new
// person. A person who has a TMDB id is never taken for another TMDB id, so two
// people of one name stay two. Two enrichments that meet the same new person at
// once end up with the same row: the TMDB id is unique.
func (s *Service) findOrCreatePerson(ctx context.Context, tmdbID int64, name string) (string, personFind, error) {
	if tmdbID <= 0 {
		return s.personByName(ctx, name)
	}
	key := strconv.FormatInt(tmdbID, 10)
	for attempt := 0; attempt < 3; attempt++ {
		var id string
		err := s.pool.QueryRow(ctx,
			`SELECT id FROM com_nalet_katalog_people WHERE tmdbpersonid = $1`, key).Scan(&id)
		if err == nil {
			return id, personFound, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		err = s.pool.QueryRow(ctx, `UPDATE com_nalet_katalog_people SET
				tmdbpersonid = $1,
				fieldorigins = COALESCE(fieldorigins, '{}'::jsonb) || '{"externalIds": "tmdb"}'::jsonb,
				modifiedat = now()
			WHERE id = (SELECT id FROM com_nalet_katalog_people
			            WHERE name = $2 AND tmdbpersonid IS NULL ORDER BY id LIMIT 1)
			  AND tmdbpersonid IS NULL
			RETURNING id`, key, name).Scan(&id)
		if err == nil {
			return id, personMatched, nil
		}
		if isUniqueViolation(err) {
			continue // the id was given to someone meanwhile: find them
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		err = s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people
				(id, name, tmdbpersonid, fieldorigins, createdat, modifiedat)
			VALUES (gen_random_uuid()::varchar, $2, $1, '{"name": "tmdb", "externalIds": "tmdb"}', now(), now())
			ON CONFLICT (tmdbpersonid) WHERE tmdbpersonid IS NOT NULL DO NOTHING
			RETURNING id`, key, name).Scan(&id)
		if err == nil {
			return id, personCreated, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		// created meanwhile by another enrichment: find them on the next pass
	}
	return "", 0, fmt.Errorf("TMDB person %s was created and taken concurrently; giving up", key)
}

// personByName is how a credit without a TMDB id finds its person: by name,
// preferring someone without a TMDB id, as before ids were kept.
func (s *Service) personByName(ctx context.Context, name string) (string, personFind, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_people WHERE name = $1
		ORDER BY tmdbpersonid IS NULL DESC, id LIMIT 1`, name).Scan(&id)
	if err == nil {
		return id, personFound, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, err
	}
	err = s.pool.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people (id, name, fieldorigins, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, $1, '{"name": "tmdb"}', now(), now()) RETURNING id`, name).Scan(&id)
	return id, personCreated, err
}

// linkPerson links a person to an item in a role, once.
func (s *Service) linkPerson(ctx context.Context, itemID, personID, role string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
		SELECT gen_random_uuid()::varchar, $1::text, $2::text, $3::text
		WHERE NOT EXISTS (SELECT 1 FROM com_nalet_katalog_itempeople
		                  WHERE item_id = $1::text AND person_id = $2::text AND role = $3::text)`,
		itemID, personID, role)
	return err
}

// upsertPersonByName is how credits were stored before migration 030: the
// person found or created by name, and linked to the item in the role.
func (s *Service) upsertPersonByName(ctx context.Context, itemID, name, role string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	var personID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM com_nalet_katalog_people WHERE name = $1`, name).Scan(&personID)
	if err == pgx.ErrNoRows {
		personID = ""
	} else if err != nil {
		return
	}
	if personID == "" {
		if err := s.pool.QueryRow(ctx,
			`INSERT INTO com_nalet_katalog_people (id, name) VALUES (gen_random_uuid()::varchar, $1) RETURNING id`,
			name).Scan(&personID); err != nil {
			return
		}
	}
	_ = s.linkPerson(ctx, itemID, personID, role)
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

// oneLine is a single-line text as the library record keeps one: line breaks
// become spaces, control characters go, and so does the space around it.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}
