// Package migrations carries the catalog's SQL migrations. They apply on top of
// the base schema in the order of their numbers, and each is idempotent, so a
// migration that has already run can run again unchanged.
//
// Not every deployment runs this directory, so the service embeds the
// migrations it cannot work without and applies each one at startup when its
// objects are missing (see store.EnsureDeletionLog).
package migrations

import _ "embed"

// DeletedItems is 029_deleted_items.sql: the deletion log that every item
// delete writes to inside its own transaction.
//
//go:embed 029_deleted_items.sql
var DeletedItems string
