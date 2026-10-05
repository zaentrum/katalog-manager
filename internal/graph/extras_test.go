package graph

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const extraFields = `extras { id itemId kind title label language seasonNumber sourcePath sourceSize sourceQh1 recordPath
	registeredBy sortOrder hidden state error attempts failures nextRetryAt dispatchedAt heartbeatAt packagePath
	packagedAt durationMs videoCodec width height peakBandwidthBps packageSizeBytes playable removedAt removedBy
	removalReason createdAt createdBy modifiedAt modifiedBy }`

// An item's extras come as a viewer sees them listed, by their order (none
// last), then as they were taken in, with everything the catalog keeps of
// each, times in UTC; the removed ones only when asked, in their place, and
// a title without extras has none.
func TestItemExtras(t *testing.T) {
	st := storetest.OpenInTimeZone(t, "Pacific/Kiritimati")
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	qh1 := "sha256:" + "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"[:64]
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, label, language, seasonnumber,
		sourcepath, sourcesize, sourceqh1, registeredby, sortorder, state, attempts, packagepath, packagedat, durationms,
		videocodec, width, height, peakbandwidthbps, packagesizebytes, heartbeatat, createdat, createdby, modifiedat, modifiedby) VALUES
		('x-ready', 'm1', 'trailer', 'Trailer', 'Official trailer', 'en', NULL, '/extras/a/trailer.mov', 70000, $1, 'api', NULL,
		 'ready', 1, '/p/extras/x-/x-ready', '2026-10-05 08:00:00+00', 33000, 'avc1.64001f', 1280, 720, 2400000, 5000000,
		 '2026-10-05 08:00:00+00', '2026-10-05 07:00:00+00', 'admin-1', '2026-10-05 08:00:00+00', 'katalog-manager'),
		('x-first', 'm1', 'featurette', 'Making the film', NULL, NULL, NULL, '/extras/a/f.mkv', 1, NULL, 'scanner', 1,
		 'failed', 3, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, '2026-10-05 09:00:00+00', NULL,
		 '2026-10-05 09:00:00+00', NULL)`, qh1)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET error = 'ffmpeg exited 1', failures = 3 WHERE id = 'x-first'`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, removedat,
		removedby, removalreason, nextretryat, createdat, modifiedat) VALUES ('x-gone', 'm1', 'teaser', 'Teaser', 'api',
		'2026-10-05 10:00:00+00', 'admin-2', 'a duplicate', '2026-10-06 10:00:00+00', '2026-10-05 06:00:00+00', '2026-10-05 10:00:00+00')`)

	want := `{"item":{"extras":[` +
		`{"id":"x-first","itemId":"m1","kind":"featurette","title":"Making the film","label":null,"language":null,"seasonNumber":null,` +
		`"sourcePath":"/extras/a/f.mkv","sourceSize":1,"sourceQh1":null,"recordPath":null,"registeredBy":"scanner","sortOrder":1,` +
		`"hidden":false,"state":"failed","error":"ffmpeg exited 1","attempts":3,"failures":3,"nextRetryAt":null,"dispatchedAt":null,` +
		`"heartbeatAt":null,"packagePath":null,"packagedAt":null,"durationMs":null,"videoCodec":null,"width":null,"height":null,` +
		`"peakBandwidthBps":null,"packageSizeBytes":null,"playable":false,"removedAt":null,"removedBy":null,"removalReason":null,` +
		`"createdAt":"2026-10-05T09:00:00Z","createdBy":null,"modifiedAt":"2026-10-05T09:00:00Z","modifiedBy":null},` +
		`{"id":"x-ready","itemId":"m1","kind":"trailer","title":"Trailer","label":"Official trailer","language":"en","seasonNumber":null,` +
		`"sourcePath":"/extras/a/trailer.mov","sourceSize":70000,"sourceQh1":"` + qh1 + `","recordPath":null,"registeredBy":"api",` +
		`"sortOrder":null,"hidden":false,"state":"ready","error":null,"attempts":1,"failures":0,"nextRetryAt":null,"dispatchedAt":null,` +
		`"heartbeatAt":"2026-10-05T08:00:00Z","packagePath":"/p/extras/x-/x-ready","packagedAt":"2026-10-05T08:00:00Z",` +
		`"durationMs":33000,"videoCodec":"avc1.64001f","width":1280,"height":720,"peakBandwidthBps":2400000,` +
		`"packageSizeBytes":5000000,"playable":true,"removedAt":null,"removedBy":null,"removalReason":null,` +
		`"createdAt":"2026-10-05T07:00:00Z","createdBy":"admin-1","modifiedAt":"2026-10-05T08:00:00Z","modifiedBy":"katalog-manager"}]}}`
	if got := query(t, st, `{ item(id: "m1") { `+extraFields+` } }`); got != want {
		t.Errorf("extras:\n got  %s\n want %s", got, want)
	}
	if got, want := query(t, st, `{ item(id: "m1") { extras(removed: true) { id removedAt removedBy removalReason nextRetryAt playable } } }`),
		`{"item":{"extras":[{"id":"x-first","removedAt":null,"removedBy":null,"removalReason":null,"nextRetryAt":null,"playable":false},`+
			`{"id":"x-gone","removedAt":"2026-10-05T10:00:00Z","removedBy":"admin-2","removalReason":"a duplicate",`+
			`"nextRetryAt":"2026-10-06T10:00:00Z","playable":false},`+
			`{"id":"x-ready","removedAt":null,"removedBy":null,"removalReason":null,"nextRetryAt":null,"playable":true}]}}`; got != want {
		t.Errorf("with the removed:\n got  %s\n want %s", got, want)
	}
	if got := query(t, st, `{ item(id: "s1") { extras { id } } }`); got != `{"item":{"extras":[]}}` {
		t.Errorf("a title without extras: %s", got)
	}
}

// refusingExtras refuses every addExtra as a title there is not, and
// answers the rest as nothing done.
type refusingExtras struct{ calls []string }

func (f *refusingExtras) AddExtra(_ context.Context, in AddExtraRequest) (AddExtraResult, error) {
	f.calls = append(f.calls, "add "+in.ItemID)
	r := Refused(http.StatusConflict, "EXTRA_CONFLICT", "%s is extra x1 of item m2 already", in.Path)
	r.Extra = &model.Extra{ID: "x1", ItemID: "m2"}
	return AddExtraResult{}, r
}
func (f *refusingExtras) RemoveExtra(context.Context, string, string) (*model.Extra, error) {
	return nil, nil
}
func (f *refusingExtras) PackageExtra(context.Context, string) (ExtraPackagingResult, error) {
	return ExtraPackagingResult{}, Refused(http.StatusNotFound, "NOT_FOUND", "unknown extra: x9")
}
func (f *refusingExtras) PackageExtras(context.Context, string) (ExtraPackagingResult, error) {
	return ExtraPackagingResult{}, nil
}

// A refused extra's operation is a GraphQL error with its code, and the
// extra in the way when there is one; removing an extra there is not is
// null.
func TestARefusedExtraIsAnErrorWithItsCode(t *testing.T) {
	f := &refusingExtras{}
	schema := MustSchema(NewResolver(nil, testConfig, Services{Extras: f}))
	resp := schema.Exec(as(admin), `mutation { addExtra(itemId: "m1", path: "/extras/t.mov", kind: "trailer") { created } }`, "", nil)
	if len(resp.Errors) != 1 || len(f.calls) != 1 {
		t.Fatalf("errors %v, calls %v", resp.Errors, f.calls)
	}
	got, _ := json.Marshal(resp.Errors[0])
	if want := `{"message":"/extras/t.mov is extra x1 of item m2 already","path":["addExtra"],` +
		`"extensions":{"code":"EXTRA_CONFLICT","extraId":"x1","itemId":"m2"}}`; string(got) != want {
		t.Errorf("the error\n got  %s\n want %s", got, want)
	}
	resp = schema.Exec(as(admin), `mutation { packageExtra(id: "x9") { queued } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "NOT_FOUND" {
		t.Errorf("packageExtra of an extra there is not: %v", resp.Errors)
	}
	resp = schema.Exec(as(admin), `mutation { removeExtra(id: "x9") { id } }`, "", nil)
	if len(resp.Errors) != 0 || string(resp.Data) != `{"removeExtra":null}` {
		t.Errorf("removeExtra of an extra there is not: %s %v", resp.Data, resp.Errors)
	}
	resp = MustSchema(NewResolver(nil, testConfig, Services{})).Exec(as(admin),
		`mutation { addExtra(itemId: "m1", path: "/x.mov", kind: "trailer") { created } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Message != "feature not configured" {
		t.Errorf("addExtra without the extras wired: %v", resp.Errors)
	}
}
