package store

import (
	"context"

	"github.com/zaentrum/katalog-manager/db/migrations"
)

// SourceCaptionsTable is the table db/migrations/058_source_captions.sql
// creates: what a read of an original's frames, in windows across its
// picture, found of its closed captions and of its fields.
const SourceCaptionsTable = "com_nalet_katalog_sourcecaptions"

// CaptionHandoutsTable is the table 058 keeps each hand-out of an original
// to be read in, for the captions backfill's hourly rate.
const CaptionHandoutsTable = "com_nalet_katalog_captionhandouts"

// SourceCaptionsReady reports whether migration 058 is in place.
func (s *Store) SourceCaptionsReady(ctx context.Context) (bool, error) {
	return sourceCaptionsReady(ctx, s.pool)
}

// sourceCaptionsColumns are the columns of 058's final shape, and
// sourceCaptionsGone those of an earlier one it drops: the migration is in
// place when the table has every one of the first and none of the second.
var (
	sourceCaptionsColumns = []string{"source_id", "item_id", "state", "captionwindows", "captions", "ffprobeversion",
		"framesdecoded", "duration", "captionstreams", "offlist", "sourcesize", "sourceqh1", "claimsize", "claimqh1", "claimdurationms",
		"claimspanstart", "claimspanend",
		"claimrecordedat", "sourcerecordedat", "finding",
		"reason", "readat", "failures", "nextattemptat", "claimedat", "claimedby",
		"claimtoken", "lapsedtoken", "lastclaimedat", "attempts", "createdat", "modifiedat"}
	sourceCaptionsGone = []string{"probe", "packets", "packageversionid", "packagestate", "packagecaptionwindows",
		"packagecaptions", "packageffprobeversion", "packageframesdecoded", "packagereason", "packagereadat"}
)

// sourceCaptionsReady is SourceCaptionsReady read by q: the pool, or the
// transaction that holds the migrations' lock. It looks at the columns, so
// that a catalog that applied an earlier shape of 058 has it applied again.
func sourceCaptionsReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass('idx_sourcecaptions_item') IS NOT NULL
			AND to_regclass('idx_captionhandouts_at') IS NOT NULL
			AND to_regclass('idx_sourcecaptions_claimed') IS NOT NULL
			AND (SELECT count(*) FROM information_schema.columns
			     WHERE table_schema = current_schema() AND table_name = $1 AND column_name = ANY($2)) = cardinality($2::text[])
			AND NOT EXISTS (SELECT 1 FROM information_schema.columns
			     WHERE table_schema = current_schema() AND table_name = $1 AND column_name = ANY($3))`,
		SourceCaptionsTable, sourceCaptionsColumns, sourceCaptionsGone).Scan(&ok)
	return ok, err
}

// CaptionsWaitingIndex is the index db/migrations/059_captions_retire_waiting.sql
// creates with its column, which says it is in place.
const CaptionsWaitingIndex = "idx_sourcecaptions_retirewaiting"

// CaptionsWaitingReady reports whether migration 059 is in place.
func (s *Store) CaptionsWaitingReady(ctx context.Context) (bool, error) {
	return captionsWaitingReady(ctx, s.pool)
}

func captionsWaitingReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, CaptionsWaitingIndex).Scan(&ok)
	return ok, err
}

// EnsureCaptionsWaiting applies db/migrations/059_captions_retire_waiting.sql,
// after 058 (EnsureSourceCaptions), when it is missing, under the
// migrations' lock. Without it the captions backfill hands nothing out.
func (s *Store) EnsureCaptionsWaiting(ctx context.Context) error {
	if err := s.EnsureSourceCaptions(ctx); err != nil {
		return err
	}
	return s.applyLocked(ctx, captionsWaitingReady, migrations.CaptionsWaiting)
}

// CaptionsSpanIndex is the index db/migrations/060_captions_read_span.sql
// creates with its columns, which says it is in place.
const CaptionsSpanIndex = "idx_sourcecaptions_unscanned"

// CaptionsSpanReady reports whether migration 060 is in place.
func (s *Store) CaptionsSpanReady(ctx context.Context) (bool, error) {
	return captionsSpanReady(ctx, s.pool)
}

func captionsSpanReady(ctx context.Context, q querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, CaptionsSpanIndex).Scan(&ok)
	return ok, err
}

// EnsureCaptionsSpan applies db/migrations/060_captions_read_span.sql, after
// 058 (EnsureSourceCaptions), when it is missing, under the migrations'
// lock. Without it the captions backfill hands nothing out.
func (s *Store) EnsureCaptionsSpan(ctx context.Context) error {
	if err := s.EnsureSourceCaptions(ctx); err != nil {
		return err
	}
	return s.applyLocked(ctx, captionsSpanReady, migrations.CaptionsSpan)
}

// EnsureSourceCaptions applies db/migrations/058_source_captions.sql when any
// of it is missing, under the migrations' lock. Without it the captions
// backfill hands nothing out, and the deletion gate keeps every original
// whose captions are not read.
func (s *Store) EnsureSourceCaptions(ctx context.Context) error {
	return s.applyLocked(ctx, sourceCaptionsReady, migrations.SourceCaptions)
}
