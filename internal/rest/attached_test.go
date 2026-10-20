package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// attachBody is a POST body of the attach seam.
func attachBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// packagedFilm is the v2 film with its version complete: the version that
// plays, a subtitle can be attached to.
func packagedFilm(t *testing.T) *v2Film {
	t.Helper()
	f := newV2Film(t)
	if code, answer := f.complete(t, f.payload(filmVersion, filmPackage, f.version(t, filmVersion, filmPackage))); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	return f
}

const germanSRT = "1\r\n00:00:01,000 --> 00:00:02,500\r\nGuten Tag\r\n\r\n2\r\n00:00:03,000 --> 00:00:04,000\r\n<i>Wie geht's?</i>\r\n"

const germanVTT = "WEBVTT\n\n00:00:01.000 --> 00:00:02.500\nGuten Tag\n\n00:00:03.000 --> 00:00:04.000\n<i>Wie geht's?</i>\n"

// An addon attaches a subtitle to a packaged title: the SubRip it sends is
// written as WebVTT into the subs/ folder of the version that plays, named
// by its bytes, and recorded as made, not default, with the version it
// follows and the client that attached it. The catalog's subtitles list it
// and serve it to a player as any other, the version's chain still holds,
// and the same file attached again is the subtitle there is.
func TestAnAddonAttachesASubtitle(t *testing.T) {
	f := packagedFilm(t)
	addon := f.iss.Addon(t)
	w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
		attachBody(t, map[string]any{"language": "ger", "origin": "provider", "content": germanSRT}), addon)
	if w.Code != http.StatusCreated {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	var answer struct {
		ItemID  string           `json:"itemId"`
		Created bool             `json:"created"`
		Sub     library.Subtitle `json:"subtitle"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(germanVTT))
	vdir := library.VersionDir(f.itemDir, filmVersion)
	path := filepath.Join(vdir, "subs", "attached-"+hex.EncodeToString(sum[:])+".de.vtt")
	if b, err := os.ReadFile(path); err != nil || string(b) != germanVTT {
		t.Fatalf("the file at %s: %q %v", path, b, err)
	}
	s := answer.Sub
	if answer.ItemID != filmItem || !answer.Created || s.Language != "de" || s.Format != "webvtt" || s.Label != nil ||
		s.Forced || s.SDH || s.Origin == nil || *s.Origin != "provider" || !s.Attached || s.VersionID == nil ||
		*s.VersionID != filmVersion || s.AttachedBy == nil || *s.AttachedBy != "example-addon" || s.AttachedAt == nil {
		t.Errorf("the answer: %s", w.Body)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1 AND path = $2
		AND NOT isdefault AND sha256 = $3`, s.ID, path, hex.EncodeToString(sum[:])); n != 1 {
		t.Error("the row is not the file's")
	}

	// A player lists it and plays it, with the title's own.
	viewer := f.iss.Viewer(t)
	w = do(f.h, http.MethodGet, "/api/subtitles/items/"+filmItem, "", viewer)
	if !strings.Contains(w.Body.String(), `"id":"`+s.ID+`"`) || !strings.Contains(w.Body.String(), `"lang":"de"`) {
		t.Errorf("the title's subtitles: %s", w.Body)
	}
	w = do(f.h, http.MethodGet, "/api/subtitles/"+s.ID, "", viewer)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/vtt; charset=utf-8" || w.Body.String() != germanVTT {
		t.Errorf("the subtitle served: %d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	if _, err := library.VerifyVersion(vdir, true); err != nil {
		t.Errorf("the version's chain: %v", err)
	}

	// Again, as WebVTT this time: the same file, the same subtitle.
	w = do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
		attachBody(t, map[string]any{"language": "de", "origin": "provider", "content": germanVTT, "format": "webvtt"}), addon)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"created":false`) || !strings.Contains(w.Body.String(), s.ID) {
		t.Errorf("attached again: %d %s", w.Code, w.Body)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin IS NOT NULL`); n != 1 {
		t.Errorf("%d attached subtitles, want the one", n)
	}

	// Forced and for the hard of hearing, made by a model, without a label:
	// a file of its own, labelled by what it is.
	w = do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles", attachBody(t, map[string]any{
		"language": "zh-hans", "origin": "translated", "sdh": true, "forced": true, "content": germanVTT}), addon)
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"label":"SDH · AI"`) ||
		!strings.Contains(w.Body.String(), `"language":"zh-Hans"`) || !strings.Contains(w.Body.String(), `"forced":true`) {
		t.Errorf("a translated SDH subtitle: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(vdir, "subs", "attached-"+hex.EncodeToString(sum[:])+".zh-hans.forced.sdh.vtt")); err != nil {
		t.Error(err)
	}
}

// The title's subtitles say which are its own and which attached, the
// version one is attached to, and, asked for, what the original is matched
// by: its size, its Sum64 and what its name claimed.
func TestTheTitlesSubtitlesAndItsOriginal(t *testing.T) {
	f := packagedFilm(t)
	original := f.cfg.Roots(true).Arrivals + "/A Film (2024)/A Film (2024).mkv"
	librarytest.Write(t, original, make([]byte, 200000))
	librarytest.WriteSource(t, library.SourceDir(f.itemDir, filmSource), map[string]any{"sourceId": filmSource,
		"labels": map[string]any{"quality": "1080p", "medium": "web", "resolution": "1080p", "edition": nil}}, nil)
	addon := f.iss.Addon(t)
	if w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
		attachBody(t, map[string]any{"language": "en", "origin": "provider", "content": germanVTT}), addon); w.Code != http.StatusCreated {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	w := do(f.h, http.MethodGet, "/api/library/items/"+filmItem+"/subtitles?fingerprint=true", "", addon)
	var answer struct {
		ItemID     string             `json:"itemId"`
		Type       string             `json:"type"`
		VersionID  string             `json:"versionId"`
		Attachable bool               `json:"attachable"`
		Subtitles  []library.Subtitle `json:"subtitles"`
		Original   struct {
			SizeBytes int64          `json:"sizeBytes"`
			Sum64     string         `json:"sum64"`
			Labels    map[string]any `json:"labels"`
		} `json:"original"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	if answer.ItemID != filmItem || answer.Type != "movie" || answer.VersionID != filmVersion || !answer.Attachable {
		t.Errorf("the title: %s", w.Body)
	}
	var own, attached []string
	for _, s := range answer.Subtitles {
		if s.Attached {
			attached = append(attached, s.Language)
		} else {
			own = append(own, s.Language)
		}
	}
	if strings.Join(own, ",") != "de,ger,spa" || strings.Join(attached, ",") != "en" || answer.Subtitles[3].Language != "en" {
		t.Errorf("own %v, attached %v: %s", own, attached, w.Body)
	}
	if answer.Original.SizeBytes != 200000 || answer.Original.Sum64 != "0000000000030d40" || answer.Original.Labels["medium"] != "web" {
		t.Errorf("the original: %+v", answer.Original)
	}
	if w := do(f.h, http.MethodGet, "/api/library/items/"+filmItem+"/subtitles", "", addon); strings.Contains(w.Body.String(), "sum64") {
		t.Errorf("the original read unasked: %s", w.Body)
	}

	// Without its original: the subtitles, and why there is no fingerprint.
	os.Remove(original)
	w = do(f.h, http.MethodGet, "/api/library/items/"+filmItem+"/subtitles?fingerprint=true", "", addon)
	if !strings.Contains(w.Body.String(), `"original":null`) || !strings.Contains(w.Body.String(), `"originalNote":"the original cannot be read`) {
		t.Errorf("no original: %s", w.Body)
	}
}

