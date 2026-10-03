package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth/authtest"
)

var testPolicy = Policy{AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon", ServiceClients: []string{"zaentrum-manager"}}

func roles(rs ...string) map[string]any {
	list := make([]any, len(rs))
	for i, r := range rs {
		list[i] = r
	}
	return map[string]any{"realm_access": map[string]any{"roles": list}}
}

// Who may do what: a viewer reads, an admin does everything, the service
// account does the workers' work, an addon only ingests; nobody without a
// principal, and a stream token only reads.
func TestPolicyAllows(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		p                             *Principal
		viewer, admin, worker, ingest bool
	}{
		{"no caller", nil, false, false, false, false},
		{"a viewer", &Principal{Subject: "v", Client: "zaentrum-web", Claims: roles("zaentrum-user", "offline_access")}, true, false, false, false},
		{"a viewer with no roles claim", &Principal{Subject: "v", Client: "zaentrum-web"}, true, false, false, false},
		{"an admin", &Principal{Subject: "a", Client: "zaentrum-web", Claims: roles("zaentrum-user", "zaentrum-admin")}, true, true, true, true},
		{"an admin through the CLI", &Principal{Subject: "a", Client: "zae", Claims: roles("zaentrum-admin")}, true, true, true, true},
		{"the service account", &Principal{Subject: "s", Client: "zaentrum-manager", Claims: roles("offline_access")}, true, false, true, true},
		{"another confidential client", &Principal{Subject: "s", Client: "zaentrum-other", Claims: roles("offline_access")}, true, false, false, false},
		{"an addon", &Principal{Subject: "x", Client: "example-addon", Claims: roles("zaentrum-addon")}, true, false, false, true},
		{"a stream token", &Principal{Subject: "v", Stream: true}, true, false, false, false},
		{"a stream token naming the service's client", &Principal{Subject: "v", Stream: true, Client: "zaentrum-manager"}, true, false, false, false},
		{"auth off", &Principal{Subject: "anonymous", Unrestricted: true}, true, true, true, true},
		{"the admin role as a client role", &Principal{Subject: "c", Client: "zaentrum-web",
			Claims: map[string]any{"resource_access": map[string]any{"zaentrum-web": map[string]any{"roles": []any{"zaentrum-admin"}}}}}, true, false, false, false},
		{"the admin role in another claim", &Principal{Subject: "c", Client: "zaentrum-web", Claims: map[string]any{"roles": []any{"zaentrum-admin"}}}, true, false, false, false},
	} {
		for a, want := range map[Access]bool{Viewer: tc.viewer, Admin: tc.admin, Worker: tc.worker, Ingest: tc.ingest} {
			if got := testPolicy.Allows(tc.p, a); got != want {
				t.Errorf("%s, %s: allowed %v, want %v", tc.name, testPolicy.Requirement(a), got, want)
			}
		}
	}
	if (Policy{}).Allows(&Principal{Subject: "a", Claims: roles("")}, Admin) {
		t.Error("a policy without an admin role admits a token with an empty role as an admin")
	}
	if testPolicy.Allows(&Principal{Subject: "a", Claims: roles("zaentrum-admin")}, Access(99)) {
		t.Error("an access level the policy does not know admits an admin")
	}
}

