package graph

import (
	"context"
	"sort"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
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

func (r *playbackAssetResolver) ID() graphql.ID             { return gid(r.m.ID) }
func (r *playbackAssetResolver) ItemID() graphql.ID         { return gid(r.m.ItemID) }
func (r *playbackAssetResolver) Path() string               { return r.m.Path }
func (r *playbackAssetResolver) Codec() *string             { return r.m.Codec }
func (r *playbackAssetResolver) Resolution() *string        { return r.m.Resolution }
func (r *playbackAssetResolver) BitrateKbps() *int32        { return r.m.BitrateKbps }
func (r *playbackAssetResolver) SizeBytes() *float64        { return i64ptrToFloat(r.m.SizeBytes) }
func (r *playbackAssetResolver) Hash() *string              { return r.m.Hash }
func (r *playbackAssetResolver) IsPrimary() *bool           { return r.m.IsPrimary }
func (r *playbackAssetResolver) Kind() *string              { return r.m.Kind }
func (r *playbackAssetResolver) AudioCodec() *string        { return r.m.AudioCodec }
func (r *playbackAssetResolver) AudioLanguage() *string     { return r.m.AudioLanguage }
func (r *playbackAssetResolver) AudioChannels() *int32      { return r.m.AudioChannels }
func (r *playbackAssetResolver) AudioBitrateKbps() *int32   { return r.m.AudioBitrateKbps }
func (r *playbackAssetResolver) AudioTrackCount() *int32    { return r.m.AudioTrackCount }
func (r *playbackAssetResolver) SubtitleTrackCount() *int32 { return r.m.SubtitleTrackCount }
func (r *playbackAssetResolver) DurationMs() *float64       { return i64ptrToFloat(r.m.DurationMs) }
func (r *playbackAssetResolver) SizeMB() *float64           { return i64ptrToFloat(r.m.SizeMB) }

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

func (r *mediaSegmentResolver) ID() graphql.ID       { return gid(r.m.ID) }
func (r *mediaSegmentResolver) ItemID() graphql.ID   { return gid(r.m.ItemID) }
func (r *mediaSegmentResolver) Kind() string         { return r.m.Kind }
func (r *mediaSegmentResolver) StartMs() float64     { return i64ToFloat(r.m.StartMs) }
func (r *mediaSegmentResolver) EndMs() float64       { return i64ToFloat(r.m.EndMs) }
func (r *mediaSegmentResolver) Source() string       { return r.m.Source }
func (r *mediaSegmentResolver) Confidence() *float64 { return r.m.Confidence }
func (r *mediaSegmentResolver) Label() *string       { return r.m.Label }

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

func (r *overallStatusResolver) ItemID() graphql.ID      { return gid(r.m.ItemID) }
func (r *overallStatusResolver) OverallStatus() *string  { return r.m.OverallStatus }
func (r *overallStatusResolver) DoneCount() *int32       { return i32fromI64(r.m.DoneCount) }
func (r *overallStatusResolver) PendingCount() *int32    { return i32fromI64(r.m.PendingCount) }
func (r *overallStatusResolver) FailedCount() *int32     { return i32fromI64(r.m.FailedCount) }
func (r *overallStatusResolver) InProgressCount() *int32 { return i32fromI64(r.m.InProgressCount) }
func (r *overallStatusResolver) NotApplicableCount() *int32 {
	return i32fromI64(r.m.NotApplicableCount)
}
func (r *overallStatusResolver) TotalSteps() *int32 { return i32fromI64(r.m.TotalSteps) }
func (r *overallStatusResolver) LastStepFinishedAt() *graphql.Time {
	return gtime(r.m.LastStepFinishedAt)
}

// ---- Genre / Person / ItemPerson ----

type genreResolver struct{ m *model.Genre }

func (r *genreResolver) ID() graphql.ID { return gid(r.m.ID) }
func (r *genreResolver) Name() string   { return r.m.Name }

type personResolver struct {
	m *model.Person
	s *store.Store
}

func (r *personResolver) ID() graphql.ID              { return gid(r.m.ID) }
func (r *personResolver) Name() string                { return r.m.Name }
func (r *personResolver) SortName() *string           { return r.m.SortName }
func (r *personResolver) AlsoKnownAs() []string       { return nonNil(r.m.AlsoKnownAs) }
func (r *personResolver) BirthDate() *string          { return r.m.BirthDate }
func (r *personResolver) DeathDate() *string          { return r.m.DeathDate }
func (r *personResolver) BirthPlace() *string         { return r.m.BirthPlace }
func (r *personResolver) TmdbPersonID() *string       { return r.m.TmdbPersonID }
func (r *personResolver) ImdbID() *string             { return r.m.ImdbID }
func (r *personResolver) KnownForDepartment() *string { return r.m.KnownForDepartment }
func (r *personResolver) MetadataLocked() bool        { return r.m.MetadataLocked }
func (r *personResolver) LockedFields() []string      { return nonNil(r.m.LockedFields) }
func (r *personResolver) TmdbFetchedAt() *graphql.Time {
	return gtime(r.m.TmdbFetchedAt)
}
func (r *personResolver) TmdbChangedAt() *string    { return r.m.TmdbChangedAt }
func (r *personResolver) CreatedAt() *graphql.Time  { return gtime(r.m.CreatedAt) }
func (r *personResolver) ModifiedAt() *graphql.Time { return gtime(r.m.ModifiedAt) }

func (r *personResolver) Biography() []*localizedTextResolver {
	out := []*localizedTextResolver{}
	for _, lang := range sortedKeys(r.m.Biography) {
		out = append(out, &localizedTextResolver{language: lang, text: r.m.Biography[lang]})
	}
	return out
}

func (r *personResolver) FieldOrigins() []*fieldOriginResolver {
	out := []*fieldOriginResolver{}
	for _, f := range sortedKeys(r.m.FieldOrigins) {
		out = append(out, &fieldOriginResolver{field: f, origin: r.m.FieldOrigins[f]})
	}
	return out
}

// Credits reads the person's credits with their titles, in one query.
func (r *personResolver) Credits(ctx context.Context) ([]*personCreditResolver, error) {
	cs, err := r.s.CreditsByPerson(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*personCreditResolver, 0, len(cs))
	for _, c := range cs {
		out = append(out, &personCreditResolver{m: c, s: r.s})
	}
	return out, nil
}

// personCreditResolver is a person's credit; its title comes with its parent,
// read with the credit.
type personCreditResolver struct {
	m *model.PersonCredit
	s *store.Store
}

func (r *personCreditResolver) ID() graphql.ID       { return gid(r.m.ID) }
func (r *personCreditResolver) Role() string         { return r.m.Role }
func (r *personCreditResolver) Job() *string         { return r.m.Job }
func (r *personCreditResolver) Character() *string   { return r.m.Character }
func (r *personCreditResolver) Order() *int32        { return r.m.Order }
func (r *personCreditResolver) EpisodeCount() *int32 { return r.m.EpisodeCount }
func (r *personCreditResolver) Item() *itemResolver {
	return &itemResolver{m: &r.m.Item, s: r.s, parent: r.m.Parent, parentRead: true}
}

type localizedTextResolver struct{ language, text string }

func (r *localizedTextResolver) Language() string { return r.language }
func (r *localizedTextResolver) Text() string     { return r.text }

type fieldOriginResolver struct{ field, origin string }

func (r *fieldOriginResolver) Field() string  { return r.field }
func (r *fieldOriginResolver) Origin() string { return r.origin }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type itemPersonResolver struct {
	m      *model.ItemPerson
	person *model.Person
	s      *store.Store
}

func (r *itemPersonResolver) ID() graphql.ID       { return gid(r.m.ID) }
func (r *itemPersonResolver) Role() string         { return r.m.Role }
func (r *itemPersonResolver) Job() *string         { return r.m.Job }
func (r *itemPersonResolver) Character() *string   { return r.m.Character }
func (r *itemPersonResolver) Order() *int32        { return r.m.Order }
func (r *itemPersonResolver) EpisodeCount() *int32 { return r.m.EpisodeCount }
func (r *itemPersonResolver) Person() *personResolver {
	if r.person == nil {
		return &personResolver{m: &model.Person{ID: r.m.PersonID}, s: r.s}
	}
	return &personResolver{m: r.person, s: r.s}
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

func (r *trailerLinkResolver) ID() graphql.ID              { return gid(r.m.ID) }
func (r *trailerLinkResolver) ItemID() graphql.ID          { return gid(r.m.ItemID) }
func (r *trailerLinkResolver) Source() string              { return r.m.Source }
func (r *trailerLinkResolver) Site() *string               { return r.m.Site }
func (r *trailerLinkResolver) ExternalID() *string         { return r.m.ExternalID }
func (r *trailerLinkResolver) URL() string                 { return r.m.URL }
func (r *trailerLinkResolver) Title() *string              { return r.m.Title }
func (r *trailerLinkResolver) DurationSec() *int32         { return r.m.DurationSec }
func (r *trailerLinkResolver) PublishedAt() *graphql.Time  { return gtime(r.m.PublishedAt) }
func (r *trailerLinkResolver) DownloadedAt() *graphql.Time { return gtime(r.m.DownloadedAt) }
func (r *trailerLinkResolver) LocalPath() *string          { return r.m.LocalPath }

// ---- ItemDiagnostics ----

type diagnosticsResolver struct{ m *model.ItemDiagnostics }

func (r *diagnosticsResolver) ID() graphql.ID             { return gid(r.m.ID) }
func (r *diagnosticsResolver) ItemID() graphql.ID         { return gid(r.m.ItemID) }
func (r *diagnosticsResolver) GeneratedAt() *graphql.Time { return gtime(r.m.GeneratedAt) }
func (r *diagnosticsResolver) SourcePath() *string        { return r.m.SourcePath }
func (r *diagnosticsResolver) SourceSize() *float64       { return i64ptrToFloat(r.m.SourceSize) }
func (r *diagnosticsResolver) SourceMtime() *graphql.Time { return gtime(r.m.SourceMtime) }
func (r *diagnosticsResolver) FfprobeData() *string       { return r.m.FfprobeData }
func (r *diagnosticsResolver) FolderListing() *string     { return r.m.FolderListing }
func (r *diagnosticsResolver) Notes() *string             { return r.m.Notes }

// ---- ScanJob ----

type scanJobResolver struct{ m *model.ScanJob }

func (r *scanJobResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *scanJobResolver) Source() string            { return r.m.Source }
func (r *scanJobResolver) Status() string            { return r.m.Status }
func (r *scanJobResolver) StartedAt() *graphql.Time  { return gtime(r.m.StartedAt) }
func (r *scanJobResolver) FinishedAt() *graphql.Time { return gtime(r.m.FinishedAt) }
func (r *scanJobResolver) ErrorMessage() *string     { return r.m.ErrorMessage }
func (r *scanJobResolver) FilesSeen() *int32         { return r.m.FilesSeen }
func (r *scanJobResolver) ItemsInserted() *int32     { return r.m.ItemsInserted }
func (r *scanJobResolver) ItemsUpdated() *int32      { return r.m.ItemsUpdated }

// ---- ActivityEvent ----

type activityEventResolver struct{ m *store.ActivityRow }

func (r *activityEventResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *activityEventResolver) ItemID() graphql.ID        { return gid(r.m.ItemID) }
func (r *activityEventResolver) ItemTitle() string         { return r.m.ItemTitle }
func (r *activityEventResolver) ItemType() string          { return r.m.ItemType }
func (r *activityEventResolver) Step() string              { return r.m.Step }
func (r *activityEventResolver) Status() string            { return r.m.Status }
func (r *activityEventResolver) Attempts() *int32          { return r.m.Attempts }
func (r *activityEventResolver) Error() *string            { return r.m.Error }
func (r *activityEventResolver) StartedAt() *graphql.Time  { return gtime(r.m.StartedAt) }
func (r *activityEventResolver) FinishedAt() *graphql.Time { return gtime(r.m.FinishedAt) }
func (r *activityEventResolver) UpdatedAt() *graphql.Time  { return gtime(r.m.UpdatedAt) }

// ---- DeletedItem ----

type deletedItemResolver struct{ m *model.DeletedItem }

func (r *deletedItemResolver) ID() graphql.ID          { return gid(r.m.ID) }
func (r *deletedItemResolver) Type() string            { return r.m.Type }
func (r *deletedItemResolver) Title() string           { return r.m.Title }
func (r *deletedItemResolver) DeletedAt() graphql.Time { return graphql.Time{Time: r.m.DeletedAt} }
func (r *deletedItemResolver) DeletedBy() string       { return r.m.DeletedBy }
func (r *deletedItemResolver) Reason() *string         { return r.m.Reason }

// ---- ReferenceSync ----

type referenceSyncResolver struct{ m *model.ReferenceSync }

func (r *referenceSyncResolver) Kind() string             { return r.m.Kind }
func (r *referenceSyncResolver) Cursor() string           { return r.m.Cursor }
func (r *referenceSyncResolver) LastRunAt() *graphql.Time { return gtime(r.m.LastRunAt) }
func (r *referenceSyncResolver) LastRunChanges() *int32   { return r.m.LastRunChanges }
func (r *referenceSyncResolver) LastRunMatched() *int32   { return r.m.LastRunMatched }
func (r *referenceSyncResolver) LastRunRefreshed() *int32 { return r.m.LastRunRefreshed }
func (r *referenceSyncResolver) LastRunSkipped() *int32   { return r.m.LastRunSkipped }
func (r *referenceSyncResolver) LastRunFailed() *int32    { return r.m.LastRunFailed }
func (r *referenceSyncResolver) LastRunError() *string    { return r.m.LastRunError }

// ---- EnrichmentStatusCode ----

type enrichmentStatusCodeResolver struct{ m *model.EnrichmentStatusCode }

func (r *enrichmentStatusCodeResolver) Code() string  { return r.m.Code }
func (r *enrichmentStatusCodeResolver) Name() *string { return r.m.Name }
