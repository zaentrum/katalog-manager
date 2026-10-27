package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library/librarytest"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// verifierOf is the verifier of the fixture's catalog, its clock what now
// holds.
func verifierOf(f *retireFixture, now *time.Time) *Verifier {
	v := NewVerifier(f.st.Pool(), f.cfg)
	v.now = func() time.Time { return *now }
	return v
}

// verifyPass runs a pass of the verifier, which must do without an error.
func verifyPass(t *testing.T, v *Verifier) VerifyReport {
	t.Helper()
	rep, err := v.Pass(context.Background())
	if err != nil {
		t.Fatalf("the pass: %v", err)
	}
	return rep
}

// verifiedOf is how the catalog notes the version id verified: when and how
// far, "-" for not at all.
func verifiedOf(t *testing.T, f *retireFixture, id string) string {
	t.Helper()
	var at *time.Time
	var level *string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT verifiedat, verifiedlevel FROM com_nalet_katalog_itemversions
		WHERE id = $1`, id).Scan(&at, &level); err != nil {
		t.Fatal(err)
	}
	if at == nil {
		return "-"
	}
	return at.UTC().Format(time.RFC3339) + " " + deref(level)
}

var verifyClock = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

// A complete version no one verified is verified in full once, noted at the
// moment its verification began; the library is as it was, and a pass after
// finds nothing to do.
func TestTheVerifierVerifiesANewVersionInFull(t *testing.T) {
	f := newRetireFixture(t)
	before := fsPrint(t, f.cfg.LibraryRoot)
	now := verifyClock
	v := verifierOf(f, &now)
	var read []string
	v.read = func(dir string, full bool) (*Chain, error) {
		read = append(read, dir)
		now = now.Add(3 * time.Second)
		if !full {
			t.Error("the version is read for its chain alone")
		}
		return VerifyVersion(dir, full)
	}
	rep := verifyPass(t, v)
	if rep.Verified != 1 || rep.Left != 0 || rep.Bytes <= 0 || len(read) != 1 || read[0] != f.versionDir {
		t.Fatalf("the pass: %+v, read %v", rep, read)
	}
	if got := verifiedOf(t, f, rtVersion); got != "2026-10-27T10:00:00Z full" {
		t.Errorf("the version is noted %q", got)
	}
	if after := fsPrint(t, f.cfg.LibraryRoot); after != before {
		t.Errorf("the library changed:\n%s\nwas\n%s", after, before)
	}
	if rep := verifyPass(t, v); rep != (VerifyReport{}) || len(read) != 1 {
		t.Errorf("a second pass: %+v, read %v", rep, read)
	}
}

// A version whose files do not hold their checksums is left unverified, not
// read again before the backoff is over, and verified once it holds them.
func TestTheVerifierLeavesAVersionWhoseFilesDoNotHold(t *testing.T) {
	f := newRetireFixture(t)
	seg := filepath.Join(f.versionDir, "hls/v0/seg0.m4s")
	librarytest.Write(t, seg, []byte("a segment of the pictur3")) // its bytes as many, others
	now := verifyClock
	v := verifierOf(f, &now)
	if rep := verifyPass(t, v); rep.Verified != 0 || rep.Left != 1 {
		t.Fatalf("the pass: %+v", rep)
	}
	if got := verifiedOf(t, f, rtVersion); got != "-" {
		t.Errorf("a version whose file does not hold is noted %q", got)
	}
	librarytest.Write(t, seg, []byte(librarytest.DefaultFiles["hls/v0/seg0.m4s"]))
	now = now.Add(VerifyNewBackoff - time.Minute)
	if rep := verifyPass(t, v); rep != (VerifyReport{}) {
		t.Errorf("within the backoff: %+v", rep)
	}
	now = now.Add(2 * time.Minute)
	if rep := verifyPass(t, v); rep.Verified != 1 || rep.Left != 0 {
		t.Errorf("after the backoff: %+v", rep)
	}
	if got := verifiedOf(t, f, rtVersion); got != now.Format(time.RFC3339)+" full" {
		t.Errorf("the version is noted %q", got)
	}
}

// What the verifier leaves: a version whose checksums are pending, whose
// package.json is another package's than the catalog's, whose folder lies
// outside the library (its chain sound there), whose chain lacks a file, and
// one whose row changed while it was read — superseded, or noted by the
// retire job, whose note stays.
func TestTheVerifierLeavesWhatItCannotNote(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *retireFixture, v *Verifier)
		want  string // how the version is noted after
	}{
		{"its checksums pending", func(t *testing.T, f *retireFixture, v *Verifier) {
			if err := os.RemoveAll(f.versionDir); err != nil {
				t.Fatal(err)
			}
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Pending: true})
		}, "-"},
		{"another package's", func(t *testing.T, f *retireFixture, v *Verifier) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET packageid = $2 WHERE id = $1`,
				rtVersion, "7a6b5c4d-3e2f-4a1b-8c0d-9e8f7a6b5c4d")
		}, "-"},
		{"outside the library", func(t *testing.T, f *retireFixture, v *Verifier) {
			out := filepath.Join(t.TempDir(), "versions", rtVersion)
			librarytest.WriteVersion(t, out, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z"})
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET dir = $2 WHERE id = $1`, rtVersion, out)
		}, "-"},
		{"a file missing", func(t *testing.T, f *retireFixture, v *Verifier) {
			if err := os.Remove(filepath.Join(f.versionDir, "subs/1.vtt")); err != nil {
				t.Fatal(err)
			}
		}, "-"},
		{"superseded while it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			v.read = func(dir string, full bool) (*Chain, error) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, rtVersion)
				return VerifyVersion(dir, full)
			}
		}, "-"},
		{"noted by the retire job while it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			v.read = func(dir string, full bool) (*Chain, error) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = '2026-10-27T09:00:00Z',
					verifiedlevel = 'chain' WHERE id = $1`, rtVersion)
				return VerifyVersion(dir, full)
			}
		}, "2026-10-27T09:00:00Z chain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			now := verifyClock
			v := verifierOf(f, &now)
			c.setup(t, f, v)
			if rep := verifyPass(t, v); rep.Verified != 0 || rep.Left != 1 {
				t.Errorf("the pass: %+v", rep)
			}
			if got := verifiedOf(t, f, rtVersion); got != c.want {
				t.Errorf("the version is noted %q, want %q", got, c.want)
			}
			if rep := verifyPass(t, v); rep != (VerifyReport{}) {
				t.Errorf("a pass within the backoff: %+v", rep)
			}
		})
	}
}

