package captions

import (
	"context"
	"time"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// Progress is how far the closed captions are: of the originals on disk
// (present or superseded), how many have theirs read and how many of them
// carry closed captions, how many wait, and how the backfill stands.
type Progress struct {
	// Originals is how many originals are on disk; Read those whose
	// captions are read in full, every window of the read decoded, or that
	// a read found off the closed lists or with a stream of captions of
	// their own (WithCaptions of them carry closed captions);
	// LengthMismatch those a read found of another length than their
	// record's, kept for good until their file or their record is another
	// (LengthMismatches their source ids), a finding of its own and none of
	// Read; AwaitingRecord those a read found its record leaves not known
	// (it says no length, its records give no plan of its picture, or its
	// picture's captions need its packets' signature), kept until the record
	// is written again, which hands them
	// out again, none of Read nor of ToRead; ToRead the rest.
	Originals, Read, WithCaptions, LengthMismatch, AwaitingRecord, ToRead int
	LengthMismatches                                                      []string
	// Failed are those whose last read failed or had a window that
	// decoded no frame (each waits its backoff), InFlight those handed out
	// now.
	Failed, InFlight int
	// HandedOutLastHour is how many hand-outs the backfill made in the last
	// hour (an original handed out again counts again).
	HandedOutLastHour int
	LastReadAt        *time.Time
	// Settings are the backfill's settings, and Idle why it hands out
	// nothing now ("" when it may).
	Settings Settings
	Idle     string
}

// Progress reads how far the closed captions are.
func (s *Service) Progress(ctx context.Context) (Progress, error) {
	var p Progress
	if err := s.ready(ctx); err != nil {
		return p, err
	}
	pool := s.st.Pool()
	now := s.now().UTC()
	full := readUsable("c", "s")
	mismatch := full + ` AND c.finding IS NOT DISTINCT FROM '` + library.FindingLengthMismatch + `'`
	awaiting := full + ` AND c.finding IN ('` + library.FindingNoLength + `', '` + library.FindingNoPlan + `', '` +
		library.FindingUnscanned + `')`
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE `+full+` AND c.finding IS NULL),
			count(*) FILTER (WHERE `+full+` AND c.finding IS NULL AND (c.captions OR COALESCE(c.captionstreams, 0) > 0)),
			count(*) FILTER (WHERE `+mismatch+`),
			COALESCE(array_agg(s.id ORDER BY s.id) FILTER (WHERE `+mismatch+`), '{}'),
			count(*) FILTER (WHERE `+awaiting+`),
			count(*) FILTER (WHERE c.source_id IS NOT NULL AND c.state <> 'pending' AND NOT `+full+` AND c.claimedat IS NULL)
		FROM com_nalet_katalog_itemsources s LEFT JOIN `+store.SourceCaptionsTable+` c ON c.source_id = s.id
		WHERE s.state IN ('present', 'superseded') AND COALESCE(s.arrivalpath, '') <> ''`).
		Scan(&p.Originals, &p.Read, &p.WithCaptions, &p.LengthMismatch, &p.LengthMismatches, &p.AwaitingRecord,
			&p.Failed); err != nil {
		return p, err
	}
	p.ToRead = p.Originals - p.Read - p.LengthMismatch - p.AwaitingRecord
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE claimedat IS NOT NULL),
			(SELECT count(*) FROM `+store.CaptionHandoutsTable+` WHERE handedoutat > $1), max(readat)
		FROM `+store.SourceCaptionsTable, now.Add(-time.Hour)).
		Scan(&p.InFlight, &p.HandedOutLastHour, &p.LastReadAt); err != nil {
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
		p.Idle = "originals enough are being read (" + SettingInflight + ")"
	case p.HandedOutLastHour >= set.Rate:
		p.Idle = "the hour's hand-outs were made (" + SettingRate + ")"
	case p.ToRead == 0:
		p.Idle = "every original on disk has its closed captions read"
	}
	return p, nil
}
