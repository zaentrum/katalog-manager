package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A catalog that sets nothing keeps the legacy layout and every original.
func TestTheLibrarysDefaults(t *testing.T) {
	d := Defaults()
	if d.V2() || d.DeleteOriginals() || d.RetireDelay != 10*time.Minute || d.RetireRate != 30 || d.Verify != VerifyFull ||
		d.VerifyMaxAge != 30*24*time.Hour || d.SupersededGrace != 24*time.Hour || d.TrashGrace != 24*time.Hour {
		t.Errorf("the defaults: %+v", d)
	}
}

// A duration is a Go duration, days, or both; 0 is none.
func TestParseDuration(t *testing.T) {
	for v, want := range map[string]time.Duration{"0": 0, "10m": 10 * time.Minute, " 24h ": 24 * time.Hour,
		"30d": 30 * 24 * time.Hour, "1d12h": 36 * time.Hour, "0s": 0, "7D": 7 * 24 * time.Hour} {
		if got, err := ParseDuration(v); err != nil || got != want {
			t.Errorf("%q: %s, %v, want %s", v, got, err, want)
		}
	}
	for _, v := range []string{"", "soon", "-1h", "1d-1h", "d", "5 days"} {
		if got, err := ParseDuration(v); err == nil {
			t.Errorf("%q: %s, want an error", v, got)
		}
	}
}

// Each setting takes what it may hold, letter case and spaces aside; what it
// cannot hold is the default, and said.
func TestTheSettingsAreRead(t *testing.T) {
	var s Settings
	s = Defaults()
	for k, v := range map[string]string{SettingLayout: " V2 ", SettingOriginals: "delete-after-package", SettingRetireDelay: "1h",
		SettingRetireRate: "5", SettingVerify: "chain", SettingVerifyMaxAge: "7d", SettingSupersededGrace: "2h",
		SettingTrashGrace: "0"} {
		s.set(k, v)
	}
	if !s.V2() || !s.DeleteOriginals() || s.RetireDelay != time.Hour || s.RetireRate != 5 || s.Verify != VerifyChain ||
		s.VerifyMaxAge != 7*24*time.Hour || s.SupersededGrace != 2*time.Hour || s.TrashGrace != 0 || len(s.Problems) != 0 {
		t.Errorf("read: %+v", s)
	}
	s = Defaults()
	for k, v := range map[string]string{SettingLayout: "v3", SettingOriginals: "delete", SettingRetireRate: "0",
		SettingVerify: "some", SettingTrashGrace: "a while", SettingRetireDelay: ""} {
		s.set(k, v)
	}
	d := Defaults()
	if s.Layout != d.Layout || s.Originals != d.Originals || s.RetireRate != d.RetireRate || s.Verify != d.Verify ||
		s.TrashGrace != d.TrashGrace || s.RetireDelay != d.RetireDelay || len(s.Problems) != 5 {
		t.Errorf("what cannot be read: %+v", s)
	}
	if !strings.Contains(strings.Join(s.Problems, "\n"), `library.layout is "v3", which is not legacy or v2; the layout is legacy`) {
		t.Errorf("the problems: %v", s.Problems)
	}
}

// The settings come from the settings table, a key without the spaces around
// it, of a key held twice the row of the lowest id.
func TestReadSettingsFromTheCatalog(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES
		('1', ' library.layout ', 'v2'), ('2', 'library.layout', 'legacy'), ('3', 'library.originals', 'delete-after-package'),
		('4', 'extras.scan', 'true'), ('5', 'library.trash.grace', '7d')`)
	s, err := ReadSettings(context.Background(), st.Pool())
	if err != nil {
		t.Fatal(err)
	}
	if !s.V2() || !s.DeleteOriginals() || s.TrashGrace != 7*24*time.Hour || len(s.Problems) != 0 {
		t.Errorf("read: %+v", s)
	}
}

// The work folder gets its folders, ARRIVALS_ROOT and EXTRAS_ROOT where they
// are configured, group-writable; again, nothing changes.
func TestEnsureWorkTree(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{WorkRoot: dir + "/.work", ArrivalsRoot: dir + "/drop", ExtrasRoot: dir + "/.work/extras"}
	for i := 0; i < 2; i++ {
		if err := EnsureWorkTree(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"drop", ".work/extras", ".work/replace", ".work/inbox", ".work/staging", ".work/trash",
		".work/quarantine", ".work/migration", ".work/legacy"} {
		if fi, err := os.Stat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	if err := EnsureWorkTree(config.Config{WorkRoot: "relative", ArrivalsRoot: dir, ExtrasRoot: dir}); err == nil {
		t.Error("a relative work folder")
	}
}
