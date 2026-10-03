package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// Access is who may call an operation of the service.
type Access int

const (
	// Viewer is any signed-in caller: a bearer token, or on an artwork read a
	// stream token.
	Viewer Access = iota
	// Admin is a caller whose token carries the admin role.
	Admin
	// Worker is an admin, or the platform's service account: the confidential
	// client the pipeline workers (analyzer, transcoder, packager) and the
	// scan Job mint client-credentials tokens with.
	Worker
	// Ingest is a worker, or an addon's service account (a token with the
	// addon role): POST /api/ingest, the seam an addon hands files through.
	Ingest
)

// DefaultRolesClaim is where Keycloak puts a token's realm roles.
const DefaultRolesClaim = "realm_access.roles"

// Policy tells the callers of the service apart. The zero Policy admits
// nobody but a caller of a service whose auth is off (Principal.Unrestricted).
type Policy struct {
	// AdminRole is the role an administrator's token carries.
	AdminRole string
	// AddonRole is the role an addon's service account carries.
	AddonRole string
	// RolesClaim is where a token carries its roles: a dot-separated path into
	// its claims, DefaultRolesClaim when empty. It leads to a list of strings
	// (or to one string).
	RolesClaim string
	// ServiceClients are the OIDC clients (a token's azp) of the platform's
	// service account. Only a confidential client that mints tokens for itself
	// alone (client credentials, no sign-in) belongs here: any token issued to
	// one counts as the service.
	ServiceClients []string
}

// Roles returns the roles p's token carries.
func (pol Policy) Roles(p *Principal) []string {
	if p == nil {
		return nil
	}
	path := pol.RolesClaim
	if path == "" {
		path = DefaultRolesClaim
	}
	return claimStrings(p.Claims, path)
}

func (pol Policy) hasRole(p *Principal, role string) bool {
	return role != "" && slices.Contains(pol.Roles(p), role)
}

// IsAdmin reports whether p is an administrator: its token carries the admin
// role, or the service runs with its auth off.
func (pol Policy) IsAdmin(p *Principal) bool {
	return p != nil && (p.Unrestricted || pol.hasRole(p, pol.AdminRole))
}

// IsService reports whether p is the platform's service account: a bearer
// token issued to one of ServiceClients.
func (pol Policy) IsService(p *Principal) bool {
	return p != nil && !p.Stream && p.Client != "" && slices.Contains(pol.ServiceClients, p.Client)
}

// IsAddon reports whether p is an addon's service account: its token carries
// the addon role.
func (pol Policy) IsAddon(p *Principal) bool {
	return p != nil && pol.hasRole(p, pol.AddonRole)
}

// Allows reports whether p may call an operation that takes a.
func (pol Policy) Allows(p *Principal, a Access) bool {
	if p == nil {
		return false
	}
	switch a {
	case Viewer:
		return true
	case Admin:
		return pol.IsAdmin(p)
	case Worker:
		return pol.IsAdmin(p) || pol.IsService(p)
	case Ingest:
		return pol.IsAdmin(p) || pol.IsService(p) || pol.IsAddon(p)
	}
	return false
}

// Requirement says who may call an operation that takes a, for a refusal to
// name.
func (pol Policy) Requirement(a Access) string {
	switch a {
	case Viewer:
		return "a signed-in caller"
	case Admin:
		return "the " + pol.AdminRole + " role"
	case Worker:
		return "the " + pol.AdminRole + " role or the platform's service account"
	case Ingest:
		return "the " + pol.AdminRole + " or the " + pol.AddonRole + " role, or the platform's service account"
	}
	return "a rule that admits the caller"
}

// Check returns nil when the caller in ctx may call operation, which takes a,
// and its refusal otherwise.
func (pol Policy) Check(ctx context.Context, a Access, operation string) error {
	p, _ := PrincipalFrom(ctx)
	if pol.Allows(p, a) {
		return nil
	}
	return &Forbidden{Operation: operation, Requires: pol.Requirement(a), Role: pol.AdminRole}
}

// Require refuses a request whose caller may not call an operation that takes
// a: 403 with {"error": "..."} naming who may. It runs after the
// authentication middleware, which puts the caller on the context.
func (pol Policy) Require(a Access) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := PrincipalFrom(r.Context())
			if !pol.Allows(p, a) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				refusal := &Forbidden{Requires: pol.Requirement(a), Role: pol.AdminRole}
				_ = json.NewEncoder(w).Encode(map[string]string{"error": refusal.Error()})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Forbidden refuses an operation to a caller who may not call it. As the
// error of a GraphQL field it carries the code FORBIDDEN and the admin role
// in its extensions.
type Forbidden struct {
	// Operation is what was refused (a GraphQL field); empty for a route.
	Operation string
	// Requires says who may (Policy.Requirement).
	Requires string
	// Role is the admin role.
	Role string
}

func (e *Forbidden) Error() string {
	if e.Operation == "" {
		return "forbidden: requires " + e.Requires
	}
	return "forbidden: " + e.Operation + " requires " + e.Requires
}

// Extensions are the GraphQL error's extensions.
func (e *Forbidden) Extensions() map[string]any {
	return map[string]any{"code": "FORBIDDEN", "role": e.Role}
}

// claimStrings reads the strings at path, a dot-separated path into claims
// ("realm_access.roles"): those of a list, or a single string. Nothing when
// the path leads nowhere, or to anything else.
func claimStrings(claims map[string]any, path string) []string {
	var v any = claims
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}
