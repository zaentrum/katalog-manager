package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zaentrum/katalog-manager/db/migrations"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
)

// extrasTable is the table db/migrations/039_item_extras.sql creates: a
// title's extras. Its rows hang off an item and go with it (DeleteItems).
const extrasTable = "com_nalet_katalog_itemextras"

// extrasIndexes are the indexes 039 creates beside its table.
var extrasIndexes = []string{"idx_itemextras_item", "idx_itemextras_source", "idx_itemextras_due"}

// ErrNoExtras says what an extra needs and lacks: the table.
var ErrNoExtras = errors.New("the extras migration (db/migrations/039_item_extras.sql) is not applied")

// ErrExtraGone is the answer for an extra there is not: unknown, or removed.
var ErrExtraGone = errors.New("no such extra: unknown, or removed")

// ErrExtraMissing refuses a worker's report of an extra whose file the scanner
// found gone: its state is the scanner's to change.
var ErrExtraMissing = errors.New("the extra's file is missing")

// ExtraConflict refuses a file that is an extra of another item already.
type ExtraConflict struct{ Extra *model.Extra }

func (e *ExtraConflict) Error() string {
	return fmt.Sprintf("%s is extra %s of item %s already", strOf(e.Extra.SourcePath), e.Extra.ID, e.Extra.ItemID)
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ItemExtrasReady reports whether migration 039 is in place: its table and
// its three indexes. Without it the catalog keeps no extra.
func (s *Store) ItemExtrasReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL
		AND (SELECT count(*) FROM unnest($2::text[]) AS i(name) WHERE to_regclass(i.name) IS NOT NULL) = cardinality($2::text[])`,
		extrasTable, extrasIndexes).Scan(&ok)
	return ok, err
}

// EnsureItemExtras applies db/migrations/039_item_extras.sql when its table or
// any of its indexes is missing. The check comes first for the reason
// EnsureDeletionLog gives.
func (s *Store) EnsureItemExtras(ctx context.Context) error {
	ready, err := s.ItemExtrasReady(ctx)
	if err != nil || ready {
		return err
	}
	_, err = s.pool.Exec(ctx, migrations.ItemExtras)
	return err
}

// noExtras maps a catalog without migration 039 to ErrNoExtras.
func noExtras(err error) error {
	if undefinedTable(err) {
		return ErrNoExtras
	}
	return err
}

// extraCols are the columns an extra is read with, in the order scanExtra
// takes them.
const extraCols = `id, item_id, kind, title, language, seasonnumber, sourcepath, sourcesize, sourceqh1, recordpath,
	registeredby, sortorder, hidden, label, state, error, attempts, failures, nextretryat, dispatchedat, heartbeatat,
	packagepath, packagedat, durationms, videocodec, width, height, peakbandwidthbps, packagesizebytes,
	removedat, removedby, removalreason, createdat, createdby, modifiedat, modifiedby`

// xCols are extraCols of the alias x, for a statement that joins.
var xCols = qualified("x", extraCols)

func scanExtra(row pgx.Row, x *model.Extra) error {
	if err := row.Scan(&x.ID, &x.ItemID, &x.Kind, &x.Title, &x.Language, &x.SeasonNumber, &x.SourcePath, &x.SourceSize,
		&x.SourceQH1, &x.RecordPath, &x.RegisteredBy, &x.SortOrder, &x.Hidden, &x.Label, &x.State, &x.Error,
		&x.Attempts, &x.Failures, &x.NextRetryAt, &x.DispatchedAt, &x.HeartbeatAt, &x.PackagePath, &x.PackagedAt,
		&x.DurationMs, &x.VideoCodec, &x.Width, &x.Height, &x.PeakBandwidthBps, &x.PackageSizeBytes,
		&x.RemovedAt, &x.RemovedBy, &x.RemovalReason, &x.CreatedAt, &x.CreatedBy, &x.ModifiedAt, &x.ModifiedBy); err != nil {
		return err
	}
	for _, t := range []**time.Time{&x.NextRetryAt, &x.DispatchedAt, &x.HeartbeatAt, &x.PackagedAt, &x.RemovedAt} {
		*t = utc(*t)
	}
	x.CreatedAt, x.ModifiedAt = x.CreatedAt.UTC(), x.ModifiedAt.UTC()
	return nil
}

// queryExtras runs a query of extras, each selected as extraCols.
func queryExtras(ctx context.Context, q querier, sql string, args ...any) ([]*model.Extra, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, noExtras(err)
	}
	defer rows.Close()
	var out []*model.Extra
	for rows.Next() {
		var x model.Extra
		if err := scanExtra(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, noExtras(rows.Err())
}

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// extraRow reads one extra by a query of extraCols; nil when there is none.
func extraRow(ctx context.Context, q querier, sql string, args ...any) (*model.Extra, error) {
	var x model.Extra
	err := scanExtra(q.QueryRow(ctx, sql, args...), &x)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, noExtras(err)
	}
	return &x, nil
}

// GetExtra reads the extra id, a removed one too; nil when there is none. A
// catalog without migration 039 has none.
func (s *Store) GetExtra(ctx context.Context, id string) (*model.Extra, error) {
	x, err := extraRow(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+` WHERE id = $1`, id)
	if errors.Is(err, ErrNoExtras) {
		return nil, nil
	}
	return x, err
}

// extraOrder lists a title's extras as a viewer sees them: by the order an
// admin gave them (those without one last), then as they were taken in.
const extraOrder = `sortorder NULLS LAST, createdat, id`

// ExtrasByItem lists the item's extras that are not removed, with removed
// those that are too, as a viewer sees them listed. A catalog without
// migration 039 has none.
func (s *Store) ExtrasByItem(ctx context.Context, itemID string, removed bool) ([]*model.Extra, error) {
	out, err := queryExtras(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE item_id = $1 AND ($2 OR removedat IS NULL) ORDER BY `+extraOrder, itemID, removed)
	if errors.Is(err, ErrNoExtras) {
		return nil, nil
	}
	return out, err
}

