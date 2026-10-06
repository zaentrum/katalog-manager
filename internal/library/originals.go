package library

import (
	"context"
	"fmt"
	"time"
)

// What a title has of its original, as the actions that read it ask before
// they go on (platform-library/1, 4.6): a re-encode, a packaging, a reset of
// the analyzer's passes, a retry of a step that reads the original.
const (
	// OriginalPresent: its primary asset is its file, of no source or of a
	// present one.
	OriginalPresent = "present"
	// OriginalRetiring: the retire job is deleting it.
	OriginalRetiring = "retiring"
	// OriginalRetired: it was deleted after packaging; the package is what
	// is left.
	OriginalRetired = "retired"
	// OriginalNone: the title has no file, and had none deleted.
	OriginalNone = "none"
)

// Original is what a title has of its original, and, of one retired or
// retiring, the event that records its deletion and its moment.
type Original struct {
	State   string
	EventID string
	At      *time.Time
}

// Gone reports whether the original is retired, or being retired: nothing
// may read it any more.
func (o Original) Gone() bool { return o.State == OriginalRetired || o.State == OriginalRetiring }

// when is the moment of the original's retirement, as a record writes one.
func (o Original) when() string {
	if o.At == nil {
		return "an unknown time"
	}
	return Timestamp(*o.At)
}

func (o Original) event() string {
	if o.EventID == "" {
		return "none"
	}
	return o.EventID
}

// Why says what became of a retired or retiring original: "the original was
// deleted after packaging (event <id>, <at>)"; "" when the title has it.
func (o Original) Why() string {
	switch o.State {
	case OriginalRetired:
		return fmt.Sprintf("the original was deleted after packaging (event %s, %s)", o.event(), o.when())
	case OriginalRetiring:
		return fmt.Sprintf("the original is being deleted after packaging (event %s, %s)", o.event(), o.when())
	}
	return ""
}

// RetiredAt says when the original was retired, as a refused retry says it:
// "the original was retired at <at> (event <id>)"; "" when the title has it.
func (o Original) RetiredAt() string {
	switch o.State {
	case OriginalRetired:
		return fmt.Sprintf("the original was retired at %s (event %s)", o.when(), o.event())
	case OriginalRetiring:
		return fmt.Sprintf("the original is being retired since %s (event %s)", o.when(), o.event())
	}
	return ""
}

// OriginalsOf reads what each of the titles ids has of its original: present
// while it has a primary asset whose source, if it names one, is not being
// retired; retired once it has none and a source of it was deleted after
// packaging. A catalog without migration 040 keeps no sources: every title
// with a primary asset has its original.
func OriginalsOf(ctx context.Context, q Querier, ids []string) (map[string]Original, error) {
	out := make(map[string]Original, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT i.id,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = i.id AND p.isprimary = true),
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p JOIN com_nalet_katalog_itemsources s ON s.id = p.sourceid
				WHERE p.item_id = i.id AND p.isprimary = true AND s.state = 'retiring'),
			r.state, r.retireeventid, r.at
		FROM unnest($1::text[]) AS i(id)
		LEFT JOIN LATERAL (SELECT s.state, s.retireeventid, COALESCE(s.retireeventat, s.deletedat) AS at
			FROM com_nalet_katalog_itemsources s
			WHERE s.item_id = i.id AND s.state IN ('retiring', 'deleted')
			ORDER BY s.state = 'retiring' DESC, COALESCE(s.retireeventat, s.deletedat) DESC NULLS LAST, s.id
			LIMIT 1) r ON true`, ids)
	if isUndefinedTable(err) {
		return presentOnly(ctx, q, ids)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var primary, retiring bool
		var state, event *string
		var at *time.Time
		if err := rows.Scan(&id, &primary, &retiring, &state, &event, &at); err != nil {
			return nil, err
		}
		o := Original{State: OriginalNone, EventID: deref(event), At: at}
		switch {
		case primary && retiring:
			o.State = OriginalRetiring
		case primary:
			o = Original{State: OriginalPresent}
		case state != nil && *state == SourceDeleted:
			o.State = OriginalRetired
		case state != nil && *state == SourceRetiring:
			o.State = OriginalRetiring
		default:
			o = Original{State: OriginalNone}
		}
		out[id] = o
	}
	return out, rows.Err()
}

// presentOnly is OriginalsOf on a catalog without migration 040.
func presentOnly(ctx context.Context, q Querier, ids []string) (map[string]Original, error) {
	rows, err := q.Query(ctx, `SELECT i.id,
			EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets p WHERE p.item_id = i.id AND p.isprimary = true)
		FROM unnest($1::text[]) AS i(id)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Original, len(ids))
	for rows.Next() {
		var id string
		var primary bool
		if err := rows.Scan(&id, &primary); err != nil {
			return nil, err
		}
		out[id] = Original{State: OriginalNone}
		if primary {
			out[id] = Original{State: OriginalPresent}
		}
	}
	return out, rows.Err()
}

// OriginalGone is the SQL condition that the title item has no original any
// more, which OriginalsOf says retired or retiring: its primary asset's
// source is being retired, or it has no primary asset and a source of it
// was deleted after packaging. It needs migration 040.
func OriginalGone(item string) string {
	return `(EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets og
			JOIN com_nalet_katalog_itemsources ogs ON ogs.id = og.sourceid
			WHERE og.item_id = ` + item + ` AND og.isprimary = true AND ogs.state = 'retiring')
		OR (NOT EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets og WHERE og.item_id = ` + item + ` AND og.isprimary = true)
			AND EXISTS (SELECT 1 FROM com_nalet_katalog_itemsources ogs WHERE ogs.item_id = ` + item + ` AND ogs.state = 'deleted')))`
}

// OriginalOf is what the title id has of its original (OriginalsOf).
func OriginalOf(ctx context.Context, q Querier, id string) (Original, error) {
	m, err := OriginalsOf(ctx, q, []string{id})
	if err != nil {
		return Original{}, err
	}
	o, ok := m[id]
	if !ok {
		o = Original{State: OriginalNone}
	}
	return o, nil
}
