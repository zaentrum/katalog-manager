package rest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// v2Film is a film of the library's v2 layout whose package the packager
// built: its source (recorded, its streams a picture, two sounds and two
// subtitles), the version the pipeline builds, and the catalog's rows of it
// from before (a legacy package's subtitle, a subtitle file beside the
// source).
type v2Film struct {
	st          *store.Store
	cfg         config.Config
	h           http.Handler
	svc         string
	itemDir     string
	source, ver string
}

const (
	filmSource  = "0b6c0000-0000-4000-8000-000000000003"
	filmVersion = "9a2e0000-0000-4000-8000-000000000002"
	filmPackage = "4f1d0000-0000-4000-8000-000000000005"
)

func newV2Film(t *testing.T) *v2Film {
	t.Helper()
	st := storetest.Open(t)
	v2Layout(t, st)
	dir := t.TempDir()
	cfg := v2Config(dir)
	h, iss := server(t, st, cfg)
	f := &v2Film{st: st, cfg: cfg, h: h, svc: iss.Service(t, "zaentrum-manager"), itemDir: dir + "/movies/f1/" + filmItem,
		source: filmSource, ver: filmVersion}
	original := cfg.Roots(true).Arrivals + "/A Film (2024)/A Film (2024).mkv"
	storetest.AddItem(t, st, filmItem, "movie", "A Film", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, sizebytes, state)
		VALUES ($1, $2, 'A Film (2024).mkv', $3, 1000, 'present')`, filmSource, filmItem, original)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, isprimary, sizebytes, sourceid)
		VALUES ('src-f1', $1, $2, true, 1000, $3)`, filmItem, original, filmSource)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		filmVersion, filmItem, []string{filmSource})
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label, isdefault) VALUES
		('s-de', $1, $2, 'srt', 'de', 'Deutsch', false),
		('old-pkg', $1, $3, 'webvtt', 'ger', '', true)`, filmItem, filepath.Dir(original)+"/A Film (2024).de.srt",
		cfg.PackagesRoot+"/movies/f1/"+filmItem+"/subs/0.vtt")
	librarytest.WriteSource(t, library.SourceDir(f.itemDir, filmSource), map[string]any{"sourceId": filmSource,
		"streams": []map[string]any{{"index": 0, "type": "video"}, {"index": 1, "type": "audio"}, {"index": 2, "type": "audio"},
			{"index": 3, "type": "subtitle"}, {"index": 4, "type": "subtitle"}}}, nil)
	return f
}

// version writes the version vid of package pid into the film's folder, and
// answers its .complete.
func (f *v2Film) version(t *testing.T, vid, pid string) string {
	t.Helper()
	return librarytest.WriteVersion(t, library.VersionDir(f.itemDir, vid), librarytest.Version{VersionID: vid, PackageID: pid,
		SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Package: map[string]any{
			"durationMs": 600000, "peakBandwidthBps": 4200000,
			"renditions": map[string]any{
				"video": []map[string]any{{"id": "v0", "dir": "hls/v0", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 800,
					"bitrateBps": 4000000}},
				"audio": []map[string]any{
					{"id": "a0", "dir": "hls/a0", "codec": "mp4a.40.2", "language": "eng", "title": "", "default": false, "channels": 2,
						"bitrateBps": 128000, "sourceStreamIndex": 2},
					{"id": "a1", "dir": "hls/a1", "codec": "mp4a.40.2", "language": "zxx", "title": "Music only", "default": true,
						"channels": 6, "bitrateBps": 96000, "sourceStreamIndex": 1}}},
			"subtitles": []map[string]any{
				{"id": "sub0", "path": "subs/0.vtt", "language": "ger", "title": "", "default": false, "forced": false,
					"format": "webvtt", "sourceStreamIndex": 4},
				{"id": "sub1", "path": "subs/1.vtt", "language": "spa", "title": "Signs", "name": "Spanish (signs)", "default": false,
					"forced": true, "format": "webvtt", "sourceStreamIndex": 3},
				{"id": "sub2", "path": "subs/2.vtt", "language": "deu", "title": "Deutsch", "default": false, "forced": false,
					"format": "webvtt", "fromSidecar": "sources/" + filmSource + "/A Film (2024).de.srt"}}}})
}

// payload is the packager's v2 payload for the version vid.
func (f *v2Film) payload(vid, pid, complete string) string {
	return fmt.Sprintf(`{"layout": "v2", "versionId": %q, "packageId": %q, "versionDir": %q, "complete": %q,
		"sourceId": %q, "sourceRecorded": true, "package": {},
		"sidecars": [{"subtitleAssetId": "s-de", "rendition": "sub2", "path": "subs/2.vtt"}],
		"source": {"codec": "h264", "width": 1920, "height": 818, "durationMs": 600100, "bitRate": 5000000}}`,
		vid, pid, library.VersionDir(f.itemDir, vid), complete, filmSource)
}

func (f *v2Film) complete(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	w := do(f.h, http.MethodPost, "/api/items/"+filmItem+"/packaging-complete", body, f.svc)
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return w.Code, m
}

// A version the packager built is taken: it is complete (its package, its
// folder, completed when its package was), its source recorded with the
// files it mapped, the packaged asset is its package.json with what its top
// rendition, default audio, peak and size say, the package's subtitles
// replace the package's before (each not default, labelled by its name else
// its title; the rendition of a file beside the source gets none, the file's
// row stays), and the source's tracks are recorded at their ordinals among
// the source's streams. Taken again, the same answer, nothing changed.
func TestPackagingCompleteTakesAVersion(t *testing.T) {
	f := newV2Film(t)
	complete := f.version(t, filmVersion, filmPackage)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	code, answer := f.complete(t, f.payload(filmVersion, filmPackage, complete))
	want := map[string]any{"itemId": filmItem, "versionId": filmVersion, "current": true, "superseded": nil,
		"packagedAssetWritten": true, "subtitlesWritten": 2.0, "audioTracks": 2.0}
	if code != http.StatusOK || fmt.Sprint(answer) != fmt.Sprint(want) {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	vdir := library.VersionDir(f.itemDir, filmVersion)
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'complete'
		AND packageid = $2 AND dir = $3 AND completedat = '2026-10-06T09:00:00Z'`, filmVersion, filmPackage, vdir); n != 1 {
		t.Error("the version is not complete as its package says")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemsources WHERE id = $1 AND recordedat IS NOT NULL
		AND recorddir = $2 AND sidecars = '[{"path": "subs/2.vtt", "rendition": "sub2", "subtitleAssetId": "s-de"}]'`,
		filmSource, library.SourceDir(f.itemDir, filmSource)); n != 1 {
		t.Error("the source is not recorded with the files it mapped")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND kind = 'packaged'
		AND path = $2 AND versionid = $3 AND codec = 'hvc1.1.6.L120.90' AND resolution = '1920x800' AND bitratekbps = 4200
		AND audiocodec = 'mp4a.40.2' AND audiolanguage = 'zxx' AND audiochannels = 6 AND audiobitratekbps = 96
		AND audiotrackcount = 2 AND subtitletrackcount = 3 AND durationms = 600000 AND sizebytes > 0`,
		filmItem, vdir+"/package.json", filmVersion); n != 1 {
		t.Error("the packaged asset is not the version's package")
	}
	if got, want := subtitleRows(t, f.st, filmItem), fmt.Sprintf(`s-de %[1]s/A Film (2024)/A Film (2024).de.srt de false false
%%s %[2]s/subs/0.vtt ger false false
%%s %[2]s/subs/1.vtt spa false true`, f.cfg.Roots(true).Arrivals, vdir); !sameIgnoringIDs(got, want) {
		t.Errorf("the subtitles:\n%s\nwant:\n%s", got, want)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE label = 'Spanish (signs)'`); n != 1 {
		t.Error("a subtitle is not labelled by its name")
	}
	if got, want := trackLinesOf(t, f.st, filmItem), "audio 0 zxx -\naudio 1 eng -\nsubtitle 0 spa -\nsubtitle 1 ger -"; got != want {
		t.Errorf("the source's tracks:\n%s\nwant:\n%s", got, want)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'src-f1' AND codec = 'h264'
		AND resolution = '1920x818'`); n != 1 {
		t.Error("the source's probe is not kept")
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_items WHERE modifiedat > '2001-01-01'`); n != 1 {
		t.Error("the item was not modified")
	}
	// Again: the same answer.
	rows := subtitleRows(t, f.st, filmItem)
	code, again := f.complete(t, f.payload(filmVersion, filmPackage, complete))
	if code != http.StatusOK || fmt.Sprint(again) != fmt.Sprint(want) {
		t.Errorf("taken again: %d %v", code, again)
	}
	if got := subtitleRows(t, f.st, filmItem); got != rows {
		t.Errorf("taken again, the subtitles changed:\n%s", got)
	}
}

// sameIgnoringIDs compares subtitle lines whose ids want leaves out (%s).
func sameIgnoringIDs(got, want string) bool {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if strings.HasPrefix(w[i], "%s ") {
			if _, rest, ok := strings.Cut(g[i], " "); !ok || rest != strings.TrimPrefix(w[i], "%s ") {
				return false
			}
			continue
		}
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// A version completes over the one that plays: the old one gets its
// package-superseded event first, naming its package and its successor, and
// is superseded by the new one, which plays. The answer names the old.
func TestPackagingCompleteSupersedesTheVersionThatPlays(t *testing.T) {
	f := newV2Film(t)
	const next, nextPkg = "77c10000-0000-4000-8000-000000000006", "88c10000-0000-4000-8000-000000000007"
	_, _ = f.complete(t, f.payload(filmVersion, filmPackage, f.version(t, filmVersion, filmPackage)))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state) VALUES ($1, $2, $3, 'building')`,
		next, filmItem, []string{filmSource})
	code, answer := f.complete(t, f.payload(next, nextPkg, f.version(t, next, nextPkg)))
	if code != http.StatusOK || answer["superseded"] != filmVersion {
		t.Fatalf("the next version: %d %v", code, answer)
	}
	var eventID string
	if err := f.st.Pool().QueryRow(t.Context(), `SELECT supersedeeventid FROM com_nalet_katalog_itemversions WHERE id = $1
		AND state = 'superseded' AND supersededby = $2 AND supersededat IS NOT NULL`, filmVersion, next).Scan(&eventID); err != nil {
		t.Fatalf("the old version is not superseded: %v", err)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'complete'`, next); n != 1 {
		t.Error("the new version does not play")
	}
	dir, err := library.FindEvent(f.itemDir, eventID, library.EventPackageSuperseded)
	if err != nil || dir == "" {
		t.Fatalf("no event %s: %v", eventID, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, library.EventFile))
	for _, s := range []string{`"versionId": "` + filmVersion + `"`, `"packageId": "` + filmPackage + `"`,
		`"supersededBy": {` + "\n" + `    "versionId": "` + next + `",` + "\n" + `    "packageId": "` + nextPkg + `"`} {
		if !strings.Contains(string(b), s) {
			t.Errorf("the event does not say %s:\n%s", s, b)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, library.SumsFile)); err != nil {
		t.Errorf("the event is not complete: %v", err)
	}
	// The old version's package is the item's no more.
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_subtitleassets WHERE path LIKE $1`,
		library.VersionDir(f.itemDir, filmVersion)+"/%"); n != 0 {
		t.Errorf("%d subtitles of the superseded package stay", n)
	}
}

