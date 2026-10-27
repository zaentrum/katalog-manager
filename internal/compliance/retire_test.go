package compliance

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The check's word on retiring an original (Passed): a title it checked
// against the rules both workers report now, not marked to be checked again
// since, that breaks no rule, passes; one that breaks a rule, one it never
// checked, one marked to be checked again, one checked against other rules,
// one whose records it couldn't read, one processing again can't help, and
// any while a worker hasn't reported, don't, each saying why.
func TestTheChecksWordOnRetiringAnOriginal(t *testing.T) {
	s, st, _ := library4(t)
	ctx := context.Background()
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	passed := func(ids ...string) map[string]string {
		t.Helper()
		got, err := s.Passed(ctx, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for id, want := range map[string]string{
		uuid(2):            "",
		uuid(1):            "the compliance check finds it breaks OVER_BITRATE_CAP, OVER_SIZE_CAP",
		uuid(4):            "the compliance check finds it breaks NO_SURROUND, IMAGE_SUBTITLES_MISSING",
		uuid(5):            "the compliance check finds it breaks NOT_PACKAGED",
		"an-unknown-title": "the compliance check hasn’t checked it",
	} {
		if got := passed(id)[id]; got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET unchecked = 'versions/v/package.json is larger than 4 MiB, more than any record'
		WHERE item_id = $1`, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "the compliance check couldn’t read its records: versions/v/package.json is larger "+
		"than 4 MiB, more than any record" {
		t.Errorf("records unread: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET unchecked = NULL, blocked = 'dolbyvision:7' WHERE item_id = $1`, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "the compliance check found that processing it again can’t help: "+
		"Dolby Vision profile 7 needs a tone-mapping encode" {
		t.Errorf("blocked: %q", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_compliance SET blocked = NULL WHERE item_id = $1`, uuid(2))
	s.Touch(ctx, uuid(2))
	if got := passed(uuid(2))[uuid(2)]; got != "it changed since the compliance check checked it: it waits to be checked again" {
		t.Errorf("marked to be checked again: %q", got)
	}
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(uuid(2))[uuid(2)]; got != "" {
		t.Errorf("checked again: %q", got)
	}
	// rules that change: no title is within them until it is checked again
	reportFor(t, s, Transcoder, strings.Replace(transcoderReport, `"movie1080Mbps": 8`, `"movie1080Mbps": 9`, 1))
	if got := passed(uuid(2))[uuid(2)]; !strings.HasPrefix(got, "the compliance check checked it against other rules") {
		t.Errorf("rules changed since: %q", got)
	}
	if _, err := s.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := passed(uuid(2))[uuid(2)]; got != "" {
		t.Errorf("checked against the rules reported now: %q", got)
	}
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_workerrules WHERE worker = 'packager'`)
	if got := passed(uuid(2))[uuid(2)]; got != "the packager hasn’t reported its rules, so the compliance check can’t tell "+
		"whether it is within them" {
		t.Errorf("the packager not reported: %q", got)
	}
}
