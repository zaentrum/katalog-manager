// Package chapters keeps a title's chapters, and what reading them found
// (migration 055, README "Chapters").
//
// A title's chapters live in the catalog (com_nalet_katalog_itemchapters),
// not in its package, which carries none: they are read from its original,
// and a deletion wave keeps the original until they are in the catalog. Two
// readers read them. The analyzer reads the original's header (ffprobe's
// chapters, nothing decoded), in its pass of a new title and in the backfill
// (Service.Claim), and reports each read (PUT /api/chapters/items/{id},
// Record). The catalog reads the original's source record
// (sources/<id>/ffprobe.json, the library's probe of the file, its chapters
// among it), first, in the backfill, and wherever a read of the header found
// no chapter. The catalog keeps the last read of each title, of which
// original it was and what it found, in the transaction that writes the
// chapters. A title misses its chapters (the view
// com_nalet_katalog_chaptersmissing, NeedsOriginal) until a read of its
// current original settles it: chapters written, a none its record agrees
// with, an unreadable an admin accepted.
package chapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The states of a title's read.
const (
	StatePending    = "pending"    // the backfill handed it out; nothing reported yet
	StateRead       = "read"       // chapters found and written
	StateNone       = "none"       // the original holds no chapter with a length (Confirmed: its record agrees)
	StateFailed     = "failed"     // the last read failed (Reason); read again from NextAttemptAt
	StateUnreadable = "unreadable" // MaxFailures header reads failed on the file: settled only once an admin accepts it
	StateHeld       = "held"       // what was read and the chapters in the catalog disagree: an admin decides
)

// Where a read read the chapters from.
const (
	ReadFromHeader  = "header"  // the analyzer's read of the original's header
	ReadFromRecord  = "record"  // the catalog's read of the original's source record
	ReadFromCatalog = "catalog" // the chapters in the catalog, an admin's decision
)

// MaxFailures is how many header reads in a row that ran on the original
// and could not read its chapters make a title unreadable. Each counts once
// a backoff: the first wait is FirstBackoff, doubled after each, MaxBackoff at
// most, so the fifth comes at least 15 hours after the first, whoever reports
// them. A failure of the storage or the worker (Transient) counts for nothing
// but the backoff. A title handed out and given back unread waits
// ReleaseWait; one a read settled is not handed out again before
// SettledWait (its view may hold it still for another of its originals).
const (
	MaxFailures  = 5
	FirstBackoff = time.Hour
	MaxBackoff   = 24 * time.Hour
	ReleaseWait  = 15 * time.Minute
	SettledWait  = 24 * time.Hour
)

// TitleMax is the most characters a chapter's title keeps (its column).
const TitleMax = 120

// ErrNotReady says migration 055 is not in place.
var ErrNotReady = errors.New("the chapter reads' migration (db/migrations/055_chapter_reads.sql) is not applied")

// ErrNoTitle says there is no such item.
var ErrNoTitle = errors.New("no such title")

// BadReport is a report that can't be taken: what is wrong with it.
type BadReport struct{ Why string }

func (e *BadReport) Error() string { return e.Why }

// Chapter is a chapter as the catalog keeps it: its range in ms on the
// title's timeline, its title (none when the file gives none) and its place
// among the title's chapters.
type Chapter struct {
	StartMs int64   `json:"startMs"`
	EndMs   int64   `json:"endMs"`
	Title   *string `json:"title"`
	Ordinal int     `json:"ordinal"`
}

// CleanTitle is a chapter's title as the catalog keeps it: without NUL (no
// text column holds one) and other control characters, cut to TitleMax
// characters; nil when nothing is left.
func CleanTitle(t *string) *string {
	if t == nil {
		return nil
	}
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			if r == '\t' || r == '\n' || r == '\r' {
				return ' '
			}
			return -1
		}
		return r
	}, strings.ToValidUTF8(*t, ""))
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > TitleMax {
		s = strings.TrimSpace(string([]rune(s)[:TitleMax]))
	}
	if s == "" {
		return nil
	}
	return &s
}

