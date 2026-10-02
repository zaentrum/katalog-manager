package tmdb

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// A title's credits follow TMDB. Whenever the catalog reads a title's TMDB
// credits — enriching it, the refreshPeople backfill, the change-list refresh —
// they replace the title's credits: a person TMDB lists is credited, a credit
// TMDB no longer lists goes, and a person no title credits after that is
// deleted (and logged). Only a title whose metadata is locked, or whose
// lockedfields name its credits, keeps them as they are.
//
// This is safe because every credit in the catalog is TMDB's: there is no way
// to add one by hand. A feature that adds credits by hand must mark the ones
// it adds (a source on com_nalet_katalog_itempeople) and replaceCredits must
// keep the marked ones — or it must lock the title's credits.

// The fields of a title that lock its credits, as the library record's
// metadata.json names them ("people" as the catalog's tables do).
const (
	fieldCredits = "credits"
	fieldPeople  = "people"
)

// creditChange says what replacing one title's credits did.
type creditChange struct {
	locked   bool     // the title keeps its credits: nothing changed
	matched  int      // people without a TMDB id that a credit gave theirs
	created  int      // credited people the catalog did not hold yet
	added    int      // credits the title gained
	dropped  int      // credits TMDB no longer lists, gone
	relinked int      // credits that were on a namesake and are now on the person credited
	deleted  int      // people no title credits any more, deleted and logged
	people   []string // the people TMDB credits, by catalog id, each once
}

// link is one credit: a person in a role.
type link struct{ person, role string }

// replaceCredits makes a title's credits TMDB's list c, in one transaction,
// unless the title keeps its credits (metadatalocked, or credits or people in
// its lockedfields). Each credited person is found by TMDB id (a person known
// only by name is matched by it once), or created. The people whom no title
// credits once the title's dropped credits are gone are deleted with their
// images and recorded in the deletion log, attributed to whoever asked (the
// principal on ctx, or the service). On any failure nothing changes.
func (s *Service) replaceCredits(ctx context.Context, itemID string, c *tmdbCredits) (creditChange, error) {
	var out creditChange
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		out = creditChange{}
		var title string
		var lk locks
		var lockedRaw []byte
		err := tx.QueryRow(ctx, `SELECT title, metadatalocked, to_jsonb(i)->'lockedfields'
			FROM com_nalet_katalog_items i WHERE id = $1 FOR UPDATE`, itemID).Scan(&title, &lk.all, &lockedRaw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted meanwhile: no credits to replace
		}
		if err != nil {
			return err
		}
		lk.fields = jsonStrings(lockedRaw)
		if lk.has(fieldCredits) || lk.has(fieldPeople) {
			out.locked = true
			return nil
		}

		// What TMDB credits: each person once per role, in TMDB's order.
		want := map[link]string{} // → the credited name
		var order []link
		seen := map[string]bool{}
		credit := func(role string, cr tmdbCredit) error {
			name := clip(oneLine(cr.Name), 255)
			if name == "" {
				return nil
			}
			id, how, err := findOrCreatePerson(ctx, tx, cr.ID, name)
			if err != nil {
				return fmt.Errorf("credit %q (TMDB person %d): %w", name, cr.ID, err)
			}
			switch how {
			case personMatched:
				out.matched++
			case personCreated:
				out.created++
			}
			if l := (link{id, role}); want[l] == "" {
				want[l] = name
				order = append(order, l)
			}
			if !seen[id] {
				seen[id] = true
				out.people = append(out.people, id)
			}
			return nil
		}
		for _, d := range c.Crew {
			if err := credit(roleDirector, d); err != nil {
				return err
			}
		}
		for _, a := range c.Cast {
			if err := credit(roleActor, a); err != nil {
				return err
			}
		}

		// What the title credits now.
		rows, err := tx.Query(ctx, `SELECT ip.id, ip.person_id, ip.role, COALESCE(p.name, '')
			FROM com_nalet_katalog_itempeople ip LEFT JOIN com_nalet_katalog_people p ON p.id = ip.person_id
			WHERE ip.item_id = $1 ORDER BY ip.id FOR UPDATE OF ip`, itemID)
		if err != nil {
			return err
		}
		type held struct{ id, person, role, name string }
		var have []held
		for rows.Next() {
			var h held
			if err := rows.Scan(&h.id, &h.person, &h.role, &h.name); err != nil {
				rows.Close()
				return err
			}
			have = append(have, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		kept := map[link]bool{}
		var drop, uncredited []string
		var dropped []held
		for _, h := range have {
			l := link{h.person, h.role}
			if want[l] != "" && !kept[l] {
				kept[l] = true
				continue
			}
			drop, uncredited, dropped = append(drop, h.id), append(uncredited, h.person), append(dropped, h)
		}
		var addPeople, addRoles []string
		gained := map[string]int{} // role + name → credits gained under that name
		for _, l := range order {
			if !kept[l] {
				addPeople, addRoles = append(addPeople, l.person), append(addRoles, l.role)
				gained[l.role+"\x00"+want[l]]++
			}
		}
		for _, h := range dropped { // a credit that moved from a namesake to the person credited
			if k := h.role + "\x00" + h.name; h.name != "" && gained[k] > 0 {
				gained[k]--
				out.relinked++
			}
		}
		out.added, out.dropped = len(addPeople)-out.relinked, len(drop)-out.relinked

		if len(drop) > 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itempeople WHERE id = ANY($1::text[])`, drop); err != nil {
				return err
			}
		}
		if len(addPeople) > 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
				SELECT gen_random_uuid()::varchar, $1, p, r FROM unnest($2::text[], $3::text[]) AS c(p, r)`,
				itemID, addPeople, addRoles); err != nil {
				return err
			}
		}
		out.deleted, err = store.DeleteUncreditedPeople(ctx, tx, uncredited, store.Deletion{
			By:     auth.Actor(ctx, "katalog-manager/tmdb"),
			Reason: fmt.Sprintf("no title credits them any more: TMDB's credits of %q no longer list them", title),
		})
		return err
	})
	return out, err
}