// What is not a subtitle of a packaged title is refused, saying why, and
// nothing is written.
func TestAnAttachIsRefused(t *testing.T) {
	f := newV2Film(t) // building, not packaged
	addon := f.iss.Addon(t)
	post := func(item string, body string) (int, string) {
		w := do(f.h, http.MethodPost, "/api/library/items/"+item+"/subtitles", body, addon)
		return w.Code, w.Body.String()
	}
	ok := map[string]any{"language": "de", "origin": "provider", "content": germanSRT}
	if code, body := post(filmItem, attachBody(t, ok)); code != http.StatusConflict || !strings.Contains(body, "no package in the library") {
		t.Errorf("a title not packaged: %d %s", code, body)
	}
	if code, body := post("00000000-0000-4000-8000-00000000dead", attachBody(t, ok)); code != http.StatusNotFound {
		t.Errorf("no such title: %d %s", code, body)
	}
	f = packagedFilm(t)
	addon = f.iss.Addon(t)
	with := func(k string, v any) string {
		m := map[string]any{}
		for key, x := range ok {
			m[key] = x
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return attachBody(t, m)
	}
	for name, c := range map[string]struct {
		body   string
		status int
		says   string
	}{
		"no JSON":          {"{", http.StatusBadRequest, "invalid JSON"},
		"no language":      {with("language", nil), http.StatusBadRequest, "required"},
		"no origin":        {with("origin", nil), http.StatusBadRequest, "required"},
		"no content":       {with("content", nil), http.StatusBadRequest, "required"},
		"no language tag":  {with("language", "German"), http.StatusBadRequest, "BCP 47"},
		"undetermined":     {with("language", "und"), http.StatusBadRequest, "BCP 47"},
		"another origin":   {with("origin", "downloaded"), http.StatusBadRequest, "origin must be one of source, provider"},
		"another format":   {with("format", "ass"), http.StatusBadRequest, "webvtt or srt"},
		"not UTF-8":        {with("content", "1\n00:00:01,000 --> 00:00:02,000\n\xff\xfe\n"), http.StatusUnprocessableEntity, "could not be read"},
		"no cue":           {with("content", "WEBVTT\n\n"), http.StatusUnprocessableEntity, "no cue"},
		"WebVTT, said SRT": {with("format", "webvtt"), http.StatusUnprocessableEntity, "WEBVTT header"},
		"too large":        {with("content", strings.Repeat("x", 9<<20)), http.StatusRequestEntityTooLarge, "8 MiB"},
	} {
		code, body := post(filmItem, c.body)
		if code != c.status || !strings.Contains(body, c.says) {
			t.Errorf("%s: %d %s, want %d saying %q", name, code, body, c.status, c.says)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin IS NOT NULL`); n != 0 {
		t.Errorf("%d subtitles attached by refused calls", n)
	}
	entries, _ := os.ReadDir(filepath.Join(library.VersionDir(f.itemDir, filmVersion), "subs"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), library.AttachedPrefix) {
			t.Errorf("a refused call wrote %s", e.Name())
		}
	}
}

// A caller removes a subtitle it attached, an admin any attached one: its
// row and its file go, and removing it again is done as well. The title's
// own subtitles are no caller's to remove, nor is one another client
// attached.
func TestASubtitleAttachedIsRemoved(t *testing.T) {
	f := packagedFilm(t)
	addon := f.iss.Addon(t)
	attach := func(token, lang string) library.Subtitle {
		t.Helper()
		w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
			attachBody(t, map[string]any{"language": lang, "origin": "provider", "content": germanSRT}), token)
		var answer struct {
			Sub library.Subtitle `json:"subtitle"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusCreated {
			t.Fatalf("attach: %d %s", w.Code, w.Body)
		}
		return answer.Sub
	}
	mine, theirs := attach(addon, "de"), attach(f.iss.Token(t, map[string]any{"sub": "addon-2", "azp": "other-addon",
		"aud": "chino", "realm_access": map[string]any{"roles": []string{"zaentrum-addon"}}}), "en")
	del := func(id, token string) (int, string) {
		w := do(f.h, http.MethodDelete, "/api/library/items/"+filmItem+"/subtitles/"+id, "", token)
		return w.Code, w.Body.String()
	}
	if code, body := del(theirs.ID, addon); code != http.StatusForbidden || !strings.Contains(body, "another client attached") {
		t.Errorf("another's: %d %s", code, body)
	}
	var own string
	if err := f.st.Pool().QueryRow(t.Context(), `SELECT id FROM com_nalet_katalog_subtitleassets WHERE item_id = $1
		AND origin IS NULL ORDER BY id LIMIT 1`, filmItem).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if code, body := del(own, f.iss.Admin(t)); code != http.StatusConflict || !strings.Contains(body, "the title's own") {
		t.Errorf("the title's own: %d %s", code, body)
	}
	if code, body := del(mine.ID, addon); code != http.StatusNoContent {
		t.Fatalf("its own: %d %s", code, body)
	}
	if _, err := os.Stat(mine.Path); err == nil {
		t.Error("the file of the subtitle removed stays")
	}
	// Removed already, or never there: it is not attached either way.
	for _, id := range []string{mine.ID, "no-such-subtitle"} {
		if code, body := del(id, addon); code != http.StatusNoContent {
			t.Errorf("%s again: %d %s", id, code, body)
		}
	}
	var path string
	_ = f.st.Pool().QueryRow(t.Context(), `SELECT path FROM com_nalet_katalog_subtitleassets WHERE id = $1`, theirs.ID).Scan(&path)
	if code, body := del(theirs.ID, f.iss.Admin(t)); code != http.StatusNoContent {
		t.Errorf("an admin's: %d %s", code, body)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the file of the subtitle an admin removed stays")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin IS NOT NULL`); n != 0 {
		t.Errorf("%d attached subtitles stay", n)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1`, own); n != 1 {
		t.Error("the title's own subtitle went")
	}
}

// A new version of the same file carries the subtitles attached to the
// title over: their files copied into its subs/, their rows pointing there,
// the version they follow the new one; the package's own are replaced as
// ever. A version of another file does not: its timing may be another's.
func TestAVersionOfTheSameFileCarriesTheAttachedSubtitles(t *testing.T) {
	f := packagedFilm(t)
	addon := f.iss.Addon(t)
	w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
		attachBody(t, map[string]any{"language": "de", "origin": "transcribed", "content": germanSRT}), addon)
	if w.Code != http.StatusCreated {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	const next, nextPkg = "77c10000-0000-4000-8000-000000000006", "88c10000-0000-4000-8000-000000000007"
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		next, filmItem, []string{filmSource})
	if code, answer := f.complete(t, f.payload(next, nextPkg, f.version(t, next, nextPkg))); code != http.StatusOK {
		t.Fatalf("the next version: %d %v", code, answer)
	}
	sum := sha256.Sum256([]byte(germanVTT))
	moved := filepath.Join(library.VersionDir(f.itemDir, next), "subs", "attached-"+hex.EncodeToString(sum[:])+".de.vtt")
	if b, err := os.ReadFile(moved); err != nil || string(b) != germanVTT {
		t.Fatalf("the file in the new version: %q %v", b, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin = 'transcribed'
		AND path = $1 AND versionid = $2 AND label = 'AI'`, moved, next); n != 1 {
		t.Error("the attached subtitle does not follow the new version")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE path LIKE $1`,
		library.VersionDir(f.itemDir, filmVersion)+"/%"); n != 0 {
		t.Errorf("%d rows of the superseded version stay", n)
	}
	if _, err := library.VerifyVersion(library.VersionDir(f.itemDir, next), true); err != nil {
		t.Errorf("the new version's chain: %v", err)
	}

	// A version of another file.
	const other, otherSource, otherPkg = "77c10000-0000-4000-8000-000000000016", "0b6c0000-0000-4000-8000-000000000013",
		"88c10000-0000-4000-8000-000000000017"
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, librarypath, sizebytes, state)
		VALUES ($1, $2, 'original.mkv', 'A Film (2024)/other.mkv', 2000, 'present')`, otherSource, filmItem)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		other, filmItem, []string{otherSource})
	librarytest.WriteSource(t, library.SourceDir(f.itemDir, otherSource), map[string]any{"sourceId": otherSource}, nil)
	complete := librarytest.WriteVersion(t, library.VersionDir(f.itemDir, other), librarytest.Version{VersionID: other,
		PackageID: otherPkg, SourceIDs: []string{otherSource}, CreatedAt: "2026-10-07T09:00:00Z",
		Package: map[string]any{"renditions": map[string]any{"video": []any{}, "audio": []any{}}, "subtitles": []any{}}})
	body := strings.Replace(strings.Replace(f.payload(other, otherPkg, complete), `"sourceId": "`+filmSource, `"sourceId": "`+otherSource, 1),
		`"sidecars": [{"subtitleAssetId": "s-de", "rendition": "sub2", "path": "subs/2.vtt"}]`, `"sidecars": []`, 1)
	if code, answer := f.complete(t, body); code != http.StatusOK {
		t.Fatalf("a version of another file: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin IS NOT NULL`); n != 0 {
		t.Errorf("%d attached subtitles follow a version of another file", n)
	}
}

// The instance's subtitle languages are its own setting's, else the
// languages its players show, the packager's whitelist; each spelled as a
// language tag, once, in the setting's order. None without either.
func TestTheInstancesSubtitleLanguages(t *testing.T) {
	st := storetest.Open(t)
	h, iss := server(t, st, testConfig(t.TempDir()))
	addon := iss.Addon(t)
	get := func() string {
		t.Helper()
		w := do(h, http.MethodGet, "/api/library/subtitles/languages", "", addon)
		if w.Code != http.StatusOK {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return strings.TrimSpace(w.Body.String())
	}
	if got := get(); got != `{"from":null,"languages":[]}` {
		t.Errorf("no setting: %s", got)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('w', 'packager.language_whitelist', 'eng, ger,  , zh, de')`)
	if got := get(); got != `{"from":"packager.language_whitelist","languages":["en","de","zh"]}` {
		t.Errorf("the packager's whitelist: %s", got)
	}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('s', 'subtitles.languages', 'en,de,zh-Hans')`)
	if got := get(); got != `{"from":"subtitles.languages","languages":["en","de","zh-Hans"]}` {
		t.Errorf("the subtitles' own setting: %s", got)
	}
}

// An episode another's file covers has the subtitles of that file: the
// seam answers, and attaches, for the episode whose file it is.
func TestACoveredEpisodeAttachesToItsHolder(t *testing.T) {
	f := packagedFilm(t)
	const covered = "e2e2e2e2-0000-4000-8000-000000000002"
	storetest.AddItem(t, f.st, covered, "episode", "The Second Half", "")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $1 WHERE id = $2`, filmItem, covered)
	w := do(f.h, http.MethodPost, "/api/library/items/"+covered+"/subtitles",
		attachBody(t, map[string]any{"language": "de", "origin": "provider", "content": germanSRT}), f.iss.Addon(t))
	if w.Code != http.StatusCreated || !strings.Contains(w.Body.String(), `"itemId":"`+filmItem+`"`) {
		t.Fatalf("attach to a covered episode: %d %s", w.Code, w.Body)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND origin IS NOT NULL`,
		filmItem); n != 1 {
		t.Error("the subtitle is not the holder's")
	}
}

// The seam is an addon's, the service account's and an admin's: a viewer and
// another confidential client are refused each of its routes, and nothing
// is attached or removed for them.
func TestTheAttachSeamIsForWhomItIsFor(t *testing.T) {
	f := packagedFilm(t)
	var own string
	if err := f.st.Pool().QueryRow(t.Context(), `SELECT id FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 LIMIT 1`,
		filmItem).Scan(&own); err != nil {
		t.Fatal(err)
	}
	body := attachBody(t, map[string]any{"language": "de", "origin": "provider", "content": germanSRT})
	for _, token := range []string{f.iss.Viewer(t), f.iss.Service(t, "zaentrum-other")} {
		for _, c := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/library/items/" + filmItem + "/subtitles", ""},
			{http.MethodPost, "/api/library/items/" + filmItem + "/subtitles", body},
			{http.MethodDelete, "/api/library/items/" + filmItem + "/subtitles/" + own, ""},
		} {
			if w := do(f.h, c.method, c.path, c.body, token); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s, want 403", c.method, c.path, w.Code, w.Body)
			}
		}
	}
	if w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles", body, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	for _, token := range []string{f.iss.Service(t, "zaentrum-manager"), f.iss.Admin(t)} {
		if w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles", body, token); w.Code/100 != 2 {
			t.Errorf("attach: %d %s", w.Code, w.Body)
		}
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE origin IS NOT NULL`); n != 1 {
		t.Errorf("%d attached, want the one the service account and the admin attached", n)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1`, own); n != 1 {
		t.Error("a refused caller removed a subtitle")
	}
}

// A catalog without migration 048 still takes a package in: it has no
// attached subtitle to carry over.
func TestAPackageWithoutTheAttachedSubtitlesMigration(t *testing.T) {
	f := newV2Film(t)
	storetest.Exec(t, f.st, `ALTER TABLE com_nalet_katalog_subtitleassets DROP COLUMN origin, DROP COLUMN issdh,
		DROP COLUMN versionid, DROP COLUMN sha256, DROP COLUMN attachedby, DROP COLUMN attachedat`)
	if code, answer := f.complete(t, f.payload(filmVersion, filmPackage, f.version(t, filmVersion, filmPackage))); code != http.StatusOK {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE path LIKE $1`,
		library.VersionDir(f.itemDir, filmVersion)+"/%"); n == 0 {
		t.Error("the package's subtitles were not recorded")
	}
}
