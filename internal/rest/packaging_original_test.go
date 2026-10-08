package rest

import (
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

// originalNamed are the fields of version.json naming the original in its
// folder.
var originalNamed = map[string]any{"originalFiles": []string{"original.mkv"}}

// arrived gives the film's original its bytes where it arrived, and the
// source the size and quick hash they have: it answers where it lies.
func (f *v2Film) arrived(t *testing.T) string {
	t.Helper()
	path := f.cfg.Roots(true).Arrivals + "/A Film (2024)/A Film (2024).mkv"
	librarytest.Write(t, path, []byte(strings.Repeat("the picture and the sound ", 4000)))
	size, qh1, err := library.QH1(path)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemsources SET sizebytes = $2, qh1 = $3 WHERE id = $1`, filmSource, size, qh1)
	return path
}

// renameIn renames the original at path into the version's folder as the
// packager does, as original.mkv, and answers where it lies now.
func (f *v2Film) renameIn(t *testing.T, path, vid string) string {
	t.Helper()
	to := filepath.Join(library.VersionDir(f.itemDir, vid), "original.mkv")
	if err := os.MkdirAll(filepath.Dir(to), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, to); err != nil {
		t.Fatal(err)
	}
	return to
}

// withOriginal is the payload naming the original the run renamed into the
// version's folder.
func withOriginal(payload, path string) string {
	return strings.TrimSuffix(strings.TrimSpace(payload), "}") +
		fmt.Sprintf(`, "original": {"path": %q, "name": %q}}`, path, filepath.Base(path))
}

// sourceRow is the source and the asset of the title's file as the catalog
// holds them: where the original lies, its name, and the asset's path.
func (f *v2Film) sourceRow(t *testing.T) string {
	t.Helper()
	var out string
	if err := f.st.Pool().QueryRow(t.Context(), `SELECT s.arrivalpath || ' ' || s.filename || ' ' || (s.recordedat IS NOT NULL) ||
			' ' || a.path FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_playbackassets a ON a.sourceid = s.id
		WHERE s.id = $1`, filmSource).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A run that renamed the original into the version's folder names it in
// its payload: the transaction that records the version records the source
// where its original lies now, under its name there, and the asset of the
// title's file follows it.
func TestPackagingCompleteRecordsTheOriginalWithItsVersion(t *testing.T) {
	f := newV2Film(t)
	moved := f.renameIn(t, f.arrived(t), filmVersion)
	complete := f.versionOf(t, filmVersion, filmPackage, originalNamed)
	code, answer := f.complete(t, withOriginal(f.payload(filmVersion, filmPackage, complete), moved))
	if code != http.StatusOK || answer["current"] != true {
		t.Fatalf("packaging-complete: %d %v", code, answer)
	}
	if got, want := f.sourceRow(t), moved+" original.mkv true "+moved; got != want {
		t.Errorf("the source and the title's file: %q, want %q", got, want)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'complete'`,
		filmVersion); n != 1 {
		t.Error("the version is not complete")
	}
	// Taken again: the same answer.
	if code, again := f.complete(t, withOriginal(f.payload(filmVersion, filmPackage, complete), moved)); code != http.StatusOK ||
		fmt.Sprint(again) != fmt.Sprint(answer) {
		t.Errorf("taken again: %d %v", code, again)
	}
}

// An original the payload names outside the version's folder, under a name
// the library does not give one, that version.json does not name, or that
// is not the source's file is refused (422): the version's folder goes out
// of the record, and the original the run renamed (into it, or elsewhere in
// the item's folder) goes back where the catalog says it lies first. A
// folder holding a file named as an original that is not the source's, or
// whose original's place is taken, stays where it is. The catalog's paths
// stay as they were.
func TestPackagingCompleteRefusesAnOriginalOutsideItsVersion(t *testing.T) {
	f := newV2Film(t)
	arrival := f.arrived(t)
	vdir := library.VersionDir(f.itemDir, filmVersion)
	for _, c := range []struct {
		name, path, says string
		version          map[string]any
		stays            bool
	}{
		{"outside the folder", filepath.Join(f.itemDir, "versions", "original.mkv"), "does not lie in the version's folder", originalNamed, false},
		{"in its package", filepath.Join(vdir, "hls", "original.mkv"), "does not lie in the version's folder", originalNamed, false},
		{"named as it arrived", filepath.Join(vdir, "A Film (2024).mkv"), "is not the one the library gives it", originalNamed, false},
		{"not in version.json", filepath.Join(vdir, "original.mkv"), "originalFiles do not name",
			map[string]any{"originalFiles": []string{"original.ts"}}, false},
		{"another file", filepath.Join(vdir, "original.mkv"), "the quick hash", originalNamed, true},
	} {
		os.RemoveAll(vdir)
		complete := librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: filmVersion, PackageID: filmPackage,
			SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Version: c.version})
		if c.name == "another file" {
			librarytest.Write(t, c.path, []byte(strings.Repeat("x", 104000)))
		} else {
			if err := os.MkdirAll(filepath.Dir(c.path), 0o775); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(arrival, c.path); err != nil {
				t.Fatal(err)
			}
		}
		code, m := f.complete(t, withOriginal(f.payload(filmVersion, filmPackage, complete), c.path))
		if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(m["error"]), c.says) {
			t.Errorf("%s: %d %v, want 422 saying %s", c.name, code, m, c.says)
		}
		if _, err := os.Stat(arrival); err != nil {
			t.Errorf("%s: the original is not back where it arrived: %v", c.name, err)
		}
		if _, err := os.Stat(vdir); (err == nil) != c.stays {
			t.Errorf("%s: the refused version's folder in the record: %v, want it to stay %v", c.name, err, c.stays)
		}
		if got, want := f.sourceRow(t), arrival+" A Film (2024).mkv false "+arrival; got != want {
			t.Errorf("%s: the source and the title's file: %q, want %q", c.name, got, want)
		}
	}
	// A folder whose original cannot go back, its place taken, stays.
	os.RemoveAll(vdir)
	librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: filmVersion, PackageID: filmPackage,
		SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Version: originalNamed})
	moved := f.renameIn(t, arrival, filmVersion)
	librarytest.Write(t, arrival, []byte("a file that arrived since"))
	code, m := f.complete(t, withOriginal(f.payload(filmVersion, filmPackage, "sha256:"+strings.Repeat("0", 64)), moved))
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(m["error"]), "stays in the record") {
		t.Errorf("a broken chain whose original's place is taken: %d %v", code, m)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("the original of a folder that stays: %v", err)
	}
}

