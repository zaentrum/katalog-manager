package graph

import (
	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/model"
)

func i32fromI64(v *int64) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}

// ---- PlaybackAsset ----

type playbackAssetResolver struct{ m *model.PlaybackAsset }

func (r *playbackAssetResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *playbackAssetResolver) ItemID() graphql.ID        { return gid(r.m.ItemID) }
func (r *playbackAssetResolver) Path() string              { return r.m.Path }
func (r *playbackAssetResolver) Codec() *string            { return r.m.Codec }
func (r *playbackAssetResolver) Resolution() *string       { return r.m.Resolution }
func (r *playbackAssetResolver) BitrateKbps() *int32       { return r.m.BitrateKbps }
func (r *playbackAssetResolver) SizeBytes() *float64       { return i64ptrToFloat(r.m.SizeBytes) }
func (r *playbackAssetResolver) Hash() *string             { return r.m.Hash }
func (r *playbackAssetResolver) IsPrimary() *bool          { return r.m.IsPrimary }
func (r *playbackAssetResolver) Kind() *string             { return r.m.Kind }
func (r *playbackAssetResolver) AudioCodec() *string       { return r.m.AudioCodec }
func (r *playbackAssetResolver) AudioLanguage() *string    { return r.m.AudioLanguage }
func (r *playbackAssetResolver) AudioChannels() *int32     { return r.m.AudioChannels }
func (r *playbackAssetResolver) AudioBitrateKbps() *int32  { return r.m.AudioBitrateKbps }
func (r *playbackAssetResolver) AudioTrackCount() *int32   { return r.m.AudioTrackCount }
func (r *playbackAssetResolver) SubtitleTrackCount() *int32 { return r.m.SubtitleTrackCount }
func (r *playbackAssetResolver) DurationMs() *float64      { return i64ptrToFloat(r.m.DurationMs) }
func (r *playbackAssetResolver) SizeMB() *float64          { return i64ptrToFloat(r.m.SizeMB) }

// ---- SubtitleAsset ----

type subtitleAssetResolver struct{ m *model.SubtitleAsset }

func (r *subtitleAssetResolver) ID() graphql.ID     { return gid(r.m.ID) }
func (r *subtitleAssetResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *subtitleAssetResolver) Path() string       { return r.m.Path }
func (r *subtitleAssetResolver) Format() *string    { return r.m.Format }
func (r *subtitleAssetResolver) Lang() *string      { return r.m.Lang }
func (r *subtitleAssetResolver) Label() *string     { return r.m.Label }
func (r *subtitleAssetResolver) IsDefault() *bool   { return r.m.IsDefault }

// ---- MediaSegment ----

type mediaSegmentResolver struct{ m *model.MediaSegment }

func (r *mediaSegmentResolver) ID() graphql.ID     { return gid(r.m.ID) }
func (r *mediaSegmentResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *mediaSegmentResolver) Kind() string       { return r.m.Kind }
func (r *mediaSegmentResolver) StartMs() float64   { return i64ToFloat(r.m.StartMs) }
func (r *mediaSegmentResolver) EndMs() float64     { return i64ToFloat(r.m.EndMs) }
func (r *mediaSegmentResolver) Source() string     { return r.m.Source }
func (r *mediaSegmentResolver) Confidence() *float64 { return r.m.Confidence }
func (r *mediaSegmentResolver) Label() *string     { return r.m.Label }

// ---- ItemChapter ----

type itemChapterResolver struct{ m *model.ItemChapter }

func (r *itemChapterResolver) ID() graphql.ID     { return gid(r.m.ID) }
func (r *itemChapterResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *itemChapterResolver) StartMs() float64   { return i64ToFloat(r.m.StartMs) }
func (r *itemChapterResolver) EndMs() float64     { return i64ToFloat(r.m.EndMs) }
func (r *itemChapterResolver) Title() *string     { return r.m.Title }
func (r *itemChapterResolver) Ordinal() *int32    { return r.m.Ordinal }

// ---- ItemProcessingStep ----

type processingStepResolver struct{ m *model.ItemProcessingStep }

