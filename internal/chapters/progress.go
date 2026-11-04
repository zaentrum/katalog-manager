package chapters

import (
	"context"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// Progress is how far the chapters are: the titles that miss theirs (those of
// them whose version is complete, whose originals the deletion waves would
// retire next), the reads the catalog keeps by what they found, and how the
// backfill stands.
type Progress struct {
	// Missing are the titles that miss their chapters
	// (com_nalet_katalog_chaptersmissing), MissingPackaged those of them
	// whose version is complete.
	Missing, MissingPackaged int
	// Read, None, Unreadable and Failed are the titles whose last read wrote
	// chapters, found none with a length, could not read the file for good,
	// and failed (each waits its backoff); InFlight those handed out now.
	Read, None, Unreadable, Failed, InFlight int
	// HandedOutLastHour is how many the backfill handed out in the last hour.
	HandedOutLastHour int
	LastReadAt        *time.Time
	// Settings are the backfill's settings, and Idle why it hands out
	// nothing now ("" when it may).
	Settings Settings
	Idle     string
}

// Progress reads how far the chapters are.
func (s *Service) Progress(ctx context.Context) (Progress, error) {
	var p Progress
	if err := s.ready(ctx); err != nil {
		return p, err
	}
	pool := s.st.Pool()
	now := s.now().UTC()
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT m.item_id),
			count(DISTINCT m.item_id) FILTER (WHERE EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
			                                                WHERE v.item_id = m.item_id AND v.state = 'complete'))
		FROM `+store.ChaptersMissingView+` m`).Scan(&p.Missing, &p.MissingPackaged); err != nil {
		return p, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'read'), count(*) FILTER (WHERE state = 'none'),
			count(*) FILTER (WHERE state = 'unreadable'), count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE claimedat IS NOT NULL), count(*) FILTER (WHERE lastclaimedat > $1), max(readat)
		FROM `+store.ChapterReadsTable, now.Add(-time.Hour)).
		Scan(&p.Read, &p.None, &p.Unreadable, &p.Failed, &p.InFlight, &p.HandedOutLastHour, &p.LastReadAt); err != nil {
		return p, err
	}
	var err error
	if p.Settings, err = ReadSettings(ctx, pool); err != nil {
		return p, err
	}
	switch set := p.Settings; {
	case !set.On:
		p.Idle = SettingBackfill + " is off"
	case set.Window.Never:
		p.Idle = SettingWindow + " is no window"
	case !set.Window.Contains(now):
		p.Idle = "outside " + SettingWindow + " (" + set.Window.String() + ")"
	case set.Rate == 0:
		p.Idle = SettingRate + " is 0"
	case p.InFlight >= set.Inflight:
		p.Idle = "titles enough are being read (" + SettingInflight + ")"
	case p.HandedOutLastHour >= set.Rate:
		p.Idle = "the hour's titles were handed out (" + SettingRate + ")"
	case p.Missing == 0:
		p.Idle = "no title misses its chapters"
	}
	return p, nil
}
