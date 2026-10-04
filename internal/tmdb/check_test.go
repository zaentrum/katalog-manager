package tmdb

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/graph"
)

// checker is a Service whose TMDB is f, waiting wait for a check.
func checker(f *fakeTMDB, base string, wait time.Duration) *Service {
	c := newClient(func() string { return "the-key-in-effect" }, "en-US")
	c.apiBase = base
	if f != nil {
		c.apiBase = f.srv.URL + "/3"
	}
	c.http = &http.Client{Transport: loopbackOnly{}, Timeout: 10 * time.Second}
	c.checkWait = wait
	return &Service{tmdb: c}
}

// A TMDB token is checked with TMDB's authentication endpoint, once, with the
// token as the bearer and within the check's timeout: a token TMDB takes is
// valid, one it refuses (401) refused, saying what TMDB said; any other
// answer, none in time and no connection leave it unchecked. No message and
// no log line holds the token, and no key but TMDB's is checked.
func TestCheckSecretAsksTMDB(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	const v3Key = "0123456789abcdef0123456789abcdef"

	for _, tc := range []struct {
		name, token string
		fail        int // what the authentication endpoint answers instead, 0: as TMDB does
		status, msg string
	}{
		{"a token TMDB takes", "test-token", 0, graph.SecretValid, "TMDB took the token"},
		{"a v3 API key", v3Key, 0, graph.SecretRefused,
			"TMDB refused the token (HTTP 401: Invalid API key: You must be granted a valid key.): it is no TMDB API read access token, or one TMDB no longer takes"},
		{"TMDB down", "test-token", http.StatusServiceUnavailable, graph.SecretUnchecked,
			"could not check the token with TMDB (HTTP 503): it is saved all the same"},
		{"TMDB rate limiting", "test-token", http.StatusTooManyRequests, graph.SecretUnchecked,
			"could not check the token with TMDB (HTTP 429): it is saved all the same"},
		{"a 403, no definite no", v3Key, http.StatusForbidden, graph.SecretUnchecked,
			"could not check the token with TMDB (HTTP 403): it is saved all the same"},
	} {
		f := newFakeTMDB(t)
		if tc.fail != 0 {
			f.failing("/3/authentication", tc.fail)
		}
		got := checker(f, "", time.Second).CheckSecret(context.Background(), "tmdb.api_key", tc.token)
		if got.Status != tc.status || got.Message != tc.msg {
			t.Errorf("%s: %+v, want %s: %q", tc.name, got, tc.status, tc.msg)
		}
		if reqs := f.calls("/3/"); len(reqs) != 1 || reqs[0] != "/3/authentication?" {
			t.Errorf("%s: TMDB was asked %q, want GET /3/authentication once", tc.name, reqs)
		}
		if strings.Contains(got.Message, tc.token) {
			t.Errorf("%s: the message holds the token: %s", tc.name, got.Message)
		}
	}

	// No answer within the timeout: unchecked, and the check returns.
	f := newFakeTMDB(t)
	arrived, release := f.hold("/3/authentication")
	t.Cleanup(release)
	start := time.Now()
	got := checker(f, "", 150*time.Millisecond).CheckSecret(context.Background(), "tmdb.api_key", v3Key)
	<-arrived
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a check TMDB never answers took %v, want its timeout", took)
	}
	if got.Status != graph.SecretUnchecked || got.Message != "could not check the token with TMDB (no answer within 150ms): it is saved all the same" {
		t.Errorf("no answer in time: %+v", got)
	}

	// No connection at all.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	got = checker(nil, gone.URL+"/3", time.Second).CheckSecret(context.Background(), "tmdb.api_key", v3Key)
	if got.Status != graph.SecretUnchecked || !strings.HasPrefix(got.Message, "could not check the token with TMDB (TMDB could not be reached: ") ||
		!strings.HasSuffix(got.Message, "): it is saved all the same") || strings.Contains(got.Message, v3Key) {
		t.Errorf("no connection: %+v", got)
	}

	// TMDB's words repeating the token are written without it.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		http.Error(w, `{"status_code":7,"status_message":"Invalid API key `+token+`"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(echo.Close)
	got = checker(nil, echo.URL+"/3", time.Second).CheckSecret(context.Background(), "tmdb.api_key", v3Key)
	if got.Status != graph.SecretRefused || strings.Contains(got.Message, v3Key) || !strings.Contains(got.Message, "Invalid API key REDACTED") {
		t.Errorf("TMDB repeating the token: %+v", got)
	}

	// A token with a control character in it is refused without asking TMDB:
	// it could not be sent, and no TMDB token holds one.
	broken := newFakeTMDB(t)
	for _, token := range []string{"eyJhbGciOiJIUzI1NiJ9\n.eyJzdWIiOiJ4In0", "test-\ttoken", "test-token\x00"} {
		got := checker(broken, "", time.Second).CheckSecret(context.Background(), "tmdb.api_key", token)
		if got.Status != graph.SecretRefused || got.Message != "the token holds a control character, a line break say, which no TMDB token does" {
			t.Errorf("%q: %+v, want it refused", token, got)
		}
	}
	if reqs := broken.calls("/"); len(reqs) != 0 {
		t.Errorf("tokens no header can carry were sent to TMDB: %q", reqs)
	}

	// Another key is no TMDB token.
	other := newFakeTMDB(t)
	for _, key := range []string{"omdb.api_key", "fanart.api_key", "fanart.client_key", " tmdb.api_key"} {
		if got := checker(other, "", time.Second).CheckSecret(context.Background(), key, "test-token"); got != (graph.SecretCheck{}) {
			t.Errorf("%q was checked: %+v", key, got)
		}
	}
	if reqs := other.calls("/"); len(reqs) != 0 {
		t.Errorf("checking other keys asked TMDB %q", reqs)
	}
	if strings.Contains(logged.String(), v3Key) || strings.Contains(logged.String(), "test-token") {
		t.Errorf("a token was logged:\n%s", logged.String())
	}
}
