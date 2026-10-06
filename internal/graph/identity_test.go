package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A recorded item keeps its identity: once its item.json is written an edit
// that changes its type or its parent is refused (IDENTITY_KEPT), and one of
// anything else, or one that gives them as they are, goes. An item not
// recorded changes both as before.
func TestARecordedItemKeepsItsIdentity(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	storetest.AddItem(t, st, "s2", "series", "Another Series", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "m1", "movie", "Not Recorded", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET recordedat = now() WHERE id IN ('s1', 'e1')`)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	for q, want := range map[string]string{
		`mutation { updateItem(id: "e1", input: {parentId: "s2"}) { id } }`: "a recorded item keeps its identity: item e1 was " +
			"recorded under s1, which its item.json says; renumber it in its metadata",
		`mutation { updateItem(id: "e1", input: {type: "movie"}) { id } }`: "a recorded item keeps its identity: item e1 was " +
			"recorded with the type episode, which its item.json says",
	} {
		resp := schema.Exec(as(admin), q, "", nil)
		if len(resp.Errors) != 1 || resp.Errors[0].Message != want || resp.Errors[0].Extensions["code"] != "IDENTITY_KEPT" {
			t.Errorf("%s: %v", q, resp.Errors)
		}
	}
	var parent, typ string
	if err := st.Pool().QueryRow(context.Background(), `SELECT parent_id, type FROM com_nalet_katalog_items WHERE id = 'e1'`).
		Scan(&parent, &typ); err != nil || parent != "s1" || typ != "episode" {
		t.Errorf("the recorded episode: %s %s, %v", parent, typ, err)
	}
	if got := query(t, st, `mutation { updateItem(id: "e1", input: {title: "The Pilot", parentId: "s1", type: "episode", episodeNumber: 2}) { title } }`); !strings.Contains(got, "The Pilot") {
		t.Errorf("an edit of the rest: %s", got)
	}
	if got := query(t, st, `mutation { updateItem(id: "m1", input: {type: "episode", parentId: "s2"}) { id } }`); !strings.Contains(got, "m1") {
		t.Errorf("an item not recorded: %s", got)
	}
}
