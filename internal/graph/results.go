package graph

import (
	graphql "github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/sourceprobe"
	"github.com/zaentrum/katalog-manager/internal/sourcetracks"
)

// Plain value-backed resolvers for action results. Fields are exported on the
// backing structs and exposed through methods to match the SDL exactly.

type SearchItem struct {
	ID     string
	Type   string
	Title  string
	Year   *int32
	Rating *float64
	Score  *float64
}

type searchItemResolver struct{ m SearchItem }

func (r *searchItemResolver) ID() graphql.ID   { return gid(r.m.ID) }
func (r *searchItemResolver) Type() string     { return r.m.Type }
func (r *searchItemResolver) Title() string    { return r.m.Title }
func (r *searchItemResolver) Year() *int32     { return r.m.Year }
func (r *searchItemResolver) Rating() *float64 { return r.m.Rating }
func (r *searchItemResolver) Score() *float64  { return r.m.Score }

type SearchResult struct {
	Items  []SearchItem
	Total  int32
	Limit  int32
	Offset int32
}

type searchResultResolver struct{ m SearchResult }

func (r *searchResultResolver) Items() []*searchItemResolver {
	out := make([]*searchItemResolver, 0, len(r.m.Items))
	for i := range r.m.Items {
		out = append(out, &searchItemResolver{m: r.m.Items[i]})
	}
	return out
}
func (r *searchResultResolver) Total() int32  { return r.m.Total }
func (r *searchResultResolver) Limit() int32  { return r.m.Limit }
func (r *searchResultResolver) Offset() int32 { return r.m.Offset }

type enrichStatusResolver struct{ tmdbEnabled bool }

func (r *enrichStatusResolver) TmdbEnabled() bool { return r.tmdbEnabled }

type EnrichResult struct {
	ItemID  string
	Status  string
	Message *string
}

type enrichResultResolver struct{ m EnrichResult }

func (r *enrichResultResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *enrichResultResolver) Status() string     { return r.m.Status }
func (r *enrichResultResolver) Message() *string   { return r.m.Message }

type EnrichPendingResult struct {
	Queued int32
	Type   *string
}

type enrichPendingResultResolver struct{ m EnrichPendingResult }

func (r *enrichPendingResultResolver) Queued() int32 { return r.m.Queued }
func (r *enrichPendingResultResolver) Type() *string { return r.m.Type }

type ratingsBackfillResultResolver struct{ m RatingsBackfillResult }

func (r *ratingsBackfillResultResolver) TitlesRead() int32    { return r.m.TitlesRead }
func (r *ratingsBackfillResultResolver) TitlesRated() int32   { return r.m.TitlesRated }
func (r *ratingsBackfillResultResolver) TitlesUnrated() int32 { return r.m.TitlesUnrated }
func (r *ratingsBackfillResultResolver) TitlesFailed() int32  { return r.m.TitlesFailed }
func (r *ratingsBackfillResultResolver) Countries() []string {
	if r.m.Countries == nil {
		return []string{}
	}
	return r.m.Countries
}
func (r *ratingsBackfillResultResolver) StartedAt() graphql.Time {
	return graphql.Time{Time: r.m.StartedAt}
}
func (r *ratingsBackfillResultResolver) FinishedAt() graphql.Time {
	return graphql.Time{Time: r.m.FinishedAt}
}

type peopleRefreshResultResolver struct{ m PeopleRefreshResult }

func (r *peopleRefreshResultResolver) TitlesRead() int32          { return r.m.TitlesRead }
func (r *peopleRefreshResultResolver) TitlesFailed() int32        { return r.m.TitlesFailed }
func (r *peopleRefreshResultResolver) TitlesLocked() int32        { return r.m.TitlesLocked }
func (r *peopleRefreshResultResolver) PeopleMatched() int32       { return r.m.PeopleMatched }
func (r *peopleRefreshResultResolver) PeopleCreated() int32       { return r.m.PeopleCreated }
func (r *peopleRefreshResultResolver) CreditsAdded() int32        { return r.m.CreditsAdded }
func (r *peopleRefreshResultResolver) CreditsUpdated() int32      { return r.m.CreditsUpdated }
func (r *peopleRefreshResultResolver) CreditsDropped() int32      { return r.m.CreditsDropped }
func (r *peopleRefreshResultResolver) CreditsRelinked() int32     { return r.m.CreditsRelinked }
func (r *peopleRefreshResultResolver) PeopleDeleted() int32       { return r.m.PeopleDeleted }
func (r *peopleRefreshResultResolver) PeopleFetched() int32       { return r.m.PeopleFetched }
func (r *peopleRefreshResultResolver) PeopleLocked() int32        { return r.m.PeopleLocked }
func (r *peopleRefreshResultResolver) PeopleNotFound() int32      { return r.m.PeopleNotFound }
func (r *peopleRefreshResultResolver) PeopleFailed() int32        { return r.m.PeopleFailed }
func (r *peopleRefreshResultResolver) PeopleWithoutTmdbID() int32 { return r.m.PeopleWithoutTmdbID }
func (r *peopleRefreshResultResolver) StartedAt() graphql.Time {
	return graphql.Time{Time: r.m.StartedAt}
}
func (r *peopleRefreshResultResolver) FinishedAt() graphql.Time {
	return graphql.Time{Time: r.m.FinishedAt}
}

