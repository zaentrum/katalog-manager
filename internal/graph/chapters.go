package graph

import (
	"context"

	"github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/chapters"
	"github.com/zaentrum/katalog-manager/internal/library"
)

// How far the titles' chapters are (migration 055, chapters.Progress): the
// titles that miss theirs, what the reads found, and the backfill; and an
// admin's acceptance of the titles whose header could not be read.

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

func (r *chapterProgressResolver) Missing() int32             { return int32(r.m.Missing) }
func (r *chapterProgressResolver) MissingPackaged() int32     { return int32(r.m.MissingPackaged) }
func (r *chapterProgressResolver) Read() int32                { return int32(r.m.Read) }
func (r *chapterProgressResolver) FromRecord() int32          { return int32(r.m.FromRecord) }
func (r *chapterProgressResolver) None() int32                { return int32(r.m.None) }
func (r *chapterProgressResolver) Unconfirmed() int32         { return int32(r.m.Unconfirmed) }
func (r *chapterProgressResolver) Unreadable() int32          { return int32(r.m.Unreadable) }
func (r *chapterProgressResolver) Accepted() int32            { return int32(r.m.Accepted) }
func (r *chapterProgressResolver) Failed() int32              { return int32(r.m.Failed) }
func (r *chapterProgressResolver) InFlight() int32            { return int32(r.m.InFlight) }
func (r *chapterProgressResolver) HandedOutLastHour() int32   { return int32(r.m.HandedOutLastHour) }
func (r *chapterProgressResolver) RecordsReadLastHour() int32 { return int32(r.m.RecordsReadLastHour) }
func (r *chapterProgressResolver) LastReadAt() *graphql.Time {
	return gtimeUTC(r.m.LastReadAt)
}
func (r *chapterProgressResolver) Backfill() bool           { return r.m.Settings.On }
func (r *chapterProgressResolver) Window() string           { return r.m.Settings.Window.String() }
func (r *chapterProgressResolver) MaxInFlight() int32       { return int32(r.m.Settings.Inflight) }
func (r *chapterProgressResolver) MaxPerHour() int32        { return int32(r.m.Settings.Rate) }
func (r *chapterProgressResolver) MaxRecordsPerHour() int32 { return int32(r.m.Settings.Records) }
func (r *chapterProgressResolver) Lease() string            { return library.DurationText(r.m.Settings.Lease) }
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

// AcceptChapterReads settles, for an admin, the unreadable titles of items.
func (r *Resolver) AcceptChapterReads(ctx context.Context, args struct{ Items []graphql.ID }) (*chapterAcceptResolver,
	error) {
	if err := r.allow(ctx, "Mutation.acceptChapterReads"); err != nil {
		return nil, err
	}
	ids := make([]string, len(args.Items))
	for i, id := range args.Items {
		ids[i] = string(id)
	}
	accepted, refused, err := chapters.New(r.store, r.cfg).Accept(ctx, ids, auth.Actor(ctx, "katalog-manager"))
	if err != nil {
		return nil, err
	}
	return &chapterAcceptResolver{accepted: accepted, refused: refused}, nil
}

type chapterAcceptResolver struct{ accepted, refused []string }

func (r *chapterAcceptResolver) Accepted() []graphql.ID {
	out := make([]graphql.ID, len(r.accepted))
	for i, id := range r.accepted {
		out[i] = graphql.ID(id)
	}
	return out
}
func (r *chapterAcceptResolver) Refused() []string { return r.refused }
