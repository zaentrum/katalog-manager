package library

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A movie packaged from its original, waiting for the retire job.
const (
	rtFilm    = "5e0a1c2d-3b4f-4a6e-8d9c-0b1a2c3d4e5f"
	rtSource  = "0b6c7d8e-9f0a-4b1c-8d2e-3f4a5b6c7d8e"
	rtVersion = "9a2e3f4a-5b6c-4d7e-8f9a-0b1c2d3e4f5a"
	rtPackage = "4f1d2e3f-4a5b-4c6d-8e7f-8a9b0c1d2e3f"
)

// retireFixture is a library with a movie whose version is complete and whose
// original waits in the arrivals, with its sidecars: an English subtitle the
// package made a rendition of, a German one the source's record keeps a copy
// of, and an .nfo the record lists.
type retireFixture struct {
	st                             *store.Store
	cfg                            config.Config
	p                              Paths
	r                              *Retirer
	itemDir, versionDir, sourceDir string
	folder, original               string
	en, de, nfo                    string
	src, pkg                       map[string]any // the essences of the records addMovie writes
}

// The essences of the fixture's records, as the deletion gate reads them:
// a 7.1 original, and a package with its 5.1 companion (stereo and the 5.1).
var (
	sevenOne = map[string]any{"surround": true, "maxAudioChannels": 8, "audioLanguages": []string{"en"},
		"subtitleLanguages": []string{"en", "de"}}
	withFiveOne = map[string]any{"surround": true, "maxAudioChannels": 6, "audioLanguages": []string{"en"},
		"subtitleLanguages": []string{"en", "de"}}
	fiveOne = withFiveOne
	stereo  = map[string]any{"surround": false, "maxAudioChannels": 2, "audioLanguages": []string{"en"},
		"subtitleLanguages": []string{"en", "de"}}
)

func newRetireFixture(t *testing.T) *retireFixture {
	t.Helper()
	return newRetireFixtureOf(t, sevenOne, withFiveOne)
}

