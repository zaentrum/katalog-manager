package graph

import (
	"context"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/auth"
)

// fieldAccess is who may call each root field of the schema (auth.Access).
//
// The GraphQL API is the catalog console's: katalog-manager is the catalog's
// writer, and viewers read the catalog through katalog-api and its images
// through chino-api's artwork proxy, which reaches this service only on the
// REST artwork routes. So every field is an administrator's: what the catalog
// holds down to its paths on disk and the pipeline's errors, the scan jobs,
// the settings, the deletion log, and every change. The one
// other caller is the scan Job a deployment runs after it starts, which calls
// triggerScan with the platform's service account.
//
// A root field missing here is refused to everyone (allow), and
// TestEveryRootFieldHasARule keeps the table and the schema in step.
var fieldAccess = map[string]auth.Access{
	"Query.item":                  auth.Admin,
	"Query.items":                 auth.Admin,
	"Query.movies":                auth.Admin,
	"Query.series":                auth.Admin,
	"Query.episodes":              auth.Admin,
	"Query.albums":                auth.Admin,
	"Query.searchItems":           auth.Admin,
	"Query.scanJob":               auth.Admin,
	"Query.scanJobs":              auth.Admin,
	"Query.activity":              auth.Admin,
	"Query.settings":              auth.Admin,
	"Query.genres":                auth.Admin,
	"Query.people":                auth.Admin,
	"Query.person":                auth.Admin,
	"Query.enrichStatus":          auth.Admin,
	"Query.enrichmentStatusCodes": auth.Admin,
	"Query.deletedItems":          auth.Admin,
	"Query.referenceSync":         auth.Admin,
	"Query.processingOverview":    auth.Admin,

	"Mutation.triggerScan":              auth.Worker, // the scan Job's
	"Mutation.enrichOne":                auth.Admin,
	"Mutation.identify":                 auth.Admin,
	"Mutation.enrichPending":            auth.Admin,
	"Mutation.refreshPeople":            auth.Admin,
	"Mutation.backfillEpisodeBackdrops": auth.Admin,
	"Mutation.retryNotFound":            auth.Admin,
	"Mutation.retryStep":                auth.Admin,
	"Mutation.retryFailed":              auth.Admin,
	"Mutation.packageItem":              auth.Admin,
	"Mutation.validateItem":             auth.Admin,
	"Mutation.createItem":               auth.Admin,
	"Mutation.updateItem":               auth.Admin,
	"Mutation.deleteItem":               auth.Admin,
	"Mutation.setItemGenres":            auth.Admin,
	"Mutation.setItemTags":              auth.Admin,
	"Mutation.createSetting":            auth.Admin,
	"Mutation.updateSetting":            auth.Admin,
	"Mutation.deleteSetting":            auth.Admin,
	"Mutation.setSecretSetting":         auth.Admin,
	"Mutation.clearSecretSetting":       auth.Admin,
}

// allow returns nil when the caller in ctx may call field ("Query.items",
// "Mutation.deleteItem"), and its refusal otherwise: a FORBIDDEN error that
// names who may. Every root resolver asks it first, before it reads or
// changes anything.
func (r *Resolver) allow(ctx context.Context, field string) error {
	name := field[strings.IndexByte(field, '.')+1:]
	a, ok := fieldAccess[field]
	if !ok {
		return &auth.Forbidden{Operation: name, Requires: "an access rule, and it has none", Role: r.access.AdminRole}
	}
	return r.access.Check(ctx, a, name)
}