// The roles are read where the policy says, a dot-separated path into the
// claims; a single string is one role; anything else is none.
func TestRolesAtTheClaimPath(t *testing.T) {
	claims := map[string]any{
		"realm_access":    map[string]any{"roles": []any{"zaentrum-admin", 7, "zaentrum-user"}},
		"roles":           []any{"top"},
		"groups":          "one-group",
		"resource_access": map[string]any{"katalog": map[string]any{"roles": []any{"client-role"}}},
		"flat":            map[string]any{"roles": map[string]any{"not": "a list"}},
	}
	p := &Principal{Claims: claims}
	for path, want := range map[string][]string{
		"":                              {"zaentrum-admin", "zaentrum-user"},
		"realm_access.roles":            {"zaentrum-admin", "zaentrum-user"},
		"roles":                         {"top"},
		"groups":                        {"one-group"},
		"resource_access.katalog.roles": {"client-role"},
		"flat.roles":                    nil,
		"realm_access":                  nil,
		"realm_access.roles.more":       nil,
		"nowhere":                       nil,
	} {
		if got := (Policy{RolesClaim: path}).Roles(p); !slices.Equal(got, want) {
			t.Errorf("roles at %q: %q, want %q", path, got, want)
		}
	}
	if got := testPolicy.Roles(nil); got != nil {
		t.Errorf("the roles of no caller: %q", got)
	}
	if !(Policy{AdminRole: "top", RolesClaim: "roles"}).IsAdmin(p) {
		t.Error("an admin role read from another claim did not count")
	}
}

// A refusal names who may: the admin role, and the service account or the
// addon role where they may too.
func TestRequirementNamesWhoMay(t *testing.T) {
	for a, want := range map[Access]string{
		Viewer: "a signed-in caller",
		Admin:  "the zaentrum-admin role",
		Worker: "the zaentrum-admin role or the platform's service account",
		Ingest: "the zaentrum-admin or the zaentrum-addon role, or the platform's service account",
	} {
		if got := testPolicy.Requirement(a); got != want {
			t.Errorf("requirement %d: %q, want %q", a, got, want)
		}
	}
}

// Check refuses with a Forbidden that is a GraphQL error with the code
// FORBIDDEN, and lets an allowed caller through.
func TestCheck(t *testing.T) {
	viewer := WithPrincipal(context.Background(), &Principal{Subject: "v", Claims: roles("zaentrum-user")})
	err := testPolicy.Check(viewer, Admin, "deleteItem")
	var f *Forbidden
	if !errors.As(err, &f) {
		t.Fatalf("a viewer's deleteItem: %v, want a Forbidden", err)
	}
	if got, want := err.Error(), "forbidden: deleteItem requires the zaentrum-admin role"; got != want {
		t.Errorf("message %q, want %q", got, want)
	}
	if ext := f.Extensions(); ext["code"] != "FORBIDDEN" || ext["role"] != "zaentrum-admin" {
		t.Errorf("extensions %v, want the code FORBIDDEN and the role", ext)
	}
	if err := testPolicy.Check(context.Background(), Viewer, "item"); err == nil {
		t.Error("a context without a caller passed the check")
	}
	admin := WithPrincipal(context.Background(), &Principal{Subject: "a", Claims: roles("zaentrum-admin")})
	if err := testPolicy.Check(admin, Admin, "deleteItem"); err != nil {
		t.Errorf("an admin's deleteItem: %v", err)
	}
}

