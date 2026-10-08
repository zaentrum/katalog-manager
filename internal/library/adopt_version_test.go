package library

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// Two films the run stages as the tool stages them now: one packaged, its
// original moved into its version's folder once the item is published; one
// nothing packaged, taken in, its original moved into its staged version's
// folder before the item is published.
const (
	mgInto  = "a1a1a1a1-0000-4000-8000-00000000001a"
	mgTaken = "a2a2a2a2-0000-4000-8000-00000000002a"
)

// newVersionsFixture is the migration fixture with the two films.
func newVersionsFixture(t *testing.T) *migrationFixture {
	t.Helper()
	f := newMigrationFixture(t)
	storetest.AddItem(t, f.st, mgInto, "movie", "Film Into", "")
	storetest.AddItem(t, f.st, mgTaken, "movie", "Film Taken", "")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2026-10-01 09:00:00.25' WHERE id IN ($1, $2)`,
		mgInto, mgTaken)
	f.into = true
	f.packaged(t, mgInto, "media/Into (2021)/Into (2021) Bluray-1080p.MKV", false)
	f.unpackaged(t, mgTaken, "media/Taken (2022)/Taken (2022).m2ts")
	return f
}

// unpackaged stages the item id that nothing packaged, its original at
// media: its source record, its version's version.json naming the original
// under its library name and no package, the plan moving the original into
// the staged version's folder, and the catalog taking the version in.
func (f *migrationFixture) unpackaged(t *testing.T, id, media string) {
	t.Helper()
	staged, final := f.stagedDir(id), f.itemDir(id)
	sid, vid := IDOf(id+":source"), IDOf(id+":version")
	f.record(t, id, staged)
	path := filepath.Join(f.dir, media)
	librarytest.Write(t, path, []byte("the original of "+id))
	size, qh1, err := QH1(path)
	if err != nil {
		t.Fatal(err)
	}
	name := OriginalName(path, 0)
	librarytest.WriteSource(t, filepath.Join(staged, "sources", sid), map[string]any{"sourceId": sid,
		"file": map[string]any{"name": name, "sizeBytes": size, "fixity": map[string]any{"qh1": qh1}}}, nil)
	librarytest.Write(t, filepath.Join(staged, "versions", vid, VersionFile), librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.version/2", "versionId": vid, "sourceIds": []string{sid}, "originalFiles": []string{name}}))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sizebytes)
		VALUES ('pa-' || left($1::varchar, 4), $1::varchar, $2, true, 'primary', $3)`, id, path, size)
	placed := filepath.Join(final, "versions", vid, name)
	updated := f.updatedAt(t, "items", id)
	db := UnitDB{RecordedAt: "2026-10-07T10:00:00Z", ProjectedDatabaseUpdatedAt: &updated,
		Sources: []UnitSource{{SourceID: sid, Filename: name, ArrivalPath: &placed, LibraryPath: ptr(strings.TrimPrefix(media, "media/")),
			SizeBytes: size, QH1: &qh1, RecordDir: ptr(filepath.Join(final, "sources", sid))}},
		Versions: []UnitVersion{{VersionID: vid, Dir: filepath.Join(final, "versions", vid), SourceIDs: []string{sid}}},
		Assets:   []UnitAsset{{ID: "pa-" + id[:4], Path: placed, SourceID: &sid}}}
	f.unit(t, id, "movie", []Move{{MoveOriginal, path, filepath.Join(staged, "versions", vid, name)},
		{MovePublish, staged, final}}, nil, db)
}

// stateOf is what an adoption changes of the items ids in the database, as
// JSON.
func (f *migrationFixture) stateOf(t *testing.T, ids ...string) string {
	t.Helper()
	all := []any{ids}
	return strings.Join([]string{
		f.rows(t, "playbackassets", "t.item_id = ANY($1)", all...),
		f.rows(t, "subtitleassets", "t.item_id = ANY($1)", all...),
		f.rows(t, "itemsources", "t.item_id = ANY($1)", all...),
		f.rows(t, "itemversions", "t.item_id = ANY($1)", all...),
		f.rows(t, "items", "t.id = ANY($1)", all...),
	}, "\n")
}

// The adopt takes an original into its version's folder, as the plan moves
// it there (into the published folder, or into the staged one before the
// item is published), under its library name: the source and the asset of
// the title's file point at it there. A title nothing packaged gets its
// version taken in: its folder holding its version.json and its original, no
// package, no packaged asset. The retire job then retires the packaged one's
// original from its version's folder, and leaves the one taken in.
func TestTheAdoptTakesOriginalsIntoTheirVersions(t *testing.T) {
	f := newVersionsFixture(t)
	ctx := context.Background()
	rep, err := f.migration(t).Adopt(ctx, []string{mgInto, mgTaken})
	if err != nil || rep.Adopted != 2 || rep.Failed+rep.Refused+rep.Stale+rep.Busy != 0 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	into := filepath.Join(VersionDir(f.itemDir(mgInto), IDOf(mgInto+":version")), "original.mkv")
	taken := filepath.Join(VersionDir(f.itemDir(mgTaken), IDOf(mgTaken+":version")), "original.m2ts")
	for _, path := range []string{into, taken, filepath.Join(filepath.Dir(taken), VersionFile)} {
		if !statOK(path) {
			t.Errorf("%s is not there", path)
		}
	}
	for _, gone := range []string{filepath.Join(f.dir, "media", "Into (2021)"), filepath.Join(f.dir, "media", "Taken (2022)")} {
		if exists(gone) {
			t.Errorf("%s is still there", gone)
		}
	}
	for sql, want := range map[string]string{
		`SELECT s.arrivalpath || ' ' || s.filename || ' ' || a.path || ' ' || a.kind || ' ' || a.isprimary
			FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets a ON a.sourceid = s.id
			WHERE s.item_id = '` + mgInto + `'`: into + " original.mkv " + into + " primary true",
		`SELECT s.arrivalpath || ' ' || s.filename || ' ' || a.path || ' ' || (s.recordedat IS NOT NULL)
			FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets a ON a.sourceid = s.id
			WHERE s.item_id = '` + mgTaken + `'`: taken + " original.m2ts " + taken + " true",
		`SELECT state || ' ' || dir || ' ' || (packageid IS NULL) || ' ' || (completedat IS NULL)
			FROM com_nalet_katalog_itemversions WHERE item_id = '` + mgTaken + `'`: "taken " + filepath.Dir(taken) + " true true",
		`SELECT count(*)::text FROM com_nalet_katalog_playbackassets WHERE item_id = '` + mgTaken + `' AND kind = 'packaged'`: "0",
		`SELECT state FROM com_nalet_katalog_itemversions WHERE item_id = '` + mgInto + `'`:                                   "complete",
	} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, sql).Scan(&got); err != nil || got != want {
			t.Errorf("%s:\n got  %q, %v\n want %q", sql, got, err, want)
		}
	}

	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package'), ('s3', 'library.retire.delay', '0')`)
	r := NewRetirer(f.st.Pool(), f.cfg, processing.New(f.st.Pool()))
	if rep, err := r.Pass(ctx); err != nil || rep.Originals != 1 {
		t.Fatalf("the retire pass: %+v, %v", rep, err)
	}
	if statOK(into) || !statOK(taken) {
		t.Errorf("after the retire pass: the packaged one's original there %v, the one taken in's %v", statOK(into), statOK(taken))
	}
}

