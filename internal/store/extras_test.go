package store_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// 039 makes a title's extras, one row per extra, keyed by its id, with the
// columns the catalog contract names and katalog-api reads; running it again
// changes nothing, and the startup check applies it where its table or an
// index of it is missing, and only then.
func TestItemExtrasMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if ready, err := st.ItemExtrasReady(ctx); err != nil || ready {
		t.Fatalf("ItemExtrasReady on a catalog without 039: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureItemExtras(ctx); err != nil {
			t.Fatalf("EnsureItemExtras, %d. time: %v", run, err)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		VALUES ('x1', 'm1', 'trailer', 'Trailer', 'api', '/m/a-trailer.mkv')`)
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.ItemExtras); err != nil {
			t.Fatalf("applying 039 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.ItemExtrasReady(ctx); err != nil || !ready {
		t.Fatalf("ItemExtrasReady after EnsureItemExtras: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE state = 'pending'
		AND NOT hidden AND attempts = 0 AND failures = 0 AND createdat > now() - interval '1 minute'`); n != 1 {
		t.Errorf("applying 039 again left %d extras as they were taken in, want the one", n)
	}
	want := `id character varying(36) NOT NULL
item_id character varying(36) NOT NULL
kind character varying(20) NOT NULL
title character varying(255) NOT NULL
localizedtitles jsonb NULL
language character varying(35) NULL
seasonnumber integer NULL
origin jsonb NULL
sourcepath character varying(2048) NULL
sourcesize bigint NULL
sourceqh1 character varying(71) NULL
recordpath character varying(2048) NULL
registeredby character varying(10) NOT NULL
sortorder integer NULL
hidden boolean NOT NULL
label character varying(255) NULL
state character varying(12) NOT NULL
error character varying(500) NULL
attempts integer NOT NULL
failures integer NOT NULL
nextretryat timestamp with time zone NULL
dispatchedat timestamp with time zone NULL
heartbeatat timestamp with time zone NULL
packagepath character varying(2048) NULL
packagedat timestamp with time zone NULL
durationms bigint NULL
videocodec character varying(40) NULL
width integer NULL
height integer NULL
peakbandwidthbps bigint NULL
packagesizebytes bigint NULL
removedat timestamp with time zone NULL
removedby character varying(255) NULL
removalreason character varying(500) NULL
createdat timestamp with time zone NOT NULL
createdby character varying(255) NULL
modifiedat timestamp with time zone NOT NULL
modifiedby character varying(255) NULL`
	if got := columns(t, st, "com_nalet_katalog_itemextras"); got != want {
		t.Errorf("com_nalet_katalog_itemextras:\n%s\nwant:\n%s", got, want)
	}

	// A kind of the list, a title, a season of 0 or more, objects for the
	// titles in other languages and the origin, a quick hash written
	// sha256:<64 lowercase hex>, how it was taken in and a state of their
	// lists; one live extra per file.
	qh1 := "'sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" + "'"
	for _, bad := range []string{
		`(id, item_id, kind, title, registeredby) VALUES ('b1', 'm1', 'clip', 'A clip', 'api')`,
		`(id, item_id, kind, title, registeredby) VALUES ('b1', 'm1', 'trailer', '', 'api')`,
		`(id, item_id, kind, title, registeredby, seasonnumber) VALUES ('b1', 'm1', 'trailer', 'T', 'api', -1)`,
		`(id, item_id, kind, title, registeredby, localizedtitles) VALUES ('b1', 'm1', 'trailer', 'T', 'api', '["Trailer"]')`,
		`(id, item_id, kind, title, registeredby, origin) VALUES ('b1', 'm1', 'trailer', 'T', 'api', '"somewhere"')`,
		`(id, item_id, kind, title, registeredby, sourceqh1) VALUES ('b1', 'm1', 'trailer', 'T', 'api', 'sha256:ABC')`,
		`(id, item_id, kind, title, registeredby, sourceqh1) VALUES ('b1', 'm1', 'trailer', 'T', 'api', upper(` + qh1 + `))`,
		`(id, item_id, kind, title, registeredby) VALUES ('b1', 'm1', 'trailer', 'T', 'user')`,
		`(id, item_id, kind, title, registeredby, state) VALUES ('b1', 'm1', 'trailer', 'T', 'api', 'done')`,
		`(id, item_id, kind, title, registeredby, sourcepath) VALUES ('b1', 'm2', 'teaser', 'T', 'api', '/m/a-trailer.mkv')`,
		`(id, item_id, kind, title, registeredby) VALUES ('x1', 'm1', 'teaser', 'T', 'api')`,
	} {
		if _, err := st.Pool().Exec(ctx, `INSERT INTO com_nalet_katalog_itemextras `+bad); err == nil {
			t.Errorf("taken: %s", bad)
		}
	}
	for _, good := range []string{
		`(id, item_id, kind, title, registeredby, sourceqh1, localizedtitles, origin, seasonnumber) VALUES
			('g1', 'm1', 'making-of', 'Making Of', 'library', ` + qh1 + `, '{"de": "Hinter den Kulissen"}', '{"kind": "link"}', 0)`,
		`(id, item_id, kind, title, registeredby) VALUES ('g2', 'm1', 'gag-reel', 'Bloopers', 'scanner')`,
		`(id, item_id, kind, title, registeredby) VALUES ('g3', 'm1', 'other', 'Extra', 'scanner')`,
	} {
		storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras `+good)
	}
	// A removed extra's file may be an extra again.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET removedat = now() WHERE id = 'x1'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath)
		VALUES ('x2', 'm1', 'trailer', 'Trailer', 'api', '/m/a-trailer.mkv')`)

	// An index missing brings the migration back; the rows stay.
	storetest.Exec(t, st, `DROP INDEX idx_itemextras_due`)
	if ready, err := st.ItemExtrasReady(ctx); err != nil || ready {
		t.Fatalf("ItemExtrasReady with an index of 039 missing: %v, %v", ready, err)
	}
	if err := st.EnsureItemExtras(ctx); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.ItemExtrasReady(ctx); err != nil || !ready {
		t.Fatalf("ItemExtrasReady after the startup check: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras`); n != 5 {
		t.Errorf("%d extras after applying 039 again, want 5", n)
	}
}

// storetest.Open gives a test every migration the service applies at
// startup, 039 among them.
func TestTheTestSchemaHasTheItemExtras(t *testing.T) {
	if ready, err := storetest.Open(t).ItemExtrasReady(context.Background()); err != nil || !ready {
		t.Fatalf("ItemExtrasReady on the test schema: %v, %v", ready, err)
	}
}

// An item's extras go with it, the removed ones too, and nobody else's do;
// the deletion log names the item, not its extras.
func TestAnItemsExtrasGoWithIt(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "series", "Series B", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, removedat) VALUES
		('xa1', $1, 'trailer', 'Trailer', 'api', '/x/a-trailer.mkv', NULL),
		('xa2', $1, 'teaser', 'Teaser', 'scanner', '/x/a-teaser.mkv', now()),
		('xb1', $2, 'featurette', 'Featurette', 'api', '/x/b.mkv', NULL)`, movieA, movieB)
	if n, err := st.DeleteItems(context.Background(), []string{movieA}, store.Deletion{By: "subject-1"}); err != nil || n != 1 {
		t.Fatalf("DeleteItems: %d, %v", n, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE item_id = $1`, movieA); n != 0 {
		t.Errorf("%d extras of the deleted item are left", n)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE id = 'xb1'`); n != 1 {
		t.Error("another item's extra went with it")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_deleteditems`); n != 1 {
		t.Errorf("the deletion log holds %d rows, want the item's alone", n)
	}
}

