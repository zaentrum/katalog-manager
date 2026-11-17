package graph

import (
	"context"

	"github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/captions"
	"github.com/zaentrum/katalog-manager/internal/library"
)

// CaptionReads is how far the closed captions are, and the backfill
// (captions.Service).
func (r *Resolver) CaptionReads(ctx context.Context) (*captionsProgressResolver, error) {
	if err := r.allow(ctx, "Query.captionReads"); err != nil {
		return nil, err
	}
	p, err := captions.New(r.store).Progress(ctx)
	if err != nil {
		return nil, err
	}
	return &captionsProgressResolver{m: p}, nil
}

type captionsProgressResolver struct{ m captions.Progress }

func (r *captionsProgressResolver) Originals() int32      { return int32(r.m.Originals) }
func (r *captionsProgressResolver) Read() int32           { return int32(r.m.Read) }
func (r *captionsProgressResolver) WithCaptions() int32   { return int32(r.m.WithCaptions) }
func (r *captionsProgressResolver) ToRead() int32         { return int32(r.m.ToRead) }
func (r *captionsProgressResolver) LengthMismatch() int32 { return int32(r.m.LengthMismatch) }
func (r *captionsProgressResolver) LengthMismatches() []string {
	if r.m.LengthMismatches == nil {
		return []string{}
	}
	return r.m.LengthMismatches
}

// Packaged, PackagesRead and PackagesWithCaptions are 0: packages are not
// read for closed captions (the gate asks the original alone); the fields
// stay for the clients that read them.
func (r *captionsProgressResolver) Packaged() int32             { return 0 }
func (r *captionsProgressResolver) PackagesRead() int32         { return 0 }
func (r *captionsProgressResolver) PackagesWithCaptions() int32 { return 0 }
func (r *captionsProgressResolver) Failed() int32               { return int32(r.m.Failed) }
func (r *captionsProgressResolver) InFlight() int32             { return int32(r.m.InFlight) }
func (r *captionsProgressResolver) HandedOutLastHour() int32    { return int32(r.m.HandedOutLastHour) }
func (r *captionsProgressResolver) LastReadAt() *graphql.Time   { return gtimeUTC(r.m.LastReadAt) }
func (r *captionsProgressResolver) Backfill() bool              { return r.m.Settings.On }
func (r *captionsProgressResolver) Window() string              { return r.m.Settings.Window.String() }
func (r *captionsProgressResolver) MaxInFlight() int32          { return int32(r.m.Settings.Inflight) }
func (r *captionsProgressResolver) MaxPerHour() int32           { return int32(r.m.Settings.Rate) }
func (r *captionsProgressResolver) Lease() string               { return library.DurationText(r.m.Settings.Lease) }
func (r *captionsProgressResolver) Idle() *string {
	if r.m.Idle == "" {
		return nil
	}
	return &r.m.Idle
}
func (r *captionsProgressResolver) Problems() []string {
	if r.m.Settings.Problems == nil {
		return []string{}
	}
	return r.m.Settings.Problems
}
