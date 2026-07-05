package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

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
// (audience optional). A disabled verifier (no issuer configured, or
// AuthDisabled) authorizes everything.
//
// OIDC discovery is resolved LAZILY: the issuer's discovery document is fetched
// once at construction (bounded), and on failure the constructor does NOT error
// — it returns a verifier that fails closed (401 on protected routes) and keeps
// retrying discovery in the background until the issuer is reachable. This is
// deliberate: during a simultaneous rollout the identity provider (bundled
// Keycloak) may still be starting, and an eager, fatal init would crash-loop the
// whole service until Keycloak was up. Failing closed + retrying keeps the
// service booting and self-heals the moment the issuer answers.
type JWTVerifier struct {
	disabled bool

	issuer string
	cfg    *oidc.Config

	mu       sync.RWMutex
	verifier *oidc.IDTokenVerifier // nil until discovery succeeds
}

// NewJWTVerifier builds a verifier. When disabled is true or issuer is blank,
// every request is authorized (principal subject "anonymous"). Otherwise it
// attempts OIDC discovery once; discovery failure is non-fatal (see the type
// doc) — the caller keeps running and the verifier self-heals in the background.
func NewJWTVerifier(ctx context.Context, issuer, audience string, audienceRequired, disabled bool) (*JWTVerifier, error) {
	if disabled || issuer == "" {
		return &JWTVerifier{disabled: true}, nil
	}
	cfg := &oidc.Config{SkipClientIDCheck: !audienceRequired}
	if audienceRequired {
		cfg.ClientID = audience
	}
	j := &JWTVerifier{issuer: issuer, cfg: cfg}

	// Try once synchronously (bounded) so the common case — issuer already up —
	// is ready the instant the server starts serving. On failure, don't block
	// or crash: retry in the background with capped backoff.
	if !j.discover(ctx) {
		log.Printf("auth: OIDC issuer %s not reachable yet — serving with auth fail-closed, retrying discovery in the background", issuer)
		go j.retryDiscovery(ctx)
	}
	return j, nil
}

// discover attempts a single bounded OIDC discovery. On success it installs the
// token verifier and returns true. Safe to call concurrently.
func (j *JWTVerifier) discover(ctx context.Context) bool {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(dctx, j.issuer)
	if err != nil {
		return false
	}
	v := provider.Verifier(j.cfg)
	j.mu.Lock()
	j.verifier = v
	j.mu.Unlock()
	return true
}

// retryDiscovery re-attempts discovery with capped exponential backoff until it
// succeeds or ctx is cancelled (server shutdown). Because the verifier fails
// closed while discovery is pending, a persisting failure (e.g. a misconfigured
// issuer that will never resolve) is a silent auth outage — every bearer request
// is rejected. So failures are logged (throttled) to keep that state visible,
// not just the single line emitted at startup.
func (j *JWTVerifier) retryDiscovery(ctx context.Context) {
	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second
	attempts := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		attempts++
		if j.discover(ctx) {
			log.Printf("auth: OIDC discovery for %s succeeded after %d retry attempt(s) — bearer authentication active", j.issuer, attempts)
			return
		}
		// Surface a persisting outage: the first few attempts, then once per
		// capped-backoff interval so a permanent misconfig stays visible in the
		// logs without flooding them.
		if attempts <= 3 || backoff >= maxBackoff {
			log.Printf("auth: OIDC discovery for %s still failing (attempt %d) — bearer auth remains fail-closed, retrying", j.issuer, attempts)
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// tokenVerifier returns the installed verifier, or nil if discovery has not yet
// completed.
func (j *JWTVerifier) tokenVerifier() *oidc.IDTokenVerifier {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return j.verifier
}

// verifyBearer extracts and validates the Authorization bearer token. Before
// discovery has completed it fails closed (returns not-authenticated) rather
// than authorizing.
func (j *JWTVerifier) verifyBearer(ctx context.Context, r *http.Request) (*Principal, bool) {
	if j.disabled {
		return &Principal{Subject: "anonymous"}, true
	}
	verifier := j.tokenVerifier()
	if verifier == nil {
		// OIDC discovery not ready yet — fail closed.
		return nil, false
	}
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, false
	}
	raw := strings.TrimSpace(h[len("Bearer "):])
	if raw == "" {
		return nil, false
	}
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return nil, false
	}
	return &Principal{Subject: tok.Subject}, true
}