func (r *processingStepResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *processingStepResolver) ItemID() graphql.ID        { return gid(r.m.ItemID) }
func (r *processingStepResolver) Step() string              { return r.m.Step }
func (r *processingStepResolver) Status() string            { return r.m.Status }
func (r *processingStepResolver) StartedAt() *graphql.Time  { return gtime(r.m.StartedAt) }
func (r *processingStepResolver) FinishedAt() *graphql.Time { return gtime(r.m.FinishedAt) }
func (r *processingStepResolver) Attempts() *int32          { return r.m.Attempts }
func (r *processingStepResolver) Error() *string            { return r.m.Error }
func (r *processingStepResolver) Details() *string          { return r.m.Details }
func (r *processingStepResolver) StatusCriticality() *int32 { return r.m.StatusCriticality }

// ---- ItemOverallStatus ----

type overallStatusResolver struct{ m *model.ItemOverallStatus }

func (r *overallStatusResolver) ItemID() graphql.ID             { return gid(r.m.ItemID) }
func (r *overallStatusResolver) OverallStatus() *string         { return r.m.OverallStatus }
func (r *overallStatusResolver) DoneCount() *int32              { return i32fromI64(r.m.DoneCount) }
func (r *overallStatusResolver) PendingCount() *int32           { return i32fromI64(r.m.PendingCount) }
func (r *overallStatusResolver) FailedCount() *int32            { return i32fromI64(r.m.FailedCount) }
func (r *overallStatusResolver) InProgressCount() *int32        { return i32fromI64(r.m.InProgressCount) }
func (r *overallStatusResolver) NotApplicableCount() *int32     { return i32fromI64(r.m.NotApplicableCount) }
func (r *overallStatusResolver) TotalSteps() *int32             { return i32fromI64(r.m.TotalSteps) }
func (r *overallStatusResolver) LastStepFinishedAt() *graphql.Time { return gtime(r.m.LastStepFinishedAt) }

// ---- Genre / Person / ItemPerson ----

type genreResolver struct{ m *model.Genre }

func (r *genreResolver) ID() graphql.ID { return gid(r.m.ID) }
func (r *genreResolver) Name() string   { return r.m.Name }

type personResolver struct{ m *model.Person }

func (r *personResolver) ID() graphql.ID { return gid(r.m.ID) }
func (r *personResolver) Name() string   { return r.m.Name }

type itemPersonResolver struct {
	m      *model.ItemPerson
	person *model.Person
}

func (r *itemPersonResolver) ID() graphql.ID { return gid(r.m.ID) }
func (r *itemPersonResolver) Role() string   { return r.m.Role }
func (r *itemPersonResolver) Person() *personResolver {
	if r.person == nil {
		return &personResolver{m: &model.Person{ID: r.m.PersonID}}
	}
	return &personResolver{m: r.person}
}

// ---- ItemArtwork / ItemExternalId ----

type artworkResolver struct{ m *model.ItemArtwork }

func (r *artworkResolver) ID() graphql.ID     { return gid(r.m.ID) }
func (r *artworkResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *artworkResolver) Kind() string       { return r.m.Kind }
func (r *artworkResolver) URL() string        { return r.m.URL }

type externalIDResolver struct{ m *model.ItemExternalID }

func (r *externalIDResolver) ID() graphql.ID     { return gid(r.m.ID) }
func (r *externalIDResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *externalIDResolver) Source() string     { return r.m.Source }
func (r *externalIDResolver) ExternalID() string { return r.m.ExternalID }

// ---- ItemTrailerLink ----

type trailerLinkResolver struct{ m *model.ItemTrailerLink }

func (r *trailerLinkResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *trailerLinkResolver) ItemID() graphql.ID        { return gid(r.m.ItemID) }
func (r *trailerLinkResolver) Source() string            { return r.m.Source }
func (r *trailerLinkResolver) Site() *string             { return r.m.Site }
func (r *trailerLinkResolver) ExternalID() *string       { return r.m.ExternalID }
func (r *trailerLinkResolver) URL() string               { return r.m.URL }
func (r *trailerLinkResolver) Title() *string            { return r.m.Title }
func (r *trailerLinkResolver) DurationSec() *int32       { return r.m.DurationSec }
func (r *trailerLinkResolver) PublishedAt() *graphql.Time  { return gtime(r.m.PublishedAt) }
func (r *trailerLinkResolver) DownloadedAt() *graphql.Time { return gtime(r.m.DownloadedAt) }
func (r *trailerLinkResolver) LocalPath() *string        { return r.m.LocalPath }

