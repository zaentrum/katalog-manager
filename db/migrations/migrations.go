// Package migrations carries the catalog's SQL migrations. They apply on top of
// the base schema in the order of their numbers, and each is idempotent, so a
// migration that has already run can run again unchanged.
//
// Not every deployment runs this directory, so the service embeds the
// migrations it cannot work without and applies each one at startup when its
// objects are missing (see store.EnsureDeletionLog, store.EnsurePeople,
// store.EnsureItemLockedFields, store.EnsureCreditDetails,
// store.EnsureStepRetries and store.EnsureScanJobRunner). It applies 035,
// which drops what is left of a retired integration, at every start when any
// of that is there (store.DropRetiredJobTables).
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

// ItemLockedFields is 031_item_locked_fields.sql: the fields of a title, such
// as its credits, that automation leaves alone.
//
//go:embed 031_item_locked_fields.sql
var ItemLockedFields string

// CreditDetails is 032_credit_details.sql: what a credit says besides its
// role, its job, character, order and episodes.
//
//go:embed 032_credit_details.sql
var CreditDetails string

// StepRetries is 033_step_retries.sql: what the service keeps to retry a
// processing step, its failures in a row, last error and next retry.
//
//go:embed 033_step_retries.sql
var StepRetries string

// ScanJobRunner is 034_scan_job_runner.sql: the process that runs a scan job,
// and its scanner's last word.
//
//go:embed 034_scan_job_runner.sql
var ScanJobRunner string

// RetiredJobTables is 035_retired_job_tables.sql: it drops the job tables of
// an integration the core no longer carries, each only while it is empty.
//
//go:embed 035_retired_job_tables.sql
var RetiredJobTables string