// ExtraAt reads the extra the file at path is while it is not removed; nil
// when it is none.
func (s *Store) ExtraAt(ctx context.Context, path string) (*model.Extra, error) {
	return extraRow(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE sourcepath = $1 AND removedat IS NULL`, path)
}

// ExtraWrite is an extra as it is taken in.
type ExtraWrite struct {
	ItemID       string
	Kind         string
	Title        string
	Language     *string
	SeasonNumber *int32
	SourcePath   string
	SourceSize   int64
	SourceQH1    string
	RegisteredBy string
	By           string // who takes it in
}

// AddExtra takes an extra in, waiting to be sent (pending, due now), unless
// its file is an extra already: of the same item, that extra is the answer
// and created is false; of another item, the answer is an *ExtraConflict. A
// removed extra's file is taken in again as a new extra. The id is a new
// lower-case UUID. A catalog without migration 039 answers ErrNoExtras.
func (s *Store) AddExtra(ctx context.Context, w ExtraWrite) (*model.Extra, bool, error) {
	if !model.ValidExtraKind(w.Kind) {
		return nil, false, fmt.Errorf("an extra is of kind %s, not %q", strings.Join(model.ExtraKinds, ", "), w.Kind)
	}
	if strings.TrimSpace(w.Title) == "" || w.SourcePath == "" {
		return nil, false, errors.New("an extra needs a title and a file")
	}
	by := clip(strings.TrimSpace(w.By), 255)
	for range 2 { // a second time when the extra in the way is removed meanwhile
		x, err := extraRow(ctx, s.pool, `INSERT INTO `+extrasTable+` AS x
			(id, item_id, kind, title, language, seasonnumber, sourcepath, sourcesize, sourceqh1, registeredby,
			 state, createdat, createdby, modifiedat, modifiedby)
			VALUES (gen_random_uuid()::varchar, $1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', now(), $10, now(), $10)
			ON CONFLICT (sourcepath) WHERE removedat IS NULL AND sourcepath IS NOT NULL DO NOTHING
			RETURNING `+xCols,
			w.ItemID, w.Kind, clip(strings.TrimSpace(w.Title), 255), w.Language, w.SeasonNumber, w.SourcePath,
			w.SourceSize, w.SourceQH1, w.RegisteredBy, by)
		if err != nil || x != nil {
			return x, x != nil, err
		}
		cur, err := s.ExtraAt(ctx, w.SourcePath)
		if err != nil {
			return nil, false, err
		}
		if cur == nil {
			continue
		}
		if cur.ItemID != w.ItemID {
			return cur, false, &ExtraConflict{Extra: cur}
		}
		return cur, false, nil
	}
	return nil, false, fmt.Errorf("%s could not be taken in: it was an extra, removed while it was taken in", w.SourcePath)
}

// RemoveExtra removes the extra id, as by says who and reason why (the
// extra-removed fact): it stays, removed, plays no more, and its file may be
// taken in again; its package is deleted once grace has passed (the sweep).
// Removing a removed extra changes nothing. It answers the extra, nil when
// there is none.
func (s *Store) RemoveExtra(ctx context.Context, id, by, reason string, grace time.Duration) (*model.Extra, error) {
	var why *string
	if r := strings.TrimSpace(reason); r != "" {
		r = clip(r, 500)
		why = &r
	}
	x, err := extraRow(ctx, s.pool, `UPDATE `+extrasTable+` x SET removedat = now(), removedby = $2, removalreason = $3,
			nextretryat = now() + make_interval(secs => $4::float8), dispatchedat = NULL,
			modifiedat = now(), modifiedby = $2
		WHERE id = $1 AND removedat IS NULL RETURNING `+xCols, id, clip(strings.TrimSpace(by), 255), why, grace.Seconds())
	if err != nil || x != nil {
		return x, err
	}
	return s.GetExtra(ctx, id)
}

// Steps of an extra's packaging a worker reports on, as its trigger names
// them.
const (
	ExtraStepTranscode = "transcode"
	ExtraStepPackage   = "package"
)

// ExtraReport is a worker's report of a step of an extra's packaging: the
// transcoder's (transcode) or the packager's (package), with the status an
// item's step takes: in_progress, done, not_applicable or failed.
type ExtraReport struct {
	Step, Status string
	Error        *string
}

// extraChange is what a worker's report does to an extra.
type extraChange struct {
	state   string
	attempt bool          // a run starts
	fail    bool          // a failed run: one failure more, its error kept
	retryIn time.Duration // after a failed run, when the extra is sent again; 0: no attempt left
	ignore  bool          // the report is of a run the extra is past: nothing changes
}

// extraTransition is what a report does to an extra in state cur that has
// failed failures times in a row, by the policy:
//   - transcode in_progress: transcoding, a run started (unless it was
//     transcoding already);
//   - transcode done or not_applicable: transcoded (not_applicable: the
//     packager packages the source as it is), unless the extra is past its
//     transcode (transcoded, packaging, ready);
//   - package in_progress: packaging;
//   - package done or not_applicable: nothing; packaging-complete makes an
//     extra ready;
//   - failed: a failed run, of the transcode while the extra waits for the
//     transcoder or is transcoding (queued, transcoding), of the package while
//     it waits for the packager or is packaging (transcoded, packaging): one
//     failure more, and pending, sent again a backoff later, while the policy
//     retries that many failures in a row, else failed. A failure of a run the
//     extra is past, or one the service gave up on already (pending, failed),
//     changes nothing.
func extraTransition(cur string, failures int32, rep ExtraReport, pol processing.Policy) (extraChange, error) {
	// The states in which a run of the step is what the extra waits for or
	// runs: the run a failure is of.
	running, ok := map[string][]string{
		ExtraStepTranscode: {model.ExtraQueued, model.ExtraTranscoding},
		ExtraStepPackage:   {model.ExtraTranscoded, model.ExtraPackaging},
	}[rep.Step]
	if !ok {
		return extraChange{}, fmt.Errorf("%w: an extra's steps are transcode and package, not %q", processing.ErrBadStep, rep.Step)
	}
	ignore := extraChange{state: cur, ignore: true}
	switch rep.Status {
	case processing.StatusFailed:
		if !slices.Contains(running, cur) {
			return ignore, nil
		}
		n := int(failures) + 1
		if pol.Retries(n) {
			return extraChange{state: model.ExtraPending, fail: true, retryIn: pol.Delay(n)}, nil
		}
		return extraChange{state: model.ExtraFailed, fail: true}, nil
	case processing.StatusInProgress:
		if rep.Step == ExtraStepTranscode {
			return extraChange{state: model.ExtraTranscoding, attempt: cur != model.ExtraTranscoding}, nil
		}
		return extraChange{state: model.ExtraPackaging}, nil
	case processing.StatusDone, processing.StatusNotApplicable:
		if rep.Step == ExtraStepPackage ||
			slices.Contains([]string{model.ExtraTranscoded, model.ExtraPackaging, model.ExtraReady}, cur) {
			return ignore, nil
		}
		return extraChange{state: model.ExtraTranscoded}, nil
	}
	return extraChange{}, fmt.Errorf("%w: an extra's step reports in_progress, done, not_applicable or failed, not %q",
		processing.ErrBadStatus, rep.Status)
}

// ReportExtraStep records a worker's report of a step of the extra id's
// packaging (extraTransition), by pol: a report taken is its worker's word
// (heartbeatat) and answers a trigger the service sent (dispatchedat
// cleared); a failure's error is kept as a step's is (processing.CleanError).
// It answers the extra as it is after the report. An extra there is not, or a
// removed one, is ErrExtraGone; one whose file is missing ErrExtraMissing.
func (s *Store) ReportExtraStep(ctx context.Context, id string, rep ExtraReport, pol processing.Policy) (*model.Extra, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	cur, err := extraRow(ctx, tx, `SELECT `+extraCols+` FROM `+extrasTable+` WHERE id = $1 FOR UPDATE`, id)
	if err != nil {
		return nil, err
	}
	if cur == nil || cur.RemovedAt != nil {
		return nil, ErrExtraGone
	}
	if cur.State == model.ExtraMissing {
		return cur, ErrExtraMissing
	}
	ch, err := extraTransition(cur.State, cur.Failures, rep, pol)
	if err != nil || ch.ignore {
		return cur, err
	}
	var msg *string
	if ch.fail {
		msg = rep.Error
		if msg == nil || strings.TrimSpace(*msg) == "" {
			m := "the " + rep.Step + " failed"
			msg = &m
		}
		msg = processing.CleanError(*msg)
	}
	x, err := extraRow(ctx, tx, `UPDATE `+extrasTable+` x SET state = $2,
			attempts = x.attempts + CASE WHEN $3 THEN 1 ELSE 0 END,
			failures = x.failures + CASE WHEN $4 THEN 1 ELSE 0 END,
			error = $5,
			nextretryat = CASE WHEN $4 AND $6::float8 > 0 THEN now() + make_interval(secs => $6::float8) END,
			dispatchedat = NULL, heartbeatat = now(), modifiedat = now()
		WHERE id = $1 RETURNING `+xCols, id, ch.state, ch.attempt, ch.fail, msg, ch.retryIn.Seconds())
	if err != nil {
		return nil, err
	}
	return x, tx.Commit(ctx)
}

// ExtraPackage is what packaging-complete records of an extra's package.
type ExtraPackage struct {
	Path             string // the package's folder
	DurationMs       *int64
	VideoCodec       *string // its top rendition's
	Width, Height    *int32
	PeakBandwidthBps *int64 // the highest BANDWIDTH its master playlist names
	SizeBytes        *int64
}

// CompleteExtraPackage records the package of the extra id: it is ready (one
// whose file is missing stays missing, its package kept for when the file is
// back), its failures over; the package's folder, when it was recorded, how
// long it plays, its top rendition's codec and size, its peak bandwidth and
// its size. An extra there is not, or a removed one, is ErrExtraGone.
func (s *Store) CompleteExtraPackage(ctx context.Context, id string, p ExtraPackage) (*model.Extra, error) {
	x, err := extraRow(ctx, s.pool, `UPDATE `+extrasTable+` x SET
			state = CASE WHEN x.state = 'missing' THEN 'missing' ELSE 'ready' END,
			packagepath = $2, packagedat = now(), durationms = $3, videocodec = $4, width = $5, height = $6,
			peakbandwidthbps = $7, packagesizebytes = $8,
			failures = 0, error = NULL, nextretryat = NULL, dispatchedat = NULL, heartbeatat = now(), modifiedat = now()
		WHERE id = $1 AND removedat IS NULL RETURNING `+xCols,
		id, p.Path, p.DurationMs, clipped(p.VideoCodec, 40), p.Width, p.Height, p.PeakBandwidthBps, p.SizeBytes)
	if err != nil {
		return nil, err
	}
	if x == nil {
		return nil, ErrExtraGone
	}
	return x, nil
}

// ClaimedExtra is an extra claimed to have its trigger sent.
type ClaimedExtra struct {
	ID, ItemID, Kind, RegisteredBy string
	Failures                       int32 // its failures in a row: a trigger after one is a retry
}

func claimedExtras(rows pgx.Rows, err error) ([]ClaimedExtra, error) {
	if err != nil {
		return nil, noExtras(err)
	}
	defer rows.Close()
	var out []ClaimedExtra
	for rows.Next() {
		var c ClaimedExtra
		if err := rows.Scan(&c.ID, &c.ItemID, &c.Kind, &c.RegisteredBy, &c.Failures); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, noExtras(rows.Err())
}

// ClaimDueExtras claims the extras waiting to be sent (pending) that are due
// (their retry's time has come, or none is scheduled), of ids when given, at
// most limit, the longest due first: each is queued, its trigger noted as
// sent now. Rows another caller claims are skipped, so two instances never
// send an extra twice. An extra whose item is gone is left alone.
func (s *Store) ClaimDueExtras(ctx context.Context, ids []string, limit int) ([]ClaimedExtra, error) {
	return claimedExtras(s.pool.Query(ctx, `WITH due AS (
			SELECT x.id FROM `+extrasTable+` x
			WHERE x.state = 'pending' AND x.removedat IS NULL AND (x.nextretryat IS NULL OR x.nextretryat <= now())
			  AND ($1::text[] IS NULL OR x.id = ANY($1::text[]))
			  AND EXISTS (SELECT 1 FROM com_nalet_katalog_items i WHERE i.id = x.item_id)
			ORDER BY COALESCE(x.nextretryat, x.modifiedat), x.id
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		UPDATE `+extrasTable+` x SET state = 'queued', dispatchedat = now(), nextretryat = NULL, modifiedat = now()
		FROM due WHERE x.id = due.id
		RETURNING x.id, x.item_id, x.kind, x.registeredby, x.failures`, nullable(ids), limit))
}

func nullable(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// PutBackExtras puts back the extras whose trigger could not be sent, those
// of ids still waiting on that send: pending, due again in again (now when
// zero), saying why. It counts no failure: the extra did not fail.
func (s *Store) PutBackExtras(ctx context.Context, ids []string, why string, again time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE `+extrasTable+` SET state = 'pending', dispatchedat = NULL,
			nextretryat = now() + make_interval(secs => $3::float8), error = $2, modifiedat = now()
		WHERE id = ANY($1) AND state = 'queued' AND dispatchedat IS NOT NULL`, ids, processing.CleanError(why), again.Seconds())
	return noExtras(err)
}

// ReapedExtra is an extra the reaper took for stuck: in Was, now in State.
type ReapedExtra struct {
	ClaimedExtra
	Was, State string
}

// ExtraTimeouts are how long an extra may wait for its worker (queued: for
// the transcoder to start; transcoded: for the packager to start) and how
// long a worker may be silent while it runs (transcoding, packaging).
type ExtraTimeouts struct {
	Wait, Silent           time.Duration
	WaitLabel, SilentLabel string // how a reaped extra's error names them: 24h, 2h
}

// ReapExtras takes the extras stuck in their packaging for failed runs, at
// most limit, by pol: one queued whose trigger no transcoder started within
// the wait, one transcoded that no packager started within it, one
// transcoding or packaging whose worker has been silent for longer than the
// silent timeout. Each counts a failure and says so in its error. With an
// attempt left, one transcoded stays transcoded, its trigger noted as sent
// again (the caller sends the transcoder a trigger that is no retry, so that
// it announces the transcode again), and any other is put back to pending,
// sent again a backoff later; with none left it is failed. Rows another
// caller holds are skipped.
func (s *Store) ReapExtras(ctx context.Context, t ExtraTimeouts, pol processing.Policy, limit int) ([]ReapedExtra, error) {
	delays := []float64{}
	for n := 1; pol.Retries(n); n++ {
		delays = append(delays, pol.Delay(n).Seconds())
	}
	rows, err := s.pool.Query(ctx, `WITH stale AS (
			SELECT x.id, x.state, x.failures FROM `+extrasTable+` x
			WHERE x.removedat IS NULL AND (
			     (x.state = 'queued' AND COALESCE(x.dispatchedat, x.modifiedat) < now() - make_interval(secs => $1::float8))
			  OR (x.state = 'transcoded'
			      AND COALESCE(x.dispatchedat, x.heartbeatat, x.modifiedat) < now() - make_interval(secs => $1::float8))
			  OR (x.state IN ('transcoding', 'packaging')
			      AND COALESCE(x.heartbeatat, x.modifiedat) < now() - make_interval(secs => $2::float8)))
			ORDER BY x.modifiedat, x.id
			LIMIT $5
			FOR UPDATE SKIP LOCKED)
		UPDATE `+extrasTable+` x SET
			failures = x.failures + 1,
			error = m.msg,
			state = CASE WHEN m.delay IS NULL THEN 'failed' WHEN stale.state = 'transcoded' THEN 'transcoded' ELSE 'pending' END,
			nextretryat = CASE WHEN m.delay IS NOT NULL AND stale.state <> 'transcoded' THEN now() + make_interval(secs => m.delay) END,
			dispatchedat = CASE WHEN m.delay IS NOT NULL AND stale.state = 'transcoded' THEN now() END,
			modifiedat = now()
		FROM stale, LATERAL (SELECT ($6::float8[])[stale.failures + 1] AS delay, CASE stale.state
			WHEN 'queued' THEN 'timed out: its trigger was sent and no transcoder started it within ' || $3::text
			WHEN 'transcoded' THEN 'timed out: no packager started on its transcode within ' || $3::text ||
				'; the transcoder is asked to announce it again'
			WHEN 'transcoding' THEN 'timed out: no word from its transcoder for ' || $4::text
			ELSE 'timed out: no word from its packager for ' || $4::text END AS msg) m
		WHERE x.id = stale.id
		RETURNING x.id, x.item_id, x.kind, x.registeredby, x.failures, stale.state, x.state`,
		t.Wait.Seconds(), t.Silent.Seconds(), t.WaitLabel, t.SilentLabel, limit, delays)
	if err != nil {
		return nil, noExtras(err)
	}
	defer rows.Close()
	var out []ReapedExtra
	for rows.Next() {
		var r ReapedExtra
		if err := rows.Scan(&r.ID, &r.ItemID, &r.Kind, &r.RegisteredBy, &r.Failures, &r.Was, &r.State); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, noExtras(rows.Err())
}

// UnreapExtras undoes the reaping of the transcoded extras of ids whose
// trigger could not be sent: the failure the reaper counted goes, and they
// are reaped again at the next sweep. why says what happened.
func (s *Store) UnreapExtras(ctx context.Context, ids []string, why string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE `+extrasTable+` SET failures = GREATEST(failures - 1, 0), dispatchedat = NULL,
			error = $2, modifiedat = now()
		WHERE id = ANY($1) AND state = 'transcoded' AND dispatchedat IS NOT NULL`, ids, processing.CleanError(why))
	return noExtras(err)
}

// RemovedPackage is a removed extra whose package is due to be deleted.
type RemovedPackage struct {
	ID          string
	PackagePath *string
}

// ClaimRemovedPackages claims the removed extras whose package is due to be
// deleted (their removal's grace is over), at most limit: they are not due
// again unless put back (PutBackRemovedPackages). Rows another caller holds
// are skipped.
func (s *Store) ClaimRemovedPackages(ctx context.Context, limit int) ([]RemovedPackage, error) {
	rows, err := s.pool.Query(ctx, `WITH due AS (
			SELECT x.id FROM `+extrasTable+` x
			WHERE x.removedat IS NOT NULL AND x.nextretryat <= now()
			ORDER BY x.nextretryat, x.id
			LIMIT $1
			FOR UPDATE SKIP LOCKED)
		UPDATE `+extrasTable+` x SET nextretryat = NULL, modifiedat = now()
		FROM due WHERE x.id = due.id
		RETURNING x.id, x.packagepath`, limit)
	if err != nil {
		return nil, noExtras(err)
	}
	defer rows.Close()
	var out []RemovedPackage
	for rows.Next() {
		var p RemovedPackage
		if err := rows.Scan(&p.ID, &p.PackagePath); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, noExtras(rows.Err())
}

// RemovedPackagesDeleted notes that the packages of the removed extras of ids
// are gone from disk; PutBackRemovedPackages puts those it could not delete
// back, due again in again, saying why.
func (s *Store) RemovedPackagesDeleted(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE `+extrasTable+` SET packagepath = NULL, modifiedat = now()
		WHERE id = ANY($1) AND removedat IS NOT NULL`, ids)
	return noExtras(err)
}

// PutBackRemovedPackages: see RemovedPackagesDeleted.
func (s *Store) PutBackRemovedPackages(ctx context.Context, ids []string, why string, again time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE `+extrasTable+` SET nextretryat = now() + make_interval(secs => $3::float8),
			error = $2, modifiedat = now()
		WHERE id = ANY($1) AND removedat IS NOT NULL`, ids, processing.CleanError(why), again.Seconds())
	return noExtras(err)
}

// ResetExtras packages the extras of ids again (a re-encode), those decide
// takes, in one transaction that holds them while it decides: each waits to
// be sent afresh (pending, due now), its failures in a row and its error
// cleared, its file no longer missing; its package plays until the new one is
// in place. decide sees each extra that is not removed, and says why it is
// left alone, "" to take it. It answers the extras looked at as they are
// after, and why each one left alone was.
func (s *Store) ResetExtras(ctx context.Context, ids []string, by string, decide func(*model.Extra) string) ([]*model.Extra, map[string]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	cur, err := queryExtras(ctx, tx, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE id = ANY($1) AND removedat IS NULL ORDER BY `+extraOrder+` FOR UPDATE`, ids)
	if err != nil {
		return nil, nil, err
	}
	whys := map[string]string{}
	var take []string
	for _, x := range cur {
		if why := decide(x); why != "" {
			whys[x.ID] = why
			continue
		}
		take = append(take, x.ID)
	}
	if len(take) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+extrasTable+` SET state = 'pending', failures = 0, error = NULL,
				nextretryat = NULL, dispatchedat = NULL, hidden = CASE WHEN state = 'missing' THEN false ELSE hidden END,
				modifiedat = now(), modifiedby = $2
			WHERE id = ANY($1)`, take, clip(strings.TrimSpace(by), 255)); err != nil {
			return nil, nil, err
		}
	}
	after, err := queryExtras(ctx, tx, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE id = ANY($1) AND removedat IS NULL ORDER BY `+extraOrder, ids)
	if err != nil {
		return nil, nil, err
	}
	return after, whys, tx.Commit(ctx)
}

// ExtraSource is the file an extra is packaged from, as the scanner finds it.
type ExtraSource struct {
	Path string
	Size int64
	QH1  string
}

// UpdateExtraSource records where the scanner finds the file of the extra id,
// its size and its quick hash, as by says who: a file found again after it
// went missing is no longer hidden, and ready again when its package was made
// of the same file (its size and quick hash unchanged), else it waits to be
// packaged (pending); a file that changed is packaged again unless its
// packaging runs; a file found as it was changes nothing. It answers the
// extra as it is after; nil when there is none, or it is removed.
func (s *Store) UpdateExtraSource(ctx context.Context, id string, src ExtraSource, by string) (*model.Extra, error) {
	x, err := s.updateExtraSource(ctx, id, src, by)
	if err != nil || x != nil {
		return x, err
	}
	if x, err = s.GetExtra(ctx, id); err != nil || x == nil || x.RemovedAt != nil {
		return nil, err
	}
	return x, nil
}

func (s *Store) updateExtraSource(ctx context.Context, id string, src ExtraSource, by string) (*model.Extra, error) {
	return extraRow(ctx, s.pool, `UPDATE `+extrasTable+` x SET
			sourcepath = $2::text, sourcesize = $3::bigint, sourceqh1 = $4::text,
			hidden = CASE WHEN x.state = 'missing' THEN false ELSE x.hidden END,
			state = CASE
				WHEN x.state = 'missing' AND x.packagedat IS NOT NULL AND same.file THEN 'ready'
				WHEN x.state = 'missing' THEN 'pending'
				WHEN NOT same.file AND x.state IN ('pending', 'ready', 'failed') THEN 'pending'
				ELSE x.state END,
			failures = CASE WHEN x.state = 'missing' OR (NOT same.file AND x.state IN ('pending', 'ready', 'failed'))
				THEN 0 ELSE x.failures END,
			error = CASE WHEN x.state = 'missing' OR (NOT same.file AND x.state IN ('pending', 'ready', 'failed'))
				THEN NULL ELSE x.error END,
			nextretryat = CASE WHEN x.state = 'missing' OR (NOT same.file AND x.state IN ('pending', 'ready', 'failed'))
				THEN NULL ELSE x.nextretryat END,
			modifiedat = now(), modifiedby = $5
		FROM (SELECT x2.sourcesize IS NOT DISTINCT FROM $3::bigint AND x2.sourceqh1 IS NOT DISTINCT FROM $4::text AS file
			FROM `+extrasTable+` x2 WHERE x2.id = $1) same
		WHERE x.id = $1 AND x.removedat IS NULL
		  AND (x.state = 'missing' OR NOT same.file OR x.sourcepath IS DISTINCT FROM $2::text)
		RETURNING `+xCols,
		id, src.Path, src.Size, src.QH1, clip(strings.TrimSpace(by), 255))
}

// MovableExtras lists the item's extras that are not removed whose file has
// size and quick hash qh1: the extra a file found at a new path may be, moved.
func (s *Store) MovableExtras(ctx context.Context, itemID string, size int64, qh1 string) ([]*model.Extra, error) {
	return queryExtras(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE item_id = $1 AND removedat IS NULL AND sourcesize = $2 AND sourceqh1 = $3
		ORDER BY createdat, id`, itemID, size, qh1)
}

