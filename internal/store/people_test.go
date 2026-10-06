package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// After 030 a person has these columns (and 040's, when the library's
// projection was last written), and so do their images and the change-list
// cursors.
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
		"libraryprojectedat timestamp with time zone NULL",
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

// creditLine is a person's credit as the tests compare it: the credit, its
// title and the title's parent, and what TMDB says of it.
func creditLine(c *model.PersonCredit) string {
	n := func(v *int32) string {
		if v == nil {
			return "-"
		}
		return strconv.Itoa(int(*v))
	}
	s := func(v *string) string {
		if v == nil {
			return "-"
		}
		return strconv.Quote(*v)
	}
	line := fmt.Sprintf("%s %s on %s %s %q %s S%sE%s parent %s", c.ID, c.Role, c.Item.ID, c.Item.Type, c.Item.Title,
		n(c.Item.Year), n(c.Item.SeasonNumber), n(c.Item.EpisodeNumber), s(c.Item.ParentID))
	if c.Parent != nil {
		line += fmt.Sprintf(" (%s %s %q %s)", c.Parent.ID, c.Parent.Type, c.Parent.Title, n(c.Parent.Year))
	}
	return line + fmt.Sprintf(" job %s character %s order %s episodes %s", s(c.Job), s(c.Character), n(c.Order), n(c.EpisodeCount))
}

func creditLines(cs []*model.PersonCredit) string {
	lines := make([]string, 0, len(cs))
	for _, c := range cs {
		lines = append(lines, creditLine(c))
	}
	return strings.Join(lines, "\n")
}

