package store_test

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 036 a title has these columns: the five it adds after the base
// schema's, nothing in them for a title older than it.
const ratedItemShape = `id character varying(36) NOT NULL
createdat timestamp without time zone NULL
createdby character varying(255) NULL
modifiedat timestamp without time zone NULL
modifiedby character varying(255) NULL
type character varying(20) NOT NULL
title character varying(255) NOT NULL
sorttitle character varying(255) NULL
year integer NULL
description text NULL
rating numeric(3,1) NULL
durationms bigint NULL
parent_id character varying(36) NULL
seasonnumber integer NULL
episodenumber integer NULL
tagline character varying(500) NULL
metadatalocked boolean NOT NULL
certification character varying(40) NULL
certification_country character varying(2) NULL
min_age smallint NULL
min_age_override smallint NULL
certification_fetched_at timestamp with time zone NULL`

// 036 gives a title its certification, its country, the minimum age it
// means, an admin's override and when TMDB was read, none of them for a title
// older than it, and the index of the age a title without a parent is held
// to; running it again changes nothing, and the startup check applies it
// where any of it is missing, and only then.
func TestItemRatingsMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	if ready, err := st.ItemRatingsReady(ctx); err != nil || ready {
		t.Fatalf("ItemRatingsReady on a catalog without 036: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureItemRatings(ctx); err != nil {
			t.Fatalf("EnsureItemRatings, %d. time: %v", run, err)
		}
	}
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.ItemRatings); err != nil {
			t.Fatalf("applying 036 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.ItemRatingsReady(ctx); err != nil || !ready {
		t.Fatalf("ItemRatingsReady after EnsureItemRatings: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_items"); got != ratedItemShape {
		t.Errorf("items after 036:\n%s\nwant:\n%s", got, ratedItemShape)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 'm1' AND title = 'A Film'
		AND certification IS NULL AND certification_country IS NULL AND min_age IS NULL AND min_age_override IS NULL
		AND certification_fetched_at IS NULL`); n != 1 {
		t.Error("a title older than 036 must come through as it was, unrated")
	}
	// pg_get_indexdef of this schema's index alone: pg_indexes would read the
	// indexes of every schema, those other tests drop meanwhile too.
	if n := storetest.Count(t, st, `SELECT count(*) WHERE pg_get_indexdef(to_regclass('idx_items_rated_age'))
		LIKE '%ON ' || current_schema() || '.com_nalet_katalog_items %(COALESCE(min_age_override, min_age))%'`); n != 1 {
		t.Error("idx_items_rated_age does not index the age of a title without a parent")
	}

	// An age is 0 to 21 years, a country two capital letters.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET certification = '12', certification_country = 'DE',
		min_age = 12, min_age_override = 21 WHERE id = 'm1'`)
	for _, bad := range []string{`min_age = -1`, `min_age = 22`, `min_age_override = 22`,
		`certification_country = 'de'`, `certification_country = 'D'`} {
		if _, err := st.Pool().Exec(ctx, `UPDATE com_nalet_katalog_items SET `+bad+` WHERE id = 'm1'`); err == nil {
			t.Errorf("SET %s was taken", bad)
		}
	}

	// The index missing brings the migration back, and the columns keep what
	// they hold.
	storetest.Exec(t, st, `DROP INDEX idx_items_rated_age`)
	if ready, err := st.ItemRatingsReady(ctx); err != nil || ready {
		t.Fatalf("ItemRatingsReady with the index of 036 missing: %v, %v", ready, err)
	}
	if err := st.EnsureItemRatings(ctx); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.ItemRatingsReady(ctx); err != nil || !ready {
		t.Fatalf("ItemRatingsReady after the startup check: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 'm1'
		AND certification = '12' AND certification_country = 'DE' AND min_age = 12 AND min_age_override = 21`); n != 1 {
		t.Error("applying 036 again changed what a title holds")
	}
}

// storetest.Open gives a test every migration the service applies at startup,
// 036 among them.
func TestTheTestSchemaHasTheItemRatings(t *testing.T) {
	if ready, err := storetest.Open(t).ItemRatingsReady(context.Background()); err != nil || !ready {
		t.Fatalf("ItemRatingsReady on the test schema: %v, %v", ready, err)
	}
}
