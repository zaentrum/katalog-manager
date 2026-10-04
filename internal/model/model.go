// Package model holds the pure-Go domain types for the katalog catalog.
// These mirror the lowercase Postgres tables com_nalet_katalog_* (see SPEC).
// Types are storage-friendly (time.Time, *string, int32/int64); the graph
// package wraps them into GraphQL resolvers.
package model

import (
	"regexp"
	"time"
)

// CreditRoles are the roles the catalog knows a title to credit people in, in
// the order a title lists its credits; TMDB's credits give each of them. A
// role is any lowercase word (ValidRole): a credit in another role lists after
// these, by role.
var CreditRoles = []string{"actor", "creator", "director", "writer", "producer", "composer", "cinematographer", "editor"}

var roleRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// ValidRole reports whether s is a role: a lowercase letter, then up to 39
// lowercase letters, digits and hyphens.
func ValidRole(s string) bool { return roleRE.MatchString(s) }

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
	// What migration 033 keeps of its retries (0 and nil without it).
	Failures     int32
	LastError    *string
	NextRetryAt  *time.Time
	DispatchedAt *time.Time
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

// Person is someone a title credits. Everything but ID and Name comes with
// db/migrations/030_people.sql; on a catalog without it the rest is empty.
// LockedFields and FieldOrigins name fields as the library record's
// person.json does (name, sortName, alsoKnownAs, birthDate, deathDate,
// birthPlace, biography, externalIds, images, knownForDepartment).
type Person struct {
	ID                 string
	Name               string
	SortName           *string
	AlsoKnownAs        []string
	BirthDate          *string // YYYY-MM-DD
	DeathDate          *string // YYYY-MM-DD
	BirthPlace         *string
	Biography          map[string]string // language (primary subtag) → text
	TmdbPersonID       *string
	ImdbID             *string
	KnownForDepartment *string
	MetadataLocked     bool              // every field is left alone by automation
	LockedFields       []string          // these fields are
	FieldOrigins       map[string]string // field → tmdb | manual | …
	TmdbFetchedAt      *time.Time        // TMDB last read for the person
	TmdbChangedAt      *string           // YYYY-MM-DD: the last day TMDB's change list named them
	CreatedAt          *time.Time
	ModifiedAt         *time.Time // their data last changed
}

// ReferenceSync is one TMDB change list's cursor (db/migrations/030): the day
// its next run reads from, and what its last run did.
type ReferenceSync struct {
	Kind             string // person | movie | tv
	Cursor           string // YYYY-MM-DD
	LastRunAt        *time.Time
	LastRunChanges   *int32 // ids the change list named
	LastRunMatched   *int32 // of them, held by the catalog
	LastRunRefreshed *int32
	LastRunSkipped   *int32 // locked, or TMDB no longer knows them
	LastRunFailed    *int32
	LastRunError     *string
}

type ItemGenre struct {
	ID      string
	ItemID  string
	GenreID string
}

// ItemPerson is a credit: a person in a role on a title, once per person and
// role. The rest is what TMDB says of it (db/migrations/032), nil when unknown.
type ItemPerson struct {
	ID           string
	ItemID       string
	PersonID     string
	Role         string  // a role (ValidRole), e.g. one of CreditRoles
	Job          *string // the person's jobs in the role, joined with ", "
	Character    *string // whom an actor plays
	Order        *int32  // the credit's place in its role, 0 first
	EpisodeCount *int32  // the episodes of a series the person is credited in, in the role
}

// PersonCredit is a credit seen from the person: the credit, the title that
// gives it, and that title's parent (an episode's series), read together.
type PersonCredit struct {
	ItemPerson
	Item   Item
	Parent *Item // nil when the title has none, or its parent is not in the catalog
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

// DeletedItem mirrors db/migrations/029_deleted_items.sql: an item the catalog
// deleted, or a person (Type "person") it deleted because no title credits them
// any more, as of its latest deletion. If an item or person with the same ID
// exists again, it was re-created after this deletion, and the one that exists
// wins.
type DeletedItem struct {
	ID        string
	Type      string    // the item's type, or "person"
	Title     string    // the title it had when it was deleted
	DeletedAt time.Time // UTC
	DeletedBy string    // a principal's subject, or the service that deleted
	Reason    *string
}
