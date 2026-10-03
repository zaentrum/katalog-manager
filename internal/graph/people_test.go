package graph

import (
	"context"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const personFields = `id name sortName alsoKnownAs birthDate deathDate birthPlace biography { language text }
	tmdbPersonId imdbId knownForDepartment metadataLocked lockedFields fieldOrigins { field origin }
	tmdbFetchedAt tmdbChangedAt modifiedAt`

func query(t *testing.T, st *store.Store, q string) string {
	t.Helper()
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin), q, "", nil)
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

// fakePeople records the call the refreshPeople mutation makes.
type fakePeople struct{ all *bool }

func (f *fakePeople) RefreshPeople(_ context.Context, all bool) (PeopleRefreshResult, error) {
	f.all = &all
	return PeopleRefreshResult{TitlesRead: 3, TitlesFailed: 1, TitlesLocked: 1, PeopleMatched: 70, PeopleCreated: 2,
		CreditsAdded: 4, CreditsUpdated: 5, CreditsDropped: 26, CreditsRelinked: 1, PeopleDeleted: 25,
		PeopleFetched: 71, PeopleLocked: 1, PeopleNotFound: 1, PeopleFailed: 0, PeopleWithoutTmdbID: 3,
		StartedAt: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 2, 8, 0, 9, 0, time.UTC)}, nil
}

// refreshPeople runs the backfill (all defaults to false) and reports its counts.
func TestRefreshPeopleMutation(t *testing.T) {
	for _, tc := range []struct {
		args string
		all  bool
	}{{"(all: true)", true}, {"", false}} {
		fp := &fakePeople{}
		schema := MustSchema(NewResolver(nil, testConfig, Services{People: fp}))
		resp := schema.Exec(as(admin), `mutation { refreshPeople`+tc.args+` { titlesRead titlesFailed
			titlesLocked peopleMatched peopleCreated creditsAdded creditsUpdated creditsDropped creditsRelinked
			peopleDeleted peopleFetched peopleLocked peopleNotFound peopleFailed peopleWithoutTmdbId startedAt finishedAt } }`, "", nil)
		if len(resp.Errors) > 0 {
			t.Fatal(resp.Errors)
		}
		if fp.all == nil || *fp.all != tc.all {
			t.Errorf("refreshPeople%s called the refresher with all=%v", tc.args, fp.all)
		}
		want := `{"refreshPeople":{"titlesRead":3,"titlesFailed":1,"titlesLocked":1,"peopleMatched":70,"peopleCreated":2,` +
			`"creditsAdded":4,"creditsUpdated":5,"creditsDropped":26,"creditsRelinked":1,"peopleDeleted":25,` +
			`"peopleFetched":71,"peopleLocked":1,"peopleNotFound":1,"peopleFailed":0,"peopleWithoutTmdbId":3,` +
			`"startedAt":"2026-10-02T08:00:00Z","finishedAt":"2026-10-02T08:00:09Z"}}`
		if got := string(resp.Data); got != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
	resp := MustSchema(NewResolver(nil, testConfig, Services{})).Exec(as(admin),
		`mutation { refreshPeople(all: true) { titlesRead } }`, "", nil)
	if len(resp.Errors) == 0 {
		t.Error("refreshPeople without a refresher answered")
	}
}

// referenceSync reads each change list's cursor and last run, by kind; a
// catalog without migration 030 has none.
func TestReferenceSyncQuery(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_referencesync (kind, cursor, lastrunat, lastrunchanges,
		lastrunmatched, lastrunrefreshed, lastrunskipped, lastrunfailed, lastrunerror) VALUES
		('tv', '2026-10-02', '2026-10-02 03:00:00+00', 120, 2, 2, 0, 0, NULL),
		('person', '2026-09-28', '2026-10-02 03:00:01+00', 5400, 3, 1, 1, 1, '1 of 3 refreshes failed')`)
	got := query(t, st, `{ referenceSync { kind cursor lastRunAt lastRunChanges lastRunMatched lastRunRefreshed
		lastRunSkipped lastRunFailed lastRunError } }`)
	want := `{"referenceSync":[` +
		`{"kind":"person","cursor":"2026-09-28","lastRunAt":"2026-10-02T03:00:01Z","lastRunChanges":5400,"lastRunMatched":3,` +
		`"lastRunRefreshed":1,"lastRunSkipped":1,"lastRunFailed":1,"lastRunError":"1 of 3 refreshes failed"},` +
		`{"kind":"tv","cursor":"2026-10-02","lastRunAt":"2026-10-02T03:00:00Z","lastRunChanges":120,"lastRunMatched":2,` +
		`"lastRunRefreshed":2,"lastRunSkipped":0,"lastRunFailed":0,"lastRunError":null}]}`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := query(t, storetest.OpenBase(t), `{ referenceSync { kind } }`); got != `{"referenceSync":[]}` {
		t.Errorf("without 030: %s", got)
	}
}
