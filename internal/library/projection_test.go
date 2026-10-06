package library

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// golden reads an expected projection the tool wrote.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "projection", "expected", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// imagesOf are the images of an export row, its bytes base64 as the export
// carries them.
func imagesOf(t *testing.T, row Doc) []ImageIn {
	t.Helper()
	var out []ImageIn
	for _, e := range asSlice(get(row, "artwork")) {
		a := e.(Doc)
		raw, err := base64.StdEncoding.DecodeString(textOr(a, "base64"))
		if err != nil {
			t.Fatal(err)
		}
		primary, _ := get(a, "isPrimary").(bool)
		in := ImageIn{Kind: textOr(a, "kind"), SHA256: SHA256(raw), Size: int64(len(raw)), Head: raw,
			Bytes: func() ([]byte, error) { return raw, nil }, Primary: primary, FetchedAt: get(a, "fetchedAt")}
		if s, ok := get(a, "sourcePath").(string); ok {
			in.SourcePath = &s
		}
		out = append(out, in)
	}
	return out
}

// The projections are the tool's, byte for byte: each item's metadata.json
// and each person's person.json the port makes of the export are what
// library-v2-from-catalog.py --projections-only wrote of it, its images the
// files the tool wrote.
func TestTheProjectionsAreTheTools(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "projection", "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	export, err := DecodeDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	items := asSlice(get(export, "items"))
	var images []string
	for _, e := range items {
		row := e.(Doc)
		id := textOr(row, "id")
		var seasons []int64
		seen := map[int64]bool{}
		for _, f := range items {
			ep := f.(Doc)
			if textOr(ep, "parentId") == id {
				if n, ok := pyInt(get(ep, "seasonNumber")); ok && !seen[n] {
					seen[n] = true
					seasons = append(seasons, n)
				}
			}
		}
		sort.Slice(seasons, func(i, j int) bool { return seasons[i] < seasons[j] })
		doc, files, err := MetadataDoc(row, ItemProjection{ProjectedBy: "library-v2-from-catalog", AsOf: fxAsOf,
			Images: imagesOf(t, row), Seasons: seasons, ExternalIDs: true})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}
		if want := golden(t, id+".metadata.json"); !bytes.Equal(got, want) {
			t.Errorf("%s's metadata.json:\n%s\nwant:\n%s", id, got, want)
		}
		for _, f := range files {
			images = append(images, id+"/"+f.Name)
		}
	}
	for _, e := range asSlice(get(export, "people")) {
		row := e.(Doc)
		id := textOr(row, "id")
		doc, files, err := PersonDoc(id, row, PersonProjection{ProjectedBy: "library-v2-from-catalog", AsOf: fxAsOf,
			Images: imagesOf(t, row)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}
		if want := golden(t, id+".person.json"); !bytes.Equal(got, want) {
			t.Errorf("%s's person.json:\n%s\nwant:\n%s", id, got, want)
		}
		for _, f := range files {
			images = append(images, id+"/"+f.Name)
		}
	}
	if got, want := imageNames(images), imageNames(strings.Fields(string(golden(t, "images.txt")))); got != want {
		t.Errorf("the images:\n%s\nwant:\n%s", got, want)
	}
}

