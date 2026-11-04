package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
)

// An original another file took the place of as its title's (replaceSource
// with deleteOldFile, the library's v2 layout) is superseded (migration 056):
// the title plays and is encoded from its new file, its source the new one's,
// and the old original stays where it lies — in the arrivals, or in the folder
// of its version — with the version made of it, so that a wrong file given in
// its place can be taken back. Nothing deletes it then. Its row names the
// source that took its place (supersededby) and since when; the title's
// folder records a note of what became of it, by whoever asked the replace.
// The scanner and the ingest take it, and a copy of it, for that title's old
// original, no title of its own; the size of a title's originals counts it;
// a version whose folder holds it is not removed; a title removed with its
// files takes it with them. The retire job retires it once a version of the
// title made without it is complete and verified, unless the title holds its
// originals (holdOriginal), through the trash, as it retires an original.

// ReplacedBy is who asked a replace, as the note of what became of the old
// original says: replaceSource, asked by caller.
func ReplacedBy(caller string) string { return clip("replaceSource, asked by "+caller, 255) }

// SupersedeReady reports whether migration 056 is in place: a source may be
// superseded, and its columns are there.
func SupersedeReady(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass('com_nalet_katalog_itemsources') AND contype = 'c'
			  AND pg_get_constraintdef(oid) LIKE '%''superseded''%')
		AND (SELECT count(*) FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_itemsources')
			AND attname IN ('supersededby', 'supersededat', 'replacekey') AND attnum > 0 AND NOT attisdropped) = 3`).Scan(&ok)
	return ok, err
}

// ErrNotPresent says a source is not present, so that no other file takes
// its place now: it is being retired, or what it is says.
type ErrNotPresent struct{ ID, State string }

func (e *ErrNotPresent) Error() string {
	if e.State == SourceRetiring {
		return fmt.Sprintf("the title's original (source %s) is being retired", e.ID)
	}
	return fmt.Sprintf("the title's original (source %s) is %s, not present", e.ID, e.State)
}

// Supersede notes, in q (the replace's transaction, which holds the item's
// lock), that the source old, present, is superseded by the source by: the
// file of by took its place as its title's. A source not present (being
// retired, or other) is not: ErrNotPresent. A source recorded without its
// quick hash (an older row) gets it, read from its file while that is of the
// size it recorded, so that the retire job can tell it later.
func Supersede(ctx context.Context, q Querier, old, by string) error {
	var qh1 *string
	if s, err := SourceByID(ctx, q, old); err != nil {
		return err
	} else if s != nil && s.QH1 == nil && s.ArrivalPath != nil {
		if size, h, err := QH1(*s.ArrivalPath); err == nil && size == s.SizeBytes {
			qh1 = &h
		}
	}
	tag, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemsources SET state = 'superseded', supersededby = $2,
			supersededat = now(), qh1 = COALESCE(qh1, $3), error = NULL, modifiedat = now()
		WHERE id = $1 AND state = 'present'`, old, by, qh1)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	state := "gone"
	if err := q.QueryRow(ctx, `SELECT state FROM com_nalet_katalog_itemsources WHERE id = $1`, old).Scan(&state); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return &ErrNotPresent{ID: old, State: state}
}

// SupersededBy is the source that took the place of the source id as its
// title's, "" when none did (or migration 056 is not in place).
func SupersededBy(ctx context.Context, q Querier, id string) (string, error) {
	var by *string
	err := q.QueryRow(ctx, `SELECT supersededby FROM com_nalet_katalog_itemsources WHERE id = $1`, id).Scan(&by)
	switch {
	case errors.Is(err, pgx.ErrNoRows), isUndefinedColumn(err):
		return "", nil
	case err != nil:
		return "", err
	}
	return deref(by), nil
}

// Recorded reports whether the source s has its record in the item folder
// itemDir (sources/<sourceId>/source.json): an event may name it.
func Recorded(itemDir string, s *Source) bool {
	return s != nil && s.RecordedAt != nil && statOK(filepath.Join(SourceDir(itemDir, s.ID), "source.json"))
}

