package store

import (
	"context"
	"time"
)

// ActivityRow is one pipeline event: a processing-step row joined to its item.
// The pipeline is a DB state machine — scan seeds pending steps, and the
// enrichment/analyze/transcode/package workers advance them (pending →
// in_progress → done|failed). Reading the step rows newest-first therefore
// reconstructs the whole pipeline timeline without any separate event log.
type ActivityRow struct {
	ID         string
	ItemID     string
	ItemTitle  string
	ItemType   string
	Step       string
	Status     string
	Attempts   *int32
	Error      *string
	StartedAt  *time.Time
	FinishedAt *time.Time
	UpdatedAt  *time.Time
}

// ActivityFeed returns the most recently changed processing steps across all
// items, newest first. This is the read side of the Activity monitor: one row
// per (item, step) reflecting its current status and last transition time.
func (s *Store) ActivityFeed(ctx context.Context, limit int32) ([]*ActivityRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.item_id, COALESCE(i.title, ''), COALESCE(i.type, ''),
		       s.step, s.status, s.attempts, s.error, s.startedat, s.finishedat,
		       COALESCE(s.modifiedat, s.createdat)
		FROM com_nalet_katalog_itemprocessingsteps s
		JOIN com_nalet_katalog_items i ON i.id = s.item_id
		ORDER BY COALESCE(s.modifiedat, s.createdat) DESC NULLS LAST
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ActivityRow
	for rows.Next() {
		var x ActivityRow
		if err := rows.Scan(&x.ID, &x.ItemID, &x.ItemTitle, &x.ItemType,
			&x.Step, &x.Status, &x.Attempts, &x.Error, &x.StartedAt, &x.FinishedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &x)
	}
	return out, rows.Err()
}
