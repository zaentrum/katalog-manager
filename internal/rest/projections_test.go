package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// POST /api/library/projections writes the named items' projections now,
// with the legacy layout too, and answers what became of each; a body that
// is no list of items is refused.
func TestTheProjectionsRoute(t *testing.T) {
	st := storetest.Open(t)
	const film = "f1f1f1f1-2222-4000-8000-000000000001"
	storetest.AddItem(t, st, film, "movie", "A Film", "")
	dir := t.TempDir()
	cfg := v2Config(dir)
	if _, err := library.PathsOf(cfg).EnsureItemRecord(context.Background(), st.Pool(), film); err != nil {
		t.Fatal(err)
	}
	h, iss := server(t, st, cfg)
	service := iss.Service(t, "zaentrum-manager")
	w := do(h, http.MethodPost, "/api/library/projections", `{"items": ["`+film+`", "nobody"]}`, service)
	var rep library.RefreshReport
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rep) != nil {
		t.Fatalf("the route: %d %s", w.Code, w.Body.String())
	}
	if rep.Projected != 1 || rep.Failed != 1 || len(rep.Items) != 2 || rep.Items[0].State != "projected" ||
		rep.Items[1].Reason != "unknown item" {
		t.Errorf("the answer: %s", w.Body.String())
	}
	if _, err := os.Stat(library.PathsOf(cfg).MovieDir(film) + "/metadata.json"); err != nil {
		t.Errorf("no metadata.json: %v", err)
	}
	if w := do(h, http.MethodPost, "/api/library/projections", `{}`, service); w.Code != http.StatusOK ||
		w.Body.String() != `{"projected":0,"unchanged":1,"failed":0,"items":[]}`+"\n" {
		t.Errorf("every item, all current: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodPost, "/api/library/projections", `["a"]`, service); w.Code != http.StatusBadRequest {
		t.Errorf("a body that is no list of items: %d %s", w.Code, w.Body.String())
	}
}
