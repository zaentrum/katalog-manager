package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 032 a credit has these columns: the four it adds are nullable.
const creditShape = `id character varying(36) NOT NULL
item_id character varying(36) NOT NULL
person_id character varying(36) NOT NULL
role character varying(40) NOT NULL
job character varying(255) NULL
charactername text NULL
ordinal integer NULL
episodecount integer NULL`

// 032 gives a credit its job, character, order and episodes, unknown (NULL)
// for every credit older than it; running it again changes nothing, and the
// startup check adds it where any of it is missing, and only then.
func TestCreditDetailsMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
		VALUES ('l1', $1, 'p1', 'actor')`, movieA)
	if ready, err := st.CreditDetailsReady(ctx); err != nil || ready {
		t.Fatalf("CreditDetailsReady on a catalog without 032: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureCreditDetails(ctx); err != nil {
			t.Fatalf("EnsureCreditDetails, %d. time: %v", run, err)
		}
	}
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.CreditDetails); err != nil {
			t.Fatalf("applying 032 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.CreditDetailsReady(ctx); err != nil || !ready {
		t.Fatalf("CreditDetailsReady after EnsureCreditDetails: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_itempeople"); got != creditShape {
		t.Errorf("credits after 032:\n%s\nwant:\n%s", got, creditShape)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople WHERE id = 'l1' AND role = 'actor'
		AND job IS NULL AND charactername IS NULL AND ordinal IS NULL AND episodecount IS NULL`); n != 1 {
		t.Error("a credit older than 032 must come through as it was, its details unknown")
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itempeople SET job = 'Writer, Co-Writer', charactername = 'Sintel',
		ordinal = 0, episodecount = 7 WHERE id = 'l1'`)

	// A column missing brings the migration back; the others keep what they hold.
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_itempeople DROP COLUMN episodecount`)
	if ready, err := st.CreditDetailsReady(ctx); err != nil || ready {
		t.Fatalf("CreditDetailsReady with a column of 032 missing: %v, %v", ready, err)
	}
	if err := st.EnsureCreditDetails(ctx); err != nil {
		t.Fatal(err)
	}
	if got := columns(t, st, "com_nalet_katalog_itempeople"); got != creditShape {
		t.Errorf("credits after the startup check:\n%s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itempeople WHERE id = 'l1'
		AND job = 'Writer, Co-Writer' AND charactername = 'Sintel' AND ordinal = 0 AND episodecount IS NULL`); n != 1 {
		t.Error("bringing a column back changed what the others hold")
	}
}

// storetest.Open gives a test every migration the service applies at startup,
// 032 among them.
func TestTheTestSchemaHasTheCreditDetails(t *testing.T) {
	st := storetest.Open(t)
	if ready, err := st.CreditDetailsReady(context.Background()); err != nil || !ready {
		t.Fatalf("CreditDetailsReady in a test schema: %v, %v", ready, err)
	}
	if got := columns(t, st, "com_nalet_katalog_itempeople"); !strings.HasSuffix(got, "episodecount integer NULL") {
		t.Errorf("credits in a test schema:\n%s", got)
	}
}
