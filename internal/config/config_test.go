package config

import (
	"strings"
	"testing"
	"time"
)

// TMDB_REFRESH_INTERVAL is a Go duration, 24h when unset or unreadable; 0 and
// off turn the change-list refresh off.
func TestTMDBRefreshInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"": 24 * time.Hour, "6h": 6 * time.Hour, "90m": 90 * time.Minute, "0": 0, "off": 0, "OFF": 0,
		"false": 0, "disabled": 0, "daily": 24 * time.Hour, "-1h": 0,
	} {
		t.Setenv("TMDB_REFRESH_INTERVAL", v)
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.TMDBRefreshInterval; got != want {
			t.Errorf("TMDB_REFRESH_INTERVAL=%q: %s, want %s", v, got, want)
		}
	}
}

// KATALOG_CREDIT_ROLES names the roles credits follow TMDB in, every one of
// them when unset, each once and in the order a title lists its credits.
func TestCreditRoles(t *testing.T) {
	all := "actor creator director writer producer composer cinematographer editor"
	for v, want := range map[string]string{
		"":                                all,
		"   ":                             all,
		"actor,director":                  "actor director",
		" writer , actor ,actor,":         "actor writer",
		"editor,creator,composer":         "creator composer editor",
		strings.ReplaceAll(all, " ", ","): all,
	} {
		t.Setenv("KATALOG_CREDIT_ROLES", v)
		cfg, err := Load()
		if err != nil {
			t.Errorf("KATALOG_CREDIT_ROLES=%q: %v", v, err)
			continue
		}
		if got := strings.Join(cfg.CreditRoles, " "); got != want {
			t.Errorf("KATALOG_CREDIT_ROLES=%q: %s, want %s", v, got, want)
		}
	}
}

// Anything in KATALOG_CREDIT_ROLES that is not a role TMDB's credits give
// fails loading the configuration, saying what is wrong: a mistyped role
// would otherwise drop every credit in the role meant.
func TestCreditRolesThatAreNoneFailLoading(t *testing.T) {
	for v, says := range map[string]string{
		"Actor":           `"Actor" is not a role`,
		"actor;director":  `"actor;director" is not a role`,
		"actor, 2nd unit": `"2nd unit" is not a role`,
		"actors":          `TMDB's credits give no role "actors"; they give actor, creator, director,`,
		"actor,gaffer":    `TMDB's credits give no role "gaffer"`,
		",":               "names no role",
		" , ,":            "names no role",
	} {
		t.Setenv("KATALOG_CREDIT_ROLES", v)
		_, err := Load()
		if err == nil || !strings.HasPrefix(err.Error(), "KATALOG_CREDIT_ROLES") || !strings.Contains(err.Error(), says) {
			t.Errorf("KATALOG_CREDIT_ROLES=%q: %v, want an error that says %s", v, err, says)
		}
	}
}
