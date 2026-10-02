package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	movieA = "aaaaaaaa-0000-4000-8000-000000000001"
	movieB = "bbbbbbbb-0000-4000-8000-000000000002"
	movieC = "cccccccc-0000-4000-8000-000000000003"
	absent = "dddddddd-0000-4000-8000-000000000004"
)

// deletePath is one way the store deletes items; both go through the same
// transaction, and both must record what they remove.
type deletePath struct {
	name string
	del  func(st *store.Store, d store.Deletion, ids ...string) (int64, error)
}

var deletePaths = []deletePath{
	{"DeleteItem", func(st *store.Store, d store.Deletion, ids ...string) (int64, error) {
		var n int64
		for _, id := range ids {
			ok, err := st.DeleteItem(context.Background(), id, d)
			if err != nil {
				return n, err
			}
			if ok {
				n++
			}
		}
		return n, nil
	}},
	{"DeleteItems", func(st *store.Store, d store.Deletion, ids ...string) (int64, error) {
		return st.DeleteItems(context.Background(), ids, d)
	}},
}

func TestDeleteRecordsTheItemInTheDeletionLog(t *testing.T) {
	for _, p := range deletePaths {
		t.Run(p.name, func(t *testing.T) {
			st := storetest.Open(t)
			storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
			storetest.AddFacets(t, st, movieA)
			storetest.AddItem(t, st, movieC, "movie", "Movie C", "")

			n, err := p.del(st, store.Deletion{By: "subject-1", Reason: "a duplicate of another item"}, movieA)
			if err != nil || n != 1 {
				t.Fatalf("delete: n=%d err=%v, want 1 removed", n, err)
			}
			if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, movieA) != 0 {
				t.Fatal("the item is still in the catalog")
			}
			if left := storetest.FacetRows(t, st, movieA); left != 0 {
				t.Fatalf("%d facet rows survived their item", left)
			}
			d, ok := storetest.Deleted(t, st, movieA)
			if !ok {
				t.Fatal("the delete left no row in the deletion log")
			}
			if d.Type != "movie" || d.Title != "Movie A" || d.DeletedBy != "subject-1" ||
				d.Reason == nil || *d.Reason != "a duplicate of another item" {
				t.Fatalf("log row = %+v (reason %v)", d, deref(d.Reason))
			}
			if age := time.Since(d.DeletedAt); age < -time.Minute || age > time.Minute {
				t.Fatalf("deletedat %s is not now in UTC (%s away)", d.DeletedAt, age.Round(time.Second))
			}
			if _, ok := storetest.Deleted(t, st, movieC); ok {
				t.Fatal("an item that was not deleted is in the log")
			}
			if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, movieC) != 1 {
				t.Fatal("an item that was not asked for was deleted")
			}
		})
	}
}

// A bulk delete is one transaction: every item it removes is in the log, with
// the one deletedat they share, and an id that does not exist leaves no row.
func TestDeleteItemsRecordsEveryItemItRemoves(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "series", "Series A", "")
	storetest.AddItem(t, st, movieB, "episode", "Episode B", movieA)
	storetest.AddItem(t, st, movieC, "movie", "Movie C", "")

	n, err := st.DeleteItems(context.Background(), []string{movieA, movieB, absent}, store.Deletion{By: "subject-1"})
	if err != nil || n != 2 {
		t.Fatalf("DeleteItems: n=%d err=%v, want 2", n, err)
	}
	a, okA := storetest.Deleted(t, st, movieA)
	b, okB := storetest.Deleted(t, st, movieB)
	if !okA || !okB {
		t.Fatalf("both removed items must be logged: series %v, episode %v", okA, okB)
	}
	if !a.DeletedAt.Equal(b.DeletedAt) {
		t.Errorf("one delete, two times: %s and %s", a.DeletedAt, b.DeletedAt)
	}
	if a.Reason != nil {
		t.Errorf("no reason was given, the log says %q", *a.Reason)
	}
	if _, ok := storetest.Deleted(t, st, absent); ok {
		t.Error("an id that did not exist was logged as deleted")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 2 {
		t.Errorf("the log holds %d rows, want 2", n)
	}
}

// When the delete fails, nothing is deleted and nothing is logged.
func TestDeleteThatFailsLeavesNoRow(t *testing.T) {
	for _, p := range deletePaths {
		t.Run(p.name, func(t *testing.T) {
			st := storetest.Open(t)
			storetest.AddItem(t, st, movieA, "movie", "Deletable", "")
			storetest.AddFacets(t, st, movieA)
			storetest.AddItem(t, st, movieB, "movie", "Undeletable", "")
			storetest.AddFacets(t, st, movieB)
			storetest.Exec(t, st, `CREATE FUNCTION refuse_delete() RETURNS trigger LANGUAGE plpgsql AS
				$$ BEGIN RAISE EXCEPTION 'this item cannot be deleted'; END $$`)
			storetest.Exec(t, st, `CREATE TRIGGER refuse_delete BEFORE DELETE ON com_nalet_katalog_items
				FOR EACH ROW WHEN (OLD.id = '`+movieB+`') EXECUTE FUNCTION refuse_delete()`)

			ids := []string{movieB}
			if p.name == "DeleteItems" {
				ids = []string{movieA, movieB} // one fails, so neither goes
			}
			if _, err := p.del(st, store.Deletion{By: "subject-1"}, ids...); err == nil {
				t.Fatal("the delete succeeded although the database refused it")
			}
			for _, id := range ids {
				if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, id) != 1 {
					t.Errorf("%s left the catalog in a delete that failed", id)
				}
				if left := storetest.FacetRows(t, st, id); left != storetest.Facets {
					t.Errorf("%s kept %d of its %d facet rows", id, left, storetest.Facets)
				}
			}
			if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 0 {
				t.Fatalf("a delete that failed left %d row(s) in the log", n)
			}
		})
	}
}

