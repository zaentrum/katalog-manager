package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	if code, body := post(filmItem, attachBody(t, ok)); code != http.StatusConflict || !strings.Contains(body, "no version in the library") {
		t.Errorf("a title not packaged nor taken in: %d %s", code, body)
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

// takenFilm is the v2 film taken in: its version taken, its folder holding
// its version.json and the original the title plays from, and no package.
// It answers the film.
func takenFilm(t *testing.T) *v2Film {
	t.Helper()
	f := newV2Film(t)
	moved := f.renameIn(t, f.arrived(t), filmVersion)
	vdir := library.VersionDir(f.itemDir, filmVersion)
	librarytest.Write(t, filepath.Join(vdir, library.VersionFile), librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.version/2", "versionId": filmVersion, "sourceIds": []string{filmSource},
		"originalFiles": []string{"original.mkv"}}))
	if code, answer := f.complete(t, takeInPayload(vdir, moved)); code != http.StatusOK || answer["takenIn"] != true {
		t.Fatalf("the take-in: %d %v", code, answer)
	}
	return f
}

// attachTo attaches the German subtitle, made from the title's audio, to the
// item for the token, and answers it.
func (f *v2Film) attachTo(t *testing.T, item, token string) library.Subtitle {
	t.Helper()
	w := do(f.h, http.MethodPost, "/api/library/items/"+item+"/subtitles",
		attachBody(t, map[string]any{"language": "ger", "origin": "transcribed", "content": germanSRT}), token)
	var answer struct {
		Sub library.Subtitle `json:"subtitle"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("attach: %d %s", w.Code, w.Body)
	}
	return answer.Sub
}

// germanFile is where the German subtitle attached to the version vid of the
// film lies: in its subs/, named by its bytes.
func (f *v2Film) germanFile(vid string) string {
	sum := sha256.Sum256([]byte(germanVTT))
	return library.AttachedPath(library.VersionDir(f.itemDir, vid), library.AttachedName(hex.EncodeToString(sum[:]), "de", false, false))
}

// attachable is what the seam says of the film's version a subtitle is
// attached to: its id, whether it is, why not, and the ids of the subtitles
// attached to it, with the version each follows.
func (f *v2Film) attachable(t *testing.T, token string) (string, bool, string, []string) {
	t.Helper()
	w := do(f.h, http.MethodGet, "/api/library/items/"+filmItem+"/subtitles", "", token)
	var answer struct {
		VersionID  *string            `json:"versionId"`
		Attachable bool               `json:"attachable"`
		Reason     string             `json:"reason"`
		Subtitles  []library.Subtitle `json:"subtitles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusOK {
		t.Fatalf("the title's subtitles: %d %s", w.Code, w.Body)
	}
	id := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	var attached []string
	for _, s := range answer.Subtitles {
		if s.Attached {
			attached = append(attached, s.ID+"@"+id(s.VersionID))
		}
	}
	return id(answer.VersionID), answer.Attachable, answer.Reason, attached
}