// Require answers 403 with a JSON error naming who may, and passes an allowed
// caller on.
func TestRequire(t *testing.T) {
	reached := false
	h := testPolicy.Require(Worker)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, tc := range []struct {
		name string
		p    *Principal
		code int
	}{
		{"a viewer", &Principal{Subject: "v", Claims: roles("zaentrum-user")}, http.StatusForbidden},
		{"no caller", nil, http.StatusForbidden},
		{"the service account", &Principal{Subject: "s", Client: "zaentrum-manager"}, http.StatusNoContent},
		{"an admin", &Principal{Subject: "a", Claims: roles("zaentrum-admin")}, http.StatusNoContent},
	} {
		reached = false
		req := httptest.NewRequest(http.MethodPut, "/api/analyze/items/x/steps/chapter", nil)
		if tc.p != nil {
			req = req.WithContext(WithPrincipal(req.Context(), tc.p))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.code || reached != (tc.code == http.StatusNoContent) {
			t.Errorf("%s: %d (handler reached: %v), want %d", tc.name, w.Code, reached, tc.code)
		}
		if tc.code == http.StatusForbidden {
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil ||
				body["error"] != "forbidden: requires the zaentrum-admin role or the platform's service account" {
				t.Errorf("%s: body %s, want a JSON error naming who may", tc.name, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("%s: Content-Type %q", tc.name, ct)
			}
		}
	}
}

func verifier(t *testing.T, issuer string) *JWTVerifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := NewJWTVerifier(ctx, issuer, "chino", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if v.tokenVerifier() == nil {
		t.Fatalf("discovery of %s did not complete", issuer)
	}
	return v
}

func bearer(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/manage/query", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// A bearer token's caller comes with the client it was issued to and its
// claims, so the policy can tell a viewer, an admin (also through the CLI,
// whose token names no audience) and the service account apart.
func TestBearerTokenCarriesItsClientAndClaims(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := verifier(t, iss.URL)
	for _, tc := range []struct {
		name, token, client string
		admin, service      bool
	}{
		{"a viewer", iss.Viewer(t), "zaentrum-web", false, false},
		{"an admin", iss.Admin(t), "zaentrum-web", true, false},
		{"an admin through the CLI", iss.CLIAdmin(t), "zae", true, false},
		{"the service account", iss.Service(t, "zaentrum-manager"), "zaentrum-manager", false, true},
		{"a client named by client_id", iss.Token(t, map[string]any{"sub": "s", "client_id": "zaentrum-manager"}), "zaentrum-manager", false, true},
	} {
		p, ok := v.verifyBearer(context.Background(), bearer(tc.token))
		if !ok {
			t.Errorf("%s: refused", tc.name)
			continue
		}
		if p.Client != tc.client || testPolicy.IsAdmin(p) != tc.admin || testPolicy.IsService(p) != tc.service {
			t.Errorf("%s: client %q, admin %v, service %v; want %q, %v, %v",
				tc.name, p.Client, testPolicy.IsAdmin(p), testPolicy.IsService(p), tc.client, tc.admin, tc.service)
		}
	}
	other := authtest.NewIssuer(t)
	admin := iss.Admin(t)
	for name, token := range map[string]string{
		"another issuer's token": other.Admin(t),
		"an expired token": iss.Token(t, map[string]any{"sub": "admin-1", "exp": time.Now().Add(-time.Minute).Unix(),
			"realm_access": map[string]any{"roles": []string{"zaentrum-admin"}}}),
		"a forged signature": admin[:len(admin)-4] + "AAAA",
	} {
		if _, ok := v.verifyBearer(context.Background(), bearer(token)); ok {
			t.Errorf("%s was accepted", name)
		}
	}
}

// streamToken mints a stream token as chino-api does, valid for an hour.
func streamToken(key []byte, subject string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(subject + "|" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// A stream token stands in for a bearer token on an artwork read only: not on
// a write of the same path, nor anywhere else.
func TestStreamTokenOnlyOnArtworkReads(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	stream, err := NewStreamVerifier(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	iss := authtest.NewIssuer(t)
	var seen *Principal
	h := NewMiddleware(verifier(t, iss.URL), stream).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	tok := "?stream=" + streamToken(key, "viewer-1")
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/artwork/m1/poster", http.StatusOK},
		{http.MethodHead, "/api/manage/artwork/m1/poster", http.StatusOK},
		{http.MethodGet, "/api/manage/artwork/person/p1/profile", http.StatusOK},
		{http.MethodPut, "/api/artwork/m1/poster", http.StatusUnauthorized},
		{http.MethodPost, "/api/manage/artwork/m1/poster", http.StatusUnauthorized},
		{http.MethodGet, "/api/play/m1", http.StatusUnauthorized},
		{http.MethodPost, "/api/manage/query", http.StatusUnauthorized},
	} {
		seen = nil
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path+tok, nil))
		if w.Code != tc.code {
			t.Errorf("%s %s with a stream token: %d, want %d", tc.method, tc.path, w.Code, tc.code)
		}
		if tc.code == http.StatusOK && (seen == nil || !seen.Stream || seen.Subject != "viewer-1") {
			t.Errorf("%s %s: the caller %+v, want the stream token's viewer", tc.method, tc.path, seen)
		}
	}
}