// NoteReplaced records, in the item folder itemDir, a note of what became of
// the title's old original once another file took its place (old: its
// source, nil when it had none; newName the new file's name): by whoever asked
// the replace (by), naming the old source when its record is there. It
// answers the note's event id, "" with an error when it could not be written,
// and nothing when the title has no folder.
func NoteReplaced(itemDir, by string, old *Source, newName string, superseded bool, at time.Time) (string, error) {
	if fi, err := os.Stat(itemDir); err != nil || !fi.IsDir() {
		return "", nil
	}
	name := ""
	if old != nil {
		name = old.Filename
		if old.ArrivalPath != nil {
			name = filepath.Base(*old.ArrivalPath)
		}
	}
	reason := fmt.Sprintf("the title was given another file in its place: %s (replaceSource)", newName)
	switch {
	case old == nil:
	case superseded:
		reason += fmt.Sprintf("; its original %s is superseded, kept where it lies until a version of the title made "+
			"without it is complete and verified, then the retire job moves it into the trash", name)
	default:
		reason += fmt.Sprintf("; its original %s stays a source of the title, kept where it lies", name)
	}
	ev := Event{ID: NewID(), At: at.UTC().Truncate(time.Second), By: clip(by, 255), Kind: EventNote, Reason: &reason}
	if Recorded(itemDir, old) {
		ev.SourceID = old.ID
	}
	if _, err := WriteEvent(itemDir, ev); err != nil {
		return "", err
	}
	return ev.ID, nil
}

// Superseded is what the catalog holds of an original another file took the
// place of (migrations 056 and 057): the source, its title, the source that
// took its place and since when, why it waits for an admin, against which
// master, and what deleting it gives up against that master (Losses, nil
// when no record tells), and what an admin decided (accept or keep), what
// they accepted, who and when.
type Superseded struct {
	SourceID, ItemID string
	By               string
	At               *time.Time
	Hold, HoldFor    *string
	Losses           []string
	Decision         *string
	Accepted         []string
	DecidedBy        *string
	DecidedAt        *time.Time
}

// The decisions an admin takes of a superseded original that waits for one.
const (
	DecisionAccept = "accept"
	DecisionKeep   = "keep"
)

const supersededCols = `id, item_id, COALESCE(supersededby, ''), supersededat, supersededhold, supersededholdfor, supersededlosses,
	supersededdecision, supersededaccepted, supersededdecidedby, supersededdecidedat`

func scanSuperseded(row pgx.Row) (*Superseded, error) {
	var x Superseded
	var losses, accepted []byte
	if err := row.Scan(&x.SourceID, &x.ItemID, &x.By, &x.At, &x.Hold, &x.HoldFor, &losses, &x.Decision, &accepted, &x.DecidedBy,
		&x.DecidedAt); err != nil {
		return nil, err
	}
	x.Losses, x.Accepted = namesOf(losses), namesOf(accepted)
	return &x, nil
}

// namesOf are the names of a JSON list of them; nil for none, or SQL NULL.
func namesOf(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	v, err := Decode(b)
	if err != nil {
		return nil
	}
	return stringsOf(v)
}

