package graph

import (
	"context"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
)

// Pipeline retries the processing steps, encodes a title again, and says
// what the pipeline holds (implemented by retry).
type Pipeline interface {
	RetryStep(ctx context.Context, itemID, step string) (RetryStepResult, error)
	RetryFailed(ctx context.Context, step string) (RetryFailedResult, error)
	ReencodeItem(ctx context.Context, id string) (ReencodeResult, error)
	Overview(ctx context.Context, step string, limit, offset int32) (ProcessingOverview, error)
	Policy(ctx context.Context) (RetryPolicyInfo, error)
	// ClearReencodeQueue deletes the queue's titles in states (by default the
	// waiting and the finished, not those sent), and answers how many.
	ClearReencodeQueue(ctx context.Context, states []string) (int32, error)
}

// ReencodeRequest is what a request to the re-encode queue names: titles (a
// series its episodes), the titles whose retire is held for their surround,
// every packaged movie and episode; together, each title once.
type ReencodeRequest struct {
	Items     []string
	Held, All bool
}

// ReencodeSkipped is a title a request named that the queue does not take,
// and why.
type ReencodeSkipped struct{ ItemID, Reason string }

// ReencodeEnqueued is what a request to the queue did: the titles queued,
// those that were queued already (waiting or sent), and those skipped.
type ReencodeEnqueued struct {
	Queued, AlreadyQueued int32
	Skipped               []ReencodeSkipped
}

// ReencodeQueue is the re-encode queue: its titles by state, of the live
// ones (queued or sent) the one enqueued first and the one enqueued last, and
// why the sweep sends none now (nil when it may).
type ReencodeQueue struct {
	Queued, Sent, Done, Failed int32
	Oldest, Newest             *QueuedTitle
	Idle                       *string
}

// QueuedTitle is a title in the queue.
type QueuedTitle struct {
	ItemID, State string
	EnqueuedAt    time.Time
}

// ReencodeResult says what a reencodeItem call did.
type ReencodeResult struct {
	ItemID    string
	Titles    int32 // the titles looked at: the movie or episode, or a series' episodes with a file
	Reencoded int32 // of them, those reset whose transcoder event went
	Busy      int32 // those left alone: their transcode or package running or waiting within its timeout
	NotSent   int32 // those reset whose event could not be sent: their transcode put back, failed
	Message   string
}

// RetryStepResult says what a retryStep call did.
type RetryStepResult struct {
	ItemID  string
	Step    string
	Retried bool
	Status  *string // the step's status after the call; nil for a step the item does not have
	Message string
}

// RetryFailedResult says what a retryFailed call did.
type RetryFailedResult struct {
	Retried int32 // steps whose trigger was sent again
	Items   int32 // items those events went to
	NotSent int32 // steps whose event could not be sent, left failed
	Step    *string
	Message string
}

// ProcessingOverview is what the pipeline holds and what failed.
type ProcessingOverview struct {
	Steps       []StepCounts
	Failed      []FailedStep
	FailedTotal int32
	Retry       RetryPolicyInfo
	// Reencode is the re-encode queue; nil without migration 043.
	Reencode *ReencodeQueue
}

// StepCounts are the items in each state of a step.
type StepCounts struct {
	Step                                                      string
	Pending, InProgress, Done, Failed, Skipped, NotApplicable int32
	Retrying, Stalled                                         int32
	TimeoutSeconds                                            int32
}

// FailedStep is a failed step and its item.
type FailedStep struct {
	ItemID, ItemTitle, ItemType string
	SeriesID, SeriesTitle       *string
	SeasonNumber, EpisodeNumber *int32
	Step                        string
	Failures                    int32
	LastError                   *string
	FailedAt, NextRetryAt       *time.Time
}

// RetryPolicyInfo is how the service retries a step, and whether it can.
type RetryPolicyInfo struct {
	Automatic, Available                                            bool
	Reason                                                          *string
	MaxAttempts, BackoffSeconds, BackoffMaxSeconds, IntervalSeconds int32
}

// ProcessingOverview says what the pipeline holds, step by step, and lists
// the failed steps.
func (r *Resolver) ProcessingOverview(ctx context.Context, args struct {
	Step   *string
	Limit  *int32
	Offset *int32
}) (*processingOverviewResolver, error) {
	if err := r.allow(ctx, "Query.processingOverview"); err != nil {
		return nil, err
	}
	if r.svc.Pipeline == nil {
		return nil, errNotConfigured
	}
	o, err := r.svc.Pipeline.Overview(ctx, strDeref(args.Step), deref32(args.Limit), deref32(args.Offset))
	if err != nil {
		return nil, err
	}
	return &processingOverviewResolver{m: o}, nil
}

