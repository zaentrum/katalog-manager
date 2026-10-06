package extras

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// With the v2 layout an extra's file lies under ARRIVALS_ROOT (beside its
// title, as the scanner's convention finds it) or EXTRAS_ROOT, and never in
// the library's record: the media root and the share's legacy folders are no
// roots of it any more.
func TestTheRootsOfAnExtrasFileWithTheV2Layout(t *testing.T) {
	f := newFixture(t)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('l', 'library.layout', 'v2')`)
	f.cfg.LibraryRoot, f.cfg.ArrivalsRoot, f.cfg.ExtrasRoot = f.dir, f.dir+"/.work/incoming", f.dir+"/.work/extras"
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	ctx := asAdmin()
	for _, rel := range []string{".work/extras/bbb/trailer.mov", ".work/incoming/Big Buck Bunny-trailer.mkv"} {
		if res, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, rel, 100), Kind: "trailer"}); err != nil ||
			!res.Created {
			t.Errorf("%s: %+v, %v", rel, res, err)
		}
	}
	for _, rel := range []string{"media/Big Buck Bunny-teaser.mkv", "library/movies/x/teaser.mkv", "extras/bbb/teaser.mkv"} {
		_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, rel, 100), Kind: "teaser"})
		if r := refusal(t, err); r.Status != http.StatusBadRequest || !strings.Contains(r.Message, "is not under ARRIVALS_ROOT or EXTRAS_ROOT") {
			t.Errorf("%s: %d %s", rel, r.Status, r.Message)
		}
	}
	// An extras' root inside the record holds no original.
	f.cfg.ExtrasRoot = f.dir + "/movies/bb"
	f.svc = New(f.st, f.cfg, policy, events.ProducerOn(f.w))
	_, err := f.svc.AddExtra(ctx, graph.AddExtraRequest{ItemID: film, Path: f.write(t, "movies/bb/teaser.mkv", 100), Kind: "teaser"})
	if r := refusal(t, err); r.Status != http.StatusBadRequest || !strings.Contains(r.Message, "is in the library's record") {
		t.Errorf("in the record: %d %s", r.Status, r.Message)
	}
}
