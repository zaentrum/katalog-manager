package graph

import (
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A title's library reads what the library holds of it for the console:
// when it was recorded, whether its originals are held, its folder, its
// versions (the newest first), its originals with what their deletion lost,
// and the events its folder records. A catalog without migration 040 reads
// none.
func TestTheLibraryOfATitle(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	const film = "a1a1a1a1-0000-4000-8000-000000000001"
	storetest.AddItem(t, st, film, "movie", "Example Film", "")
	cfg := testConfig
	cfg.LibraryRoot = t.TempDir()
	dir := library.PathsOf(cfg).MovieDir(film)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_items SET recordedat = '2026-10-06 10:00:00+00' WHERE id = $1`, film)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat,
			verifiedat, verifiedlevel, supersededby, supersededat, createdat) VALUES
		('v1', $1, ARRAY['s1']::varchar[], 'superseded', 'p1', $2 || '/versions/v1', '2026-10-01 10:00:00+00', NULL, NULL, 'v2',
		 '2026-10-05 10:00:00+00', '2026-10-01 09:00:00+00'),
		('v2', $1, ARRAY['s1']::varchar[], 'complete', 'p2', $2 || '/versions/v2', '2026-10-05 10:00:00+00', '2026-10-06 09:00:00+00',
		 'full', NULL, NULL, '2026-10-05 09:00:00+00')`, film, dir)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, sizebytes, state, retireeventid, deletedat,
			deletedby, lost) VALUES ('s1', $1, 'Film.mkv', 1234, 'deleted', 'e0e0e0e0-0000-4000-8000-000000000001',
			'2026-10-06 11:00:00+00', 'katalog-manager (library.originals=delete-after-package)', '["surround"]')`, film)
	reason := "originals are not kept: the package is the record"
	for _, ev := range []library.Event{
		{ID: "e1e1e1e1-0000-4000-8000-000000000002", At: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), Kind: library.EventPackageSuperseded,
			VersionID: "v1", PackageID: "p1", Successor: &library.Successor{VersionID: "v2", PackageID: "p2"}},
		{ID: "e0e0e0e0-0000-4000-8000-000000000001", At: time.Date(2026, 10, 6, 11, 0, 0, 0, time.UTC), Kind: library.EventOriginalDeleted,
			By: library.RetiredBy, VersionID: "v2", SourceID: "s1", Reason: &reason, Accepted: []string{"surround"}},
	} {
		if _, err := library.WriteEvent(dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	schema := MustSchema(NewResolver(st, cfg, Services{}))
	q := `{ item(id: "` + film + `") { library { recorded hold dir versions { id state packageId sourceIds verifiedLevel supersededBy }
		sources { id state sizeBytes lost retireEventId deletedBy } events { id kind at by versionId sourceId packageId supersededBy
		reason accepted } } } }`
	resp := schema.Exec(as(admin), q, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	want := `{"item":{"library":{"recorded":"2026-10-06T10:00:00Z","hold":false,"dir":"` + dir + `",` +
		`"versions":[{"id":"v2","state":"complete","packageId":"p2","sourceIds":["s1"],"verifiedLevel":"full","supersededBy":null},` +
		`{"id":"v1","state":"superseded","packageId":"p1","sourceIds":["s1"],"verifiedLevel":null,"supersededBy":"v2"}],` +
		`"sources":[{"id":"s1","state":"deleted","sizeBytes":1234,"lost":["surround"],"retireEventId":"e0e0e0e0-0000-4000-8000-000000000001",` +
		`"deletedBy":"katalog-manager (library.originals=delete-after-package)"}],` +
		`"events":[{"id":"e1e1e1e1-0000-4000-8000-000000000002","kind":"package-superseded","at":"2026-10-05T10:00:00Z","by":null,` +
		`"versionId":"v1","sourceId":null,"packageId":"p1","supersededBy":"v2","reason":null,"accepted":null},` +
		`{"id":"e0e0e0e0-0000-4000-8000-000000000001","kind":"original-deleted","at":"2026-10-06T11:00:00Z",` +
		`"by":"katalog-manager (library.originals=delete-after-package)","versionId":"v2","sourceId":"s1","packageId":null,` +
		`"supersededBy":null,"reason":"originals are not kept: the package is the record","accepted":["surround"]}]}}}`
	if got := string(resp.Data); got != want {
		t.Errorf("the library:\n got  %s\n want %s", got, want)
	}

	base := storetest.OpenBase(t)
	withItemView(t, base)
	storetest.AddItem(t, base, film, "movie", "Example Film", "")
	resp = MustSchema(NewResolver(base, cfg, Services{})).Exec(as(admin), `{ item(id: "`+film+`") { library { hold } } }`, "", nil)
	if len(resp.Errors) > 0 || string(resp.Data) != `{"item":{"library":null}}` {
		t.Errorf("without 040: %s %v", resp.Data, resp.Errors)
	}
	resp = MustSchema(NewResolver(base, cfg, Services{})).Exec(as(admin), `mutation { holdOriginal(id: "`+film+`", hold: true) { hold } }`,
		"", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "UNAVAILABLE" {
		t.Errorf("holdOriginal without 040: %v", resp.Errors)
	}
}

// holdOriginal holds a title's originals, and lets them go; a series' hold
// is its episodes' too, under it or under a season of it. An item there is
// not is not found.
func TestHoldOriginal(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	storetest.AddItem(t, st, "e1", "episode", "Pilot", "s1")
	storetest.AddItem(t, st, "sn", "season", "Season 2", "s1")
	storetest.AddItem(t, st, "e2", "episode", "Second", "sn")
	storetest.AddItem(t, st, "m1", "movie", "Another", "")
	held := func() string {
		t.Helper()
		var out string
		if err := st.Pool().QueryRow(as(admin), `SELECT string_agg(id, ' ' ORDER BY id) FROM com_nalet_katalog_items WHERE retirehold`).
			Scan(&out); err != nil {
			return ""
		}
		return out
	}
	if got := query(t, st, `mutation { holdOriginal(id: "s1", hold: true) { hold recorded } }`); got !=
		`{"holdOriginal":{"hold":true,"recorded":null}}` {
		t.Errorf("hold the series: %s", got)
	}
	if got := held(); got != "e1 e2 s1" {
		t.Errorf("held: %q, want the series and its episodes", got)
	}
	if got := query(t, st, `mutation { holdOriginal(id: "e2", hold: false) { hold } }`); got != `{"holdOriginal":{"hold":false}}` {
		t.Errorf("let an episode go: %s", got)
	}
	if got := held(); got != "e1 s1" {
		t.Errorf("held: %q", got)
	}
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin), `mutation { holdOriginal(id: "nobody", hold: true) { hold } }`,
		"", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "NOT_FOUND" || !strings.Contains(resp.Errors[0].Message, "unknown item") {
		t.Errorf("an unknown item: %v", resp.Errors)
	}
}

// An admin lists the superseded originals, those waiting for one first, each
// with why it waits and what deleting it gives up, and decides one: accepted,
// the retire job has it go; kept, never. A source that is no superseded
// original is NOT_FOUND; a viewer may do neither. A title's library says the
// same of its sources.
func TestAnAdminDecidesASupersededOriginal(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	const film = "a2a2a2a2-0000-4000-8000-000000000002"
	storetest.AddItem(t, st, film, "movie", "Example Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state, supersededby,
			supersededat, supersededhold, supersededholdfor, supersededlosses) VALUES
		('s-held', $1, 'Film.mkv', '/lib/.work/incoming/Film.mkv', 1000, 'superseded', 's-new', '2026-11-04 10:00:00+00',
		 'kept for an admin: deleting it gives up, against version v2: dolbyVision', 'v2', '["dolbyVision"]'),
		('s-free', $1, 'Film 2.mkv', '/lib/.work/incoming/Film 2.mkv', 900, 'superseded', 's-new', '2026-11-03 10:00:00+00', NULL, NULL, NULL),
		('s-new', $1, 'Film 4K.mkv', '/lib/.work/incoming/Film 4K.mkv', 5000, 'present', NULL, NULL, NULL, NULL, NULL)`, film)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	const fields = `itemId title source { id state supersededBy supersededAt hold holdFor holdLosses decision acceptedLosses decidedBy }`
	resp := schema.Exec(as(admin), `{ supersededOriginals { `+fields+` } }`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	want := `{"supersededOriginals":[{"itemId":"` + film + `","title":"Example Film","source":{"id":"s-held","state":"superseded",` +
		`"supersededBy":"s-new","supersededAt":"2026-11-04T10:00:00Z","hold":"kept for an admin: deleting it gives up, against version v2: ` +
		`dolbyVision","holdFor":"v2","holdLosses":["dolbyVision"],"decision":null,"acceptedLosses":null,"decidedBy":null}},` +
		`{"itemId":"` + film + `","title":"Example Film","source":{"id":"s-free","state":"superseded","supersededBy":"s-new",` +
		`"supersededAt":"2026-11-03T10:00:00Z","hold":null,"holdFor":null,"holdLosses":null,"decision":null,"acceptedLosses":null,` +
		`"decidedBy":null}}]}`
	if string(resp.Data) != want {
		t.Errorf("the list:\n%s\nwant\n%s", resp.Data, want)
	}
	if got := query(t, st, `{ supersededOriginals(held: true) { source { id } } }`); got != `{"supersededOriginals":[{"source":{"id":"s-held"}}]}` {
		t.Errorf("those waiting for an admin: %s", got)
	}
	resp = schema.Exec(as(admin), `mutation { decideSupersededOriginal(sourceId: "s-held", decision: ACCEPT) { source { id decision
		acceptedLosses decidedBy } } }`, "", nil)
	if len(resp.Errors) > 0 || string(resp.Data) != `{"decideSupersededOriginal":{"source":{"id":"s-held","decision":"accept",`+
		`"acceptedLosses":["dolbyVision"],"decidedBy":"admin-1"}}}` {
		t.Errorf("accept: %s %v", resp.Data, resp.Errors)
	}
	resp = schema.Exec(as(admin), `mutation { decideSupersededOriginal(sourceId: "s-free", decision: KEEP) { source { decision } } }`, "", nil)
	if len(resp.Errors) > 0 || string(resp.Data) != `{"decideSupersededOriginal":{"source":{"decision":"keep"}}}` {
		t.Errorf("keep: %s %v", resp.Data, resp.Errors)
	}
	resp = schema.Exec(as(admin), `mutation { decideSupersededOriginal(sourceId: "s-new", decision: KEEP) { itemId } }`, "", nil)
	if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "NOT_FOUND" {
		t.Errorf("a present source: %v", resp.Errors)
	}
	for _, q := range []string{`{ supersededOriginals { itemId } }`, `mutation { decideSupersededOriginal(sourceId: "s-held", decision: KEEP) { itemId } }`} {
		if resp := schema.Exec(as(viewer), q, "", nil); len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "FORBIDDEN" {
			t.Errorf("a viewer: %s: %v", q, resp.Errors)
		}
	}
	got := query(t, st, `{ item(id: "`+film+`") { library { sources { id decision holdLosses supersededBy } } } }`)
	if !strings.Contains(got, `{"id":"s-held","decision":"accept","holdLosses":["dolbyVision"],"supersededBy":"s-new"}`) ||
		!strings.Contains(got, `{"id":"s-new","decision":null,"holdLosses":null,"supersededBy":null}`) {
		t.Errorf("the title's sources: %s", got)
	}
}