// A stale run is refused (409): a version the item does not build, one
// superseded (its folder, which a session may still play, stays), a
// versionDir that is not the version's folder. A chain that does not hold is
// refused (422). The folder of a refused version the catalog does not keep
// is moved out of the record; a legacy manifest is refused with the v2
// layout.
func TestPackagingCompleteRefusals(t *testing.T) {
	f := newV2Film(t)
	complete := f.version(t, filmVersion, filmPackage)
	vdir := library.VersionDir(f.itemDir, filmVersion)
	for _, c := range []struct {
		name, body string
		status     int
		says       string
	}{
		{"another package", f.payload(filmVersion, "5a5a0000-0000-4000-8000-000000000009", complete), 422, "packageId"},
		{"another .complete", f.payload(filmVersion, filmPackage, "sha256:"+strings.Repeat("0", 64)), 422, ".complete holds"},
		{"another folder", strings.Replace(f.payload(filmVersion, filmPackage, complete), vdir, vdir+"x", 1), 409, "versionDir is"},
		{"a legacy manifest", `{"renditions": {"video": []}}`, 409, "the library's layout is v2"},
		{"no version", `{"layout": "v2", "packageId": "p"}`, 400, "names no versionId"},
	} {
		os.RemoveAll(vdir)
		if got := f.version(t, filmVersion, filmPackage); got != complete {
			t.Fatalf("the version was written again otherwise: %s", got)
		}
		code, m := f.complete(t, c.body)
		if code != c.status || !strings.Contains(fmt.Sprint(m["error"]), c.says) {
			t.Errorf("%s: %d %v, want %d saying %s", c.name, code, m, c.status, c.says)
		}
		// A refusal of the version being built moves its folder out; a body
		// that is no v2 payload names none.
		if _, err := os.Stat(vdir); (err == nil) != (code == http.StatusBadRequest || c.name == "a legacy manifest") {
			t.Errorf("%s: the version's folder in the record: %v", c.name, err)
		}
	}
	moved, _ := filepath.Glob(filepath.Join(f.cfg.Roots(true).Work, "legacy", "*", "refused", filmItem+"-"+filmVersion+"-*"))
	if len(moved) != 3 {
		t.Errorf("the refused version's folders in legacy/: %v, want the three refusals'", moved)
	}
	// Rebuilt, it is taken.
	complete = f.version(t, filmVersion, filmPackage)
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK {
		t.Fatalf("the rebuilt version: %d %v", code, m)
	}
	// An unknown version: refused, its folder moved out.
	const unknown, unknownPkg = "66660000-0000-4000-8000-000000000001", "66660000-0000-4000-8000-000000000002"
	uc := f.version(t, unknown, unknownPkg)
	if code, m := f.complete(t, f.payload(unknown, unknownPkg, uc)); code != http.StatusConflict ||
		!strings.Contains(fmt.Sprint(m["error"]), "is no version the item builds") {
		t.Errorf("an unknown version: %d %v", code, m)
	}
	if _, err := os.Stat(library.VersionDir(f.itemDir, unknown)); !os.IsNotExist(err) {
		t.Error("an unknown version's folder stays in the record")
	}
	// Superseded: refused, its folder stays.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, filmVersion)
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusConflict ||
		!strings.Contains(fmt.Sprint(m["error"]), "is superseded") {
		t.Errorf("a superseded version: %d %v", code, m)
	}
	if _, err := os.Stat(vdir); err != nil {
		t.Errorf("a superseded version's folder was moved: %v", err)
	}
	// Without its source's record, or with version.json naming another
	// source: a broken chain.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'building' WHERE id = $1`, filmVersion)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_itemversions WHERE id <> $1`, filmVersion)
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_playbackassets WHERE kind = 'packaged'`)
	os.Remove(filepath.Join(library.SourceDir(f.itemDir, filmSource), library.SumsFile))
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusUnprocessableEntity ||
		!strings.Contains(fmt.Sprint(m["error"]), "the source's record") {
		t.Errorf("without the source's record: %d %v", code, m)
	}
}

// An extra's folder the packager built is taken: recorded, ready, its folder
// its package's and its record's, with what its top rendition and its
// package say. Again, the same; another package of it is refused (written
// once); a broken one is refused and moved out of the record.
func TestPackagingCompleteTakesAnExtra(t *testing.T) {
	f := newV2Film(t)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, sourcepath, state)
		VALUES ($1, $2, 'trailer', 'Trailer', 'api', '/extras/trailer.mov', 'packaging')`, extraX1, filmItem)
	xdir := library.ExtraDir(f.itemDir, extraX1)
	const xpkg = "5e1d0000-0000-4000-8000-000000000001"
	write := func(pid string) string {
		os.RemoveAll(xdir)
		return librarytest.WriteExtra(t, xdir, librarytest.Extra{ExtraID: extraX1, PackageID: pid, CreatedAt: "2026-10-06T09:00:00Z",
			Package: map[string]any{"durationMs": 52000, "peakBandwidthBps": 2500000, "renditions": map[string]any{"video": []map[string]any{
				{"id": "v0", "codec": "hvc1.1.6.L93.90", "width": 1280, "height": 720}, {"id": "v1", "codec": "hvc1.1.6.L120.90", "width": 1920, "height": 1080}}}}})
	}
	body := func(pid, complete, dir string) string {
		return fmt.Sprintf(`{"layout": "v2", "extraId": %q, "extraDir": %q, "packageId": %q, "complete": %q, "package": {}, "source": {}}`,
			extraX1, dir, pid, complete)
	}
	post := func(b string) (int, string) {
		w := do(f.h, http.MethodPost, "/api/extras/"+extraX1+"/packaging-complete", b, f.svc)
		return w.Code, w.Body.String()
	}
	complete := write(xpkg)
	if code, b := post(body(xpkg, complete, xdir)); code != http.StatusOK || !strings.Contains(b, `"durationMs":52000`) {
		t.Fatalf("the extra: %d %s", code, b)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemextras WHERE id = $1 AND state = 'ready'
		AND packagepath = $2 AND recordpath = $2 AND recordedat IS NOT NULL AND packageid = $3 AND videocodec = 'hvc1.1.6.L120.90'
		AND width = 1920 AND height = 1080 AND peakbandwidthbps = 2500000 AND packagesizebytes > 0`, extraX1, xdir, xpkg); n != 1 {
		t.Error("the extra is not recorded as its package says")
	}
	if code, b := post(body(xpkg, complete, xdir)); code != http.StatusOK {
		t.Errorf("again: %d %s", code, b)
	}
	if code, b := post(body("5e1d0000-0000-4000-8000-000000000002", complete, xdir)); code != http.StatusConflict ||
		!strings.Contains(b, "written once") {
		t.Errorf("another package of a recorded extra: %d %s", code, b)
	}
	if code, b := post(body(xpkg, complete, xdir+"x")); code != http.StatusConflict || !strings.Contains(b, "extraDir is") {
		t.Errorf("another folder: %d %s", code, b)
	}
	if _, err := os.Stat(xdir); err != nil {
		t.Errorf("a recorded extra's folder was moved: %v", err)
	}
	// Not recorded and broken: refused, moved out.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemextras SET recordedat = NULL, packageid = NULL WHERE id = $1`, extraX1)
	if code, b := post(body(xpkg, "sha256:"+strings.Repeat("1", 64), xdir)); code != http.StatusUnprocessableEntity {
		t.Errorf("a broken chain: %d %s", code, b)
	}
	if _, err := os.Stat(xdir); !os.IsNotExist(err) {
		t.Errorf("a refused extra's folder stays in the record: %v", err)
	}
}
