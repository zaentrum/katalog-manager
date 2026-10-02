package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const personFields = `id name sortName alsoKnownAs birthDate deathDate birthPlace biography { language text }
	tmdbPersonId imdbId knownForDepartment metadataLocked lockedFields fieldOrigins { field origin }
	tmdbFetchedAt tmdbChangedAt modifiedAt`

func query(t *testing.T, st *store.Store, q string) string {
	t.Helper()
	resp := MustSchema(NewResolver(st, config.Config{}, Services{})).Exec(context.Background(), q, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatalf("%s: %v", q, resp.Errors)
	}
	return string(resp.Data)
}

// A person reads with everything the catalog keeps about them; texts and
// origins come in a fixed order, times in UTC.
func TestPersonQueries(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name, sortname, alsoknownas, birthdate, deathdate,
		birthplace, biography, tmdbpersonid, imdbid, knownfordepartment, metadatalocked, lockedfields, fieldorigins,
		tmdbfetchedat, tmdbchangedat, modifiedat) VALUES
		('p1', 'Ada Example', 'Example, Ada', '["A. Example"]', '1815-12-10', '1852-11-27', 'London',
		 '{"en": "English.", "de": "Deutsch."}', '101', 'nm0000001', 'Writing', false, '["biography"]',
		 '{"name": "tmdb", "biography": "manual"}', '2026-10-01 08:00:00+00', '2026-09-30', '2026-10-01 08:00:00.5+00'),
		('p2', 'Ben Example', NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, true, NULL, NULL, NULL, NULL, NULL)`)

	if got, want := query(t, st, `{ person(id: "p1") { `+personFields+` } }`), `{"person":{"id":"p1","name":"Ada Example",`+
		`"sortName":"Example, Ada","alsoKnownAs":["A. Example"],"birthDate":"1815-12-10","deathDate":"1852-11-27",`+
		`"birthPlace":"London","biography":[{"language":"de","text":"Deutsch."},{"language":"en","text":"English."}],`+
		`"tmdbPersonId":"101","imdbId":"nm0000001","knownForDepartment":"Writing","metadataLocked":false,`+
		`"lockedFields":["biography"],"fieldOrigins":[{"field":"biography","origin":"manual"},{"field":"name","origin":"tmdb"}],`+
		`"tmdbFetchedAt":"2026-10-01T08:00:00Z","tmdbChangedAt":"2026-09-30","modifiedAt":"2026-10-01T08:00:00.5Z"}}`; got != want {
		t.Errorf("person:\n got  %s\n want %s", got, want)
	}
	if got, want := query(t, st, `{ people { id metadataLocked alsoKnownAs biography { text } fieldOrigins { field } } }`),
		`{"people":[{"id":"p1","metadataLocked":false,"alsoKnownAs":["A. Example"],"biography":[{"text":"Deutsch."},{"text":"English."}],`+
			`"fieldOrigins":[{"field":"biography"},{"field":"name"}]},`+
			`{"id":"p2","metadataLocked":true,"alsoKnownAs":[],"biography":[],"fieldOrigins":[]}]}`; got != want {
		t.Errorf("people:\n got  %s\n want %s", got, want)
	}
	if got := query(t, st, `{ person(id: "nobody") { id } }`); got != `{"person":null}` {
		t.Errorf("an unknown person: %s", got)
	}
}

// On a catalog without migration 030 people still read, with what it has.
func TestPersonQueriesWithoutThePeopleMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada Example')`)

	if got, want := query(t, st, `{ people { `+personFields+` } }`), `{"people":[{"id":"p1","name":"Ada Example",`+
		`"sortName":null,"alsoKnownAs":[],"birthDate":null,"deathDate":null,"birthPlace":null,"biography":[],`+
		`"tmdbPersonId":null,"imdbId":null,"knownForDepartment":null,"metadataLocked":false,"lockedFields":[],`+
		`"fieldOrigins":[],"tmdbFetchedAt":null,"tmdbChangedAt":null,"modifiedAt":null}]}`; got != want {
		t.Errorf("people:\n got  %s\n want %s", got, want)
	}
}
