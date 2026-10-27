package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		read = append(read, dir)
		now = now.Add(3 * time.Second)
		if !full {
			t.Error("the version is read for its chain alone")
		}
		return VerifyVersionContext(ctx, dir, full)
	}
	rep := verifyPass(t, v)
	if rep.Verified != 1 || rep.Left != 0 || rep.Changed != 0 || rep.Bytes <= 0 || len(read) != 1 || read[0] != f.versionDir {
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

// A version whose files do not hold their checksums is left unverified and
// not read again before its wait is over, six hours, then twice as long;
// once it holds them it is verified.
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
	for _, step := range []struct {
		after time.Duration
		left  int
	}{
		{VerifyNewBackoff - time.Minute, 0},
		{2 * time.Minute, 1}, // its first wait over: read again, left again
		{2*VerifyNewBackoff - 2*time.Minute, 0},
	} {
		now = now.Add(step.after)
		if rep := verifyPass(t, v); rep.Left != step.left || rep.Verified != 0 {
			t.Errorf("%s on: %+v, want %d left", now.Sub(verifyClock), rep, step.left)
		}
	}
	librarytest.Write(t, seg, []byte(librarytest.DefaultFiles["hls/v0/seg0.m4s"]))
	now = now.Add(3 * time.Minute)
	if rep := verifyPass(t, v); rep.Verified != 1 || rep.Left != 0 {
		t.Errorf("after its second wait: %+v", rep)
	}
	if got := verifiedOf(t, f, rtVersion); got != now.Format(time.RFC3339)+" full" {
		t.Errorf("the version is noted %q", got)
	}
	if len(v.left) != 0 {
		t.Errorf("a version verified is still left: %v", v.left)
	}
}

// A version whose files are all hashed as a shutdown comes is noted all the
// same.
func TestTheVerifierNotesAVersionProvenAsItStops(t *testing.T) {
	f := newRetireFixture(t)
	now := verifyClock
	v := verifierOf(f, &now)
	ctx, cancel := context.WithCancel(context.Background())
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		c, err := VerifyVersionContext(ctx, dir, full)
		cancel()
		return c, err
	}
	if rep, err := v.Pass(ctx); err != nil || rep.Verified != 1 {
		t.Errorf("the pass: %+v, %v", rep, err)
	}
	if got := verifiedOf(t, f, rtVersion); got != "2026-10-27T10:00:00Z full" {
		t.Errorf("the version is noted %q", got)
	}
}

// The waits grow from six hours, twice as long each time, to a week at most.
func TestTheVerifiersWaitsGrow(t *testing.T) {
	for times, want := range map[int]time.Duration{1: 6 * time.Hour, 2: 12 * time.Hour, 3: 24 * time.Hour, 5: 96 * time.Hour,
		6: 7 * 24 * time.Hour, 40: 7 * 24 * time.Hour} {
		if got := backoff(times); got != want {
			t.Errorf("left %d times: %s, want %s", times, got, want)
		}
	}
}

