package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// subtitleRows are the item's subtitles, one per line: "id path lang default
// forced", by path.
func subtitleRows(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT id, path, COALESCE(lang, '-'), COALESCE(isdefault, false), isforced
		FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 ORDER BY path`, item)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, path, lang string
		var def, forced bool
		if err := rows.Scan(&id, &path, &lang, &def, &forced); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %v %v", id, path, lang, def, forced))
	}
	return strings.Join(out, "\n")
}

// packagedManifest is what a packager sends for the film: two audio tracks
// and two subtitle tracks of its source, labelled with the languages an
// admin set (audio 1 zxx, subtitle 1 spa), the second subtitle forced and
// the default, and its rendition of the German subtitle file beside the
// source (external, its id counting on from the source's).
const packagedManifest = `{"version": 2, "durationMs": 600000,
	"renditions": {
		"video": [{"id": "v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 800, "bitrateBps": 4000000}],
		"audio": [
			{"id": "a0", "idx": 0, "language": "eng", "title": "", "default": false, "codec": "mp4a.40.2", "channels": 2},
			{"id": "a1", "idx": 1, "language": "zxx", "title": "Music only", "default": true, "codec": "mp4a.40.2", "channels": 2}],
		"audioSurround": [{"id": "a2", "idx": 0, "language": "eng", "default": true, "codec": "ec-3", "channels": 6}]},
	"subtitles": [
		{"id": "sub0", "path": "subs/0.vtt", "language": "ger", "title": "", "default": false, "forced": false, "format": "webvtt"},
		{"id": "sub1", "path": "subs/1.vtt", "language": "spa", "title": "Signs", "default": true, "forced": true, "format": "webvtt"},
		{"id": "sub2", "path": "subs/2.vtt", "language": "deu", "title": "Deutsch", "default": false, "forced": false,
		 "format": "webvtt", "external": true}]}`

// A packaging that completes replaces the package's subtitles with the
// manifest's, each with whether it is forced, and leaves the subtitle files
// beside the source as they are, rows, ids and all, however often the title
// is packaged. The packager's rendition of such a file (external) gets no row:
// the file's row stands for it. The manifest's languages are the ones the
// tracks play as, the packager having labelled them with an admin's, and so
// is the packaged asset's audio. The source's tracks are recorded from the
// manifest, the file's rendition not among them; a track the packager
// labelled with an admin's language has no source tag known.
func TestPackagingCompleteKeepsTheSubtitleFilesBesideTheSource(t *testing.T) {
	st := storetest.Open(t)
	cfg := testConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	dir := cfg.NFSRoot + "/A Film (2024)/"
	pkg := cfg.PackagesRoot + "/movies/f1/" + filmItem
	// The files beside the source; a package's subtitles from an earlier
	// packaging, and one from a package under another category (the title
	// was an episode once).
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault) VALUES
		('s-de', $1, $2 || 'A Film (2024).de.srt', 'srt', 'de', 'Deutsch', false),
		('s-en', $1, $2 || 'A Film (2024).en.vtt', 'vtt', 'en', 'English', false),
		('old-0', $1, $3 || '/subs/0.vtt', 'webvtt', 'ger', '', true),
		('old-9', $1, $4, 'webvtt', 'fre', '', false)`, filmItem, dir, pkg, cfg.PackagesRoot+"/shows/f1/"+filmItem+"/subs/9.vtt")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES
		($1, 'audio', 1, 'zxx'), ($1, 'subtitle', 1, 'spa')`, filmItem)

	want := fmt.Sprintf(`s-de %[1]sA Film (2024).de.srt de false false
s-en %[1]sA Film (2024).en.vtt en false false
%%s %[2]s/subs/0.vtt ger false false
%%s %[2]s/subs/1.vtt spa true true`, dir, pkg)
	for run := 1; run <= 2; run++ {
		w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", packagedManifest, svc)
		if w.Code != http.StatusOK {
			t.Fatalf("packaging-complete, %d. time: %d %s", run, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["subtitlesWritten"] != 2.0 || body["audioTracks"] != 2.0 {
			t.Errorf("packaging-complete, %d. time: %s", run, w.Body.String())
		}
		got := subtitleRows(t, st, filmItem)
		lines := strings.Split(got, "\n")
		if len(lines) != 4 {
			t.Fatalf("after packaging %d times:\n%s", run, got)
		}
		// the package's rows are new each time; the files' keep their ids
		ids := []any{strings.Fields(lines[2])[0], strings.Fields(lines[3])[0]}
		if w := fmt.Sprintf(want, ids...); got != w || ids[0] == "old-0" {
			t.Errorf("subtitles after packaging %d times:\n%s\nwant:\n%s", run, got, w)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE item_id = $1
		AND kind = 'packaged' AND audiolanguage = 'zxx' AND subtitletrackcount = 3`, filmItem); n != 1 {
		t.Error("the packaged asset's audio is not the language its primary track plays as")
	}
	if got, want := trackLinesOf(t, st, filmItem), `audio 0 eng -
audio 1 - zxx
subtitle 0 ger -
subtitle 1 - spa`; got != want {
		t.Errorf("the source's tracks:\n%s\nwant:\n%s", got, want)
	}

	// A manifest that lists no subtitle leaves the files beside the source,
	// and says the source has no subtitle streams; one without subtitles or
	// audio at all says nothing of the tracks.
	if w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", `{"renditions": {"video": [], "audio": []}, "subtitles": []}`, svc); w.Code != http.StatusOK {
		t.Fatalf("an empty manifest: %d %s", w.Code, w.Body.String())
	}
	if got, want := subtitleRows(t, st, filmItem), fmt.Sprintf("s-de %[1]sA Film (2024).de.srt de false false\ns-en %[1]sA Film (2024).en.vtt en false false", dir); got != want {
		t.Errorf("after a manifest without subtitles:\n%s\nwant:\n%s", got, want)
	}
	if got, want := trackLinesOf(t, st, filmItem), "audio 1 - zxx\nsubtitle 1 - spa"; got != want {
		t.Errorf("the tracks after a manifest without any:\n%s\nwant:\n%s", got, want)
	}
	if w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", packagedManifest, svc); w.Code != http.StatusOK {
		t.Fatalf("the manifest again: %d %s", w.Code, w.Body.String())
	}
	if w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", `{}`, svc); w.Code != http.StatusOK {
		t.Fatalf("{}: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, filmItem); n != 2 {
		t.Errorf("%d subtitles after {}, want the two files", n)
	}
	if got, want := trackLinesOf(t, st, filmItem), "audio 0 eng -\naudio 1 - zxx\nsubtitle 0 ger -\nsubtitle 1 - spa"; got != want {
		t.Errorf("the tracks after {}:\n%s\nwant them as the manifest before said", got)
	}
}

// On a catalog without migration 038 a packaging completes as before, its
// subtitles written without a word of whether they are forced.
func TestPackagingCompleteWithoutTheForcedMigration(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `ALTER TABLE com_nalet_katalog_subtitleassets DROP COLUMN isforced`)
	cfg := testConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	h, iss := server(t, st, cfg)
	w := do(h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", packagedManifest, iss.Service(t, "zaentrum-manager"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"subtitlesWritten":2`) {
		t.Fatalf("packaging-complete without 038: %d %s", w.Code, w.Body.String())
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1`, filmItem); n != 2 {
		t.Errorf("%d subtitles, want the package's two", n)
	}
}

// trackLinesOf are the item's tracks, one per line: "kind ordinal source
// override", - for none.
func trackLinesOf(t *testing.T, st *store.Store, item string) string {
	t.Helper()
	tracks, err := st.Tracks(context.Background(), item)
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
		out = append(out, fmt.Sprintf("%s %d %s %s", tr.Kind, tr.Ordinal, opt(tr.Language), opt(tr.Override)))
	}
	return strings.Join(out, "\n")
}
