package graph

import (
	"context"
	"strings"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store"
	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// The secret values the tests store; no answer may hold one.
var secretValues = []string{"tmdb-v4-read-token", "fanart-personal-key", "alerts-token", "new-omdb-key", "second-tmdb-token"}

// exec runs q as an admin, failing t on an error unless failing says it
// should fail; it fails t when the answer holds a secret value.
func exec(t *testing.T, st *store.Store, q string, failing bool) string {
	t.Helper()
	resp := MustSchema(NewResolver(st, testConfig, Services{})).Exec(as(admin), q, "", nil)
	if failing != (len(resp.Errors) > 0) {
		t.Fatalf("%s: errors %v, want them: %v", q, resp.Errors, failing)
	}
	answer := string(resp.Data)
	for _, e := range resp.Errors {
		answer += " " + e.Message
	}
	for _, v := range secretValues {
		if strings.Contains(answer, v) {
			t.Fatalf("%s: the answer holds the secret %q: %s", q, v, answer)
		}
	}
	return answer
}

func storedValue(t *testing.T, st *store.Store, key string) []string {
	t.Helper()
	rows, err := st.Pool().Query(context.Background(), `SELECT valuetext FROM com_nalet_katalog_settings WHERE key = $1 ORDER BY id`, key)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func someSettings(t *testing.T, st *store.Store) {
	t.Helper()
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, createdat, modifiedat, key, valuetext, valuetype, description) VALUES
		('s1', '2026-09-01 10:00:00', '2026-10-01 08:00:00', 'tmdb.api_key', 'tmdb-v4-read-token', 'string', 'TMDB'),
		('s2', '2026-09-01 10:00:00', NULL, 'omdb.api_key', '  ', 'string', NULL),
		('s3', '2026-09-02 10:00:00', '2026-09-03 10:00:00', 'fanart.client_key', 'fanart-personal-key', 'string', NULL),
		('s4', '2026-09-04 10:00:00', NULL, 'alerts.token', 'alerts-token', 'string', NULL),
		('s5', '2026-09-05 10:00:00', '2026-09-06 10:00:00', 'validate.small_file_threshold_mb', '50', 'int', 'small files'),
		('s6', '2026-09-07 10:00:00', NULL, 'packager.languages', 'en,de', 'list_csv', NULL)`)
}

// The settings query never returns a secret's value: a secret says it is one,
// whether it is set and when it was written; any other setting keeps its
// value.
func TestSettingsKeepSecretsWriteOnly(t *testing.T) {
	st := storetest.Open(t)
	someSettings(t, st)
	got := exec(t, st, `{ settings { id key valueText valueType description isSecret isSet updatedAt } }`, false)
	want := `{"settings":[` +
		`{"id":"s4","key":"alerts.token","valueText":null,"valueType":"string","description":null,"isSecret":true,"isSet":true,"updatedAt":"2026-09-04T10:00:00Z"},` +
		`{"id":"s3","key":"fanart.client_key","valueText":null,"valueType":"string","description":null,"isSecret":true,"isSet":true,"updatedAt":"2026-09-03T10:00:00Z"},` +
		`{"id":"s2","key":"omdb.api_key","valueText":null,"valueType":"string","description":null,"isSecret":true,"isSet":false,"updatedAt":"2026-09-01T10:00:00Z"},` +
		`{"id":"s6","key":"packager.languages","valueText":"en,de","valueType":"list_csv","description":null,"isSecret":false,"isSet":true,"updatedAt":"2026-09-07T10:00:00Z"},` +
		`{"id":"s1","key":"tmdb.api_key","valueText":null,"valueType":"string","description":"TMDB","isSecret":true,"isSet":true,"updatedAt":"2026-10-01T08:00:00Z"},` +
		`{"id":"s5","key":"validate.small_file_threshold_mb","valueText":"50","valueType":"int","description":"small files","isSecret":false,"isSet":true,"updatedAt":"2026-09-06T10:00:00Z"}]}`
	if got != want {
		t.Errorf("settings:\n got  %s\n want %s", got, want)
	}
}

// setSecretSetting sets a secret (trimmed), creating it when there is none,
// every row of a key twice in the table; it refuses a setting that is no
// secret and a blank value. clearSecretSetting deletes it. Neither answers
// with the value.
func TestSetAndClearASecret(t *testing.T) {
	st := storetest.Open(t)
	someSettings(t, st)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_settings (id, key, valuetext) VALUES ('s0', 'tmdb.api_key', 'an old twin')`)

	if got := exec(t, st, `mutation { setSecretSetting(key: "omdb.api_key", value: "  new-omdb-key\n") { id key valueText isSecret isSet } }`, false); got !=
		`{"setSecretSetting":{"id":"s2","key":"omdb.api_key","valueText":null,"isSecret":true,"isSet":true}}` {
		t.Errorf("set omdb.api_key: %s", got)
	}
	if v := storedValue(t, st, "omdb.api_key"); len(v) != 1 || v[0] != "new-omdb-key" {
		t.Errorf("omdb.api_key holds %q, want the new key, trimmed", v)
	}
	exec(t, st, `mutation { setSecretSetting(key: "tmdb.api_key", value: "second-tmdb-token") { id } }`, false)
	if v := storedValue(t, st, "tmdb.api_key"); len(v) != 2 || v[0] != "second-tmdb-token" || v[1] != "second-tmdb-token" {
		t.Errorf("tmdb.api_key holds %q, want the new token in both rows", v)
	}
	if got := exec(t, st, `mutation { setSecretSetting(key: "fanart.api_key", value: "fanart-project") { key valueText valueType isSet } }`, false); got !=
		`{"setSecretSetting":{"key":"fanart.api_key","valueText":null,"valueType":"string","isSet":true}}` {
		t.Errorf("set fanart.api_key: %s", got)
	}
	if v := storedValue(t, st, "fanart.api_key"); len(v) != 1 || v[0] != "fanart-project" {
		t.Errorf("fanart.api_key holds %q, want it created", v)
	}

	for q, says := range map[string]string{
		`mutation { setSecretSetting(key: "packager.languages", value: "fr") { id } }`: `"packager.languages" is not a secret setting`,
		`mutation { setSecretSetting(key: "alerts.token", value: "  ") { id } }`:       "alerts.token: a secret setting is cleared with clearSecretSetting",
		`mutation { clearSecretSetting(key: "packager.languages") }`:                   `"packager.languages" is not a secret setting`,
	} {
		if got := exec(t, st, q, true); !strings.Contains(got, says) {
			t.Errorf("%s: %s, want it refused saying %s", q, got, says)
		}
	}
	if v := storedValue(t, st, "packager.languages"); len(v) != 1 || v[0] != "en,de" {
		t.Errorf("packager.languages holds %q after refusals", v)
	}
	if v := storedValue(t, st, "alerts.token"); len(v) != 1 || v[0] != "alerts-token" {
		t.Errorf("alerts.token holds %q after a blank set", v)
	}

	if got := exec(t, st, `mutation { clearSecretSetting(key: "tmdb.api_key") }`, false); got != `{"clearSecretSetting":true}` {
		t.Errorf("clear tmdb.api_key: %s", got)
	}
	if v := storedValue(t, st, "tmdb.api_key"); len(v) != 0 {
		t.Errorf("tmdb.api_key holds %q after clearing", v)
	}
	if got := exec(t, st, `mutation { clearSecretSetting(key: "tmdb.api_key") }`, false); got != `{"clearSecretSetting":false}` {
		t.Errorf("clear tmdb.api_key again: %s", got)
	}
}

