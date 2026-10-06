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

// Who may do what: the admin and addon roles, where roles are read, and the
// service account's clients. Unset, they are what the platform's realm uses,
// and the service account is the client the deployment gives the service for
// the workers' client credentials.
func TestAccessPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		env                       map[string]string
		admin, addon, claim, svcs string
	}{
		{"unset", nil, "zaentrum-admin", "zaentrum-addon", "realm_access.roles", "zaentrum-manager"},
		{"the deployment's worker client", map[string]string{"KEYCLOAK_KATALOG_CLIENT_ID": "zaentrum-instance-workers"},
			"zaentrum-admin", "zaentrum-addon", "realm_access.roles", "zaentrum-instance-workers"},
		{"named", map[string]string{"KATALOG_ADMIN_ROLE": "catalog-admin", "KATALOG_ADDON_ROLE": "catalog-addon",
			"KATALOG_ROLES_CLAIM": "resource_access.katalog.roles", "KATALOG_SERVICE_CLIENTS": " workers , scan-job,workers,",
			"KEYCLOAK_KATALOG_CLIENT_ID": "zaentrum-instance-workers"},
			"catalog-admin", "catalog-addon", "resource_access.katalog.roles", "workers scan-job"},
		{"blank is unset", map[string]string{"KATALOG_ADMIN_ROLE": " ", "KATALOG_ROLES_CLAIM": " ", "KATALOG_SERVICE_CLIENTS": " "},
			"zaentrum-admin", "zaentrum-addon", "realm_access.roles", "zaentrum-manager"},
		{"no service clients", map[string]string{"KATALOG_SERVICE_CLIENTS": ","}, "zaentrum-admin", "zaentrum-addon", "realm_access.roles", ""},
	} {
		for _, k := range []string{"KATALOG_ADMIN_ROLE", "KATALOG_ADDON_ROLE", "KATALOG_ROLES_CLAIM", "KATALOG_SERVICE_CLIENTS", "KEYCLOAK_KATALOG_CLIENT_ID"} {
			t.Setenv(k, tc.env[k])
		}
		cfg, err := Load()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		pol := cfg.Policy()
		if pol.AdminRole != tc.admin || pol.AddonRole != tc.addon || pol.RolesClaim != tc.claim ||
			strings.Join(pol.ServiceClients, " ") != tc.svcs {
			t.Errorf("%s: %+v, want admin %s, addon %s, claim %s, service clients %q", tc.name, pol, tc.admin, tc.addon, tc.claim, tc.svcs)
		}
	}
}

// A KATALOG_ROLES_CLAIM that is no claim path fails loading: nobody's roles
// would be found, so nobody could administer the catalog.
func TestRolesClaimThatIsNoPathFailsLoading(t *testing.T) {
	for _, v := range []string{"realm_access..roles", ".roles", "roles.", "realm_access. roles"} {
		t.Setenv("KATALOG_ROLES_CLAIM", v)
		_, err := Load()
		if err == nil || !strings.HasPrefix(err.Error(), "KATALOG_ROLES_CLAIM") || !strings.Contains(err.Error(), "not a claim path") {
			t.Errorf("KATALOG_ROLES_CLAIM=%q: %v, want an error that says it is not a claim path", v, err)
		}
	}
}

// The retries of the processing steps: three runs in a row, a backoff from a
// minute to an hour, a sweep every 30s and the steps' default timeouts,
// unless the KATALOG_RETRY_* settings and KATALOG_STEP_TIMEOUTS say other.
func TestRetryPolicy(t *testing.T) {
	keys := []string{"KATALOG_RETRY_MAX_ATTEMPTS", "KATALOG_RETRY_BACKOFF", "KATALOG_RETRY_BACKOFF_MAX", "KATALOG_RETRY_INTERVAL", "KATALOG_STEP_TIMEOUTS"}
	for _, tc := range []struct {
		name              string
		env               map[string]string
		attempts          int
		backoff, max, run time.Duration
		transcode, tmdb   time.Duration
	}{
		{"unset", nil, 3, time.Minute, time.Hour, 30 * time.Second, 6 * time.Hour, 15 * time.Minute},
		{"set", map[string]string{"KATALOG_RETRY_MAX_ATTEMPTS": " 5 ", "KATALOG_RETRY_BACKOFF": "30s", "KATALOG_RETRY_BACKOFF_MAX": "10m",
			"KATALOG_RETRY_INTERVAL": "1m", "KATALOG_STEP_TIMEOUTS": "transcode=12h, tmdb=5m"},
			5, 30 * time.Second, 10 * time.Minute, time.Minute, 12 * time.Hour, 5 * time.Minute},
		{"one attempt, the sweep off", map[string]string{"KATALOG_RETRY_MAX_ATTEMPTS": "1", "KATALOG_RETRY_INTERVAL": "off"},
			1, time.Minute, time.Hour, 0, 6 * time.Hour, 15 * time.Minute},
		{"the sweep at 0", map[string]string{"KATALOG_RETRY_INTERVAL": "0"}, 3, time.Minute, time.Hour, 0, 6 * time.Hour, 15 * time.Minute},
	} {
		for _, k := range keys {
			t.Setenv(k, tc.env[k])
		}
		cfg, err := Load()
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		p := cfg.RetryPolicy()
		if p.MaxAttempts != tc.attempts || p.Backoff != tc.backoff || p.BackoffMax != tc.max || cfg.RetryInterval != tc.run ||
			p.Timeout("transcode") != tc.transcode || p.Timeout("tmdb") != tc.tmdb || p.Timeout("package") != 2*time.Hour {
			t.Errorf("%s: %+v, interval %s", tc.name, p, cfg.RetryInterval)
		}
	}
}