// Replace makes chs the chapters of the item itemID, in tx: its chapters are
// deleted, and chs written, each title as CleanTitle keeps it.
func Replace(ctx context.Context, tx pgx.Tx, itemID string, chs []Chapter) error {
	if _, err := tx.Exec(ctx, `DELETE FROM com_nalet_katalog_itemchapters WHERE item_id = $1`, itemID); err != nil {
		return err
	}
	if len(chs) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, c := range chs {
		batch.Queue(`INSERT INTO com_nalet_katalog_itemchapters
			(id, createdat, modifiedat, item_id, startms, endms, title, ordinal)
			VALUES (gen_random_uuid()::varchar, now(), now(), $1, $2, $3, $4, $5)`,
			itemID, c.StartMs, c.EndMs, CleanTitle(c.Title), c.Ordinal)
	}
	br := tx.SendBatch(ctx, batch)
	for range chs {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return err
		}
	}
	return br.Close()
}

// Of reads the chapters of the item itemID, in their order.
func Of(ctx context.Context, q library.Querier, itemID string) ([]Chapter, error) {
	rows, err := q.Query(ctx, `SELECT startms, endms, title, COALESCE(ordinal, 0)
		FROM com_nalet_katalog_itemchapters WHERE item_id = $1 ORDER BY ordinal NULLS LAST, startms, id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Chapter{}
	for rows.Next() {
		var c Chapter
		var ordinal int32
		if err := rows.Scan(&c.StartMs, &c.EndMs, &c.Title, &ordinal); err != nil {
			return nil, err
		}
		c.Ordinal = int(ordinal)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Read is a title's last read as the catalog keeps it.
type Read struct {
	ItemID, State string
	// ReadFrom is the reader (ReadFromHeader, ReadFromRecord); Confirmed says a
	// none's record agrees with it.
	ReadFrom  *string
	Confirmed bool
	// SourceID is the original read, Path the file and FileSize its size;
	// RowsSourceID the original the chapters in the catalog were read of
	// (nil: of none known); Replaced the chapters a read replaced last.
	SourceID, Path *string
	FileSize       *int64
	RowsSourceID   *string
	Replaced       json.RawMessage
	// Found are the chapter marks found, Written the chapters written of them.
	Found, Written *int32
	Probe, Reason  *string
	Seconds        *float64
	ReadAt         *time.Time
	// RecordSourceID is the source whose record the catalog read last,
	// RecordAt when, RecordNote why it could not settle the title.
	RecordSourceID *string
	RecordAt       *time.Time
	RecordNote     *string
	ClaimedAt      *time.Time
	ClaimedBy      *string
	ClaimToken     *string
	LastClaimedAt  *time.Time
	LapsedToken    *string
	LapsedClaimed  *time.Time
	Attempts       int32
	Failures       int32
	Transients     int32
	NextAttemptAt  *time.Time
	AcceptedAt     *time.Time
	AcceptedBy     *string
	ModifiedAt     time.Time
}

// of reports whether the read is of the original sourceID.
func (r *Read) of(sourceID *string) bool {
	return r != nil && r.SourceID != nil && sourceID != nil && *r.SourceID == *sourceID
}

// settles reports whether the read settles the title's original sourceID,
// whose title has chapters in the catalog or not, as the view does: while it
// is not held of that original, chapters read of that original in the
// catalog, a confirmed none of it with none in the catalog, an unreadable of
// it an admin accepted.
func (r *Read) settles(sourceID *string, chapters bool) bool {
	if r == nil || sourceID == nil || r.heldOf(sourceID) {
		return false
	}
	if chapters && r.RowsSourceID != nil && *r.RowsSourceID == *sourceID {
		return true
	}
	if !r.of(sourceID) {
		return false
	}
	switch r.State {
	case StateNone:
		return r.Confirmed && !chapters
	case StateUnreadable:
		return r.AcceptedAt != nil
	}
	return false
}

// heldOf reports whether the read holds the title's original sourceID for an
// admin: a read of another original holds none of this one.
func (r *Read) heldOf(sourceID *string) bool {
	return r != nil && r.State == StateHeld && r.of(sourceID)
}

// The ways a read of an original takes the chapters in the catalog.
const (
	takeWrite = iota // its chapters in place of the catalog's, those kept to undo
	takeKeep         // the catalog's are its chapters: kept, now of that original
	takeNone         // it holds none: the catalog's, of another original, go (kept to undo)
	takeStay         // it found none, and the catalog's, read before of that original, stay
	takeHold         // it and the catalog's, of no original known, disagree: an admin decides
)

// take is how a read of the original sourceID that found got (empty: none)
// takes the chapters in the catalog, have, read of rowsSource (nil: of none
// known), and why it holds the title when it does. A read's chapters replace
// those of another original, and those of no original known when they are as
// many or more; fewer, or none, than those of no original known (they were
// written before the reads were kept) hold the title for an admin, as fewer
// than those read before of the same original do; a none takes those of
// another original away; chapters that are the read's are kept as they are.
func take(have, got []Chapter, rowsSource, sourceID *string) (int, string) {
	other := rowsSource != nil && (sourceID == nil || *rowsSource != *sourceID)
	earlier := "an earlier read"
	if rowsSource != nil && !other {
		earlier = "an earlier read of this original"
	}
	switch {
	case len(got) > 0 && len(have) > 0 && same(have, got):
		return takeKeep, ""
	case len(got) > 0 && (len(have) == 0 || other || len(got) >= len(have)):
		return takeWrite, ""
	case len(got) > 0:
		return takeHold, fmt.Sprintf("it holds %d chapters, the catalog %d of %s: an admin decides", len(got),
			len(have), earlier)
	case len(have) == 0 || other:
		return takeNone, ""
	case rowsSource != nil:
		return takeStay, ""
	}
	return takeHold, fmt.Sprintf("it holds no chapter with a length, the catalog %d of %s: an admin decides",
		len(have), earlier)
}

// put makes got the chapters of the item itemID in tx, in place of have,
// which are kept to undo (the read row's replaced: when, by which reader, of
// which original they were read, rowsSource, and they) when they are
// chapters that are not got; chapters in the catalog that are got are kept
// as they are. The chapters in the catalog are then of the original sourceID
// (of none, when got is none).
func put(ctx context.Context, tx pgx.Tx, itemID string, have, got []Chapter, rowsSource, sourceID *string, by string,
	now time.Time) error {
	switch {
	case len(got) > 0 && same(have, got):
	default:
		if len(have) > 0 {
			undo, err := json.Marshal(map[string]any{"at": now.UTC(), "by": by, "sourceId": rowsSource, "chapters": have})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET replaced = $2 WHERE item_id = $1`,
				itemID, undo); err != nil {
				return err
			}
		}
		if err := Replace(ctx, tx, itemID, got); err != nil {
			return err
		}
	}
	var rows *string
	if len(got) > 0 {
		rows = sourceID
	}
	_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET rowssourceid = $2 WHERE item_id = $1`, itemID, rows)
	return err
}

// replacedNote is note, saying that the n chapters the catalog held were
// replaced and kept to undo.
func replacedNote(note string, n int) string {
	s := fmt.Sprintf("the %d chapters it had before replaced, kept to undo", n)
	if note == "" {
		return s
	}
	return note + "; " + s
}

const readColumns = `r.item_id, r.state, r.readfrom, r.confirmed, r.sourceid, r.rowssourceid, r.replaced, r.path,
	r.filesize, r.found, r.written,
	r.probe, r.reason, r.seconds, r.readat, r.recordsourceid, r.recordat, r.recordnote, r.claimedat, r.claimedby,
	r.claimtoken, r.lastclaimedat, r.lapsedtoken, r.lapsedclaimedat, r.attempts, r.failures, r.transients,
	r.nextattemptat, r.acceptedat, r.acceptedby, r.modifiedat`

func scanRead(row pgx.Row) (*Read, error) {
	r := &Read{}
	var secs *float32
	err := row.Scan(&r.ItemID, &r.State, &r.ReadFrom, &r.Confirmed, &r.SourceID, &r.RowsSourceID, &r.Replaced, &r.Path,
		&r.FileSize, &r.Found,
		&r.Written, &r.Probe, &r.Reason, &secs, &r.ReadAt, &r.RecordSourceID, &r.RecordAt, &r.RecordNote,
		&r.ClaimedAt, &r.ClaimedBy, &r.ClaimToken, &r.LastClaimedAt, &r.LapsedToken, &r.LapsedClaimed,
		&r.Attempts, &r.Failures, &r.Transients, &r.NextAttemptAt, &r.AcceptedAt, &r.AcceptedBy, &r.ModifiedAt)
	if err != nil {
		return nil, err
	}
	if secs != nil {
		v := float64(*secs)
		r.Seconds = &v
	}
	return r, nil
}

// ReadOf reads the last read of the item itemID; nil when it has none.
func ReadOf(ctx context.Context, q library.Querier, itemID string) (*Read, error) {
	r, err := scanRead(q.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.ChapterReadsTable+` r
		WHERE r.item_id = $1`, itemID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// NeedsOriginal reports whether the item itemID still needs its original
// sourceID ("" for its current one) for its chapters: it is among
// com_nalet_katalog_chaptersmissing, of that original. A deletion wave keeps
// the original while it does. A catalog without migration 055 is
// ErrNotReady, and keeps it.
func NeedsOriginal(ctx context.Context, q library.Querier, itemID, sourceID string) (bool, error) {
	var ready bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, store.ChaptersMissingView).Scan(&ready); err != nil {
		return true, err
	}
	if !ready {
		return true, ErrNotReady
	}
	var missing bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+store.ChaptersMissingView+` m
		WHERE m.item_id = $1 AND ($2 = '' OR m.sourceid = $2))`, itemID, sourceID).Scan(&missing)
	return missing, err
}

// ReadReport is what the analyzer says of a read of a title's original's
// header: the file it read (Path; FileSize its size) and the original it is
// (SourceID, the backfill's claim's), the chapter marks the header held
// (Found), why the read failed (Error: nothing was found, and the title's
// chapters stay as they are; Transient: the storage or the worker failed, no
// file to read, no answer in time, an error of the storage, which says
// nothing of the file), what was left out (Note), what read it (Probe) and
// how long it took (Seconds).
type ReadReport struct {
	Path      string   `json:"path"`
	SourceID  string   `json:"sourceId"`
	FileSize  *int64   `json:"fileSize"`
	Found     *int32   `json:"found"`
	Error     string   `json:"error"`
	Transient bool     `json:"transient"`
	Note      string   `json:"note"`
	Probe     string   `json:"probe"`
	Seconds   *float64 `json:"seconds"`
}

// Report is a read the analyzer reports: what it read (Read), the chapters it
// found (Chapters: the whole set; nil for a read that failed), and the token
// of the backfill's claim it read for (none for the analyzer's pass of a new
// title). Released gives a claim back unread; nothing else is read of it.
type Report struct {
	Read     ReadReport
	Chapters []Chapter
	Token    string
	Released bool
}

// state is the state the report would leave its title in, or why it can't
// be taken.
func (r Report) state() (string, error) {
	if r.Released {
		return "", nil
	}
	if strings.TrimSpace(r.Read.Path) == "" {
		return "", &BadReport{"a read names the file it read (read.path)"}
	}
	if r.Read.Found != nil && *r.Read.Found < 0 {
		return "", &BadReport{"read.found is the chapter marks the header held, 0 or more"}
	}
	if strings.TrimSpace(r.Read.Error) != "" {
		if r.Chapters != nil {
			return "", &BadReport{"a read that failed (read.error) found no chapters to write"}
		}
		return StateFailed, nil
	}
	if r.Chapters == nil {
		return "", &BadReport{"a read names the chapters it found (chapters, [] for none), or why it failed (read.error)"}
	}
	for i, c := range r.Chapters {
		if c.StartMs < 0 || c.EndMs <= c.StartMs {
			return "", &BadReport{"startMs/endMs invalid (need 0 <= start < end) of chapter " + strconv.Itoa(i+1)}
		}
	}
	if len(r.Chapters) == 0 {
		return StateNone, nil
	}
	return StateRead, nil
}

// failure is what the failures-th failed header read in a row, why, leaves
// its title in: failed, read again after its backoff; the MaxFailures-th
// makes it unreadable (not handed out again), saying how many failed.
func failure(failures int32, why string, now time.Time) (state string, next *time.Time, reason string) {
	at := now.Add(backoff(failures))
	if failures >= MaxFailures {
		return StateUnreadable, &at, clip(strconv.Itoa(int(failures))+" reads in a row failed, the last: "+why, 500)
	}
	return StateFailed, &at, clip(why, 500)
}

// backoff is the wait after the n-th failure in a row.
func backoff(n int32) time.Duration {
	d := FirstBackoff
	for i := int32(1); i < n && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// claimSQL is what the claim fields become as a report is kept: the claim it
// owns ($2) ends, and its lapsed one ($3) is forgotten.
const claimSQL = `claimedat = CASE WHEN $2 THEN NULL ELSE claimedat END,
	claimedby = CASE WHEN $2 THEN NULL ELSE claimedby END,
	claimtoken = CASE WHEN $2 THEN NULL ELSE claimtoken END,
	lapsedtoken = CASE WHEN $3 THEN NULL ELSE lapsedtoken END,
	lapsedclaimedat = CASE WHEN $3 THEN NULL ELSE lapsedclaimedat END`

// Record keeps a read of the header of the title itemID's original that the
// analyzer reported, and answers the title's read as it is now.
//
// What a read is of is the original its claim named (read.sourceId, one of
// the item's), else the original of its file (in a folder of one of the
// item's versions, the first original that version was made of; else the
// source of the item's primary asset of that path).
//
// A read that found chapters takes the chapters in the catalog as take says:
// they replace those of another original, and those of no original known
// when they are as many or more (the replaced ones kept to undo); fewer than
// those hold the title for an admin; chapters that are the read's are kept
// as they are, now of that original. A late report never replaces a newer
// read. A read that found none never deletes chapters but a replaced
// original's: one of an original whose chapters were read before, or whose
// none was confirmed, leaves them as they are; otherwise it is held against
// the original's source record (the library's probe, p): where the record
// holds chapters they are taken so, read from it; where it holds none with a
// length, or the compliance check's record of that original says it holds
// none at all, the none is confirmed (a replaced original's chapters going
// with it, those of no original known holding the title); where the
// record's marks lie past the file's end and do not fit it an hour earlier,
// the title is held; where the record can't be read but the compliance
// check's says chapters, the read counts as a failure; where nothing can say,
// the none is kept unconfirmed, settling nothing, and its record is read
// again.
//
// A read that failed leaves the chapters as they are. One the storage or the
// worker failed (read.transient) counts for nothing but its backoff. One
// that ran on the file counts a failure, once its backoff: the MaxFailures-th
// in a row makes the title unreadable, which settles nothing until an admin
// accepts it (Accept). A failure of a title a read of that original settled
// leaves what was read.
//
// Released gives the title's claim back unread; it waits ReleaseWait. A
// report ends the claim it names (its token) and only that one; one without a
// token (the analyzer's pass) leaves any claim as it is; one whose token names
// a claim that ended already, whose report came or that was taken back twice,
// changes nothing. A series or a season is no title read (400); an unknown
// one is ErrNoTitle.
func Record(ctx context.Context, st *store.Store, p library.Paths, itemID string, r Report, now time.Time) (*Read, error) {
	outcome, err := r.state()
	if err != nil {
		return nil, err
	}
	pool := st.Pool()
	var typ string
	if err := pool.QueryRow(ctx, `SELECT type FROM com_nalet_katalog_items WHERE id = $1`, itemID).
		Scan(&typ); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoTitle
	} else if err != nil {
		return nil, err
	}
	if typ != "movie" && typ != "episode" {
		return nil, &BadReport{"a " + typ + " has no file of its own: its movies or episodes are read"}
	}
	var sourceID *string
	var size *int64
	if !r.Released {
		if sourceID, size, err = sourceOf(ctx, pool, itemID, r.Read); err != nil {
			return nil, err
		}
		if r.Read.FileSize != nil && *r.Read.FileSize > 0 {
			size = r.Read.FileSize
		}
	}
	// A read that found none is held against the original's record, read
	// before the transaction (small files of the library).
	var rec *RecordRead
	var says *bool
	if outcome == StateNone && sourceID != nil {
		x := RecordOf(ctx, pool, p, itemID, *sourceID)
		rec = &x
		if !x.Says() && x.Held == "" {
			if says, err = recordSays(ctx, pool, itemID, *sourceID); err != nil {
				return nil, err
			}
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `INSERT INTO `+store.ChapterReadsTable+` (item_id, state, createdat, modifiedat)
		VALUES ($1, 'pending', $2, $2) ON CONFLICT (item_id) DO NOTHING`, itemID, now); err != nil {
		return nil, err
	}
	kept, err := scanRead(tx.QueryRow(ctx, `SELECT `+readColumns+` FROM `+store.ChapterReadsTable+` r
		WHERE r.item_id = $1 FOR UPDATE OF r`, itemID))
	if err != nil {
		return nil, err
	}
	have, err := Of(ctx, tx, itemID)
	if err != nil {
		return nil, err
	}
	chapters := len(have) > 0
	token := strings.TrimSpace(r.Token)
	// owns: the report is of the claim the title is under; lapsed: of the
	// claim whose lease ended last. claimed: when its claim was handed out.
	owns := token != "" && kept.ClaimedAt != nil && kept.ClaimToken != nil && token == *kept.ClaimToken
	lapsed := token != "" && kept.LapsedToken != nil && token == *kept.LapsedToken
	var claimed *time.Time
	switch {
	case owns:
		claimed = kept.ClaimedAt
	case lapsed:
		claimed = kept.LapsedClaimed
	}
	// stale: of a claim that ended already (its report came, or it was taken
	// back since its lease ended): it changes nothing
	stale := token != "" && !owns && !lapsed
	// newer: a read was reported since this one's claim was handed out
	newer := claimed != nil && kept.ReadAt != nil && kept.ReadAt.After(*claimed) &&
		(kept.State == StateRead || kept.State == StateNone || kept.State == StateHeld)
	path, probe := clip(r.Read.Path, 2048), clip(strings.TrimSpace(r.Read.Probe), 200)
	found := int32(0)
	if r.Read.Found != nil {
		found = *r.Read.Found
	}

	// only: the claim ends, and the reason says why (when why is set)
	only := func(why string) error {
		_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`,
			reason = COALESCE(NULLIF($4, ''), reason), modifiedat = $5 WHERE item_id = $1`,
			itemID, owns, lapsed, clip(why, 500), now)
		return err
	}
	// failed counts a header read that ran on the file and could not read
	// its chapters, once its backoff
	failed := func(why string) error {
		if kept.Failures > 0 && (kept.State == StateFailed || kept.State == StateUnreadable) &&
			kept.NextAttemptAt != nil && now.Before(*kept.NextAttemptAt) {
			// within the last failure's backoff (the step's quick retries of
			// a pass): said, counted once
			_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, reason = $4, readat = $5,
				modifiedat = $5 WHERE item_id = $1`, itemID, owns, lapsed, clip(why, 500), now)
			return err
		}
		f := kept.Failures + 1
		state, next, reason := failure(f, why, now)
		_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = $4, readfrom = 'header',
			confirmed = false, sourceid = $5, path = $6, filesize = $7, found = NULL, written = NULL,
			probe = NULLIF($8, ''), seconds = $9, reason = $10, readat = $11, failures = $12, transients = 0,
			nextattemptat = $13, modifiedat = $11 WHERE item_id = $1`, itemID, owns, lapsed, state, sourceID, path, size,
			probe, seconds(r.Read.Seconds), reason, now, f, next)
		return err
	}
	// kept keeps what a read found on the read row: state, reader, confirmed,
	// the file it read and what it found; recorded, when the record was read
	keep := func(state, from string, confirmed bool, readPath, readProbe string, written int32, secs *float32,
		reason string, recorded bool) error {
		next := now.Add(SettledWait)
		if state == StateNone && !confirmed {
			next = now.Add(MaxBackoff)
		}
		var recordSource *string
		if recorded {
			recordSource = sourceID
		}
		_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = $4, readfrom = $5,
			confirmed = $6, sourceid = $7, path = $8, filesize = $9, found = $10, written = $11, probe = NULLIF($12, ''),
			seconds = $13, reason = NULLIF($14, ''), readat = $15, failures = 0, transients = 0, nextattemptat = $16,
			recordsourceid = CASE WHEN $17::varchar IS NOT NULL THEN $17::varchar
			                      WHEN $4::varchar = 'none' AND NOT $6 THEN NULL ELSE recordsourceid END,
			recordat = CASE WHEN $17::varchar IS NOT NULL THEN $15 ELSE recordat END,
			recordnote = CASE WHEN $17::varchar IS NOT NULL THEN NULL ELSE recordnote END, modifiedat = $15
			WHERE item_id = $1`, itemID, owns, lapsed, state, from, confirmed, sourceID, clip(readPath, 2048), size,
			max(found, written), written, clip(readProbe, 200), secs, clip(reason, 500), now, next, recordSource)
		return err
	}
	// hold holds the title for an admin: what was read disagrees with the
	// chapters in the catalog, or the record's marks lie past the file's end
	hold := func(why string, recorded bool) error {
		var recordSource *string
		if recorded {
			recordSource = sourceID
		}
		_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = 'held', sourceid = $4,
			path = $5, found = $6, written = NULL, reason = $7, readat = $8, failures = 0, transients = 0,
			nextattemptat = NULL, recordsourceid = COALESCE($9, recordsourceid),
			recordat = CASE WHEN $9::varchar IS NOT NULL THEN $8 ELSE recordat END, modifiedat = $8
			WHERE item_id = $1`, itemID, owns, lapsed, sourceID, path, found, clip(why, 500), now, recordSource)
		return err
	}
	// settle takes what a read of the original found, got from the reader
	// from (its none confirmed or not), as take says
	settle := func(got []Chapter, from string, confirmed bool, readPath, readProbe string, secs *float32,
		note string, recorded bool) error {
		how, why := take(have, got, kept.RowsSourceID, sourceID)
		switch {
		case len(got) == 0 && !confirmed && chapters:
			// a none nothing confirms takes no chapters away, and holds nothing:
			// it settles nothing, and its record is read again
		case how == takeHold:
			return hold(why, recorded)
		case how == takeStay:
			return only("a read found no chapter; the chapters read before of this original stay")
		case how == takeKeep:
			if _, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET rowssourceid = $2 WHERE item_id = $1`,
				itemID, sourceID); err != nil {
				return err
			}
			note = "the chapters in the catalog are those it read"
		default:
			if err := put(ctx, tx, itemID, have, got, kept.RowsSourceID, sourceID, from, now); err != nil {
				return err
			}
			if len(have) > 0 {
				note = replacedNote(note, len(have))
			}
		}
		state := StateRead
		if len(got) == 0 {
			state = StateNone
		}
		return keep(state, from, confirmed, readPath, readProbe, int32(len(got)), secs, note, recorded)
	}

	switch {
	case stale:
	case outcome == "": // released
		if owns {
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`,
				nextattemptat = GREATEST(nextattemptat, $4), modifiedat = $5 WHERE item_id = $1`,
				itemID, owns, lapsed, now.Add(ReleaseWait), now)
		}
	case outcome == StateFailed:
		why := strings.TrimSpace(r.Read.Error)
		switch {
		case lapsed:
			// its lease ended, its backoff counted then: this only says why
			err = only(why)
		case newer || kept.settles(sourceID, chapters) || kept.heldOf(sourceID):
			// a read settled it, or holds it for an admin: that stands
			err = only("")
		case r.Read.Transient:
			t := kept.Transients + 1
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`,
				state = CASE WHEN state = 'pending' THEN 'failed' ELSE state END, transients = $4,
				nextattemptat = $5, reason = $6, readat = $7, modifiedat = $7 WHERE item_id = $1`,
				itemID, owns, lapsed, t, now.Add(backoff(t)), clip(why, 500), now)
		default:
			err = failed(why)
		}
	case newer:
		// a read reported since this one was handed out stands
		err = only("")
	case outcome == StateRead:
		err = settle(r.Chapters, ReadFromHeader, false, path, probe, seconds(r.Read.Seconds),
			strings.TrimSpace(r.Read.Note), false)
	default: // a read of the header that found none
		switch {
		case chapters && kept.RowsSourceID != nil && sourceID != nil && *kept.RowsSourceID == *sourceID:
			// the chapters read before of this original stay
			err = only("a read found no chapter; the chapters read before of this original stay")
		case kept.State == StateNone && kept.Confirmed && kept.of(sourceID) && !chapters:
			// a none of this original its record confirmed stands, whether its
			// record can be read now or not
			err = only("")
		case rec != nil && rec.Held != "":
			err = hold(rec.Held, true)
		case rec != nil && rec.Says() && len(rec.Chapters) > 0:
			// its record holds the chapters the header read did not find
			err = settle(rec.Chapters, ReadFromRecord, false, rec.Path, "source record", nil,
				"the header read found no chapter ("+strconv.Itoa(int(found))+" marks); its source record holds "+
					strconv.Itoa(len(rec.Chapters))+": read from it", true)
		case rec != nil && rec.Says():
			reason := strings.TrimSpace(r.Read.Note)
			if reason == "" {
				reason = rec.Note
			}
			err = settle(nil, ReadFromHeader, true, path, probe, seconds(r.Read.Seconds), reason, true)
		case says != nil && !*says:
			err = settle(nil, ReadFromHeader, true, path, probe, seconds(r.Read.Seconds),
				"its source record says it holds no chapter", true)
		case says != nil && *says:
			err = failed("the header read found no chapter, and the source's record says it holds some (" +
				recordWhy(rec) + ")")
		default:
			// nothing can say: settles nothing, and its record is read again
			err = settle(nil, ReadFromHeader, false, path, probe, seconds(r.Read.Seconds),
				"nothing confirms that the original holds no chapter: "+recordWhy(rec), false)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ReadOf(ctx, pool, itemID)
}

