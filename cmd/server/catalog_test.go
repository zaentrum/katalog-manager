package main

import (
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// How much the catalog holds, and a search's total, through the service as
// main wires it with the bearer tokens of a realm: an admin reads the counts
// and every match of a search counted, its page aside; a viewer, an addon
// and the service account are refused both.
func TestCatalogCountsThroughTheService(t *testing.T) {
	in := newInstance(t) // a movie, A Film, and a person
	storetest.AddItem(t, in.st, "m2", "movie", "Another Film", "")
	storetest.AddItem(t, in.st, "s1", "series", "A Series", "")
	storetest.AddItem(t, in.st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, in.st, "e2", "episode", "Second", "s1")

	docs := map[string]string{
		`{ catalogStats { movies series episodes people } }`:          `{"catalogStats":{"movies":2,"series":1,"episodes":2,"people":1}}`,
		`{ searchItems(q: "Film", limit: 1) { total items { id } } }`: `{"searchItems":{"total":2,"items":[{"id":"m1"}]}}`,
	}
	for _, who := range []struct{ name, token string }{
		{"a viewer", in.iss.Viewer(t)}, {"an addon", in.iss.Addon(t)}, {"the service account", in.iss.Service(t, "zaentrum-manager")},
	} {
		for doc := range docs {
			if _, a := in.gql(t, "/api/manage/query", who.token, doc); len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
				t.Errorf("%s: %s: %s %v, want it refused", who.name, doc, a.Data, a.Errors)
			}
		}
	}
	for doc, want := range docs {
		if _, a := in.gql(t, "/api/manage/query", in.iss.Admin(t), doc); len(a.Errors) > 0 || string(a.Data) != want {
			t.Errorf("an admin: %s:\n got  %s %v\n want %s", doc, a.Data, a.Errors, want)
		}
	}
}
