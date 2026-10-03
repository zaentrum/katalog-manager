package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// deletedItems answers from the log: newest first, since inclusive, times in UTC.
func TestDeletedItemsQuery(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedat, deletedby, reason) VALUES
		('a', 'movie', 'Oldest', '2026-09-30 22:30:00', 'subject-1', NULL),
		('b', 'episode', 'Middle', '2026-10-01 08:00:00', 'katalog-manager/scanner', NULL),
		('c', 'series', 'Newest', '2026-10-02 12:00:00.250', 'subject-2', 'a duplicate')`)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))

	resp := schema.Exec(as(admin), `{
		deletedItems(since: "2026-10-01T10:00:00+02:00") { id type title deletedAt deletedBy reason }
	}`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	want := `{"deletedItems":[` +
		`{"id":"c","type":"series","title":"Newest","deletedAt":"2026-10-02T12:00:00.25Z","deletedBy":"subject-2","reason":"a duplicate"},` +
		`{"id":"b","type":"episode","title":"Middle","deletedAt":"2026-10-01T08:00:00Z","deletedBy":"katalog-manager/scanner","reason":null}]}`
	if got := string(resp.Data); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// fakeRemover records the call the deleteItem mutation makes.
type fakeRemover struct {
	id                          string
	deleteFiles, deletePackages bool
	reason                      string
}

func (f *fakeRemover) RemoveItem(_ context.Context, id string, deleteFiles, deletePackages bool, reason string) (RemoveResult, error) {
	f.id, f.deleteFiles, f.deletePackages, f.reason = id, deleteFiles, deletePackages, reason
	return RemoveResult{Deleted: true, ItemsRemoved: 1}, nil
}

// The reason an operator gives travels to the remover, which keeps it in the
// deletion log; without one, the remover gets none.
func TestDeleteItemPassesTheReason(t *testing.T) {
	for _, tc := range []struct{ query, reason string }{
		{`mutation { deleteItem(id: "x", reason: "a duplicate") { deleted } }`, "a duplicate"},
		{`mutation { deleteItem(id: "x") { deleted } }`, ""},
	} {
		rm := &fakeRemover{}
		schema := MustSchema(NewResolver(nil, testConfig, Services{Remover: rm}))
		resp := schema.Exec(as(admin), tc.query, "", nil)
		if len(resp.Errors) > 0 {
			t.Fatalf("%s: %v", tc.query, resp.Errors)
		}
		if rm.id != "x" || rm.reason != tc.reason || rm.deleteFiles || !rm.deletePackages {
			t.Errorf("%s: remover got %+v, want id x, reason %q, packages but not files", tc.query, *rm, tc.reason)
		}
	}
}
