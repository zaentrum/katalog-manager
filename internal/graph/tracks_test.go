package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

const trackFields = `tracks { kind ordinal sourceLanguage languageOverride effectiveLanguage title format forced reported }`

// filmWithTracks is a film whose package reported two audio tracks and a
// subtitle, an admin having set the language of the first audio track and of
// a subtitle no package reported; and a series, which has no source file.
func filmWithTracks(t *testing.T) *store.Store {
	t.Helper()
	st := storetest.Open(t)
	withItemView(t, st)
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('src-m1', 'm1', '/media/a-film.mkv', true)`)
	storetest.AddItem(t, st, "s1", "series", "A Series", "")
	und, eng, ger, webvtt, commentary := "und", "eng", "ger", "webvtt", "Commentary"
	if _, err := st.RecordSourceTracks(context.Background(), "m1", map[string][]model.SourceTrack{
		"audio":    {{Kind: "audio", Ordinal: 0, Language: &und}, {Kind: "audio", Ordinal: 1, Language: &eng, Title: &commentary}},
		"subtitle": {{Kind: "subtitle", Ordinal: 0, Language: &ger, Format: &webvtt, Forced: true}},
	}); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language)
		VALUES ('m1', 'audio', 0, 'zxx'), ('m1', 'subtitle', 3, 'fre')`)
	return st
}

// An item's tracks are its source's, audio first, each kind by ordinal, each
// with the language its source tags it with, the one an admin set and the
// one it plays as: the admin's, else the source's, else und. A track only an
// admin's language names comes, not reported; a title with no tracks has
// none, and so has every title of a catalog without migration 037.
func TestItemTracks(t *testing.T) {
	st := filmWithTracks(t)
	want := `{"item":{"tracks":[` +
		`{"kind":"audio","ordinal":0,"sourceLanguage":"und","languageOverride":"zxx","effectiveLanguage":"zxx","title":null,"format":null,"forced":false,"reported":true},` +
		`{"kind":"audio","ordinal":1,"sourceLanguage":"eng","languageOverride":null,"effectiveLanguage":"eng","title":"Commentary","format":null,"forced":false,"reported":true},` +
		`{"kind":"subtitle","ordinal":0,"sourceLanguage":"ger","languageOverride":null,"effectiveLanguage":"ger","title":null,"format":"webvtt","forced":true,"reported":true},` +
		`{"kind":"subtitle","ordinal":3,"sourceLanguage":null,"languageOverride":"fre","effectiveLanguage":"fre","title":null,"format":null,"forced":false,"reported":false}]}}`
	if got := query(t, st, `{ item(id: "m1") { `+trackFields+` } }`); got != want {
		t.Errorf("tracks:\n got  %s\n want %s", got, want)
	}
	if got := query(t, st, `{ item(id: "s1") { tracks { kind } } }`); got != `{"item":{"tracks":[]}}` {
		t.Errorf("a series: %s", got)
	}

	// A track the source tags with no language plays as und.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemtracks SET language = NULL WHERE item_id = 'm1' AND kind = 'audio' AND ordinal = 1`)
	if got := query(t, st, `{ item(id: "m1") { tracks { ordinal effectiveLanguage } } }`); !strings.Contains(got, `{"ordinal":1,"effectiveLanguage":"und"}`) {
		t.Errorf("a track without a tag: %s", got)
	}

	base := storetest.OpenBase(t)
	withItemView(t, base)
	storetest.AddItem(t, base, "m1", "movie", "A Film", "")
	if got := query(t, base, `{ item(id: "m1") { tracks { kind } } }`); got != `{"item":{"tracks":[]}}` {
		t.Errorf("without 037: %s", got)
	}
}

