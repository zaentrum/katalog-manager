package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 030 a person has these columns, and so do their images and the
// change-list cursors.
var peopleShape = map[string][]string{
	"com_nalet_katalog_people": {
		"id character varying(36) NOT NULL",
		"name character varying(255) NOT NULL",
		"sortname character varying(255) NULL",
		"alsoknownas jsonb NULL",
		"birthdate date NULL",
		"deathdate date NULL",
		"birthplace text NULL",
		"biography jsonb NULL",
		"tmdbpersonid text NULL",
		"imdbid text NULL",
		"knownfordepartment text NULL",
		"metadatalocked boolean NOT NULL",
		"lockedfields jsonb NULL",
		"fieldorigins jsonb NULL",
		"tmdbfetchedat timestamp with time zone NULL",
		"tmdbchangedat date NULL",
		"createdat timestamp with time zone NULL",
		"modifiedat timestamp with time zone NULL",
	},
	"com_nalet_katalog_personartwork": {
		"id character varying(36) NOT NULL",
		"person_id character varying(36) NOT NULL",
		"kind character varying(20) NOT NULL",
		"contenttype character varying(80) NOT NULL",
		"bytes bytea NOT NULL",
		"sha256 character(64) NOT NULL",
		"width integer NULL",
		"height integer NULL",
		"isprimary boolean NOT NULL",
		"sourcepath character varying(2048) NULL",
		"fetchedat timestamp with time zone NULL",
	},
	"com_nalet_katalog_referencesync": {
		"kind text NOT NULL",
		"cursor date NOT NULL",
		"lastrunat timestamp with time zone NULL",
		"lastrunchanges integer NULL",
		"lastrunmatched integer NULL",
		"lastrunrefreshed integer NULL",
		"lastrunskipped integer NULL",
		"lastrunfailed integer NULL",
		"lastrunerror text NULL",
	},
}

// Each table's indexes and check constraints after 030, so that a run that
// added any of them twice shows.
var peopleObjects = map[string][2]int{ // table: {indexes, checks}
	"com_nalet_katalog_people":        {2, 6}, // the primary key and tmdbpersonid; six JSON/id checks
	"com_nalet_katalog_personartwork": {3, 2}, // the primary key, person_id, one primary per kind; kind and sha256
	"com_nalet_katalog_referencesync": {1, 1}, // the primary key; kind
}

// The migration runs wherever a deployment applies db/migrations and again at
// startup when one of its objects is missing, so running it on a database that
// already has it must change nothing and fail nothing.
func TestPeopleMigrationIsIdempotent(t *testing.T) {
	st := storetest.Open(t) // has applied 030 once, through EnsurePeople
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid, biography, metadatalocked)
		VALUES ('p1', 'Kept Person', '42', '{"en": "Kept."}', true)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary)
		VALUES ('a1', 'p1', 'image/jpeg', '\xffd8ff', repeat('0', 64), true)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_referencesync (kind, cursor) VALUES ('person', '2026-10-01')`)

	for run := 2; run <= 3; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.People); err != nil {
			t.Fatalf("applying 030 for the %d. time: %v", run, err)
		}
	}
	if err := st.EnsurePeople(ctx); err != nil {
		t.Fatalf("startup check on a database that has 030: %v", err)
	}

	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people
		WHERE id = 'p1' AND name = 'Kept Person' AND tmdbpersonid = '42' AND biography = '{"en": "Kept."}' AND metadatalocked`); n != 1 {
		t.Error("re-running the migration must keep what a person holds")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_personartwork WHERE id = 'a1' AND isprimary`); n != 1 {
		t.Error("re-running the migration must keep a person's images")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_referencesync WHERE cursor = '2026-10-01'`); n != 1 {
		t.Error("re-running the migration must keep the change-list cursors")
	}
	for table, want := range peopleShape {
		if got := columns(t, st, table); got != strings.Join(want, "\n") {
			t.Errorf("%s after three runs:\n%s\nwant:\n%s", table, got, strings.Join(want, "\n"))
		}
	}
	for table, want := range peopleObjects {
		idx := storetest.Count(t, st, `SELECT count(*) FROM pg_index WHERE indrelid = to_regclass($1)`, table)
		chk := storetest.Count(t, st, `SELECT count(*) FROM pg_constraint WHERE conrelid = to_regclass($1) AND contype = 'c'`, table)
		if idx != want[0] || chk != want[1] {
			t.Errorf("%s after three runs: %d indexes and %d checks, want %d and %d", table, idx, chk, want[0], want[1])
		}
	}
}

