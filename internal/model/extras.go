package model

import (
	"slices"
	"time"
)

// ExtraKinds are the kinds of an extra (db/migrations/039_item_extras.sql), as
// the library's extra record names them: a short piece about the work, footage
// of its production, a documentary of it, a scene cut from it, cast or crew
// talking about it, a trailer or a teaser that is a file of its own, outtakes,
// a short work released with it, and anything else.
var ExtraKinds = []string{"featurette", "behind-the-scenes", "making-of", "deleted-scene", "interview", "trailer",
	"teaser", "gag-reel", "short", "other"}

// ValidExtraKind reports whether kind is one of ExtraKinds.
func ValidExtraKind(kind string) bool { return slices.Contains(ExtraKinds, kind) }

// ExtraKindTitle is what an extra of kind is called when nothing else names
// it: "Trailer", "Behind the Scenes".
func ExtraKindTitle(kind string) string {
	switch kind {
	case "featurette":
		return "Featurette"
	case "behind-the-scenes":
		return "Behind the Scenes"
	case "making-of":
		return "Making Of"
	case "deleted-scene":
		return "Deleted Scene"
	case "interview":
		return "Interview"
	case "trailer":
		return "Trailer"
	case "teaser":
		return "Teaser"
	case "gag-reel":
		return "Gag Reel"
	case "short":
		return "Short"
	}
	return "Extra"
}

// Where an extra's packaging stands: waiting to be sent (pending), sent to
// the transcoder (queued), encoded (transcoding, transcoded), packaged
// (packaging, ready); failed when no attempt is left, and missing when the
// scanner found its file gone.
const (
	ExtraPending     = "pending"
	ExtraQueued      = "queued"
	ExtraTranscoding = "transcoding"
	ExtraTranscoded  = "transcoded"
	ExtraPackaging   = "packaging"
	ExtraReady       = "ready"
	ExtraFailed      = "failed"
	ExtraMissing     = "missing"
)

// How an extra was taken in: by an operator (POST /api/extras, addExtra), by
// the scanner's convention, or from its library record.
const (
	ExtraByAPI     = "api"
	ExtraByScanner = "scanner"
	ExtraByLibrary = "library"
)

// Extra is a row of com_nalet_katalog_itemextras: bonus material of a movie
// or a series, packaged on its own. Its ID is the extraId.
type Extra struct {
	ID               string
	ItemID           string
	Kind             string
	Title            string // as taken in; Label is what a viewer is shown instead
	Language         *string
	SeasonNumber     *int32
	SourcePath       *string
	SourceSize       *int64
	SourceQH1        *string // sha256:<hex> of its first and last 64 KiB and its size
	RecordPath       *string
	RegisteredBy     string
	SortOrder        *int32
	Hidden           bool
	Label            *string
	State            string
	Error            *string
	Attempts         int32 // the runs of its packaging started
	Failures         int32 // its failures in a row
	NextRetryAt      *time.Time
	DispatchedAt     *time.Time // when its trigger was last sent
	HeartbeatAt      *time.Time // its worker's last word
	PackagePath      *string
	PackagedAt       *time.Time
	DurationMs       *int64
	VideoCodec       *string // its package's top rendition's
	Width            *int32
	Height           *int32
	PeakBandwidthBps *int64
	PackageSizeBytes *int64
	RemovedAt        *time.Time
	RemovedBy        *string
	RemovalReason    *string
	CreatedAt        time.Time
	CreatedBy        *string
	ModifiedAt       time.Time
	ModifiedBy       *string
}

// Playable reports whether the extra plays: it is packaged and not removed,
// not hidden, and its file is not missing.
func (e *Extra) Playable() bool {
	return e.PackagedAt != nil && e.RemovedAt == nil && !e.Hidden && e.State != ExtraMissing
}
