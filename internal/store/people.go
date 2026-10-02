package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
)

// personCols is a person as migration 030 keeps them; personBaseCols as a
// catalog without it does. Both read com_nalet_katalog_people as p.
const (
	personCols = `p.id, p.name, p.sortname, p.alsoknownas, to_char(p.birthdate, 'YYYY-MM-DD'),
		to_char(p.deathdate, 'YYYY-MM-DD'), p.birthplace, p.biography, p.tmdbpersonid, p.imdbid,
		p.knownfordepartment, p.metadatalocked, p.lockedfields, p.fieldorigins, p.tmdbfetchedat,
		to_char(p.tmdbchangedat, 'YYYY-MM-DD'), p.createdat, p.modifiedat`
	personBaseCols = `p.id, p.name`
)

func scanPerson(row pgx.Rows, full bool, lead []any, p *model.Person) error {
	if !full {
		return row.Scan(append(lead, &p.ID, &p.Name)...)
	}
	var aka, bio, locked, origins []byte
	if err := row.Scan(append(lead, &p.ID, &p.Name, &p.SortName, &aka, &p.BirthDate, &p.DeathDate,
		&p.BirthPlace, &bio, &p.TmdbPersonID, &p.ImdbID, &p.KnownForDepartment, &p.MetadataLocked,
		&locked, &origins, &p.TmdbFetchedAt, &p.TmdbChangedAt, &p.CreatedAt, &p.ModifiedAt)...); err != nil {
		return err
	}
	p.AlsoKnownAs, p.LockedFields = jsonStrings(aka), jsonStrings(locked)
	p.Biography, p.FieldOrigins = jsonStringMap(bio), jsonStringMap(origins)
	p.TmdbFetchedAt, p.CreatedAt, p.ModifiedAt = utc(p.TmdbFetchedAt), utc(p.CreatedAt), utc(p.ModifiedAt)
	return nil
}

// utc is t in UTC: a timestamptz arrives in the process's zone.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// queryPeople runs a people query written with {cols} for the person
// columns, in full; on a catalog without migration 030 (a column it names is
// missing) it runs it again with the id and name alone. each gets every row:
// the lead values the query selects before the person, and the person.
func (s *Store) queryPeople(ctx context.Context, sql func(cols string) string, lead func() []any,
	each func(p *model.Person), args ...any) error {
	err := s.peopleRows(ctx, sql(personCols), true, lead, each, args...)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42703" { // undefined_column: older than 030
		return s.peopleRows(ctx, sql(personBaseCols), false, lead, each, args...)
	}
	return err
}

func (s *Store) peopleRows(ctx context.Context, sql string, full bool, lead func() []any,
	each func(p *model.Person), args ...any) error {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p model.Person
		if err := scanPerson(rows, full, lead(), &p); err != nil {
			return err
		}
		each(&p)
	}
	return rows.Err()
}

func noLead() []any { return nil }

// ListPeople returns every person, by name.
func (s *Store) ListPeople(ctx context.Context) ([]*model.Person, error) {
	var out []*model.Person
	err := s.queryPeople(ctx, func(cols string) string {
		return `SELECT ` + cols + ` FROM com_nalet_katalog_people p ORDER BY p.name, p.id`
	}, noLead, func(p *model.Person) { out = append(out, p) })
	return out, err
}

// GetPerson returns one person, or nil.
func (s *Store) GetPerson(ctx context.Context, id string) (*model.Person, error) {
	var out *model.Person
	err := s.queryPeople(ctx, func(cols string) string {
		return `SELECT ` + cols + ` FROM com_nalet_katalog_people p WHERE p.id = $1`
	}, noLead, func(p *model.Person) { out = p }, id)
	return out, err
}

// ListReferenceSync returns the cursors of TMDB's change lists, by kind; none
// on a catalog without migration 030.
func (s *Store) ListReferenceSync(ctx context.Context) ([]*model.ReferenceSync, error) {
	rows, err := s.pool.Query(ctx, `SELECT kind, to_char(cursor, 'YYYY-MM-DD'), lastrunat, lastrunchanges,
		lastrunmatched, lastrunrefreshed, lastrunskipped, lastrunfailed, lastrunerror
		FROM com_nalet_katalog_referencesync ORDER BY kind`)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42P01" { // undefined_table: older than 030
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ReferenceSync
	for rows.Next() {
		var r model.ReferenceSync
		if err := rows.Scan(&r.Kind, &r.Cursor, &r.LastRunAt, &r.LastRunChanges, &r.LastRunMatched,
			&r.LastRunRefreshed, &r.LastRunSkipped, &r.LastRunFailed, &r.LastRunError); err != nil {
			return nil, err
		}
		r.LastRunAt = utc(r.LastRunAt)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// jsonStrings reads a JSON array of strings; whatever else it holds is skipped.
func jsonStrings(raw []byte) []string {
	var vs []any
	_ = json.Unmarshal(raw, &vs)
	out := []string{}
	for _, v := range vs {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// jsonStringMap reads a JSON object of strings; whatever else it holds is skipped.
func jsonStringMap(raw []byte) map[string]string {
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	out := map[string]string{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// peopleColumns are the columns db/migrations/030_people.sql adds to
// com_nalet_katalog_people.
var peopleColumns = []string{
	"sortname", "alsoknownas", "birthdate", "deathdate", "birthplace", "biography",
	"tmdbpersonid", "imdbid", "knownfordepartment", "metadatalocked", "lockedfields",
	"fieldorigins", "tmdbfetchedat", "tmdbchangedat", "createdat", "modifiedat",
}

// PeopleReady reports whether migration 030 is in place: every column it adds
// to the people table, the person artwork and change-list cursor tables, and
// their indexes.
func (s *Store) PeopleReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_attribute
		 WHERE attrelid = to_regclass('com_nalet_katalog_people') AND attnum > 0 AND NOT attisdropped
		   AND attname = ANY($1::text[])) = cardinality($1::text[])
		AND to_regclass('idx_people_tmdbpersonid') IS NOT NULL
		AND to_regclass('com_nalet_katalog_personartwork') IS NOT NULL
		AND to_regclass('idx_personartwork_person') IS NOT NULL
		AND to_regclass('idx_personartwork_primary') IS NOT NULL
		AND to_regclass('com_nalet_katalog_referencesync') IS NOT NULL`, peopleColumns).Scan(&ok)
	return ok, err
}

// EnsurePeople applies db/migrations/030_people.sql when any of its objects is
// missing. Without it people keep only their names: enrichment links credits
// by name as it always did, and nothing about a person is fetched or kept.
//
// The check comes first for the reason EnsureDeletionLog gives: ALTER TABLE
// needs the table's owner even when every column already exists, so a role
// that may write the tables but not alter them starts cleanly once the
// migration has been applied by hand.
func (s *Store) EnsurePeople(ctx context.Context) error {
	ready, err := s.PeopleReady(ctx)
	if err != nil || ready {
		return err
	}
	// No arguments, so pgx sends the file over the simple protocol: all of its
	// statements run, as one implicit transaction.
	_, err = s.pool.Exec(ctx, migrations.People)
	return err
}
