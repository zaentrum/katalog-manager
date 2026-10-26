package graph

import (
	"context"
	"fmt"
	"strings"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
)

// Compliance is the library's compliance check: the rules the workers report
// they run with, and how each movie and episode fares against them, as the
// check last found (implemented by compliance).
type Compliance interface {
	PipelineRules(ctx context.Context) (PipelineRules, error)
	// Report is a page of the titles f matches that break a rule, in the
	// order o, first of them after the cursor after ("" from the first; a
	// cursor of the same order).
	Report(ctx context.Context, f ComplianceFilter, o ComplianceOrder, first int32, after string) (ComplianceReport, error)
	// Matching are the titles f matches that break a rule, as a request by
	// the filter takes them: those it queues, in the report's order (the
	// report's queueable), and the others as queueRework would say them,
	// those waiting or sent in the re-encode queue already, which keeps
	// such a title as it is, and those processing again can't help (a row's
	// reworkBlocked), which the queue skips.
	Matching(ctx context.Context, f ComplianceFilter) (ComplianceMatching, error)
	// Refresh has every title of kind ("" every kind) checked again, and
	// answers whether a pass was set going.
	Refresh(ctx context.Context, kind string) (bool, error)
	// Queued says what a request to process titles again did, as the queue
	// answered it, in words; matching says it named its titles by a filter.
	Queued(ctx context.Context, res ReencodeEnqueued, matching bool) (ComplianceQueued, error)
}

// Rework queues titles to be processed again, through the paced re-encode
// queue (implemented by retry).
type Rework interface {
	EnqueueReencode(ctx context.Context, req ReencodeRequest) (ReencodeEnqueued, error)
}

// The kinds of title the check holds against the rules, as the catalog names
// them.
const (
	ComplianceMovie   = "movie"
	ComplianceEpisode = "episode"
)

// ComplianceFilter is what a report narrows the titles to: a kind ("" both),
// those that break any of rules (none: any rule), and those whose title or
// whose series' title holds search.
type ComplianceFilter struct {
	Kind   string
	Rules  []string
	Search string
}

// ComplianceMatching are the titles a filter matches that break a rule, as
// a request by the filter takes them: those it queues (Queueable, the
// report's queueable, in the report's order), how many of the others wait
// or are sent in the re-encode queue already (AlreadyQueued), and those
// processing again can't help, each a skip of its kind (Skipped).
type ComplianceMatching struct {
	Queueable     []string
	AlreadyQueued int32
	Skipped       []ReencodeSkipped
}

// Of is what a request by the filter did: what the queue did of the titles
// it queued (res), and of the others what queueRework says of them, those
// in the queue already and those it skips.
func (m ComplianceMatching) Of(res ReencodeEnqueued) ReencodeEnqueued {
	res.AlreadyQueued += m.AlreadyQueued
	res.Skipped = append(res.Skipped, m.Skipped...)
	return res
}

// ComplianceOrder is the order a report's rows are in: a ComplianceSort
// ("" the default, PACKAGE_SIZE), descending or not.
type ComplianceOrder struct {
	Sort       string
	Descending bool
}

// RuleSetting is one rule a worker reported: where it is in the report
// (key), what it is (label) and its value, written out.
type RuleSetting struct {
	Key, Label, Value string
	// Detail is the configuration as the worker reported it, where the
	// value says it in words ("source:hevc"); nil where the value is it.
	Detail *string
}

// WorkerRules is what a worker last reported of the rules it runs with.
type WorkerRules struct {
	Worker     string
	Version    *string
	ReportedAt time.Time
	Rules      []RuleSetting
}

// PipelineRules are the rules the transcoder and the packager report; nil
// for a worker that never reported.
type PipelineRules struct{ Transcoder, Packager *WorkerRules }

// ComplianceFinding is a rule a title breaks: the rule, what the check
// found, in words, what it measured and the rule's limit, and what a tool
// said of it (Detail), never in the words.
type ComplianceFinding struct {
	Rule, Text              string
	Measured, Limit, Detail *string
}

