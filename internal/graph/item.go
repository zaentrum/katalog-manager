package graph

import (
	"context"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// itemResolver resolves the Item GraphQL type. Computed fields reproduce the
// katalogservice_* view formulas (SPEC §1.1); relations are read lazily from
// the store.
type itemResolver struct {
	m *model.Item
	s *store.Store
	// With parentRead the parent was read with the item (a person's credit
	// reads its title's): parent, nil when there is none. Without, Parent
	// reads it.
	parent     *model.Item
	parentRead bool
}

func newItemResolver(m *model.Item, s *store.Store) *itemResolver {
	if m == nil {
		return nil
	}
	return &itemResolver{m: m, s: s}
}

func newItemResolvers(ms []*model.ItemRow, s *store.Store) []*itemResolver {
	out := make([]*itemResolver, 0, len(ms))
	for _, r := range ms {
		out = append(out, &itemResolver{m: &r.Item, s: s})
	}
	return out
}

func gidptr(p *string) *graphql.ID {
	if p == nil {
		return nil
	}
	id := graphql.ID(*p)
	return &id
}

// ---- scalars ----

func (r *itemResolver) ID() graphql.ID            { return gid(r.m.ID) }
func (r *itemResolver) Type() string              { return r.m.Type }
func (r *itemResolver) Title() string             { return r.m.Title }
func (r *itemResolver) SortTitle() *string        { return r.m.SortTitle }
func (r *itemResolver) Year() *int32              { return r.m.Year }
func (r *itemResolver) Description() *string      { return r.m.Description }
func (r *itemResolver) Rating() *float64          { return r.m.Rating }
func (r *itemResolver) DurationMs() *float64      { return i64ptrToFloat(r.m.DurationMs) }
func (r *itemResolver) ParentID() *graphql.ID     { return gidptr(r.m.ParentID) }
func (r *itemResolver) SeasonNumber() *int32      { return r.m.SeasonNumber }
func (r *itemResolver) EpisodeNumber() *int32     { return r.m.EpisodeNumber }
func (r *itemResolver) Tagline() *string          { return r.m.Tagline }
func (r *itemResolver) CreatedAt() *graphql.Time  { return gtime(r.m.CreatedAt) }
func (r *itemResolver) ModifiedAt() *graphql.Time { return gtime(r.m.ModifiedAt) }

// ---- computed (view formulas) ----

func (r *itemResolver) PosterURL() *string {
	s := "/api/manage/artwork/" + r.m.ID + "/poster"
	return &s
}

func (r *itemResolver) BackdropURL() *string {
	s := "/api/manage/artwork/" + r.m.ID + "/backdrop"
	return &s
}

func (r *itemResolver) RuntimeMin() *float64 {
	if r.m.DurationMs == nil {
		return nil
	}
	v := float64(*r.m.DurationMs / 60000)
	return &v
}

func (r *itemResolver) YearText() *string {
	if r.m.Year == nil {
		return nil
	}
	s := itoa32(*r.m.Year)
	return &s
}

func (r *itemResolver) IsPackaged(ctx context.Context) (bool, error) {
	return r.s.ItemIsPackaged(ctx, r.m.ID, r.m.Type)
}
func (r *itemResolver) HasIntro(ctx context.Context) (bool, error) {
	return r.s.ItemHasSegment(ctx, r.m.ID, "intro")
}
func (r *itemResolver) HasCredits(ctx context.Context) (bool, error) {
	return r.s.ItemHasSegment(ctx, r.m.ID, "credits")
}
func (r *itemResolver) HasRecap(ctx context.Context) (bool, error) {
	return r.s.ItemHasSegment(ctx, r.m.ID, "recap")
}
func (r *itemResolver) MetadataLocked(ctx context.Context) (bool, error) {
	return r.s.ItemMetadataLocked(ctx, r.m.ID)
}

// ---- relations ----

func (r *itemResolver) Parent(ctx context.Context) (*itemResolver, error) {
	if r.m.ParentID == nil {
		return nil, nil
	}
	if r.parentRead {
		return newItemResolver(r.parent, r.s), nil
	}
	p, err := r.s.GetItemBase(ctx, *r.m.ParentID)
	if err != nil || p == nil {
		return nil, err
	}
	return newItemResolver(p, r.s), nil
}

func (r *itemResolver) Children(ctx context.Context) ([]*itemResolver, error) {
	kids, err := r.s.ChildrenByParent(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*itemResolver, 0, len(kids))
	for _, k := range kids {
		out = append(out, newItemResolver(k, r.s))
	}
	return out, nil
}

func (r *itemResolver) ExternalIds(ctx context.Context) ([]*externalIDResolver, error) {
	rows, err := r.s.ExternalIDsByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*externalIDResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &externalIDResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Artwork(ctx context.Context) ([]*artworkResolver, error) {
	rows, err := r.s.ArtworkByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*artworkResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &artworkResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Assets(ctx context.Context) ([]*playbackAssetResolver, error) {
	rows, err := r.s.AssetsByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*playbackAssetResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &playbackAssetResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Subtitles(ctx context.Context) ([]*subtitleAssetResolver, error) {
	rows, err := r.s.SubtitlesByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*subtitleAssetResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &subtitleAssetResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Segments(ctx context.Context) ([]*mediaSegmentResolver, error) {
	rows, err := r.s.SegmentsByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*mediaSegmentResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &mediaSegmentResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Chapters(ctx context.Context) ([]*itemChapterResolver, error) {
	rows, err := r.s.ChaptersByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*itemChapterResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &itemChapterResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) TrailerLinks(ctx context.Context) ([]*trailerLinkResolver, error) {
	rows, err := r.s.TrailerLinksByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*trailerLinkResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &trailerLinkResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) Diagnostics(ctx context.Context) (*diagnosticsResolver, error) {
	d, err := r.s.DiagnosticsByItem(ctx, r.m.ID)
	if err != nil || d == nil {
		return nil, err
	}
	return &diagnosticsResolver{m: d}, nil
}

func (r *itemResolver) ProcessingSteps(ctx context.Context) ([]*processingStepResolver, error) {
	rows, err := r.s.StepsByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*processingStepResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &processingStepResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) OverallStatus(ctx context.Context) (*overallStatusResolver, error) {
	o, err := r.s.OverallStatusByItem(ctx, r.m.ID)
	if err != nil || o == nil {
		return nil, err
	}
	return &overallStatusResolver{m: o}, nil
}

func (r *itemResolver) Genres(ctx context.Context) ([]*genreResolver, error) {
	rows, err := r.s.GenresByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*genreResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &genreResolver{m: x})
	}
	return out, nil
}

func (r *itemResolver) People(ctx context.Context) ([]*itemPersonResolver, error) {
	rows, people, err := r.s.PeopleByItem(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*itemPersonResolver, 0, len(rows))
	for i, x := range rows {
		var p *model.Person
		if i < len(people) {
			p = people[i]
		}
		out = append(out, &itemPersonResolver{m: x, person: p, s: r.s})
	}
	return out, nil
}

func (r *itemResolver) Tags(ctx context.Context) ([]string, error) {
	return r.s.TagsByItem(ctx, r.m.ID)
}

// AgeRating is the title's age rating, in one read of the item and its
// parent.
func (r *itemResolver) AgeRating(ctx context.Context) (*ageRatingResolver, error) {
	m, err := r.s.ItemRating(ctx, r.m.ID)
	if err != nil || m == nil {
		return nil, err
	}
	return &ageRatingResolver{m: m}, nil
}

// Tracks are the tracks of the title's source, with the language each plays
// as.
func (r *itemResolver) Tracks(ctx context.Context) ([]*trackResolver, error) {
	ts, err := r.s.Tracks(ctx, r.m.ID)
	if err != nil {
		return nil, err
	}
	out := make([]*trackResolver, 0, len(ts))
	for _, t := range ts {
		out = append(out, &trackResolver{m: t})
	}
	return out, nil
}

// ---- Track ----

type trackResolver struct{ m *model.Track }

func (r *trackResolver) Kind() string              { return r.m.Kind }
func (r *trackResolver) Ordinal() int32            { return r.m.Ordinal }
func (r *trackResolver) SourceLanguage() *string   { return r.m.Language }
func (r *trackResolver) LanguageOverride() *string { return r.m.Override }
func (r *trackResolver) EffectiveLanguage() string {
	return languages.Effective(r.m.Override, r.m.Language)
}
func (r *trackResolver) Title() *string  { return r.m.Title }
func (r *trackResolver) Format() *string { return r.m.Format }
func (r *trackResolver) Forced() bool    { return r.m.Forced }
func (r *trackResolver) Reported() bool  { return r.m.Reported }

// ---- AgeRating ----

type ageRatingResolver struct{ m *model.ItemRating }

func (r *ageRatingResolver) Certification() *string   { return r.m.Certification }
func (r *ageRatingResolver) Country() *string         { return r.m.Country }
func (r *ageRatingResolver) MinAge() *int32           { return r.m.MinAge }
func (r *ageRatingResolver) MinAgeOverride() *int32   { return r.m.MinAgeOverride }
func (r *ageRatingResolver) EffectiveMinAge() *int32  { return r.m.Effective }
func (r *ageRatingResolver) FetchedAt() *graphql.Time { return gtime(r.m.FetchedAt) }
