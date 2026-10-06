// Package export holds the catalog's export, library-export.sql: the catalog
// as one JSON document for the library record tools. These tests run it
// against PostgreSQL (see internal/store/storetest).
package export

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The keys of a person and of an image, in the order the tools that read the
// export are written against.
var (
	personKeys = []string{"id", "name", "sortName", "alsoKnownAs", "birthDate", "deathDate", "birthPlace",
		"biography", "externalIds", "knownForDepartment", "metadataLocked", "lockedFields", "fieldOrigins",
		"tmdbFetchedAt", "tmdbChangedAt", "modifiedAt", "artwork"}
	externalIDKeys = []string{"tmdbPerson", "imdb"}
	artworkKeys    = []string{"kind", "contentType", "base64", "sha256", "width", "height", "isPrimary",
		"sourcePath", "fetchedAt"}
)

// runExport runs library-export.sql as psql does: the lines that start with a
// backslash are psql's own commands, the rest goes to the server as one query,
// and the document is the one value of its last statement.
func runExport(t *testing.T, st *store.Store) []byte {
	t.Helper()
	return runExportShard(t, st, "")
}

// runExportShard is runExport asked for one shard, as psql -v shard=<shard>
// asks for it: psql gives the query the variable as a quoted literal, the
// whole catalog when it is empty.
func runExportShard(t *testing.T, st *store.Store, shard string) []byte {
	t.Helper()
	raw, err := os.ReadFile("library-export.sql")
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), `\`) {
			sql = append(sql, line)
		}
	}
	ctx := context.Background()
	conn, err := st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { // the export pins its session's settings: do not hand that session back
		conn.Conn().Close(ctx)
		conn.Release()
	}()
	// A server may encode binary in XML as hex: the export must not depend on it.
	if _, err := conn.Exec(ctx, `SET xmlbinary TO hex`); err != nil {
		t.Fatal(err)
	}
	query := strings.ReplaceAll(strings.Join(sql, "\n"), ":'shard'", "'"+strings.ReplaceAll(shard, "'", "''")+"'")
	results, err := conn.Conn().PgConn().Exec(ctx, query).ReadAll()
	if err != nil {
		t.Fatalf("the export failed: %v", err)
	}
	last := results[len(results)-1]
	if len(last.Rows) != 1 || len(last.Rows[0]) != 1 {
		t.Fatalf("the export's last statement gave %d rows, want one value", len(last.Rows))
	}
	return last.Rows[0][0]
}

// section is one top-level part of the document.
func section(t *testing.T, doc []byte, key string) json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatalf("the export is not one JSON document: %v\n%s", err, doc)
	}
	v, ok := top[key]
	if !ok {
		t.Fatalf("the export has no %s", key)
	}
	return v
}

// keys lists an object's keys in the order they are written.
func keys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("not an object: %s", raw)
	}
	var out []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func pattern(n, mul, add int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*mul + add) % 256)
	}
	return b
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fillPeople gives the catalog the people testdata/people.json describes: one
// with everything, two images among it; one locked; one from before 030.
func fillPeople(t *testing.T, st *store.Store) {
	t.Helper()
	// Their images first: images given to a person mark them changed
	// (migration 041), and the people are as their rows say.
	primary, older := pattern(300, 7, 3), pattern(80, 13, 1)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork
		(id, person_id, kind, contenttype, bytes, sha256, width, height, isprimary, sourcepath, fetchedat) VALUES
		('art-older', 'a1a1a1a1-0000-4000-8000-000000000001', 'profile', 'image/png', $1, $2, NULL, NULL, false, NULL, '2026-09-01 02:00:00+02'),
		('art-primary', 'a1a1a1a1-0000-4000-8000-000000000001', 'profile', 'image/jpeg', $3, $4, 421, 632, true, '/ada.jpg', '2026-10-01 06:00:00+00')`,
		older, hexSHA(older), primary, hexSHA(primary))
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, sortname, alsoknownas, birthdate, deathdate,
		birthplace, biography, tmdbpersonid, imdbid, knownfordepartment, metadatalocked, lockedfields, fieldorigins,
		tmdbfetchedat, tmdbchangedat, createdat, modifiedat) VALUES
		('a1a1a1a1-0000-4000-8000-000000000001', 'Ada Example', 'Example, Ada', '["A. Example", "Ада Пример"]',
		 '1815-12-10', '1852-11-27', 'London, England', '{"en": "English.\nA second paragraph.", "de": "Deutsch."}',
		 '101', 'nm0000001', 'Writing', false, '["biography"]',
		 '{"name": "tmdb", "biography": "manual", "images": "tmdb", "externalIds": "tmdb"}',
		 '2026-10-01 08:00:00.75+02', '2026-09-30', '2026-09-01 12:00:00+00', '2026-10-01 06:00:01+00'),
		('b2b2b2b2-0000-4000-8000-000000000002', 'Ben Example', NULL, NULL, NULL, NULL, NULL, NULL,
		 '102', NULL, NULL, true, NULL, '{"name": "manual"}', NULL, NULL, NULL, '2026-09-16 00:00:00+02')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('0c0c0c0c-0000-4000-8000-000000000003', 'Name Only')`)
}

// The export's people are exactly testdata/people.json: every person, ordered
// by id, each with every field of the contract in its order, timestamps in UTC
// whatever the server's zone, images with their bytes in base64 without line
// breaks, the primary first.
func TestExportPeople(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	fillPeople(t, st)
	people := section(t, runExport(t, st), "people")

	want, err := os.ReadFile("testdata/people.json")
	if err != nil {
		t.Fatal(err)
	}
	var got, expected any
	if err := json.Unmarshal(people, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		pretty, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("people:\n%s\nwant testdata/people.json", pretty)
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(people, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if k := keys(t, e); !reflect.DeepEqual(k, personKeys) {
			t.Errorf("a person's keys: %v, want %v", k, personKeys)
		}
		var p struct {
			ExternalIDs json.RawMessage   `json:"externalIds"`
			Artwork     []json.RawMessage `json:"artwork"`
		}
		if err := json.Unmarshal(e, &p); err != nil {
			t.Fatal(err)
		}
		if k := keys(t, p.ExternalIDs); !reflect.DeepEqual(k, externalIDKeys) {
			t.Errorf("externalIds keys: %v, want %v", k, externalIDKeys)
		}
		for _, a := range p.Artwork {
			if k := keys(t, a); !reflect.DeepEqual(k, artworkKeys) {
				t.Errorf("an image's keys: %v, want %v", k, artworkKeys)
			}
		}
	}
}

// On a catalog without migration 030 the export still runs, and people is
// null, as deletedItems is without 029: such a catalog holds no person
// records, only the names its credits carry.
func TestExportOnACatalogOlderThanThePeopleMigration(t *testing.T) {
	for _, withLog := range []bool{true, false} {
		st := storetest.OpenBase(t)
		if withLog {
			if err := st.EnsureDeletionLog(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('0c0c0c0c-0000-4000-8000-000000000003', 'Name Only')`)
		storetest.AddItem(t, st, "m1", "movie", "A Film", "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role)
			VALUES ('l1', 'm1', '0c0c0c0c-0000-4000-8000-000000000003', 'actor')`)
		doc := runExport(t, st)
		if people := string(section(t, doc, "people")); people != "null" {
			t.Errorf("with the deletion log %v: people %s, want null", withLog, people)
		}
		if got := string(section(t, doc, "items")); !strings.Contains(got, `"personId" : "0c0c0c0c-0000-4000-8000-000000000003", "name" : "Name Only"`) {
			t.Errorf("with the deletion log %v: the credit is not in the items: %s", withLog, got)
		}
		if deleted := string(section(t, doc, "deletedItems")); (deleted == "null") == withLog {
			t.Errorf("with the deletion log %v: deletedItems %s", withLog, deleted)
		}
	}

	// With 030, a person from before it exports with every field empty.
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('0c0c0c0c-0000-4000-8000-000000000003', 'Name Only')`)
	var people, want []any
	if err := json.Unmarshal(section(t, runExport(t, st), "people"), &people); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile("testdata/people.json")
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(people, want[:1]) {
		t.Errorf("a person from before 030: %v, want %v", people, want[:1])
	}
}

