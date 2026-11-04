package graph

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

func bytesOf(n int64) *int64 { return &n }

func boolOf(b bool) *bool { return &b }

// The items of a list read what they have on disk as one page: each of them
// has the same page, the first field asked reads it for all of them, in the
// list's order, once however many fields of however many items ask at once,
// and each item takes its own from the answer (one the answer lacks has
// nothing). A page that could not be read is every field's error, read once
// all the same, and so is one whose read panicked, which no item reads as
// having nothing. An item made on its own is a page of one.
func TestAListReadsItsSizesOnce(t *testing.T) {
	rows := []*model.ItemRow{{Item: model.Item{ID: "m1"}}, {Item: model.Item{ID: "m2"}}, {Item: model.Item{ID: "e1"}}}
	rs := newItemResolvers(rows, nil, library.Paths{})
	page := rs[0].sizes
	for _, r := range rs {
		if r.sizes != page {
			t.Fatalf("item %s reads another page", r.m.ID)
		}
	}
	var mu sync.Mutex
	var reads [][]string
	page.read = func(_ context.Context, ids []string) (map[string]store.ItemSizes, error) {
		mu.Lock()
		defer mu.Unlock()
		reads = append(reads, ids)
		return map[string]store.ItemSizes{
			"m1": {OriginalBytes: bytesOf(4_000_000_000), PackageBytes: bytesOf(1_500_000_000), HasOriginal: boolOf(true)},
			"m2": {PackageBytes: bytesOf(900_000_000), HasOriginal: boolOf(false)},
		}, nil
	}
	type sizes struct {
		original, pkg *float64
		has           *bool
	}
	got := make([]sizes, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := r.OriginalBytes(context.Background())
			p, err2 := r.PackageBytes(context.Background())
			h, err3 := r.HasOriginal(context.Background())
			if err := errors.Join(err, err2, err3); err != nil {
				t.Errorf("item %s: %v", r.m.ID, err)
			}
			got[i] = sizes{o, p, h}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(reads, [][]string{{"m1", "m2", "e1"}}) {
		t.Errorf("the page was read %d times, of %v; want once, of m1 m2 e1", len(reads), reads)
	}
	f := func(n float64) *float64 { return &n }
	if want := []sizes{{f(4e9), f(1.5e9), boolOf(true)}, {nil, f(9e8), boolOf(false)}, {nil, nil, nil}}; !reflect.DeepEqual(got, want) {
		t.Errorf("the sizes:\n got  %v\n want %v", got, want)
	}

	for name, read := range map[string]func(context.Context, []string) (map[string]store.ItemSizes, error){
		"an error": func(context.Context, []string) (map[string]store.ItemSizes, error) {
			return nil, errors.New("the catalog is down")
		},
		"a panic": func(context.Context, []string) (map[string]store.ItemSizes, error) {
			panic("the catalog is down")
		},
	} {
		failing := newItemResolvers(rows[:2], nil, library.Paths{})
		n := 0
		failing[0].sizes.read = func(ctx context.Context, ids []string) (map[string]store.ItemSizes, error) {
			n++
			return read(ctx, ids)
		}
		for _, r := range failing {
			if o, err := r.OriginalBytes(context.Background()); err == nil || o != nil || !strings.Contains(err.Error(), "the catalog is down") {
				t.Errorf("%s: item %s: %v %v, want the page's error", name, r.m.ID, o, err)
			}
			if has, err := r.HasOriginal(context.Background()); err == nil || has != nil {
				t.Errorf("%s: item %s: %v %v, want the page's error", name, r.m.ID, has, err)
			}
		}
		if n != 1 {
			t.Errorf("%s: a page that could not be read was read %d times, want once", name, n)
		}
	}

	one := newItemResolver(&model.Item{ID: "x1"}, nil, library.Paths{})
	if one.sizes == nil || !reflect.DeepEqual(one.sizes.ids, []string{"x1"}) || one.sizes == page {
		t.Errorf("an item on its own reads %+v, want a page of its own", one.sizes)
	}
}

// onDiskRow is what a query asks of an item's sizes.
type onDiskRow struct {
	ID            string   `json:"id"`
	OriginalBytes *float64 `json:"originalBytes"`
	PackageBytes  *float64 `json:"packageBytes"`
	HasOriginal   *bool    `json:"hasOriginal"`
}

func (r onDiskRow) String() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// listed reads a list of items an answer gives under key, by id.
func listed(t *testing.T, data, key string) map[string]onDiskRow {
	t.Helper()
	var answer map[string][]onDiskRow
	if err := json.Unmarshal([]byte(data), &answer); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	out := map[string]onDiskRow{}
	for _, r := range answer[key] {
		out[r.ID] = r
	}
	return out
}

func idsOf(rows map[string]onDiskRow) []string {
	out := make([]string, 0, len(rows))
	for id := range rows {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// What a title has on disk, as the catalog's list reads it: the size of its
// sources present, every one of them (a version in parts, an old file
// replaceSource kept beside the new one), not one removed nor one retired, a
// file of no source (the legacy layout's) with its size as its original, and
// the size of its packaged row, none for a title taken in without one. A
// title whose original was retired has its package and no original. An
// episode another's file covers has no sizes of its own, that file counted
// once, on its holder, and has an original while its holder has. A series, a
// season and an album have nothing, not even a no. The list's filter keeps
// the titles that have an original, or those that have none, never a
// container, with the other filters; none given keeps every item. A title
// read on its own, a series' children and a holder's covered episodes read
// the same.
func TestWhatATitleHasOnDisk(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	ctx := context.Background()
	for _, it := range []struct{ id, typ, title, parent string }{
		{"m1", "movie", "A Film", ""},            // its original present, packaged
		{"m2", "movie", "Another Film", ""},      // its original retired after packaging
		{"m3", "movie", "A Third Film", ""},      // taken in: its original present, no package
		{"m4", "movie", "A Long Film", ""},       // its original in two parts, and one removed
		{"m5", "movie", "An Old Film", ""},       // the legacy layout's: a file of no source
		{"m6", "movie", "A Remastered Film", ""}, // replaceSource kept its old file
		{"m7", "movie", "A Lost Film", ""},       // a file of no source whose size isn't known
		{"s1", "series", "A Show", ""},
		{"sn", "season", "Season 2", "s1"},
		{"al", "album", "An Album", ""},
		{"e1", "episode", "First", "s1"},  // holds e2: its original present, packaged
		{"e2", "episode", "Second", "s1"}, // its file is e1's
		{"e3", "episode", "Third", "s1"},  // holds e4: its original retired
		{"e4", "episode", "Fourth", "s1"}, // its file is e3's
	} {
		storetest.AddItem(t, st, it.id, it.typ, it.title, it.parent)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = right(id, 1)::int
		WHERE type = 'episode'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 2 WHERE id = 'sn'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state) VALUES
		('src-m1', 'm1', 'original.mkv', 4000000000, 'present'),
		('src-m2', 'm2', 'original.mkv', 6000000000, 'deleted'),
		('src-m3', 'm3', 'original.mkv', 2500000000, 'present'),
		('src-m4a', 'm4', 'original-1.mkv', 1000000000, 'present'),
		('src-m4b', 'm4', 'original-2.mkv', 2000000000, 'present'),
		('src-m4c', 'm4', 'Another Cut.mkv', 3000000000, 'removed'),
		('src-m6old', 'm6', 'A Remastered Film (1999).mp4', 5000000000, 'present'),
		('src-m6', 'm6', 'A Remastered Film (1999).mkv', 3500000000, 'present'),
		('src-e1', 'e1', 'original.mkv', 2000000000, 'present'),
		('src-e3', 'e3', 'original.mkv', 1800000000, 'deleted')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES
		('v-m1', 'm1', ARRAY['src-m1'], 'complete'), ('v-m2', 'm2', ARRAY['src-m2'], 'complete'),
		('v-m3', 'm3', ARRAY['src-m3'], 'taken'), ('v-m4', 'm4', ARRAY['src-m4a', 'src-m4b'], 'taken'),
		('v-e1', 'e1', ARRAY['src-e1'], 'complete'), ('v-e3', 'e3', ARRAY['src-e3'], 'complete')`)
	// a title's file, its package, and of an original retired its row, of
	// kind original and no longer primary
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind, sourceid,
			versionid, codec) VALUES
		('a-m1', 'm1', '/library/m1/versions/v-m1/original.mkv', 4000000000, true, 'primary', 'src-m1', NULL, NULL),
		('p-m1', 'm1', '/library/m1/versions/v-m1/package.json', 1500000000, false, 'packaged', NULL, 'v-m1', 'hvc1'),
		('a-m2', 'm2', '/library/m2/sources/src-m2/source.json', 6000000000, false, 'original', 'src-m2', NULL, NULL),
		('p-m2', 'm2', '/library/m2/versions/v-m2/package.json', 900000000, false, 'packaged', NULL, 'v-m2', 'hvc1'),
		('a-m3', 'm3', '/library/m3/versions/v-m3/original.mkv', 2500000000, true, 'primary', 'src-m3', NULL, NULL),
		('a-m4', 'm4', '/library/m4/versions/v-m4/original-1.mkv', 1000000000, true, 'primary', 'src-m4a', NULL, NULL),
		('a-m5', 'm5', '/media/An Old Film (1999).mkv', 700000000, true, 'primary', NULL, NULL, NULL),
		('a-m6', 'm6', '/media/A Remastered Film (1999).mkv', 3500000000, true, 'primary', 'src-m6', NULL, NULL),
		('a-m7', 'm7', '/media/A Lost Film (1931).mkv', NULL, true, 'primary', NULL, NULL, NULL),
		('a-e1', 'e1', '/library/s1/e1/versions/v-e1/original.mkv', 2000000000, true, 'primary', 'src-e1', NULL, NULL),
		('p-e1', 'e1', '/library/s1/e1/versions/v-e1/package.json', 800000000, false, 'packaged', NULL, 'v-e1', 'hvc1'),
		('a-e3', 'e3', '/library/s1/e3/sources/src-e3/source.json', 1800000000, false, 'original', 'src-e3', NULL, NULL),
		('p-e3', 'e3', '/library/s1/e3/versions/v-e3/package.json', 700000000, false, 'packaged', NULL, 'v-e3', 'hvc1')`)
	for _, l := range [][2]string{{"e1", "e2"}, {"e3", "e4"}} {
		if _, err := library.Link(ctx, st.Pool(), l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	f := func(n float64) *float64 { return &n }
	yes, no := boolOf(true), boolOf(false)
	want := map[string]onDiskRow{
		"m1": {"m1", f(4e9), f(1.5e9), yes},
		"m2": {"m2", nil, f(9e8), no},
		"m3": {"m3", f(2.5e9), nil, yes},
		"m4": {"m4", f(3e9), nil, yes},
		"m5": {"m5", f(7e8), nil, yes},
		"m6": {"m6", f(8.5e9), nil, yes},
		"m7": {"m7", nil, nil, no},
		"s1": {"s1", nil, nil, nil},
		"sn": {"sn", nil, nil, nil},
		"al": {"al", nil, nil, nil},
		"e1": {"e1", f(2e9), f(8e8), yes},
		"e2": {"e2", nil, nil, yes},
		"e3": {"e3", nil, f(7e8), no},
		"e4": {"e4", nil, nil, no},
	}
	const fields = `id originalBytes packageBytes hasOriginal`
	if got := listed(t, query(t, st, `{ items(limit: 200) { `+fields+` } }`), "items"); !reflect.DeepEqual(got, want) {
		t.Errorf("the list:\n got  %v\n want %v", got, want)
	}

	for _, tc := range []struct {
		args string
		want []string
	}{
		{`hasOriginal: true`, []string{"e1", "e2", "m1", "m3", "m4", "m5", "m6"}},
		{`hasOriginal: false`, []string{"e3", "e4", "m2", "m7"}},
		{`type: "movie", hasOriginal: false`, []string{"m2", "m7"}},
		{`type: "series", hasOriginal: false`, []string{}},
		{`type: "episode", hasOriginal: true`, []string{"e1", "e2"}},
		{`search: "film", hasOriginal: true`, []string{"m1", "m3", "m4", "m5", "m6"}},
		{`hasOriginal: null`, idsOf(want)},
	} {
		got := listed(t, query(t, st, `{ items(`+tc.args+`) { `+fields+` } }`), "items")
		if ids := idsOf(got); !reflect.DeepEqual(ids, tc.want) {
			t.Errorf("items(%s): %v, want %v", tc.args, ids, tc.want)
		}
		for id, r := range got {
			if r.String() != want[id].String() {
				t.Errorf("items(%s): %s, want %s", tc.args, r, want[id])
			}
		}
	}
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin),
		`query($has: Boolean) { items(hasOriginal: $has) { id } }`, "", map[string]any{"has": false})
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	if ids := idsOf(listed(t, string(resp.Data), "items")); !reflect.DeepEqual(ids, []string{"e3", "e4", "m2", "m7"}) {
		t.Errorf("items($has: false): %v", ids)
	}

	for id, w := range want {
		var answer struct{ Item onDiskRow }
		if err := json.Unmarshal([]byte(query(t, st, `{ item(id: "`+id+`") { `+fields+` } }`)), &answer); err != nil {
			t.Fatal(err)
		}
		if answer.Item.String() != w.String() {
			t.Errorf("item %s on its own: %s, want %s", id, answer.Item, w)
		}
	}
	var series struct {
		Item struct{ Children []onDiskRow }
	}
	if err := json.Unmarshal([]byte(query(t, st, `{ item(id: "s1") { children { `+fields+` } } }`)), &series); err != nil {
		t.Fatal(err)
	}
	var children []string
	for _, c := range series.Item.Children {
		children = append(children, c.String())
	}
	if w := []string{want["e1"].String(), want["e2"].String(), want["e3"].String(), want["e4"].String(), want["sn"].String()}; !reflect.DeepEqual(children, w) {
		t.Errorf("the series' children:\n got  %v\n want %v", children, w)
	}
	if got := query(t, st, `{ item(id: "e1") { covers { `+fields+` } } }`); got !=
		`{"item":{"covers":[{"id":"e2","originalBytes":null,"packageBytes":null,"hasOriginal":true}]}}` {
		t.Errorf("the episodes e1's file covers: %s", got)
	}
}

// A page whose sizes could not be read leaves the list as it is: its items
// are listed, each with its sizes and hasOriginal null and an error, not an
// answer of no data at all.
func TestAPageThatCannotBeReadKeepsTheList(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	query(t, st, `{ items { id hasOriginal } }`)                              // the catalog's migrations seen in place
	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_itemsources CASCADE`) // and the view of migration 055 that reads it
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin),
		`{ items { id originalBytes packageBytes hasOriginal } }`, "", nil)
	if got := listed(t, string(resp.Data), "items"); !reflect.DeepEqual(idsOf(got), []string{"m1", "m2"}) ||
		got["m1"].String() != `{"id":"m1","originalBytes":null,"packageBytes":null,"hasOriginal":null}` {
		t.Errorf("the list: %s", resp.Data)
	}
	if len(resp.Errors) != 6 {
		t.Errorf("%d errors, want one an item and field: %v", len(resp.Errors), resp.Errors)
	}
}

// A catalog without migration 040 keeps no sources: a title's original is its
// file, its primary asset with its size, while it has one, and its package
// its packaged row; without 045 no episode is covered, and each reads its
// own. The filter holds the titles to the same, and leaves a series out.
func TestWhatATitleHasOnDiskWithoutTheLibrary(t *testing.T) {
	st := storetest.OpenBase(t)
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Another Film", "")
	storetest.AddItem(t, st, "m3", "movie", "A Third Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Show", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, sizebytes, isprimary, kind) VALUES
		('a-m1', 'm1', '/media/A Film (2001).mkv', 3000000000, true, 'primary'),
		('p-m1', 'm1', '/packages/m1/manifest.json', 1000000000, false, 'packaged'),
		('p-m2', 'm2', '/packages/m2/manifest.json', 500000000, false, 'packaged'),
		('a-m3', 'm3', '/media/A Third Film (2003).mkv', NULL, true, 'primary')`)
	f := func(n float64) *float64 { return &n }
	want := map[string]onDiskRow{
		"m1": {"m1", f(3e9), f(1e9), boolOf(true)},
		"m2": {"m2", nil, f(5e8), boolOf(false)},
		"m3": {"m3", nil, nil, boolOf(false)},
		"s1": {"s1", nil, nil, nil},
	}
	const fields = `id originalBytes packageBytes hasOriginal`
	if got := listed(t, query(t, st, `{ items { `+fields+` } }`), "items"); !reflect.DeepEqual(got, want) {
		t.Errorf("the list:\n got  %v\n want %v", got, want)
	}
	if ids := idsOf(listed(t, query(t, st, `{ items(hasOriginal: false) { id } }`), "items")); !reflect.DeepEqual(ids, []string{"m2", "m3"}) {
		t.Errorf("items(hasOriginal: false): %v", ids)
	}
	if ids := idsOf(listed(t, query(t, st, `{ items(hasOriginal: true) { id } }`), "items")); !reflect.DeepEqual(ids, []string{"m1"}) {
		t.Errorf("items(hasOriginal: true): %v", ids)
	}
}
