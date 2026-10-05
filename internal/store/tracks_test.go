package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// 037 makes a title's source tracks and an admin's language of a track, each
// keyed by the item, the kind and the ordinal; running it again changes
// nothing, and the startup check applies it where any of it is missing, and
// only then.
func TestTrackLanguagesMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if ready, err := st.TrackLanguagesReady(ctx); err != nil || ready {
		t.Fatalf("TrackLanguagesReady on a catalog without 037: %v, %v", ready, err)
	}
	for run := 1; run <= 2; run++ {
		if err := st.EnsureTrackLanguages(ctx); err != nil {
			t.Fatalf("EnsureTrackLanguages, %d. time: %v", run, err)
		}
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language)
		VALUES ('m1', 'audio', 0, 'zxx')`)
	for run := 3; run <= 4; run++ {
		if _, err := st.Pool().Exec(ctx, migrations.TrackLanguages); err != nil {
			t.Fatalf("applying 037 for the %d. time: %v", run, err)
		}
	}
	if ready, err := st.TrackLanguagesReady(ctx); err != nil || !ready {
		t.Fatalf("TrackLanguagesReady after EnsureTrackLanguages: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages`); n != 1 {
		t.Errorf("applying 037 again left %d languages, want the one", n)
	}
	for table, want := range map[string]string{
		"com_nalet_katalog_itemtracks": `item_id character varying(36) NOT NULL
kind character varying(10) NOT NULL
ordinal integer NOT NULL
language character varying(35) NULL
title character varying(255) NULL
format character varying(20) NULL
forced boolean NOT NULL
updatedat timestamp with time zone NOT NULL`,
		"com_nalet_katalog_itemtracklanguages": `item_id character varying(36) NOT NULL
kind character varying(10) NOT NULL
ordinal integer NOT NULL
language character varying(3) NOT NULL
modifiedat timestamp with time zone NOT NULL
modifiedby character varying(255) NULL`,
	} {
		if got := columns(t, st, table); got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", table, got, want)
		}
	}
	// A track is audio or a subtitle, at an ordinal of 0 or more; an admin's
	// language three lowercase letters; one row per item, kind and ordinal.
	for _, bad := range []string{
		`INSERT INTO com_nalet_katalog_itemtracks (item_id, kind, ordinal) VALUES ('m1', 'video', 0)`,
		`INSERT INTO com_nalet_katalog_itemtracks (item_id, kind, ordinal) VALUES ('m1', 'audio', -1)`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ('m1', 'audio', 1, 'EN')`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ('m1', 'audio', 1, 'en')`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ('m1', 'subtitle', -1, 'eng')`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ('m1', 'audio', 0, 'eng')`,
	} {
		if _, err := st.Pool().Exec(ctx, bad); err == nil {
			t.Errorf("taken: %s", bad)
		}
	}

	// A table missing brings the migration back; the other keeps its rows.
	storetest.Exec(t, st, `DROP TABLE com_nalet_katalog_itemtracks`)
	if ready, err := st.TrackLanguagesReady(ctx); err != nil || ready {
		t.Fatalf("TrackLanguagesReady with a table of 037 missing: %v, %v", ready, err)
	}
	if err := st.EnsureTrackLanguages(ctx); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.TrackLanguagesReady(ctx); err != nil || !ready {
		t.Fatalf("TrackLanguagesReady after the startup check: %v, %v", ready, err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages WHERE language = 'zxx'`); n != 1 {
		t.Error("applying 037 again lost an admin's language")
	}
}

// storetest.Open gives a test every migration the service applies at startup,
// 037 among them.
func TestTheTestSchemaHasTheTrackLanguages(t *testing.T) {
	if ready, err := storetest.Open(t).TrackLanguagesReady(context.Background()); err != nil || !ready {
		t.Fatalf("TrackLanguagesReady on the test schema: %v, %v", ready, err)
	}
}

// filmWithASource is a film with a source file, and a series without one.
func filmWithASource(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.AddItem(t, st, movieA, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary)
		VALUES ('src-a', $1, '/media/a.mkv', true)`, movieA)
	storetest.AddItem(t, st, movieB, "series", "A Series", "")
}