// A person's credits come with their titles and a title's parent (an
// episode's series), newest title first: by year, a title without one in its
// parent's, unknown last; the titles of a year by name, a title under its
// parent's name after the parent, by season and episode, then by name and id;
// a title's credits by role as a title lists them, then by id. Each says what
// TMDB says of it; a credit on a title the catalog does not hold, and another
// person's, are left out.
func TestCreditsByPerson(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, year, parent_id, seasonnumber,
		episodenumber, createdat, modifiedat) VALUES
		('i-zeta', 'movie', 'Zeta', 2012, NULL, NULL, NULL, now(), now()),
		('i-beta', 'series', 'Beta Show', 2010, NULL, NULL, NULL, now(), now()),
		('i-b110', 'episode', 'Aa', 2010, 'i-beta', 1, 10, now(), now()),
		('i-b21', 'episode', 'A0', 2010, 'i-beta', 2, 1, now(), now()),
		('i-b12', 'episode', 'Ab', 2010, 'i-beta', 1, 2, now(), now()),
		('i-a-bonus', 'episode', 'Bonus', 2010, 'i-beta', NULL, NULL, now(), now()),
		('i-alpha', 'movie', 'Alpha', 2010, NULL, NULL, NULL, now(), now()),
		('i-kappa', 'series', 'Kappa', 2011, NULL, NULL, NULL, now(), now()),
		('i-k11', 'episode', 'Kappa One', NULL, 'i-kappa', 1, 1, now(), now()),
		('i-delta', 'movie', 'Delta', NULL, NULL, NULL, NULL, now(), now()),
		('i-same2', 'movie', 'Same', 2009, NULL, NULL, NULL, now(), now()),
		('i-same1', 'movie', 'Same', 2009, NULL, NULL, NULL, now(), now()),
		('i-orphan', 'episode', 'Orphan', 2008, 'i-gone', 3, 4, now(), now()),
		('i-other', 'movie', 'Not Theirs', 2013, NULL, NULL, NULL, now(), now())`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada'), ('p2', 'Ben')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('c-z1', 'i-zeta', 'p1', 'narrator'), ('c-z2', 'i-zeta', 'p1', 'gaffer'), ('c-z3', 'i-zeta', 'p1', 'writer'),
		('c-z4', 'i-zeta', 'p1', 'producer'), ('c-z5', 'i-zeta', 'p1', 'actor'),
		('c-k', 'i-k11', 'p1', 'actor'),
		('c-b2', 'i-beta', 'p1', 'creator'), ('c-b1', 'i-beta', 'p1', 'actor'),
		('c-e3', 'i-b21', 'p1', 'actor'), ('c-e2', 'i-b110', 'p1', 'actor'), ('c-e1', 'i-b12', 'p1', 'actor'),
		('c-bonus', 'i-a-bonus', 'p1', 'actor'),
		('c-a', 'i-alpha', 'p1', 'director'),
		('c-d', 'i-delta', 'p1', 'actor'), ('c-c', 'i-delta', 'p1', 'actor'),
		('c-s1', 'i-same2', 'p1', 'actor'), ('c-s2', 'i-same1', 'p1', 'actor'),
		('c-o', 'i-orphan', 'p1', 'actor'),
		('c-gone', 'i-missing', 'p1', 'actor'),
		('c-x', 'i-other', 'p2', 'actor')`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itempeople SET job = d.j, charactername = d.c, ordinal = d.o,
		episodecount = d.e FROM (VALUES ('c-z5', NULL, 'Zed', 3, NULL), ('c-z3', 'Screenplay, Novel', NULL, 0, NULL),
			('c-b1', NULL, 'Self / Host', 1, 6), ('c-b2', 'Creator', NULL, 0, NULL)) AS d(id, j, c, o, e)
		WHERE com_nalet_katalog_itempeople.id = d.id`)

	got, err := st.CreditsByPerson(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`c-z5 actor on i-zeta movie "Zeta" 2012 S-E- parent - job - character "Zed" order 3 episodes -`,
		`c-z3 writer on i-zeta movie "Zeta" 2012 S-E- parent - job "Screenplay, Novel" character - order 0 episodes -`,
		`c-z4 producer on i-zeta movie "Zeta" 2012 S-E- parent - job - character - order - episodes -`,
		`c-z2 gaffer on i-zeta movie "Zeta" 2012 S-E- parent - job - character - order - episodes -`,
		`c-z1 narrator on i-zeta movie "Zeta" 2012 S-E- parent - job - character - order - episodes -`,
		`c-k actor on i-k11 episode "Kappa One" - S1E1 parent "i-kappa" (i-kappa series "Kappa" 2011) job - character - order - episodes -`,
		`c-a director on i-alpha movie "Alpha" 2010 S-E- parent - job - character - order - episodes -`,
		`c-b1 actor on i-beta series "Beta Show" 2010 S-E- parent - job - character "Self / Host" order 1 episodes 6`,
		`c-b2 creator on i-beta series "Beta Show" 2010 S-E- parent - job "Creator" character - order 0 episodes -`,
		`c-bonus actor on i-a-bonus episode "Bonus" 2010 S-E- parent "i-beta" (i-beta series "Beta Show" 2010) job - character - order - episodes -`,
		`c-e1 actor on i-b12 episode "Ab" 2010 S1E2 parent "i-beta" (i-beta series "Beta Show" 2010) job - character - order - episodes -`,
		`c-e2 actor on i-b110 episode "Aa" 2010 S1E10 parent "i-beta" (i-beta series "Beta Show" 2010) job - character - order - episodes -`,
		`c-e3 actor on i-b21 episode "A0" 2010 S2E1 parent "i-beta" (i-beta series "Beta Show" 2010) job - character - order - episodes -`,
		`c-s2 actor on i-same1 movie "Same" 2009 S-E- parent - job - character - order - episodes -`,
		`c-s1 actor on i-same2 movie "Same" 2009 S-E- parent - job - character - order - episodes -`,
		`c-o actor on i-orphan episode "Orphan" 2008 S3E4 parent "i-gone" job - character - order - episodes -`,
		`c-c actor on i-delta movie "Delta" - S-E- parent - job - character - order - episodes -`,
		`c-d actor on i-delta movie "Delta" - S-E- parent - job - character - order - episodes -`,
	}, "\n")
	if lines := creditLines(got); lines != want {
		t.Errorf("Ada's credits:\n%s\nwant:\n%s", lines, want)
	}
	for _, c := range got {
		if c.PersonID != "p1" || c.ItemID != c.Item.ID {
			t.Errorf("credit %s: person %s, item %s on %s", c.ID, c.PersonID, c.ItemID, c.Item.ID)
		}
	}

	if got, err := st.CreditsByPerson(ctx, "nobody"); err != nil || len(got) != 0 {
		t.Errorf("credits of a person no title credits: %v, %v", got, err)
	}

	// Twin credits, the same person in the same role on a title more than once
	// (nothing keeps a catalog from holding them), come by id.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p3', 'Cy')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('t5', 'i-zeta', 'p3', 'actor'), ('t2', 'i-zeta', 'p3', 'actor'), ('t7', 'i-zeta', 'p3', 'actor'),
		('t0', 'i-zeta', 'p3', 'actor'), ('t3', 'i-zeta', 'p3', 'actor'), ('t6', 'i-zeta', 'p3', 'actor'),
		('t1', 'i-zeta', 'p3', 'actor'), ('t4', 'i-zeta', 'p3', 'actor')`)
	twins, err := st.CreditsByPerson(ctx, "p3")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range twins {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != "t0 t1 t2 t3 t4 t5 t6 t7" {
		t.Errorf("twin credits: %s", got)
	}
}

// On a catalog without migration 032 a person's credits are their roles on
// their titles, the rest unknown; without 030 too.
func TestCreditsByPersonWithoutMigration032(t *testing.T) {
	for _, with030 := range []bool{true, false} {
		st := storetest.OpenBase(t)
		ctx := context.Background()
		if with030 {
			if err := st.EnsurePeople(ctx); err != nil {
				t.Fatal(err)
			}
		}
		storetest.AddItem(t, st, "i-show", "series", "A Show", "")
		storetest.AddItem(t, st, "i-ep", "episode", "An Episode", "i-show")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada')`)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
			('c2', 'i-ep', 'p1', 'actor'), ('c1', 'i-show', 'p1', 'creator')`)
		got, err := st.CreditsByPerson(ctx, "p1")
		if err != nil {
			t.Fatalf("with 030 %v: %v", with030, err)
		}
		want := `c1 creator on i-show series "A Show" - S-E- parent - job - character - order - episodes -` + "\n" +
			`c2 actor on i-ep episode "An Episode" - S-E- parent "i-show" (i-show series "A Show" -) job - character - order - episodes -`
		if lines := creditLines(got); lines != want {
			t.Errorf("with 030 %v:\n%s\nwant:\n%s", with030, lines, want)
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
