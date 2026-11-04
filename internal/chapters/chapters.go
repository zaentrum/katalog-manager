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
	"errors"
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
)

// Where a read read the chapters from.
const (
	ReadFromHeader = "header" // the analyzer's read of the original's header
	ReadFromRecord = "record" // the catalog's read of the original's source record
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
	// SourceID is the original read, Path the file and FileSize its size.
	SourceID, Path *string
	FileSize       *int64
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
// whose title has chapters in the catalog or not: chapters written, a
// confirmed none, an unreadable an admin accepted, of that original.
func (r *Read) settles(sourceID *string, chapters bool) bool {
	if !r.of(sourceID) {
		return false
	}
	switch r.State {
	case StateRead:
		return chapters
	case StateNone:
		return r.Confirmed
	case StateUnreadable:
		return r.AcceptedAt != nil
	}
	return false
}

const readColumns = `r.item_id, r.state, r.readfrom, r.confirmed, r.sourceid, r.path, r.filesize, r.found, r.written,
	r.probe, r.reason, r.seconds, r.readat, r.recordsourceid, r.recordat, r.recordnote, r.claimedat, r.claimedby,
	r.claimtoken, r.lastclaimedat, r.lapsedtoken, r.lapsedclaimedat, r.attempts, r.failures, r.transients,
	r.nextattemptat, r.acceptedat, r.acceptedby, r.modifiedat`

func scanRead(row pgx.Row) (*Read, error) {
	r := &Read{}
	var secs *float32
	err := row.Scan(&r.ItemID, &r.State, &r.ReadFrom, &r.Confirmed, &r.SourceID, &r.Path, &r.FileSize, &r.Found,
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
// A read that found chapters writes them, in place of the title's, unless a
// read reported since its claim was handed out stands (a late report never
// replaces a newer one). A read that found none never deletes chapters: one
// of an original whose chapters were read before leaves them, one of an
// original whose none was confirmed leaves that none; otherwise it is
// held against the original's source record (the library's probe, p): where
// the record holds chapters they are written, read from it; where it holds
// none with a length, or the compliance check's record of that source says
// it holds none at all, the none is confirmed and settles the title; where
// the record says chapters (the compliance check's word, its probe unread)
// the read counts as a failure; where nothing can say, the none is kept
// unconfirmed, settling nothing, and its record is read again.
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
	// before the transaction (a small file of the library).
	var rec *RecordRead
	var says *bool
	if outcome == StateNone && sourceID != nil {
		x := RecordOf(ctx, pool, p, itemID, *sourceID)
		rec = &x
		if !x.Says() {
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
	var chapters bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_itemchapters WHERE item_id = $1)`,
		itemID).Scan(&chapters); err != nil {
		return nil, err
	}
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
		(kept.State == StateRead || kept.State == StateNone)
	path, probe := clip(r.Read.Path, 2048), clip(strings.TrimSpace(r.Read.Probe), 200)

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
	// none keeps a read that found no chapter with a length, confirmed or
	// not, the title's chapters as they are
	none := func(confirmed bool, reason, recordNote string, recorded *string) error {
		found := int32(0)
		if r.Read.Found != nil {
			found = *r.Read.Found
		}
		next := now.Add(SettledWait)
		if !confirmed {
			next = now.Add(MaxBackoff)
		}
		var recordAt *time.Time
		if recorded != nil {
			recordAt = &now
		}
		_, err := tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = 'none', readfrom = 'header',
			confirmed = $4, sourceid = $5, path = $6, filesize = $7, found = $8, written = 0, probe = NULLIF($9, ''),
			seconds = $10, reason = NULLIF($11, ''), readat = $12, failures = 0, transients = 0, nextattemptat = $13,
			recordsourceid = $14, recordat = COALESCE($15, recordat), recordnote = NULLIF($16, ''), modifiedat = $12
			WHERE item_id = $1`, itemID, owns, lapsed, confirmed, sourceID, path, size, found, probe,
			seconds(r.Read.Seconds), clip(reason, 500), now, next, recorded, recordAt, clip(recordNote, 500))
		return err
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
		case newer || kept.settles(sourceID, chapters):
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
		if err = Replace(ctx, tx, itemID, r.Chapters); err != nil {
			return nil, err
		}
		written := int32(len(r.Chapters))
		found := written
		if r.Read.Found != nil && *r.Read.Found > written {
			found = *r.Read.Found
		}
		_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = 'read', readfrom = 'header',
			confirmed = false, sourceid = $4, path = $5, filesize = $6, found = $7, written = $8, probe = NULLIF($9, ''),
			seconds = $10, reason = NULLIF($11, ''), readat = $12, failures = 0, transients = 0, nextattemptat = $13,
			modifiedat = $12 WHERE item_id = $1`, itemID, owns, lapsed, sourceID, path, size, found, written, probe,
			seconds(r.Read.Seconds), clip(strings.TrimSpace(r.Read.Note), 500), now, now.Add(SettledWait))
	default: // a read that found none
		switch {
		case kept.State == StateRead && kept.of(sourceID) && chapters:
			// the chapters read before of this original stay
			err = only("a read of the header found no chapter; the chapters read before of this original stay")
		case kept.State == StateNone && kept.Confirmed && kept.of(sourceID):
			// a none of this original its record confirmed stands, whether its
			// record can be read now or not
			err = only("")
		case rec != nil && rec.Says() && len(rec.Chapters) > 0:
			// its record holds the chapters the header read did not find
			if err = Replace(ctx, tx, itemID, rec.Chapters); err != nil {
				return nil, err
			}
			found := int32(0)
			if r.Read.Found != nil {
				found = *r.Read.Found
			}
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claimSQL+`, state = 'read', readfrom = 'record',
				confirmed = false, sourceid = $4, path = $5, filesize = $6, found = $7, written = $8, probe = 'source record',
				seconds = NULL, reason = $9, readat = $10, failures = 0, transients = 0, nextattemptat = $11,
				recordsourceid = $4, recordat = $10, recordnote = NULL, modifiedat = $10 WHERE item_id = $1`,
				itemID, owns, lapsed, sourceID, clip(rec.Path, 2048), size, rec.Found, int32(len(rec.Chapters)),
				clip("the header read found no chapter ("+strconv.Itoa(int(found))+" marks); its source record holds "+
					strconv.Itoa(len(rec.Chapters))+": read from it", 500), now, now.Add(SettledWait))
		case rec != nil && rec.Says():
			// its record agrees: no chapter with a length
			reason := strings.TrimSpace(r.Read.Note)
			if reason == "" {
				reason = rec.Note
			}
			err = none(true, reason, "", sourceID)
		case says != nil && !*says:
			err = none(true, "its source record says it holds no chapter", recordWhy(rec), sourceID)
		case says != nil && *says:
			err = failed("the header read found no chapter, and the source's record says it holds some (" +
				recordWhy(rec) + ")")
		default:
			// nothing can say: settles nothing, and its record is read again
			err = none(false, "nothing confirms that the original holds no chapter: "+recordWhy(rec), recordWhy(rec), nil)
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