// trackLines are the item's tracks, one per line: "kind ordinal source
// override reported title format forced", - for none.
func trackLines(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	tracks, err := st.Tracks(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	opt := func(s *string) string {
		if s == nil {
			return "-"
		}
		return *s
	}
	var out []string
	for _, tr := range tracks {
		out = append(out, fmt.Sprintf("%s %d %s %s %v %s %s %v", tr.Kind, tr.Ordinal, opt(tr.Language), opt(tr.Override),
			tr.Reported, opt(tr.Title), opt(tr.Format), tr.Forced))
	}
	return strings.Join(out, "\n")
}

// An admin sets the language of a track of a title's source: an ISO 639-2
// code, three lowercase letters (zxx and und too), at a kind and an ordinal;
// nil clears it. A change modifies the title as the admin; one that changes
// nothing does not. A title there is not is not found; one without a source
// file, a kind, an ordinal or a code that is none are refused, and nothing is
// kept.
func TestSetTrackLanguage(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	filmWithASource(t, st)
	set := func(kind string, ordinal int32, lang *string, by string) (bool, error) {
		return st.SetTrackLanguage(ctx, movieA, kind, ordinal, lang, by)
	}
	languages := func() string {
		ls, err := st.TrackLanguages(ctx, movieA)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range ls {
			out = append(out, fmt.Sprintf("%s/%d=%s", l.Kind, l.Ordinal, l.Language))
		}
		return strings.Join(out, " ")
	}

	for _, c := range []struct {
		kind    string
		ordinal int32
		lang    string
	}{{"audio", 0, "zxx"}, {"subtitle", 2, "ger"}, {"audio", 1, "und"}, {"subtitle", 0, "eng"}} {
		if found, err := set(c.kind, c.ordinal, str(c.lang), "admin-1"); err != nil || !found {
			t.Fatalf("set %s %d %s: %v %v", c.kind, c.ordinal, c.lang, found, err)
		}
	}
	if got, want := languages(), "audio/0=zxx audio/1=und subtitle/0=eng subtitle/2=ger"; got != want {
		t.Errorf("languages %q, want %q: audio first, each kind by ordinal", got, want)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1
		AND modifiedby = 'admin-1' AND modifiedat > now() - interval '1 minute'`, movieA); n != 1 {
		t.Error("setting a track's language does not modify the title as the admin")
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages WHERE item_id = $1
		AND kind = 'audio' AND ordinal = 0 AND modifiedby = 'admin-1' AND modifiedat > now() - interval '1 minute'`, movieA); n != 1 {
		t.Error("a track's language does not say who set it, and when")
	}

	// The same language again changes nothing, another replaces it.
	if _, err := set("audio", 0, str("zxx"), "admin-2"); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND modifiedby = 'admin-1'`, movieA); n != 1 {
		t.Error("setting the language a track has modified the title")
	}
	if _, err := set("audio", 0, str("eng"), "admin-2"); err != nil {
		t.Fatal(err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_items WHERE id = $1 AND modifiedby = 'admin-2'`, movieA); n != 1 {
		t.Error("changing a track's language does not modify the title as the admin")
	}

	// Cleared, a track plays as its source tags it; clearing one without a
	// language is no error.
	if found, err := set("audio", 1, nil, "admin-2"); err != nil || !found {
		t.Fatalf("clear audio 1: %v %v", found, err)
	}
	if found, err := set("audio", 7, nil, "admin-2"); err != nil || !found {
		t.Fatalf("clear a track without a language: %v %v", found, err)
	}
	if got, want := languages(), "audio/0=eng subtitle/0=eng subtitle/2=ger"; got != want {
		t.Errorf("after clearing: %q, want %q", got, want)
	}

	for _, c := range []struct {
		kind    string
		ordinal int32
		lang    *string
		err     string
	}{
		{"video", 0, str("eng"), `kind audio or subtitle, not "video"`},
		{"", 0, nil, `kind audio or subtitle, not ""`},
		{"audio", -1, str("eng"), "0 or more, not -1"},
		{"audio", 0, str("EN"), `three lowercase letters (zxx: no dialogue, und: unknown), not "EN"`},
		{"audio", 0, str("en"), `not "en"`},
		{"audio", 0, str("ENG"), `not "ENG"`},
		{"audio", 0, str("english"), `not "english"`},
		{"audio", 0, str(""), `not ""`},
		{"audio", 0, str("de-CH"), `not "de-CH"`},
	} {
		if _, err := set(c.kind, c.ordinal, c.lang, "admin-3"); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("set %q %d %v: %v, want an error saying %q", c.kind, c.ordinal, c.lang, err, c.err)
		}
	}
	if got, want := languages(), "audio/0=eng subtitle/0=eng subtitle/2=ger"; got != want {
		t.Errorf("after the refusals: %q, want %q", got, want)
	}

	if found, err := st.SetTrackLanguage(ctx, absent, "audio", 0, str("eng"), "admin-3"); err != nil || found {
		t.Errorf("a title there is not: %v %v, want it not found", found, err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieB, "audio", 0, str("eng"), "admin-3"); err == nil ||
		!strings.Contains(err.Error(), "has no source file") {
		t.Errorf("a series: %v, want it refused for having no source file", err)
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_itemtracklanguages WHERE item_id <> $1`, movieA); n != 0 {
		t.Errorf("%d languages kept for titles refused", n)
	}
}

// Once a package has reported the source's audio tracks, a language is set
// only for one of them, as a package carries every audio stream of its
// source; until then for any ordinal. A subtitle's ordinal is not checked, as
// an encode may leave out a subtitle stream the source has, and clearing one
// is never refused.
func TestSetTrackLanguageOfATrackTheSourceDoesNotHave(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	filmWithASource(t, st)
	if _, err := st.SetTrackLanguage(ctx, movieA, "audio", 3, str("eng"), "admin-1"); err != nil {
		t.Fatalf("an ordinal before any package reported the tracks: %v", err)
	}
	if _, err := st.RecordSourceTracks(ctx, movieA, map[string][]model.SourceTrack{
		"audio":    {{Kind: "audio", Ordinal: 0, Language: str("und")}, {Kind: "audio", Ordinal: 1, Language: str("eng")}},
		"subtitle": {{Kind: "subtitle", Ordinal: 0, Language: str("ger")}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := st.SetTrackLanguage(ctx, movieA, "audio", 2, str("eng"), "admin-1")
	if err == nil || !strings.Contains(err.Error(), "has no audio track 2: its package reported 2, ordinals 0 to 1") {
		t.Errorf("an audio track the package did not report: %v", err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieA, "audio", 1, str("ger"), "admin-1"); err != nil {
		t.Errorf("a track the package reported: %v", err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieA, "subtitle", 4, str("eng"), "admin-1"); err != nil {
		t.Errorf("a subtitle its package did not report: %v", err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieA, "audio", 3, nil, "admin-1"); err != nil {
		t.Errorf("clearing the language of a track the package did not report: %v", err)
	}
	if got, want := trackLines(t, st, movieA), `audio 0 und - true - - false
audio 1 eng ger true - - false
subtitle 0 ger - true - - false
subtitle 4 - eng false - - false`; got != want {
		t.Errorf("tracks:\n%s\nwant:\n%s", got, want)
	}
}

// A package's report of the source's tracks replaces what the catalog held of
// each kind it lists: a track it no longer lists goes, a kind it does not
// list stays. A track's language is the package's, but a language equal to
// the one an admin set says nothing of the source's tag (the packager labels
// the track with the admin's), so the track keeps the tag it had.
func TestRecordSourceTracks(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	filmWithASource(t, st)
	record := func(tracks map[string][]model.SourceTrack) int {
		t.Helper()
		n, err := st.RecordSourceTracks(ctx, movieA, tracks)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	audio := func(ordinal int32, lang, title string) model.SourceTrack {
		tr := model.SourceTrack{Kind: "audio", Ordinal: ordinal, Language: str(lang)}
		if title != "" {
			tr.Title = str(title)
		}
		return tr
	}
	sub := func(ordinal int32, lang, format string, forced bool) model.SourceTrack {
		return model.SourceTrack{Kind: "subtitle", Ordinal: ordinal, Language: str(lang), Format: str(format), Forced: forced}
	}

	if n := record(map[string][]model.SourceTrack{
		"audio":    {audio(0, "und", ""), audio(1, " ENG ", "Commentary")},
		"subtitle": {sub(1, "eng", "webvtt", true), sub(0, "ger", "pgs", false)},
	}); n != 4 {
		t.Errorf("recorded %d tracks, want 4", n)
	}
	if got, want := trackLines(t, st, movieA), `audio 0 und - true - - false
audio 1 eng - true Commentary - false
subtitle 0 ger - true - pgs false
subtitle 1 eng - true - webvtt true`; got != want {
		t.Errorf("the first report:\n%s\nwant:\n%s", got, want)
	}

	// An admin's languages; the package then labels the tracks with them.
	for ordinal, lang := range map[int32]string{0: "zxx", 1: "eng"} {
		if _, err := st.SetTrackLanguage(ctx, movieA, "audio", ordinal, str(lang), "admin-1"); err != nil {
			t.Fatal(err)
		}
	}
	record(map[string][]model.SourceTrack{"audio": {audio(0, "zxx", ""), audio(1, "eng", "Director's commentary")}})
	if got, want := trackLines(t, st, movieA), `audio 0 und zxx true - - false
audio 1 eng eng true Director's commentary - false
subtitle 0 ger - true - pgs false
subtitle 1 eng - true - webvtt true`; got != want {
		t.Errorf("a package labelled with the admin's languages:\n%s\nwant:\n%s", got, want)
	}

	// A packager that labels the tracks as the source tags them says the
	// tag; a track it no longer lists goes, with its admin's language kept; a
	// track first reported with the admin's language (one set before a
	// package reported the source's subtitles) has no known tag.
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language)
		VALUES ($1, 'subtitle', 2, 'fre')`, movieA)
	record(map[string][]model.SourceTrack{
		"audio":    {audio(0, "mis", "")},
		"subtitle": {sub(0, "ger", "pgs", false), sub(2, "fre", "webvtt", false)},
	})
	if got, want := trackLines(t, st, movieA), `audio 0 mis zxx true - - false
audio 1 - eng false - - false
subtitle 0 ger - true - pgs false
subtitle 2 - fre true - webvtt false`; got != want {
		t.Errorf("the second report:\n%s\nwant:\n%s", got, want)
	}

	// A kind listed without a track: the source has none of it. Of a track
	// reported twice the first counts; one at a negative ordinal is none.
	record(map[string][]model.SourceTrack{
		"audio":    {audio(0, "eng", "first"), audio(0, "ger", "second"), audio(-1, "fre", "")},
		"subtitle": {},
	})
	if got, want := trackLines(t, st, movieA), `audio 0 eng zxx true first - false
audio 1 - eng false - - false
subtitle 2 - fre false - - false`; got != want {
		t.Errorf("the third report:\n%s\nwant:\n%s", got, want)
	}
	if _, err := st.RecordSourceTracks(ctx, movieA, map[string][]model.SourceTrack{"video": {}}); err == nil {
		t.Error("a report of video tracks was taken")
	}
}

