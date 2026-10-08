package library

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/processing"
)

// One file of several episodes (migration 045, the published v2 contract): a
// file whose name numbers a range of episodes (S05E15-E16) is never split.
// It belongs to the first episode it covers, its holder, as any episode's
// file belongs to its episode: its source, its versions, its package, its
// pipeline run and its retire are the holder's, in the holder's folder. Every
// other episode it covers keeps an item of its own (item.json, metadata.json:
// its own title, overview and images) and names its holder (coveredby); it
// has no source and no version of its own, its steps of a file do not apply
// (processing.CoveredReason), and it plays its holder's version: its
// projection's primaryVersionId is the holder's current version, and its
// coveredBy the holder. The holder's source record lists the episodes its
// file covers, its own id first, in episode order (covers), and its
// projection numbers it up to the last of them (numbering's episodeEnd).

// coversColumn is the column of migration 045 that names a covered episode's
// holder.
const coversColumn = "coveredby"

// CoversReady reports whether migration 045 is in place in the catalog q
// reads: an item may name the episode whose file covers it. Without it a
// file covers the episode its name numbers first and no other.
func CoversReady(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_items')
		AND attname = $1 AND attnum > 0 AND NOT attisdropped)`, coversColumn).Scan(&ok)
	return ok, err
}

// HolderOf is the episode whose file covers the item id; "" when none does
// (also on a catalog without migration 045, whose column it reads by name).
func HolderOf(ctx context.Context, q Querier, id string) (string, error) {
	var holder *string
	err := q.QueryRow(ctx, `SELECT to_jsonb(i)->>'coveredby' FROM com_nalet_katalog_items i WHERE i.id = $1`, id).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil || holder == nil {
		return "", err
	}
	return *holder, nil
}

// HolderOrSelf is the episode whose file is the item id's: its holder when
// another's file covers it, the item itself otherwise.
func HolderOrSelf(ctx context.Context, q Querier, id string) (string, error) {
	holder, err := HolderOf(ctx, q, id)
	if err != nil || holder == "" {
		return id, err
	}
	return holder, nil
}

// CoveredNote begins what an action on the episode id says when it acts on
// its holder, whose file covers it: re-encoding it, packaging it, taking it
// in, giving it another file.
func CoveredNote(id, holder string) string {
	return "the file of episode " + holder + " covers episode " + id + ", and acts for it: "
}

// CoveredEpisode is an episode another's file covers: its id and numbers.
type CoveredEpisode struct {
	ID              string
	Season, Episode *int32
}

// CoveredOf are the episodes the file of the holder covers besides the
// holder, in episode order (season, number, id); none on a catalog without
// migration 045.
func CoveredOf(ctx context.Context, q Querier, holder string) ([]CoveredEpisode, error) {
	if ok, err := CoversReady(ctx, q); err != nil || !ok {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT id, seasonnumber, episodenumber FROM com_nalet_katalog_items
		WHERE coveredby = $1 AND id <> $1 ORDER BY seasonnumber NULLS LAST, episodenumber NULLS LAST, id`, holder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoveredEpisode
	for rows.Next() {
		var c CoveredEpisode
		if err := rows.Scan(&c.ID, &c.Season, &c.Episode); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CoversOf is what the holder's source record lists as the episodes its file
// covers (covers): the holder first, then every episode it covers besides
// it, in episode order; nil when it covers no other.
func CoversOf(ctx context.Context, q Querier, holder string) ([]string, error) {
	covered, err := CoveredOf(ctx, q, holder)
	if err != nil || len(covered) == 0 {
		return nil, err
	}
	out := []string{holder}
	for _, c := range covered {
		out = append(out, c.ID)
	}
	return out, nil
}

// Link has the file of the episode holder cover the episode covered: it
// names its holder, its steps of a file do not apply, saying so, and both are
// marked changed (the covered one's projection plays the holder's version,
// the holder's numbers it up to the last it covers). It answers whether the
// link is new. It needs migration 045.
func Link(ctx context.Context, q Querier, holder, covered string) (bool, error) {
	if holder == covered {
		return false, fmt.Errorf("episode %s cannot cover itself", holder)
	}
	var cur *string
	err := q.QueryRow(ctx, `SELECT coveredby FROM com_nalet_katalog_items WHERE id = $1`, covered).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("%w: %s", ErrNoItem, covered)
	}
	if err != nil {
		return false, err
	}
	if err := coverSteps(ctx, q, []string{covered}, processing.CoveredReason(holder)); err != nil {
		return false, err
	}
	if cur != nil && *cur == holder {
		return false, nil
	}
	if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_items SET coveredby = $2, modifiedat = now() WHERE id = $1`,
		covered, holder); err != nil {
		return false, err
	}
	// The holder numbers it now, and one that did before no more.
	_, err = q.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE id = $1 OR id = $2`, holder, cur)
	return true, err
}

