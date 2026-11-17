package graph

import (
	"encoding/json"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// captionReads, an admin's, counts originals (its package counters 0) and
// says that the retire job's extras step goes through the deletion gate,
// which a client that lets originals be deleted asks first.
func TestCaptionReadsSaysTheExtrasStepIsGated(t *testing.T) {
	st := storetest.Open(t)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	resp := schema.Exec(as(admin), `{ captionReads { originals toRead packaged packagesRead extrasGated } }`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	var got struct {
		CaptionReads struct {
			Originals, ToRead, Packaged, PackagesRead int
			ExtrasGated                               bool
		}
	}
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if c := got.CaptionReads; !c.ExtrasGated || c.Packaged != 0 || c.PackagesRead != 0 {
		t.Errorf("captionReads: %+v; want extrasGated, and no package counted", c)
	}
}
