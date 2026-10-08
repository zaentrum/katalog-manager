package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// mgEp2 is the fixture's second episode, S01E02, which the episode's one
// file covers too: it has no file, and its unit records it alone.
const mgEp2 = "d1d1d1d1-0000-4000-8000-0000000000d1"

// covering has the episode's file cover mgEp2 in the plan: the item, staged
// with no source, and the episode's source's covers.
func (f *migrationFixture) covering(t *testing.T, covers ...string) {
	t.Helper()
	storetest.AddItem(t, f.st, mgEp2, "episode", "Pilot, Part Two", mgShow)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 2,
		modifiedat = '2026-10-01 09:00:00.25' WHERE id = $1`, mgEp2) // as the export had it
	staged := f.stagedDir(mgEp2)
	f.record(t, mgEp2, staged)
	f.unit(t, mgEp2, "episode", []Move{{MovePublish, staged, f.itemDir(mgEp2)}}, nil, UnitDB{})
	u := f.units[mgEp]
	u.DB.Sources[0].Covers = covers
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	librarytest.Write(t, filepath.Join(f.runDir, "units", mgEp+".json"), b)
}

// libraryOfItem is the library of an item's metadata.json as it lies.
func (f *migrationFixture) libraryOfItem(t *testing.T, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.itemDir(id), "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := DecodeDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	lib, _ := d.Get("library")
	out, _ := Encode(lib)
	return strings.Join(strings.Fields(string(out)), " ")
}

// The adopt links the episodes a source's one file covers besides its item
// (db.sources[].covers, the item first) in the item's transaction: the
// covered episode names it, its steps of a file do not apply, and both are
// projected again from the catalog: the item numbered up to it, the covered
// one playing the item's version and naming it. A revert unlinks it, saying
// so.
func TestTheAdoptLinksTheEpisodesASourceCovers(t *testing.T) {
	f := newMigrationFixture(t)
	f.covering(t, mgEp, mgEp2)
	ctx := context.Background()
	rep, err := f.migration(t).Adopt(ctx, nil)
	if err != nil || rep.Adopted != 5 || rep.Failed+rep.Stale+rep.Refused+rep.Busy != 0 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	if covers, err := CoversOf(ctx, f.st.Pool(), mgEp); err != nil || !slices.Equal(covers, []string{mgEp, mgEp2}) {
		t.Errorf("the episode covers %v, %v; want it and S01E02", covers, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2`, mgEp2, processing.CoveredReason(mgEp)); n != len(processing.CoveredSteps) {
		t.Errorf("%d of S01E02's steps do not apply, saying whose file covers it", n)
	}
	vid := IDOf(mgEp + ":version")
	if got := f.libraryOfItem(t, mgEp); !strings.Contains(got, `"primaryVersionId": "`+vid+`"`) ||
		!strings.Contains(got, `"episode": 1, "episodeEnd": 2`) {
		t.Errorf("the episode's projection: %s", got)
	}
	if got := f.libraryOfItem(t, mgEp2); !strings.Contains(got, `"primaryVersionId": "`+vid+`", "coveredBy": "`+mgEp+`"`) {
		t.Errorf("the covered episode's projection: %s", got)
	}

	rep, err = f.migration(t).Revert(ctx, nil)
	if err != nil || rep.Reverted != 5 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if h, err := HolderOf(ctx, f.st.Pool(), mgEp2); err != nil || h != "" {
		t.Errorf("after the revert S01E02 is covered by %q, %v", h, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1
		AND status = 'not_applicable' AND error = $2`, mgEp2,
		processing.UncoveredReason(mgEp, "covers it no more: its adoption was reverted")); n != len(processing.CoveredSteps) {
		t.Errorf("%d of S01E02's steps say its link was reverted", n)
	}
}

// A plan whose source covers episodes the catalog cannot link is refused,
// nothing moved: covers that do not begin with the item, an episode named
// twice, one of another series, one with a file of its own (which wins), one
// another file covers already, and any on a catalog without migration 045.
func TestAPlanThatCoversWhatCannotBeLinkedIsRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		covers []string
		setup  func(f *migrationFixture)
		why    string
	}{
		{"not first", []string{mgEp2, mgEp}, nil, "do not begin with the item"},
		{"twice", []string{mgEp, mgEp2, mgEp2}, nil, "twice"},
		{"its own file", []string{mgEp, mgEp2}, func(f *migrationFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary) VALUES ('own', $1, '/x.mkv', true)`, mgEp2)
		}, "has a file of its own, which wins"},
		{"another series", []string{mgEp, mgFilm}, nil, "no episode of its series"},
		{"covered already", []string{mgEp, mgEp2}, func(f *migrationFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $1 WHERE id = $2`, mgGone, mgEp2)
		}, "covers already"},
		{"no migration", []string{mgEp, mgEp2}, func(f *migrationFixture) {
			storetest.Exec(t, f.st, `ALTER TABLE com_nalet_katalog_items DROP COLUMN coveredby`)
		}, "migration 045"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMigrationFixture(t)
			f.covering(t, c.covers...)
			if c.setup != nil {
				c.setup(f)
			}
			rep, err := f.migration(t).Adopt(context.Background(), []string{mgShow, mgEp})
			if err != nil || rep.Adopted != 1 || rep.Refused != 1 || !strings.Contains(rep.Units[1].Reason, c.why) {
				t.Fatalf("Adopt: %+v, %v; want it refused: %s", rep, err, c.why)
			}
			if statOK(filepath.Join(f.itemDir(mgEp), ItemFile)) {
				t.Error("a refused unit was published")
			}
		})
	}
}