// newRetireFixtureOf is the fixture with its original's essence src and its
// package's pkg.
func newRetireFixtureOf(t *testing.T, src, pkg map[string]any) *retireFixture {
	t.Helper()
	st := storetest.Open(t)
	root := t.TempDir()
	cfg := config.Config{LibraryRoot: root, PackagesRoot: filepath.Join(root, "packages"), NFSRoot: filepath.Join(root, "media")}
	f := &retireFixture{st: st, cfg: cfg, p: PathsOf(cfg)}
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('s1', 'library.layout', 'v2'), ('s2', 'library.originals', 'delete-after-package')`)
	f.r = NewRetirer(st.Pool(), cfg, processing.New(st.Pool()))
	f.src, f.pkg = src, pkg
	f.addMovie(t, rtFilm, rtSource, rtVersion, rtPackage, "Example Film")
	f.itemDir = f.p.MovieDir(rtFilm)
	f.versionDir = VersionDir(f.itemDir, rtVersion)
	f.sourceDir = SourceDir(f.itemDir, rtSource)
	f.folder = filepath.Join(f.p.Arrivals, "Example Film (2020)")
	f.original = filepath.Join(f.folder, "Example Film.mkv")
	f.en = filepath.Join(f.folder, "Example Film.en.srt")
	f.de = filepath.Join(f.folder, "Example Film.de.srt")
	f.nfo = filepath.Join(f.folder, "Example Film.nfo")
	return f
}

// addMovie gives the fixture's library a movie, title, packaged from its
// original in the arrivals into its complete version an hour ago, every step
// that read the original done.
func (f *retireFixture) addMovie(t *testing.T, item, source, version, pkg, title string) {
	t.Helper()
	ctx := context.Background()
	storetest.AddItem(t, f.st, item, "movie", title, "")
	itemDir := f.p.MovieDir(item)
	versionDir, sourceDir := VersionDir(itemDir, version), SourceDir(itemDir, source)
	folder := filepath.Join(f.p.Arrivals, title+" (2020)")
	original := filepath.Join(folder, title+".mkv")
	librarytest.Write(t, original, bytes.Repeat([]byte(title+" "), 40000))
	sidecars := map[string]string{title + ".en.srt": "1\n00:00:01,000 --> 00:00:02,000\nHello\n",
		title + ".de.srt": "1\n00:00:01,000 --> 00:00:02,000\nHallo\n", title + ".nfo": "<movie/>\n"}
	for name, content := range sidecars {
		librarytest.Write(t, filepath.Join(folder, name), []byte(content))
	}
	size, qh1, err := QH1(original)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemsources (id, item_id, filename, arrivalpath, librarypath,
			sizebytes, qh1, state, recordedat, recorddir, sidecars)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'present', now() - interval '1 hour', $8,
			jsonb_build_array(jsonb_build_object('subtitleAssetId', $9::text, 'rendition', 'sub0', 'path', 'subs/0.vtt')))`,
		source, item, title+".mkv", original, title+" (2020)/"+title+".mkv", size, qh1, sourceDir, "en-"+item[:8])
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`, version, item, source, pkg, versionDir)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_playbackassets (id, item_id, path, codec, resolution, sizebytes,
			isprimary, kind, sourceid) VALUES ($1, $2, $3, 'h264', '1920x1080', $4, true, 'primary', $5),
			($6, $2, $7, 'hevc', '1920x1080', 1000, false, 'packaged', NULL)`,
		"orig-"+item[:8], item, original, size, source, "pkg-"+item[:8], filepath.Join(versionDir, PackageFile))
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang, label) VALUES
		($1, $3, $4, 'srt', 'en', 'English'), ($2, $3, $5, 'srt', 'de', 'Deutsch'), ($6, $3, $7, 'webvtt', 'fr', 'Français')`,
		"en-"+item[:8], "de-"+item[:8], item, filepath.Join(folder, title+".en.srt"), filepath.Join(folder, title+".de.srt"),
		"fr-"+item[:8], filepath.Join(versionDir, "subs/1.vtt"))
	steps := processing.New(f.st.Pool())
	for _, step := range processing.OriginalSteps {
		if err := steps.Upsert(ctx, item, step, processing.StatusDone, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	librarytest.WriteVersion(t, versionDir, librarytest.Version{VersionID: version, PackageID: pkg, SourceIDs: []string{source},
		CreatedAt: "2026-10-06T10:00:00Z", Package: map[string]any{"essence": f.pkg}})
	copies := map[string]string{}
	var listed []any
	for _, name := range []string{title + ".de.srt", title + ".en.srt", title + ".nfo"} {
		copies[name] = sidecars[name]
		kind := "subtitle"
		if strings.HasSuffix(name, ".nfo") {
			kind = "nfo"
		}
		listed = append(listed, map[string]any{"file": "sources/" + source + "/" + name, "originalName": name, "kind": kind})
	}
	librarytest.WriteSource(t, sourceDir, map[string]any{"sourceId": source, "file": map[string]any{"name": title + ".mkv",
		"sizeBytes": size, "fixity": map[string]any{"qh1": qh1}}, "sidecars": listed, "essence": f.src}, copies)
}

// set sets a library setting.
func (f *retireFixture) set(t *testing.T, key, value string) {
	t.Helper()
	storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_settings WHERE key = $1`, key)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES (gen_random_uuid()::varchar, $1, $2)`,
		key, value)
}

// pass runs a pass of the retire job, which must do without an error.
func (f *retireFixture) pass(t *testing.T) Report {
	t.Helper()
	rep, err := f.r.Pass(context.Background())
	if err != nil {
		t.Fatalf("the pass: %v", err)
	}
	return rep
}

// source reads the fixture's source as the catalog holds it.
func (f *retireFixture) source(t *testing.T, id string) *Source {
	t.Helper()
	s, err := SourceByID(context.Background(), f.st.Pool(), id)
	if err != nil || s == nil {
		t.Fatalf("source %s: %v", id, err)
	}
	return s
}

// step is the item's retire step: its status, failures, error and details.
func (f *retireFixture) step(t *testing.T, item string) string {
	t.Helper()
	var out string
	err := f.st.Pool().QueryRow(context.Background(), `SELECT status || ' ' || failures || ' error=' || COALESCE(error, '-') ||
		' details=' || COALESCE(details, '-') FROM com_nalet_katalog_itemprocessingsteps WHERE item_id = $1 AND step = 'retire'`,
		item).Scan(&out)
	if err != nil {
		return "none"
	}
	return out
}

// files lists the files under dir, relative to it, sorted.
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// kept checks that nothing of the original was deleted: it and its sidecars
// are where they arrived, the source is present, no event is recorded and
// the trash holds nothing.
func (f *retireFixture) kept(t *testing.T) {
	t.Helper()
	for _, path := range []string{f.original, f.en, f.de, f.nfo} {
		if !exists(path) {
			t.Errorf("%s is gone", path)
		}
	}
	if s := f.source(t, rtSource); s.State != SourcePresent || s.RetireEventID != nil || s.TrashPath != nil {
		t.Errorf("the source: %s, event %v, trash %v; want present, neither", s.State, s.RetireEventID, s.TrashPath)
	}
	if evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events")); len(evs) > 0 {
		t.Errorf("events recorded: %v", evs)
	}
	if got := files(t, filepath.Join(f.p.Work, WorkTrash)); len(got) > 0 {
		t.Errorf("the trash holds %v", got)
	}
}

// With the v2 layout and library.originals=delete-after-package, the retire
// job deletes the original of a version complete for the delay: verified in
// full, its event recorded with what the package does not carry of it (a
// 7.1 original's two channels its 5.1 companion has not), the
// original and every sidecar the scanner paired or the source's record lists
// moved to the trash, and the arrival folder pruned. The catalog says it is
// deleted: the playback row is an original's in its record, the subtitle row
// of the sidecar the package made a rendition of points at the rendition, the
// other at its copy in the record, both with their ids; the retire step is
// done, saying what was lost, and the version is verified in full.
func TestTheRetireJobDeletesAnOriginalAfterPackaging(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	rep := f.pass(t)
	if rep.Originals != 1 || rep.Failed != 0 {
		t.Fatalf("the pass: %+v", rep)
	}
	s := f.source(t, rtSource)
	if s.State != SourceDeleted || s.ArrivalPath != nil || s.Error != nil || deref(s.DeletedBy) != RetiredBy ||
		s.DeletedAt == nil || s.RetireEventID == nil || s.RetireEventAt == nil {
		t.Fatalf("the source: %+v", s)
	}
	if string(s.Lost) != `["maxAudioChannels"]` {
		t.Errorf("lost: %s", s.Lost)
	}
	trash := f.p.TrashDir(*s.RetireEventAt, rtSource)
	if deref(s.TrashPath) != trash {
		t.Errorf("the trash: %s, want %s", deref(s.TrashPath), trash)
	}
	want := []string{"Example Film.de.srt", "Example Film.en.srt", "Example Film.mkv", "Example Film.nfo"}
	if got := files(t, trash); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the trash holds %v, want %v", got, want)
	}
	if exists(f.folder) || !exists(f.p.Arrivals) {
		t.Errorf("the arrival folder is not pruned up to the arrivals: %v, %v", exists(f.folder), exists(f.p.Arrivals))
	}
	dir, err := FindEvent(f.itemDir, *s.RetireEventID, EventOriginalDeleted)
	if err != nil || dir == "" || !exists(filepath.Join(dir, SumsFile)) {
		t.Fatalf("the event: %q, %v", dir, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, EventFile))
	wantEvent := `{
  "schema": "zaentrum.library.event/2",
  "eventId": "` + *s.RetireEventID + `",
  "at": "` + Timestamp(*s.RetireEventAt) + `",
  "by": "katalog-manager (library.originals=delete-after-package)",
  "kind": "original-deleted",
  "versionId": "` + rtVersion + `",
  "sourceId": "` + rtSource + `",
  "reason": "originals are not kept: the package is the record",
  "accepted": [
    "maxAudioChannels"
  ]
}
`
	if string(b) != wantEvent {
		t.Errorf("event.json:\n%s\nwant:\n%s", b, wantEvent)
	}
	var primary bool
	var kind, path string
	if err := f.st.Pool().QueryRow(ctx, `SELECT isprimary, kind, path FROM com_nalet_katalog_playbackassets WHERE id = $1`,
		"orig-"+rtFilm[:8]).Scan(&primary, &kind, &path); err != nil || primary || kind != "original" || path != f.sourceDir {
		t.Errorf("the original's playback row: %v %s %s, %v", primary, kind, path, err)
	}
	for id, want := range map[string]string{
		"en-" + rtFilm[:8]: filepath.Join(f.versionDir, "subs/0.vtt") + " webvtt",
		"de-" + rtFilm[:8]: filepath.Join(f.sourceDir, "Example Film.de.srt") + " srt",
		"fr-" + rtFilm[:8]: filepath.Join(f.versionDir, "subs/1.vtt") + " webvtt",
	} {
		var got string
		if err := f.st.Pool().QueryRow(ctx, `SELECT path || ' ' || format FROM com_nalet_katalog_subtitleassets WHERE id = $1`,
			id).Scan(&got); err != nil || got != want {
			t.Errorf("subtitle row %s: %q, %v; want %q", id, got, err, want)
		}
	}
	if got := f.step(t, rtFilm); got != "done 0 error=- details=lost: maxAudioChannels" {
		t.Errorf("the retire step: %s", got)
	}
	v, _ := VersionByID(ctx, f.st.Pool(), rtVersion)
	if v == nil || deref(v.VerifiedLevel) != VerifyFull || v.VerifiedAt == nil {
		t.Errorf("the version's verification: %+v", v)
	}
	// Done: a pass again does nothing.
	if rep := f.pass(t); rep != (Report{}) {
		t.Errorf("a pass again: %+v", rep)
	}
}

// Nothing is retired before every condition holds: the layout is v2, the
// policy deletes originals, the title is not held, its version has been
// complete for the delay, every step that reads the original is over (a
// failed one with no attempt left is), and its retire step has not failed
// with its retry still to come. Then it is.
func TestTheRetireJobWaitsForItsConditions(t *testing.T) {
	for _, c := range []struct {
		name   string
		setup  func(t *testing.T, f *retireFixture)
		retire bool
	}{
		{"the legacy layout", func(t *testing.T, f *retireFixture) { f.set(t, SettingLayout, "legacy") }, false},
		{"originals kept", func(t *testing.T, f *retireFixture) { f.set(t, SettingOriginals, "keep") }, false},
		{"no policy set", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `DELETE FROM com_nalet_katalog_settings WHERE key = 'library.originals'`)
		}, false},
		{"the title held", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true`)
		}, false},
		{"the delay not over", func(t *testing.T, f *retireFixture) { f.set(t, SettingRetireDelay, "2h") }, false},
		{"a step waiting", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'pending' WHERE step = 'subtitle'`)
		}, false},
		{"a step running", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'in_progress' WHERE step = 'chapter'`)
		}, false},
		{"a step to be retried", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', failures = 1,
				nextretryat = now() + interval '1 minute' WHERE step = 'transcode'`)
		}, false},
		{"its retire step failed, its retry to come", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, failures, nextretryat)
				VALUES ('r', $1, 'retire', 'failed', 1, now() + interval '1 minute')`, rtFilm)
		}, false},
		{"its retire step failed with no attempt left", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, failures)
				VALUES ('r', $1, 'retire', 'failed', 3)`, rtFilm)
		}, false},
		{"its version superseded", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededat = now()`)
		}, false},
		{"a step failed with no attempt left", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', failures = 3, nextretryat = NULL
				WHERE step = 'silence'`)
		}, true},
		{"its retire step's retry due", func(t *testing.T, f *retireFixture) {
			storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status, failures, nextretryat)
				VALUES ('r', $1, 'retire', 'failed', 1, now() - interval '1 second')`, rtFilm)
		}, true},
		{"steps that do not read the original", func(t *testing.T, f *retireFixture) {
			for _, step := range []string{"tmdb", "tidb"} {
				storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status)
					VALUES (gen_random_uuid()::varchar, $1, $2, 'pending')`, rtFilm, step)
			}
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			c.setup(t, f)
			rep := f.pass(t)
			if c.retire {
				if rep.Originals != 1 || f.source(t, rtSource).State != SourceDeleted {
					t.Errorf("not retired: %+v", rep)
				}
				return
			}
			if rep.Originals != 0 {
				t.Errorf("retired: %+v", rep)
			}
			f.kept(t)
		})
	}
}

// A version that does not verify keeps its original: the retire step fails
// naming the file, the source is present again saying so, and nothing is
// deleted, nor any event recorded. A full verification within
// library.verify.maxAge has the chain do, which does not read every byte;
// the chain finds a file missing.
func TestAVersionThatDoesNotVerifyKeepsItsOriginal(t *testing.T) {
	f := newRetireFixture(t)
	seg := filepath.Join(f.versionDir, "hls/v0/seg0.m4s")
	b, _ := os.ReadFile(seg)
	b[0] ^= 0xff
	librarytest.Write(t, seg, b)
	if _, err := f.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "seg0.m4s") {
		t.Errorf("the pass: %v; want the segment named", err)
	}
	f.kept(t)
	s := f.source(t, rtSource)
	if s.Error == nil || !strings.Contains(*s.Error, "version "+rtVersion+" does not verify (full): "+seg+": its hash is") ||
		!strings.HasSuffix(*s.Error, "the original is kept") {
		t.Errorf("the source's error: %v", deref(s.Error))
	}
	if got := f.step(t, rtFilm); !strings.HasPrefix(got, "failed 1 error=version "+rtVersion+" does not verify (full): "+seg) {
		t.Errorf("the retire step: %s", got)
	}

	// Verified in full within the age: the chain does, and the segment's
	// bytes are not read.
	f2 := newRetireFixture(t)
	librarytest.Write(t, filepath.Join(f2.versionDir, "hls/v0/seg0.m4s"), b)
	storetest.Exec(t, f2.st, `UPDATE com_nalet_katalog_itemversions SET verifiedlevel = 'full', verifiedat = now() - interval '1 day'`)
	if rep := f2.pass(t); rep.Originals != 1 {
		t.Errorf("verified in full a day ago: %+v", rep)
	}
	v, _ := VersionByID(context.Background(), f2.st.Pool(), rtVersion)
	if deref(v.VerifiedLevel) != VerifyFull || v.VerifiedAt == nil || time.Since(*v.VerifiedAt) < 23*time.Hour {
		t.Errorf("the chain verified keeps the full verification: %v %v", deref(v.VerifiedLevel), v.VerifiedAt)
	}

	// The chain finds what is missing.
	f3 := newRetireFixture(t)
	storetest.Exec(t, f3.st, `UPDATE com_nalet_katalog_itemversions SET verifiedlevel = 'full', verifiedat = now()`)
	if err := os.Remove(filepath.Join(f3.versionDir, "hls/a0/seg0.m4s")); err != nil {
		t.Fatal(err)
	}
	if _, err := f3.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "does not verify (chain)") {
		t.Errorf("a file missing: %v", err)
	}
	f3.kept(t)
}

// An original that is not the file recorded (another size or quick hash)
// is kept: the retire step fails saying it changed since it was recorded. A
// subtitle file beside it that neither the package nor the source's record
// holds keeps it too: deleting it would lose the subtitle.
func TestAnOriginalNotAsRecordedIsKept(t *testing.T) {
	f := newRetireFixture(t)
	fh, err := os.OpenFile(f.original, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString("more")
	_ = fh.Close()
	if _, err := f.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), "the original changed since it was recorded: "+f.original) {
		t.Errorf("the pass: %v", err)
	}
	f.kept(t)
	if got := f.step(t, rtFilm); !strings.HasPrefix(got, "failed 1 error=the original changed since it was recorded: ") ||
		!strings.Contains(got, "; nothing is deleted") {
		t.Errorf("the retire step: %s", got)
	}

	f2 := newRetireFixture(t)
	fr := filepath.Join(f2.folder, "Example Film.fr.srt")
	librarytest.Write(t, fr, []byte("1\n00:00:01,000 --> 00:00:02,000\nBonjour\n"))
	storetest.Exec(t, f2.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang) VALUES ('late', $1, $2, 'srt', 'fr')`,
		rtFilm, fr)
	if _, err := f2.r.Pass(context.Background()); err == nil || !strings.Contains(err.Error(),
		"the subtitle file "+fr+" beside the original is in neither the package of version "+rtVersion+" nor the source's record") {
		t.Errorf("a subtitle paired after packaging: %v", err)
	}
	f2.kept(t)
	if !exists(fr) {
		t.Error("the late subtitle is gone")
	}
}

