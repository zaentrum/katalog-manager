// Package captions is what the catalog knows of the originals' closed
// captions (migration 058, com_nalet_katalog_sourcecaptions), and the
// backfill that has the analyzer read them.
//
// A source record's closedCaptions comes from a probe that decodes no frame,
// which says false of an original whose video carries A53 closed captions
// (EIA/CEA-608/708, a cc_data SEI with each picture): nothing in a container
// tells, the decoder's side data does. An original that carries closed
// captions, or whose captions are not known, is not retired (library's
// captions rule), so the backfill has the analyzer read them: it hands the
// originals on disk, present or superseded, whose captions are not read (or
// were read with a window that decoded no frame) to the analyzer (POST
// /api/captions/claim), at the pace the settings allow, and only within its
// window where one is set (any time by default). The analyzer reads each in
// windows across its picture (its start, and 10 to 90 % of its span), up to
// Packets video packets each from a keyframe, with ffprobe 7.1, and reports
// what each window found with the claim's token
// (PUT /api/captions/sources/{sourceId}, Record). katalog-manager opens no
// media file: it hands out where an original lies (a stat of it) and keeps
// what the analyzer read. An original handed out is claimed until its report
// comes back or its lease ends; one whose run never reported back, whose
// read failed or had a window that decoded no frame, or that is not on disk
// when it comes up counts a failure and waits its backoff, and so does one
// whose report, taken, leaves it not read in full; one given back unread
// waits ReleaseWait. What was read in full, and what a read found that keeps
// it for good, is not handed out again.
// The originals whose retirement waits on their captions alone go first
// (the retire job notes them, migration 059: their title has a complete
// version and nothing else keeps them), then those of titles with a complete
// version (the deletion's next), then the rest, the oldest first in each.
// Everything it keeps is in the catalog, so it resumes where it was after a
// restart of either side.
package captions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/picture"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The backfill's settings, read on every claim: an instance switches it off,
// slows it down or moves its hours by setting them, without a restart.
const (
	// SettingBackfill is on (the default) or off: whether originals are
	// handed out at all. On by default, as the deletion gate's captions
	// rule ships with it: off, an original is kept until its captions are
	// read.
	SettingBackfill = "captions.backfill"
	// SettingWindow is when originals are handed out, a daily span
	// "HH:MM-HH:MM" in UTC (it may cross midnight): an installation can keep
	// the reads out of the hours its library is watched. Empty, the
	// default, is any time.
	SettingWindow = "captions.backfill.window"
	// SettingInflight is how many originals may be handed out and not
	// reported at once (2).
	SettingInflight = "captions.backfill.inflight"
	// SettingRate is how many hand-outs there are an hour at most (600; an
	// original handed out again counts again); 0 hands out none.
	SettingRate = "captions.backfill.rate"
	// SettingLease is how long an original handed out is the worker's
	// before it is taken for lost (10m: a read of its first packets takes
	// seconds).
	SettingLease = "captions.backfill.lease"
)

// The settings' defaults.
const (
	DefaultWindow   = "" // any time
	DefaultInflight = 2
	DefaultRate     = 600
	DefaultLease    = 10 * time.Minute
)

// Packets is how many video packets a read decodes of a window at most,
// from its keyframe (the window plan's, library.WindowPackets): a captioned
// video's SEI comes with every picture.
const Packets = library.WindowPackets

// Settings are the backfill's settings as they are read.
type Settings struct {
	On       bool
	Window   Window
	Inflight int
	Rate     int
	Lease    time.Duration
	// Problems says of each setting that could not be read what it held and
	// what is used instead.
	Problems []string
}

// DefaultSettings are the settings of a catalog that sets none.
func DefaultSettings() Settings {
	w, _ := picture.ParseWindow(DefaultWindow)
	return Settings{On: true, Window: Window{Window: w}, Inflight: DefaultInflight, Rate: DefaultRate, Lease: DefaultLease}
}

