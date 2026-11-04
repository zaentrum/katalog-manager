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
	// Read are the titles whose last read wrote chapters, FromRecord those of
	// them read from their source records; None those whose original holds no
	// chapter with a length, Unconfirmed those of them no record agrees with
	// yet; Unreadable those whose header could not be read again and again,
	// Accepted those of them an admin settled; Failed those whose last read
	// failed (each waits its backoff); InFlight those handed out now.
	Read, FromRecord, None, Unconfirmed, Unreadable, Accepted, Failed, InFlight int
	// HandedOutLastHour and RecordsReadLastHour are the titles the backfill
	// handed out, and the source records the catalog read, in the last hour.
	HandedOutLastHour, RecordsReadLastHour int
	LastReadAt                             *time.Time
	// Settings are the backfill's settings, and Idle why it reads nothing
	// now ("" when it may).
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'read'),
			count(*) FILTER (WHERE state = 'read' AND readfrom = 'record'), count(*) FILTER (WHERE state = 'none'),
			count(*) FILTER (WHERE state = 'none' AND NOT confirmed), count(*) FILTER (WHERE state = 'unreadable'),
			count(*) FILTER (WHERE state = 'unreadable' AND acceptedat IS NOT NULL), count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE claimedat IS NOT NULL), count(*) FILTER (WHERE recordat > $1), max(readat)
		FROM `+store.ChapterReadsTable, now.Add(-time.Hour)).
		Scan(&p.Read, &p.FromRecord, &p.None, &p.Unconfirmed, &p.Unreadable, &p.Accepted, &p.Failed, &p.InFlight,
			&p.RecordsReadLastHour, &p.LastReadAt); err != nil {
		return p, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+store.ChapterClaimsTable+` WHERE claimedat > $1`,
		now.Add(-time.Hour)).Scan(&p.HandedOutLastHour); err != nil {
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
	case p.Missing == 0:
		p.Idle = "no title misses its chapters"
	}
	return p, nil
}
