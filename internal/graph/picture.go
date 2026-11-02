package graph

import (
	"context"
	"sync"
	"time"

	"github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/picture"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// A title's picture area (migration 054, picture.Area): the part of its frame
// its picture fills, and how far the backfill is.

// pictureReady holds the catalogs migration 054 was seen in place in.
var pictureReady sync.Map

func picturesReady(ctx context.Context, s *store.Store) (bool, error) {
	if _, ok := pictureReady.Load(s); ok {
		return true, nil
	}
	ok, err := s.PictureAreasReady(ctx)
	if err == nil && ok {
		pictureReady.Store(s, true)
	}
	return ok, err
}

// PictureArea is the title's picture area; nil when it was never measured,
// and on a catalog older than migration 054.
func (r *itemResolver) PictureArea(ctx context.Context) (*pictureAreaResolver, error) {
	if ok, err := picturesReady(ctx, r.s); err != nil || !ok {
		return nil, err
	}
	a, err := picture.Of(ctx, r.s.Pool(), picture.KindItem, r.m.ID)
	if err != nil || a == nil {
		return nil, err
	}
	return &pictureAreaResolver{m: a}, nil
}

type pictureAreaResolver struct{ m *picture.Area }

func (r *pictureAreaResolver) State() string          { return r.m.State }
func (r *pictureAreaResolver) Current() bool          { return r.m.Current }
func (r *pictureAreaResolver) FrameWidth() *int32     { return r.m.FrameWidth }
func (r *pictureAreaResolver) FrameHeight() *int32    { return r.m.FrameHeight }
func (r *pictureAreaResolver) X() *int32              { return r.measured(r.m.X) }
func (r *pictureAreaResolver) Y() *int32              { return r.measured(r.m.Y) }
func (r *pictureAreaResolver) Width() *int32          { return r.measured(r.m.Width) }
func (r *pictureAreaResolver) Height() *int32         { return r.measured(r.m.Height) }
func (r *pictureAreaResolver) Full() bool             { return r.m.Full() }
func (r *pictureAreaResolver) Path() *string          { return r.m.Path }
func (r *pictureAreaResolver) SourceID() *graphql.ID  { return gidptr(r.m.SourceID) }
func (r *pictureAreaResolver) VersionID() *graphql.ID { return gidptr(r.m.VersionID) }
func (r *pictureAreaResolver) Method() *string        { return r.m.Method }
func (r *pictureAreaResolver) Samples() *int32        { return r.m.Samples }
func (r *pictureAreaResolver) Informative() *int32    { return r.m.Informative }
func (r *pictureAreaResolver) Reason() *string        { return r.m.Reason }
func (r *pictureAreaResolver) MeasuredAt() *graphql.Time {
	return gtimeUTC(r.m.MeasuredAt)
}
func (r *pictureAreaResolver) Attempts() int32 { return r.m.Attempts }
func (r *pictureAreaResolver) Failures() int32 { return r.m.Failures }
func (r *pictureAreaResolver) NextAttemptAt() *graphql.Time {
	return gtimeUTC(r.m.NextAttemptAt)
}
func (r *pictureAreaResolver) ClaimedAt() *graphql.Time { return gtimeUTC(r.m.ClaimedAt) }
func (r *pictureAreaResolver) ClaimedBy() *string       { return r.m.ClaimedBy }

// measured is an edge of the area, of a measured one only.
func (r *pictureAreaResolver) measured(v *int32) *int32 {
	if r.m.State != picture.StateMeasured {
		return nil
	}
	return v
}

// PictureAreas is how far the picture areas are, and the backfill.
func (r *Resolver) PictureAreas(ctx context.Context) (*pictureProgressResolver, error) {
	if err := r.allow(ctx, "Query.pictureAreas"); err != nil {
		return nil, err
	}
	p, err := picture.New(r.store, r.cfg).Progress(ctx)
	if err != nil {
		return nil, err
	}
	return &pictureProgressResolver{m: p}, nil
}

type pictureProgressResolver struct{ m picture.Progress }

func (r *pictureProgressResolver) Titles() int32            { return int32(r.m.Titles) }
func (r *pictureProgressResolver) Letterboxed() int32       { return int32(r.m.Letterboxed) }
func (r *pictureProgressResolver) Full() int32              { return int32(r.m.Full) }
func (r *pictureProgressResolver) Unknown() int32           { return int32(r.m.Unknown) }
func (r *pictureProgressResolver) ToMeasure() int32         { return int32(r.m.ToMeasure) }
func (r *pictureProgressResolver) Failed() int32            { return int32(r.m.Failed) }
func (r *pictureProgressResolver) InFlight() int32          { return int32(r.m.InFlight) }
func (r *pictureProgressResolver) HandedOutLastHour() int32 { return int32(r.m.HandedOutLastHour) }
func (r *pictureProgressResolver) CPUSecondsPerHour() *float64 {
	return r.m.CPUSecondsPerHour
}
func (r *pictureProgressResolver) LastMeasuredAt() *graphql.Time { return gtimeUTC(r.m.LastMeasuredAt) }
func (r *pictureProgressResolver) Backfill() bool                { return r.m.Settings.On }
func (r *pictureProgressResolver) Window() string                { return r.m.Settings.Window.String() }
func (r *pictureProgressResolver) MaxInFlight() int32            { return int32(r.m.Settings.Inflight) }
func (r *pictureProgressResolver) MaxPerHour() int32             { return int32(r.m.Settings.Rate) }
func (r *pictureProgressResolver) Lease() string                 { return library.DurationText(r.m.Settings.Lease) }
func (r *pictureProgressResolver) Idle() *string {
	if r.m.Idle == "" {
		return nil
	}
	return &r.m.Idle
}
func (r *pictureProgressResolver) Problems() []string {
	if r.m.Settings.Problems == nil {
		return []string{}
	}
	return r.m.Settings.Problems
}

// gtimeUTC is gtime of t in UTC.
func gtimeUTC(t *time.Time) *graphql.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return gtime(&u)
}
