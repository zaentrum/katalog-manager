package picture

import (
	"context"
	"time"

	"github.com/zaentrum/katalog-manager/internal/store"
)

// Progress is how far the picture areas are: of the titles there are to
// measure (movies and episodes with a file, extras with a package), how many
// have an area of what they play now and what it is, how many wait, and how
// the backfill stands.
type Progress struct {
	// Titles is how many titles there are to measure; Letterboxed (bars,
	// letterbox or pillarbox), Full (no bars) and Unknown (the analyzer
	// could not tell) are those with an area of what they play now, and
	// ToMeasure the rest.
	Titles, Letterboxed, Full, Unknown, ToMeasure int
	// Failed are those whose last run failed (each waits its backoff),
	// InFlight those handed out now.
	Failed, InFlight int
	// HandedOutLastHour is how many the backfill handed out in the last hour.
	HandedOutLastHour int
	// CPUSecondsPerHour is the CPU the measurements of what plays now took,
	// an hour of video; nil before any reported it.
	CPUSecondsPerHour *float64
	LastMeasuredAt    *time.Time
	// Settings are the backfill's settings, and Idle why it hands out
	// nothing now ("" when it may).
	Settings Settings
	Idle     string
}

// Progress reads how far the picture areas are.
func (s *Service) Progress(ctx context.Context) (Progress, error) {
	var p Progress
	if err := s.ready(ctx); err != nil {
		return p, err
	}
	pool := s.st.Pool()
	now := s.now().UTC()
	err := pool.QueryRow(ctx, `
		WITH t AS (
			SELECT i.id FROM com_nalet_katalog_items i
			WHERE i.type IN ('movie', 'episode') AND i.coveredby IS NULL
			  AND (EXISTS (SELECT 1 FROM com_nalet_katalog_itemversions v
			               WHERE v.item_id = i.id AND v.state = 'complete' AND v.dir IS NOT NULL)
			    OR EXISTS (SELECT 1 FROM com_nalet_katalog_playbackassets a
			               WHERE a.item_id = i.id AND ((a.isprimary AND COALESCE(a.kind, 'primary') = 'primary')
			                                          OR a.kind = 'packaged')))
			UNION ALL
			SELECT x.id FROM com_nalet_katalog_itemextras x
			WHERE x.removedat IS NULL AND x.packagedat IS NOT NULL AND x.packagepath IS NOT NULL
		)
		SELECT count(*),
		       count(*) FILTER (WHERE c.state = 'measured' AND NOT (c.areax = 0 AND c.areay = 0
		                        AND c.areawidth = c.framewidth AND c.areaheight = c.frameheight)),
		       count(*) FILTER (WHERE c.state = 'measured' AND c.areax = 0 AND c.areay = 0
		                        AND c.areawidth = c.framewidth AND c.areaheight = c.frameheight),
		       count(*) FILTER (WHERE c.state = 'unknown'),
		       count(*) FILTER (WHERE c.id IS NULL OR c.state NOT IN ('measured', 'unknown')),
		       sum(c.cpuseconds) FILTER (WHERE c.state IN ('measured', 'unknown') AND c.durationms > 0)
		         / NULLIF(sum(c.durationms) FILTER (WHERE c.state IN ('measured', 'unknown') AND c.cpuseconds IS NOT NULL
		                                            AND c.durationms > 0), 0) * 3600000
		FROM t LEFT JOIN `+store.CurrentPictureAreasView+` c ON c.id = t.id`).
		Scan(&p.Titles, &p.Letterboxed, &p.Full, &p.Unknown, &p.ToMeasure, &p.CPUSecondsPerHour)
	if err != nil {
		return p, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'failed'),
			count(*) FILTER (WHERE claimedat IS NOT NULL), count(*) FILTER (WHERE lastclaimedat > $1),
			max(measuredat)
		FROM `+store.PictureAreasTable, now.Add(-time.Hour)).
		Scan(&p.Failed, &p.InFlight, &p.HandedOutLastHour, &p.LastMeasuredAt); err != nil {
		return p, err
	}
	if p.Settings, err = ReadSettings(ctx, pool); err != nil {
		return p, err
	}
	switch set := p.Settings; {
	case !set.On:
		p.Idle = SettingBackfill + " is off"
	case !set.Window.Contains(now):
		p.Idle = "outside " + SettingWindow + " (" + set.Window.String() + ")"
	case set.Rate == 0:
		p.Idle = SettingRate + " is 0"
	case p.InFlight >= set.Inflight:
		p.Idle = "titles enough are being measured (" + SettingInflight + ")"
	case p.HandedOutLastHour >= set.Rate:
		p.Idle = "the hour's titles were handed out (" + SettingRate + ")"
	case p.ToMeasure == 0:
		p.Idle = "every title has a picture area of what it plays now"
	}
	return p, nil
}
