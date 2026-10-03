package auth

import (
	"net/http"
	"strings"
)

// Middleware wires JWT + stream-token authentication into the request chain,
// mirroring the CAP SecurityConfig (SPEC §6):
//   - /healthz, /actuator/health/** , /katalog/** are public.
//   - a read (GET, HEAD) of /api/artwork/** accepts a valid bearer JWT OR a
//     ?stream= token.
//   - everything else requires a valid bearer JWT.
//
// It authenticates; what a caller may do is the Policy's (access.go), applied
// by the routes and the GraphQL fields. When AuthDisabled is set the
// JWTVerifier is in disabled mode and authorizes every request.
type Middleware struct {
	jwt    *JWTVerifier
	stream *StreamVerifier
}

func NewMiddleware(jwt *JWTVerifier, stream *StreamVerifier) *Middleware {
	return &Middleware{jwt: jwt, stream: stream}
}

func isPublic(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/actuator/health"):
		return true
	case strings.HasPrefix(path, "/katalog/"):
		return true
	}
	return false
}

// Handler returns the authentication middleware.
func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if isPublic(path) {
			next.ServeHTTP(w, r)
			return
		}

		// Stream token: only on artwork reads, as an alternative to JWT —
		// an <img> tag cannot send an Authorization header. Both mount points
		// count: /api/artwork is the in-cluster one, /api/manage/artwork is
		// the one published by a Route and therefore the one browsers use.
		// Never on a write: the analyzer's PUT of a keyframe takes a bearer
		// token, and a viewer's stream token must not stand in for it.
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		if read && (strings.HasPrefix(path, "/api/artwork/") ||
			strings.HasPrefix(path, "/api/manage/artwork/")) && m.stream.Configured() {
			if tok := r.URL.Query().Get("stream"); tok != "" {
				if sub, ok := m.stream.Verify(tok); ok {
					ctx := WithPrincipal(r.Context(), &Principal{Subject: sub, Stream: true})
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
		}

		if p, ok := m.jwt.verifyBearer(r.Context(), r); ok {
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}

		w.Header().Set("WWW-Authenticate", `Bearer`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}