// The verifier verifies complete versions no one verified alone: one noted
// verified already stays as it is noted, and a superseded one, or one still
// building, is not read.
func TestTheVerifierReadsNoOtherVersion(t *testing.T) {
	for state, verified := range map[string]bool{"complete": true, "superseded": false, "building": false} {
		f := newRetireFixture(t)
		storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = $2 WHERE id = $1`, rtVersion, state)
		if verified {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = '2026-10-20T08:00:00Z',
				verifiedlevel = 'chain' WHERE id = $1`, rtVersion)
		}
		now := verifyClock
		v := verifierOf(f, &now)
		v.read = func(dir string, full bool) (*Chain, error) {
			t.Errorf("%s: %s was read", state, dir)
			return VerifyVersion(dir, full)
		}
		if rep := verifyPass(t, v); rep != (VerifyReport{}) {
			t.Errorf("%s: the pass: %+v", state, rep)
		}
		want := "-"
		if verified {
			want = "2026-10-20T08:00:00Z chain"
		}
		if got := verifiedOf(t, f, rtVersion); got != want {
			t.Errorf("%s: the version is noted %q, want %q", state, got, want)
		}
	}
}

// The versions waiting are verified in one pass, the one completed first
// first.
func TestTheVerifierVerifiesTheVersionCompletedFirstFirst(t *testing.T) {
	f := newRetireFixture(t)
	f.addMovie(t, ltFilm, ltSource, ltVersion, ltPackage, "Another Film")
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET completedat = now() - interval '3 hours' WHERE id = $1`,
		ltVersion)
	now := verifyClock
	v := verifierOf(f, &now)
	var read []string
	v.read = func(dir string, full bool) (*Chain, error) {
		read = append(read, filepath.Base(dir))
		return VerifyVersion(dir, full)
	}
	if rep := verifyPass(t, v); rep.Verified != 2 || rep.Left != 0 {
		t.Fatalf("the pass: %+v", rep)
	}
	if len(read) != 2 || read[0] != ltVersion || read[1] != rtVersion {
		t.Errorf("read %v, want the other film's first", read)
	}
}

// The verifier verifies with library.layout v2 and library.verify.new on
// alone (one it cannot read is on), in one instance at a time, and stops
// after the version it is at when the setting turns off.
func TestTheVerifierFollowsItsSettings(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		key, value string
		verified   int
	}{
		{SettingVerifyNew, "off", 0},
		{SettingLayout, "legacy", 0},
		{SettingVerifyNew, "sometimes", 1},
		{SettingVerifyNew, " On ", 1},
	} {
		f := newRetireFixture(t)
		f.set(t, c.key, c.value)
		now := verifyClock
		if rep := verifyPass(t, verifierOf(f, &now)); rep.Verified != c.verified {
			t.Errorf("%s=%s: %+v", c.key, c.value, rep)
		}
	}

	f := newRetireFixture(t)
	f.addMovie(t, ltFilm, ltSource, ltVersion, ltPackage, "Another Film")
	now := verifyClock
	v := verifierOf(f, &now)
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('library-verifier'))`); err != nil {
		t.Fatal(err)
	}
	if rep := verifyPass(t, v); rep != (VerifyReport{}) {
		t.Errorf("while another instance verifies: %+v", rep)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('library-verifier'))`); err != nil {
		t.Fatal(err)
	}
	v.read = func(dir string, full bool) (*Chain, error) {
		f.set(t, SettingVerifyNew, VerifyNewOff)
		return VerifyVersion(dir, full)
	}
	if rep := verifyPass(t, v); rep.Verified != 1 || rep.Left != 0 {
		t.Errorf("turned off while it verifies: %+v", rep)
	}
	if verifiedOf(t, f, rtVersion) == "-" && verifiedOf(t, f, ltVersion) == "-" {
		t.Error("no version is noted")
	}
	if verifiedOf(t, f, rtVersion) != "-" && verifiedOf(t, f, ltVersion) != "-" {
		t.Error("the verifier went on once it was turned off")
	}
}

// A list skips a title whose version no one verified; once the verifier has
// verified it, a dry run would retire it.
func TestAListTakesAVersionTheVerifierVerified(t *testing.T) {
	f := newListFixture(t)
	storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = NULL, verifiedlevel = NULL`)
	if _, items := f.dry(t, rtFilm); len(items) != 1 || said(items[0]) != "skipped: its complete version "+rtVersion+
		" was never verified" {
		t.Fatalf("the dry run before: %+v", items)
	}
	now := time.Now()
	if rep := verifyPass(t, verifierOf(f.retireFixture, &now)); rep.Verified != 1 {
		t.Fatalf("the verifier: %+v", rep)
	}
	if _, items := f.dry(t, rtFilm); len(items) != 1 || !wouldRetire(items[0]) {
		t.Errorf("the dry run after: %+v", items)
	}
}
