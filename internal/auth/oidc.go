package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	errStreamKeyNotBase64 = errors.New("STREAM_SIGNING_KEY must be base64")
	errStreamKeyTooShort  = errors.New("STREAM_SIGNING_KEY must be at least 16 bytes (base64-decoded)")
)

// Principal identifies an authenticated caller.
type Principal struct {
	Subject string
	Stream  bool // true when authenticated via a stream token (ROLE_STREAM)
}

type ctxKey int

const principalKey ctxKey = 0

// WithPrincipal stores p on the context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the principal stored on the context, if any.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}

// JWTVerifier validates bearer access tokens against an OIDC issuer's JWKS.
// MVP semantics mirror the CAP service: issuer + signature + expiry only
// (audience optional). A nil verifier (no issuer configured, or AuthDisabled)
// authorizes everything.
type JWTVerifier struct {
	verifier *oidc.IDTokenVerifier
	disabled bool
}

// NewJWTVerifier builds a verifier. When disabled is true or issuer is blank,
// every request is authorized (principal subject "anonymous").
func NewJWTVerifier(ctx context.Context, issuer, audience string, audienceRequired, disabled bool) (*JWTVerifier, error) {
	if disabled || issuer == "" {
		return &JWTVerifier{disabled: true}, nil
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	cfg := &oidc.Config{SkipClientIDCheck: !audienceRequired}
	if audienceRequired {
		cfg.ClientID = audience
	}
	return &JWTVerifier{verifier: provider.Verifier(cfg)}, nil
}

// verifyBearer extracts and validates the Authorization bearer token.
func (j *JWTVerifier) verifyBearer(ctx context.Context, r *http.Request) (*Principal, bool) {
	if j.disabled {
		return &Principal{Subject: "anonymous"}, true
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(h[len("Bearer "):])
	if raw == "" {
		return nil, false
	}
	tok, err := j.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, false
	}
	return &Principal{Subject: tok.Subject}, true
}