// A title taken in, its original in its version's folder and no package,
// plays from that original, and takes a subtitle as a packaged one does: the
// seam says so, its version the one taken in. The file goes into the subs/
// of that version, beside its record and its original, named by its bytes,
// and its row follows the version, labelled by how it was made, not default.
// A player lists it and plays it, and the title still plays its original.
// Removed, the subtitle takes its file with it, and the folder it made: the
// version is as it was taken in.
func TestASubtitleIsAttachedToAVersionTakenIn(t *testing.T) {
	f := takenFilm(t)
	addon := f.iss.Addon(t)
	if v, ok, why, attached := f.attachable(t, addon); v != filmVersion || !ok || why != "" || len(attached) != 0 {
		t.Fatalf("taken in: %s %v %q %v", v, ok, why, attached)
	}
	s := f.attachTo(t, filmItem, addon)
	path := f.germanFile(filmVersion)
	if b, err := os.ReadFile(path); err != nil || string(b) != germanVTT {
		t.Fatalf("the file at %s: %q %v", path, b, err)
	}
	if s.Language != "de" || s.Format != "webvtt" || s.Label == nil || *s.Label != "AI" || s.Origin == nil ||
		*s.Origin != "transcribed" || !s.Attached || s.VersionID == nil || *s.VersionID != filmVersion {
		t.Errorf("the subtitle: %+v", s)
	}
	sum := sha256.Sum256([]byte(germanVTT))
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1 AND item_id = $2
		AND path = $3 AND versionid = $4 AND origin = 'transcribed' AND label = 'AI' AND NOT isdefault AND NOT isforced
		AND NOT issdh AND sha256 = $5 AND attachedby = 'example-addon' AND attachedat IS NOT NULL`,
		s.ID, filmItem, path, filmVersion, hex.EncodeToString(sum[:])); n != 1 {
		t.Error("the row is not the subtitle attached to the version taken in")
	}
	vdir := library.VersionDir(f.itemDir, filmVersion)
	if got := strings.Join(fileNames(t, vdir), " "); got != "original.mkv subs version.json" {
		t.Errorf("the version taken in holds %s", got)
	}
	if got := strings.Join(fileNames(t, filepath.Join(vdir, "subs")), " "); got != filepath.Base(path) {
		t.Errorf("its subs/ holds %s", got)
	}
	if v, ok, _, attached := f.attachable(t, addon); v != filmVersion || !ok || strings.Join(attached, ",") != s.ID+"@"+filmVersion {
		t.Errorf("attached: %s %v %v", v, ok, attached)
	}

	viewer := f.iss.Viewer(t)
	if w := do(f.h, http.MethodGet, "/api/subtitles/items/"+filmItem, "", viewer); !strings.Contains(w.Body.String(), `"id":"`+s.ID+`"`) {
		t.Errorf("the title's subtitles: %s", w.Body)
	}
	if w := do(f.h, http.MethodGet, "/api/subtitles/"+s.ID, "", viewer); w.Code != http.StatusOK || w.Body.String() != germanVTT {
		t.Errorf("the subtitle served: %d %q", w.Code, w.Body)
	}
	original := filepath.Join(vdir, "original.mkv")
	if w := do(f.h, http.MethodGet, "/api/play/"+filmItem, "", viewer); w.Code != http.StatusOK ||
		w.Header().Get("Content-Disposition") != `inline; filename="original.mkv"` {
		t.Errorf("the title plays %d %q, want its original %s", w.Code, w.Header().Get("Content-Disposition"), original)
	}

	if w := do(f.h, http.MethodDelete, "/api/library/items/"+filmItem+"/subtitles/"+s.ID, "", addon); w.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(fileNames(t, vdir), " "); got != "original.mkv version.json" {
		t.Errorf("the version taken in holds %s once the subtitle is removed", got)
	}
	if v, ok, _, attached := f.attachable(t, addon); v != filmVersion || !ok || len(attached) != 0 {
		t.Errorf("removed: %s %v %v", v, ok, attached)
	}
}

// The package added to a version taken in keeps the version, its id and its
// folder, and with them a subtitle attached to it meanwhile: the package's
// files go in beside it, and packaging-complete leaves its row as it is
// (CarryAttached), the version it follows now complete, while the
// package's own subtitles are recorded. The chain the package closes does
// not list it, and holds with it, in full too.
func TestASubtitleAttachedWhileTakenInStaysWithItsVersion(t *testing.T) {
	f := takenFilm(t)
	addon := f.iss.Addon(t)
	s := f.attachTo(t, filmItem, addon)
	path, vdir := f.germanFile(filmVersion), library.VersionDir(f.itemDir, filmVersion)
	follows := func(what string) {
		t.Helper()
		if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1 AND path = $2
			AND versionid = $3 AND origin = 'transcribed'`, s.ID, path, filmVersion); n != 1 {
			t.Errorf("%s: the subtitle does not follow the version", what)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != germanVTT {
			t.Errorf("%s: its file %q %v", what, b, err)
		}
		if v, ok, _, attached := f.attachable(t, addon); v != filmVersion || !ok || strings.Join(attached, ",") != s.ID+"@"+filmVersion {
			t.Errorf("%s: %s %v %v", what, v, ok, attached)
		}
	}

	// The package added: the version is complete, with the subtitle.
	complete := f.versionOf(t, filmVersion, filmPackage, originalNamed)
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK || m["current"] != true {
		t.Fatalf("the package added: %d %v", code, m)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'complete'
		AND packageid = $2 AND dir = $3`, filmVersion, filmPackage, vdir); n != 1 {
		t.Error("the version is not complete with its package")
	}
	follows("the package added")
	if got, want := subtitleRows(t, f.st, filmItem), fmt.Sprintf(`s-de %[1]s/A Film (2024)/A Film (2024).de.srt de false false
%%s %[2]s/subs/0.vtt ger false false
%%s %[2]s/subs/1.vtt spa false true
%[3]s %[4]s de false false`, f.cfg.Roots(true).Arrivals, vdir, s.ID, path); !sameIgnoringIDs(got, want) {
		t.Errorf("the subtitles:\n%s\nwant:\n%s", got, want)
	}
	c, err := library.VerifyVersion(vdir, true)
	if err != nil {
		t.Fatalf("the version's chain: %v", err)
	}
	if _, listed := c.Listed["subs/"+filepath.Base(path)]; listed {
		t.Error("the chain lists the subtitle attached to the version")
	}
	if _, err := os.Stat(filepath.Join(vdir, "original.mkv")); err != nil {
		t.Errorf("the original: %v", err)
	}
	viewer := f.iss.Viewer(t)
	if w := do(f.h, http.MethodGet, "/api/subtitles/"+s.ID, "", viewer); w.Code != http.StatusOK || w.Body.String() != germanVTT {
		t.Errorf("the subtitle served: %d %q", w.Code, w.Body)
	}
}

// A subtitle attached while the title was taken in goes on as any other
// once its version is complete: a new version of the same file carries it
// over (its file copied into the new version's subs/, its row following
// it), and the version it was attached to is superseded.
func TestANewVersionOfTheSameFileCarriesWhatWasAttachedWhileTakenIn(t *testing.T) {
	f := takenFilm(t)
	addon := f.iss.Addon(t)
	s := f.attachTo(t, filmItem, addon)
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, f.versionOf(t, filmVersion, filmPackage, originalNamed))); code != http.StatusOK {
		t.Fatalf("the package added: %d %v", code, m)
	}
	const next, nextPkg = "77c10000-0000-4000-8000-000000000006", "88c10000-0000-4000-8000-000000000007"
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		next, filmItem, []string{filmSource})
	if code, m := f.complete(t, f.payload(next, nextPkg, f.version(t, next, nextPkg))); code != http.StatusOK || m["superseded"] != filmVersion {
		t.Fatalf("the next version: %d %v", code, m)
	}
	moved := f.germanFile(next)
	if b, err := os.ReadFile(moved); err != nil || string(b) != germanVTT {
		t.Fatalf("the file in the new version: %q %v", b, err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE id = $1 AND path = $2
		AND versionid = $3 AND label = 'AI'`, s.ID, moved, next); n != 1 {
		t.Error("the attached subtitle does not follow the new version")
	}
	if v, ok, _, attached := f.attachable(t, addon); v != next || !ok || strings.Join(attached, ",") != s.ID+"@"+next {
		t.Errorf("the new version: %s %v %v", v, ok, attached)
	}
	if _, err := library.VerifyVersion(library.VersionDir(f.itemDir, next), true); err != nil {
		t.Errorf("the new version's chain: %v", err)
	}
}