// An admin sets the language of a track, and clears it: the answer is the
// title with its tracks as they now are. A kind, an ordinal or a code that is
// none, a title without a source file and an ordinal its package did not
// report are refused, saying why, and change nothing; a title there is not is
// null.
func TestSetTrackLanguageMutation(t *testing.T) {
	st := filmWithTracks(t)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	exec := func(q string) (string, string) {
		t.Helper()
		resp := schema.Exec(as(admin), q, "", nil)
		errs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			errs = append(errs, e.Message)
		}
		return string(resp.Data), strings.Join(errs, "; ")
	}
	languages := func() string {
		t.Helper()
		ls, err := st.TrackLanguages(context.Background(), "m1")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range ls {
			out = append(out, l.Kind+"/"+itoa32(l.Ordinal)+"="+l.Language)
		}
		return strings.Join(out, " ")
	}

	got, errs := exec(`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 1, language: "zxx") {
		id tracks { kind ordinal sourceLanguage languageOverride effectiveLanguage } } }`)
	if want := `{"setTrackLanguage":{"id":"m1","tracks":[` +
		`{"kind":"audio","ordinal":0,"sourceLanguage":"und","languageOverride":"zxx","effectiveLanguage":"zxx"},` +
		`{"kind":"audio","ordinal":1,"sourceLanguage":"eng","languageOverride":"zxx","effectiveLanguage":"zxx"},` +
		`{"kind":"subtitle","ordinal":0,"sourceLanguage":"ger","languageOverride":null,"effectiveLanguage":"ger"},` +
		`{"kind":"subtitle","ordinal":3,"sourceLanguage":null,"languageOverride":"fre","effectiveLanguage":"fre"}]}}`; got != want || errs != "" {
		t.Errorf("setting audio 1 to zxx:\n got  %s %s\n want %s", got, errs, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = 'm1'
		AND modifiedby = 'admin-1' AND modifiedat > now() - interval '1 minute'`); n != 1 {
		t.Error("setting a track's language does not modify the title as the admin")
	}
	if got, errs := exec(`mutation { setTrackLanguage(itemId: "m1", kind: "subtitle", ordinal: 0, language: "eng") {
		tracks { kind ordinal effectiveLanguage } } }`); !strings.Contains(got, `{"kind":"subtitle","ordinal":0,"effectiveLanguage":"eng"}`) || errs != "" {
		t.Errorf("setting subtitle 0 to eng: %s %s", got, errs)
	}
	// Cleared, without a language or with null: the track plays as its
	// source tags it.
	for _, q := range []string{
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0) { tracks { ordinal effectiveLanguage } } }`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "subtitle", ordinal: 3, language: null) { id } }`,
	} {
		if _, errs := exec(q); errs != "" {
			t.Errorf("%s: %s", q, errs)
		}
	}
	if got, want := languages(), "audio/1=zxx subtitle/0=eng"; got != want {
		t.Errorf("languages %q, want %q", got, want)
	}

	for q, want := range map[string]string{
		`mutation { setTrackLanguage(itemId: "m1", kind: "video", ordinal: 0, language: "eng") { id } }`:     `a track is of kind audio or subtitle, not "video"`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: -1, language: "eng") { id } }`:    "0 or more, not -1",
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0, language: "EN") { id } }`:      `an ISO 639-2 code, three lowercase letters (zxx: no dialogue, und: unknown), not "EN"`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0, language: "en") { id } }`:      `not "en"`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0, language: "English") { id } }`: `not "English"`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0, language: "") { id } }`:        `not ""`,
		`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 2, language: "eng") { id } }`:     "has no audio track 2: its package reported 2, ordinals 0 to 1",
		`mutation { setTrackLanguage(itemId: "m1", kind: "subtitle", ordinal: 1, language: "eng") { id } }`:  "has no subtitle track 1: its package reported 1, ordinals 0 to 0",
		`mutation { setTrackLanguage(itemId: "s1", kind: "audio", ordinal: 0, language: "eng") { id } }`:     "item s1 has no source file",
	} {
		if got, errs := exec(q); got != `{"setTrackLanguage":null}` || !strings.Contains(errs, want) {
			t.Errorf("%s:\n got  %s %s\n want it refused, saying %q", q, got, errs, want)
		}
	}
	if got, errs := exec(`mutation { setTrackLanguage(itemId: "nothing", kind: "audio", ordinal: 0, language: "eng") { id } }`); got != `{"setTrackLanguage":null}` || errs != "" {
		t.Errorf("a title there is not: %s %s", got, errs)
	}
	if got, want := languages(), "audio/1=zxx subtitle/0=eng"; got != want {
		t.Errorf("after the refusals: %q, want %q", got, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages WHERE item_id <> 'm1'`); n != 0 {
		t.Errorf("%d languages kept for titles refused or not there", n)
	}
}

// setTrackLanguage is an admin's: a viewer, an addon, the platform's service
// account and a context without a caller are refused with FORBIDDEN, naming
// the admin role, and the track keeps its language.
func TestSetTrackLanguageIsAnAdmins(t *testing.T) {
	st := filmWithTracks(t)
	schema := MustSchema(NewResolver(st, testConfig, Services{}))
	for who, ctx := range map[string]context.Context{
		"a viewer": as(viewer), "an addon": as(addon), "the service account": as(service), "no caller": context.Background(),
	} {
		for _, q := range []string{
			`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0, language: "eng") { id } }`,
			`mutation { setTrackLanguage(itemId: "m1", kind: "audio", ordinal: 0) { id } }`,
		} {
			resp := schema.Exec(ctx, q, "", nil)
			if len(resp.Errors) != 1 || resp.Errors[0].Extensions["code"] != "FORBIDDEN" ||
				resp.Errors[0].Message != "forbidden: setTrackLanguage requires the zaentrum-admin role" {
				t.Errorf("%s, %s: %v, want FORBIDDEN naming the admin role", who, q, resp.Errors)
			}
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages
		WHERE item_id = 'm1' AND kind = 'audio' AND ordinal = 0 AND language = 'zxx'`); n != 1 {
		t.Error("a refused caller changed a track's language")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE modifiedby IS NOT NULL`); n != 0 {
		t.Error("a refused caller modified a title")
	}
}

// backfillSourceTracks records the tracks of the packaged titles from their
// packages' manifests on disk, says how many, and which it could not read;
// the titles then list their tracks. Without migration 037 it is refused.
func TestBackfillSourceTracksMutation(t *testing.T) {
	st := storetest.Open(t)
	withItemView(t, st)
	dir := t.TempDir()
	storetest.AddItem(t, st, "m1", "movie", "A Film", "")
	storetest.AddItem(t, st, "m2", "movie", "Gone", "")
	manifest := filepath.Join(dir, "m1", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"version": 2, "renditions": {"audio": [{"id": "a0", "idx": 0, "language": "und"}]},
		"subtitles": [{"id": "sub0", "language": "ger", "format": "webvtt"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, kind) VALUES
		('pkg-m1', 'm1', $1, false, 'packaged'), ('pkg-m2', 'm2', $2, false, 'packaged')`,
		manifest, filepath.Join(dir, "m2", "manifest.json"))

	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin),
		`mutation { backfillSourceTracks { titles recorded audioTracks subtitleTracks failed errors } }`, "", nil)
	if len(resp.Errors) > 0 {
		t.Fatal(resp.Errors)
	}
	var got struct {
		B struct {
			Titles, Recorded, AudioTracks, SubtitleTracks, Failed int
			Errors                                                []string
		} `json:"backfillSourceTracks"`
	}
	if err := json.Unmarshal(resp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if b := got.B; b.Titles != 2 || b.Recorded != 1 || b.AudioTracks != 1 || b.SubtitleTracks != 1 || b.Failed != 1 ||
		len(b.Errors) != 1 || !strings.HasPrefix(b.Errors[0], "m2: read its manifest: ") {
		t.Errorf("backfillSourceTracks: %s", resp.Data)
	}
	if got := query(t, st, `{ item(id: "m1") { tracks { kind ordinal sourceLanguage effectiveLanguage } } }`); got !=
		`{"item":{"tracks":[{"kind":"audio","ordinal":0,"sourceLanguage":"und","effectiveLanguage":"und"},`+
			`{"kind":"subtitle","ordinal":0,"sourceLanguage":"ger","effectiveLanguage":"ger"}]}}` {
		t.Errorf("the tracks after the backfill: %s", got)
	}

	base := storetest.OpenBase(t)
	resp = MustSchema(NewResolver(base, testConfig, Services{})).Exec(as(admin), `mutation { backfillSourceTracks { titles } }`, "", nil)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0].Message, "037_track_languages.sql") {
		t.Errorf("without 037: %v", resp.Errors)
	}
}
