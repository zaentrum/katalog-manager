package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

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
