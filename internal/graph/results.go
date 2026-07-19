package graph

import graphql "github.com/graph-gophers/graphql-go"

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
func (r *searchItemResolver) Year() *int32      { return r.m.Year }
func (r *searchItemResolver) Rating() *float64  { return r.m.Rating }
func (r *searchItemResolver) Score() *float64   { return r.m.Score }

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
func (r *enrichResultResolver) Message() *string    { return r.m.Message }

type EnrichPendingResult struct {
	Queued int32
	Type   *string
}

type enrichPendingResultResolver struct{ m EnrichPendingResult }

func (r *enrichPendingResultResolver) Queued() int32 { return r.m.Queued }
func (r *enrichPendingResultResolver) Type() *string  { return r.m.Type }

type backfillResultResolver struct{ artworkData, artwork int32 }

func (r *backfillResultResolver) ArtworkData() int32 { return r.artworkData }
func (r *backfillResultResolver) Artwork() int32     { return r.artwork }

type RetryResult struct {
	Reset int32
	Type  *string
}

type retryResultResolver struct{ m RetryResult }

func (r *retryResultResolver) Reset() int32 { return r.m.Reset }
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

func (r *packageResultResolver) Status() *string         { return r.m.Status }
func (r *packageResultResolver) AlreadyActive() *bool    { return r.m.AlreadyActive }
func (r *packageResultResolver) Message() *string        { return r.m.Message }
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

type FetchTrailersResult struct {
	ItemID    string
	Title     *string
	Enqueued  int32
	PackageID *string
	JobIDs    []string
	Message   *string
}

type fetchTrailersResultResolver struct{ m FetchTrailersResult }

func (r *fetchTrailersResultResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *fetchTrailersResultResolver) Title() *string     { return r.m.Title }
func (r *fetchTrailersResultResolver) Enqueued() int32    { return r.m.Enqueued }
func (r *fetchTrailersResultResolver) PackageID() *string { return r.m.PackageID }
func (r *fetchTrailersResultResolver) JobIds() []string   { return r.m.JobIDs }
func (r *fetchTrailersResultResolver) Message() *string   { return r.m.Message }

type DownloadCommandResult struct {
	OK          bool
	Adapter     *string
	ClientJobID *string
	Message     *string
}

type downloadCommandResultResolver struct{ m DownloadCommandResult }

func (r *downloadCommandResultResolver) Ok() bool             { return r.m.OK }
func (r *downloadCommandResultResolver) Adapter() *string     { return r.m.Adapter }
func (r *downloadCommandResultResolver) ClientJobID() *string { return r.m.ClientJobID }
func (r *downloadCommandResultResolver) Message() *string     { return r.m.Message }
