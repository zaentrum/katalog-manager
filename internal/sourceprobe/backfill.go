package sourceprobe

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BackfillResult says what a Backfill did: the source assets it looked at
// (those missing any of the four), those it filled anything of, by column,
// and those it found nothing for.
type BackfillResult struct {
	Assets, Filled, Codecs, Resolutions, Durations, Bitrates, Unknown int32
}

// backfillBatch is the most source assets a Backfill reads and writes at once.
const backfillBatch = 500

// Backfill fills the empty probe columns of every source asset from what the
// catalog has recorded of it, and only those: its item's diagnostics (the
// ffprobe output a diagnostics run stored), then its transcode step's details
// (the codec and resolution), then its packaged asset's duration; a bit rate
// is the size over the duration when nothing says it. A value a source has
// stays. Running it again changes nothing.
func Backfill(ctx context.Context, pool *pgxpool.Pool) (BackfillResult, error) {
	var res BackfillResult
	after := ""
	for {
		rows, err := pool.Query(ctx, `SELECT a.id, NULLIF(a.codec, ''), NULLIF(a.resolution, ''),
				NULLIF(a.durationms, 0), NULLIF(a.bitratekbps, 0), NULLIF(a.sizebytes, 0),
				COALESCE((SELECT s.details FROM com_nalet_katalog_itemprocessingsteps s
				          WHERE s.item_id = a.item_id AND s.step = 'transcode'), ''),
				(SELECT p.durationms FROM com_nalet_katalog_playbackassets p
				 WHERE p.item_id = a.item_id AND p.kind = 'packaged' AND p.durationms > 0 LIMIT 1),
				COALESCE((SELECT d.ffprobedata FROM com_nalet_katalog_itemdiagnostics d
				          WHERE d.item_id = a.item_id AND d.ffprobedata IS NOT NULL
				          ORDER BY d.generatedat DESC NULLS LAST LIMIT 1), '')
			FROM com_nalet_katalog_playbackassets a
			WHERE a.isprimary = true AND COALESCE(a.kind, 'primary') = 'primary' AND a.id > $1
			  AND (NULLIF(a.codec, '') IS NULL OR NULLIF(a.resolution, '') IS NULL
			       OR NULLIF(a.durationms, 0) IS NULL OR NULLIF(a.bitratekbps, 0) IS NULL)
			ORDER BY a.id
			LIMIT $2`, after, backfillBatch)
		if err != nil {
			return res, fmt.Errorf("read the sources to fill: %w", err)
		}
		var ids []string
		var durations, bitrates []*int64
		var codecSet, resSet []*string
		n := 0
		for rows.Next() {
			var id, details, ffprobe string
			var codec, resolution *string
			var dur, kbps, size, packaged *int64
			if err := rows.Scan(&id, &codec, &resolution, &dur, &kbps, &size, &details, &packaged, &ffprobe); err != nil {
				rows.Close()
				return res, err
			}
			n++
			after = id
			res.Assets++
			known := FromFFprobe(ffprobe)
			fromDetails := FromTranscodeDetails(details)
			next := Probe{Codec: first(known.Codec, fromDetails.Codec), Resolution: first(known.Resolution, fromDetails.Resolution),
				DurationMs: firstNum(known.DurationMs, packaged), BitrateKbps: known.BitrateKbps}
			d := firstNum(dur, next.DurationMs)
			if kbps == nil && next.BitrateKbps == nil && d != nil && *d > 0 && size != nil && *size > 0 {
				next.BitrateKbps = num(*size * 8 / *d)
			}
			gained := false
			if codec == nil && next.Codec != nil {
				res.Codecs++
				gained = true
			} else {
				next.Codec = nil
			}
			if resolution == nil && next.Resolution != nil {
				res.Resolutions++
				gained = true
			} else {
				next.Resolution = nil
			}
			if dur == nil && next.DurationMs != nil {
				res.Durations++
				gained = true
			} else {
				next.DurationMs = nil
			}
			if kbps == nil && next.BitrateKbps != nil {
				res.Bitrates++
				gained = true
			} else {
				next.BitrateKbps = nil
			}
			if !gained {
				res.Unknown++
				continue
			}
			res.Filled++
			ids = append(ids, id)
			codecSet, resSet = append(codecSet, next.Codec), append(resSet, next.Resolution)
			durations, bitrates = append(durations, next.DurationMs), append(bitrates, next.BitrateKbps)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return res, err
		}
		if len(ids) > 0 {
			if _, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets a SET
					codec       = COALESCE(NULLIF(a.codec, ''), v.codec),
					resolution  = COALESCE(NULLIF(a.resolution, ''), v.resolution),
					durationms  = COALESCE(NULLIF(a.durationms, 0), v.durationms),
					bitratekbps = COALESCE(NULLIF(a.bitratekbps, 0), v.bitratekbps)
				FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::bigint[])
				     AS v(id, codec, resolution, durationms, bitratekbps)
				WHERE a.id = v.id`, ids, codecSet, resSet, durations, bitrates); err != nil {
				return res, fmt.Errorf("fill the sources: %w", err)
			}
		}
		if n < backfillBatch {
			return res, nil
		}
	}
}

func first(a, b *string) *string {
	if a != nil {
		return a
	}
	return b
}

func firstNum(a, b *int64) *int64 {
	if a != nil {
		return a
	}
	return b
}
