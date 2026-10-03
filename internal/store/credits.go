package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// creditDetailColumns are the columns db/migrations/032_credit_details.sql adds
// to com_nalet_katalog_itempeople.
var creditDetailColumns = []string{"job", "charactername", "ordinal", "episodecount"}

// CreditDetailsReady reports whether migration 032 is in place: every column
// it adds to the credits table. Without it a credit is a person in a role and
// nothing more.
func (s *Store) CreditDetailsReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_attribute
		WHERE attrelid = to_regclass('com_nalet_katalog_itempeople') AND attnum > 0 AND NOT attisdropped
		  AND attname = ANY($1::text[])) = cardinality($1::text[])`, creditDetailColumns).Scan(&ok)
	return ok, err
}

// EnsureCreditDetails applies db/migrations/032_credit_details.sql when any
// column it adds is missing. Without it credits keep only their roles: TMDB's
// job, character, order and episodes of a credit are not kept. The check comes
// first for the reason EnsurePeople gives.
func (s *Store) EnsureCreditDetails(ctx context.Context) error {
	ready, err := s.CreditDetailsReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.CreditDetails)
	return err
}