// Unlink has the file that covers each of the episodes ids cover it no more,
// as why says (processing.UncoveredReason): each names no holder, its steps
// still do not apply, saying so, and it and its holder are marked changed.
// It answers the episodes it unlinked. On a catalog without migration 045
// none is linked.
func Unlink(ctx context.Context, q Querier, ids []string, why string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if ok, err := CoversReady(ctx, q); err != nil || !ok {
		return nil, err
	}
	rows, err := q.Query(ctx, `WITH old AS (SELECT id, coveredby FROM com_nalet_katalog_items
			WHERE id = ANY($1) AND coveredby IS NOT NULL FOR UPDATE)
		UPDATE com_nalet_katalog_items i SET coveredby = NULL, modifiedat = now()
		FROM old WHERE i.id = old.id
		RETURNING i.id, old.coveredby`, ids)
	if err != nil {
		return nil, err
	}
	type unlinked struct{ id, holder string }
	var gone []unlinked
	for rows.Next() {
		var u unlinked
		if err := rows.Scan(&u.id, &u.holder); err != nil {
			rows.Close()
			return nil, err
		}
		gone = append(gone, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, u := range gone {
		if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE id = $1`, u.holder); err != nil {
			return out, err
		}
		if err := coverSteps(ctx, q, []string{u.id}, processing.UncoveredReason(u.holder, why)); err != nil {
			return out, err
		}
		out = append(out, u.id)
	}
	return out, nil
}

// MarkCoveredChanged marks the episodes the file of the holder covers
// changed (modifiedat): their projections play its version, which changed.
// Nothing is marked on a catalog without migration 045.
func MarkCoveredChanged(ctx context.Context, q Querier, holder string) error {
	if ok, err := CoversReady(ctx, q); err != nil || !ok {
		return err
	}
	_, err := q.Exec(ctx, `UPDATE com_nalet_katalog_items SET modifiedat = now() WHERE coveredby = $1 AND id <> $1`, holder)
	return err
}

// coverSteps has the steps of a file of the episodes ids not apply, as
// reason says: every step they have but their own (processing.OwnSteps), and
// processing.CoveredSteps made when they lack one. A step that says so
// already is left as it is. It writes the step table with or without
// migration 033: a step that does not apply has no failure in a row, and no
// retry.
func coverSteps(ctx context.Context, q Querier, ids []string, reason string) error {
	reason = *processing.CleanError(reason)
	var retries bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('com_nalet_katalog_itemprocessingsteps')
		AND attname = 'nextretryat' AND attnum > 0 AND NOT attisdropped)`).Scan(&retries); err != nil {
		return err
	}
	set := `status = 'not_applicable', error = $2, modifiedat = now()`
	if retries {
		set += `, failures = 0, nextretryat = NULL, dispatchedat = NULL`
	}
	if _, err := q.Exec(ctx, `UPDATE com_nalet_katalog_itemprocessingsteps SET `+set+`
		WHERE item_id = ANY($1) AND NOT (step = ANY($3)) AND (status <> 'not_applicable' OR error IS DISTINCT FROM $2)`,
		ids, reason, processing.OwnSteps); err != nil {
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO com_nalet_katalog_itemprocessingsteps (id, createdat, modifiedat, item_id, step, status, attempts, error)
		SELECT gen_random_uuid()::varchar, now(), now(), c.id, st.step, 'not_applicable', 0, $2
		FROM unnest($1::text[]) AS c(id) CROSS JOIN unnest($3::text[]) AS st(step)
		ON CONFLICT (item_id, step) DO NOTHING`, ids, reason, processing.CoveredSteps)
	return err
}