type backfillResultResolver struct{ artworkData, artwork int32 }

func (r *backfillResultResolver) ArtworkData() int32 { return r.artworkData }
func (r *backfillResultResolver) Artwork() int32     { return r.artwork }

type sourceProbeBackfillResultResolver struct{ m sourceprobe.BackfillResult }

func (r *sourceProbeBackfillResultResolver) Assets() int32      { return r.m.Assets }
func (r *sourceProbeBackfillResultResolver) Filled() int32      { return r.m.Filled }
func (r *sourceProbeBackfillResultResolver) Codecs() int32      { return r.m.Codecs }
func (r *sourceProbeBackfillResultResolver) Resolutions() int32 { return r.m.Resolutions }
func (r *sourceProbeBackfillResultResolver) Durations() int32   { return r.m.Durations }
func (r *sourceProbeBackfillResultResolver) Bitrates() int32    { return r.m.Bitrates }
func (r *sourceProbeBackfillResultResolver) Unknown() int32     { return r.m.Unknown }

type sourceTrackBackfillResultResolver struct{ m sourcetracks.Result }

func (r *sourceTrackBackfillResultResolver) Titles() int32         { return r.m.Titles }
func (r *sourceTrackBackfillResultResolver) Recorded() int32       { return r.m.Recorded }
func (r *sourceTrackBackfillResultResolver) AudioTracks() int32    { return r.m.AudioTracks }
func (r *sourceTrackBackfillResultResolver) SubtitleTracks() int32 { return r.m.SubtitleTracks }
func (r *sourceTrackBackfillResultResolver) Failed() int32         { return r.m.Failed }
func (r *sourceTrackBackfillResultResolver) Errors() []string {
	if r.m.Errors == nil {
		return []string{}
	}
	return r.m.Errors
}

type RetryResult struct {
	Reset int32
	Type  *string
}

type retryResultResolver struct{ m RetryResult }

func (r *retryResultResolver) Reset() int32  { return r.m.Reset }
func (r *retryResultResolver) Type() *string { return r.m.Type }

type PackageResult struct {
	Status           *string
	AlreadyActive    *bool
	Message          *string
	EpisodesEnqueued *int32
	EpisodesTotal    *int32
}

type deleteItemResultResolver struct{ m RemoveResult }

func (r *deleteItemResultResolver) Deleted() bool          { return r.m.Deleted }
func (r *deleteItemResultResolver) ItemsRemoved() int32    { return r.m.ItemsRemoved }
func (r *deleteItemResultResolver) FilesRemoved() int32    { return r.m.FilesRemoved }
func (r *deleteItemResultResolver) PackagesRemoved() int32 { return r.m.PackagesRemoved }
func (r *deleteItemResultResolver) Errors() []string {
	if r.m.Errors == nil {
		return []string{}
	}
	return r.m.Errors
}

type packageResultResolver struct{ m PackageResult }

func (r *packageResultResolver) Status() *string          { return r.m.Status }
func (r *packageResultResolver) AlreadyActive() *bool     { return r.m.AlreadyActive }
func (r *packageResultResolver) Message() *string         { return r.m.Message }
func (r *packageResultResolver) EpisodesEnqueued() *int32 { return r.m.EpisodesEnqueued }
func (r *packageResultResolver) EpisodesTotal() *int32    { return r.m.EpisodesTotal }

type ValidateFinding struct {
	Code    string
	Message string
}

type validateFindingResolver struct{ m ValidateFinding }

func (r *validateFindingResolver) Code() string    { return r.m.Code }
func (r *validateFindingResolver) Message() string { return r.m.Message }

type ValidateResult struct {
	Code        string
	Message     string
	SourcePath  *string
	PackagePath *string
	Findings    []ValidateFinding
}

type validateResultResolver struct{ m ValidateResult }

func (r *validateResultResolver) Code() string         { return r.m.Code }
func (r *validateResultResolver) Message() string      { return r.m.Message }
func (r *validateResultResolver) SourcePath() *string  { return r.m.SourcePath }
func (r *validateResultResolver) PackagePath() *string { return r.m.PackagePath }
func (r *validateResultResolver) Findings() []*validateFindingResolver {
	out := make([]*validateFindingResolver, 0, len(r.m.Findings))
	for i := range r.m.Findings {
		out = append(out, &validateFindingResolver{m: r.m.Findings[i]})
	}
	return out
}