// ComplianceRow is a title that breaks a rule, as the check last found it.
type ComplianceRow struct {
	ItemID, Title               string
	Year                        *int32
	Kind                        string
	SeriesID, SeriesTitle       *string
	Season, Episode             *int32
	Findings                    []ComplianceFinding
	OriginalBytes, PackageBytes *float64
	EstimatedBytes              *float64
	Queue, QueueReason          *string
	QueueDetail                 *string
	ReworkBlocked               *string
	EvaluatedAt                 time.Time
	// What the report's columns show of it: its video's size and codec, its
	// video's bit rate (bit/s) and whether that was estimated, the caps that
	// hold for it (bit/s, bytes), its duration (s), its package's and its
	// original's audio, and what processing it again does (Outcome).
	Resolution, VideoCodec               *string
	VideoBitrate                         *float64
	VideoBitrateEstimated                bool
	BitrateCap, SizeCap, DurationSeconds *float64
	PackageAudio, OriginalAudio          *string
	Outcome                              *string
}

// ComplianceSummary is how many of the titles a report covers break a rule,
// the size of their packages and what those are to be at most once processed
// again.
type ComplianceSummary struct {
	Rule           string
	Titles         int32
	PackageBytes   float64
	EstimatedBytes *float64
}

// ComplianceTotals are the titles a report's whole filter matches: how many,
// the sizes of their originals and packages, and what those packages are to
// be after they are processed again (nil when that is known of none).
type ComplianceTotals struct {
	Titles                      int32
	OriginalBytes, PackageBytes float64
	EstimatedBytes              *float64
}

// ComplianceQueueCounts are the re-encode queue's titles by state: queued,
// processing (sent), processed (done) and failed.
type ComplianceQueueCounts struct {
	Queued, Processing, Processed, Failed int32
}

// ComplianceReport is a page of the titles that break a rule.
type ComplianceReport struct {
	Rules        PipelineRules
	NotEvaluated []string
	EvaluatedAt  *time.Time
	Evaluating   bool
	Pending      int32
	Total        int32
	// CheckedTitles are the titles of the report's kind and search the
	// check covers and has checked, whether they break a rule or not.
	CheckedTitles int32
	Matching      ComplianceTotals
	Queueable     ComplianceTotals
	Queue         ComplianceQueueCounts
	Summary       []ComplianceSummary
	Rows          []ComplianceRow
	NextCursor    *string
}

// ComplianceSkip is a title a request to process titles again didn't queue:
// its id, its name as a list names it, and why, the clause a sentence says
// after a colon.
type ComplianceSkip struct{ ItemID, Title, Reason string }

// ComplianceQueued is what a request to process titles again did, each title
// it skipped among SkippedTitles, as the console says it: a toast of the
// titles queued (QueuedText), a plain one of those already in the queue
// (AlreadyText) and a banner line of those not queued, or of nothing queued
// (NotQueuedText), each nil when it has nothing to say; Message is the three
// in one text.
type ComplianceQueued struct {
	Queued, AlreadyQueued, Skipped         int32
	SkippedTitles                          []ComplianceSkip
	QueuedText, AlreadyText, NotQueuedText *string
	Message                                string
}

// kinds maps a ComplianceKind to the catalog's name of it.
var kinds = map[string]string{"MOVIE": ComplianceMovie, "EPISODE": ComplianceEpisode}

// filterOf is the filter of the report's arguments.
func filterOf(kind *string, rules *[]string, search *string) ComplianceFilter {
	var f ComplianceFilter
	if kind != nil {
		f.Kind = kinds[*kind]
	}
	if rules != nil {
		f.Rules = *rules
	}
	if search != nil {
		f.Search = strings.TrimSpace(*search)
	}
	return f
}

// PipelineRules are the rules the transcoder and the packager last reported
// they run with.
func (r *Resolver) PipelineRules(ctx context.Context) (*pipelineRulesResolver, error) {
	if err := r.allow(ctx, "Query.pipelineRules"); err != nil {
		return nil, err
	}
	if r.svc.Compliance == nil {
		return nil, errNotConfigured
	}
	p, err := r.svc.Compliance.PipelineRules(ctx)
	if err != nil {
		return nil, err
	}
	return &pipelineRulesResolver{m: p}, nil
}