// A retirement a crash stopped resumes where it stopped: after the claim (it
// is verified and its event recorded then), after the event (which is not
// written again, nor the version verified again), and after the files moved
// to the trash (only the catalog is left to note). Before its event is
// recorded, a retirement is called off when the policy keeps originals
// again: the source is present, the retire step waits, and an event folder
// begun and not recorded goes.
func TestARetirementResumesWhereACrashStoppedIt(t *testing.T) {
	ctx := context.Background()
	prepare := func(t *testing.T, f *retireFixture, upTo string) *retirement {
		t.Helper()
		s := f.source(t, rtSource)
		rt := &retirement{src: s, itemDir: f.itemDir}
		set, _ := ReadSettings(ctx, f.st.Pool())
		now := time.Now().UTC()
		if upTo == "claim" {
			v, _ := Current(ctx, f.st.Pool(), rtFilm)
			rt.ver = v
			if ok, err := f.r.claim(ctx, f.p, now, rt); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			return rt
		}
		if err := f.r.prepare(ctx, f.p, set, now, rt); err != nil || rt.src == nil {
			t.Fatalf("prepare: %v", err)
		}
		if upTo == "move" {
			if err := f.r.deleteFiles(f.p, set, rt); err != nil {
				t.Fatal(err)
			}
		}
		return rt
	}
	for _, upTo := range []string{"claim", "event", "move"} {
		t.Run("after the "+upTo, func(t *testing.T) {
			f := newRetireFixture(t)
			rt := prepare(t, f, upTo)
			if s := f.source(t, rtSource); s.State != SourceRetiring {
				t.Fatalf("the source after the %s: %s", upTo, s.State)
			}
			if got := f.step(t, rtFilm); !strings.HasPrefix(got, "in_progress ") {
				t.Errorf("the retire step after the %s: %s", upTo, got)
			}
			var stamp time.Time
			if upTo != "claim" {
				dir, _ := FindEvent(f.itemDir, *rt.src.RetireEventID, EventOriginalDeleted)
				fi, err := os.Stat(filepath.Join(dir, EventFile))
				if err != nil {
					t.Fatal(err)
				}
				stamp = fi.ModTime()
				// Recorded: the version is not verified again.
				librarytest.Write(t, filepath.Join(f.versionDir, "hls/v0/seg0.m4s"), []byte("not the segment"))
			}
			if rep := f.pass(t); rep.Originals != 1 {
				t.Fatalf("the pass: %+v", rep)
			}
			s := f.source(t, rtSource)
			if s.State != SourceDeleted || *s.RetireEventID != *rt.src.RetireEventID {
				t.Errorf("the source: %s, event %s; want deleted, %s", s.State, deref(s.RetireEventID), *rt.src.RetireEventID)
			}
			evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events"))
			if len(evs) != 1 {
				t.Errorf("events: %v", evs)
			}
			if upTo != "claim" {
				dir, _ := FindEvent(f.itemDir, *s.RetireEventID, EventOriginalDeleted)
				if fi, _ := os.Stat(filepath.Join(dir, EventFile)); !fi.ModTime().Equal(stamp) {
					t.Error("the event was written again")
				}
			}
			if got := files(t, deref(s.TrashPath)); len(got) != 4 {
				t.Errorf("the trash: %v", got)
			}
			if got := f.step(t, rtFilm); got != "done 0 error=- details=lost: maxAudioChannels" {
				t.Errorf("the retire step: %s", got)
			}
		})
	}

	t.Run("called off", func(t *testing.T) {
		f := newRetireFixture(t)
		rt := prepare(t, f, "claim")
		begun := EventDir(f.itemDir, *rt.src.RetireEventAt, *rt.src.RetireEventID, EventOriginalDeleted)
		librarytest.Write(t, filepath.Join(begun, EventFile), []byte("{}\n"))
		f.set(t, SettingOriginals, "keep")
		if rep := f.pass(t); rep.Originals != 0 {
			t.Errorf("the pass: %+v", rep)
		}
		f.kept(t)
		if got := f.step(t, rtFilm); got != "pending 0 error=- details=library.originals is keep: the original is kept" {
			t.Errorf("the retire step: %s", got)
		}
		// On again: it is retired as any other.
		f.set(t, SettingOriginals, "delete-after-package")
		if rep := f.pass(t); rep.Originals != 1 {
			t.Errorf("the pass with the policy on again: %+v", rep)
		}
	})

	t.Run("recorded, not called off", func(t *testing.T) {
		f := newRetireFixture(t)
		prepare(t, f, "event")
		f.set(t, SettingOriginals, "keep")
		// A subtitle paired since, which the record does not hold, stays.
		fr := filepath.Join(f.folder, "Example Film.fr.srt")
		librarytest.Write(t, fr, []byte("1\n00:00:01,000 --> 00:00:02,000\nBonjour\n"))
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_subtitleassets (id, item_id, path, format, lang) VALUES ('late', $1, $2, 'srt', 'fr')`,
			rtFilm, fr)
		if rep := f.pass(t); rep.Originals != 1 {
			t.Errorf("a recorded deletion goes on whatever the policy: %+v", rep)
		}
		if !exists(fr) || exists(f.original) {
			t.Errorf("the late subtitle there: %v, the original: %v", exists(fr), exists(f.original))
		}
	})
}

// The trash's days go once library.trash.grace is over after their day, and
// the legacy folder's once library.superseded.grace is; a trash grace of 0
// unlinks an original at once, and nothing goes to the trash.
func TestTheTrashIsEmptiedAfterItsGrace(t *testing.T) {
	f := newRetireFixture(t)
	f.set(t, SettingOriginals, "keep")
	now := time.Now().UTC()
	old, recent := now.Add(-50*time.Hour), now.Add(-time.Hour)
	for _, dir := range []string{f.p.TrashDir(old, "a"), f.p.TrashDir(recent, "b"), filepath.Join(f.p.LegacyDay(old), "refused", "x"),
		filepath.Join(f.p.LegacyDay(recent), "refused", "y")} {
		librarytest.Write(t, filepath.Join(dir, "file"), []byte("x"))
	}
	librarytest.Write(t, filepath.Join(f.p.Work, WorkTrash, "not-a-day", "file"), []byte("x"))
	rep := f.pass(t)
	if rep.Purged != 2 {
		t.Errorf("purged %d days, want the trash's and the legacy folder's old ones", rep.Purged)
	}
	for dir, want := range map[string]bool{f.p.TrashDay(old): false, f.p.TrashDay(recent): true,
		f.p.LegacyDay(old): false, f.p.LegacyDay(recent): true, filepath.Join(f.p.Work, WorkTrash, "not-a-day"): true} {
		if exists(dir) != want {
			t.Errorf("%s there: %v, want %v", dir, exists(dir), want)
		}
	}

	g := newRetireFixture(t)
	g.set(t, SettingTrashGrace, "0")
	if rep := g.pass(t); rep.Originals != 1 {
		t.Fatalf("the pass: %+v", rep)
	}
	for _, path := range []string{g.original, g.en, g.de, g.nfo} {
		if exists(path) {
			t.Errorf("%s is there", path)
		}
	}
	if got := files(t, filepath.Join(g.p.Work, WorkTrash)); len(got) > 0 {
		t.Errorf("the trash holds %v", got)
	}
	if s := g.source(t, rtSource); s.State != SourceDeleted {
		t.Errorf("the source: %s", s.State)
	}
}

// A superseded version is removed once library.superseded.grace is over: its
// removal noted, the version-removed event recorded, its folder deleted;
// one within the grace stays. A removal a crash stopped goes on. A legacy
// package folder of an item whose version has been complete for the grace
// is moved to the legacy folder.
func TestASupersededVersionIsRemovedAfterItsGrace(t *testing.T) {
	f := newRetireFixture(t)
	f.set(t, SettingOriginals, "keep")
	ctx := context.Background()
	const old, recent = "11111111-2222-4333-8444-555555555555", "66666666-7777-4888-8999-aaaaaaaaaaaa"
	for _, c := range []struct {
		id, ago string
	}{{old, "2 days"}, {recent, "1 hour"}} {
		dir := VersionDir(f.itemDir, c.id)
		librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: c.id, PackageID: NewID(), SourceIDs: []string{rtSource},
			CreatedAt: "2026-10-01T10:00:00Z"})
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir,
				completedat, supersededby, supersededat)
			VALUES ($1, $2, ARRAY[$3::varchar], 'superseded', $7, $4, now() - interval '3 days', $5,
				now() - $6::interval)`, c.id, rtFilm, rtSource, dir, rtVersion, c.ago, "p-"+c.id[:8])
	}
	legacy := LegacyPackageDir(f.cfg.PackagesRoot, "movie", rtFilm)
	librarytest.Write(t, filepath.Join(legacy, "manifest.json"), []byte("{}\n"))
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '2 days' WHERE id = $1`, rtVersion)

	rep := f.pass(t)
	if rep.Versions != 1 || rep.Legacy != 1 {
		t.Fatalf("the pass: %+v", rep)
	}
	v, _ := VersionByID(ctx, f.st.Pool(), old)
	if v.State != VersionRemoved || v.RemovedAt == nil || v.RemoveEventID == nil {
		t.Fatalf("the old version: %+v", v)
	}
	if exists(VersionDir(f.itemDir, old)) {
		t.Error("the old version's folder is there")
	}
	dir, _ := FindEvent(f.itemDir, *v.RemoveEventID, EventVersionRemoved)
	b, _ := os.ReadFile(filepath.Join(dir, EventFile))
	if !bytes.Contains(b, []byte(`"versionId": "`+old+`"`)) || !bytes.Contains(b, []byte(`"packageId": "p-11111111"`)) ||
		!bytes.Contains(b, []byte(`"by": "katalog-manager (library.superseded.grace)"`)) ||
		!bytes.Contains(b, []byte(`"reason": "superseded by version `+rtVersion+`, and library.superseded.grace is over"`)) {
		t.Errorf("the version-removed event:\n%s", b)
	}
	if v, _ := VersionByID(ctx, f.st.Pool(), recent); v.State != VersionSuperseded || v.RemovedAt != nil || !exists(VersionDir(f.itemDir, recent)) {
		t.Errorf("the version within its grace: %+v", v)
	}
	if exists(legacy) || !exists(filepath.Join(f.p.LegacyDay(time.Now()), "packages", "movies", rtFilm[:2], rtFilm, "manifest.json")) {
		t.Errorf("the legacy package folder is not moved aside: %v", files(t, f.p.LegacyDir()))
	}

	// Its removal begun and its event recorded before a crash: the folder
	// goes, and the version is removed.
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET supersededat = now() - interval '2 days',
		removedat = date_trunc('second', now()), removeeventid = $2 WHERE id = $1`, recent, NewID())
	v, _ = VersionByID(ctx, f.st.Pool(), recent)
	if _, err := WriteEvent(f.itemDir, Event{ID: *v.RemoveEventID, At: *v.RemovedAt, By: RemovedBy, Kind: EventVersionRemoved,
		VersionID: recent}); err != nil {
		t.Fatal(err)
	}
	if rep := f.pass(t); rep.Versions != 1 {
		t.Errorf("the removal begun: %+v", rep)
	}
	if v, _ := VersionByID(ctx, f.st.Pool(), recent); v.State != VersionRemoved || exists(VersionDir(f.itemDir, recent)) {
		t.Errorf("the version whose removal began: %+v", v)
	}
	if evs, _ := os.ReadDir(filepath.Join(f.itemDir, "events")); len(evs) != 2 {
		t.Errorf("events: %v", evs)
	}
}