// A version taken in (takenIn: true, no package) is recorded taken: its
// folder, its sources, and its source where its original lies now, with the
// asset of the title's file; no packaged asset, no subtitle, nothing
// announced, the title not modified for its projection. Again, the same
// answer. A package added to it later completes it, the original staying in
// its folder; one refused leaves the folder as it was taken in, the package
// moved out of the record.
func TestPackagingCompleteTakesAVersionIn(t *testing.T) {
	f := newV2Film(t)
	moved := f.renameIn(t, f.arrived(t), filmVersion)
	vdir := library.VersionDir(f.itemDir, filmVersion)
	librarytest.Write(t, filepath.Join(vdir, "version.json"), librarytest.JSON(t, map[string]any{
		"schema": "zaentrum.library.version/2", "versionId": filmVersion, "sourceIds": []string{filmSource},
		"originalFiles": []string{"original.mkv"}}))
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_items SET modifiedat = '2001-01-01'`)
	body := fmt.Sprintf(`{"layout": "v2", "versionId": %q, "versionDir": %q, "sourceId": %q, "sourceRecorded": true,
		"takenIn": true, "original": {"path": %q, "name": "original.mkv"}, "source": {"codec": "hevc", "width": 3840, "height": 1600}}`,
		filmVersion, vdir, filmSource, moved)
	code, answer := f.complete(t, body)
	want := map[string]any{"itemId": filmItem, "versionId": filmVersion, "takenIn": true, "current": false, "superseded": nil,
		"packagedAssetWritten": false, "subtitlesWritten": 0.0, "audioTracks": 0.0}
	if code != http.StatusOK || fmt.Sprint(answer) != fmt.Sprint(want) {
		t.Fatalf("packaging-complete of a version taken in: %d %v", code, answer)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'taken'
		AND dir = $2 AND packageid IS NULL AND completedat IS NULL`, filmVersion, vdir); n != 1 {
		t.Error("the version is not taken in")
	}
	if got, want := f.sourceRow(t), moved+" original.mkv true "+moved; got != want {
		t.Errorf("the source and the title's file: %q, want %q", got, want)
	}
	for sql, want := range map[string]int{
		`SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE kind = 'packaged'`:                          0,
		`SELECT count(*) FROM com_nalet_katalog_playbackassets WHERE id = 'src-f1' AND resolution = '3840x1600'`: 1,
		`SELECT count(*) FROM com_nalet_katalog_items WHERE modifiedat > '2001-01-01'`:                           0,
	} {
		if n := storetest.Count(t, f.st, sql); n != want {
			t.Errorf("%s: %d, want %d", sql, n, want)
		}
	}
	if code, again := f.complete(t, body); code != http.StatusOK || fmt.Sprint(again) != fmt.Sprint(want) {
		t.Errorf("taken in again: %d %v", code, again)
	}

	// A package added to it, refused: it goes out, the version stays as it
	// was taken in.
	complete := librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: filmVersion, PackageID: filmPackage,
		SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Version: map[string]any{"originalFiles": []string{"original.mkv"}}})
	code, m := f.complete(t, f.payload(filmVersion, "5a5a0000-0000-4000-8000-000000000009", complete))
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(m["error"]), "the version keeps its original") {
		t.Errorf("a package added and refused: %d %v", code, m)
	}
	if got := strings.Join(fileNames(t, vdir), " "); got != "original.mkv version.json" {
		t.Errorf("the version taken in holds %s after the refusal", got)
	}
	// Added again, whole: complete, its original where it is.
	complete = librarytest.WriteVersion(t, vdir, librarytest.Version{VersionID: filmVersion, PackageID: filmPackage,
		SourceIDs: []string{filmSource}, CreatedAt: "2026-10-06T09:00:00Z", Version: map[string]any{"originalFiles": []string{"original.mkv"}}})
	if code, m := f.complete(t, f.payload(filmVersion, filmPackage, complete)); code != http.StatusOK || m["current"] != true {
		t.Fatalf("a package added to the version taken in: %d %v", code, m)
	}
	if n := storetest.Count(t, f.st, `SELECT count(*) FROM com_nalet_katalog_itemversions WHERE id = $1 AND state = 'complete'
		AND packageid = $2`, filmVersion, filmPackage); n != 1 {
		t.Error("the version taken in is not complete with its package")
	}
	if got, want := f.sourceRow(t), moved+" original.mkv true "+moved; got != want {
		t.Errorf("after the package: %q, want %q", got, want)
	}
	if code, m := f.complete(t, body); code != http.StatusConflict {
		t.Errorf("taken in once complete: %d %v", code, m)
	}
}

// fileNames are the names in dir, sorted.
func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