// LibraryCompliance is a page of the movies and episodes that break a rule
// the workers report, as the check last found them.
func (r *Resolver) LibraryCompliance(ctx context.Context, args struct {
	Kind       *string
	Rules      *[]string
	Search     *string
	First      int32
	After      *string
	Sort       string
	Descending bool
}) (*complianceReportResolver, error) {
	if err := r.allow(ctx, "Query.libraryCompliance"); err != nil {
		return nil, err
	}
	if r.svc.Compliance == nil {
		return nil, errNotConfigured
	}
	if args.First < 0 {
		return nil, fmt.Errorf("first is %d: a page holds 0 titles or more", args.First)
	}
	rep, err := r.svc.Compliance.Report(ctx, filterOf(args.Kind, args.Rules, args.Search),
		ComplianceOrder{Sort: args.Sort, Descending: args.Descending}, args.First, strDeref(args.After))
	if err != nil {
		return nil, err
	}
	return &complianceReportResolver{m: rep}, nil
}

// QueueRework queues the titles items names to be encoded again under the
// pipeline's current rules, through the re-encode queue.
func (r *Resolver) QueueRework(ctx context.Context, args struct{ Items []graphql.ID }) (*complianceQueuedResolver, error) {
	if err := r.allow(ctx, "Mutation.queueRework"); err != nil {
		return nil, err
	}
	if r.svc.Compliance == nil || r.svc.Rework == nil {
		return nil, errNotConfigured
	}
	items := make([]string, 0, len(args.Items))
	for _, id := range args.Items {
		if s := strings.TrimSpace(string(id)); s != "" {
			items = append(items, s)
		}
	}
	return r.rework(ctx, items, nil)
}

// rework queues items to be processed again, and says what that did, with
// what m says of the titles of a filter it didn't queue when the request
// named its titles by a filter (m not nil); none is said so.
func (r *Resolver) rework(ctx context.Context, items []string, m *ComplianceMatching) (*complianceQueuedResolver, error) {
	var res ReencodeEnqueued
	if len(items) > 0 {
		var err error
		if res, err = r.svc.Rework.EnqueueReencode(ctx, ReencodeRequest{Items: items}); err != nil {
			return nil, err
		}
	}
	if m != nil {
		res = m.Of(res)
	}
	q, err := r.svc.Compliance.Queued(ctx, res, m != nil)
	if err != nil {
		return nil, err
	}
	return &complianceQueuedResolver{m: q}, nil
}

// QueueReworkMatching queues the titles the filter matches that break a rule
// and that a request queues (the report's queueable), as queueRework does,
// and says of every title the filter matches what queueRework says of it:
// one in the queue already is counted so, one processing again can't help
// is skipped, saying why.
func (r *Resolver) QueueReworkMatching(ctx context.Context, args struct {
	Kind   *string
	Rules  *[]string
	Search *string
}) (*complianceQueuedResolver, error) {
	if err := r.allow(ctx, "Mutation.queueReworkMatching"); err != nil {
		return nil, err
	}
	if r.svc.Compliance == nil || r.svc.Rework == nil {
		return nil, errNotConfigured
	}
	m, err := r.svc.Compliance.Matching(ctx, filterOf(args.Kind, args.Rules, args.Search))
	if err != nil {
		return nil, err
	}
	return r.rework(ctx, m.Queueable, &m)
}

// RefreshCompliance has the titles of kind (every kind when omitted) checked
// against the rules again, in the background.
func (r *Resolver) RefreshCompliance(ctx context.Context, args struct{ Kind *string }) (bool, error) {
	if err := r.allow(ctx, "Mutation.refreshCompliance"); err != nil {
		return false, err
	}
	if r.svc.Compliance == nil {
		return false, errNotConfigured
	}
	kind := ""
	if args.Kind != nil {
		kind = kinds[*args.Kind]
	}
	return r.svc.Compliance.Refresh(ctx, kind)
}

// stamp writes a moment as the report says one: RFC 3339, in UTC.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

type pipelineRulesResolver struct{ m PipelineRules }

func (r *pipelineRulesResolver) Transcoder() *workerRulesResolver {
	return newWorkerRulesResolver(r.m.Transcoder)
}
func (r *pipelineRulesResolver) Packager() *workerRulesResolver {
	return newWorkerRulesResolver(r.m.Packager)
}

type workerRulesResolver struct{ m WorkerRules }

func newWorkerRulesResolver(m *WorkerRules) *workerRulesResolver {
	if m == nil {
		return nil
	}
	return &workerRulesResolver{m: *m}
}

