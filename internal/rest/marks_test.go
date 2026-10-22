package rest

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// What the analyzer writes of a title after its record was written reaches
// the record: its ranges replaced (PUT /api/segments/items/{id}) or deleted,
// its chapters replaced (PUT /api/chapters/items/{id}) or deleted, and a
// series' reset, which deletes its episodes' fingerprinted ranges, each
// mark the title, and the projector's next pass writes the marks the
// catalog holds into its metadata.json. Nothing of the title's row changes.
func TestTheAnalyzersMarksReachTheRecord(t *testing.T) {
	st := storetest.Open(t)
	dir := t.TempDir()
	v2Layout(t, st)
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	worker := iss.Service(t, "zaentrum-manager")
	ctx := context.Background()
	const series, episode = "5e5e5e5e-2222-4000-8000-000000000001", "e1e1e1e1-2222-4000-8000-000000000001"
	storetest.AddItem(t, st, series, "series", "A Series", "")
	storetest.AddItem(t, st, episode, "episode", "One", series)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET seasonnumber = 1, episodenumber = 1 WHERE id = $1`, episode)
	paths := library.PathsOf(cfg)
	for _, id := range []string{series, episode} {
		if _, err := paths.EnsureItemRecord(ctx, st.Pool(), id); err != nil {
			t.Fatal(err)
		}
	}
	p := library.NewProjector(st.Pool(), cfg)
	pass := func() int {
		t.Helper()
		items, _, err := p.Pass(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	if n := pass(); n != 2 {
		t.Fatalf("the first pass projected %d items, want 2", n)
	}
	marks := func() library.Doc {
		t.Helper()
		pl, err := library.PlaceOf(ctx, st.Pool(), episode)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(paths.ItemDir(pl), "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		d, err := library.DecodeDoc(b)
		if err != nil {
			t.Fatal(err)
		}
		m, _ := d.Get("marks")
		out, _ := m.(library.Doc)
		return out
	}
	count := func(key string) int {
		t.Helper()
		v, _ := marks().Get(key)
		l, _ := v.([]any)
		return len(l)
	}
	if count("segments")+count("chapters") != 0 {
		t.Fatalf("the episode's first projection holds marks: %v", marks())
	}
	for _, tc := range []struct {
		name, method, path, body string
		segments, chapters       int
	}{
		{"ranges replaced", http.MethodPut, "/api/segments/items/" + episode, `{"segments": [
			{"kind": "intro", "startMs": 0, "endMs": 48878, "source": "chromaprint", "confidence": 0.99, "label": "chromaprint x4"},
			{"kind": "credits", "startMs": 3428757, "endMs": 3432258, "source": "silence", "confidence": 0.4}]}`, 2, 0},
		{"chapters replaced", http.MethodPut, "/api/chapters/items/" + episode, `{"chapters": [
			{"startMs": 0, "endMs": 600000, "title": "Prologue"}, {"startMs": 600000, "endMs": 3432258, "title": "Part 1"}]}`, 2, 2},
		{"a series reset", http.MethodPost, "/api/analyze/series/" + series + "/reset", "", 1, 2},
		{"ranges deleted", http.MethodDelete, "/api/segments/items/" + episode, "", 0, 2},
		{"chapters deleted", http.MethodDelete, "/api/chapters/items/" + episode, "", 0, 0},
	} {
		if w := do(h, tc.method, tc.path, tc.body, worker); w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
		if n := pass(); n != 1 {
			t.Errorf("%s: %d items projected, want the episode", tc.name, n)
		}
		if s, c := count("segments"), count("chapters"); s != tc.segments || c != tc.chapters {
			t.Errorf("%s: the projection holds %d ranges and %d chapters, want %d and %d: %v", tc.name, s, c,
				tc.segments, tc.chapters, marks())
		}
	}
}
