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
