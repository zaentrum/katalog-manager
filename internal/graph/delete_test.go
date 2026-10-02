package graph

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
)

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
		schema := MustSchema(NewResolver(nil, config.Config{}, Services{Remover: rm}))
		resp := schema.Exec(context.Background(), tc.query, "", nil)
		if len(resp.Errors) > 0 {
			t.Fatalf("%s: %v", tc.query, resp.Errors)
		}
		if rm.id != "x" || rm.reason != tc.reason || rm.deleteFiles || !rm.deletePackages {
			t.Errorf("%s: remover got %+v, want id x, reason %q, packages but not files", tc.query, *rm, tc.reason)
		}
	}
}