// deletedItems says what each deleted id was: the item's type, or person for
// someone the catalog deleted because no title credits them any more. The
// fields it had stay, in their order.
func TestExportDeletedItemsSayWhatTheyWere(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedat, deletedby, reason) VALUES
		('a2a2a2a2-0000-4000-8000-000000000001', 'movie', 'A Film', '2026-10-01 08:00:00', 'subject-1', 'a duplicate'),
		('a1a1a1a1-0000-4000-8000-000000000002', 'person', 'Selena Gomez', '2026-10-02 21:15:30.5', 'katalog-manager/tmdb',
		 'no title credits them any more: TMDB''s credits of "Spring" no longer list them')`)
	raw := section(t, runExport(t, st), "deletedItems")
	want := `[{"id":"a1a1a1a1-0000-4000-8000-000000000002","type":"person","deletedAt":"2026-10-02T21:15:30Z",` +
		`"deletedBy":"katalog-manager/tmdb"},` +
		`{"id":"a2a2a2a2-0000-4000-8000-000000000001","type":"movie","deletedAt":"2026-10-01T08:00:00Z","deletedBy":"subject-1"}]`
	var got, expected any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(want), &expected)
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("deletedItems:\n %s\nwant\n %s", raw, want)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if k := keys(t, e); !reflect.DeepEqual(k, []string{"id", "type", "deletedAt", "deletedBy"}) {
			t.Errorf("a deletedItems entry's keys: %v", k)
		}
	}
}

// psql prints the document and nothing else: the file's QUIET header keeps the
// tags of its SET statements out. Needs psql on PATH.
func TestExportThroughPsqlPrintsOnlyTheDocument(t *testing.T) {
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Skip("psql is not on PATH")
	}
	st := storetest.Open(t)
	fillPeople(t, st)
	fillCredits(t, st, true)
	var schema string
	if err := st.Pool().QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(psql, "-X", "-At", "-v", "ON_ERROR_STOP=1", "-f", "library-export.sql", os.Getenv(storetest.EnvURL))
	cmd.Env = append(os.Environ(), "PGOPTIONS=-c search_path="+schema)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("psql: %v\n%s", err, stderr.String())
	}
	if lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n"); len(lines) != 1 {
		t.Fatalf("psql printed %d lines, want the document alone:\n%.300s", len(lines), out)
	}
	var doc any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("psql's output is not one JSON document: %v", err)
	}
	var direct any
	_ = json.Unmarshal(runExport(t, st), &direct)
	for _, key := range []string{"people", "items"} {
		if !reflect.DeepEqual(doc.(map[string]any)[key], direct.(map[string]any)[key]) {
			t.Errorf("psql and the test exported different %s", key)
		}
	}
	sameCredits(t, st, out, creditsWithDetails)
}

// The keys of a credit, in the order the tools that read the export are
// written against.
var creditKeys = []string{"personId", "name", "role", "job", "character", "order", "episodeCount"}

// creditsWithDetails are the credits fillCredits gives, with their details, as
// the export lists them.
const creditsWithDetails = `[
		{"personId": "p-8", "name": "Ben", "role": "actor", "job": null, "character": "First / Young First", "order": 0, "episodeCount": 6},
		{"personId": "p-9", "name": "Ada", "role": "actor", "job": null, "character": "Second", "order": 1, "episodeCount": 4},
		{"personId": "p-1", "name": "Ida", "role": "actor", "job": null, "character": null, "order": null, "episodeCount": null},
		{"personId": "p-2", "name": "Hal", "role": "creator", "job": "Creator", "character": null, "order": 0, "episodeCount": null},
		{"personId": "p-7", "name": "Cy", "role": "director", "job": "Director", "character": null, "order": 0, "episodeCount": 3},
		{"personId": "p-5", "name": "Eve", "role": "writer", "job": "Writer, Co-Writer", "character": null, "order": 0, "episodeCount": 2},
		{"personId": "p-6", "name": "Dee", "role": "writer", "job": null, "character": null, "order": null, "episodeCount": null},
		{"personId": "p-3", "name": "Gus", "role": "gaffer", "job": null, "character": null, "order": null, "episodeCount": null},
		{"personId": "p-4", "name": "Fay", "role": "narrator", "job": null, "character": null, "order": null, "episodeCount": null}]`

// fillCredits gives the catalog a title crediting people in roles the catalog
// knows and in two it does not, some of them without an order, with what TMDB
// says of each when the catalog keeps that (migration 032).
func fillCredits(t *testing.T, st *store.Store, details bool) {
	t.Helper()
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p-9', 'Ada'), ('p-8', 'Ben'),
		('p-7', 'Cy'), ('p-6', 'Dee'), ('p-5', 'Eve'), ('p-4', 'Fay'), ('p-3', 'Gus'), ('p-2', 'Hal'),
		('p-1', 'Ida')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('c1', 'm1', 'p-4', 'narrator'), ('c2', 'm1', 'p-5', 'writer'), ('c3', 'm1', 'p-9', 'actor'),
		('c4', 'm1', 'p-3', 'gaffer'), ('c5', 'm1', 'p-7', 'director'), ('c6', 'm1', 'p-6', 'writer'),
		('c7', 'm1', 'p-8', 'actor'), ('c8', 'm1', 'p-2', 'creator'), ('c9', 'm1', 'p-1', 'actor')`)
	if details {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itempeople SET ordinal = d.o, job = d.j, charactername = d.c,
			episodecount = d.e FROM (VALUES ('c3', 1, NULL, 'Second', 4), ('c7', 0, NULL, 'First / Young First', 6),
				('c5', 0, 'Director', NULL, 3), ('c2', 0, 'Writer, Co-Writer', NULL, 2),
				('c8', 0, 'Creator', NULL, NULL)) AS d(id, o, j, c, e)
			WHERE com_nalet_katalog_itempeople.id = d.id`)
	}
}

// itemPeople is the export's people of an item.
func itemPeople(t *testing.T, doc []byte, id string) json.RawMessage {
	t.Helper()
	var items []struct {
		ID     string          `json:"id"`
		People json.RawMessage `json:"people"`
	}
	if err := json.Unmarshal(section(t, doc, "items"), &items); err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == id {
			return it.People
		}
	}
	t.Fatalf("the export has no item %s", id)
	return nil
}

// sameCredits fails t unless the export's credits of m1 are want, each with
// the keys of a credit in their order, in the order GraphQL lists them.
func sameCredits(t *testing.T, st *store.Store, doc []byte, want string) {
	t.Helper()
	raw := itemPeople(t, doc, "m1")
	var got, expected any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		pretty, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("the credits of m1:\n%s\nwant\n%s", pretty, want)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	var exported []string
	for _, e := range entries {
		if k := keys(t, e); !reflect.DeepEqual(k, creditKeys) {
			t.Errorf("a credit's keys: %v, want %v", k, creditKeys)
		}
		var c struct{ PersonID, Role string }
		if err := json.Unmarshal(e, &c); err != nil {
			t.Fatal(err)
		}
		exported = append(exported, c.Role+" "+c.PersonID)
	}
	links, _, err := st.PeopleByItem(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, l := range links {
		listed = append(listed, l.Role+" "+l.PersonID)
	}
	if strings.Join(exported, ", ") != strings.Join(listed, ", ") {
		t.Errorf("the export lists the credits\n %s\nGraphQL\n %s", strings.Join(exported, ", "), strings.Join(listed, ", "))
	}
}

// An item's people are its credits with what TMDB says of each, in the order
// a title lists them: by role (the roles the catalog knows in their order,
// then any other, by role), then by order, unknown last, then by name — the
// order GraphQL lists them in.
func TestExportCredits(t *testing.T) {
	st := storetest.Open(t)
	fillCredits(t, st, true)
	sameCredits(t, st, runExport(t, st), creditsWithDetails)

	// The roles' order in the export is the catalog's.
	raw, err := os.ReadFile("library-export.sql")
	if err != nil {
		t.Fatal(err)
	}
	roles := "array['" + strings.Join(model.CreditRoles, "', '") + "']"
	if !strings.Contains(string(raw), roles) {
		t.Errorf("library-export.sql does not order credits by %s", roles)
	}
}

// On a catalog without migration 032 (and without 030) an item's credits
// still export, each with every key, null where the catalog knows nothing,
// by role and name.
func TestExportCreditsWithoutMigration032(t *testing.T) {
	for _, with030 := range []bool{true, false} {
		st := storetest.OpenBase(t)
		if with030 {
			if err := st.EnsurePeople(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		fillCredits(t, st, false)
		sameCredits(t, st, runExport(t, st), `[
			{"personId": "p-9", "name": "Ada", "role": "actor", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-8", "name": "Ben", "role": "actor", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-1", "name": "Ida", "role": "actor", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-2", "name": "Hal", "role": "creator", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-7", "name": "Cy", "role": "director", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-6", "name": "Dee", "role": "writer", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-5", "name": "Eve", "role": "writer", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-3", "name": "Gus", "role": "gaffer", "job": null, "character": null, "order": null, "episodeCount": null},
			{"personId": "p-4", "name": "Fay", "role": "narrator", "job": null, "character": null, "order": null, "episodeCount": null}]`)
	}
}

// libraryRows gives the catalog a movie and a series, the series with a season
// and two episodes — one under the season, one under the series — people the
// movie and an episode credit, and the rows of the library record's tables
// (039, 040): an extra of the movie and one it removed, a package run of the
// movie and one removed, and an original of the movie and one of an episode.
func libraryRows(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.AddItem(t, st, "f0000000-0000-4000-8000-000000000001", "movie", "A Film", "")
	storetest.AddItem(t, st, "ab000000-0000-4000-8000-000000000002", "series", "A Show", "")
	storetest.AddItem(t, st, "cd000000-0000-4000-8000-000000000003", "season", "Season 1", "ab000000-0000-4000-8000-000000000002")
	storetest.AddItem(t, st, "ef000000-0000-4000-8000-000000000004", "episode", "Pilot", "cd000000-0000-4000-8000-000000000003")
	storetest.AddItem(t, st, "12000000-0000-4000-8000-000000000005", "episode", "Second", "ab000000-0000-4000-8000-000000000002")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p-1', 'Ada'), ('p-2', 'Ben')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role) VALUES
		('c1', 'f0000000-0000-4000-8000-000000000001', 'p-1', 'actor'),
		('c2', 'ef000000-0000-4000-8000-000000000004', 'p-2', 'actor')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, localizedtitles, origin,
		sourcepath, sourcesize, sourceqh1, registeredby, sortorder, hidden, label, state, packagepath, packagedat,
		durationms, removedat, createdat, createdby) VALUES
		('16aa63f3-0000-4000-8000-000000000001', 'f0000000-0000-4000-8000-000000000001', 'trailer', 'Trailer',
		 '{"de": "Vorschau"}', '{"kind": "link", "site": "example.org"}', '/srv/extras/a/trailer.mov', 34,
		 'sha256:' || repeat('a', 64), 'api', 1, false, 'The Trailer', 'ready', '/srv/packages/extras/16/16aa63f3',
		 '2026-10-06 10:05:00+02', 30000, NULL, '2026-10-06 08:00:00+00', 'api'),
		('64000000-0000-4000-8000-000000000002', 'f0000000-0000-4000-8000-000000000001', 'featurette', 'Gone',
		 NULL, NULL, NULL, NULL, NULL, 'api', NULL, false, NULL, 'ready', NULL, NULL, NULL,
		 '2026-10-05 10:00:00+00', '2026-10-01 08:00:00+00', 'api')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir,
		completedat, verifiedat, verifiedlevel) VALUES
		('9a2e0000-0000-4000-8000-000000000001', 'f0000000-0000-4000-8000-000000000001',
		 '{0b6c0000-0000-4000-8000-000000000001}', 'complete', '4f1d0000-0000-4000-8000-000000000001',
		 '/srv/movies/f0/f0000000-0000-4000-8000-000000000001/versions/9a2e0000-0000-4000-8000-000000000001',
		 '2026-10-06 09:00:00+00', '2026-10-06 09:30:00+00', 'full'),
		('77c10000-0000-4000-8000-000000000002', 'f0000000-0000-4000-8000-000000000001', '{}', 'removed',
		 NULL, NULL, NULL, NULL, NULL)`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath,
		sizebytes, qh1, state, recordedat, recorddir, sidecars) VALUES
		('0b6c0000-0000-4000-8000-000000000001', 'f0000000-0000-4000-8000-000000000001', 'A Film (2024).mkv',
		 '/srv/.work/incoming/A Film (2024).mkv', 'A Film (2024).mkv', 1234, 'sha256:' || repeat('b', 64), 'present',
		 '2026-10-06 09:00:00+00', '/srv/movies/f0/f0000000-0000-4000-8000-000000000001/sources/0b6c0000-0000-4000-8000-000000000001',
		 '[{"subtitleAssetId": "s-1", "rendition": "sub3", "path": "subs/3.vtt"}]'),
		('0b6c0000-0000-4000-8000-000000000002', 'ef000000-0000-4000-8000-000000000004', 'Pilot.mkv', NULL, 'tv/Pilot.mkv',
		 99, NULL, 'deleted', NULL, NULL, NULL)`)
}

// ids lists the ids of a section's entries, in their order.
func ids(t *testing.T, raw json.RawMessage, key string) []string {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("not a list: %s", raw)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e[key].(string))
	}
	return out
}

// The library tables the migration stages its records from are in the
// document — the extras that are part of their titles, the package runs that
// are not removed, the originals — and a shard narrows the items, the people
// they credit and those tables to the titles whose folder is in it: a series
// with its season and its episodes, the one under the season too. A catalog
// without 039 and 040 exports them as null.
func TestExportLibraryTablesAndShards(t *testing.T) {
	st := storetest.Open(t)
	libraryRows(t, st)
	doc := runExport(t, st)
	if s := string(section(t, doc, "shard")); s != "null" {
		t.Errorf("the whole catalog's shard is %s, want null", s)
	}
	if got := ids(t, section(t, doc, "extras"), "id"); !reflect.DeepEqual(got, []string{"16aa63f3-0000-4000-8000-000000000001"}) {
		t.Errorf("extras %v, want the one the movie did not remove", got)
	}
	if got := ids(t, section(t, doc, "versions"), "id"); !reflect.DeepEqual(got, []string{"9a2e0000-0000-4000-8000-000000000001"}) {
		t.Errorf("versions %v, want the one not removed", got)
	}
	if got := ids(t, section(t, doc, "sources"), "id"); !reflect.DeepEqual(got, []string{
		"0b6c0000-0000-4000-8000-000000000002", "0b6c0000-0000-4000-8000-000000000001"}) {
		t.Errorf("sources %v, want both, by item", got)
	}
	var extras []map[string]any
	_ = json.Unmarshal(section(t, doc, "extras"), &extras)
	want := map[string]any{"id": "16aa63f3-0000-4000-8000-000000000001", "itemId": "f0000000-0000-4000-8000-000000000001",
		"kind": "trailer", "title": "Trailer", "localizedTitles": map[string]any{"de": "Vorschau"}, "language": nil,
		"seasonNumber": nil, "origin": map[string]any{"kind": "link", "site": "example.org"},
		"sourcePath": "/srv/extras/a/trailer.mov", "sourceSize": float64(34), "sourceQh1": "sha256:" + strings.Repeat("a", 64),
		"recordPath": nil, "registeredBy": "api", "sortOrder": float64(1), "hidden": false, "label": "The Trailer",
		"state": "ready", "packageId": nil, "packagePath": "/srv/packages/extras/16/16aa63f3",
		"packagedAt": "2026-10-06T08:05:00Z", "recordedAt": nil, "durationMs": float64(30000),
		"createdAt": "2026-10-06T08:00:00Z", "createdBy": "api", "modifiedAt": extras[0]["modifiedAt"]}
	if !reflect.DeepEqual(extras[0], want) {
		pretty, _ := json.MarshalIndent(extras[0], "", "  ")
		t.Errorf("the extra:\n%s", pretty)
	}
	var versions []map[string]any
	_ = json.Unmarshal(section(t, doc, "versions"), &versions)
	if v := versions[0]; !reflect.DeepEqual(v["sourceIds"], []any{"0b6c0000-0000-4000-8000-000000000001"}) ||
		v["completedAt"] != "2026-10-06T09:00:00Z" || v["verifiedLevel"] != "full" || v["state"] != "complete" {
		t.Errorf("the version: %v", v)
	}
	var sources []map[string]any
	_ = json.Unmarshal(section(t, doc, "sources"), &sources)
	if s := sources[1]; s["qh1"] != "sha256:"+strings.Repeat("b", 64) || s["sizeBytes"] != float64(1234) ||
		!reflect.DeepEqual(s["sidecars"], []any{map[string]any{"subtitleAssetId": "s-1", "rendition": "sub3", "path": "subs/3.vtt"}}) {
		t.Errorf("the movie's source: %v", s)
	}

	f0 := runExportShard(t, st, "f0")
	if s := string(section(t, f0, "shard")); s != `"f0"` {
		t.Errorf("the shard is %s", s)
	}
	if got := ids(t, section(t, f0, "items"), "id"); !reflect.DeepEqual(got, []string{"f0000000-0000-4000-8000-000000000001"}) {
		t.Errorf("shard f0 holds %v, want the movie alone", got)
	}
	if got := ids(t, section(t, f0, "people"), "id"); !reflect.DeepEqual(got, []string{"p-1"}) {
		t.Errorf("shard f0's people %v, want whom the movie credits", got)
	}
	if got := ids(t, section(t, f0, "sources"), "id"); !reflect.DeepEqual(got, []string{"0b6c0000-0000-4000-8000-000000000001"}) {
		t.Errorf("shard f0's sources %v", got)
	}
	ab := runExportShard(t, st, "ab")
	if got := ids(t, section(t, ab, "items"), "id"); !reflect.DeepEqual(got, []string{
		"12000000-0000-4000-8000-000000000005", "ab000000-0000-4000-8000-000000000002",
		"cd000000-0000-4000-8000-000000000003", "ef000000-0000-4000-8000-000000000004"}) {
		t.Errorf("shard ab holds %v, want the series, its season and both episodes", got)
	}
	if got := ids(t, section(t, ab, "people"), "id"); !reflect.DeepEqual(got, []string{"p-2"}) {
		t.Errorf("shard ab's people %v", got)
	}
	if got := ids(t, section(t, ab, "extras"), "id"); got != nil {
		t.Errorf("shard ab's extras %v, want none", got)
	}
	if got := string(section(t, ab, "deletedItems")); got != "[]" {
		t.Errorf("the deletion log of a shard is the whole log: %s", got)
	}

	// psql asked for a shard prints that shard: -v gives the file its variable
	if psql, err := exec.LookPath("psql"); err == nil {
		var schema string
		if err := st.Pool().QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(psql, "-X", "-At", "-v", "ON_ERROR_STOP=1", "-v", "shard=ab", "-f", "library-export.sql",
			os.Getenv(storetest.EnvURL))
		cmd.Env = append(os.Environ(), "PGOPTIONS=-c search_path="+schema)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("psql -v shard=ab: %v\n%s", err, stderr.String())
		}
		if got := ids(t, section(t, out, "items"), "id"); !reflect.DeepEqual(got, ids(t, section(t, ab, "items"), "id")) {
			t.Errorf("psql -v shard=ab printed the items %v", got)
		}
	}

	base := storetest.OpenBase(t)
	storetest.AddItem(t, base, "f0000000-0000-4000-8000-000000000001", "movie", "A Film", "")
	old := runExport(t, base)
	for _, key := range []string{"extras", "versions", "sources"} {
		if s := string(section(t, old, key)); s != "null" {
			t.Errorf("a catalog without 039 and 040 exports %s as %s, want null", key, s)
		}
	}
}
