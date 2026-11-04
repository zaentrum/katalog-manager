package graph

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/chapters"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// chapterReads, an admin's, says how far the chapters are: the titles that
// miss theirs (of them those packaged), what the reads found, and why the
// backfill reads nothing: it is off; acceptChapterReads settles only an
// unreadable title.
func TestChapterReadsSaysHowFarTheChaptersAre(t *testing.T) {
	st := storetest.Open(t)
	for _, m := range []struct{ id, source string }{{"m1", "s1"}, {"m2", "s2"}, {"m3", "s3"}} {
		storetest.AddItem(t, st, m.id, "movie", "Film "+m.id, "")
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state)
			VALUES ($1, $2, 'original.mkv', 6, 'present')`, m.source, m.id)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind, sourceid)
			VALUES (gen_random_uuid()::varchar, $1, $2, true, 'primary', $3)`, m.id, "/media/"+m.id+".mkv", m.source)
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_compliance (item_id, kind, ruleshash, records)
			VALUES ($1, 'movie', 'h', '{"srcEssence": {"chapters": true}}')`, m.id)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, state, dir, sourceids)
		VALUES ('v1', 'm1', 'complete', '/library/m1/versions/v1', ARRAY['s1']::varchar[])`)
	zero := int32(0)
	if _, err := chapters.Record(context.Background(), st, library.Paths{}, "m3", chapters.Report{
		Read:     chapters.ReadReport{Path: "/media/m3.mkv", Found: &zero},
		Chapters: []chapters.Chapter{}}, time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	resp := schema.Exec(as(admin), `{
		chapterReads { missing missingPackaged read fromRecord none unconfirmed unreadable accepted failed inFlight
			handedOutLastHour recordsReadLastHour lastReadAt backfill window maxInFlight maxPerHour maxRecordsPerHour
			lease idle problems }
	}`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	// m3's none: no record says the same, so it settles nothing
	want := `{"chapterReads":{"missing":3,"missingPackaged":1,"read":0,"fromRecord":0,"none":1,"unconfirmed":1,` +
		`"unreadable":0,"accepted":0,"failed":0,"inFlight":0,"handedOutLastHour":0,"recordsReadLastHour":0,` +
		`"lastReadAt":"2026-11-04T03:00:00Z","backfill":false,"window":"22:00-17:00 UTC","maxInFlight":1,` +
		`"maxPerHour":120,"maxRecordsPerHour":3600,"lease":"30m","idle":"chapters.backfill is off","problems":[]}}`
	if got := string(resp.Data); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// acceptChapterReads refuses a title whose read is none, saying why
	resp = schema.Exec(as(admin), `mutation { acceptChapterReads(items: ["m3"]) { accepted refused } }`, "", nil)
	if len(resp.Errors) > 0 || string(resp.Data) !=
		`{"acceptChapterReads":{"accepted":[],"refused":["m3: its last read is none, not unreadable"]}}` {
		t.Errorf("acceptChapterReads: %s %v", resp.Data, resp.Errors)
	}
	if resp := schema.Exec(as(viewer), `{ chapterReads { missing } }`, "", nil); len(resp.Errors) == 0 ||
		!strings.Contains(resp.Errors[0].Message, "chapterReads") {
		t.Errorf("a viewer read chapterReads: %+v", resp)
	}
}