// What the verifier leaves: a version whose checksums are pending, whose
// package.json is another package's than the catalog's, whose folder lies
// outside the library (its chain sound there), whose chain lacks a file, and
// one whose folder or package the catalog changed while it was read. One
// that was superseded, or that the retire job noted, while it was read is
// not noted either, and waits for no one: the retire job's note stays.
func TestTheVerifierLeavesWhatItCannotNote(t *testing.T) {
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, f *retireFixture, v *Verifier)
		changed bool   // the version went or another noted it, rather than left
		want    string // how the version is noted after
	}{
		{"its checksums pending", func(t *testing.T, f *retireFixture, v *Verifier) {
			if err := os.RemoveAll(f.versionDir); err != nil {
				t.Fatal(err)
			}
			librarytest.WriteVersion(t, f.versionDir, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z", Pending: true})
		}, false, "-"},
		{"another package's", func(t *testing.T, f *retireFixture, v *Verifier) {
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET packageid = $2 WHERE id = $1`,
				rtVersion, "7a6b5c4d-3e2f-4a1b-8c0d-9e8f7a6b5c4d")
		}, false, "-"},
		{"outside the library", func(t *testing.T, f *retireFixture, v *Verifier) {
			out := filepath.Join(t.TempDir(), "versions", rtVersion)
			librarytest.WriteVersion(t, out, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z"})
			storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET dir = $2 WHERE id = $1`, rtVersion, out)
		}, false, "-"},
		{"a file missing", func(t *testing.T, f *retireFixture, v *Verifier) {
			if err := os.Remove(filepath.Join(f.versionDir, "subs/1.vtt")); err != nil {
				t.Fatal(err)
			}
		}, false, "-"},
		{"moved to another folder while it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			other := VersionDir(f.itemDir, "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f")
			librarytest.WriteVersion(t, other, librarytest.Version{VersionID: rtVersion, PackageID: rtPackage,
				SourceIDs: []string{rtSource}, CreatedAt: "2026-10-06T10:00:00Z"})
			v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET dir = $2 WHERE id = $1`, rtVersion, other)
				return VerifyVersionContext(ctx, dir, full)
			}
		}, false, "-"},
		{"of another package once it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
				c, err := VerifyVersionContext(ctx, dir, full)
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET packageid = $2 WHERE id = $1`,
					rtVersion, "7a6b5c4d-3e2f-4a1b-8c0d-9e8f7a6b5c4d")
				return c, err
			}
		}, false, "-"},
		{"superseded while it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET state = 'superseded' WHERE id = $1`, rtVersion)
				return VerifyVersionContext(ctx, dir, full)
			}
		}, true, "-"},
		{"noted by the retire job while it was read", func(t *testing.T, f *retireFixture, v *Verifier) {
			v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
				storetest.Exec(t, f.st, `UPDATE com_nalet_katalog_itemversions SET verifiedat = '2026-10-27T09:00:00Z',
					verifiedlevel = 'chain' WHERE id = $1`, rtVersion)
				return VerifyVersionContext(ctx, dir, full)
			}
		}, true, "2026-10-27T09:00:00Z chain"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRetireFixture(t)
			now := verifyClock
			v := verifierOf(f, &now)
			c.setup(t, f, v)
			want := VerifyReport{Left: 1}
			if c.changed {
				want = VerifyReport{Changed: 1}
			}
			if rep := verifyPass(t, v); rep != want {
				t.Errorf("the pass: %+v, want %+v", rep, want)
			}
			if got := verifiedOf(t, f, rtVersion); got != c.want {
				t.Errorf("the version is noted %q, want %q", got, c.want)
			}
			v.read = VerifyVersionContext
			if rep := verifyPass(t, v); rep != (VerifyReport{}) {
				t.Errorf("a pass within the wait: %+v", rep)
			}
		})
	}
}

// A note the catalog refuses is an error of the pass, and the version waits
// as one left does: it is not read again before its wait is over.
func TestTheVerifierWaitsAfterANoteItCouldNotWrite(t *testing.T) {
	f := newRetireFixture(t)
	storetest.Exec(t, f.st, `CREATE FUNCTION refuse_note() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'no note'; END $$
		LANGUAGE plpgsql`)
	storetest.Exec(t, f.st, `CREATE TRIGGER refuse_note BEFORE UPDATE ON com_nalet_katalog_itemversions
		FOR EACH ROW EXECUTE FUNCTION refuse_note()`)
	now := verifyClock
	v := verifierOf(f, &now)
	reads := 0
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		reads++
		return VerifyVersionContext(ctx, dir, full)
	}
	if rep, err := v.Pass(context.Background()); err == nil || rep.Left != 1 || rep.Verified != 0 {
		t.Fatalf("the pass: %+v, %v", rep, err)
	}
	storetest.Exec(t, f.st, `DROP TRIGGER refuse_note ON com_nalet_katalog_itemversions`)
	if rep := verifyPass(t, v); rep != (VerifyReport{}) || reads != 1 {
		t.Errorf("within its wait: %+v, read %d times", rep, reads)
	}
	now = now.Add(VerifyNewBackoff + time.Minute)
	if rep := verifyPass(t, v); rep.Verified != 1 || reads != 2 {
		t.Errorf("after its wait: %+v, read %d times", rep, reads)
	}
}