// createSetting and updateSetting refuse a secret, so its value has one way
// in and none out; a setting that is none they write as before.
func TestTheGeneralSettingWritesRefuseSecrets(t *testing.T) {
	st := storetest.Open(t)
	someSettings(t, st)
	for q, says := range map[string]string{
		`mutation { createSetting(key: "tmdb.api_key", valueText: "x") { id } }`:  "tmdb.api_key is a secret setting: set it with setSecretSetting",
		`mutation { createSetting(key: "smtp.password", valueText: "x") { id } }`: "smtp.password is a secret setting",
		`mutation { createSetting(key: "license.key ", valueText: "x") { id } }`:  "license.key  is a secret setting",
		`mutation { updateSetting(id: "s3", valueText: "x") { id } }`:             "fanart.client_key is a secret setting",
		`mutation { updateSetting(id: "s4", description: "x") { id } }`:           "alerts.token is a secret setting",
	} {
		if got := exec(t, st, q, true); !strings.Contains(got, says) {
			t.Errorf("%s: %s, want it refused saying %s", q, got, says)
		}
	}
	if n := storetest.Count(t, st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key IN ('tmdb.api_key', 'smtp.password')`); n != 1 {
		t.Errorf("%d rows of tmdb.api_key and smtp.password, want only the one there was", n)
	}
	if v := storedValue(t, st, "fanart.client_key"); len(v) != 1 || v[0] != "fanart-personal-key" {
		t.Errorf("fanart.client_key holds %q after a refused update", v)
	}
	if got := exec(t, st, `mutation { createSetting(key: "scanner.roots", valueText: "/a", valueType: "list_csv") { key valueText isSecret isSet } }`, false); got !=
		`{"createSetting":{"key":"scanner.roots","valueText":"/a","isSecret":false,"isSet":true}}` {
		t.Errorf("create scanner.roots: %s", got)
	}
	if got := exec(t, st, `mutation { updateSetting(id: "s5", valueText: "60") { key valueText } }`, false); got !=
		`{"updateSetting":{"key":"validate.small_file_threshold_mb","valueText":"60"}}` {
		t.Errorf("update the threshold: %s", got)
	}
	if got := exec(t, st, `mutation { deleteSetting(id: "s4") }`, false); got != `{"deleteSetting":true}` {
		t.Errorf("delete alerts.token: %s", got)
	}
}

// A secret is a key the service reads as an API key, or one that names a
// credential; the rest keep their values.
func TestIsSecretSetting(t *testing.T) {
	for key, want := range map[string]bool{
		"tmdb.api_key": true, "omdb.api_key": true, "fanart.api_key": true, "fanart.client_key": true,
		"TMDB_API_KEY": true, "subtitles.apikey": true, "webhooks.api-key": true, "smtp.password": true,
		"db.passwd": true, "oauth.client_secret": true, "Secret": true, "trailers.token": true,
		"registry.credentials": true, "license.key": true, "signing_key": true, "key": true,
		" license.key ": true, "signing_key\n": true, "\tkey": true,
		"validate.small_file_threshold_mb": false, "packager.languages": false, "scanner.roots": false,
		"monkey.count": false, "keyboard.layout": false, "keys.sorted": false, "": false,
	} {
		if got := isSecretSetting(key); got != want {
			t.Errorf("isSecretSetting(%q) = %v, want %v", key, got, want)
		}
	}
}
