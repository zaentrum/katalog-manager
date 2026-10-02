// Package migrations carries the catalog's SQL migrations. They apply on top of
// the base schema in the order of their numbers, and each is idempotent, so a
// migration that has already run can run again unchanged.
//
// Not every deployment runs this directory, so the service embeds the
// migrations it cannot work without and applies each one at startup when its
// objects are missing (see store.EnsureDeletionLog and store.EnsurePeople).
package migrations

import _ "embed"

// DeletedItems is 029_deleted_items.sql: the deletion log that every item
// delete writes to inside its own transaction.
//
//go:embed 029_deleted_items.sql
var DeletedItems string

// People is 030_people.sql: a person's TMDB identity and details, their
// images, and the cursors of TMDB's change lists.
//
//go:embed 030_people.sql
var People string
