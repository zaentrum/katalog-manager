package graph

import (
	"context"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
)

// testConfig tells callers apart as the platform's realm does.
var testConfig = config.Config{AdminRole: "zaentrum-admin", AddonRole: "zaentrum-addon",
	RolesClaim: auth.DefaultRolesClaim, ServiceClients: []string{"zaentrum-manager"}}

// caller is a bearer token's caller: the client it was issued to and the
// realm roles it carries.
func caller(subject, client string, roles ...string) *auth.Principal {
	list := make([]any, len(roles))
	for i, r := range roles {
		list[i] = r
	}
	return &auth.Principal{Subject: subject, Client: client,
		Claims: map[string]any{"realm_access": map[string]any{"roles": list}}}
}

var (
	viewer  = caller("viewer-1", "zaentrum-web", "zaentrum-user", "offline_access")
	admin   = caller("admin-1", "zaentrum-web", "zaentrum-admin", "zaentrum-user")
	service = caller("service-1", "zaentrum-manager", "offline_access", "uma_authorization")
	addon   = caller("addon-1", "example-addon", "zaentrum-addon")
)

// as is a context whose caller is p.
func as(p *auth.Principal) context.Context { return auth.WithPrincipal(context.Background(), p) }
