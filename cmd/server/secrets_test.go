package main

import (
	"context"
	"testing"

	"github.com/zaentrum/katalog-manager/internal/store/storetest"
)

// A TMDB token, through the service as main wires it with the bearer tokens of
// a realm: a viewer and the service account are refused before anything is
// checked or stored; an admin's token TMDB refuses is not stored, the answer
// an error with the code SECRET_REFUSED; one TMDB could not be asked about is
// stored all the same, its check unchecked, and one TMDB takes stored, valid.
// No answer holds a token (newInstance's cleanup reads every one).
func TestATMDBTokenIsCheckedThroughTheService(t *testing.T) {
	in := newInstance(t)
	set := func(token, value string) gqlAnswer {
		t.Helper()
		_, a := in.gql(t, "/api/manage/query", token,
			`mutation { setSecretSetting(key: "tmdb.api_key", value: "`+value+`") { key isSet check { status message } } }`)
		return a
	}
	stored := func() string {
		t.Helper()
		var v string
		if n := storetest.Count(t, in.st, `SELECT count(*) FROM com_nalet_katalog_settings WHERE key = 'tmdb.api_key'`); n != 1 {
			t.Fatalf("%d rows of tmdb.api_key", n)
		}
		if err := in.st.Pool().QueryRow(context.Background(), `SELECT valuetext FROM com_nalet_katalog_settings WHERE key = 'tmdb.api_key'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	for _, who := range []struct{ name, token string }{{"a viewer", in.iss.Viewer(t)}, {"the service account", in.iss.Service(t, "zaentrum-manager")}} {
		if a := set(who.token, goodToken); len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "FORBIDDEN" {
			t.Errorf("%s: %s %v, want it refused", who.name, a.Data, a.Errors)
		}
	}
	if calls := in.f.take(); len(calls) > 0 {
		t.Errorf("refused callers had the token checked: %q", calls)
	}
	if v := stored(); v != tmdbSecret {
		t.Fatal("a refused caller changed the TMDB key")
	}

	admin := in.iss.Admin(t)
	a := set(admin, refusedToken)
	if string(a.Data) != "null" || len(a.Errors) != 1 || a.Errors[0].Extensions["code"] != "SECRET_REFUSED" ||
		a.Errors[0].Extensions["key"] != "tmdb.api_key" ||
		a.Errors[0].Message != "tmdb.api_key: TMDB refused the token (HTTP 401); nothing was saved" {
		t.Errorf("a token TMDB refuses: %s %+v", a.Data, a.Errors)
	}
	if stored() != tmdbSecret {
		t.Error("a token TMDB refused was stored")
	}
	for token, want := range map[string]string{
		uncheckedToken: `{"setSecretSetting":{"key":"tmdb.api_key","isSet":true,"check":{"status":"unchecked",` +
			`"message":"could not check the token with TMDB (HTTP 503): it is saved all the same"}}}`,
		goodToken: `{"setSecretSetting":{"key":"tmdb.api_key","isSet":true,"check":{"status":"valid","message":"TMDB took the token"}}}`,
	} {
		if a := set(admin, token); len(a.Errors) > 0 || string(a.Data) != want {
			t.Errorf("%s:\n got  %s %v\n want %s", token, a.Data, a.Errors, want)
		}
		if stored() != token {
			t.Errorf("%s was not stored", token)
		}
	}
	if calls := in.f.take(); len(calls) != 3 {
		t.Errorf("an admin's three tokens were checked %q, want three times", calls)
	}
}
