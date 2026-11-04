// Package chapters keeps a title's chapters, and what reading them from its
// original found (migration 055, README "Chapters").
//
// A title's chapters live in the catalog (com_nalet_katalog_itemchapters),
// not in its package, which carries none: the original is the only place
// they are read from, and a deletion wave keeps it until they are in the
// catalog. The analyzer reads them from the original's header (ffprobe's
// chapters; nothing is decoded), in its pass of a new title and in the
// backfill (Service.Claim), and reports each read with the chapters it found
// (PUT /api/chapters/items/{id}, Record). The catalog writes the chapters and
// keeps the read in the same transaction, so a read that found none settles a
// title as one that wrote chapters does, and so does a title whose reads
// failed MaxFailures times in a row (unreadable). A title whose original's
// record says it has chapters, which has none in the catalog and no read of
// that original that settled it, misses them (the view
// com_nalet_katalog_chaptersmissing, NeedsOriginal).
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
	StateNone       = "none"       // the header holds no chapter with a length
	StateFailed     = "failed"     // the last read failed (Reason); read again from NextAttemptAt
	StateUnreadable = "unreadable" // MaxFailures reads in a row failed: settled while the original is the same
)

// MaxFailures is how many failed reads in a row settle a title as
// unreadable. The first wait after a failure is FirstBackoff, doubled after
// each, MaxBackoff at most: the fifth failure comes at least 15 hours after
// the first, so an hour's outage of the storage settles nothing. A title
// handed out and given back unread waits ReleaseWait.
const (
	MaxFailures  = 5
	FirstBackoff = time.Hour
	MaxBackoff   = 24 * time.Hour
	ReleaseWait  = 15 * time.Minute
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
	// SourceID is the original read, Path the file and FileSize its size.
	SourceID, Path *string
	FileSize       *int64
	// Found are the chapter marks the header held, Written the chapters
	// written of them.
	Found, Written *int32
	Probe, Reason  *string
	Seconds        *float64
	ReadAt         *time.Time
	ClaimedAt      *time.Time
	ClaimedBy      *string
	ClaimToken     *string
	LastClaimedAt  *time.Time
	Attempts       int32
	Failures       int32
	NextAttemptAt  *time.Time
	LapsedToken    *string
	ModifiedAt     time.Time
}

// Settled reports whether the read settles the title's chapters for its
// original sourceID: a read of it that found none, or that could not read it
// for good. (One that wrote chapters is settled by the chapters themselves.)
func (r *Read) Settled(sourceID string) bool {
	return r != nil && (r.State == StateNone || r.State == StateUnreadable) && r.SourceID != nil &&
		sourceID != "" && *r.SourceID == sourceID
}

const readColumns = `r.item_id, r.state, r.sourceid, r.path, r.filesize, r.found, r.written, r.probe, r.reason,
	r.seconds, r.readat, r.claimedat, r.claimedby, r.claimtoken, r.lastclaimedat, r.attempts, r.failures,
	r.nextattemptat, r.lapsedtoken, r.modifiedat`

