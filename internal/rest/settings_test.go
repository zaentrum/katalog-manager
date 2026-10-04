package rest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// GET /api/settings answers the workers every setting that is no secret, in
// the shape the packager reads ({key: {valueText, ...}}); a secret is left
// out, set or blank, and its value appears nowhere in the answer. A key comes
// without the spaces around it, a key held twice once (its lowest id).
func TestTheWorkersReadTheSettingsThatAreNoSecret(t *testing.T) {
	st := storetest.Open(t)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext, valuetype) VALUES
		('s1', 'tmdb.api_key', 'tmdb-secret-token', 'string'),
		('s2', 'omdb.api_key', '', 'string'),
		('s3', 'alerts.token', 'alerts-secret', 'string'),
		('s4', ' license.key ', 'license-secret', 'string'),
		('s5', 'packager.language_whitelist', 'en,de', 'list_csv'),
		('s6', 'packager.language_whitelist', 'fr', 'list_csv'),
		('s7', 'packager.keep_original_if_single', 'false', 'bool'),
		('s8', 'validate.small_file_threshold_mb', '50', 'int'),
		('s9', '  scanner.depth ', '', 'int')`)
	h, iss := server(t, st, testConfig(t.TempDir()))

	w := do(h, http.MethodGet, "/api/settings", "", iss.Service(t, "zaentrum-manager"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/settings as the service account: %d %s", w.Code, w.Body.String())
	}
	body := strings.TrimSpace(w.Body.String())
	want := `{"packager.keep_original_if_single":{"valueText":"false","valueType":"bool"},` +
		`"packager.language_whitelist":{"valueText":"en,de","valueType":"list_csv"},` +
		`"scanner.depth":{"valueText":"","valueType":"int"},` +
		`"validate.small_file_threshold_mb":{"valueText":"50","valueType":"int"}}`
	if body != want {
		t.Errorf("the settings:\n got  %s\n want %s", body, want)
	}
	for _, secret := range []string{"tmdb", "omdb", "alerts", "license"} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer names or holds a secret (%s): %s", secret, body)
		}
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", cc)
	}

	// What the packager makes of it: {k: (v.get("valueText") or "") for k, v
	// in raw.items() if isinstance(v, dict)}, then its two keys.
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	flat := map[string]string{}
	for k, v := range raw {
		if m, ok := v.(map[string]any); ok {
			s, _ := m["valueText"].(string)
			flat[k] = s
		}
	}
	if flat["packager.language_whitelist"] != "en,de" || flat["packager.keep_original_if_single"] != "false" {
		t.Errorf("the packager reads %v", flat)
	}

	// No settings at all is an empty object, which the packager reads as
	// its defaults.
	storetest.Exec(t, st, `DELETE FROM com_nalet_katalog_settings`)
	if w := do(h, http.MethodGet, "/api/settings", "", iss.Admin(t)); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{}` {
		t.Errorf("no settings: %d %s, want {}", w.Code, w.Body.String())
	}
}
