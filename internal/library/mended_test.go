package library

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
)

// A version the schemas' library-v2-mend.py mended is verified again in full
// by the run's mends journal and noted verified then: while it is what the
// tool left. A journal of another package.json notes nothing; a second call
// changes nothing; a version put back is verified and noted again.
func TestAMendedVersionIsVerifiedAgainAndNoted(t *testing.T) {
	f := newBesideFixture(t)
	ctx := context.Background()
	if rep, err := f.migration(t).Adopt(ctx, []string{mgBeside}); err != nil || rep.Adopted != 1 {
		t.Fatalf("Adopt: %+v, %v", rep, err)
	}
	vid := IDOf(mgBeside + ":version")
	vdir := VersionDir(f.itemDir(mgBeside), vid)
	librarytest.Fill(t, vdir)
	m := f.migration(t)
	if rep, err := m.Mended(ctx, nil); err != nil || rep.Versions+rep.Unchanged+len(rep.Skipped) != 0 {
		t.Errorf("Mended without a journal: %+v, %v", rep, err)
	}
	// the mend: the init segment changed in place, its line in the checksums,
	// its entry in package.json
	init := filepath.Join(vdir, "hls/v0/init.mp4")
	before, err := os.ReadFile(init)
	if err != nil {
		t.Fatal(err)
	}
	after := []byte(strings.ToUpper(string(before)))
	librarytest.Write(t, init, after)
	pj := filepath.Join(vdir, PackageFile)
	var pkg map[string]any
	b, _ := os.ReadFile(pj)
	if err := json.Unmarshal(b, &pkg); err != nil {
		t.Fatal(err)
	}
	// its line in the checksums written again, and package.json's hash of them
	sums := filepath.Join(vdir, SumsFile)
	listing, err := os.ReadFile(sums)
	if err != nil {
		t.Fatal(err)
	}
	listing = []byte(strings.Replace(string(listing), SHA256(before)+"  hls/v0/init.mp4\n", SHA256(after)+"  hls/v0/init.mp4\n", 1))
	librarytest.Write(t, sums, listing)
	pkg["checksums"].(map[string]any)["sha256"] = "sha256:" + SHA256(listing)
	pkg["mends"] = []any{map[string]any{"kind": "avsync-editlist", "file": "hls/v0/init.mp4", "box": "moov/trak/edts/elst",
		"old": 1328, "new": 0, "sha256Before": "sha256:" + SHA256(before), "sha256After": "sha256:" + SHA256(after),
		"at": "2026-10-09T02:00:00Z", "by": "an operator"}}
	b = librarytest.JSON(t, pkg)
	librarytest.Write(t, pj, b)
	complete := "sha256:" + SHA256(b)
	librarytest.Write(t, filepath.Join(vdir, CompleteFile), []byte(complete+"\n"))
	rel := m.relOf(vdir)
	journal := func(op, complete string) {
		t.Helper()
		start := map[string]string{"mended": "mend", "undone": "undo"}[op]
		librarytest.Write(t, filepath.Join(f.runDir, MendsJournal), []byte(
			`{"at": "2026-10-09T02:00:00Z", "op": "`+start+`", "dir": "`+rel+`", "itemId": "`+mgBeside+`", "versionId": "`+vid+`"}`+"\n"+
				`{"at": "2026-10-09T02:00:01Z", "op": "`+op+`", "dir": "`+rel+`", "itemId": "`+mgBeside+`", "versionId": "`+vid+
				`", "packageId": "`+IDOf(mgBeside+":package")+`", "complete": "`+complete+`"}`+"\n"+
				`{"at": "2026-10-09T02:00:02Z", "op": "mend", "dir": "a line a crash cut sho`))
	}
	journal("mended", "sha256:11")
	if rep, err := m.Mended(ctx, nil); err != nil || rep.Versions != 0 || len(rep.Skipped) != 1 ||
		!strings.Contains(rep.Skipped[0].Reason, "is not the one the run left") {
		t.Errorf("Mended over a folder the journal did not leave: %+v, %v", rep, err)
	}
	journal("mended", complete)
	if rep, err := m.Mended(ctx, []string{mgFilm}); err != nil || rep.Versions+rep.Unchanged+len(rep.Skipped) != 0 {
		t.Errorf("Mended for another item: %+v, %v", rep, err)
	}
	if rep, err := m.Mended(ctx, nil); err != nil || rep.Versions != 1 || len(rep.Skipped) != 0 {
		t.Fatalf("Mended: %+v, %v", rep, err)
	}
	var level string
	if err := f.st.Pool().QueryRow(ctx, `SELECT verifiedlevel FROM com_nalet_katalog_itemversions WHERE id = $1
		AND verifiedat > '2026-10-09T02:00:01Z'`, vid).Scan(&level); err != nil || level != VerifyFull {
		t.Errorf("the version after Mended: %q, %v", level, err)
	}
	if rep, err := m.Mended(ctx, nil); err != nil || rep.Versions != 0 || rep.Unchanged != 1 {
		t.Errorf("Mended again: %+v, %v", rep, err)
	}
	if c, err := VerifyVersion(vdir, true); err != nil || c.Pending {
		t.Errorf("the mended chain, in full: %+v, %v", c, err)
	}
	// changed since, with no entry: the file fails its line, nothing is noted
	librarytest.Write(t, init, before)
	reset := `UPDATE com_nalet_katalog_itemversions SET verifiedat = NULL, verifiedlevel = NULL WHERE id = $1`
	if _, err := f.st.Pool().Exec(ctx, reset, vid); err != nil {
		t.Fatal(err)
	}
	if rep, err := m.Mended(ctx, nil); err != nil || rep.Versions != 0 || len(rep.Skipped) != 1 ||
		!strings.Contains(rep.Skipped[0].Reason, "its chain does not hold") {
		t.Errorf("Mended over a file changed since: %+v, %v", rep, err)
	}
}
