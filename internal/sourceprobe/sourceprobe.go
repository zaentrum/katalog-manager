// Package sourceprobe keeps what a title's source file was probed as: the
// video codec, the resolution, the duration and the bit rate of its primary
// playback asset (com_nalet_katalog_playbackassets, isprimary, kind primary).
//
// The scanner and POST /api/ingest record a source's size alone: the service
// has no prober. The pipeline's workers probe it, and say what they found:
//   - the transcoder probes the source before it plans, and reports its codec
//     (ffprobe's codec name: h264, hevc) and resolution in the transcode
//     step's details ("skip codec=hevc res=1920x804 ..." when it copies,
//     "profile=... src_codec=h264 res=1920x1080 ..." when it encodes);
//   - the packager probes what it packages, and gives its duration as the
//     manifest's durationMs (the source's for a copy, the transcode's, equal
//     up to a frame, for an encode); a manifest's source block, which the
//     packager's v1 manifest had and v2 dropped, says it all exactly.
//
// Live, each report fills what it says (a present value wins, so a re-probed
// source is kept current). The backfill fills only what is empty, from what
// the catalog has recorded: an item's diagnostics (the ffprobe output a
// diagnostics run stored), its transcode step's details and its packaged
// asset's duration; a bit rate is the size over the duration when nothing
// says it. Running it again changes nothing.
package sourceprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Probe is what is known of a source; nil is unknown.
type Probe struct {
	Codec       *string // ffprobe's codec name, as h264 or hevc
	Resolution  *string // WIDTHxHEIGHT, as 1920x804
	DurationMs  *int64
	BitrateKbps *int64
	SizeBytes   *int64
}

// Empty reports whether p says nothing.
func (p Probe) Empty() bool {
	return p.Codec == nil && p.Resolution == nil && p.DurationMs == nil && p.BitrateKbps == nil && p.SizeBytes == nil
}

var (
	srcCodecRE = regexp.MustCompile(`(?:^|\s)src_codec=([A-Za-z0-9_.-]+)`)
	codecRE    = regexp.MustCompile(`(?:^|\s)codec=([A-Za-z0-9_.-]+)`)
	resRE      = regexp.MustCompile(`(?:^|\s)res=([1-9][0-9]{1,4})x([1-9][0-9]{1,4})(?:\s|$)`)
)

// FromTranscodeDetails reads the source's codec and resolution from the
// details the transcoder reports on its step: src_codec= when it encodes,
// codec= when it skips (copies), and res= in both. Anything else in them is
// ignored, and details that say neither give an empty Probe.
func FromTranscodeDetails(details string) Probe {
	var p Probe
	if m := srcCodecRE.FindStringSubmatch(details); m != nil {
		p.Codec = str(strings.ToLower(m[1]))
	} else if strings.HasPrefix(strings.TrimSpace(details), "skip ") {
		if m := codecRE.FindStringSubmatch(details); m != nil {
			p.Codec = str(strings.ToLower(m[1]))
		}
	}
	if m := resRE.FindStringSubmatch(details); m != nil {
		p.Resolution = str(m[1] + "x" + m[2])
	}
	return p
}

// FromFFprobe reads a source from ffprobe's JSON output (-show_format
// -show_streams), as a diagnostics run stores it: the first video stream that
// is no cover picture, and the format's duration, bit rate and size. Output
// it cannot read gives an empty Probe.
func FromFFprobe(out string) Probe {
	var raw struct {
		Streams []struct {
			CodecType   string         `json:"codec_type"`
			CodecName   string         `json:"codec_name"`
			Width       int            `json:"width"`
			Height      int            `json:"height"`
			Disposition map[string]int `json:"disposition"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
			BitRate  string `json:"bit_rate"`
			Size     string `json:"size"`
		} `json:"format"`
	}
	var p Probe
	if strings.TrimSpace(out) == "" || json.Unmarshal([]byte(out), &raw) != nil {
		return p
	}
	for _, s := range raw.Streams {
		if s.CodecType != "video" || s.Disposition["attached_pic"] == 1 {
			continue
		}
		if s.CodecName != "" {
			p.Codec = str(strings.ToLower(s.CodecName))
		}
		if s.Width > 0 && s.Height > 0 {
			p.Resolution = str(strconv.Itoa(s.Width) + "x" + strconv.Itoa(s.Height))
		}
		break
	}
	if d, err := strconv.ParseFloat(strings.TrimSpace(raw.Format.Duration), 64); err == nil && d > 0 {
		p.DurationMs = num(int64(d*1000 + 0.5))
	}
	if b, err := strconv.ParseInt(strings.TrimSpace(raw.Format.BitRate), 10, 64); err == nil && b > 0 {
		p.BitrateKbps = num(b / 1000)
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(raw.Format.Size), 10, 64); err == nil && n > 0 {
		p.SizeBytes = num(n)
	}
	return p
}

// Fill writes what p says of item's source over what it held (a present
// value wins); a bit rate it does not say is the size over the duration,
// when the source has neither yet. It reports whether the item has a source
// asset.
func Fill(ctx context.Context, pool *pgxpool.Pool, itemID string, p Probe) (bool, error) {
	if p.Empty() {
		return false, nil
	}
	tag, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET
			codec       = COALESCE($2, codec),
			resolution  = COALESCE($3, resolution),
			durationms  = COALESCE($4, durationms),
			sizebytes   = COALESCE($6, sizebytes),
			bitratekbps = COALESCE($5::bigint, bitratekbps,
				CASE WHEN COALESCE($4, durationms) > 0 AND COALESCE($6, sizebytes) > 0
				     THEN COALESCE($6, sizebytes) * 8 / COALESCE($4, durationms) END)
		WHERE `+source+` AND item_id = $1`,
		itemID, p.Codec, p.Resolution, p.DurationMs, p.BitrateKbps, p.SizeBytes)
	if err != nil {
		return false, fmt.Errorf("fill the source of %s: %w", itemID, err)
	}
	return tag.RowsAffected() > 0, nil
}

// FillEmpty is Fill for what item's source does not hold yet: a value it has
// stays (an estimate never replaces what a probe said).
func FillEmpty(ctx context.Context, pool *pgxpool.Pool, itemID string, p Probe) error {
	if p.Empty() {
		return nil
	}
	_, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET
			codec       = COALESCE(NULLIF(codec, ''), $2),
			resolution  = COALESCE(NULLIF(resolution, ''), $3),
			durationms  = COALESCE(NULLIF(durationms, 0), $4),
			sizebytes   = COALESCE(NULLIF(sizebytes, 0), $6),
			bitratekbps = COALESCE(NULLIF(bitratekbps, 0), $5::bigint,
				CASE WHEN COALESCE(NULLIF(durationms, 0), $4) > 0 AND COALESCE(NULLIF(sizebytes, 0), $6) > 0
				     THEN COALESCE(NULLIF(sizebytes, 0), $6) * 8 / COALESCE(NULLIF(durationms, 0), $4) END)
		WHERE `+source+` AND item_id = $1`,
		itemID, p.Codec, p.Resolution, p.DurationMs, p.BitrateKbps, p.SizeBytes)
	if err != nil {
		return fmt.Errorf("fill the source of %s: %w", itemID, err)
	}
	return nil
}

// source picks an item's source asset: its primary playback asset that is
// no package.
const source = `isprimary = true AND COALESCE(kind, 'primary') = 'primary'`

func str(s string) *string { return &s }
func num(n int64) *int64   { return &n }
