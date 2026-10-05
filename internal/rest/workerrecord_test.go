package rest

import (
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// filmItem is a film with its source file in the library, and, under the
// packages root, the package a packager made of it.
const filmItem = "f1f1f1f1-0000-4000-8000-000000000001"

func aFilm(t *testing.T, st *store.Store, media string) {
	t.Helper()
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes)
		VALUES ('src-f1', $1, $2, true, 1000)`, filmItem, media+"/A Film (2024)/A Film (2024).mkv")
}

// The worker record the packager reads: what it always said, and, when there
// is any, the languages an admin set for the source's tracks (trackLanguages,
// audio first, each kind by ordinal) and the subtitle files beside the
// source (subtitleFiles, by path, each language an ISO 639-2 code). A
// package's subtitles, and a file the scanner does not record beside a
// source, are no subtitle file of it. Each key is left out when there is
// none, so the record is what it was for a title without them.
func TestTheWorkerRecordNamesTrackLanguagesAndSubtitleFiles(t *testing.T) {
	st := storetest.Open(t)
	cfg := testConfig(t.TempDir())
	aFilm(t, st, cfg.NFSRoot)
	h, iss := server(t, st, cfg)
	svc := iss.Service(t, "zaentrum-manager")
	record := func() string {
		t.Helper()
		w := do(h, http.MethodGet, "/api/analyze/items/"+filmItem, "", svc)
		if w.Code != http.StatusOK {
			t.Fatalf("GET the record: %d %s", w.Code, w.Body.String())
		}
		return strings.TrimSpace(w.Body.String())
	}
	before := `{"id":"` + filmItem + `","type":"movie","title":"A Film","year":null,"durationMs":null,` +
		`"path":"` + cfg.NFSRoot + `/A Film (2024)/A Film (2024).mkv","seasonNumber":null,"episodeNumber":null,` +
		`"seriesTitle":null,"seriesTmdbId":null,"movieTmdbId":null,"hasOwnPoster":false,"hasOwnBackdrop":false}`
	if got := record(); got != before {
		t.Errorf("a title without track languages or subtitle files:\n got  %s\n want %s", got, before)
	}

	dir := cfg.NFSRoot + "/A Film (2024)/"
	pkg := cfg.PackagesRoot + "/movies/f1/" + filmItem
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault) VALUES
		('s-de', $1, $2 || 'A Film (2024).de.srt', 'srt', 'de', 'Deutsch', true),
		('s-en', $1, $2 || 'A Film (2024).eng.ass', 'ass', 'eng', 'English', false),
		('s-none', $1, $2 || 'A Film (2024).vtt', 'vtt', NULL, 'Subtitles', false),
		('s-pt', $1, $2 || 'A Film (2024).pt-BR.ssa', NULL, 'pt-br', 'Português', false),
		('s-pgs', $1, $2 || 'A Film (2024).sup', 'pgs', 'eng', 'English', false),
		('s-pkg', $1, $3 || '/subs/0.vtt', 'webvtt', 'ger', '', true)`, filmItem, dir, pkg)
	for _, q := range []string{
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ($1, 'subtitle', 0, 'ger')`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ($1, 'audio', 1, 'eng')`,
		`INSERT INTO com_nalet_katalog_itemtracklanguages (item_id, kind, ordinal, language) VALUES ($1, 'audio', 0, 'zxx')`,
	} {
		storetest.Exec(t, st, q, filmItem)
	}
	want := strings.TrimSuffix(before, "}") +
		`,"trackLanguages":[{"kind":"audio","ordinal":0,"language":"zxx"},{"kind":"audio","ordinal":1,"language":"eng"},` +
		`{"kind":"subtitle","ordinal":0,"language":"ger"}],` +
		`"subtitleFiles":[` +
		`{"path":"` + dir + `A Film (2024).de.srt","language":"deu","label":"Deutsch","forced":false},` +
		`{"path":"` + dir + `A Film (2024).eng.ass","language":"eng","label":"English","forced":false},` +
		`{"path":"` + dir + `A Film (2024).pt-BR.ssa","language":"por","label":"Português","forced":false},` +
		`{"path":"` + dir + `A Film (2024).vtt","language":"und","label":"Subtitles","forced":false}]}`
	if got := record(); got != want {
		t.Errorf("a title with both:\n got  %s\n want %s", got, want)
	}

	// The record of a catalog without migration 037 is the record it was.
	base := storetest.OpenBase(t)
	aFilm(t, base, cfg.NFSRoot)
	hb, issb := server(t, base, cfg)
	if w := do(hb, http.MethodGet, "/api/analyze/items/"+filmItem, "", issb.Service(t, "zaentrum-manager")); w.Code != http.StatusOK ||
		strings.TrimSpace(w.Body.String()) != before {
		t.Errorf("without 037: %d %s", w.Code, w.Body.String())
	}
}
