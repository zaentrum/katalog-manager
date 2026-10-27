package graph

import (
	"context"
	"errors"
	"net/http"
	"strings"

	graphql "github.com/graph-gophers/graphql-go"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/library"
)

// Retirement retires the originals of an admin's list of titles, in the
// background (implemented by library.RetireList).
type Retirement interface {
	// Start runs a list as req asks; *library.ListRefused when it is not
	// run.
	Start(ctx context.Context, req library.ListRequest) (library.ListRun, error)
	// Stop stops the run that runs once the title it is at has its result,
	// on the word of by; nil when none runs.
	Stop(ctx context.Context, by string) (*library.ListRun, error)
	// Latest is the run begun last; nil when none ever ran.
	Latest(ctx context.Context) (*library.ListRun, error)
	// Items are a page of the titles of the run id, of result alone when it
	// is one ("" any).
	Items(ctx context.Context, id string, first, after int, result string) ([]library.ListItem, error)
}

// The refusals of a list, by their code, as the HTTP status they are like.
var listRefusals = map[string]int{"UNAVAILABLE": http.StatusServiceUnavailable, "LEGACY": http.StatusConflict,
	"OFF": http.StatusConflict, "DRY_RUN_ONLY": http.StatusConflict, "NAMED_TITLES": http.StatusBadRequest,
	"BOTH": http.StatusBadRequest, "EMPTY": http.StatusBadRequest, "BAD_ID": http.StatusBadRequest,
	"TOO_MANY": http.StatusBadRequest, "FROM_RUN": http.StatusConflict, "RUNNING": http.StatusConflict}

// RetireOriginals runs a list in the background, on the word of the admin
// who asks: a dry run of the titles items names, or a run of the titles the
// dry run fromRun found it would retire, retired unless dryRun is true (the
// default).
func (r *Resolver) RetireOriginals(ctx context.Context, args struct {
	Items   *[]graphql.ID
	FromRun *graphql.ID
	DryRun  bool
}) (*retireRunResolver, error) {
	if err := r.allow(ctx, "Mutation.retireOriginals"); err != nil {
		return nil, err
	}
	if r.svc.Retirement == nil {
		return nil, errNotConfigured
	}
	req := library.ListRequest{DryRun: args.DryRun, By: auth.Actor(ctx, "katalog-manager")}
	if args.Items != nil {
		for _, id := range *args.Items {
			req.Items = append(req.Items, strings.TrimSpace(string(id)))
		}
	}
	if args.FromRun != nil {
		req.FromRun = strings.TrimSpace(string(*args.FromRun))
	}
	run, err := r.svc.Retirement.Start(ctx, req)
	var refused *library.ListRefused
	if errors.As(err, &refused) {
		status, ok := listRefusals[refused.Code]
		if !ok {
			status = http.StatusBadRequest
		}
		return nil, Refused(status, refused.Code, "%s", refused.Msg)
	}
	if err != nil {
		return nil, err
	}
	return &retireRunResolver{m: run, svc: r.svc.Retirement}, nil
}

// StopRetireRun stops the run of retireOriginals that runs once the title it
// is at has its result, on the word of the admin who asks; nil when none
// runs.
func (r *Resolver) StopRetireRun(ctx context.Context) (*retireRunResolver, error) {
	if err := r.allow(ctx, "Mutation.stopRetireRun"); err != nil {
		return nil, err
	}
	if r.svc.Retirement == nil {
		return nil, errNotConfigured
	}
	run, err := r.svc.Retirement.Stop(ctx, auth.Actor(ctx, "katalog-manager"))
	if err != nil || run == nil {
		return nil, err
	}
	return &retireRunResolver{m: *run, svc: r.svc.Retirement}, nil
}

