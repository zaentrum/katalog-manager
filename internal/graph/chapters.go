package graph

import (
	"context"

	"github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/chapters"
	"github.com/zaentrum/katalog-manager/internal/library"
)

// How far the titles' chapters are (migration 055, chapters.Progress): the
// titles that miss theirs, what the reads found, and the backfill.

// ChapterReads is how far the chapters are, and their backfill.
func (r *Resolver) ChapterReads(ctx context.Context) (*chapterProgressResolver, error) {
	if err := r.allow(ctx, "Query.chapterReads"); err != nil {
		return nil, err
	}
	p, err := chapters.New(r.store, r.cfg).Progress(ctx)
	if err != nil {
		return nil, err
	}
	return &chapterProgressResolver{m: p}, nil
}

type chapterProgressResolver struct{ m chapters.Progress }

func (r *chapterProgressResolver) Missing() int32           { return int32(r.m.Missing) }
func (r *chapterProgressResolver) MissingPackaged() int32   { return int32(r.m.MissingPackaged) }
func (r *chapterProgressResolver) Read() int32              { return int32(r.m.Read) }
func (r *chapterProgressResolver) None() int32              { return int32(r.m.None) }
func (r *chapterProgressResolver) Unreadable() int32        { return int32(r.m.Unreadable) }
func (r *chapterProgressResolver) Failed() int32            { return int32(r.m.Failed) }
func (r *chapterProgressResolver) InFlight() int32          { return int32(r.m.InFlight) }
func (r *chapterProgressResolver) HandedOutLastHour() int32 { return int32(r.m.HandedOutLastHour) }
func (r *chapterProgressResolver) LastReadAt() *graphql.Time {
	return gtimeUTC(r.m.LastReadAt)
}
func (r *chapterProgressResolver) Backfill() bool     { return r.m.Settings.On }
func (r *chapterProgressResolver) Window() string     { return r.m.Settings.Window.String() }
func (r *chapterProgressResolver) MaxInFlight() int32 { return int32(r.m.Settings.Inflight) }
func (r *chapterProgressResolver) MaxPerHour() int32  { return int32(r.m.Settings.Rate) }
func (r *chapterProgressResolver) Lease() string      { return library.DurationText(r.m.Settings.Lease) }
func (r *chapterProgressResolver) Idle() *string {
	if r.m.Idle == "" {
		return nil
	}
	return &r.m.Idle
}
func (r *chapterProgressResolver) Problems() []string {
	if r.m.Settings.Problems == nil {
		return []string{}
	}
	return r.m.Settings.Problems
}