// ---- ItemDiagnostics ----

type diagnosticsResolver struct{ m *model.ItemDiagnostics }

func (r *diagnosticsResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *diagnosticsResolver) ItemID() graphql.ID        { return gid(r.m.ItemID) }
func (r *diagnosticsResolver) GeneratedAt() *graphql.Time { return gtime(r.m.GeneratedAt) }
func (r *diagnosticsResolver) SourcePath() *string       { return r.m.SourcePath }
func (r *diagnosticsResolver) SourceSize() *float64      { return i64ptrToFloat(r.m.SourceSize) }
func (r *diagnosticsResolver) SourceMtime() *graphql.Time { return gtime(r.m.SourceMtime) }
func (r *diagnosticsResolver) FfprobeData() *string      { return r.m.FfprobeData }
func (r *diagnosticsResolver) FolderListing() *string    { return r.m.FolderListing }
func (r *diagnosticsResolver) Notes() *string            { return r.m.Notes }

// ---- ScanJob ----

type scanJobResolver struct{ m *model.ScanJob }

func (r *scanJobResolver) ID() graphql.ID           { return gid(r.m.ID) }
func (r *scanJobResolver) Source() string           { return r.m.Source }
func (r *scanJobResolver) Status() string           { return r.m.Status }
func (r *scanJobResolver) StartedAt() *graphql.Time  { return gtime(r.m.StartedAt) }
func (r *scanJobResolver) FinishedAt() *graphql.Time { return gtime(r.m.FinishedAt) }
func (r *scanJobResolver) ErrorMessage() *string    { return r.m.ErrorMessage }
func (r *scanJobResolver) FilesSeen() *int32         { return r.m.FilesSeen }
func (r *scanJobResolver) ItemsInserted() *int32     { return r.m.ItemsInserted }
func (r *scanJobResolver) ItemsUpdated() *int32      { return r.m.ItemsUpdated }

// ---- DownloadJob ----

type downloadJobResolver struct{ m *model.DownloadJob }

func (r *downloadJobResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *downloadJobResolver) Adapter() string           { return r.m.Adapter }
func (r *downloadJobResolver) ClientJobID() string       { return r.m.ClientJobID }
func (r *downloadJobResolver) Title() *string            { return r.m.Title }
func (r *downloadJobResolver) WantedItemID() *string     { return r.m.WantedItemID }
func (r *downloadJobResolver) State() string             { return r.m.State }
func (r *downloadJobResolver) ProgressPct() *float64     { return r.m.ProgressPct }
func (r *downloadJobResolver) DownloadedBytes() *float64 { return i64ptrToFloat(r.m.DownloadedBytes) }
func (r *downloadJobResolver) SizeBytes() *float64       { return i64ptrToFloat(r.m.SizeBytes) }
func (r *downloadJobResolver) SpeedBps() *float64        { return i64ptrToFloat(r.m.SpeedBps) }
func (r *downloadJobResolver) EtaSec() *int32            { return r.m.EtaSec }
func (r *downloadJobResolver) Files() *string            { return r.m.Files }
func (r *downloadJobResolver) ErrorMessage() *string     { return r.m.ErrorMessage }
func (r *downloadJobResolver) StartedAt() *graphql.Time   { return gtime(r.m.StartedAt) }
func (r *downloadJobResolver) CompletedAt() *graphql.Time { return gtime(r.m.CompletedAt) }
func (r *downloadJobResolver) LastEventAt() *graphql.Time { return gtime(r.m.LastEventAt) }
func (r *downloadJobResolver) StateCriticality() *int32  { return r.m.StateCriticality }

// ---- Setting ----

type settingResolver struct{ m *model.Setting }

func (r *settingResolver) ID() graphql.ID    { return gid(r.m.ID) }
func (r *settingResolver) Key() string       { return r.m.Key }
func (r *settingResolver) ValueText() string { return r.m.ValueText }
func (r *settingResolver) ValueType() string { return r.m.ValueType }
func (r *settingResolver) Description() *string { return r.m.Description }

// ---- EnrichmentStatusCode ----

type enrichmentStatusCodeResolver struct{ m *model.EnrichmentStatusCode }

func (r *enrichmentStatusCodeResolver) Code() string  { return r.m.Code }
func (r *enrichmentStatusCodeResolver) Name() *string { return r.m.Name }
