// Package model holds the pure-Go domain types for the katalog catalog.
// These mirror the lowercase Postgres tables com_nalet_katalog_* (see SPEC).
// Types are storage-friendly (time.Time, *string, int32/int64); the graph
// package wraps them into GraphQL resolvers.
package model

import "time"

// Vocabularies ported verbatim from the CAP service (SPEC §3).
var (
	// Steps in the processing audit trail.
	Steps = []string{"scan", "tmdb", "tidb", "chapter", "chromaprint", "blackframe", "silence", "subtitle", "transcode", "package"}
	// AnalyzerSteps are the per-file analyzer passes (subset of Steps).
	AnalyzerSteps = []string{"chapter", "chromaprint", "blackframe", "silence", "subtitle", "tidb"}
	// Statuses a step can be in.
	Statuses = []string{"pending", "in_progress", "done", "failed", "skipped", "not_applicable"}
	// Passes the analyzer claim endpoint accepts.
	Passes = []string{"per_file", "tidb_first", "transcoder", "packager"}
	// SegmentKinds aligned with the TIDB vocabulary.
	SegmentKinds = []string{"intro", "recap", "credits", "preview"}
	// SegmentSources track provenance of a media segment.
	SegmentSources = []string{"tidb", "chapter", "subtitle", "silence", "blackframe", "chromaprint", "whisper", "transnet", "manual"}
)

// Item is the canonical media row (movie/series/season/episode/album/track/book).
type Item struct {
	ID            string
	CreatedAt     *time.Time
	CreatedBy     *string
	ModifiedAt    *time.Time
	ModifiedBy    *string
	Type          string
	Title         string
	SortTitle     *string
	Year          *int32
	Description   *string
	Rating        *float64
	DurationMs    *int64
	ParentID      *string
	SeasonNumber  *int32
	EpisodeNumber *int32
	Tagline       *string
}

// Computed columns the katalogservice_* views add (kept alongside Item when a
// view row is read). Pointers are nil when the row came from a base-table read.
type ItemComputed struct {
	PosterURL   *string
	BackdropURL *string
	RuntimeMin  *int64
	YearText    *string
	IsPackaged  *bool
	HasIntro    *bool
	HasCredits  *bool
	HasRecap    *bool
}

// ItemRow is a view row: base Item plus the computed columns.
type ItemRow struct {
	Item
	ItemComputed
}

type PlaybackAsset struct {
	ID                 string
	ItemID             string
	Path               string
	Codec              *string
	Resolution         *string
	BitrateKbps        *int32
	SizeBytes          *int64
	Hash               *string
	IsPrimary          *bool
	Kind               *string
	AudioCodec         *string
	AudioLanguage      *string
	AudioChannels      *int32
	AudioBitrateKbps   *int32
	AudioTrackCount    *int32
	SubtitleTrackCount *int32
	DurationMs         *int64
	SizeMB             *int64 // view-computed
}

type SubtitleAsset struct {
	ID        string
	ItemID    string
	Path      string
	Format    *string
	Lang      *string
	Label     *string
	IsDefault *bool
}

type MediaSegment struct {
	ID         string
	CreatedAt  *time.Time
	ModifiedAt *time.Time
	ItemID     string
	Kind       string
	StartMs    int64
	EndMs      int64
	Source     string
	Confidence *float64
	Label      *string
}

type ItemChapter struct {
	ID         string
	CreatedAt  *time.Time
	ModifiedAt *time.Time
	ItemID     string
	StartMs    int64
	EndMs      int64
	Title      *string
	Ordinal    *int32
}

type ItemProcessingStep struct {
	ID                string
	CreatedAt         *time.Time
	ModifiedAt        *time.Time
	ItemID            string
	Step              string
	Status            string
	StartedAt         *time.Time
	FinishedAt        *time.Time
	Attempts          *int32
	Error             *string
	Details           *string
	StatusCriticality *int32 // view-computed
}

type ItemOverallStatus struct {
	ItemID             string
	OverallStatus      *string
	DoneCount          *int64
	PendingCount       *int64
	FailedCount        *int64
	InProgressCount    *int64
	NotApplicableCount *int64
	TotalSteps         *int64
	LastStepFinishedAt *time.Time
}

type Genre struct {
	ID   string
	Name string
}

type Person struct {
	ID   string
	Name string
}

type ItemGenre struct {
	ID      string
	ItemID  string
	GenreID string
}

type ItemPerson struct {
	ID       string
	ItemID   string
	PersonID string
	Role     string
}

type ItemTag struct {
	ID     string
	ItemID string
	Tag    string
}

type ItemArtwork struct {
	ID     string
	ItemID string
	Kind   string
	URL    string
}

type ItemArtworkData struct {
	ID          string
	ItemID      string
	Kind        string
	ContentType string
	Bytes       []byte
	FetchedAt   *time.Time
}

type ItemExternalID struct {
	ID         string
	ItemID     string
	Source     string
	ExternalID string
}

type ItemTrailerLink struct {
	ID           string
	CreatedAt    *time.Time
	ModifiedAt   *time.Time
	ItemID       string
	Source       string
	Site         *string
	ExternalID   *string
	URL          string
	Title        *string
	DurationSec  *int32
	PublishedAt  *time.Time
	DownloadedAt *time.Time
	LocalPath    *string
}

type ItemDiagnostics struct {
	ID            string
	ItemID        string
	GeneratedAt   *time.Time
	SourcePath    *string
	SourceSize    *int64
	SourceMtime   *time.Time
	FfprobeData   *string
	FolderListing *string
	Notes         *string
}

type ScanJob struct {
	ID            string
	Source        string
	Status        string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	ErrorMessage  *string
	FilesSeen     *int32
	ItemsInserted *int32
	ItemsUpdated  *int32
}

type EnrichmentJob struct {
	ID              string
	Status          string
	StartedAt       *time.Time
	FinishedAt      *time.Time
	ErrorMessage    *string
	ItemsConsidered *int32
	ItemsEnriched   *int32
	ItemsFailed     *int32
}

type EnrichmentStatusCode struct {
	Code string
	Name *string
}

type Setting struct {
	ID          string
	CreatedAt   *time.Time
	ModifiedAt  *time.Time
	Key         string
	ValueText   string
	ValueType   string
	Description *string
}

type DownloadJob struct {
	ID              string
	CreatedAt       *time.Time
	ModifiedAt      *time.Time
	Adapter         string
	ClientJobID     string
	Title           *string
	WantedItemID    *string
	State           string
	ProgressPct     *float64
	DownloadedBytes *int64
	SizeBytes       *int64
	SpeedBps        *int64
	EtaSec          *int32
	Files           *string
	ErrorMessage    *string
	StartedAt       *time.Time
	CompletedAt     *time.Time
	LastEventAt     *time.Time
	StateCriticality *int32 // view-computed
}

// TrailerJob mirrors db/migrations/020_trailerjobs.sql (absent from the live
// dump; recreated by db/migrations/028).
type TrailerJob struct {
	ID            string
	CreatedAt     *time.Time
	ModifiedAt    *time.Time
	ItemID        string
	TrailerLinkID *string
	SourceURL     string
	PackageID     *string
	DownloadID    *string
	State         string
	Attempts      *int32
	StartedAt     *time.Time
	FinishedAt    *time.Time
	BytesDone     *int64
	BytesTotal    *int64
	Message       *string
	FinalPath     *string
}