// imageNames are paths as "<the folder's id>/<the image's name>", sorted.
func imageNames(paths []string) string {
	var out []string
	for _, p := range paths {
		dir := filepath.Base(filepath.Dir(p))
		if dir == "metadata" {
			dir = filepath.Base(filepath.Dir(filepath.Dir(p)))
		}
		out = append(out, dir+"/"+filepath.Base(p))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// asTheTool is a projection the catalog wrote as the tool writes it: the
// tool's name.
func asTheTool(t *testing.T, b []byte) []byte {
	t.Helper()
	d, err := DecodeDoc(b)
	if err != nil {
		t.Fatal(err)
	}
	if by, _ := d.Get("projectedBy"); by != "katalog-manager" {
		t.Errorf("projected by %v", by)
	}
	out, err := Encode(d.With("projectedBy", "library-v2-from-catalog"))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The projector writes, from the catalog, what the tool writes of its export:
// every recorded item's metadata.json and its images, every credited
// person's person.json and their portraits, but for its own name; the current
// reference ids are named as item.json names them.
// Then it notes the state each reflects, and a pass again writes nothing; a
// change marks the item, and the next pass projects it again; a person the
// catalog deleted loses their folder.
func TestTheProjectorWritesTheToolsProjections(t *testing.T) {
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
	at, _ := time.Parse(time.RFC3339, fxAsOf)
	p := NewProjector(st.Pool(), cfg)
	p.now = func() time.Time { return at }
	items, people, err := p.Pass(ctx)
	if err != nil || items != 6 || people != 3 {
		t.Fatalf("the pass: %d items, %d people, %v; want 6 and 3", items, people, err)
	}
	var images []string
	for _, id := range []string{fxFilm, fxPlain, fxSeries, fxEp1, fxEp2, fxEp3} {
		pl, err := PlaceOf(ctx, st.Pool(), id)
		if err != nil {
			t.Fatal(err)
		}
		dir := paths.ItemDir(pl)
		b, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := asTheTool(t, b), golden(t, id+".metadata.json"); !bytes.Equal(got, want) {
			t.Errorf("%s's metadata.json:\n%s\nwant:\n%s", id, got, want)
		}
		d, _ := DecodeDoc(b)
		ids, _ := Encode(get(d, "externalIds"))
		item, _ := os.ReadFile(filepath.Join(dir, ItemFile))
		rec, _ := DecodeDoc(item)
		want, _ := Encode(get(rec, "externalIds"))
		if !bytes.Equal(ids, want) {
			t.Errorf("%s's current reference ids: %s, want item.json's %s", id, ids, want)
		}
		entries, _ := os.ReadDir(filepath.Join(dir, "metadata"))
		for _, e := range entries {
			images = append(images, filepath.Join(dir, "metadata", e.Name()))
		}
	}
	for _, id := range []string{fxAda, fxBen, fxCy} {
		dir := paths.PersonDir(id)
		b, err := os.ReadFile(filepath.Join(dir, "person.json"))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := asTheTool(t, b), golden(t, id+".person.json"); !bytes.Equal(got, want) {
			t.Errorf("%s's person.json:\n%s\nwant:\n%s", id, got, want)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Name() != "person.json" {
				images = append(images, filepath.Join(dir, e.Name()))
			}
		}
	}
	if got, want := imageNames(images), imageNames(strings.Fields(string(golden(t, "images.txt")))); got != want {
		t.Errorf("the images:\n%s\nwant:\n%s", got, want)
	}

	// Nothing changed: nothing is written.
	if items, people, err := p.Pass(ctx); err != nil || items+people != 0 {
		t.Errorf("a pass again: %d items, %d people, %v", items, people, err)
	}
	// A genre given to the film marks it; renamed, Ben marks the titles that
	// credit him, and is projected again.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemgenres (id, item_id, genre_id) VALUES ('ig9', $1, 'g3')`, fxFilm)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_people SET name = 'Ben B. Example', modifiedat = now() WHERE id = $1`, fxBen)
	items, people, err = p.Pass(ctx)
	if err != nil || items != 3 || people != 1 {
		t.Errorf("after a change: %d items, %d people, %v; want the film, the series, the episode and Ben", items, people, err)
	}
	b, _ := os.ReadFile(filepath.Join(paths.MovieDir(fxFilm), "metadata.json"))
	if !bytes.Contains(b, []byte(`"Science Fiction"`)) {
		t.Error("the film's genres are not projected again")
	}
	// Deleted: the folder goes.
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_people WHERE id = $1`, fxCy)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_deleteditems (id, type, title, deletedby) VALUES ($1, 'person', 'Cy', 'test')`, fxCy)
	if _, _, err := p.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.PersonDir(fxCy)); !os.IsNotExist(err) {
		t.Errorf("a deleted person's folder: %v", err)
	}
	// The legacy layout projects nothing.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_settings SET valuetext = 'legacy'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET modifiedat = now()`)
	if items, people, err := p.Pass(ctx); err != nil || items+people != 0 {
		t.Errorf("the legacy layout: %d items, %d people, %v", items, people, err)
	}
}
