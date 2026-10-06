package library

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// What a title has of its original: present with a primary asset of no
// source or of a present one; retiring while the retire job deletes it;
// retired once its primary asset is an original's and its source deleted,
// saying the event; none without a file. A catalog without migration 040
// has every original a primary asset names.
func TestWhatATitleHasOfItsOriginal(t *testing.T) {
	st := storetest.Open(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		storetest.AddItem(t, st, id, "movie", id, "")
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, retireeventid,
			retireeventat, deletedat) VALUES
		('sb', 'b', 'b.mkv', 1, 'present', NULL, NULL, NULL),
		('sc', 'c', 'c.mkv', 1, 'retiring', 'ec', '2026-10-06 10:00:00+00', NULL),
		('sd', 'd', 'd.mkv', 1, 'deleted', 'ed', '2026-10-05 09:00:00+00', '2026-10-05 09:01:00+00')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid) VALUES
		('pa', 'a', '/m/a.mkv', true, 'primary', NULL), ('pb', 'b', '/m/b.mkv', true, 'primary', 'sb'),
		('pc', 'c', '/m/c.mkv', true, 'primary', 'sc'), ('pd', 'd', '/lib/d/sources/sd', false, 'original', 'sd')`)
	got, err := OriginalsOf(context.Background(), st.Pool(), []string{"a", "b", "c", "d", "e", "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"a": OriginalPresent, "b": OriginalPresent, "c": OriginalRetiring,
		"d": OriginalRetired, "e": OriginalNone, "unknown": OriginalNone} {
		if got[id].State != want {
			t.Errorf("%s: %s, want %s", id, got[id].State, want)
		}
	}
	if why := got["d"].Why(); why != "the original was deleted after packaging (event ed, 2026-10-05T09:00:00Z)" {
		t.Errorf("why of the retired: %q", why)
	}
	if at := got["d"].RetiredAt(); at != "the original was retired at 2026-10-05T09:00:00Z (event ed)" {
		t.Errorf("when of the retired: %q", at)
	}
	if why := got["c"].Why(); why != "the original is being deleted after packaging (event ec, 2026-10-06T10:00:00Z)" {
		t.Errorf("why of the retiring: %q", why)
	}
	if got["a"].Why() != "" || got["a"].Gone() || !got["d"].Gone() || !got["c"].Gone() || got["e"].Gone() {
		t.Errorf("gone: %+v", got)
	}

	base := storetest.OpenBase(t)
	storetest.AddItem(t, base, "a", "movie", "a", "")
	storetest.AddItem(t, base, "e", "movie", "e", "")
	storetest.Exec(t, base, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('pa', 'a', '/m/a.mkv', true)`)
	old, err := OriginalsOf(context.Background(), base.Pool(), []string{"a", "e"})
	if err != nil || old["a"].State != OriginalPresent || old["e"].State != OriginalNone {
		t.Errorf("without 040: %+v, %v", old, err)
	}
}
