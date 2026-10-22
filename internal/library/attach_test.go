package library

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The version a subtitle is attached to is the title's complete one, whose
// package plays, and else the version its file is taken in to, the one a
// package is added to later; never one being built, superseded or removed,
// one taken in of a file the title plays no more, or one without a folder.
// A complete version without a folder leaves the title without one: a
// version taken in beside it is not what plays.
func TestTheVersionASubtitleIsAttachedTo(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	p := PathsOf(config.Config{LibraryRoot: t.TempDir()})
	const item, src, other, third = "a2a2a2a2-0000-4000-8000-000000000001", "b2b2b2b2-0000-4000-8000-000000000002",
		"c2c2c2c2-0000-4000-8000-000000000003", "d2d2d2d2-0000-4000-8000-000000000004"
	storetest.AddItem(t, st, item, "movie", "A Film", "")
	itemDir := p.MovieDir(item)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
		VALUES ($1, $2, 'original.mkv', 10, 'present'), ($3, $2, 'original.mkv', 12, 'present'), ($4, $2, 'original.mkv', 14, 'present')`,
		src, item, other, third)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sourceid)
		VALUES ('a1', $1, $2, true, $3)`, item, filepath.Join(p.Arrivals, "A Film (2024)", "A Film (2024).mkv"), src)
	version := func(id, state string, sources ...string) {
		t.Helper()
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, dir)
			VALUES ($1, $2, $3, $4, $5)`, id, item, sources, state, VersionDir(itemDir, id))
	}
	plays := func(source string) {
		t.Helper()
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $2 WHERE id = 'a1' AND item_id = $1`, item, source)
	}
	attachable := func(what, want string) {
		t.Helper()
		v, err := AttachableVersion(ctx, st.Pool(), item)
		var no *NotAttachable
		switch {
		case want == "" && errors.As(err, &no) && no.Reason == notAttachable && v == nil:
		case want == "":
			t.Errorf("%s: %+v %v, want none", what, v, err)
		case err != nil || v == nil || v.ID != want || v.Dir == nil || *v.Dir != VersionDir(itemDir, want):
			t.Errorf("%s: %+v %v, want %s in its folder", what, v, err, want)
		}
	}

	attachable("no version", "")
	const building, superseded, removed = "e2e2e2e2-0000-4000-8000-000000000005", "f2f2f2f2-0000-4000-8000-000000000006",
		"a3a3a3a3-0000-4000-8000-000000000007"
	version(building, VersionBuilding, src)
	version(superseded, VersionSuperseded, src)
	version(removed, VersionRemoved, src)
	attachable("a version being built, one superseded and one removed", "")

	// Taken in: its folder holds the original the title plays from.
	const taken = "b3b3b3b3-0000-4000-8000-000000000008"
	version(taken, VersionTaken, src)
	attachable("taken in", taken)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET dir = NULL WHERE id = $1`, taken)
	attachable("taken in, the catalog not saying where its folder is", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET dir = $2 WHERE id = $1`, taken, VersionDir(itemDir, taken))

	// Another file's version taken in, and the file the title plays.
	const takenOther = "c3c3c3c3-0000-4000-8000-000000000009"
	version(takenOther, VersionTaken, other)
	attachable("beside another file's version taken in", taken)
	plays(other)
	attachable("given that file", takenOther)
	plays(third)
	attachable("given a file with no version yet", "")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_playbackassets SET sourceid = NULL WHERE id = 'a1'`)
	attachable("a file whose source the catalog does not know", "")
	plays(src)

	// A complete version plays, whatever is taken in beside it.
	const complete = "d3d3d3d3-0000-4000-8000-00000000000a"
	version(complete, VersionComplete, third)
	attachable("a complete version beside one taken in", complete)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemversions SET dir = NULL WHERE id = $1`, complete)
	attachable("a complete version without a folder", "")
}
