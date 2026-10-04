package graph

import (
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// searchItems' total is every match, not the page's length; catalogStats
// counts the catalog. Both are an admin's (access_test.go refuses everyone
// else).
func TestSearchTotalAndCatalogStats(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.AddItem(t, st, "m3", "movie", "A Film Again", "")
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_people (id, name) VALUES ('p1', 'Ada'), ('p2', 'Grace')`)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))

	for q, want := range map[string]string{
		`{ searchItems(q: "Film", limit: 2) { total limit offset items { id } } }`: `{"searchItems":{"total":3,"limit":2,"offset":0,"items":[{"id":"m1"},{"id":"m3"}]}}`,
		`{ searchItems(q: "Film", limit: 2, offset: 2) { total items { id } } }`:   `{"searchItems":{"total":3,"items":[{"id":"m2"}]}}`,
		`{ searchItems(q: "Film", offset: 5) { total items { id } } }`:             `{"searchItems":{"total":3,"items":[]}}`,
		`{ catalogStats { movies series episodes people } }`:                       `{"catalogStats":{"movies":3,"series":1,"episodes":1,"people":2}}`,
	} {
		resp := schema.Exec(as(admin), q, "", nil)
		if len(resp.Errors) > 0 || string(resp.Data) != want {
			t.Errorf("%s:\n got  %s %v\n want %s", q, resp.Data, resp.Errors, want)
		}
	}
}
