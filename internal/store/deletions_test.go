package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The migration runs wherever a deployment applies db/migrations and again at
// every startup when its table is missing, so running it on a database that
// already has it must change nothing and fail nothing.
func TestDeletionLogMigrationIsIdempotent(t *testing.T) {
	st := storetest.Open(t) // has applied 029 once, through EnsureDeletionLog
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedby)
		VALUES ('11111111-1111-4111-8111-111111111111', 'movie', 'Kept', 'test')`)

	for run := 2; run <= 3; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.DeletedItems); err != nil {
			t.Fatalf("applying 029 for the %d. time: %v", run, err)
		}
	}
	if err := st.EnsureDeletionLog(ctx); err != nil {
		t.Fatalf("startup check on a database that has the log: %v", err)
	}

	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 1 {
		t.Fatalf("re-running the migration must keep what the log holds: %d rows, want 1", n)
	}
	if got, want := columns(t, st), strings.Join([]string{
		"id character varying(36) NOT NULL",
		"type character varying(20) NOT NULL",
		"title character varying(255) NOT NULL",
		"deletedat timestamp without time zone NOT NULL",
		"deletedby character varying(255) NOT NULL",
		"reason character varying(500) NULL",
	}, "\n"); got != want {
		t.Errorf("columns after three runs:\n%s\nwant:\n%s", got, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'com_nalet_katalog_deleteditems'`); n != 2 {
		t.Errorf("indexes after three runs: %d, want 2 (the primary key and deletedat)", n)
	}
}

// At startup the service creates the log when a deployment has not.
func TestEnsureDeletionLogCreatesAMissingLog(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_deleteditems`)

	if err := st.EnsureDeletionLog(context.Background()); err != nil {
		t.Fatalf("EnsureDeletionLog on a database without the log: %v", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'idx_deleteditems_deletedat'`); n != 1 {
		t.Fatal("the log must come back with its deletedat index")
	}
}

// deletedat is UTC whatever the session's time zone. Sessions here run at
// UTC+14, so a default that followed the session would be off by 14 hours.
func TestDeletionLogDefaultIsUTC(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedby)
		VALUES ('22222222-2222-4222-8222-222222222222', 'movie', 'Now', 'test')`)

	var at time.Time
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT deletedat FROM com_nalet_katalog_deleteditems`).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(at); d < -time.Minute || d > time.Minute {
		t.Fatalf("deletedat %s is %s away from now in UTC", at.Format(time.RFC3339), d.Round(time.Minute))
	}
}

// The log reads newest first; since is inclusive and means an instant, in
// whatever zone it is given; limit caps the rows.
func TestListDeletedItems(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedat, deletedby, reason) VALUES
		('a', 'movie', 'Oldest', '2026-09-30 22:30:00', 'subject-1', NULL),
		('b', 'episode', 'Middle', '2026-10-01 08:00:00', 'katalog-manager/scanner', NULL),
		('c', 'series', 'Newest', '2026-10-02 12:00:00', 'subject-2', 'a duplicate')`)

	ids := func(since *time.Time, limit int32) string {
		t.Helper()
		ds, err := st.ListDeletedItems(ctx, since, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(nil, 0); got != "c,b,a" {
		t.Errorf("all, newest first: %s", got)
	}
	// 10:00 at UTC+2 is 08:00 UTC: b's own instant, so b is included.
	since := time.Date(2026, 10, 1, 10, 0, 0, 0, time.FixedZone("UTC+2", 2*3600))
	if got := ids(&since, 0); got != "c,b" {
		t.Errorf("since %s: %s, want c,b", since.Format(time.RFC3339), got)
	}
	if got := ids(nil, 1); got != "c" {
		t.Errorf("limit 1: %s, want c", got)
	}

	ds, err := st.ListDeletedItems(ctx, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := ds[0]
	if c.Type != "series" || c.Title != "Newest" || c.DeletedBy != "subject-2" || c.Reason == nil ||
		*c.Reason != "a duplicate" || !c.DeletedAt.Equal(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("row = %+v", *c)
	}
}

// columns lists the deletion log's columns as "name type NULL|NOT NULL".
func columns(t *testing.T, st *store.Store) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `
		SELECT attname, format_type(atttypid, atttypmod), attnotnull
		FROM pg_attribute
		WHERE attrelid = 'com_nalet_katalog_deleteditems'::regclass AND attnum > 0 AND NOT attisdropped
		ORDER BY attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, typ string
		var notNull bool
		if err := rows.Scan(&name, &typ, &notNull); err != nil {
			t.Fatal(err)
		}
		null := "NULL"
		if notNull {
			null = "NOT NULL"
		}
		out = append(out, fmt.Sprintf("%s %s %s", name, typ, null))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}
