package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
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

// A person's credits read with their titles and an episode's series, newest
// title first, a title's credits by role (the order is the store's, see
// store.CreditsByPerson); through people and an item's people too.
func TestPersonCreditsQuery(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_items (id, type, title, year, parent_id, seasonnumber,
		episodenumber, createdat, modifiedat) VALUES
		('s1', 'series', 'A Show', 2010, NULL, NULL, NULL, now(), now()),
		('e1', 'episode', 'Pilot', 2011, 's1', 1, 2, now(), now()),
		('m1', 'movie', 'A Film', 2012, NULL, NULL, NULL, now(), now())`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada'), ('p2', 'Ben')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itempeople (id, item_id, person_id, role, job, charactername,
		ordinal, episodecount) VALUES
		('c1', 's1', 'p1', 'writer', 'Writer, Co-Writer', NULL, 0, 6), ('c2', 's1', 'p1', 'actor', NULL, 'Self', 1, 6),
		('c3', 'e1', 'p1', 'actor', NULL, 'Guest', 4, NULL), ('c4', 'm1', 'p1', 'director', 'Director', NULL, 0, NULL),
		('c5', 'm1', 'p2', 'actor', NULL, 'Lead', 0, NULL)`)

	got := query(t, st, `{ person(id: "p1") { credits { id role job character order episodeCount
		item { id type title year seasonNumber episodeNumber posterUrl parent { id type title year } } } } }`)
	want := `{"person":{"credits":[` +
		`{"id":"c4","role":"director","job":"Director","character":null,"order":0,"episodeCount":null,` +
		`"item":{"id":"m1","type":"movie","title":"A Film","year":2012,"seasonNumber":null,"episodeNumber":null,` +
		`"posterUrl":"/api/manage/artwork/m1/poster","parent":null}},` +
		`{"id":"c3","role":"actor","job":null,"character":"Guest","order":4,"episodeCount":null,` +
		`"item":{"id":"e1","type":"episode","title":"Pilot","year":2011,"seasonNumber":1,"episodeNumber":2,` +
		`"posterUrl":"/api/manage/artwork/e1/poster","parent":{"id":"s1","type":"series","title":"A Show","year":2010}}},` +
		`{"id":"c2","role":"actor","job":null,"character":"Self","order":1,"episodeCount":6,` +
		`"item":{"id":"s1","type":"series","title":"A Show","year":2010,"seasonNumber":null,"episodeNumber":null,` +
		`"posterUrl":"/api/manage/artwork/s1/poster","parent":null}},` +
		`{"id":"c1","role":"writer","job":"Writer, Co-Writer","character":null,"order":0,"episodeCount":6,` +
		`"item":{"id":"s1","type":"series","title":"A Show","year":2010,"seasonNumber":null,"episodeNumber":null,` +
		`"posterUrl":"/api/manage/artwork/s1/poster","parent":null}}]}}`
	if got != want {
		t.Errorf("Ada's credits:\n got  %s\n want %s", got, want)
	}

	if got, want := query(t, st, `{ people { name credits { id } } }`),
		`{"people":[{"name":"Ada","credits":[{"id":"c4"},{"id":"c3"},{"id":"c2"},{"id":"c1"}]},`+
			`{"name":"Ben","credits":[{"id":"c5"}]}]}`; got != want {
		t.Errorf("people:\n got  %s\n want %s", got, want)
	}
	if got, want := query(t, st, `{ item(id: "m1") { people { person { name credits { item { title } } } } } }`),
		`{"item":{"people":[{"person":{"name":"Ben","credits":[{"item":{"title":"A Film"}}]}},`+
			`{"person":{"name":"Ada","credits":[{"item":{"title":"A Film"}},{"item":{"title":"Pilot"}},`+
			`{"item":{"title":"A Show"}},{"item":{"title":"A Show"}}]}}]}}`; got != want {
		t.Errorf("an item's people:\n got  %s\n want %s", got, want)
	}
	if got := query(t, st, `{ person(id: "p2") { credits { item { parent { id } } } } }`); got !=
		`{"person":{"credits":[{"item":{"parent":null}}]}}` {
		t.Errorf("a film's parent: %s", got)
	}
}

// A credit's title answers for its parent with the one read with it, and reads
// nothing more: the store is not asked again (here there is none to ask).
func TestPersonCreditTitleParentComesWithIt(t *testing.T) {
	ctx := context.Background()
	show := &model.Item{ID: "s1", Type: "series", Title: "A Show"}
	parentID := "s1"
	withParent := &personCreditResolver{m: &model.PersonCredit{
		ItemPerson: model.ItemPerson{ID: "c1", Role: "actor"},
		Item:       model.Item{ID: "e1", Type: "episode", Title: "Pilot", ParentID: &parentID},
		Parent:     show,
	}}
	p, err := withParent.Item().Parent(ctx)
	if err != nil || p == nil || p.m != show {
		t.Fatalf("the parent of a credit's episode: %+v, %v", p, err)
	}

	gone := "gone"
	orphan := &personCreditResolver{m: &model.PersonCredit{
		ItemPerson: model.ItemPerson{ID: "c2", Role: "actor"},
		Item:       model.Item{ID: "e2", Type: "episode", Title: "Orphan", ParentID: &gone},
	}}
	if p, err := orphan.Item().Parent(ctx); err != nil || p != nil {
		t.Fatalf("the parent of a credit's episode whose series is gone: %+v, %v", p, err)
	}
}