// ScannerExtras lists the extras the scanner took in that are not removed and
// whose file was not found missing, with their files.
func (s *Store) ScannerExtras(ctx context.Context) ([]*model.Extra, error) {
	return queryExtras(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+`
		WHERE registeredby = 'scanner' AND removedat IS NULL AND state <> 'missing' AND sourcepath IS NOT NULL
		ORDER BY id`)
}

// MarkExtrasMissing marks the extras of ids whose file the scanner found gone:
// missing and hidden, until the file is found again (UpdateExtraSource). Only
// those not removed change. It answers how many.
func (s *Store) MarkExtrasMissing(ctx context.Context, ids []string, by string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE `+extrasTable+` SET state = 'missing', hidden = true, nextretryat = NULL,
			dispatchedat = NULL, modifiedat = now(), modifiedby = $2
		WHERE id = ANY($1) AND removedat IS NULL AND state <> 'missing'`, ids, clip(strings.TrimSpace(by), 255))
	if err != nil {
		return 0, noExtras(err)
	}
	return int(tag.RowsAffected()), nil
}

// ExtrasOfItems lists the extras of the items of ids, the removed ones too:
// what their files and packages are, for a delete that removes them from
// disk.
func (s *Store) ExtrasOfItems(ctx context.Context, ids []string) ([]*model.Extra, error) {
	out, err := queryExtras(ctx, s.pool, `SELECT `+extraCols+` FROM `+extrasTable+` WHERE item_id = ANY($1) ORDER BY id`, ids)
	if errors.Is(err, ErrNoExtras) {
		return nil, nil
	}
	return out, err
}

// deleteExtrasOf removes, in tx, the extras of the items of ids, on a catalog
// that has the table (migration 039).
func deleteExtrasOf(ctx context.Context, tx pgx.Tx, ids []string) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass($1::text) IS NOT NULL`, extrasTable).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err := tx.Exec(ctx, `DELETE FROM `+extrasTable+` WHERE item_id = ANY($1)`, ids)
	return err
}