// A catalog older than 030 knows each person by an id and a name. The
// migration keeps them, unlocked, with no creation time it could not know,
// and a person created afterwards is stamped with theirs.
func TestEnsurePeopleMigratesPeopleWhoHaveOnlyAName(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada Example'), ('p2', 'Ben Example')`)
	if ready, err := st.PeopleReady(ctx); err != nil || ready {
		t.Fatalf("PeopleReady on a catalog without 030: %v, %v", ready, err)
	}

	if err := st.EnsurePeople(ctx); err != nil {
		t.Fatalf("EnsurePeople on a catalog without 030: %v", err)
	}
	if ready, err := st.PeopleReady(ctx); err != nil || !ready {
		t.Fatalf("PeopleReady after EnsurePeople: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_people
		WHERE name IN ('Ada Example', 'Ben Example') AND NOT metadatalocked AND createdat IS NULL
		  AND tmdbpersonid IS NULL AND modifiedat IS NULL`); n != 2 {
		t.Fatalf("%d of the 2 people came through unchanged, unlocked and without a made-up creation time", n)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p3', 'New Example')`)
	var created *time.Time
	if err := st.Pool().QueryRow(ctx, `SELECT createdat FROM com_nalet_katalog_people WHERE id = 'p3'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created == nil || time.Since(*created) > time.Minute {
		t.Fatalf("a person created after the migration has createdat %v, want now", created)
	}
}

// The startup check looks at every object 030 makes: one that is missing
// brings the migration back, and when none is, nothing runs.
func TestEnsurePeopleCompletesAndOnlyThen(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()

	storetest.Exec(t, st, `DROP INDEX idx_personartwork_primary`)
	if err := st.EnsurePeople(ctx); err != nil {
		t.Fatal(err)
	}
	if storetest.Count(t, st, `SELECT count(*) FROM pg_class WHERE oid = to_regclass('idx_personartwork_primary')`) != 1 {
		t.Fatal("a missing index of 030 was not brought back")
	}

	// With every object in place the migration does not run: the default it
	// sets stays dropped.
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_people ALTER COLUMN createdat DROP DEFAULT`)
	if err := st.EnsurePeople(ctx); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_attrdef
		WHERE adrelid = to_regclass('com_nalet_katalog_people')`); n != 1 { // metadatalocked's default only
		t.Fatalf("EnsurePeople ran the migration on a database that has it (%d defaults)", n)
	}
}

// What the tables refuse: a TMDB id twice, a second primary image of a kind,
// values of the wrong shape.
func TestPeopleConstraints(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES
		('p1', 'One', '7'), ('p2', 'Two', NULL), ('p3', 'Three', NULL)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary) VALUES
		('a1', 'p1', 'image/jpeg', '\x01', repeat('a', 64), true),
		('a2', 'p1', 'image/jpeg', '\x02', repeat('b', 64), false)`)

	for _, bad := range []string{
		`INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('p4', 'Four', '7')`,
		`INSERT INTO com_nalet_katalog_people (id, name, tmdbpersonid) VALUES ('p4', 'Four', 'tt7')`,
		`INSERT INTO com_nalet_katalog_people (id, name, imdbid) VALUES ('p4', 'Four', 'tt0000001')`,
		`INSERT INTO com_nalet_katalog_people (id, name, alsoknownas) VALUES ('p4', 'Four', '{"a": 1}')`,
		`INSERT INTO com_nalet_katalog_people (id, name, biography) VALUES ('p4', 'Four', '["text"]')`,
		`INSERT INTO com_nalet_katalog_people (id, name, lockedfields) VALUES ('p4', 'Four', '"name"')`,
		`INSERT INTO com_nalet_katalog_people (id, name, fieldorigins) VALUES ('p4', 'Four', '[]')`,
		`INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary)
			VALUES ('a3', 'p1', 'image/jpeg', '\x03', repeat('c', 64), true)`,
		`INSERT INTO com_nalet_katalog_personartwork (id, person_id, kind, contenttype, bytes, sha256)
			VALUES ('a3', 'p1', 'poster', 'image/jpeg', '\x03', repeat('c', 64))`,
		`INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256)
			VALUES ('a3', 'p1', 'image/jpeg', '\x03', 'sha256:' || repeat('c', 57))`,
		`INSERT INTO com_nalet_katalog_referencesync (kind, cursor) VALUES ('episode', '2026-10-01')`,
		`INSERT INTO com_nalet_katalog_referencesync (kind) VALUES ('tv')`,
	} {
		if _, err := st.Pool().Exec(ctx, bad); err == nil {
			t.Errorf("accepted: %s", strings.Join(strings.Fields(bad), " "))
		}
	}
	// Any number of people without a TMDB id, and a second primary for another person.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p5', 'Five')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork (id, person_id, contenttype, bytes, sha256, isprimary)
		VALUES ('a4', 'p2', 'image/png', '\x04', repeat('d', 64), true)`)
}

// An item's people come with their details, by role and name; on a catalog
// without 030 with their names.
func TestPeopleByItem(t *testing.T) {
	for _, withPeople := range []bool{true, false} {
		open := storetest.Open
		if !withPeople {
			open = storetest.OpenBase
		}
		st := open(t)
		ctx := context.Background()
		storetest.AddItem(t, st, movieA, "movie", "A Film", "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada Example'), ('p2', 'Ben Example')`)
		if withPeople {
			storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET tmdbpersonid = '101', birthdate = '1815-12-10',
				biography = '{"en": "English."}' WHERE id = 'p1'`)
		}
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
			('l1', $1, 'p2', 'director'), ('l2', $1, 'p1', 'actor')`, movieA)

		links, people, err := st.PeopleByItem(ctx, movieA)
		if err != nil {
			t.Fatalf("with 030 %v: %v", withPeople, err)
		}
		if len(links) != 2 || len(people) != 2 || links[0].ID != "l2" || links[0].Role != "actor" ||
			people[0].ID != "p1" || people[0].Name != "Ada Example" || links[1].PersonID != "p2" || people[1].Name != "Ben Example" {
			t.Fatalf("with 030 %v: links %+v, people %+v", withPeople, links, people)
		}
		ada := people[0]
		if withPeople != (ada.TmdbPersonID != nil && *ada.TmdbPersonID == "101" && ada.BirthDate != nil &&
			*ada.BirthDate == "1815-12-10" && ada.Biography["en"] == "English.") {
			t.Errorf("with 030 %v: Ada's details %+v", withPeople, *ada)
		}
	}
}