func (r *workerRulesResolver) Worker() string     { return r.m.Worker }
func (r *workerRulesResolver) Version() *string   { return r.m.Version }
func (r *workerRulesResolver) ReportedAt() string { return stamp(r.m.ReportedAt) }
func (r *workerRulesResolver) Rules() []*ruleSettingResolver {
	out := make([]*ruleSettingResolver, 0, len(r.m.Rules))
	for i := range r.m.Rules {
		out = append(out, &ruleSettingResolver{m: r.m.Rules[i]})
	}
	return out
}

type ruleSettingResolver struct{ m RuleSetting }

func (r *ruleSettingResolver) Key() string     { return r.m.Key }
func (r *ruleSettingResolver) Label() string   { return r.m.Label }
func (r *ruleSettingResolver) Value() string   { return r.m.Value }
func (r *ruleSettingResolver) Detail() *string { return r.m.Detail }

type complianceReportResolver struct{ m ComplianceReport }

func (r *complianceReportResolver) Rules() *pipelineRulesResolver {
	return &pipelineRulesResolver{m: r.m.Rules}
}
func (r *complianceReportResolver) NotEvaluated() []string {
	if r.m.NotEvaluated == nil {
		return []string{}
	}
	return r.m.NotEvaluated
}
func (r *complianceReportResolver) EvaluatedAt() *string {
	if r.m.EvaluatedAt == nil {
		return nil
	}
	s := stamp(*r.m.EvaluatedAt)
	return &s
}
func (r *complianceReportResolver) Evaluating() bool { return r.m.Evaluating }
func (r *complianceReportResolver) Pending() int32   { return r.m.Pending }
func (r *complianceReportResolver) Total() int32     { return r.m.Total }
func (r *complianceReportResolver) CheckedTitles() *int32 {
	n := r.m.CheckedTitles
	return &n
}
func (r *complianceReportResolver) Summary() []*complianceSummaryResolver {
	out := make([]*complianceSummaryResolver, 0, len(r.m.Summary))
	for i := range r.m.Summary {
		out = append(out, &complianceSummaryResolver{m: r.m.Summary[i]})
	}
	return out
}
func (r *complianceReportResolver) Rows() []*complianceRowResolver {
	out := make([]*complianceRowResolver, 0, len(r.m.Rows))
	for i := range r.m.Rows {
		out = append(out, &complianceRowResolver{m: r.m.Rows[i]})
	}
	return out
}
func (r *complianceReportResolver) NextCursor() *string { return r.m.NextCursor }
func (r *complianceReportResolver) Matching() *complianceTotalsResolver {
	return &complianceTotalsResolver{m: r.m.Matching}
}
func (r *complianceReportResolver) Queueable() *complianceTotalsResolver {
	return &complianceTotalsResolver{m: r.m.Queueable}
}
func (r *complianceReportResolver) Queue() *complianceQueueCountsResolver {
	return &complianceQueueCountsResolver{m: r.m.Queue}
}

type complianceTotalsResolver struct{ m ComplianceTotals }

func (r *complianceTotalsResolver) Titles() int32            { return r.m.Titles }
func (r *complianceTotalsResolver) OriginalBytes() float64   { return r.m.OriginalBytes }
func (r *complianceTotalsResolver) PackageBytes() float64    { return r.m.PackageBytes }
func (r *complianceTotalsResolver) EstimatedBytes() *float64 { return r.m.EstimatedBytes }

type complianceQueueCountsResolver struct{ m ComplianceQueueCounts }

func (r *complianceQueueCountsResolver) Queued() int32     { return r.m.Queued }
func (r *complianceQueueCountsResolver) Processing() int32 { return r.m.Processing }
func (r *complianceQueueCountsResolver) Processed() int32  { return r.m.Processed }
func (r *complianceQueueCountsResolver) Failed() int32     { return r.m.Failed }

type complianceSummaryResolver struct{ m ComplianceSummary }

func (r *complianceSummaryResolver) Rule() string             { return r.m.Rule }
func (r *complianceSummaryResolver) Titles() int32            { return r.m.Titles }
func (r *complianceSummaryResolver) PackageBytes() float64    { return r.m.PackageBytes }
func (r *complianceSummaryResolver) EstimatedBytes() *float64 { return r.m.EstimatedBytes }

type complianceRowResolver struct{ m ComplianceRow }

