package itemactions

import (
	"context"
	"errors"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const (
	series    = "5e5e5e5e-0000-4000-8000-000000000001"
	episode1  = "e1e1e1e1-0000-4000-8000-000000000002"
	episode2  = "e2e2e2e2-0000-4000-8000-000000000003"
	unrelated = "0a0a0a0a-0000-4000-8000-000000000004"
	operator  = "8f14e45f-ceea-467f-a8f6-2b3bd8f5b1a2" // a principal's subject
)

// Removing a series removes its episodes in the same transaction, and the log
// records all of them, attributed to the principal who asked.
func TestRemoveItemRecordsTheSeriesAndItsEpisodes(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, series, "series", "A Series", "")
	storetest.AddItem(t, st, episode1, "episode", "Pilot", series)
	storetest.AddItem(t, st, episode2, "episode", "Second", series)
	storetest.AddFacets(t, st, episode1)
	storetest.AddItem(t, st, unrelated, "movie", "Unrelated", "")
	svc := New(st, config.Config{}, processing.New(st.Pool()), nil)

	ctx := auth.WithPrincipal(context.Background(), &auth.Principal{Subject: operator})
	const reason = "the season was ingested twice"
	res, err := svc.RemoveItem(ctx, series, false, false, reason)
	if err != nil || !res.Deleted || res.ItemsRemoved != 3 {
		t.Fatalf("RemoveItem: %+v, %v; want the series and its 2 episodes removed", res, err)
	}

	logged := map[string]string{series: "series A Series", episode1: "episode Pilot", episode2: "episode Second"}
	s, _ := storetest.Deleted(t, st, series)
	for id, want := range logged {
		d, ok := storetest.Deleted(t, st, id)
		if !ok {
			t.Errorf("%s (%s) is not in the deletion log", id, want)
			continue
		}
		if got := d.Type + " " + d.Title; got != want || d.DeletedBy != operator {
			t.Errorf("%s logged as %q by %q, want %q by %q", id, got, d.DeletedBy, want, operator)
		}
		if d.Reason == nil || *d.Reason != reason {
			t.Errorf("%s logged with reason %v, want %q", id, d.Reason, reason)
		}
		if !d.DeletedAt.Equal(s.DeletedAt) {
			t.Errorf("%s deleted at %s, the series at %s: one removal, one transaction", id, d.DeletedAt, s.DeletedAt)
		}
	}
	if left := storetest.FacetRows(t, st, episode1); left != 0 {
		t.Errorf("%d facet rows of a removed episode survived", left)
	}
	if _, ok := storetest.Deleted(t, st, unrelated); ok {
		t.Error("an unrelated item is in the deletion log")
	}

	// Without a request behind it, the removal is the service's own.
	if _, err := svc.RemoveItem(context.Background(), unrelated, false, false, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := storetest.Deleted(t, st, unrelated); d.DeletedBy != "katalog-manager" || d.Reason != nil {
		t.Errorf("a removal without a principal or reason is logged by %q with reason %v, want katalog-manager and none",
			d.DeletedBy, d.Reason)
	}
}

func TestRemoveItemOfAnUnknownItemRecordsNothing(t *testing.T) {
	st := storetest.Open(t)
	svc := New(st, config.Config{}, processing.New(st.Pool()), nil)

	_, err := svc.RemoveItem(context.Background(), series, false, false, "gone")
	if !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("RemoveItem of an unknown id: %v, want ErrUnknownItem", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 0 {
		t.Fatalf("the log holds %d row(s) for an item that never existed", n)
	}
}