// Window is when the backfill hands out originals: a daily span of UTC, as
// the picture backfill's (picture.Window), or never, a setting that is no
// window.
type Window struct {
	picture.Window
	Never bool
}

// Contains reports whether t lies in the window.
func (w Window) Contains(t time.Time) bool { return !w.Never && w.Window.Contains(t) }

// String writes the window as it is set ("02:00-07:00 UTC", "any time").
func (w Window) String() string {
	if w.Never {
		return "never (no window that could be read)"
	}
	return w.Window.String()
}

// Closes is when the window that holds at t ends; the zero time when it never
// ends (any time).
func (w Window) Closes(t time.Time) time.Time {
	if w.Never {
		return time.Time{}
	}
	return w.Window.Closes(t)
}

// ReadSettings reads the backfill's settings, each key without the spaces
// around it, and of a key the table holds twice the row of the lowest id. A
// value that cannot be read is its default (a window that cannot be read
// hands out nothing), said in Problems.
func ReadSettings(ctx context.Context, q library.Querier) (Settings, error) {
	s := DefaultSettings()
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (btrim(key)) btrim(key), valuetext FROM com_nalet_katalog_settings
		WHERE btrim(key) LIKE 'captions.backfill%' ORDER BY btrim(key), id`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return s, err
		}
		s.set(key, value)
	}
	return s, rows.Err()
}

func (s *Settings) set(key, value string) {
	v := strings.TrimSpace(value)
	bad := func(what string) {
		s.Problems = append(s.Problems, fmt.Sprintf("%s is %q, which is not %s", key, value, what))
	}
	switch key {
	case SettingBackfill:
		switch strings.ToLower(v) {
		case "on", "":
			s.On = true
		case "off":
			s.On = false
		default:
			bad("on or off; the backfill is on")
		}
	case SettingWindow:
		w, err := picture.ParseWindow(v)
		if err != nil {
			s.Window = Window{Never: true}
			bad("a window, HH:MM-HH:MM in UTC, as 02:00-07:00; no original is handed out until it is one")
			return
		}
		s.Window = Window{Window: w}
	case SettingInflight:
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			s.Inflight = n
		} else if v != "" {
			bad(fmt.Sprintf("a number of originals, 1 or more; it is %d", DefaultInflight))
		}
	case SettingRate:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			s.Rate = n
		} else if v != "" {
			bad(fmt.Sprintf("a number of originals an hour, 0 or more; it is %d", DefaultRate))
		}
	case SettingLease:
		if d, err := library.ParseDuration(v); err == nil && d >= time.Minute {
			s.Lease = d
		} else if v != "" {
			bad("a duration of a minute or more, as 10m or 1h; it is " + library.DurationText(DefaultLease))
		}
	}
}

// Job is an original the backfill hands out: its source and its title,
// where it lies (Path), how many video packets to decode of each window from
// its keyframe at most, and the token its report names (Report.Token).
type Job struct {
	SourceID string `json:"sourceId"`
	ItemID   string `json:"itemId"`
	Path     string `json:"path"`
	Packets  int    `json:"packets"`
	Token    string `json:"token"`
	// DurationMs is the length the original's source record says
	// (container.durationMs), that the analyzer holds the length it reads
	// to; none where the record says none.
	DurationMs *int64 `json:"durationMs,omitempty"`
}

// Claim is what a claim hands out: its originals and the time their reads
// end by (the window's end, or the lease's), or none and why, and when to
// ask again.
type Claim struct {
	Items             []Job      `json:"items"`
	Deadline          *time.Time `json:"deadline,omitempty"`
	Idle              string     `json:"idle,omitempty"`
	RetryAfterSeconds int        `json:"retryAfterSeconds,omitempty"`
}

// Service is the originals' closed captions and their backfill, over the
// catalog of st.
type Service struct {
	st  *store.Store
	now func() time.Time
}

// New is the closed captions of the catalog st.
func New(st *store.Store) *Service {
	return &Service{st: st, now: time.Now}
}

// WithClock is s with now as its clock (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func (s *Service) ready(ctx context.Context) error {
	ok, err := s.st.SourceCaptionsReady(ctx)
	if err == nil && ok {
		ok, err = s.st.CaptionsWaitingReady(ctx)
	}
	if err == nil && ok {
		ok, err = s.st.CaptionsSpanReady(ctx)
	}
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotReady
	}
	return nil
}

// claimLock is the advisory lock a claim holds, the catalog's (its
// schema's): claims are counted against the pace one at a time.
const claimLock = `hashtext('captions-backfill/' || current_schema())`

// candidates is how many originals a claim looks at for each it hands out:
// one that is not on disk fails (and waits its backoff), and the next goes.
const candidates = 8

// Claim hands out up to limit originals to the worker (1 at least, at most
// what the pace allows), or none and why: the backfill is set off, outside its
// window, originals enough are in flight, the hour's rate is reached, or
// every original on disk has its captions read. A claim whose lease ended
// first counts as a failure of its original.
func (s *Service) Claim(ctx context.Context, worker string, limit int) (Claim, error) {
	if err := s.ready(ctx); err != nil {
		return Claim{}, err
	}
	now := s.now().UTC()
	pool := s.st.Pool()
	set, err := ReadSettings(ctx, pool)
	if err != nil {
		return Claim{}, err
	}
	switch {
	case !set.On:
		return idle(SettingBackfill+" is off", 15*time.Minute), nil
	case set.Window.Never:
		return idle(SettingWindow+" is no window", 15*time.Minute), nil
	case !set.Window.Contains(now):
		return idle("outside "+SettingWindow+" ("+set.Window.String()+")", set.Window.Opens(now).Sub(now)), nil
	case set.Rate == 0:
		return idle(SettingRate+" is 0", 15*time.Minute), nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Claim{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+claimLock+`)`); err != nil {
		return Claim{}, err
	}
	if err := expire(ctx, tx, now, set.Lease); err != nil {
		return Claim{}, err
	}
	var inflight, recent int
	var oldest *time.Time
	// the hour's hand-outs, each one (an original handed out again counts
	// again), and those before it forgotten
	if _, err := tx.Exec(ctx, `DELETE FROM `+store.CaptionHandoutsTable+` WHERE handedoutat <= $1`,
		now.Add(-time.Hour)); err != nil {
		return Claim{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+store.SourceCaptionsTable+` WHERE claimedat IS NOT NULL),
			count(*), min(handedoutat) FROM `+store.CaptionHandoutsTable+` WHERE handedoutat > $1`,
		now.Add(-time.Hour)).Scan(&inflight, &recent, &oldest); err != nil {
		return Claim{}, err
	}
	if inflight >= set.Inflight {
		return idle(fmt.Sprintf("%d originals are being read (%s %d)", inflight, SettingInflight, set.Inflight),
			time.Minute), tx.Commit(ctx)
	}
	if recent >= set.Rate {
		wait := time.Minute
		if oldest != nil {
			wait = oldest.Add(time.Hour).Sub(now)
		}
		return idle(fmt.Sprintf("%d hand-outs were made in the last hour (%s %d)", recent, SettingRate, set.Rate),
			wait), tx.Commit(ctx)
	}
	n := min(max(limit, 1), set.Inflight-inflight, set.Rate-recent)

	rows, err := tx.Query(ctx, candidatesSQL, now, n*candidates)
	if err != nil {
		return Claim{}, err
	}
	type candidate struct {
		id, item, path string
		size           int64
		qh1, dir       *string
		recorded       *time.Time
	}
	var cs []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.item, &c.path, &c.size, &c.qh1, &c.dir, &c.recorded); err != nil {
			rows.Close()
			return Claim{}, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Claim{}, err
	}
	// the reads' deadline: the lease's end, or the window's when it comes
	// first (where a window is set, nothing is handed out outside it)
	deadline := now.Add(set.Lease)
	if closes := set.Window.Closes(now); !closes.IsZero() && closes.Before(deadline) {
		deadline = closes
	}
	out := Claim{Items: []Job{}}
	for _, c := range cs {
		if len(out.Items) == n {
			break
		}
		// where it lies, as the catalog says: a stat, nothing of it read
		if fi, err := os.Stat(c.path); err != nil || !fi.Mode().IsRegular() {
			if err := fail(ctx, tx, c.id, c.item, "nothing to read: the original is not at "+c.path, now); err != nil {
				return Claim{}, err
			}
			continue
		}
		job := Job{SourceID: c.id, ItemID: c.item, Path: c.path, Packets: Packets}
		// the length its record says, which the read is held to, and the
		// span of its picture its records give, which its windows are
		// placed by (none of a record that says no length: its report is
		// a finding of that)
		// a record that can't be read is not handed out: what it says of the
		// original's length can't be told, which is no finding of it (one
		// that is read and says no length is, and so is one not yet written)
		if why := recordReadable(c.dir); why != "" {
			if err := fail(ctx, tx, c.id, c.item, "its source record can't be read: "+why, now); err != nil {
				return Claim{}, err
			}
			continue
		}
		var spanStart, spanEnd, spanStep *float64
		if c.dir != nil {
			job.DurationMs = library.RecordedDurationMs(*c.dir)
			if span := library.RecordedSpan(*c.dir); span != nil && job.DurationMs != nil {
				spanStart, spanEnd, spanStep = &span.Start, &span.End, &span.Step
			}
		}
		var token string
		if err := tx.QueryRow(ctx, `INSERT INTO `+store.SourceCaptionsTable+`
				(source_id, item_id, state, claimedat, claimedby, claimtoken, lastclaimedat, attempts, claimsize, claimqh1,
				 claimdurationms, claimrecordedat, claimspanstart, claimspanend, claimspanstep, createdat, modifiedat)
			VALUES ($1, $2, 'pending', $3, $4, gen_random_uuid()::varchar, $3, 1, $5, $6, $7, $8, $9, $10, $11, $3, $3)
			ON CONFLICT (source_id) DO UPDATE SET claimedat = $3, claimedby = $4, claimtoken = gen_random_uuid()::varchar,
				lastclaimedat = $3, attempts = `+store.SourceCaptionsTable+`.attempts + 1, claimsize = $5, claimqh1 = $6,
				claimdurationms = $7, claimrecordedat = $8, claimspanstart = $9, claimspanend = $10, claimspanstep = $11,
				modifiedat = $3
			RETURNING claimtoken`, c.id, c.item, now, clip(worker, 255), c.size, c.qh1, job.DurationMs,
			c.recorded, spanStart, spanEnd, spanStep).Scan(&token); err != nil {
			return Claim{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+store.CaptionHandoutsTable+` (source_id, handedoutat) VALUES ($1, $2)`,
			c.id, now); err != nil {
			return Claim{}, err
		}
		job.Token = token
		out.Items = append(out.Items, job)
	}
	if err := tx.Commit(ctx); err != nil {
		return Claim{}, err
	}
	if len(out.Items) == 0 {
		if len(cs) == 0 {
			return idle("every original on disk has its closed captions read, or waits its backoff", time.Hour), nil
		}
		return idle("the originals looked at were not on disk, or their records can't be read", time.Minute), nil
	}
	out.Deadline = &deadline
	return out, nil
}

func idle(why string, after time.Duration) Claim {
	return Claim{Items: []Job{}, Idle: why, RetryAfterSeconds: max(1, int(after.Round(time.Second)/time.Second))}
}

// candidatesSQL are the originals to read, $2 of them at most, in the order
// they are handed out: an original on disk (present or superseded, where
// the catalog says it lies), its source record written, without a read the
// gate takes of the file at its place (readUsable), not claimed now and not
// waiting out a failure's or a release's backoff ($1 is now); those whose
// retirement waits on their captions alone first (the retire job's note,
// retirewaitingsince, which nothing but the job writes: no gate is read
// here), then those of titles with a complete version, then the rest; of
// each, the oldest first.
var candidatesSQL = `
	SELECT s.id, s.item_id, s.arrivalpath, s.sizebytes, s.qh1, s.recorddir, s.recordedat
	FROM com_nalet_katalog_itemsources s
	LEFT JOIN ` + store.SourceCaptionsTable + ` c ON c.source_id = s.id
	LEFT JOIN LATERAL (SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
	                   WHERE v.item_id = s.item_id AND v.state = 'complete') AS packaged) p ON true
	WHERE s.state IN ('present', 'superseded') AND COALESCE(s.arrivalpath, '') <> '' AND s.recordedat IS NOT NULL
	  AND (c.source_id IS NULL OR (c.claimedat IS NULL AND (c.nextattemptat IS NULL OR c.nextattemptat <= $1)))
	  AND NOT ` + readUsable("c", "s") + `
	ORDER BY c.retirewaitingsince IS NOT NULL AND p.packaged DESC, p.packaged DESC, s.createdat, s.id
	LIMIT $2`

// readUsable is the SQL condition that the row c keeps what the gate takes
// of the source s, of the file the source is now (its size, and its quick
// hash where both say one), that no read again of the same file changes: a
// read that found what keeps the original for good - captions, in a window
// (one that failed otherwise too) or a stream of their own, or frames in
// fields counted in a window -, of whichever record; a read in full of the
// record as it is now (recordedat: none of its windows failed, and every
// window that decoded frames says how many are interlaced and repeat a
// field) whose report the catalog keeps (readreport, the contract's deletion
// gate reads it); a failure that found a stream of captions of its own or
// something off the closed lists (offlist) of the file handed out; or a
// finding (lengthMismatch, noRecordLength, noPlan, unscanned) of the file
// handed out and of the record as it is now. Another file, or another record
// of a read that found nothing, goes back to be read.
func readUsable(c, s string) string {
	return `(` + c + `.sourcesize IS NOT DISTINCT FROM ` + s + `.sizebytes
		AND (` + c + `.sourceqh1 IS NULL OR ` + s + `.qh1 IS NULL OR ` + c + `.sourceqh1 = ` + s + `.qh1)
		AND ((` + c + `.state IS NOT DISTINCT FROM 'read' AND (` + c + `.captions IS TRUE
				OR COALESCE(` + c + `.captionstreams, 0) > 0 OR ` + fieldsCounted(c+".captionwindows") + `))
		  OR (` + c + `.state IS NOT DISTINCT FROM 'read' AND ` + c + `.sourcerecordedat IS NOT DISTINCT FROM ` + s + `.recordedat
			AND ` + c + `.readreport IS NOT NULL AND NOT ` + failedWindow(c+".captionwindows") + `
			AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(` + c + `.captionwindows, '[]'::jsonb)) w
				WHERE COALESCE((w->>'frames')::numeric, 0) > 0
				  AND (w->'interlacedFrames' IS NULL OR w->'repeatFrames' IS NULL
				    OR jsonb_typeof(w->'interlacedFrames') = 'null' OR jsonb_typeof(w->'repeatFrames') = 'null')))
		  OR (` + c + `.state IS NOT DISTINCT FROM 'failed' AND ` + c + `.sourcesize IS NOT NULL
			AND (COALESCE(` + c + `.captionstreams, 0) > 0 OR COALESCE(jsonb_array_length(` + c + `.offlist), 0) > 0)
			AND ` + c + `.finding IS NULL)
		  OR (` + c + `.state IS NOT DISTINCT FROM 'failed' AND ` + c + `.sourcesize IS NOT NULL
			AND ` + c + `.finding IN ('` + library.FindingLengthMismatch + `', '` + library.FindingNoLength + `', '` +
		library.FindingNoPlan + `', '` + library.FindingUnscanned + `')
			AND ` + c + `.sourcerecordedat IS NOT DISTINCT FROM ` + s + `.recordedat)))`
}

// fieldsCounted is the SQL condition that the windows of the JSON column col
// hold one that counted frames interlaced or repeating a field.
func fieldsCounted(col string) string {
	return `EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(` + col + `, '[]'::jsonb)) w
		WHERE (jsonb_typeof(w->'interlacedFrames') = 'number' AND (w->>'interlacedFrames')::numeric > 0)
		   OR (jsonb_typeof(w->'repeatFrames') = 'number' AND (w->>'repeatFrames')::numeric > 0))`
}

// failedWindow is the SQL condition that the windows of the JSON column
// col hold one that decoded no frame.
func failedWindow(col string) string {
	return `EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(` + col + `, '[]'::jsonb)) w
		WHERE COALESCE((w->>'frames')::numeric, 0) = 0)`
}

// expire takes the claims whose lease ended for failures of their originals:
// their runs never reported back. A claim's token is kept as the lapsed one:
// its run's report, late, is counted already.
func expire(ctx context.Context, tx pgx.Tx, now time.Time, lease time.Duration) error {
	rows, err := tx.Query(ctx, `SELECT source_id, failures FROM `+store.SourceCaptionsTable+`
		WHERE claimedat IS NOT NULL AND claimedat <= $1 FOR UPDATE`, now.Add(-lease))
	if err != nil {
		return err
	}
	type lost struct {
		id       string
		failures int32
	}
	var ls []lost
	for rows.Next() {
		var l lost
		if err := rows.Scan(&l.id, &l.failures); err != nil {
			rows.Close()
			return err
		}
		ls = append(ls, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range ls {
		f := l.failures + 1
		if _, err := tx.Exec(ctx, `UPDATE `+store.SourceCaptionsTable+` SET claimedat = NULL, claimedby = NULL,
			lapsedtoken = claimtoken, claimtoken = NULL, failures = $2, nextattemptat = $3, modifiedat = $4,
			state = CASE WHEN state = 'pending' THEN 'failed' ELSE state END,
			reason = CASE WHEN state IN ('pending', 'failed') THEN $5 ELSE reason END
			WHERE source_id = $1`, l.id, f, now.Add(backoff(f)), now,
			"its run did not report back within "+library.DurationText(lease)); err != nil {
			return err
		}
	}
	return nil
}

// fail counts a failure of an original the backfill could not hand out (it
// is not on disk), which waits its backoff; a read stays as it is.
func fail(ctx context.Context, tx pgx.Tx, id, item, why string, now time.Time) error {
	var f int32
	if err := tx.QueryRow(ctx, `SELECT failures FROM `+store.SourceCaptionsTable+` WHERE source_id = $1`, id).
		Scan(&f); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	f++
	_, err := tx.Exec(ctx, `INSERT INTO `+store.SourceCaptionsTable+`
			(source_id, item_id, state, reason, failures, nextattemptat, createdat, modifiedat)
		VALUES ($1, $2, 'failed', $3, $4, $5, $6, $6)
		ON CONFLICT (source_id) DO UPDATE SET reason = EXCLUDED.reason, failures = EXCLUDED.failures,
			nextattemptat = EXCLUDED.nextattemptat, modifiedat = EXCLUDED.modifiedat,
			state = CASE WHEN `+store.SourceCaptionsTable+`.state = 'read' THEN 'read' ELSE 'failed' END`,
		id, item, clip(why, 500), f, now.Add(backoff(f)), now)
	return err
}

// FirstBackoff is how long an original whose read failed waits before the
// backfill hands it out again, doubled after each failure in a row,
// MaxBackoff at most; one given back unread waits ReleaseWait.
const (
	FirstBackoff = time.Hour
	MaxBackoff   = 7 * 24 * time.Hour
	ReleaseWait  = 15 * time.Minute
)

// backoff is the wait after the failures-th failure in a row.
func backoff(failures int32) time.Duration {
	d := FirstBackoff
	for i := int32(1); i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// clip is s cut to at most n bytes, at a character's start.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// recordReadable is library.RecordReadable of the record folder dir, ""
// where the catalog names none (no record yet).
func recordReadable(dir *string) string {
	if dir == nil {
		return ""
	}
	return library.RecordReadable(*dir)
}