// RetireRun is the run of retireOriginals begun last; nil when none ever ran.
func (r *Resolver) RetireRun(ctx context.Context) (*retireRunResolver, error) {
	if err := r.allow(ctx, "Query.retireRun"); err != nil {
		return nil, err
	}
	if r.svc.Retirement == nil {
		return nil, errNotConfigured
	}
	run, err := r.svc.Retirement.Latest(ctx)
	if err != nil || run == nil {
		return nil, err
	}
	return &retireRunResolver{m: *run, svc: r.svc.Retirement}, nil
}

// The results of a list's title, by their GraphQL names, as the library
// names them.
var listResults = map[string]string{"RETIRED": library.ListRetired, "SKIPPED": library.ListSkipped,
	"FAILED": library.ListFailed, "WOULD_RETIRE": library.ListWouldRetire}

// resultName is a result as GraphQL names it; nil for none.
func resultName(result *string) *string {
	if result == nil {
		return nil
	}
	for name, r := range listResults {
		if r == *result {
			return &name
		}
	}
	return nil
}

type retireRunResolver struct {
	m   library.ListRun
	svc Retirement
}

func (r *retireRunResolver) ID() graphql.ID          { return graphql.ID(r.m.ID) }
func (r *retireRunResolver) State() string           { return strings.ToUpper(r.m.State) }
func (r *retireRunResolver) DryRun() bool            { return r.m.DryRun }
func (r *retireRunResolver) FromRun() *graphql.ID    { return gidp(r.m.FromRun) }
func (r *retireRunResolver) RequestedBy() string     { return r.m.RequestedBy }
func (r *retireRunResolver) StartedAt() graphql.Time { return graphql.Time{Time: r.m.StartedAt.UTC()} }
func (r *retireRunResolver) FinishedAt() *graphql.Time {
	return utcTime(r.m.FinishedAt)
}
func (r *retireRunResolver) Error() *string     { return r.m.Error }
func (r *retireRunResolver) Total() int32       { return int32(r.m.Total) }
func (r *retireRunResolver) Pending() int32     { return int32(r.m.Pending) }
func (r *retireRunResolver) Retired() int32     { return int32(r.m.Retired) }
func (r *retireRunResolver) WouldRetire() int32 { return int32(r.m.WouldRetire) }
func (r *retireRunResolver) Skipped() int32     { return int32(r.m.Skipped) }
func (r *retireRunResolver) Failed() int32      { return int32(r.m.Failed) }
func (r *retireRunResolver) Bytes() float64     { return float64(r.m.Bytes) }

// Items are a page of the run's titles.
func (r *retireRunResolver) Items(ctx context.Context, args struct {
	First  int32
	After  int32
	Result *string
}) ([]*retireItemResolver, error) {
	result := ""
	if args.Result != nil {
		result = listResults[*args.Result]
	}
	items, err := r.svc.Items(ctx, r.m.ID, int(args.First), int(args.After), result)
	if err != nil {
		return nil, err
	}
	out := make([]*retireItemResolver, 0, len(items))
	for i := range items {
		out = append(out, &retireItemResolver{m: items[i]})
	}
	return out, nil
}

type retireItemResolver struct{ m library.ListItem }

func (r *retireItemResolver) Position() int32        { return int32(r.m.Position) }
func (r *retireItemResolver) ItemID() graphql.ID     { return graphql.ID(r.m.ItemID) }
func (r *retireItemResolver) Title() *string         { return r.m.Title }
func (r *retireItemResolver) SourceID() *graphql.ID  { return gidp(r.m.SourceID) }
func (r *retireItemResolver) VersionID() *graphql.ID { return gidp(r.m.VersionID) }
func (r *retireItemResolver) Result() *string        { return resultName(r.m.Result) }
func (r *retireItemResolver) Reason() *string        { return r.m.Reason }
func (r *retireItemResolver) Bytes() *float64 {
	if r.m.Bytes == nil {
		return nil
	}
	b := float64(*r.m.Bytes)
	return &b
}
func (r *retireItemResolver) At() *graphql.Time { return utcTime(r.m.At) }