// A retry setting that is no number or duration it can take fails loading,
// saying which and why.
func TestRetrySettingsThatAreNoneFailLoading(t *testing.T) {
	for _, tc := range []struct{ key, v, says string }{
		{"KATALOG_RETRY_MAX_ATTEMPTS", "0", "not a number of attempts"},
		{"KATALOG_RETRY_MAX_ATTEMPTS", "three", "not a number of attempts"},
		{"KATALOG_RETRY_MAX_ATTEMPTS", "-2", "not a number of attempts"},
		{"KATALOG_RETRY_BACKOFF", "soon", "not a duration above zero"},
		{"KATALOG_RETRY_BACKOFF", "0s", "not a duration above zero"},
		{"KATALOG_RETRY_BACKOFF_MAX", "-1m", "not a duration above zero"},
		{"KATALOG_RETRY_BACKOFF_MAX", "30s", "is shorter than KATALOG_RETRY_BACKOFF"},
		{"KATALOG_RETRY_INTERVAL", "often", "not a duration"},
		{"KATALOG_RETRY_INTERVAL", "-5s", "not a duration"},
		{"KATALOG_STEP_TIMEOUTS", "encode=2h", "KATALOG_STEP_TIMEOUTS: \"encode=2h\" is not a step's timeout"},
		{"KATALOG_STEP_TIMEOUTS", "transcode=0s", "a duration above zero"},
	} {
		for _, k := range []string{"KATALOG_RETRY_MAX_ATTEMPTS", "KATALOG_RETRY_BACKOFF", "KATALOG_RETRY_BACKOFF_MAX", "KATALOG_RETRY_INTERVAL", "KATALOG_STEP_TIMEOUTS"} {
			t.Setenv(k, "")
		}
		t.Setenv(tc.key, tc.v)
		_, err := Load()
		if err == nil || !strings.HasPrefix(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s=%q: %v, want an error that says %s", tc.key, tc.v, err, tc.says)
		}
	}
}

// The library's roots are the layout's: with no variable set, the legacy
// layout's are what they always were (the media root, the v1 record's folder
// and the extras' folder of the share), the v2 layout's the design's (the
// share's root, its .work/ folder, and in that incoming/ and extras/). A
// variable that is set wins in both layouts.
func TestTheRootsOfTheLibrary(t *testing.T) {
	type roots struct{ media, library, work, arrivals, extras string }
	for _, tc := range []struct {
		env        map[string]string
		legacy, v2 roots
	}{
		{nil,
			roots{"/var/lib/katalog/media", "/var/lib/katalog/library", "/var/lib/katalog/.work", "/var/lib/katalog/.work/incoming",
				"/var/lib/katalog/extras"},
			roots{"/var/lib/katalog/media", "/var/lib/katalog", "/var/lib/katalog/.work", "/var/lib/katalog/.work/incoming",
				"/var/lib/katalog/.work/extras"}},
		{map[string]string{"SCANNER_NFS_ROOT": "/srv/media", "LIBRARY_ROOT": "/srv/share/", "EXTRAS_ROOT": " /srv/extras "},
			roots{"/srv/media", "/srv/share", "/srv/share/.work", "/srv/share/.work/incoming", "/srv/extras"},
			roots{"/srv/media", "/srv/share", "/srv/share/.work", "/srv/share/.work/incoming", "/srv/extras"}},
		{map[string]string{"WORK_ROOT": "/scratch/work", "ARRIVALS_ROOT": "/drop"},
			roots{"/var/lib/katalog/media", "/var/lib/katalog/library", "/scratch/work", "/drop", "/var/lib/katalog/extras"},
			roots{"/var/lib/katalog/media", "/var/lib/katalog", "/scratch/work", "/drop", "/scratch/work/extras"}},
	} {
		for _, k := range []string{"SCANNER_NFS_ROOT", "NFS_ROOT", "LIBRARY_ROOT", "WORK_ROOT", "ARRIVALS_ROOT", "EXTRAS_ROOT"} {
			t.Setenv(k, tc.env[k])
		}
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		for v2, want := range map[bool]roots{false: tc.legacy, true: tc.v2} {
			r := cfg.Roots(v2)
			if got := (roots{cfg.NFSRoot, r.Library, r.Work, r.Arrivals, r.Extras}); got != want {
				t.Errorf("%v, v2 %v:\n got  %+v\n want %+v", tc.env, v2, got, want)
			}
		}
	}
}