// A shutdown stops the verifier at the file it is at: nothing is noted, the
// version waits for no one, and the next pass verifies it.
func TestTheVerifierStopsAtTheFileItIsAt(t *testing.T) {
	f := newRetireFixture(t)
	now := verifyClock
	v := verifierOf(f, &now)
	ctx, cancel := context.WithCancel(context.Background())
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		cancel()
		c, err := VerifyVersionContext(ctx, dir, full)
		if err == nil {
			t.Error("a verification goes on once its context ended")
		}
		return c, err
	}
	if rep, err := v.Pass(ctx); err != nil || rep != (VerifyReport{}) {
		t.Errorf("the pass stopped: %+v, %v", rep, err)
	}
	if got := verifiedOf(t, f, rtVersion); got != "-" {
		t.Errorf("a version whose verification stopped is noted %q", got)
	}
	v.read = VerifyVersionContext
	if rep := verifyPass(t, v); rep.Verified != 1 {
		t.Errorf("the next pass: %+v", rep)
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
		v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
			t.Errorf("%s: %s was read", state, dir)
			return VerifyVersionContext(ctx, dir, full)
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
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		read = append(read, filepath.Base(dir))
		return VerifyVersionContext(ctx, dir, full)
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
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+verifyLock+`)`).Scan(&locked); err != nil || !locked {
		t.Fatalf("the verifier's lock: %v, %v", locked, err)
	}
	if rep := verifyPass(t, v); rep != (VerifyReport{}) {
		t.Errorf("while another instance verifies: %+v", rep)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+verifyLock+`)`); err != nil {
		t.Fatal(err)
	}
	v.read = func(ctx context.Context, dir string, full bool) (*Chain, error) {
		f.set(t, SettingVerifyNew, VerifyNewOff)
		return VerifyVersionContext(ctx, dir, full)
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
	// its pass released the lock: another instance takes it
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+verifyLock+`)`).Scan(&locked); err != nil || !locked {
		t.Errorf("the verifier's lock after its pass: %v, %v", locked, err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(`+verifyLock+`)`); err != nil {
		t.Fatal(err)
	}
}

// The verifier needs one connection, the one that holds its lock: on a pool
// of one it verifies, and is no other worker's wait.
func TestTheVerifierWorksOnOneConnection(t *testing.T) {
	f := newRetireFixture(t)
	cfg := f.st.Pool().Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	now := verifyClock
	v := NewVerifier(pool, f.cfg)
	v.now = func() time.Time { return now }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan VerifyReport, 1)
	go func() {
		rep, _ := v.Pass(ctx)
		done <- rep
	}()
	select {
	case rep := <-done:
		if rep.Verified != 1 {
			t.Errorf("the pass on a pool of one: %+v", rep)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the verifier waits for a second connection")
	}
}

// The verifier's lock is its schema's: one another installation holds, of
// another schema or of no schema, is none of its.
func TestTheVerifierIsNotHeldByAnotherSchemasLock(t *testing.T) {
	ctx := context.Background()
	f := newRetireFixture(t)
	conn, err := f.st.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, key := range []string{`hashtext('library-verifier')`, `hashtext('library-verifier/another_schema')`} {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(`+key+`)`); err != nil {
			t.Fatal(err)
		}
		defer func(key string) { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(`+key+`)`) }(key)
	}
	now := verifyClock
	if rep := verifyPass(t, verifierOf(f, &now)); rep.Verified != 1 {
		t.Errorf("while other schemas' verifiers verify: %+v", rep)
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