// RetryPolicy says how the service retries a step.
func (r *Resolver) RetryPolicy(ctx context.Context) (*retryPolicyResolver, error) {
	if err := r.allow(ctx, "Query.retryPolicy"); err != nil {
		return nil, err
	}
	if r.svc.Pipeline == nil {
		return nil, errNotConfigured
	}
	p, err := r.svc.Pipeline.Policy(ctx)
	if err != nil {
		return nil, err
	}
	return &retryPolicyResolver{m: p}, nil
}

// RetryStep retries a step of an item now.
func (r *Resolver) RetryStep(ctx context.Context, args struct {
	ItemID graphql.ID
	Step   string
}) (*retryStepResultResolver, error) {
	if err := r.allow(ctx, "Mutation.retryStep"); err != nil {
		return nil, err
	}
	if r.svc.Pipeline == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Pipeline.RetryStep(ctx, string(args.ItemID), args.Step)
	if err != nil {
		return nil, err
	}
	return &retryStepResultResolver{m: res}, nil
}

// RetryFailed retries every failed step, of one step when given.
func (r *Resolver) RetryFailed(ctx context.Context, args struct{ Step *string }) (*retryFailedResultResolver, error) {
	if err := r.allow(ctx, "Mutation.retryFailed"); err != nil {
		return nil, err
	}
	if r.svc.Pipeline == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Pipeline.RetryFailed(ctx, strDeref(args.Step))
	if err != nil {
		return nil, err
	}
	return &retryFailedResultResolver{m: res}, nil
}