// A long title is cut to its column rather than failing the report.
func TestRecordSourceTracksCutsLongTags(t *testing.T) {
	st := storetest.Open(t)
	ctx := context.Background()
	filmWithASource(t, st)
	long := strings.Repeat("é", 300)
	if _, err := st.RecordSourceTracks(ctx, movieA, map[string][]model.SourceTrack{
		"audio": {{Kind: "audio", Ordinal: 0, Language: str(strings.Repeat("x", 40)), Title: &long}},
	}); err != nil {
		t.Fatal(err)
	}
	tracks, err := st.Tracks(ctx, movieA)
	if err != nil || len(tracks) != 1 {
		t.Fatalf("tracks %v, %v", tracks, err)
	}
	if len([]rune(*tracks[0].Title)) != 255 || len(*tracks[0].Language) != 35 {
		t.Errorf("title of %d characters, language of %d; want 255 and 35", len([]rune(*tracks[0].Title)), len(*tracks[0].Language))
	}
}

// A catalog without migration 037 knows no tracks and keeps no language of
// one: setting one is an error naming the migration, a package's report is
// let go, and an item is deleted as before.
func TestTracksWithoutTheMigration(t *testing.T) {
	st := storetest.OpenBase(t)
	ctx := context.Background()
	if err := st.EnsureDeletionLog(ctx); err != nil {
		t.Fatal(err)
	}
	filmWithASource(t, st)
	if tracks, err := st.Tracks(ctx, movieA); err != nil || tracks != nil {
		t.Errorf("Tracks: %v, %v", tracks, err)
	}
	if ls, err := st.TrackLanguages(ctx, movieA); err != nil || ls != nil {
		t.Errorf("TrackLanguages: %v, %v", ls, err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieA, "audio", 0, str("zxx"), "admin-1"); err == nil ||
		!strings.Contains(err.Error(), "037_track_languages.sql") {
		t.Errorf("SetTrackLanguage: %v, want an error naming the migration", err)
	}
	if _, err := st.SetTrackLanguage(ctx, movieA, "audio", 0, nil, "admin-1"); err == nil ||
		!strings.Contains(err.Error(), "037_track_languages.sql") {
		t.Errorf("clearing: %v, want an error naming the migration", err)
	}
	if n, err := st.RecordSourceTracks(ctx, movieA, map[string][]model.SourceTrack{"audio": {{Kind: "audio"}}}); err != nil || n != 0 {
		t.Errorf("RecordSourceTracks: %d, %v", n, err)
	}
	if ok, err := st.DeleteItem(ctx, movieA, store.Deletion{By: "admin-1"}); err != nil || !ok {
		t.Errorf("DeleteItem: %v, %v", ok, err)
	}
}
