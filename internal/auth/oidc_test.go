package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewJWTVerifier_UnreachableIssuerIsNonFatal pins the crash-loop fix: when
// the OIDC issuer is not reachable at startup (e.g. bundled Keycloak still
// booting during a simultaneous rollout), the constructor must NOT return an
// error (previously it did → main log.Fatal → crash loop). It must return a
// live verifier that fails closed on protected routes until discovery succeeds.
func TestNewJWTVerifier_UnreachableIssuerIsNonFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // stops the background retry goroutine

	// 127.0.0.1:1 refuses instantly, so the bounded synchronous attempt fails
	// fast without waiting out the 5s timeout.
	v, err := NewJWTVerifier(ctx, "http://127.0.0.1:1/realms/zaentrum", "chino", false, false)
	if err != nil {
		t.Fatalf("unreachable issuer must be non-fatal, got error: %v", err)
	}
	if v == nil {
		t.Fatal("expected a verifier, got nil")
	}
	if v.disabled {
		t.Fatal("verifier must not be disabled when an issuer is configured")
	}
	if v.tokenVerifier() != nil {
		t.Fatal("verifier must not be ready before discovery completes")
	}

	// Fails closed: discovery hasn't completed, so a bearer token is rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/manage/query", nil)
	req.Header.Set("Authorization", "Bearer sometoken")
	if _, ok := v.verifyBearer(ctx, req); ok {
		t.Fatal("expected fail-closed (unauthorized) before discovery completes")
	}
}

// TestNewJWTVerifier_DisabledAuthorizesAll confirms the disabled/blank-issuer
// path still authorizes every caller as "anonymous".
func TestNewJWTVerifier_DisabledAuthorizesAll(t *testing.T) {
	for _, tc := range []struct {
		name     string
		issuer   string
		disabled bool
	}{
		{"blank issuer", "", false},
		{"auth disabled", "http://issuer.example/realms/zaentrum", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewJWTVerifier(context.Background(), tc.issuer, "chino", false, tc.disabled)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/manage/query", nil)
			p, ok := v.verifyBearer(context.Background(), req)
			if !ok || p == nil || p.Subject != "anonymous" {
				t.Fatalf("disabled verifier should authorize as anonymous, got ok=%v p=%+v", ok, p)
			}
		})
	}
}