// The version taken in of a file the title plays no more (it was given
// another, which has no version yet) takes no subtitle: the seam says why,
// an attach is 409, and nothing is written into its folder. An episode
// another's file covers attaches to its holder's version taken in.
func TestOnlyTheVersionTakenInOfTheFileThatPlaysTakesASubtitle(t *testing.T) {
	f := takenFilm(t)
	addon := f.iss.Addon(t)
	const covered = "e2e2e2e2-0000-4000-8000-000000000002"
	storetest.AddItem(t, f.st, covered, "episode", "The Second Half", "")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET coveredby = $1 WHERE id = $2`, filmItem, covered)
	s := f.attachTo(t, covered, addon)
	if s.VersionID == nil || *s.VersionID != filmVersion {
		t.Errorf("a covered episode's subtitle: %+v", s)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE item_id = $1 AND versionid = $2
		AND origin IS NOT NULL`, filmItem, filmVersion); n != 1 {
		t.Error("the subtitle is not the holder's")
	}

	// Given another file.
	const other = "0b6c0000-0000-4000-8000-000000000013"
	arrival := f.cfg.Roots(true).Arrivals + "/A Film (2024)/A Film (2024) Remastered.mkv"
	librarytest.Write(t, arrival, []byte("another file"))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath, sizebytes, state)
		VALUES ($1, $2, 'A Film (2024) Remastered.mkv', $3, 'A Film (2024)/A Film (2024) Remastered.mkv', 12, 'present')`,
		other, filmItem, arrival)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_playbackassets SET path = $2, sourceid = $3 WHERE id = 'src-f1' AND item_id = $1`,
		filmItem, arrival, other)
	if v, ok, why, _ := f.attachable(t, addon); v != "" || ok || !strings.Contains(why, "no version in the library") {
		t.Errorf("given another file: %q %v %q", v, ok, why)
	}
	w := do(f.h, http.MethodPost, "/api/library/items/"+filmItem+"/subtitles",
		attachBody(t, map[string]any{"language": "en", "origin": "provider", "content": germanVTT}), addon)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no version in the library") {
		t.Errorf("an attach to it: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(fileNames(t, filepath.Join(library.VersionDir(f.itemDir, filmVersion), "subs")), " "); got != filepath.Base(f.germanFile(filmVersion)) {
		t.Errorf("the old file's version holds %s", got)
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
