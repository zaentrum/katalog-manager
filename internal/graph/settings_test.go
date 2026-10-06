package graph

import (
	"context"
	"slices"
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

// fakeChecker checks the TMDB key as TMDB would by the value: one it refuses,
// one it cannot be asked about, any other it takes. It records what it is
// asked, keys and values, and checks no other key.
type fakeChecker struct{ asked []string }

func (f *fakeChecker) CheckSecret(_ context.Context, key, value string) SecretCheck {
	f.asked = append(f.asked, key+"="+value)
	switch {
	case key != "tmdb.api_key":
		return SecretCheck{}
	case value == "refused-tmdb-token":
		return SecretCheck{Status: SecretRefused, Message: "TMDB refused the token (HTTP 401)"}
	case value == "unchecked-tmdb-token":
		return SecretCheck{Status: SecretUnchecked, Message: "could not check the token with TMDB (HTTP 503): it is saved all the same"}
	}
	return SecretCheck{Status: SecretValid, Message: "TMDB took the token"}
}

// setSecretSetting has a TMDB token checked before it stores it: one TMDB
// refuses is not stored, the answer an error that says so with the code
// SECRET_REFUSED and the key, and the token held before stays; one TMDB takes,
// and one TMDB could not be asked about, are stored, the answer's check
// saying which. A key nothing checks is stored with no check, and without a
// checker every secret is. No answer holds a value.
func TestASecretIsCheckedBeforeItIsStored(t *testing.T) {
	st := storetest.Open(t)
	someSettings(t, st)
	secrets := append(slices.Clone(secretValues), "refused-tmdb-token", "unchecked-tmdb-token", "a-good-tmdb-token", "another-omdb-key", "unchecked-twice")
	ck := &fakeChecker{}
	schema := MustSchema(NewResolver(st, testConfig, Services{Secrets: ck}))
	run := func(q string) (string, []map[string]any) {
		t.Helper()
		resp := schema.Exec(as(admin), q, "", nil)
		answer := string(resp.Data)
		var exts []map[string]any
		for _, e := range resp.Errors {
			answer += " " + e.Message
			exts = append(exts, e.Extensions)
		}
		for _, v := range secrets {
			if strings.Contains(answer, v) {
				t.Fatalf("%s: the answer holds the secret %q: %s", q, v, answer)
			}
		}
		return answer, exts
	}

	got, exts := run(`mutation { setSecretSetting(key: "tmdb.api_key", value: " refused-tmdb-token ") { key isSet check { status message } } }`)
	if want := "null tmdb.api_key: TMDB refused the token (HTTP 401); nothing was saved"; got != want ||
		len(exts) != 1 || exts[0]["code"] != "SECRET_REFUSED" || exts[0]["key"] != "tmdb.api_key" {
		t.Errorf("a token TMDB refuses: %s %v\n want %s", got, exts, want)
	}
	if v := storedValue(t, st, "tmdb.api_key"); len(v) != 1 || v[0] != "tmdb-v4-read-token" {
		t.Errorf("tmdb.api_key holds %q after a token TMDB refused, want the one it held", v)
	}
	for value, check := range map[string]string{
		"unchecked-tmdb-token": `{"status":"unchecked","message":"could not check the token with TMDB (HTTP 503): it is saved all the same"}`,
		"a-good-tmdb-token":    `{"status":"valid","message":"TMDB took the token"}`,
	} {
		got, _ := run(`mutation { setSecretSetting(key: "tmdb.api_key", value: "` + value + `") { key isSet check { status message } } }`)
		if want := `{"setSecretSetting":{"key":"tmdb.api_key","isSet":true,"check":` + check + `}}`; got != want {
			t.Errorf("%s:\n got  %s\n want %s", value, got, want)
		}
		if v := storedValue(t, st, "tmdb.api_key"); len(v) != 1 || v[0] != value {
			t.Errorf("tmdb.api_key holds %q, want %s stored", v, value)
		}
	}
	if got, _ := run(`mutation { setSecretSetting(key: "omdb.api_key", value: "another-omdb-key") { key check { status } } }`); got !=
		`{"setSecretSetting":{"key":"omdb.api_key","check":null}}` {
		t.Errorf("a key nothing checks: %s", got)
	}
	if v := storedValue(t, st, "omdb.api_key"); len(v) != 1 || v[0] != "another-omdb-key" {
		t.Errorf("omdb.api_key holds %q", v)
	}
	if got, _ := run(`{ settings { key check { status } } }`); strings.Contains(got, `"check":{`) {
		t.Errorf("the settings query carries a check: %s", got)
	}
	want := []string{"tmdb.api_key=refused-tmdb-token", "tmdb.api_key=unchecked-tmdb-token", "tmdb.api_key=a-good-tmdb-token", "omdb.api_key=another-omdb-key"}
	if len(ck.asked) != len(want) {
		t.Fatalf("the checker was asked %q, want %q", ck.asked, want)
	}
	for _, w := range want {
		if !slices.Contains(ck.asked, w) {
			t.Errorf("the checker was asked %q, want %q among it (trimmed)", ck.asked, w)
		}
	}

	// A blank value, or a setting that is no secret, is refused before
	// anything is checked.
	ck.asked = nil
	run(`mutation { setSecretSetting(key: "tmdb.api_key", value: "  ") { key } }`)
	run(`mutation { setSecretSetting(key: "packager.languages", value: "fr") { key } }`)
	if len(ck.asked) != 0 {
		t.Errorf("the checker was asked %q for values refused before", ck.asked)
	}

	// Without a checker the token is stored, unchecked by anyone.
	plain := MustSchema(NewResolver(st, testConfig, Services{}))
	if resp := plain.Exec(as(admin), `mutation { setSecretSetting(key: "tmdb.api_key", value: "unchecked-twice") { check { status } } }`, "", nil); len(resp.Errors) > 0 ||
		string(resp.Data) != `{"setSecretSetting":{"check":null}}` {
		t.Errorf("without a checker: %s %v", resp.Data, resp.Errors)
	}
	if v := storedValue(t, st, "tmdb.api_key"); len(v) != 1 || v[0] != "unchecked-twice" {
		t.Errorf("tmdb.api_key holds %q without a checker", v)
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

// The library's layout does not change while a title or an extra is between
// its transcode and its package: the transcode's handoff lies in the inbox of
// the layout it ran in. A write that changes it is refused, saying how many
// wait (LAYOUT_BUSY); one that keeps it, and any once nothing waits, goes.
func TestTheLayoutDoesNotChangeUnderAHandoff(t *testing.T) {
	st := storetest.Open(t)
	storetest.AddItem(t, st, "m1", "movie", "Sintel", "")
	storetest.AddItem(t, st, "m2", "movie", "Tears of Steel", "")
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status) VALUES
		('t1', 'm1', 'transcode', 'done'), ('p1', 'm1', 'package', 'pending'), ('t2', 'm2', 'transcode', 'in_progress')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemextras (id, item_id, kind, title, registeredby, state) VALUES
		('x1', 'm1', 'trailer', 'Trailer', 'api', 'transcoded'), ('x2', 'm1', 'teaser', 'Teaser', 'api', 'ready')`)
	busy := "library.layout stays legacy: 2 titles and 1 extras are between their transcode and their package"
	if got := exec(t, st, `mutation { createSetting(key: " library.layout", valueText: "v2") { id } }`, true); !strings.Contains(got, busy) {
		t.Errorf("create v2: %s, want it refused saying %s", got, busy)
	}
	if got := exec(t, st, `mutation { createSetting(key: "library.layout", valueText: "legacy") { id } }`, false); !strings.Contains(got, "createSetting") {
		t.Errorf("create legacy: %s", got)
	}
	var id string
	if err := st.Pool().QueryRow(context.Background(), `SELECT id FROM com_nalet_katalog_settings WHERE key = 'library.layout'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if got := exec(t, st, `mutation { updateSetting(id: "`+id+`", valueText: "V2") { id } }`, true); !strings.Contains(got, busy) {
		t.Errorf("update to v2: %s", got)
	}
	// Once the packager is done, it goes; back again is refused while
	// another waits, and the delete that would make it legacy too.
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'done' WHERE id IN ('p1', 't2')`)
	storetest.Exec(t, st, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, item_id, step, status) VALUES ('p2', 'm2', 'package', 'done')`)
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemextras SET state = 'ready' WHERE id = 'x1'`)
	if got := exec(t, st, `mutation { updateSetting(id: "`+id+`", valueText: "v2") { valueText } }`, false); got !=
		`{"updateSetting":{"valueText":"v2"}}` {
		t.Errorf("update to v2 once nothing waits: %s", got)
	}
	storetest.Exec(t, st, `UPDATE com_nalet_katalog_itemprocessingsteps SET status = 'failed', nextretryat = now() WHERE id = 'p2'`)
	if got := exec(t, st, `mutation { deleteSetting(id: "`+id+`") }`, true); !strings.Contains(got, "library.layout stays v2: 1 titles") {
		t.Errorf("delete: %s", got)
	}
	if got := exec(t, st, `mutation { updateSetting(id: "`+id+`", description: "the layout") { valueText } }`, false); got !=
		`{"updateSetting":{"valueText":"v2"}}` {
		t.Errorf("a description: %s", got)
	}
}