// An extra's original is deleted once its folder has been recorded for the
// delay and verifies, if it is the file taken in: moved to the trash, its
// folder pruned up to EXTRAS_ROOT, the extra keeping no original. One whose
// file changed is kept, and so are the extras of a held title. An extra's
// original does not wait for its title's steps.
func TestAnExtrasOriginalIsDeletedAfterPackaging(t *testing.T) {
	f := newRetireFixture(t)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'pending' WHERE step = 'subtitle'`)
	const extra, changed = "e1e1e1e1-0000-4000-8000-000000000001", "e2e2e2e2-0000-4000-8000-000000000002"
	src := map[string]string{}
	for _, x := range []string{extra, changed} {
		src[x] = filepath.Join(f.p.Extras, "example-film-"+x[:2], "Trailer.mkv")
		librarytest.Write(t, src[x], bytes.Repeat([]byte(x), 5000))
		size, qh1, _ := QH1(src[x])
		dir := ExtraDir(f.itemDir, x)
		librarytest.WriteExtra(t, dir, librarytest.Extra{ExtraID: x, PackageID: NewID(), CreatedAt: "2026-10-06T10:00:00Z"})
		storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state,
				sourcepath, sourcesize, sourceqh1, recordpath, packagepath, recordedat)
			VALUES ($1, $2, 'trailer', 'Trailer', 'api', 'ready', $3, $4, $5, $6, $6, now() - interval '1 hour')`,
			x, rtFilm, src[x], size, qh1, dir)
	}
	librarytest.Write(t, src[changed], []byte("another file"))
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = true`)
	if rep := f.pass(t); rep.Extras != 0 {
		t.Errorf("a held title's extras: %+v", rep)
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET retirehold = false`)
	rep, err := f.r.Pass(context.Background())
	if rep.Extras != 1 || rep.Originals != 0 || err == nil || !strings.Contains(err.Error(), "the original changed since it was recorded") {
		t.Fatalf("the pass: %+v, %v", rep, err)
	}
	var path *string
	var deleted *time.Time
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT sourcepath, sourcedeletedat FROM com_nalet_katalog_itemextras WHERE id = $1`,
		extra).Scan(&path, &deleted); err != nil || path != nil || deleted == nil {
		t.Errorf("the extra: %v %v, %v", path, deleted, err)
	}
	if got := files(t, f.p.ExtraTrashDir(time.Now(), extra)); len(got) != 1 || got[0] != "Trailer.mkv" {
		t.Errorf("the extra's trash: %v", got)
	}
	if exists(filepath.Dir(src[extra])) || !exists(f.p.Extras) {
		t.Error("the extra's folder is not pruned up to EXTRAS_ROOT")
	}
	if !exists(src[changed]) {
		t.Error("the changed extra's original is gone")
	}
	if !exists(f.original) || f.source(t, rtSource).State != SourcePresent {
		t.Error("the title's original, whose steps are not over, is retired")
	}
}

// One instance runs a pass at a time: while another holds the job's lock, a
// pass does nothing. At most library.retire.rate originals go in a pass.
func TestOnePassAtATimeAndItsRate(t *testing.T) {
	f := newRetireFixture(t)
	f.addMovie(t, "7c7c7c7c-1111-4222-8333-444444444444", "8d8d8d8d-1111-4222-8333-444444444444",
		"9e9e9e9e-1111-4222-8333-444444444444", "afafafaf-1111-4222-8333-444444444444", "Second Film")
	ctx := context.Background()
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('library-retire'))`); err != nil {
		t.Fatal(err)
	}
	if rep := f.pass(t); rep != (Report{}) {
		t.Errorf("a pass while another runs: %+v", rep)
	}
	_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('library-retire'))`)
	conn.Release()
	f.set(t, SettingRetireRate, "1")
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("a pass at a rate of 1: %+v", rep)
	}
	if rep := f.pass(t); rep.Originals != 1 {
		t.Errorf("the next pass: %+v", rep)
	}
	// A pass a minute at most, as the sweep calls it on its rounds.
	f.addMovie(t, "7d7d7d7d-1111-4222-8333-444444444444", "8e8e8e8e-1111-4222-8333-444444444444",
		"9f9f9f9f-1111-4222-8333-444444444444", "a0a0a0a0-1111-4222-8333-444444444444", "Third Film")
	f.r.last = time.Now().Add(-30 * time.Second)
	if did, err := f.r.Sweep(ctx); err != nil || did != "" {
		t.Errorf("a sweep within the minute: %q, %v", did, err)
	}
	f.r.last = time.Now().Add(-time.Minute)
	if did, err := f.r.Sweep(ctx); err != nil || did != "1 originals deleted" {
		t.Errorf("a sweep a minute later: %q, %v", did, err)
	}
}

// No original is retired before its title's current package carries the
// surround it had (owner decision 2026-10-07): a 5.1 original whose package
// is stereo is kept, the source saying why and its retire step waiting —
// failed, with no retry of its own, held for the version — and is not looked
// at again until another version is complete; once one with its 5.1
// companion is, it is retired. A 5.1 original whose package has the
// companion is retired with nothing lost; a stereo original with a stereo
// package too.
func TestAnOriginalIsKeptUntilItsPackageCarriesItsSurround(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixtureOf(t, fiveOne, stereo)
	rep := f.pass(t)
	if rep.Held != 1 || rep.Originals != 0 || rep.Failed != 0 {
		t.Fatalf("a 5.1 original with a stereo package: %+v", rep)
	}
	f.kept(t)
	if s := f.source(t, rtSource); deref(s.Error) != SurroundHeld {
		t.Errorf("the source's error: %q", deref(s.Error))
	}
	want := "failed 1 error=" + SurroundHeld + " details=held for version " + rtVersion
	if got := f.step(t, rtFilm); got != want {
		t.Errorf("the retire step: %s\nwant: %s", got, want)
	}
	var retry *time.Time
	var last string
	if err := f.st.Pool().QueryRow(ctx, `SELECT nextretryat, lasterror FROM com_nalet_katalog_itemprocessingsteps
		WHERE item_id = $1 AND step = 'retire'`, rtFilm).Scan(&retry, &last); err != nil || retry != nil || last != SurroundHeld {
		t.Errorf("the held step's retry %v and last error %q, %v; want none and the reason", retry, last, err)
	}
	if rep := f.pass(t); rep != (Report{}) {
		t.Errorf("a pass again, the same version: %+v", rep)
	}
	if got := f.step(t, rtFilm); got != want {
		t.Errorf("the retire step after a pass again: %s", got)
	}

	// Encoded again: a version with the 5.1 companion takes over.
	const again, againPkg = "9b2e3f4a-5b6c-4d7e-8f9a-0b1c2d3e4f5b", "4e1d2e3f-4a5b-4c6d-8e7f-8a9b0c1d2e3e"
	dir := VersionDir(f.itemDir, again)
	librarytest.WriteVersion(t, dir, librarytest.Version{VersionID: again, PackageID: againPkg, SourceIDs: []string{rtSource},
		CreatedAt: "2026-10-07T10:00:00Z", Package: map[string]any{"essence": withFiveOne}})
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded', supersededby = $2, supersededat = now()
		WHERE id = $1`, rtVersion, again)
	storetest.Exec(t, f.st, `INSERT INTO com_nalet_katalog_itemversions (id, item_id, sourceids, state, packageid, dir, completedat)
		VALUES ($1, $2, ARRAY[$3::varchar], 'complete', $4, $5, now() - interval '1 hour')`, again, rtFilm, rtSource, againPkg, dir)
	rep = f.pass(t)
	if rep.Originals != 1 || rep.Held != 0 {
		t.Fatalf("once a version with its 5.1 is complete: %+v", rep)
	}
	if s := f.source(t, rtSource); s.State != SourceDeleted || string(s.Lost) != "[]" || s.Error != nil {
		t.Errorf("the retired source: %s, lost %s, error %v", s.State, s.Lost, deref(s.Error))
	}
	if got := f.step(t, rtFilm); got != "done 0 error=- details=lost: nothing" {
		t.Errorf("the retire step: %s", got)
	}

	for name, c := range map[string]struct {
		src, pkg map[string]any
		lost     string
	}{
		"a 5.1 original whose package has its companion": {fiveOne, withFiveOne, "[]"},
		"a stereo original":                      {stereo, stereo, "[]"},
		"a 7.1 original whose package has a 5.1": {sevenOne, withFiveOne, `["maxAudioChannels"]`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRetireFixtureOf(t, c.src, c.pkg)
			if rep := f.pass(t); rep.Originals != 1 || rep.Held != 0 {
				t.Fatalf("the pass: %+v", rep)
			}
			if s := f.source(t, rtSource); s.State != SourceDeleted || string(s.Lost) != c.lost {
				t.Errorf("the source: %s, lost %s; want deleted, %s", s.State, s.Lost, c.lost)
			}
		})
	}
	// 6 to 4 channels is not a 5.1 of it either.
	g := newRetireFixtureOf(t, fiveOne, map[string]any{"surround": true, "maxAudioChannels": 4,
		"audioLanguages": []string{"en"}, "subtitleLanguages": []string{"en", "de"}})
	if rep := g.pass(t); rep.Held != 1 {
		t.Errorf("a 5.1 original whose package has four channels: %+v", rep)
	}
}