// SupersededOf is what the catalog holds of the item's sources that are or
// were superseded, by their ids; nil without migration 057.
func SupersededOf(ctx context.Context, q Querier, itemID string) (map[string]*Superseded, error) {
	rows, err := q.Query(ctx, `SELECT `+supersededCols+` FROM com_nalet_katalog_itemsources
		WHERE item_id = $1 AND supersededby IS NOT NULL ORDER BY id`, itemID)
	if isUndefinedColumn(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Superseded{}
	for rows.Next() {
		x, err := scanSuperseded(rows)
		if err != nil {
			return nil, err
		}
		out[x.SourceID] = x
	}
	return out, rows.Err()
}

// SupersededOriginal is a superseded original as an admin lists it: what the
// catalog holds of it, its title, and where its file lies.
type SupersededOriginal struct {
	Superseded
	Title, Filename string
	Path            *string
	State           string
	SizeBytes       int64
}

// SupersededOriginals lists the originals that are superseded now, those
// waiting for an admin first (held: those alone), the ones superseded first
// first, at most limit.
func SupersededOriginals(ctx context.Context, q Querier, held bool, limit int) ([]*SupersededOriginal, error) {
	rows, err := q.Query(ctx, `SELECT `+supersededColsOf("s")+`, i.title, s.filename, s.arrivalpath, s.state, s.sizebytes
		FROM com_nalet_katalog_itemsources s JOIN com_nalet_katalog_items i ON i.id = s.item_id
		WHERE s.state = 'superseded' AND (NOT $1 OR (s.supersededhold IS NOT NULL AND s.supersededdecision IS NULL))
		ORDER BY (s.supersededhold IS NOT NULL AND s.supersededdecision IS NULL) DESC, s.supersededat, s.id
		LIMIT $2`, held, limit)
	if isUndefinedColumn(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SupersededOriginal
	for rows.Next() {
		var x SupersededOriginal
		var losses, accepted []byte
		if err := rows.Scan(&x.SourceID, &x.ItemID, &x.By, &x.At, &x.Hold, &x.HoldFor, &losses, &x.Decision, &accepted,
			&x.DecidedBy, &x.DecidedAt, &x.Title, &x.Filename, &x.Path, &x.State, &x.SizeBytes); err != nil {
			return nil, err
		}
		x.Losses, x.Accepted = namesOf(losses), namesOf(accepted)
		out = append(out, &x)
	}
	return out, rows.Err()
}

// supersededColsOf is supersededCols of the row alias a.
func supersededColsOf(a string) string {
	return a + `.id, ` + a + `.item_id, COALESCE(` + a + `.supersededby, ''), ` + a + `.supersededat, ` + a + `.supersededhold, ` +
		a + `.supersededholdfor, ` + a + `.supersededlosses, ` + a + `.supersededdecision, ` + a + `.supersededaccepted, ` +
		a + `.supersededdecidedby, ` + a + `.supersededdecidedat`
}

// ErrNotSuperseded says a decision was asked of a source that is no
// superseded original (any more).
type ErrNotSuperseded struct{ ID, State string }

func (e *ErrNotSuperseded) Error() string {
	return fmt.Sprintf("source %s is %s, no superseded original: there is nothing to decide of it", e.ID, e.State)
}

// DecideSuperseded notes an admin's decision of the superseded original
// sourceID, by: accept has the retire job retire it, giving up what its hold
// says deleting it gives up against its master (or anything, when no record
// tells), once its other conditions hold; keep keeps it, never retired by the
// job until an admin accepts it. A source that is not superseded is
// ErrNotSuperseded. It answers the source as it is then.
func DecideSuperseded(ctx context.Context, q Querier, sourceID, decision, by string) (*Superseded, error) {
	if decision != DecisionAccept && decision != DecisionKeep {
		return nil, fmt.Errorf("a decision is %s or %s, not %q", DecisionAccept, DecisionKeep, decision)
	}
	x, err := scanSuperseded(q.QueryRow(ctx, `UPDATE com_nalet_katalog_itemsources SET supersededdecision = $2::text,
			supersededaccepted = CASE WHEN $2::text = 'accept' THEN supersededlosses END, supersededdecidedby = left($3, 255),
			supersededdecidedat = now(), modifiedat = now()
		WHERE id = $1 AND state = 'superseded'
		RETURNING `+supersededCols, sourceID, decision, by))
	if errors.Is(err, pgx.ErrNoRows) {
		state := "gone"
		if s, serr := SourceByID(ctx, q, sourceID); serr == nil && s != nil {
			state = s.State
		}
		return nil, &ErrNotSuperseded{ID: sourceID, State: state}
	}
	return x, err
}
