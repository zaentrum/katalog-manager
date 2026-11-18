package captions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The PUT holds a read of the whole picture to the gop's ceiling as well,
// 1200 packets at the frame step: a 40 s film at 24 frames a second read
// whole with a gop of 50 s is a read, with one of 50.1 s a failed one. A
// number past a float in a report - a gop, a length or a span of 10^400,
// 1e999 - is no number, and the report none: refused, never a crash.
func TestThePutHoldsAWholeReadToTheGopsCeiling(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		gop  string
		read bool
	}{{"50", true}, {"50.1", false}} {
		svc, source, token := claimedOf(t, fortySeconds)
		body := strings.Replace(wholeBody(960, 0, "40"), `"duration": 40,`, `"duration": 40, "gop": `+c.gop+`,`, 1)
		r := record(t, ctx, svc, source, token, body)
		if (r.State == StateRead) != c.read || (!c.read && !strings.Contains(deref(r.Reason), "is longer than the 1200")) {
			t.Errorf("a whole read of 40 s, its gop %s s: %+v (%s); want it read %v", c.gop, r, deref(r.Reason), c.read)
		}
	}
	huge := "1" + strings.Repeat("0", 400)
	for _, c := range []struct{ name, old, new string }{
		{"a gop of 10^400", `"duration": 600,`, `"duration": 600, "gop": ` + huge + `,`},
		{"a length of 10^400", `"duration": 600`, `"duration": ` + huge},
		{"a span ending at 1e999", `"end": 600}`, `"end": 1e999}`},
	} {
		body := strings.Replace(fullRead, c.old, c.new, 1)
		if body == fullRead {
			t.Fatalf("%s: the read was not changed", c.name)
		}
		var r Report
		if err := json.Unmarshal([]byte(`{"token": "t", "original": `+body+`}`), &r); err == nil {
			t.Errorf("%s: a report; want none", c.name)
		}
	}
}
