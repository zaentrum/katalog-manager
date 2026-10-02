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
	results, err := conn.Conn().PgConn().Exec(ctx, strings.Join(sql, "\n")).ReadAll()
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
	primary, older := pattern(300, 7, 3), pattern(80, 13, 1)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_personartwork
		(id, person_id, kind, contenttype, bytes, sha256, width, height, isprimary, sourcepath, fetchedat) VALUES
		('art-older', 'a1a1a1a1-0000-4000-8000-000000000001', 'profile', 'image/png', $1, $2, NULL, NULL, false, NULL, '2026-09-01 02:00:00+02'),
		('art-primary', 'a1a1a1a1-0000-4000-8000-000000000001', 'profile', 'image/jpeg', $3, $4, 421, 632, true, '/ada.jpg', '2026-10-01 06:00:00+00')`,
		older, hexSHA(older), primary, hexSHA(primary))
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
	gotPeople, wantPeople := doc.(map[string]any)["people"], direct.(map[string]any)["people"]
	if !reflect.DeepEqual(gotPeople, wantPeople) {
		t.Error("psql and the test exported different people")
	}
}
