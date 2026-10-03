// Package authtest serves an OIDC issuer for tests: its discovery document
// and signing key, and the access tokens it signs, so a test meets the bearer
// tokens the service verifies in a deployment.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Issuer is an OIDC issuer at URL, serving its discovery document and its
// signing key (RS256).
type Issuer struct {
	URL string
	key *rsa.PrivateKey
}

const kid = "authtest"

// NewIssuer starts an issuer that serves until t ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the issuer's key: %v", err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss := &Issuer{URL: srv.URL, key: key}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer":                                iss.URL,
			"authorization_endpoint":                iss.URL + "/auth",
			"token_endpoint":                        iss.URL + "/token",
			"jwks_uri":                              iss.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
			"n": b64(key.PublicKey.N.Bytes()),
			"e": b64(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}}})
	})
	return iss
}

// Token signs claims as an access token of the issuer; iss, iat and exp (an
// hour from now) are added unless claims give them.
func (i *Issuer) Token(t testing.TB, claims map[string]any) string {
	t.Helper()
	c := map[string]any{"iss": i.URL, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		c[k] = v
	}
	return i.sign(t, c)
}

func (i *Issuer) sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign a token: %v", err)
	}
	return input + "." + b64(sig)
}

// Callers whose tokens the platform's realm issues, by what they are.

// Viewer is a person without the admin role, signed in through the web client.
func (i *Issuer) Viewer(t testing.TB) string {
	return i.Token(t, map[string]any{"sub": "viewer-1", "azp": "zaentrum-web", "aud": "chino",
		"preferred_username": "viewer", "realm_access": map[string]any{"roles": []string{"zaentrum-user", "offline_access"}}})
}

// Admin is a person with the admin role, signed in through the web client.
func (i *Issuer) Admin(t testing.TB) string {
	return i.Token(t, map[string]any{"sub": "admin-1", "azp": "zaentrum-web", "aud": "chino",
		"preferred_username": "admin", "realm_access": map[string]any{"roles": []string{"zaentrum-admin", "zaentrum-user"}}})
}

// CLIAdmin is the admin signed in with the zae CLI: its own public client and
// no audience.
func (i *Issuer) CLIAdmin(t testing.TB) string {
	return i.Token(t, map[string]any{"sub": "admin-1", "azp": "zae", "preferred_username": "admin",
		"realm_access": map[string]any{"roles": []string{"zaentrum-admin", "offline_access"}}})
}

// Service is the platform's service account: a client-credentials token of
// client, with the realm's default roles only.
func (i *Issuer) Service(t testing.TB, client string) string {
	return i.Token(t, map[string]any{"sub": "service-1", "azp": client, "aud": "chino",
		"preferred_username": "service-account-" + client,
		"realm_access":       map[string]any{"roles": []string{"offline_access", "uma_authorization"}}})
}

// Addon is an addon's service account: a client-credentials token with the
// addon role.
func (i *Issuer) Addon(t testing.TB) string {
	return i.Token(t, map[string]any{"sub": "addon-1", "azp": "example-addon", "aud": "chino",
		"preferred_username": "service-account-example-addon",
		"realm_access":       map[string]any{"roles": []string{"zaentrum-addon"}}})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