// recordWhy is why a record could not say a title's chapters.
func recordWhy(rec *RecordRead) string {
	switch {
	case rec == nil:
		return "its original is not known"
	case rec.Err != nil:
		return "its source record could not be read: " + rec.Err.Error()
	case rec.Why != "":
		return rec.Why
	}
	return ""
}

// recordSays is what the compliance check's record of the item itemID's
// source sourceID says: whether the original holds chapters; nil when it
// has no record of that source, or the record does not say.
func recordSays(ctx context.Context, q library.Querier, itemID, sourceID string) (*bool, error) {
	var has bool
	err := q.QueryRow(ctx, `SELECT (records->'srcEssence'->>'chapters')::boolean FROM com_nalet_katalog_compliance
		WHERE item_id = $1 AND substring(recordkey FROM 's:([0-9a-f-]{36})@') = $2
		  AND jsonb_typeof(records->'srcEssence'->'chapters') = 'boolean'`, itemID, sourceID).Scan(&has)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &has, nil
}

// sourceOf is the original a read is of: the one its claim named, while it
// is one of the item's, else the original of its file (fileOf); with its size
// where the catalog knows it.
func sourceOf(ctx context.Context, q library.Querier, itemID string, rr ReadReport) (*string, *int64, error) {
	if id := strings.TrimSpace(rr.SourceID); id != "" {
		var size *int64
		err := q.QueryRow(ctx, `SELECT sizebytes FROM com_nalet_katalog_itemsources WHERE id = $1 AND item_id = $2`,
			id, itemID).Scan(&size)
		if err == nil {
			return &id, size, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
	}
	return fileOf(ctx, q, itemID, rr.Path)
}

// fileOf says what the file at path of the item itemID is: in a folder of one
// of its versions, the first original that version was made of; else, as
// its primary asset's path, that asset's original (none in the legacy
// layout); with the original's size where the catalog knows it.
func fileOf(ctx context.Context, q library.Querier, itemID, path string) (sourceID *string, size *int64, err error) {
	var sources []string
	err = q.QueryRow(ctx, `SELECT sourceids FROM com_nalet_katalog_itemversions
		WHERE item_id = $1 AND dir IS NOT NULL AND dir <> '' AND starts_with($2, rtrim(dir, '/') || '/')
		ORDER BY state = 'complete' DESC, id LIMIT 1`, itemID, path).Scan(&sources)
	switch {
	case err == nil:
		if len(sources) > 0 {
			sourceID = &sources[0]
		}
	case errors.Is(err, pgx.ErrNoRows):
		var assetSize *int64
		err = q.QueryRow(ctx, `SELECT sourceid, sizebytes FROM com_nalet_katalog_playbackassets
			WHERE item_id = $1 AND isprimary AND COALESCE(kind, 'primary') = 'primary' AND path = $2
			ORDER BY id LIMIT 1`, itemID, path).Scan(&sourceID, &assetSize)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
		size = assetSize
	default:
		return nil, nil, err
	}
	if sourceID != nil {
		var s *int64
		if err := q.QueryRow(ctx, `SELECT sizebytes FROM com_nalet_katalog_itemsources WHERE id = $1`, *sourceID).
			Scan(&s); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		if s != nil && *s > 0 {
			size = s
		}
	}
	return sourceID, size, nil
}

func seconds(v *float64) *float32 {
	if v == nil || *v < 0 {
		return nil
	}
	f := float32(*v)
	return &f
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
