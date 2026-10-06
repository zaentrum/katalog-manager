package rest

import (
	"net/http"
	"os"
	"path/filepath"
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

// files writes each file (relative to dir) with a line of text; a size makes
// it that large.
func files(t *testing.T, dir string, sizes map[string]int64) {
	t.Helper()
	for name, size := range sizes {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if size > 0 {
			if err := os.Truncate(path, size); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The worker record the packager reads: what it always said, and, when there
// is any, the languages an admin set for the source's tracks (trackLanguages,
// audio first, each kind by ordinal) and the subtitle files beside the
// source (subtitleFiles, by path, each with its subtitle asset's id, its
// language an ISO 639-2 code, forced a boolean). A subtitle file is one the packager takes: at an absolute path in
// the source's folder or below it, a .srt, .vtt, .ass or .ssa file there is,
// of at most 50 MB; a package's subtitle is none. Each key is left out when
// there is none, so the record is what it was for a title without them.
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
	files(t, dir, map[string]int64{
		"A Film (2024).de.srt": 0, "A Film (2024).eng.ass": 0, "A Film (2024).vtt": 0, "A Film (2024).pt-BR.ssa": 0,
		"Subs/A Film (2024).fr.srt": 0, "A Film (2024).sup": 0, "A Film (2024).nl.srt": maxSubtitleFileBytes + 1,
		"A Film (2024).sv.srt": 0, "dir.srt/x": 0,
	})
	files(t, cfg.NFSRoot+"/Another Film/", map[string]int64{"A Film (2024).es.srt": 0})
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault) VALUES
		('s-de', $1, $2 || 'A Film (2024).de.srt', 'srt', 'de', 'Deutsch', true),
		('s-en', $1, $2 || 'A Film (2024).eng.ass', 'ass', 'eng', 'English', false),
		('s-none', $1, $2 || 'A Film (2024).vtt', 'vtt', NULL, 'Subtitles', false),
		('s-pt', $1, $2 || 'A Film (2024).pt-BR.ssa', NULL, 'pt-br', 'Português', false),
		('s-fr', $1, $2 || 'Subs/A Film (2024).fr.srt', 'srt', 'fr', 'Français', false),
		('s-pgs', $1, $2 || 'A Film (2024).sup', 'pgs', 'eng', 'English', false),
		('s-big', $1, $2 || 'A Film (2024).nl.srt', 'srt', 'nl', 'Nederlands', false),
		('s-gone', $1, $2 || 'A Film (2024).it.srt', 'srt', 'it', 'Italiano', false),
		('s-dir', $1, $2 || 'dir.srt', 'srt', 'en', 'English', false),
		('s-rel', $1, 'A Film (2024).sv.srt', 'srt', 'sv', 'Svenska', false),
		('s-other', $1, $4 || '/Another Film/A Film (2024).es.srt', 'srt', 'es', 'Español', false),
		('s-pkg', $1, $3 || '/subs/0.vtt', 'webvtt', 'ger', '', true)`, filmItem, dir, pkg, cfg.NFSRoot)
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
		`{"id":"s-de","path":"` + dir + `A Film (2024).de.srt","language":"deu","label":"Deutsch","forced":false},` +
		`{"id":"s-en","path":"` + dir + `A Film (2024).eng.ass","language":"eng","label":"English","forced":false},` +
		`{"id":"s-pt","path":"` + dir + `A Film (2024).pt-BR.ssa","language":"por","label":"Português","forced":false},` +
		`{"id":"s-none","path":"` + dir + `A Film (2024).vtt","language":"und","label":"Subtitles","forced":false},` +
		`{"id":"s-fr","path":"` + dir + `Subs/A Film (2024).fr.srt","language":"fra","label":"Français","forced":false}]}`
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
