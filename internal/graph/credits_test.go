package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// withItemView gives a test schema katalogservice_items, the view item reads,
// over the base table, with its computed columns empty.
func withItemView(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.Exec(t, st, `CREATE VIEW katalogservice_items AS SELECT id, createdat, createdby, modifiedat,
		modifiedby, type, title, sorttitle, year, description, rating, durationms, parent_id, seasonnumber,
		episodenumber, tagline, NULL::varchar AS posterurl, NULL::varchar AS backdropurl, NULL::bigint AS runtimemin,
		NULL::varchar AS yeartext FROM com_nalet_katalog_items`)
}

// creditsOfAFilm gives the catalog a title crediting people in roles the
// catalog knows and in two it does not, some of them without an order, with
// what TMDB says of each when the catalog keeps that (migration 032).
func creditsOfAFilm(t *testing.T, st *store.Store, details bool) {
	t.Helper()
	withItemView(t, st)
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

const creditFields = `{ item(id: "m1") { people { id role job character order episodeCount person { name } } } }`

// An item's credits come by role, the roles the catalog knows in their order
// and any other after them, by role; then by order, unknown last; then by
// name. Each says its job, character, order and episodes.
func TestItemPeopleQuery(t *testing.T) {
	st := storetest.Open(t)
	creditsOfAFilm(t, st, true)
	want := `{"item":{"people":[` +
		`{"id":"c7","role":"actor","job":null,"character":"First / Young First","order":0,"episodeCount":6,"person":{"name":"Ben"}},` +
		`{"id":"c3","role":"actor","job":null,"character":"Second","order":1,"episodeCount":4,"person":{"name":"Ada"}},` +
		`{"id":"c9","role":"actor","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Ida"}},` +
		`{"id":"c8","role":"creator","job":"Creator","character":null,"order":0,"episodeCount":null,"person":{"name":"Hal"}},` +
		`{"id":"c5","role":"director","job":"Director","character":null,"order":0,"episodeCount":3,"person":{"name":"Cy"}},` +
		`{"id":"c2","role":"writer","job":"Writer, Co-Writer","character":null,"order":0,"episodeCount":2,"person":{"name":"Eve"}},` +
		`{"id":"c6","role":"writer","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Dee"}},` +
		`{"id":"c4","role":"gaffer","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Gus"}},` +
		`{"id":"c1","role":"narrator","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Fay"}}]}}`
	if got := query(t, st, creditFields); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// On a catalog without migration 032 an item's credits are their roles, by
// role and name, and the rest is null; without 030 too.
func TestItemPeopleQueryWithoutMigration032(t *testing.T) {
	for _, with030 := range []bool{true, false} {
		st := storetest.OpenBase(t)
		if with030 {
			if err := st.EnsurePeople(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		creditsOfAFilm(t, st, false)
		want := `{"item":{"people":[` +
			`{"id":"c3","role":"actor","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Ada"}},` +
			`{"id":"c7","role":"actor","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Ben"}},` +
			`{"id":"c9","role":"actor","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Ida"}},` +
			`{"id":"c8","role":"creator","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Hal"}},` +
			`{"id":"c5","role":"director","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Cy"}},` +
			`{"id":"c6","role":"writer","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Dee"}},` +
			`{"id":"c2","role":"writer","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Eve"}},` +
			`{"id":"c4","role":"gaffer","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Gus"}},` +
			`{"id":"c1","role":"narrator","job":null,"character":null,"order":null,"episodeCount":null,"person":{"name":"Fay"}}]}}`
		if got := query(t, st, creditFields); got != want {
			t.Errorf("with 030 %v:\n got  %s\n want %s", with030, got, want)
		}
	}
}