// 031 gives a title its lockedfields: a JSON array, NULL for every title older
// than it; running it again changes nothing, and the startup check adds it
// where it is missing.
func TestItemLockedFieldsMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Kept Film", "")
	for run := 1; run <= 2; run++ {
		if err := st.EnsureItemLockedFields(ctx); err != nil {
			t.Fatalf("EnsureItemLockedFields, %d. time: %v", run, err)
		}
	}
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.ItemLockedFields); err != nil {
			t.Fatalf("applying 031 for the %d. time: %v", run, err)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND lockedfields IS NULL`, movieA); n != 1 {
		t.Error("a title older than 031 must come through with no locked fields")
	}
	if got := columns(t, st, "com_nalet_katalog_items"); !strings.HasSuffix(got, "metadatalocked boolean NOT NULL\nlockedfields jsonb NULL") {
		t.Errorf("items after 031:\n%s", got)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = to_regclass('com_nalet_katalog_items') AND contype = 'c'`); n != 1 {
		t.Errorf("%d checks on items after four runs, want 1", n)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET lockedfields = '["credits"]' WHERE id = $1`, movieA)
	if _, err := st.Pool().Exec(ctx, `UPDATE com_nalet_katalog_items SET lockedfields = '{"credits": true}' WHERE id = $1`, movieA); err == nil {
		t.Error("lockedfields took an object")
	}
}