// A catalog without migration 039 knows no extras, and deletes an item as
// before.
func TestExtrasWithoutTheMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if err := st.EnsureDeletionLog(ctx); err != nil {
		t.Fatal(err)
	}
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	if xs, err := st.ExtrasByItem(ctx, movieA, true); err != nil || xs != nil {
		t.Errorf("ExtrasByItem: %v, %v", xs, err)
	}
	if x, err := st.GetExtra(ctx, "x1"); err != nil || x != nil {
		t.Errorf("GetExtra: %v, %v", x, err)
	}
	if _, _, err := st.AddExtra(ctx, store.ExtraWrite{ItemID: movieA, Kind: "trailer", Title: "Trailer",
		SourcePath: "/x/a.mkv", RegisteredBy: "api", By: "admin-1"}); err != store.ErrNoExtras {
		t.Errorf("AddExtra: %v, want ErrNoExtras", err)
	}
	if ok, err := st.DeleteItem(ctx, movieA, store.Deletion{By: "admin-1"}); err != nil || !ok {
		t.Errorf("DeleteItem: %v, %v", ok, err)
	}
}

// extraPolicy retries a failed run twice, a minute and then two minutes
// later.
var extraPolicy = processing.Policy{MaxAttempts: 3, Backoff: time.Minute, BackoffMax: time.Hour,
	Timeouts: map[string]time.Duration{}, DefaultTimeout: 2 * time.Hour}

// anExtra takes in an extra of item at path, as the API does.
func anExtra(t *testing.T, st *store.Store, item, kind, path string) *model.Extra {
	t.Helper()
	x, created, err := st.AddExtra(context.Background(), store.ExtraWrite{ItemID: item, Kind: kind, Title: model.ExtraKindTitle(kind),
		SourcePath: path, SourceSize: 1000, SourceQH1: qh1Of("a"), RegisteredBy: "api", By: "admin-1"})
	if err != nil || !created {
		t.Fatalf("AddExtra %s: %v, %v", path, created, err)
	}
	return x
}

func qh1Of(s string) string { return "sha256:" + strings.Repeat(s, 64)[:64] }

// extraLine is an extra's packaging as a line: "state failures attempts
// error=<error> retry=<scheduled> sent=<outstanding> heard=<a word>".
func extraLine(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	var out string
	if err := st.Pool().QueryRow(context.Background(), `SELECT state || ' ' || failures || ' ' || attempts ||
		' error=' || COALESCE(error, '-') || ' retry=' || (nextretryat IS NOT NULL) || ' sent=' || (dispatchedat IS NOT NULL) ||
		' heard=' || (heartbeatat IS NOT NULL) FROM com_nalet_katalog_itemextras WHERE id = $1`, id).Scan(&out); err != nil {
		t.Fatalf("the extra %s: %v", id, err)
	}
	return out
}