// ReencodeItem encodes a title again, a series' episodes, with the
// pipeline's current settings.
func (r *Resolver) ReencodeItem(ctx context.Context, args struct{ ID graphql.ID }) (*reencodeResultResolver, error) {
	if err := r.allow(ctx, "Mutation.reencodeItem"); err != nil {
		return nil, err
	}
	if r.svc.Pipeline == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Pipeline.ReencodeItem(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &reencodeResultResolver{m: res}, nil
}

type processingOverviewResolver struct{ m ProcessingOverview }

func (r *processingOverviewResolver) Steps() []*stepCountsResolver {
	out := make([]*stepCountsResolver, 0, len(r.m.Steps))
	for i := range r.m.Steps {
		out = append(out, &stepCountsResolver{m: r.m.Steps[i]})
	}
	return out
}
func (r *processingOverviewResolver) Failed() []*failedStepResolver {
	out := make([]*failedStepResolver, 0, len(r.m.Failed))
	for i := range r.m.Failed {
		out = append(out, &failedStepResolver{m: r.m.Failed[i]})
	}
	return out
}
func (r *processingOverviewResolver) FailedTotal() int32 { return r.m.FailedTotal }
func (r *processingOverviewResolver) ReencodeQueue() *reencodeQueueResolver {
	if r.m.Reencode == nil {
		return nil
	}
	return &reencodeQueueResolver{m: *r.m.Reencode}
}

type reencodeQueueResolver struct{ m ReencodeQueue }

func (r *reencodeQueueResolver) Queued() int32 { return r.m.Queued }
func (r *reencodeQueueResolver) Sent() int32   { return r.m.Sent }
func (r *reencodeQueueResolver) Done() int32   { return r.m.Done }
func (r *reencodeQueueResolver) Failed() int32 { return r.m.Failed }
func (r *reencodeQueueResolver) Oldest() *queuedTitleResolver {
	return newQueuedTitleResolver(r.m.Oldest)
}
func (r *reencodeQueueResolver) Newest() *queuedTitleResolver {
	return newQueuedTitleResolver(r.m.Newest)
}
func (r *reencodeQueueResolver) Idle() *string { return r.m.Idle }

type queuedTitleResolver struct{ m QueuedTitle }

func newQueuedTitleResolver(m *QueuedTitle) *queuedTitleResolver {
	if m == nil {
		return nil
	}
	return &queuedTitleResolver{m: *m}
}

func (r *queuedTitleResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *queuedTitleResolver) State() string      { return r.m.State }
func (r *queuedTitleResolver) EnqueuedAt() graphql.Time {
	return graphql.Time{Time: r.m.EnqueuedAt.UTC()}
}

// ClearReencodeQueue deletes the re-encode queue's titles in states, by
// default those waiting and those done or failed (a title sent is encoded,
// and its end still noted), and answers how many it deleted.
func (r *Resolver) ClearReencodeQueue(ctx context.Context, args struct{ States *[]string }) (int32, error) {
	if err := r.allow(ctx, "Mutation.clearReencodeQueue"); err != nil {
		return 0, err
	}
	if r.svc.Pipeline == nil {
		return 0, errNotConfigured
	}
	var states []string
	if args.States != nil {
		states = *args.States
	}
	return r.svc.Pipeline.ClearReencodeQueue(ctx, states)
}
func (r *processingOverviewResolver) Retry() *retryPolicyResolver {
	return &retryPolicyResolver{m: r.m.Retry}
}

type stepCountsResolver struct{ m StepCounts }

func (r *stepCountsResolver) Step() string          { return r.m.Step }
func (r *stepCountsResolver) Pending() int32        { return r.m.Pending }
func (r *stepCountsResolver) InProgress() int32     { return r.m.InProgress }
func (r *stepCountsResolver) Done() int32           { return r.m.Done }
func (r *stepCountsResolver) Failed() int32         { return r.m.Failed }
func (r *stepCountsResolver) Skipped() int32        { return r.m.Skipped }
func (r *stepCountsResolver) NotApplicable() int32  { return r.m.NotApplicable }
func (r *stepCountsResolver) Retrying() int32       { return r.m.Retrying }
func (r *stepCountsResolver) Stalled() int32        { return r.m.Stalled }
func (r *stepCountsResolver) TimeoutSeconds() int32 { return r.m.TimeoutSeconds }

type failedStepResolver struct{ m FailedStep }

func (r *failedStepResolver) ItemID() graphql.ID         { return gid(r.m.ItemID) }
func (r *failedStepResolver) ItemTitle() string          { return r.m.ItemTitle }
func (r *failedStepResolver) ItemType() string           { return r.m.ItemType }
func (r *failedStepResolver) SeriesID() *graphql.ID      { return gidptr(r.m.SeriesID) }
func (r *failedStepResolver) SeriesTitle() *string       { return r.m.SeriesTitle }
func (r *failedStepResolver) SeasonNumber() *int32       { return r.m.SeasonNumber }
func (r *failedStepResolver) EpisodeNumber() *int32      { return r.m.EpisodeNumber }
func (r *failedStepResolver) Step() string               { return r.m.Step }
func (r *failedStepResolver) Failures() int32            { return r.m.Failures }
func (r *failedStepResolver) LastError() *string         { return r.m.LastError }
func (r *failedStepResolver) FailedAt() *graphql.Time    { return gtime(r.m.FailedAt) }
func (r *failedStepResolver) NextRetryAt() *graphql.Time { return gtime(r.m.NextRetryAt) }

type retryPolicyResolver struct{ m RetryPolicyInfo }

func (r *retryPolicyResolver) Automatic() bool          { return r.m.Automatic }
func (r *retryPolicyResolver) Available() bool          { return r.m.Available }
func (r *retryPolicyResolver) Reason() *string          { return r.m.Reason }
func (r *retryPolicyResolver) MaxAttempts() int32       { return r.m.MaxAttempts }
func (r *retryPolicyResolver) BackoffSeconds() int32    { return r.m.BackoffSeconds }
func (r *retryPolicyResolver) BackoffMaxSeconds() int32 { return r.m.BackoffMaxSeconds }
func (r *retryPolicyResolver) IntervalSeconds() int32   { return r.m.IntervalSeconds }

type retryStepResultResolver struct{ m RetryStepResult }

func (r *retryStepResultResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *retryStepResultResolver) Step() string       { return r.m.Step }
func (r *retryStepResultResolver) Retried() bool      { return r.m.Retried }
func (r *retryStepResultResolver) Status() *string    { return r.m.Status }
func (r *retryStepResultResolver) Message() string    { return r.m.Message }

type retryFailedResultResolver struct{ m RetryFailedResult }

func (r *retryFailedResultResolver) Retried() int32  { return r.m.Retried }
func (r *retryFailedResultResolver) Items() int32    { return r.m.Items }
func (r *retryFailedResultResolver) NotSent() int32  { return r.m.NotSent }
func (r *retryFailedResultResolver) Step() *string   { return r.m.Step }
func (r *retryFailedResultResolver) Message() string { return r.m.Message }

type reencodeResultResolver struct{ m ReencodeResult }

func (r *reencodeResultResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *reencodeResultResolver) Titles() int32      { return r.m.Titles }
func (r *reencodeResultResolver) Reencoded() int32   { return r.m.Reencoded }
func (r *reencodeResultResolver) Busy() int32        { return r.m.Busy }
func (r *reencodeResultResolver) NotSent() int32     { return r.m.NotSent }
func (r *reencodeResultResolver) Message() string    { return r.m.Message }
