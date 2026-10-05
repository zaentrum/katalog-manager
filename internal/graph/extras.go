package graph

import (
	"context"
	"fmt"

	graphql "github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// Extras takes a title's extras in, removes them and has them packaged again
// (implemented by extras).
type Extras interface {
	AddExtra(ctx context.Context, in AddExtraRequest) (AddExtraResult, error)
	RemoveExtra(ctx context.Context, id, reason string) (*model.Extra, error)
	PackageExtra(ctx context.Context, id string) (ExtraPackagingResult, error)
	PackageExtras(ctx context.Context, itemID string) (ExtraPackagingResult, error)
}

// AddExtraRequest takes a file in as an extra of a movie or a series, named
// one way: by its id, by the path of its file (its primary asset), or by its
// TMDB id and its type.
type AddExtraRequest struct {
	ItemID       string
	ItemPath     string
	TmdbID       *int64
	ItemType     string
	Path         string
	Kind         string
	Title        string // "": the kind's name
	Language     *string
	SeasonNumber *int32
}

// AddExtraResult is the extra a file is, and whether it was taken in now.
type AddExtraResult struct {
	Extra   *model.Extra
	Created bool
}

// ExtraPackagingResult says what a packageExtra or packageExtras call did:
// the extras looked at, as they are after it, those packaged again, those
// left alone, those whose trigger could not be sent, and what happened.
type ExtraPackagingResult struct {
	Extras  []*model.Extra
	Queued  int32
	Busy    int32
	NotSent int32
	Message string
}

// ExtraRefused refuses an operation on an extra, saying why: Status is the
// answer's HTTP status on the REST route (400 a request that is none, 404 a
// title or an extra there is not, 409 a file that is another item's extra
// already, 503 a catalog without migration 039), Code its GraphQL code, and
// Extra the extra in the way (409).
type ExtraRefused struct {
	Status  int
	Code    string
	Message string
	Extra   *model.Extra
}

func (e *ExtraRefused) Error() string { return e.Message }

// Extensions are the GraphQL error's extensions: its code, and the extra in
// the way when there is one.
func (e *ExtraRefused) Extensions() map[string]any {
	ext := map[string]any{"code": e.Code}
	if e.Extra != nil {
		ext["extraId"], ext["itemId"] = e.Extra.ID, e.Extra.ItemID
	}
	return ext
}

// Refused is a refusal with status and code, its message formatted.
func Refused(status int, code, format string, args ...any) *ExtraRefused {
	return &ExtraRefused{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ---- Item.extras ----

// Extras are the title's extras, as a viewer sees them listed: those that are
// not removed, and with removed the removed ones too.
func (r *itemResolver) Extras(ctx context.Context, args struct{ Removed *bool }) ([]*extraResolver, error) {
	xs, err := r.s.ExtrasByItem(ctx, r.m.ID, derefBool(args.Removed))
	if err != nil {
		return nil, err
	}
	return extraResolvers(xs), nil
}

func extraResolvers(xs []*model.Extra) []*extraResolver {
	out := make([]*extraResolver, 0, len(xs))
	for _, x := range xs {
		out = append(out, &extraResolver{m: x})
	}
	return out
}

// ---- mutations ----

// AddExtra takes a file in as an extra of a movie or a series and has it
// packaged (see the SDL).
func (r *Resolver) AddExtra(ctx context.Context, in struct {
	ItemID       *graphql.ID
	ItemPath     *string
	TmdbID       *int32
	ItemType     *string
	Path         string
	Kind         string
	Title        *string
	Language     *string
	SeasonNumber *int32
}) (*addExtraResultResolver, error) {
	if err := r.allow(ctx, "Mutation.addExtra"); err != nil {
		return nil, err
	}
	if r.svc.Extras == nil {
		return nil, errNotConfigured
	}
	req := AddExtraRequest{ItemPath: strDeref(in.ItemPath), ItemType: strDeref(in.ItemType), Path: in.Path, Kind: in.Kind,
		Title: strDeref(in.Title), Language: in.Language, SeasonNumber: in.SeasonNumber}
	if in.ItemID != nil {
		req.ItemID = string(*in.ItemID)
	}
	if in.TmdbID != nil {
		id := int64(*in.TmdbID)
		req.TmdbID = &id
	}
	res, err := r.svc.Extras.AddExtra(ctx, req)
	if err != nil {
		return nil, err
	}
	return &addExtraResultResolver{m: res}, nil
}

// RemoveExtra removes an extra; nil when there is none.
func (r *Resolver) RemoveExtra(ctx context.Context, args struct {
	ID     graphql.ID
	Reason *string
}) (*extraResolver, error) {
	if err := r.allow(ctx, "Mutation.removeExtra"); err != nil {
		return nil, err
	}
	if r.svc.Extras == nil {
		return nil, errNotConfigured
	}
	x, err := r.svc.Extras.RemoveExtra(ctx, string(args.ID), strDeref(args.Reason))
	if err != nil || x == nil {
		return nil, err
	}
	return &extraResolver{m: x}, nil
}

// PackageExtra packages an extra again with the pipeline's current settings.
func (r *Resolver) PackageExtra(ctx context.Context, args struct{ ID graphql.ID }) (*extraPackagingResultResolver, error) {
	if err := r.allow(ctx, "Mutation.packageExtra"); err != nil {
		return nil, err
	}
	if r.svc.Extras == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Extras.PackageExtra(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &extraPackagingResultResolver{m: res}, nil
}

// PackageExtras packages every extra of a title again.
func (r *Resolver) PackageExtras(ctx context.Context, args struct{ ItemID graphql.ID }) (*extraPackagingResultResolver, error) {
	if err := r.allow(ctx, "Mutation.packageExtras"); err != nil {
		return nil, err
	}
	if r.svc.Extras == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Extras.PackageExtras(ctx, string(args.ItemID))
	if err != nil {
		return nil, err
	}
	return &extraPackagingResultResolver{m: res}, nil
}

// ---- Extra ----

type extraResolver struct{ m *model.Extra }

func (r *extraResolver) ID() graphql.ID              { return gid(r.m.ID) }
func (r *extraResolver) ItemID() graphql.ID          { return gid(r.m.ItemID) }
func (r *extraResolver) Kind() string                { return r.m.Kind }
func (r *extraResolver) Title() string               { return r.m.Title }
func (r *extraResolver) Label() *string              { return r.m.Label }
func (r *extraResolver) Language() *string           { return r.m.Language }
func (r *extraResolver) SeasonNumber() *int32        { return r.m.SeasonNumber }
func (r *extraResolver) SourcePath() *string         { return r.m.SourcePath }
func (r *extraResolver) SourceSize() *float64        { return i64ptrToFloat(r.m.SourceSize) }
func (r *extraResolver) SourceQh1() *string          { return r.m.SourceQH1 }
func (r *extraResolver) RecordPath() *string         { return r.m.RecordPath }
func (r *extraResolver) RegisteredBy() string        { return r.m.RegisteredBy }
func (r *extraResolver) SortOrder() *int32           { return r.m.SortOrder }
func (r *extraResolver) Hidden() bool                { return r.m.Hidden }
func (r *extraResolver) State() string               { return r.m.State }
func (r *extraResolver) Error() *string              { return r.m.Error }
func (r *extraResolver) Attempts() int32             { return r.m.Attempts }
func (r *extraResolver) Failures() int32             { return r.m.Failures }
func (r *extraResolver) NextRetryAt() *graphql.Time  { return gtime(r.m.NextRetryAt) }
func (r *extraResolver) DispatchedAt() *graphql.Time { return gtime(r.m.DispatchedAt) }
func (r *extraResolver) HeartbeatAt() *graphql.Time  { return gtime(r.m.HeartbeatAt) }
func (r *extraResolver) PackagePath() *string        { return r.m.PackagePath }
func (r *extraResolver) PackagedAt() *graphql.Time   { return gtime(r.m.PackagedAt) }
func (r *extraResolver) DurationMs() *float64        { return i64ptrToFloat(r.m.DurationMs) }
func (r *extraResolver) VideoCodec() *string         { return r.m.VideoCodec }
func (r *extraResolver) Width() *int32               { return r.m.Width }
func (r *extraResolver) Height() *int32              { return r.m.Height }
func (r *extraResolver) PeakBandwidthBps() *float64  { return i64ptrToFloat(r.m.PeakBandwidthBps) }
func (r *extraResolver) PackageSizeBytes() *float64  { return i64ptrToFloat(r.m.PackageSizeBytes) }
func (r *extraResolver) Playable() bool              { return r.m.Playable() }
func (r *extraResolver) RemovedAt() *graphql.Time    { return gtime(r.m.RemovedAt) }
func (r *extraResolver) RemovedBy() *string          { return r.m.RemovedBy }
func (r *extraResolver) RemovalReason() *string      { return r.m.RemovalReason }
func (r *extraResolver) CreatedAt() graphql.Time     { return graphql.Time{Time: r.m.CreatedAt} }
func (r *extraResolver) CreatedBy() *string          { return r.m.CreatedBy }
func (r *extraResolver) ModifiedAt() graphql.Time    { return graphql.Time{Time: r.m.ModifiedAt} }
func (r *extraResolver) ModifiedBy() *string         { return r.m.ModifiedBy }

type addExtraResultResolver struct{ m AddExtraResult }

func (r *addExtraResultResolver) Created() bool         { return r.m.Created }
func (r *addExtraResultResolver) Extra() *extraResolver { return &extraResolver{m: r.m.Extra} }

type extraPackagingResultResolver struct{ m ExtraPackagingResult }

func (r *extraPackagingResultResolver) Extras() []*extraResolver { return extraResolvers(r.m.Extras) }
func (r *extraPackagingResultResolver) Queued() int32            { return r.m.Queued }
func (r *extraPackagingResultResolver) Busy() int32              { return r.m.Busy }
func (r *extraPackagingResultResolver) NotSent() int32           { return r.m.NotSent }
func (r *extraPackagingResultResolver) Message() string          { return r.m.Message }
