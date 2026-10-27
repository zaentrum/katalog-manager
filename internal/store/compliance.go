package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// The tables db/migrations/051_library_compliance.sql creates: the rules the
// workers report they run with, and what the library's compliance check
// found of each title. A title's row goes with it (DeleteItems).
const (
	WorkerRulesTable = "com_nalet_katalog_workerrules"
	ComplianceTable  = "com_nalet_katalog_compliance"
)

// complianceRelations are the tables and indexes 051 creates.
var complianceRelations = []string{WorkerRulesTable, ComplianceTable, "idx_compliance_stale"}

// complianceColumns are the columns of the compliance a later build of 051
// added to its table.
var complianceColumns = []string{"blocked", "resolution", "videocodec", "videobitrate", "bitrateestimated", "bitratecap",
	"sizecap", "durationseconds", "packageaudio", "originalaudio", "recordkey", "records", "uncheckedrules"}

// ComplianceReady reports whether migration 051 is in place: its tables,
// their indexes, and every column of the compliance.
func (s *Store) ComplianceReady(ctx context.Context) (bool, error) {
	return complianceReady(ctx, s.pool)
}

// complianceReady is ComplianceReady read by q: the pool, or the transaction
// that holds the migrations' lock.
func complianceReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM unnest($1::text[]) AS r(name)
		WHERE to_regclass(r.name) IS NOT NULL) = cardinality($1::text[])
		AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass($2) AND attname = ANY($3::text[])
			AND attnum > 0 AND NOT attisdropped) = cardinality($3::text[])`,
		complianceRelations, ComplianceTable, complianceColumns).Scan(&ok)
	return ok, err
}

// EnsureCompliance applies db/migrations/051_library_compliance.sql when any
// of its objects is missing (a later build of it adds a column), under the
// migrations' lock (applyLocked). Without it the workers' rules are not kept,
// and the library's compliance is not checked.
func (s *Store) EnsureCompliance(ctx context.Context) error {
	return s.applyLocked(ctx, complianceReady, migrations.Compliance)
}