// A revert brings the originals back from their versions' folders to where
// they were before the library, one the retire job retired since from the
// trash, and the catalog as it was.
func TestARevertBringsOriginalsBackFromTheirVersions(t *testing.T) {
	f := newVersionsFixture(t)
	ctx := context.Background()
	ids := []string{mgInto, mgTaken}
	before, tree := f.stateOf(t, ids...), f.tree(t)
	if rep, err := f.migration(t).Adopt(ctx, ids); err != nil || rep.Adopted != 2 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package'), ('s3', 'library.retire.delay', '0')`)
	r := NewRetirer(f.st.Pool(), f.cfg, processing.New(f.st.Pool()))
	if rep, err := r.Pass(ctx); err != nil || rep.Originals != 1 {
		t.Fatalf("the retire pass: %+v, %v", rep, err)
	}
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_settings`)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_itemprocessingsteps`)
	rep, err := f.migration(t).Revert(ctx, ids)
	if err != nil || rep.Reverted != 2 || rep.Refused+rep.Failed != 0 {
		t.Fatalf("Revert: %+v, %v", rep, err)
	}
	if got := f.stateOf(t, ids...); got != before {
		t.Errorf("the database after the revert:\n%s\nbefore the adopt:\n%s", got, before)
	}
	if got := f.tree(t); got != tree {
		t.Errorf("the tree after the revert:\n%s\nbefore the adopt:\n%s", got, tree)
	}
	if rep, err := f.migration(t).Adopt(ctx, ids); err != nil || rep.Adopted != 2 {
		t.Errorf("Adopt after the revert: %+v, %v", rep, err)
	}
}

// A plan that puts an original into the record anywhere but directly into
// the folder of a version of its own, under its library name, is refused,
// and so is one whose database has an original in the record where no move
// of it goes; nothing moves.
func TestAnOriginalGoesIntoTheRecordOnlyIntoItsVersion(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(f *migrationFixture, u *Unit)
		says string
	}{
		"another version's folder": {func(f *migrationFixture, u *Unit) {
			u.Moves[0].To = filepath.Join(VersionDir(u.ItemDir, IDOf("another")), "original.m2ts")
		}, "leaves what such a move may touch"},
		"named as it arrived": {func(f *migrationFixture, u *Unit) {
			u.Moves[0].To = filepath.Join(filepath.Dir(u.Moves[0].To), "Taken (2022).m2ts")
		}, "leaves what such a move may touch"},
		"in its package": {func(f *migrationFixture, u *Unit) {
			u.Moves[0].To = filepath.Join(filepath.Dir(u.Moves[0].To), "hls", "original.m2ts")
		}, "leaves what such a move may touch"},
		"a source where no move goes": {func(f *migrationFixture, u *Unit) {
			other := filepath.Join(u.DB.Versions[0].Dir, "original.ts")
			u.DB.Sources[0].ArrivalPath = &other
		}, "where no move of it goes"},
		"an asset where no move goes": {func(f *migrationFixture, u *Unit) {
			u.DB.Assets[0].Path = filepath.Join(u.DB.Versions[0].Dir, "original.ts")
		}, "where no move of an original goes"},
		"a version out of its place": {func(f *migrationFixture, u *Unit) {
			u.DB.Versions[0].Dir = filepath.Join(u.ItemDir, "versions", "elsewhere")
		}, "the path rules at"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newVersionsFixture(t)
			u := f.units[mgTaken]
			c.edit(f, u)
			f.unit(t, mgTaken, "movie", u.Moves, &u.Guards, u.DB)
			tree := f.tree(t)
			rep, err := f.migration(t).Adopt(context.Background(), []string{mgTaken})
			if err != nil || rep.Refused != 1 || !strings.Contains(rep.Units[0].Reason, c.says) {
				t.Fatalf("Adopt: %+v, %v; want it refused saying %q", rep, err, c.says)
			}
			if got := f.tree(t); got != tree {
				t.Errorf("the tree changed:\n%s\nwant\n%s", got, tree)
			}
		})
	}
}