// When the deletion cannot be recorded, the item is not deleted: the catalog
// never forgets an item without remembering that it deleted it.
func TestDeleteThatCannotBeRecordedDoesNotHappen(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sabotage string
	}{
		{"the log refuses the row",
			`ALTER TABLE com_nalet_katalog_deleteditems ADD CONSTRAINT refuse_row CHECK (deletedby <> 'subject-1')`},
		{"the log is missing", `DROP TABLE com_nalet_katalog_deleteditems`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := storetest.Open(t)
			storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
			storetest.AddFacets(t, st, movieA)
			storetest.Exec(t, st, tc.sabotage)

			if _, err := st.DeleteItem(context.Background(), movieA, store.Deletion{By: "subject-1"}); err == nil {
				t.Fatal("the item was deleted without its row in the log")
			}
			if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, movieA) != 1 {
				t.Fatal("the item is gone, and nothing remembers that it was deleted")
			}
			if left := storetest.FacetRows(t, st, movieA); left != storetest.Facets {
				t.Fatalf("the item kept %d of its %d facet rows", left, storetest.Facets)
			}
		})
	}
}

// The log row and the delete are one transaction: a failure at commit, after
// both were written, undoes both.
func TestDeleteAndItsRowCommitTogether(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddFacets(t, st, movieA)
	storetest.Exec(t, st, `CREATE FUNCTION refuse_commit() RETURNS trigger LANGUAGE plpgsql AS
		$$ BEGIN RAISE EXCEPTION 'refused at commit'; END $$`)
	storetest.Exec(t, st, `CREATE CONSTRAINT TRIGGER refuse_commit AFTER INSERT ON com_nalet_katalog_deleteditems
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION refuse_commit()`)

	_, err := st.DeleteItem(context.Background(), movieA, store.Deletion{By: "subject-1"})
	if err == nil || !strings.Contains(err.Error(), "refused at commit") {
		t.Fatalf("DeleteItem: %v, want the commit to fail", err)
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, movieA) != 1 {
		t.Fatal("the delete committed without its log row")
	}
	if left := storetest.FacetRows(t, st, movieA); left != storetest.Facets {
		t.Fatalf("the item kept %d of its %d facet rows", left, storetest.Facets)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 0 {
		t.Fatalf("the log row committed without its delete: %d row(s)", n)
	}
}

// An item re-created with an id the log holds works like any other item, and
// deleting it again replaces the row with the latest deletion.
func TestAnItemRecreatedWithTheSameIDCanBeDeletedAgain(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "First life", "")
	if _, err := st.DeleteItem(ctx, movieA, store.Deletion{By: "subject-1", Reason: "first"}); err != nil {
		t.Fatal(err)
	}
	first, _ := storetest.Deleted(t, st, movieA)

	storetest.AddItem(t, st, movieA, "movie", "Second life", "") // e.g. restored from its record
	storetest.AddFacets(t, st, movieA)
	if _, ok := storetest.Deleted(t, st, movieA); !ok {
		t.Fatal("re-creating an item must not touch the log: the row says what happened to its first life")
	}

	ok, err := st.DeleteItem(ctx, movieA, store.Deletion{By: "katalog-manager/scanner"})
	if err != nil || !ok {
		t.Fatalf("deleting the re-created item: ok=%v err=%v", ok, err)
	}
	second, _ := storetest.Deleted(t, st, movieA)
	if second.Title != "Second life" || second.DeletedBy != "katalog-manager/scanner" || second.Reason != nil {
		t.Fatalf("the row must describe the latest deletion, got %+v (reason %v)", second, deref(second.Reason))
	}
	if second.DeletedAt.Before(first.DeletedAt) {
		t.Fatalf("the latest deletion (%s) is older than the first (%s)", second.DeletedAt, first.DeletedAt)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems WHERE id = $1`, movieA); n != 1 {
		t.Fatalf("%d rows for one id, want 1", n)
	}
}

// Every row says who deleted; a delete that cannot say so does not happen. Long
// texts are cut to their columns instead of failing the delete.
func TestDeleteSaysWhoAndWhy(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")

	if _, err := st.DeleteItem(ctx, movieA, store.Deletion{By: "  "}); err == nil {
		t.Fatal("a delete without anyone to attribute it to went through")
	}
	if storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1`, movieA) != 1 {
		t.Fatal("the item went, although its deletion could not say who deleted it")
	}

	by, reason := strings.Repeat("s", 300), strings.Repeat("é", 600)
	if ok, err := st.DeleteItem(ctx, movieA, store.Deletion{By: by, Reason: reason}); err != nil || !ok {
		t.Fatalf("a long reason must not fail the delete: ok=%v err=%v", ok, err)
	}
	d, _ := storetest.Deleted(t, st, movieA)
	if d.DeletedBy != by[:255] || d.Reason == nil || *d.Reason != strings.Repeat("é", 500) {
		t.Fatalf("by has %d characters, reason %d; want 255 and 500",
			len([]rune(d.DeletedBy)), len([]rune(deref(d.Reason))))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