// retryInSeconds is the seconds until the extra's next send, from now.
func retryInSeconds(t *testing.T, st *store.Store, id string) float64 {
	t.Helper()
	var secs float64
	if err := st.Pool().QueryRow(context.Background(), `SELECT extract(epoch FROM nextretryat - now())::float8
		FROM com_nalet_katalog_itemextras WHERE id = $1`, id).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	return secs
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// An extra is taken in once per file: waiting to be sent, under a new
// lower-case UUID, saying who took it in and how. The same file again for
// the same item is that extra; for another item a conflict naming it. A
// removed extra's file is taken in again as a new extra.
func TestAnExtraIsTakenInOncePerFile(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	storetest.AddItem(t, st, movieB, "movie", "Movie B", "")
	lang, season := "en", int32(0)
	w := store.ExtraWrite{ItemID: movieA, Kind: "trailer", Title: " Trailer ", Language: &lang, SeasonNumber: &season,
		SourcePath: "/x/a/trailer.mov", SourceSize: 1234, SourceQH1: qh1Of("b"), RegisteredBy: "api", By: "admin-1"}
	x, created, err := st.AddExtra(ctx, w)
	if err != nil || !created {
		t.Fatalf("AddExtra: %v, %v", created, err)
	}
	if !uuidRE.MatchString(x.ID) || x.ItemID != movieA || x.Kind != "trailer" || x.Title != "Trailer" || *x.Language != "en" ||
		*x.SeasonNumber != 0 || *x.SourcePath != "/x/a/trailer.mov" || *x.SourceSize != 1234 || *x.SourceQH1 != qh1Of("b") ||
		x.RegisteredBy != "api" || x.State != "pending" || x.Hidden || x.NextRetryAt != nil || *x.CreatedBy != "admin-1" ||
		*x.ModifiedBy != "admin-1" || x.Playable() {
		t.Errorf("the extra taken in: %+v", *x)
	}
	again, created, err := st.AddExtra(ctx, w)
	if err != nil || created || again.ID != x.ID {
		t.Errorf("the same file again: %v %v %v, want the extra %s", again, created, err, x.ID)
	}
	w.ItemID = movieB
	other, _, err := st.AddExtra(ctx, w)
	var conflict *store.ExtraConflict
	if !errors.As(err, &conflict) || conflict.Extra.ID != x.ID || other.ItemID != movieA ||
		err.Error() != "/x/a/trailer.mov is extra "+x.ID+" of item "+movieA+" already" {
		t.Errorf("the file for another item: %v, %v", other, err)
	}
	if _, err := st.RemoveExtra(ctx, x.ID, "admin-1", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	renewed, created, err := st.AddExtra(ctx, w)
	if err != nil || !created || renewed.ID == x.ID || renewed.ItemID != movieB {
		t.Errorf("a removed extra's file for another item: %v %v %v, want a new extra", renewed, created, err)
	}
	for _, bad := range []store.ExtraWrite{
		{ItemID: movieA, Kind: "clip", Title: "A clip", SourcePath: "/x/c.mkv", RegisteredBy: "api"},
		{ItemID: movieA, Kind: "trailer", Title: "  ", SourcePath: "/x/c.mkv", RegisteredBy: "api"},
	} {
		if _, _, err := st.AddExtra(ctx, bad); err == nil {
			t.Errorf("taken in: %+v", bad)
		}
	}
}

// A removed extra stays, removed, saying who removed it, when and why, and
// its package is due to be deleted once the grace is over; removing it again
// changes nothing. An item lists its extras as a viewer sees them, the
// removed ones only when asked.
func TestRemoveExtra(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	second := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	third := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET sortorder = 1 WHERE id = $1`, third.ID)
	x, err := st.RemoveExtra(ctx, second.ID, "admin-2", " a duplicate ", 24*time.Hour)
	if err != nil || x == nil || x.RemovedAt == nil || *x.RemovedBy != "admin-2" || *x.RemovalReason != "a duplicate" ||
		*x.ModifiedBy != "admin-2" || x.State != "pending" {
		t.Fatalf("RemoveExtra: %+v, %v", x, err)
	}
	if secs := retryInSeconds(t, st, second.ID); secs < 24*3600-60 || secs > 24*3600 {
		t.Errorf("the package of a removed extra is due in %.0fs, want a day", secs)
	}
	again, err := st.RemoveExtra(ctx, second.ID, "admin-3", "again", time.Minute)
	if err != nil || !again.RemovedAt.Equal(*x.RemovedAt) || *again.RemovedBy != "admin-2" {
		t.Errorf("removing it again: %+v, %v", again, err)
	}
	if gone, err := st.RemoveExtra(ctx, "unknown", "admin-2", "", time.Hour); err != nil || gone != nil {
		t.Errorf("an extra there is not: %v, %v", gone, err)
	}
	ids := func(removed bool) string {
		xs, err := st.ExtrasByItem(ctx, movieA, removed)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range xs {
			out = append(out, x.Kind)
		}
		return strings.Join(out, " ")
	}
	if got := ids(false); got != "featurette trailer" {
		t.Errorf("the item's extras %q, want the one with an order first, the removed one left out", got)
	}
	if got := ids(true); got != "featurette trailer teaser" {
		t.Errorf("with the removed: %q", got)
	}
	if x, err := st.GetExtra(ctx, second.ID); err != nil || x == nil || x.RemovedAt == nil {
		t.Errorf("GetExtra of a removed extra: %v, %v", x, err)
	}
}

// The workers' reports move an extra along: the transcoder's start
// (transcoding, a run counted once), its end (transcoded; not_applicable as
// done), the packager's start (packaging); the packager's end says nothing,
// packaging-complete makes it ready. A report of a run the extra is past
// changes nothing. Each report taken is the worker's word, and answers a
// trigger the service sent.
func TestWorkersReportsMoveAnExtraAlong(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	x := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	report := func(step, status string) string {
		t.Helper()
		if _, err := st.ReportExtraStep(ctx, x.ID, store.ExtraReport{Step: step, Status: status}, extraPolicy); err != nil {
			t.Fatalf("%s %s: %v", step, status, err)
		}
		return extraLine(t, st, x.ID)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued', dispatchedat = now() WHERE id = $1`, x.ID)
	for _, c := range []struct{ step, status, want string }{
		{"transcode", "in_progress", "transcoding 0 1 error=- retry=false sent=false heard=true"},
		{"transcode", "in_progress", "transcoding 0 1 error=- retry=false sent=false heard=true"},
		{"transcode", "not_applicable", "transcoded 0 1 error=- retry=false sent=false heard=true"},
		{"transcode", "done", "transcoded 0 1 error=- retry=false sent=false heard=true"},
		{"package", "in_progress", "packaging 0 1 error=- retry=false sent=false heard=true"},
		{"package", "done", "packaging 0 1 error=- retry=false sent=false heard=true"},
	} {
		if got := report(c.step, c.status); got != c.want {
			t.Errorf("%s %s: %s, want %s", c.step, c.status, got, c.want)
		}
	}
	codec, w, h, d, peak, size := "avc1.64001f", int32(1280), int32(720), int64(33000), int64(2400000), int64(5000000)
	done, err := st.CompleteExtraPackage(ctx, x.ID, store.ExtraPackage{Path: "/p/extras/" + x.ID[:2] + "/" + x.ID,
		DurationMs: &d, VideoCodec: &codec, Width: &w, Height: &h, PeakBandwidthBps: &peak, SizeBytes: &size})
	if err != nil || done.State != "ready" || !done.Playable() || *done.PackagePath != "/p/extras/"+x.ID[:2]+"/"+x.ID ||
		*done.DurationMs != d || *done.VideoCodec != codec || *done.Width != w || *done.Height != h ||
		*done.PeakBandwidthBps != peak || *done.PackageSizeBytes != size || done.PackagedAt == nil {
		t.Fatalf("CompleteExtraPackage: %+v, %v", done, err)
	}
	// The packager's end comes after packaging-complete; a late transcoder's
	// report of a run before, and its failure, change nothing.
	for _, r := range [][2]string{{"package", "done"}, {"transcode", "done"}, {"transcode", "failed"}, {"package", "failed"}} {
		if got := report(r[0], r[1]); got != "ready 0 1 error=- retry=false sent=false heard=true" {
			t.Errorf("%s %s after ready: %s", r[0], r[1], got)
		}
	}
	// A run started anew (a re-encode) counts again.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued' WHERE id = $1`, x.ID)
	if got := report("transcode", "in_progress"); got != "transcoding 0 2 error=- retry=false sent=false heard=true" {
		t.Errorf("a second run: %s", got)
	}
	if x, _ := st.GetExtra(ctx, x.ID); !x.Playable() {
		t.Error("the package does not play while a new one is made")
	}
}

// A failed run of an extra's transcode, or of its package, counts a failure
// and keeps its error, credentials redacted: pending, sent again a backoff
// later, while the policy retries that many failures in a row, then failed. A
// package that fails before the packager says it started counts too. A
// failure of a run the service gave up on (pending, failed) or is past
// changes nothing.
func TestAFailedRunOfAnExtraIsRetriedByThePolicy(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	x := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	queue := func() {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'queued', nextretryat = NULL,
			dispatchedat = now() WHERE id = $1`, x.ID)
	}
	fail := func(step, msg string) string {
		t.Helper()
		var e *string
		if msg != "" {
			e = &msg
		}
		if _, err := st.ReportExtraStep(ctx, x.ID, store.ExtraReport{Step: step, Status: "failed", Error: e}, extraPolicy); err != nil {
			t.Fatal(err)
		}
		return extraLine(t, st, x.ID)
	}
	queue()
	if got := fail("transcode", "ffmpeg: https://user:secret@example.com/x exited 1"); got !=
		"pending 1 0 error=ffmpeg: https://REDACTED@example.com/x exited 1 retry=true sent=false heard=true" {
		t.Errorf("the first failure: %s", got)
	}
	if secs := retryInSeconds(t, st, x.ID); secs < 50 || secs > 60 {
		t.Errorf("the first retry in %.0fs, want a minute", secs)
	}
	if got := fail("transcode", "late"); !strings.HasPrefix(got, "pending 1 0 error=ffmpeg") {
		t.Errorf("a failure while it waits for its retry: %s, want nothing changed", got)
	}
	queue()
	if _, err := st.ReportExtraStep(ctx, x.ID, store.ExtraReport{Step: "transcode", Status: "done"}, extraPolicy); err != nil {
		t.Fatal(err)
	}
	if got := fail("package", "transcoder handoff: renditions.json is not JSON"); got !=
		"pending 2 0 error=transcoder handoff: renditions.json is not JSON retry=true sent=false heard=true" {
		t.Errorf("a package failing before it started: %s", got)
	}
	if secs := retryInSeconds(t, st, x.ID); secs < 110 || secs > 120 {
		t.Errorf("the second retry in %.0fs, want two minutes", secs)
	}
	queue()
	if got := fail("transcode", ""); got != "failed 3 0 error=the transcode failed retry=false sent=false heard=true" {
		t.Errorf("the third failure: %s, want failed, no attempt left", got)
	}
	if got := fail("transcode", "again"); !strings.HasPrefix(got, "failed 3 0 error=the transcode failed") {
		t.Errorf("a failure of a failed extra: %s, want nothing changed", got)
	}
	// A run that starts after all heals it.
	if _, err := st.ReportExtraStep(ctx, x.ID, store.ExtraReport{Step: "transcode", Status: "in_progress"}, extraPolicy); err != nil {
		t.Fatal(err)
	}
	if got := extraLine(t, st, x.ID); got != "transcoding 3 1 error=- retry=false sent=false heard=true" {
		t.Errorf("a run of a failed extra: %s", got)
	}
}

// A worker's report of an extra there is not, or of a removed one, is
// refused as gone; of one whose file is missing as missing; a step or a
// status an extra does not have as such, and nothing changes.
func TestAReportThatIsNoneIsRefused(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	gone := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	missing := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	live := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	if _, err := st.RemoveExtra(ctx, gone.ID, "admin-1", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkExtrasMissing(ctx, []string{missing.ID}, "katalog-manager/scanner"); err != nil {
		t.Fatal(err)
	}
	rep := store.ExtraReport{Step: "transcode", Status: "in_progress"}
	for id, want := range map[string]error{"unknown": store.ErrExtraGone, gone.ID: store.ErrExtraGone, missing.ID: store.ErrExtraMissing} {
		if _, err := st.ReportExtraStep(ctx, id, rep, extraPolicy); !errors.Is(err, want) {
			t.Errorf("a report of %s: %v, want %v", id, err, want)
		}
	}
	for _, r := range []store.ExtraReport{{Step: "analyze", Status: "done"}, {Step: "transcode", Status: "skipped"},
		{Step: "package", Status: "pending"}, {Step: "transcode", Status: ""}} {
		if _, err := st.ReportExtraStep(ctx, live.ID, r, extraPolicy); err == nil ||
			!(errors.Is(err, processing.ErrBadStep) || errors.Is(err, processing.ErrBadStatus)) {
			t.Errorf("%+v: %v, want it refused", r, err)
		}
	}
	if got := extraLine(t, st, live.ID); got != "pending 0 0 error=- retry=false sent=false heard=false" {
		t.Errorf("refused reports changed the extra: %s", got)
	}
	if _, err := st.CompleteExtraPackage(ctx, gone.ID, store.ExtraPackage{Path: "/p"}); !errors.Is(err, store.ErrExtraGone) {
		t.Errorf("packaging-complete of a removed extra: %v", err)
	}
	// A package recorded for an extra whose file is missing is kept for when
	// the file is back; the extra stays missing.
	if x, err := st.CompleteExtraPackage(ctx, missing.ID, store.ExtraPackage{Path: "/p"}); err != nil || x.State != "missing" ||
		x.PackagedAt == nil || x.Playable() {
		t.Errorf("packaging-complete of a missing extra: %+v, %v", x, err)
	}
}

// The extras due to be sent are claimed once: the pending ones whose retry
// is due or that have none, of the ids given, the longest due first; each is
// queued, noted as sent. Those not due, removed, of an item gone or claimed
// already are left alone. One whose trigger could not be sent is put back,
// pending, due a backoff later, without a failure counted; one a worker
// reported on meanwhile is not.
func TestClaimDueExtras(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	due := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	retry := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	later := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	removed := anExtra(t, st, movieA, "interview", "/x/4.mkv")
	orphan := anExtra(t, st, movieA, "short", "/x/5.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET failures = 1, nextretryat = now() - interval '1 hour' WHERE id = $1`, retry.ID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET failures = 1, nextretryat = now() + interval '1 hour' WHERE id = $1`, later.ID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET item_id = 'gone' WHERE id = $1`, orphan.ID)
	if _, err := st.RemoveExtra(ctx, removed.ID, "admin-1", "", -time.Hour); err != nil {
		t.Fatal(err)
	}
	// the longest due first: the retry due an hour ago
	first, err := st.ClaimDueExtras(ctx, nil, 1)
	if err != nil || len(first) != 1 || first[0].ID != retry.ID {
		t.Fatalf("the first claimed: %v, %v; want the retry due longest", first, err)
	}
	claimed, err := st.ClaimDueExtras(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{fmt.Sprintf("%s %s %s %s %d", first[0].ID, first[0].ItemID, first[0].Kind, first[0].RegisteredBy, first[0].Failures)}
	for _, c := range claimed {
		got = append(got, fmt.Sprintf("%s %s %s %s %d", c.ID, c.ItemID, c.Kind, c.RegisteredBy, c.Failures))
	}
	want := []string{retry.ID + " " + movieA + " teaser api 1", due.ID + " " + movieA + " trailer api 0"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("claimed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, x := range []*model.Extra{due, retry} {
		if line := extraLine(t, st, x.ID); !strings.HasPrefix(line, "queued") || !strings.Contains(line, "retry=false sent=true") {
			t.Errorf("a claimed extra: %s", line)
		}
	}
	for _, x := range []*model.Extra{later, orphan} {
		if line := extraLine(t, st, x.ID); !strings.HasPrefix(line, "pending") {
			t.Errorf("an extra left alone: %s", line)
		}
	}
	if again, err := st.ClaimDueExtras(ctx, nil, 10); err != nil || len(again) != 0 {
		t.Errorf("a second claim: %v, %v", again, err)
	}

	// Put back: due's trigger could not be sent; retry's transcoder spoke.
	if _, err := st.ReportExtraStep(ctx, retry.ID, store.ExtraReport{Step: "transcode", Status: "in_progress"}, extraPolicy); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBackExtras(ctx, []string{due.ID, retry.ID}, "the trigger could not be sent: broker down", time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := extraLine(t, st, due.ID); got != "pending 0 0 error=the trigger could not be sent: broker down retry=true sent=false heard=false" {
		t.Errorf("put back: %s", got)
	}
	if got := extraLine(t, st, retry.ID); !strings.HasPrefix(got, "transcoding") {
		t.Errorf("an extra its worker reported on was put back: %s", got)
	}
	// Claimed by its id; one not due is not.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET nextretryat = NULL WHERE id = $1`, due.ID)
	if c, err := st.ClaimDueExtras(ctx, []string{due.ID, later.ID}, 10); err != nil || len(c) != 1 || c[0].ID != due.ID {
		t.Errorf("claimed by id: %v, %v", c, err)
	}
}

// The reaper takes an extra stuck in its packaging for a failed run: queued
// with no transcoder started within the wait, transcoded with no packager
// started within it, transcoding or packaging with its worker silent for
// longer than the silent timeout. With an attempt left, a transcoded one
// stays transcoded, noted as sent again, and any other is put back to
// pending, sent again a backoff later; without one it is failed. Those within
// their timeouts, removed, and done are left alone. A transcoded one whose
// trigger could not be sent is reaped again later, its failure uncounted.
func TestTheReaperTakesStuckExtras(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	x := map[string]*model.Extra{}
	for i, name := range []string{"queued", "waiting", "transcoded", "transcoding", "packaging", "running", "last", "removed", "ready"} {
		x[name] = anExtra(t, st, movieA, "trailer", fmt.Sprintf("/x/%d.mkv", i))
	}
	set := func(name, sql string) {
		storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET `+sql+` WHERE id = $1`, x[name].ID)
	}
	set("queued", `state = 'queued', dispatchedat = now() - interval '25 hours'`)
	set("waiting", `state = 'queued', dispatchedat = now() - interval '23 hours'`)
	set("transcoded", `state = 'transcoded', heartbeatat = now() - interval '25 hours'`)
	set("transcoding", `state = 'transcoding', heartbeatat = now() - interval '3 hours'`)
	set("packaging", `state = 'packaging', heartbeatat = now() - interval '121 minutes', failures = 1`)
	set("running", `state = 'transcoding', heartbeatat = now() - interval '1 hour'`)
	set("last", `state = 'transcoding', heartbeatat = now() - interval '3 hours', failures = 2`)
	set("removed", `state = 'transcoding', heartbeatat = now() - interval '3 hours', removedat = now()`)
	set("ready", `state = 'ready', heartbeatat = now() - interval '30 days', packagedat = now() - interval '30 days'`)
	reaped, err := st.ReapExtras(ctx, store.ExtraTimeouts{Wait: 24 * time.Hour, Silent: 2 * time.Hour, WaitLabel: "24h", SilentLabel: "2h"},
		extraPolicy, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range reaped {
		got[r.ID] = r.Was + " -> " + r.State
	}
	want := map[string]string{x["queued"].ID: "queued -> pending", x["transcoded"].ID: "transcoded -> transcoded",
		x["transcoding"].ID: "transcoding -> pending", x["packaging"].ID: "packaging -> pending", x["last"].ID: "transcoding -> failed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reaped %v, want %v", got, want)
	}
	for name, line := range map[string]string{
		"queued":      "pending 1 0 error=timed out: its trigger was sent and no transcoder started it within 24h retry=true sent=false heard=false",
		"transcoded":  "transcoded 1 0 error=timed out: no packager started on its transcode within 24h; the transcoder is asked to announce it again retry=false sent=true heard=true",
		"transcoding": "pending 1 0 error=timed out: no word from its transcoder for 2h retry=true sent=false heard=true",
		"packaging":   "pending 2 0 error=timed out: no word from its packager for 2h retry=true sent=false heard=true",
		"last":        "failed 3 0 error=timed out: no word from its transcoder for 2h retry=false sent=false heard=true",
		"waiting":     "queued 0 0 error=- retry=false sent=true heard=false",
		"running":     "transcoding 0 0 error=- retry=false sent=false heard=true",
		"removed":     "transcoding 0 0 error=- retry=false sent=false heard=true",
		"ready":       "ready 0 0 error=- retry=false sent=false heard=true",
	} {
		if got := extraLine(t, st, x[name].ID); got != line {
			t.Errorf("%s:\n got  %s\n want %s", name, got, line)
		}
	}
	if secs := retryInSeconds(t, st, x["packaging"].ID); secs < 110 || secs > 120 {
		t.Errorf("a second failure is retried in %.0fs, want two minutes", secs)
	}
	if again, err := st.ReapExtras(ctx, store.ExtraTimeouts{Wait: 24 * time.Hour, Silent: 2 * time.Hour}, extraPolicy, 100); err != nil || len(again) != 0 {
		t.Errorf("a second reap: %v, %v", again, err)
	}
	if err := st.UnreapExtras(ctx, []string{x["transcoded"].ID, x["queued"].ID}, "the trigger could not be sent: broker down"); err != nil {
		t.Fatal(err)
	}
	if got := extraLine(t, st, x["transcoded"].ID); got != "transcoded 0 0 error=the trigger could not be sent: broker down retry=false sent=false heard=true" {
		t.Errorf("unreaped: %s", got)
	}
	if got := extraLine(t, st, x["queued"].ID); !strings.HasPrefix(got, "pending 1 0") {
		t.Errorf("an extra put back to pending was unreaped: %s", got)
	}
}

// The packages of removed extras are claimed once their grace is over, once;
// one deleted says so (no package), one that could not be deleted is due
// again later.
func TestTheRemovedExtrasPackagesAreClaimedOnceDue(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	old := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	recent := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	live := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET packagepath = '/p/extras/' || left(id, 2) || '/' || id,
		packagedat = now(), nextretryat = now() - interval '1 minute'`)
	for id, grace := range map[string]time.Duration{old.ID: -time.Minute, recent.ID: 23 * time.Hour} {
		if _, err := st.RemoveExtra(ctx, id, "admin-1", "", grace); err != nil {
			t.Fatal(err)
		}
	}
	due, err := st.ClaimRemovedPackages(ctx, 10)
	if err != nil || len(due) != 1 || due[0].ID != old.ID || *due[0].PackagePath != "/p/extras/"+old.ID[:2]+"/"+old.ID {
		t.Fatalf("claimed %+v, %v; want the extra removed a day ago", due, err)
	}
	if again, err := st.ClaimRemovedPackages(ctx, 10); err != nil || len(again) != 0 {
		t.Errorf("claimed again: %v, %v", again, err)
	}
	if err := st.PutBackRemovedPackages(ctx, []string{old.ID}, "permission denied", time.Hour); err != nil {
		t.Fatal(err)
	}
	if secs := retryInSeconds(t, st, old.ID); secs < 3500 || secs > 3600 {
		t.Errorf("due again in %.0fs, want an hour", secs)
	}
	if err := st.RemovedPackagesDeleted(ctx, []string{old.ID, live.ID}); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE packagepath IS NULL`); n != 1 {
		t.Errorf("%d extras say they have no package, want the removed one deleted", n)
	}
}

// An extra packaged again waits to be sent afresh, its failures and error
// cleared and its file no longer missing, unless decide leaves it alone; its
// package plays meanwhile. A removed extra is not looked at.
func TestResetExtras(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	ready := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	busy := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	missing := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	removed := anExtra(t, st, movieA, "interview", "/x/4.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'ready', packagedat = now(), failures = 2,
		error = 'it failed once' WHERE id = $1`, ready.ID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'transcoding', heartbeatat = now() WHERE id = $1`, busy.ID)
	if _, err := st.MarkExtrasMissing(ctx, []string{missing.ID}, "katalog-manager/scanner"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RemoveExtra(ctx, removed.ID, "admin-1", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	after, whys, err := st.ResetExtras(ctx, []string{ready.ID, busy.ID, missing.ID, removed.ID}, "admin-2", func(x *model.Extra) string {
		seen[x.ID] = true
		if x.State == "transcoding" {
			return "it is transcoding"
		}
		return ""
	})
	if err != nil || len(after) != 3 || len(whys) != 1 || whys[busy.ID] != "it is transcoding" || seen[removed.ID] {
		t.Fatalf("ResetExtras: %d extras, whys %v, %v; seen %v", len(after), whys, err, seen)
	}
	if got := extraLine(t, st, ready.ID); got != "pending 0 0 error=- retry=false sent=false heard=false" {
		t.Errorf("packaged again: %s", got)
	}
	if x, _ := st.GetExtra(ctx, ready.ID); !x.Playable() || *x.ModifiedBy != "admin-2" {
		t.Errorf("the extra packaged again does not play meanwhile: %+v", x)
	}
	if x, _ := st.GetExtra(ctx, missing.ID); x.State != "pending" || x.Hidden {
		t.Errorf("a missing extra packaged again: %s hidden %v", x.State, x.Hidden)
	}
	if got := extraLine(t, st, busy.ID); !strings.HasPrefix(got, "transcoding") {
		t.Errorf("an extra left alone changed: %s", got)
	}
}

// The scanner finds an extra's file again: after it went missing, the extra
// is shown again, ready when its package was made of the same file, else
// waiting to be packaged; a file that changed is packaged again, unless its
// packaging runs; one unchanged changes nothing. A file found gone marks the
// scanner's extras missing and hidden. A file at a new path is the extra of
// its item with its size and quick hash.
func TestTheScannerKeepsTrackOfAnExtrasFile(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	storetest.AddItem(t, st, movieA, "movie", "Movie A", "")
	same := anExtra(t, st, movieA, "trailer", "/x/1.mkv")
	changed := anExtra(t, st, movieA, "teaser", "/x/2.mkv")
	running := anExtra(t, st, movieA, "featurette", "/x/3.mkv")
	unpackaged := anExtra(t, st, movieA, "interview", "/x/4.mkv")
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET registeredby = 'scanner'`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'ready', packagedat = now() WHERE id IN ($1, $2)`, same.ID, changed.ID)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'transcoding' WHERE id = $1`, running.ID)
	if xs, err := st.ScannerExtras(ctx); err != nil || len(xs) != 4 {
		t.Fatalf("ScannerExtras: %d, %v", len(xs), err)
	}
	if n, err := st.MarkExtrasMissing(ctx, []string{same.ID, unpackaged.ID}, "katalog-manager/scanner"); err != nil || n != 2 {
		t.Fatalf("MarkExtrasMissing: %d, %v", n, err)
	}
	if x, _ := st.GetExtra(ctx, same.ID); x.State != "missing" || !x.Hidden || x.Playable() {
		t.Errorf("a missing extra: %s hidden %v", x.State, x.Hidden)
	}
	if xs, _ := st.ScannerExtras(ctx); len(xs) != 2 {
		t.Errorf("ScannerExtras lists %d, want the two not missing", len(xs))
	}
	update := func(x *model.Extra, path string, size int64, qh1 string) *model.Extra {
		t.Helper()
		got, err := st.UpdateExtraSource(ctx, x.ID, store.ExtraSource{Path: path, Size: size, QH1: qh1}, "katalog-manager/scanner")
		if err != nil || got == nil {
			t.Fatalf("UpdateExtraSource %s: %v, %v", x.ID, got, err)
		}
		return got
	}
	if x := update(same, "/x/moved/1.mkv", 1000, qh1Of("a")); x.State != "ready" || x.Hidden || !x.Playable() || *x.SourcePath != "/x/moved/1.mkv" {
		t.Errorf("the same file found again: %s hidden %v at %s", x.State, x.Hidden, *x.SourcePath)
	}
	if x := update(unpackaged, "/x/4.mkv", 1000, qh1Of("a")); x.State != "pending" || x.Hidden {
		t.Errorf("an unpackaged file found again: %s hidden %v", x.State, x.Hidden)
	}
	if x := update(changed, "/x/2.mkv", 2000, qh1Of("c")); x.State != "pending" || *x.SourceSize != 2000 || !x.Playable() {
		t.Errorf("a changed file: %s, %d", x.State, *x.SourceSize)
	}
	if x := update(running, "/x/3.mkv", 3000, qh1Of("d")); x.State != "transcoding" || *x.SourceQH1 != qh1Of("d") {
		t.Errorf("a changed file being encoded: %s", x.State)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET modifiedby = 'before' WHERE id = $1`, same.ID)
	if x := update(same, "/x/moved/1.mkv", 1000, qh1Of("a")); x.State != "ready" || *x.ModifiedBy != "before" {
		t.Errorf("an unchanged file: %s, modified by %s", x.State, *x.ModifiedBy)
	}
	if _, err := st.RemoveExtra(ctx, same.ID, "admin-1", "", time.Hour); err != nil {
		t.Fatal(err)
	}
	if x, err := st.UpdateExtraSource(ctx, same.ID, store.ExtraSource{Path: "/x/1.mkv", Size: 1, QH1: qh1Of("e")}, "s"); err != nil || x != nil {
		t.Errorf("a removed extra's file: %v, %v", x, err)
	}
	movable, err := st.MovableExtras(ctx, movieA, 1000, qh1Of("a"))
	if err != nil || len(movable) != 1 || movable[0].ID != unpackaged.ID {
		t.Errorf("MovableExtras: %d, %v; want the one not removed", len(movable), err)
	}
	if xs, err := st.ExtrasOfItems(ctx, []string{movieA, movieB}); err != nil || len(xs) != 4 {
		t.Errorf("ExtrasOfItems: %d, %v", len(xs), err)
	}
}