func scanRead(row pgx.Row) (*Read, error) {
	r := &Read{}
	var seconds *float32
	err := row.Scan(&r.ItemID, &r.State, &r.SourceID, &r.Path, &r.FileSize, &r.Found, &r.Written, &r.Probe,
		&r.Reason, &seconds, &r.ReadAt, &r.ClaimedAt, &r.ClaimedBy, &r.ClaimToken, &r.LastClaimedAt,
		&r.Attempts, &r.Failures, &r.NextAttemptAt, &r.LapsedToken, &r.ModifiedAt)
	if err != nil {
		return nil, err
	}
	if seconds != nil {
		v := float64(*seconds)
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
// sourceID ("" for any of its originals) for its chapters: the original's
// record says it has chapters (the compliance check's), the catalog has none
// of the title, and no read of that original settled it (none, unreadable).
// A deletion wave keeps the original while it does. A title the compliance
// check has no record of, or whose record says no chapters, needs none. A
// catalog without migration 055 is ErrNotReady, and keeps it.
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

// ReadReport is what the analyzer says of a read of a title's original: the
// file it read (Path; FileSize its size), the chapter marks its header held
// (Found), why the read failed (Error: nothing was found, and the title's
// chapters stay as they are), what was left out (Note, such as marks without
// a length), what read it (Probe) and how long it took (Seconds).
type ReadReport struct {
	Path     string   `json:"path"`
	FileSize *int64   `json:"fileSize"`
	Found    *int32   `json:"found"`
	Error    string   `json:"error"`
	Note     string   `json:"note"`
	Probe    string   `json:"probe"`
	Seconds  *float64 `json:"seconds"`
}

// Report is a read the analyzer reports: what it read (Read), the chapters it
// found (Chapters: the whole set, which replaces the title's; nil for a read
// that failed), and the token of the backfill's claim it read for (none for
// the analyzer's pass of a new title). Released gives a claim back unread;
// nothing else is read of it.
type Report struct {
	Read     ReadReport
	Chapters []Chapter
	Token    string
	Released bool
}

// state is the state the report leaves its title in, or why it can't be
// taken.
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

// failure is what the failures-th failed read in a row, why, leaves its title
// in: failed, read again after its backoff; the MaxFailures-th settles it as
// unreadable, read no more (no next attempt), saying how many failed.
func failure(failures int32, why string, now time.Time) (state string, next *time.Time, reason string) {
	if failures >= MaxFailures {
		return StateUnreadable, nil, clip(strconv.Itoa(int(failures))+" reads in a row failed, the last: "+why, 500)
	}
	at := now.Add(backoff(failures))
	return StateFailed, &at, clip(why, 500)
}

// backoff is the wait after the failures-th failure in a row.
func backoff(failures int32) time.Duration {
	d := FirstBackoff
	for i := int32(1); i < failures && d < MaxBackoff; i++ {
		d *= 2
	}
	return min(d, MaxBackoff)
}

// settledSQL says, of the read row aliased r, that the title misses no
// chapters now: it is not among com_nalet_katalog_chaptersmissing.
var settledSQL = `NOT EXISTS (SELECT 1 FROM ` + store.ChaptersMissingView + ` m WHERE m.item_id = r.item_id)`

// Record keeps a read the analyzer reported of the original of the title
// itemID, and answers the title's read as it is now. A read that found its
// chapters writes them, in place of the title's (none for a header that holds
// none with a length), and keeps the read (read, or none), its failures
// ended. A read that failed leaves the title's chapters as they are and
// counts a failure, which waits its backoff; the MaxFailures-th in a row
// settles the title as unreadable. One reported before the last failure's
// backoff ended (the step's quick retries of the analyzer's pass) only says
// why: the failures that settle a title span their backoffs, 15 hours at
// least, whoever reports them. A failure of a title that misses no chapters
// (its chapters are in the catalog, or a read of its original settled it)
// is no failure of it: only its claim ends. Released gives the
// title's claim back unread, which waits ReleaseWait. A report ends the
// claim it names (its token) and only that one: one of a claim whose lease
// ended leaves the claim made since as it is, its failure counted already
// then; one without a token (the analyzer's pass of a new title) leaves any
// claim as it is. The original read is the source of the file: in a folder of
// one of the item's versions, the first original that version was made of;
// else the source of its primary asset of that path. A series or a season is
// no title read (400); an unknown one is ErrNoTitle.
func Record(ctx context.Context, st *store.Store, itemID string, r Report, now time.Time) (*Read, error) {
	state, err := r.state()
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
		if sourceID, size, err = fileOf(ctx, pool, itemID, r.Read.Path); err != nil {
			return nil, err
		}
		if r.Read.FileSize != nil && *r.Read.FileSize > 0 {
			size = r.Read.FileSize
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
	token := strings.TrimSpace(r.Token)
	// owns: the report is of the claim the title is under; lapsed: of the
	// claim whose lease ended last, counted a failure then
	owns := token != "" && kept.ClaimedAt != nil && kept.ClaimToken != nil && token == *kept.ClaimToken
	lapsed := token != "" && kept.LapsedToken != nil && token == *kept.LapsedToken
	claim := `claimedat = CASE WHEN $2 THEN NULL ELSE claimedat END,
		claimedby = CASE WHEN $2 THEN NULL ELSE claimedby END,
		claimtoken = CASE WHEN $2 THEN NULL ELSE claimtoken END,
		lapsedtoken = CASE WHEN $3 THEN NULL ELSE lapsedtoken END`

	switch state {
	case "": // released
		if owns {
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`,
				nextattemptat = GREATEST(nextattemptat, $4), modifiedat = $5 WHERE item_id = $1`,
				itemID, owns, lapsed, now.Add(ReleaseWait), now)
		}
	case StateFailed:
		why := clip(strings.TrimSpace(r.Read.Error), 500)
		var settled bool
		if err = tx.QueryRow(ctx, `SELECT `+settledSQL+` FROM `+store.ChapterReadsTable+` r WHERE r.item_id = $1`,
			itemID).Scan(&settled); err != nil {
			return nil, err
		}
		switch {
		case lapsed:
			// counted when its lease ended: this only says why
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`,
				reason = CASE WHEN state IN ('pending', 'failed') THEN $4 ELSE reason END, modifiedat = $5
				WHERE item_id = $1`, itemID, owns, lapsed, why, now)
		case settled && kept.State != StatePending && kept.State != StateFailed:
			// what a read found of it stands: the failure only ends its claim
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`, modifiedat = $4
				WHERE item_id = $1`, itemID, owns, lapsed, now)
		case kept.State == StateFailed && kept.NextAttemptAt != nil && now.Before(*kept.NextAttemptAt):
			// within the last failure's backoff (the step's quick retries
			// of a pass): what failed is said, and counted once only, so the
			// failures that settle a title span their backoffs
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`, reason = $4, readat = $5,
				modifiedat = $5 WHERE item_id = $1`, itemID, owns, lapsed, why, now)
		default:
			failures := kept.Failures + 1
			newState, next, reason := failure(failures, why, now)
			_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`, state = $4, sourceid = $5,
				path = $6, filesize = $7, found = NULL, written = NULL, probe = NULLIF($8, ''), seconds = $9,
				reason = $10, readat = $11, failures = $12, nextattemptat = $13, modifiedat = $11
				WHERE item_id = $1`, itemID, owns, lapsed, newState, sourceID, clip(r.Read.Path, 2048), size,
				clip(strings.TrimSpace(r.Read.Probe), 200), seconds(r.Read.Seconds), reason, now, failures, next)
		}
	default: // read, none
		if err = Replace(ctx, tx, itemID, r.Chapters); err != nil {
			return nil, err
		}
		written := int32(len(r.Chapters))
		found := written
		if r.Read.Found != nil && *r.Read.Found > written {
			found = *r.Read.Found
		}
		_, err = tx.Exec(ctx, `UPDATE `+store.ChapterReadsTable+` SET `+claim+`, state = $4, sourceid = $5,
			path = $6, filesize = $7, found = $8, written = $9, probe = NULLIF($10, ''), seconds = $11,
			reason = NULLIF($12, ''), readat = $13, failures = 0, nextattemptat = NULL, modifiedat = $13
			WHERE item_id = $1`, itemID, owns, lapsed, state, sourceID, clip(r.Read.Path, 2048), size, found,
			written, clip(strings.TrimSpace(r.Read.Probe), 200), seconds(r.Read.Seconds),
			clip(strings.TrimSpace(r.Read.Note), 500), now)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ReadOf(ctx, pool, itemID)
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
