package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The marks follow the catalog. A statement that replaces a title's ranges
// after its record was written — the analyzer's, a delete and an insert
// that leave the title's row as it was — marks it, and the projector's next
// pass writes them into its metadata.json; so does a chapter retitled, and
// an episode's ranges deleted, as a series' reset deletes them.
func TestTheMarksFollowTheCatalog(t *testing.T) {
	st := storetest.Open(t)
	fillProjection(t, st)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	root := t.TempDir()
	cfg := config.Config{LibraryRoot: root, WorkRoot: root + "/.work"}
	paths := PathsOf(cfg)
	ctx := context.Background()
	for _, id := range []string{fxFilm, fxPlain, fxSeries, fxEp1, fxEp2, fxEp3} {
		if _, err := paths.EnsureItemRecord(ctx, st.Pool(), id); err != nil {
			t.Fatal(err)
		}
	}
	p := NewProjector(st.Pool(), cfg)
	pass := func() int {
		t.Helper()
		items, _, err := p.Pass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	if n := pass(); n != 6 {
		t.Fatalf("the first pass projected %d items, want 6", n)
	}
	if n := pass(); n != 0 {
		t.Fatalf("a pass with nothing changed projected %d items", n)
	}
	projection := func(id string) Doc {
		t.Helper()
		pl, err := PlaceOf(ctx, st.Pool(), id)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(paths.ItemDir(pl), "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		d, err := DecodeDoc(b)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	listOf := func(id, key string) []any {
		t.Helper()
		m, ok := get(projection(id), "marks").(Doc)
		if !ok {
			t.Fatalf("%s's metadata.json has no marks", id)
		}
		return asSlice(get(m, key))
	}

	// The analyzer's ranges, written after the record.
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{`DELETE FROM com_nalet_katalog_mediasegments WHERE item_id = $1`,
		`INSERT INTO com_nalet_katalog_mediasegments (id, item_id, kind, startms, endms, source, confidence, label) VALUES
			('n1', $1, 'intro', 3996, 19063, 'silence', 0.40, 'silence 15.1s'),
			('n2', $1, 'credits', 850000, 888000, 'tidb', 0.99, 'black 38.0s')`} {
		if _, err := tx.Exec(ctx, sql, fxFilm); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := pass(); n != 1 {
		t.Errorf("the film's ranges replaced: %d items projected, want the film", n)
	}
	segs := listOf(fxFilm, "segments")
	if len(segs) != 2 || textOr(segs[0].(Doc), "detector") != "silence" || textOr(segs[1].(Doc), "detector") != "tidb" {
		t.Errorf("the film's projected ranges: %v", segs)
	}

	// A chapter retitled.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemchapters SET title = 'Opening Credits' WHERE id = 'ch1'`)
	if n := pass(); n != 1 {
		t.Errorf("a chapter retitled: %d items projected, want the film", n)
	}
	if chs := listOf(fxFilm, "chapters"); len(chs) != 4 || textOr(chs[0].(Doc), "title") != "Opening Credits" {
		t.Errorf("the film's projected chapters: %v", chs)
	}

	// An episode's ranges deleted.
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_mediasegments WHERE item_id = $1`, fxEp1)
	if n := pass(); n != 1 {
		t.Errorf("an episode's ranges deleted: %d items projected, want the episode", n)
	}
	if segs := listOf(fxEp1, "segments"); len(segs) != 0 {
		t.Errorf("the episode's projected ranges after the delete: %v", segs)
	}
}