func (r *complianceRowResolver) ItemID() graphql.ID      { return gid(r.m.ItemID) }
func (r *complianceRowResolver) Title() string           { return r.m.Title }
func (r *complianceRowResolver) Year() *int32            { return r.m.Year }
func (r *complianceRowResolver) Kind() string            { return strings.ToUpper(r.m.Kind) }
func (r *complianceRowResolver) SeriesID() *graphql.ID   { return gidptr(r.m.SeriesID) }
func (r *complianceRowResolver) SeriesTitle() *string    { return r.m.SeriesTitle }
func (r *complianceRowResolver) Season() *int32          { return r.m.Season }
func (r *complianceRowResolver) Episode() *int32         { return r.m.Episode }
func (r *complianceRowResolver) OriginalBytes() *float64 { return r.m.OriginalBytes }
func (r *complianceRowResolver) PackageBytes() *float64  { return r.m.PackageBytes }
func (r *complianceRowResolver) EstimatedBytes() *float64 {
	return r.m.EstimatedBytes
}
func (r *complianceRowResolver) Queue() *string         { return r.m.Queue }
func (r *complianceRowResolver) QueueReason() *string   { return r.m.QueueReason }
func (r *complianceRowResolver) QueueDetail() *string   { return r.m.QueueDetail }
func (r *complianceRowResolver) Resolution() *string    { return r.m.Resolution }
func (r *complianceRowResolver) VideoCodec() *string    { return r.m.VideoCodec }
func (r *complianceRowResolver) VideoBitrate() *float64 { return r.m.VideoBitrate }
func (r *complianceRowResolver) VideoBitrateEstimated() bool {
	return r.m.VideoBitrateEstimated
}
func (r *complianceRowResolver) BitrateCap() *float64      { return r.m.BitrateCap }
func (r *complianceRowResolver) SizeCap() *float64         { return r.m.SizeCap }
func (r *complianceRowResolver) DurationSeconds() *float64 { return r.m.DurationSeconds }
func (r *complianceRowResolver) PackageAudio() *string     { return r.m.PackageAudio }
func (r *complianceRowResolver) OriginalAudio() *string    { return r.m.OriginalAudio }
func (r *complianceRowResolver) Outcome() *string          { return r.m.Outcome }
func (r *complianceRowResolver) ReworkBlocked() *string    { return r.m.ReworkBlocked }
func (r *complianceRowResolver) EvaluatedAt() string       { return stamp(r.m.EvaluatedAt) }
func (r *complianceRowResolver) Findings() []*complianceFindingResolver {
	out := make([]*complianceFindingResolver, 0, len(r.m.Findings))
	for i := range r.m.Findings {
		out = append(out, &complianceFindingResolver{m: r.m.Findings[i]})
	}
	return out
}

type complianceFindingResolver struct{ m ComplianceFinding }

func (r *complianceFindingResolver) Rule() string      { return r.m.Rule }
func (r *complianceFindingResolver) Text() string      { return r.m.Text }
func (r *complianceFindingResolver) Measured() *string { return r.m.Measured }
func (r *complianceFindingResolver) Limit() *string    { return r.m.Limit }
func (r *complianceFindingResolver) Detail() *string   { return r.m.Detail }

type complianceQueuedResolver struct{ m ComplianceQueued }

func (r *complianceQueuedResolver) Queued() int32          { return r.m.Queued }
func (r *complianceQueuedResolver) AlreadyQueued() int32   { return r.m.AlreadyQueued }
func (r *complianceQueuedResolver) Skipped() int32         { return r.m.Skipped }
func (r *complianceQueuedResolver) Message() string        { return r.m.Message }
func (r *complianceQueuedResolver) QueuedText() *string    { return r.m.QueuedText }
func (r *complianceQueuedResolver) AlreadyText() *string   { return r.m.AlreadyText }
func (r *complianceQueuedResolver) NotQueuedText() *string { return r.m.NotQueuedText }
func (r *complianceQueuedResolver) SkippedTitles() []*complianceSkipResolver {
	out := make([]*complianceSkipResolver, len(r.m.SkippedTitles))
	for i, s := range r.m.SkippedTitles {
		out[i] = &complianceSkipResolver{m: s}
	}
	return out
}

type complianceSkipResolver struct{ m ComplianceSkip }

func (r *complianceSkipResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *complianceSkipResolver) Title() string      { return r.m.Title }
func (r *complianceSkipResolver) Reason() string     { return r.m.Reason }