// inTx runs fn in a transaction and commits it, and runs it again (up to three
// times) when the database picked it to break a deadlock with another title's
// credits.
func (s *Service) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	for attempt := 1; ; attempt++ {
		err := pgx.BeginFunc(ctx, s.pool, fn)
		var pe *pgconn.PgError
		if err == nil || attempt == 3 || !errors.As(err, &pe) || (pe.Code != "40P01" && pe.Code != "40001") {
			return err
		}
	}
}

// How findOrCreatePerson came by a person.
type personFind int

const (
	personFound   personFind = iota // by TMDB id, or by name for a credit without one
	personMatched                   // a person without a TMDB id, by name: they carry it now
	personCreated                   // new
)

// findOrCreatePerson returns, in tx, the person a credit names: the one with
// its TMDB id; failing that the first person of that name who has no TMDB id
// yet (the people saved before TMDB ids were kept), who gets the id; failing
// that a new person. A person who has a TMDB id is never taken for another, so
// two people of one name stay two. The person found is locked against a
// deletion until tx ends. Two transactions that meet the same new person at
// once end up with the same row: the TMDB id is unique.
func findOrCreatePerson(ctx context.Context, tx pgx.Tx, tmdbID int64, name string) (string, personFind, error) {
	if tmdbID <= 0 {
		return personByName(ctx, tx, name)
	}
	key := strconv.FormatInt(tmdbID, 10)
	for attempt := 0; attempt < 3; attempt++ {
		var id string
		err := tx.QueryRow(ctx,
			`SELECT id FROM com_nalet_katalog_people WHERE tmdbpersonid = $1 FOR SHARE`, key).Scan(&id)
		if err == nil {
			return id, personFound, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		// In a savepoint: the id may be given to someone else meanwhile, and a
		// failed statement would end the whole transaction.
		err = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error {
			return sp.QueryRow(ctx, `UPDATE com_nalet_katalog_people SET
					tmdbpersonid = $1,
					fieldorigins = COALESCE(fieldorigins, '{}'::jsonb) || '{"externalIds": "tmdb"}'::jsonb,
					modifiedat = now()
				WHERE id = (SELECT id FROM com_nalet_katalog_people
				            WHERE name = $2 AND tmdbpersonid IS NULL ORDER BY id LIMIT 1)
				  AND tmdbpersonid IS NULL
				RETURNING id`, key, name).Scan(&id)
		})
		if err == nil {
			return id, personMatched, nil
		}
		if isUniqueViolation(err) {
			continue // the id was given to someone meanwhile: find them
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", 0, err
		}
		err = tx.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people
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
		// created meanwhile by another transaction: find them on the next pass
	}
	return "", 0, fmt.Errorf("TMDB person %s was created and taken concurrently; giving up", key)
}

// personByName is how a credit without a TMDB id finds its person: by name,
// preferring someone without a TMDB id, as before ids were kept.
func personByName(ctx context.Context, tx pgx.Tx, name string) (string, personFind, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM com_nalet_katalog_people WHERE name = $1
		ORDER BY tmdbpersonid IS NULL DESC, id LIMIT 1 FOR SHARE`, name).Scan(&id)
	if err == nil {
		return id, personFound, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO com_nalet_katalog_people (id, name, fieldorigins, createdat, modifiedat)
		VALUES (gen_random_uuid()::varchar, $1, '{"name": "tmdb"}', now(), now()) RETURNING id`, name).Scan(&id)
	return id, personCreated, err
}
