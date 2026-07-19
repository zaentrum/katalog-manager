package graph

import (
	"context"
	"errors"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// errNotConfigured is returned by service-backed actions when the relevant
// integration is not wired (e.g. download-gateway URL unset).
var errNotConfigured = errors.New("feature not configured")

// Services bundles the integration services a resolver may call. Fields are
// optional (nil-safe); main wires the ones that are configured.
type Services struct {
	Scanner   ScanRunner
	Enricher  Enricher
	Packager  Packager
	Validator Validator
	Trailers  TrailerFetcher
	DLGateway DownloadGateway
}

// Narrow interfaces the graph layer depends on (implemented by integration
// packages; structural — no import cycle).
type ScanRunner interface {
	Trigger(ctx context.Context, source string) (jobID string, err error)
}
type Enricher interface {
	EnrichOne(ctx context.Context, id string) (status string, message string, err error)
	IdentifyOne(ctx context.Context, id, title string, tmdbID *int64) (status string, message string, err error)
	EnrichPending(ctx context.Context, limit int32, typ string) (queued int32, err error)
	BackfillEpisodeBackdrops(ctx context.Context) (artworkData, artwork int32, err error)
	RetryNotFound(ctx context.Context, typ string) (reset int32, err error)
}
type Packager interface {
	PackageItem(ctx context.Context, id string) (PackageResult, error)
}
type Validator interface {
	ValidateItem(ctx context.Context, id string) (ValidateResult, error)
}
type TrailerFetcher interface {
	FetchTrailers(ctx context.Context, id string) (FetchTrailersResult, error)
}
type DownloadGateway interface {
	Add(ctx context.Context, adapter, source, title, wantedItemID string) (clientJobID, message string, err error)
	Cancel(ctx context.Context, adapter, clientJobID string) (message string, err error)
	Clients(ctx context.Context) (string, error)
}

// Resolver is the GraphQL root (Query + Mutation).
type Resolver struct {
	store *store.Store
	cfg   config.Config
	svc   Services
}

func NewResolver(s *store.Store, cfg config.Config, svc Services) *Resolver {
	return &Resolver{store: s, cfg: cfg, svc: svc}
}

func deref32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func strDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func f2i64(p *float64) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

func idStr(p *graphql.ID) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

// ===================== Queries =====================

func (r *Resolver) Item(ctx context.Context, args struct{ ID graphql.ID }) (*itemResolver, error) {
	row, err := r.store.GetItem(ctx, string(args.ID))
	if err != nil || row == nil {
		return nil, err
	}
	return &itemResolver{m: &row.Item, s: r.store}, nil
}

type itemsArgs struct {
	Type   *string
	Genre  *string
	Year   *int32
	Search *string
	Limit  *int32
	Offset *int32
}

func (r *Resolver) listItems(ctx context.Context, f store.ItemFilter) ([]*itemResolver, error) {
	rows, err := r.store.ListItems(ctx, f)
	if err != nil {
		return nil, err
	}
	return newItemResolvers(rows, r.store), nil
}

func (r *Resolver) Items(ctx context.Context, args itemsArgs) ([]*itemResolver, error) {
	return r.listItems(ctx, store.ItemFilter{
		Type: args.Type, Genre: args.Genre, Year: args.Year, Search: args.Search,
		Limit: deref32(args.Limit), Offset: deref32(args.Offset),
	})
}

func (r *Resolver) Movies(ctx context.Context, args itemsArgs) ([]*itemResolver, error) {
	t := "movie"
	return r.listItems(ctx, store.ItemFilter{
		Type: &t, Genre: args.Genre, Year: args.Year, Search: args.Search,
		Limit: deref32(args.Limit), Offset: deref32(args.Offset),
	})
}

func (r *Resolver) Series(ctx context.Context, args itemsArgs) ([]*itemResolver, error) {
	t := "series"
	return r.listItems(ctx, store.ItemFilter{
		Type: &t, Genre: args.Genre, Year: args.Year, Search: args.Search,
		Limit: deref32(args.Limit), Offset: deref32(args.Offset),
	})
}

func (r *Resolver) Episodes(ctx context.Context, args struct {
	SeriesID *graphql.ID
	SeasonID *graphql.ID
	Limit    *int32
	Offset   *int32
}) ([]*itemResolver, error) {
	t := "episode"
	f := store.ItemFilter{Type: &t, Limit: deref32(args.Limit), Offset: deref32(args.Offset)}
	if args.SeasonID != nil {
		s := string(*args.SeasonID)
		f.ParentID = &s
	} else if args.SeriesID != nil {
		s := string(*args.SeriesID)
		f.SeriesID = &s
	}
	return r.listItems(ctx, f)
}

func (r *Resolver) Albums(ctx context.Context, args struct {
	Limit  *int32
	Offset *int32
}) ([]*itemResolver, error) {
	t := "album"
	return r.listItems(ctx, store.ItemFilter{Type: &t, Limit: deref32(args.Limit), Offset: deref32(args.Offset)})
}

func (r *Resolver) SearchItems(ctx context.Context, args struct {
	Q      *string
	Type   *string
	Genre  *string
	Year   *int32
	Limit  *int32
	Offset *int32
}) (*searchResultResolver, error) {
	limit := deref32(args.Limit)
	offset := deref32(args.Offset)
	items, scores, err := r.store.SearchItems(ctx, store.SearchFilter{
		Q: args.Q, Type: args.Type, Genre: args.Genre, Year: args.Year, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	hits := make([]SearchItem, 0, len(items))
	for i := range items {
		var sc *float64
		if i < len(scores) {
			sc = scores[i]
		}
		hits = append(hits, SearchItem{
			ID: items[i].ID, Type: items[i].Type, Title: items[i].Title,
			Year: items[i].Year, Rating: items[i].Rating, Score: sc,
		})
	}
	resLimit := limit
	if resLimit <= 0 || resLimit > 200 {
		resLimit = 50
	}
	resOffset := offset
	if resOffset < 0 {
		resOffset = 0
	}
	return &searchResultResolver{m: SearchResult{
		Items: hits, Total: int32(len(hits)), Limit: resLimit, Offset: resOffset,
	}}, nil
}

func (r *Resolver) ScanJob(ctx context.Context, args struct{ ID graphql.ID }) (*scanJobResolver, error) {
	j, err := r.store.GetScanJob(ctx, string(args.ID))
	if err != nil || j == nil {
		return nil, err
	}
	return &scanJobResolver{m: j}, nil
}

func (r *Resolver) ScanJobs(ctx context.Context, args struct{ Limit *int32 }) ([]*scanJobResolver, error) {
	js, err := r.store.ListScanJobs(ctx, deref32(args.Limit))
	if err != nil {
		return nil, err
	}
	out := make([]*scanJobResolver, 0, len(js))
	for _, j := range js {
		out = append(out, &scanJobResolver{m: j})
	}
	return out, nil
}

func (r *Resolver) Activity(ctx context.Context, args struct{ Limit *int32 }) ([]*activityEventResolver, error) {
	rows, err := r.store.ActivityFeed(ctx, deref32(args.Limit))
	if err != nil {
		return nil, err
	}
	out := make([]*activityEventResolver, 0, len(rows))
	for _, x := range rows {
		out = append(out, &activityEventResolver{m: x})
	}
	return out, nil
}

func (r *Resolver) DownloadJobs(ctx context.Context, args struct{ Limit *int32 }) ([]*downloadJobResolver, error) {
	js, err := r.store.ListDownloadJobs(ctx, deref32(args.Limit))
	if err != nil {
		return nil, err
	}
	out := make([]*downloadJobResolver, 0, len(js))
	for _, j := range js {
		out = append(out, &downloadJobResolver{m: j})
	}
	return out, nil
}

func (r *Resolver) DownloadClients(ctx context.Context) (string, error) {
	if r.svc.DLGateway == nil {
		return "[]", nil
	}
	return r.svc.DLGateway.Clients(ctx)
}

func (r *Resolver) Settings(ctx context.Context) ([]*settingResolver, error) {
	ss, err := r.store.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*settingResolver, 0, len(ss))
	for _, s := range ss {
		out = append(out, &settingResolver{m: s})
	}
	return out, nil
}

func (r *Resolver) Genres(ctx context.Context) ([]*genreResolver, error) {
	gs, err := r.store.ListGenres(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*genreResolver, 0, len(gs))
	for _, g := range gs {
		out = append(out, &genreResolver{m: g})
	}
	return out, nil
}

func (r *Resolver) People(ctx context.Context) ([]*personResolver, error) {
	ps, err := r.store.ListPeople(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*personResolver, 0, len(ps))
	for _, p := range ps {
		out = append(out, &personResolver{m: p})
	}
	return out, nil
}

func (r *Resolver) EnrichStatus(ctx context.Context) *enrichStatusResolver {
	return &enrichStatusResolver{tmdbEnabled: r.cfg.TMDBEnabled()}
}

func (r *Resolver) EnrichmentStatusCodes(ctx context.Context) ([]*enrichmentStatusCodeResolver, error) {
	cs, err := r.store.ListEnrichmentStatusCodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*enrichmentStatusCodeResolver, 0, len(cs))
	for _, c := range cs {
		out = append(out, &enrichmentStatusCodeResolver{m: c})
	}
	return out, nil
}

// ===================== Mutations =====================

func (r *Resolver) TriggerScan(ctx context.Context, args struct{ Source *string }) (*scanJobResolver, error) {
	if r.svc.Scanner == nil {
		return nil, errNotConfigured
	}
	source := "nfs"
	if args.Source != nil && *args.Source != "" {
		source = *args.Source
	}
	jobID, err := r.svc.Scanner.Trigger(ctx, source)
	if err != nil {
		return nil, err
	}
	j, err := r.store.GetScanJob(ctx, jobID)
	if err != nil || j == nil {
		return nil, err
	}
	return &scanJobResolver{m: j}, nil
}

func (r *Resolver) EnrichOne(ctx context.Context, args struct{ ID graphql.ID }) (*enrichResultResolver, error) {
	if r.svc.Enricher == nil {
		return nil, errNotConfigured
	}
	status, msg, err := r.svc.Enricher.EnrichOne(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	res := EnrichResult{ItemID: string(args.ID), Status: status}
	if msg != "" {
		res.Message = &msg
	}
	return &enrichResultResolver{m: res}, nil
}

// Identify re-matches an item from an operator-chosen title and/or TMDB id
// (for cases the automatic search can't resolve, e.g. a filename-derived title).
func (r *Resolver) Identify(ctx context.Context, args struct {
	ID     graphql.ID
	Title  *string
	TmdbID *int32
}) (*enrichResultResolver, error) {
	if r.svc.Enricher == nil {
		return nil, errNotConfigured
	}
	var tmdbID *int64
	if args.TmdbID != nil {
		v := int64(*args.TmdbID)
		tmdbID = &v
	}
	status, msg, err := r.svc.Enricher.IdentifyOne(ctx, string(args.ID), strDeref(args.Title), tmdbID)
	if err != nil {
		return nil, err
	}
	res := EnrichResult{ItemID: string(args.ID), Status: status}
	if msg != "" {
		res.Message = &msg
	}
	return &enrichResultResolver{m: res}, nil
}

func (r *Resolver) EnrichPending(ctx context.Context, args struct {
	Limit *int32
	Type  *string
}) (*enrichPendingResultResolver, error) {
	if r.svc.Enricher == nil {
		return nil, errNotConfigured
	}
	typ := strDeref(args.Type)
	queued, err := r.svc.Enricher.EnrichPending(ctx, deref32(args.Limit), typ)
	if err != nil {
		return nil, err
	}
	res := EnrichPendingResult{Queued: queued}
	if typ != "" {
		res.Type = &typ
	}
	return &enrichPendingResultResolver{m: res}, nil
}

func (r *Resolver) BackfillEpisodeBackdrops(ctx context.Context) (*backfillResultResolver, error) {
	if r.svc.Enricher == nil {
		return nil, errNotConfigured
	}
	ad, aw, err := r.svc.Enricher.BackfillEpisodeBackdrops(ctx)
	if err != nil {
		return nil, err
	}
	return &backfillResultResolver{artworkData: ad, artwork: aw}, nil
}

func (r *Resolver) RetryNotFound(ctx context.Context, args struct{ Type *string }) (*retryResultResolver, error) {
	if r.svc.Enricher == nil {
		return nil, errNotConfigured
	}
	typ := strDeref(args.Type)
	reset, err := r.svc.Enricher.RetryNotFound(ctx, typ)
	if err != nil {
		return nil, err
	}
	res := RetryResult{Reset: reset}
	if typ != "" {
		res.Type = &typ
	}
	return &retryResultResolver{m: res}, nil
}

func (r *Resolver) PackageItem(ctx context.Context, args struct{ ID graphql.ID }) (*packageResultResolver, error) {
	if r.svc.Packager == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Packager.PackageItem(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &packageResultResolver{m: res}, nil
}

func (r *Resolver) ValidateItem(ctx context.Context, args struct{ ID graphql.ID }) (*validateResultResolver, error) {
	if r.svc.Validator == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Validator.ValidateItem(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &validateResultResolver{m: res}, nil
}

func (r *Resolver) FetchTrailers(ctx context.Context, args struct{ ID graphql.ID }) (*fetchTrailersResultResolver, error) {
	if r.svc.Trailers == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Trailers.FetchTrailers(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &fetchTrailersResultResolver{m: res}, nil
}

func (r *Resolver) AddDownload(ctx context.Context, args struct {
	Adapter      string
	Source       string
	Title        *string
	WantedItemID *string
}) (*downloadCommandResultResolver, error) {
	if r.svc.DLGateway == nil {
		return nil, errNotConfigured
	}
	clientJobID, msg, err := r.svc.DLGateway.Add(ctx, args.Adapter, args.Source, strDeref(args.Title), strDeref(args.WantedItemID))
	res := DownloadCommandResult{OK: err == nil, Adapter: &args.Adapter}
	if clientJobID != "" {
		res.ClientJobID = &clientJobID
	}
	if msg != "" {
		res.Message = &msg
	}
	if err != nil {
		m := err.Error()
		res.Message = &m
	}
	return &downloadCommandResultResolver{m: res}, nil
}

func (r *Resolver) CancelDownload(ctx context.Context, args struct {
	Adapter     string
	ClientJobID string
}) (*downloadCommandResultResolver, error) {
	if r.svc.DLGateway == nil {
		return nil, errNotConfigured
	}
	msg, err := r.svc.DLGateway.Cancel(ctx, args.Adapter, args.ClientJobID)
	res := DownloadCommandResult{OK: err == nil, Adapter: &args.Adapter, ClientJobID: &args.ClientJobID}
	if msg != "" {
		res.Message = &msg
	}
	if err != nil {
		m := err.Error()
		res.Message = &m
	}
	return &downloadCommandResultResolver{m: res}, nil
}

type itemInput struct {
	Type           *string
	Title          *string
	SortTitle      *string
	Year           *int32
	Description    *string
	Rating         *float64
	DurationMs     *float64
	ParentID       *graphql.ID
	SeasonNumber   *int32
	EpisodeNumber  *int32
	Tagline        *string
	MetadataLocked *bool
}

func (in itemInput) toWrite() store.ItemWrite {
	return store.ItemWrite{
		Type: in.Type, Title: in.Title, SortTitle: in.SortTitle, Year: in.Year,
		Description: in.Description, Rating: in.Rating, DurationMs: f2i64(in.DurationMs),
		ParentID: idStr(in.ParentID), SeasonNumber: in.SeasonNumber, EpisodeNumber: in.EpisodeNumber,
		Tagline: in.Tagline, MetadataLocked: in.MetadataLocked,
	}
}

func (r *Resolver) CreateItem(ctx context.Context, args struct{ Input itemInput }) (*itemResolver, error) {
	it, err := r.store.CreateItem(ctx, args.Input.toWrite())
	if err != nil {
		return nil, err
	}
	return newItemResolver(it, r.store), nil
}

func (r *Resolver) UpdateItem(ctx context.Context, args struct {
	ID    graphql.ID
	Input itemInput
}) (*itemResolver, error) {
	it, err := r.store.UpdateItem(ctx, string(args.ID), args.Input.toWrite())
	if err != nil || it == nil {
		return nil, err
	}
	return newItemResolver(it, r.store), nil
}

func (r *Resolver) DeleteItem(ctx context.Context, args struct{ ID graphql.ID }) (bool, error) {
	return r.store.DeleteItem(ctx, string(args.ID))
}

func (r *Resolver) SetItemGenres(ctx context.Context, args struct {
	ID     graphql.ID
	Genres []string
}) (*itemResolver, error) {
	if err := r.store.SetItemGenres(ctx, string(args.ID), args.Genres); err != nil {
		return nil, err
	}
	it, err := r.store.GetItemBase(ctx, string(args.ID))
	if err != nil || it == nil {
		return nil, err
	}
	return newItemResolver(it, r.store), nil
}

func (r *Resolver) SetItemTags(ctx context.Context, args struct {
	ID   graphql.ID
	Tags []string
}) (*itemResolver, error) {
	if err := r.store.SetItemTags(ctx, string(args.ID), args.Tags); err != nil {
		return nil, err
	}
	it, err := r.store.GetItemBase(ctx, string(args.ID))
	if err != nil || it == nil {
		return nil, err
	}
	return newItemResolver(it, r.store), nil
}

func (r *Resolver) CreateSetting(ctx context.Context, args struct {
	Key         string
	ValueText   string
	ValueType   *string
	Description *string
}) (*settingResolver, error) {
	valueType := "string"
	if args.ValueType != nil && *args.ValueType != "" {
		valueType = *args.ValueType
	}
	s, err := r.store.CreateSetting(ctx, args.Key, args.ValueText, valueType, args.Description)
	if err != nil {
		return nil, err
	}
	return &settingResolver{m: s}, nil
}

func (r *Resolver) UpdateSetting(ctx context.Context, args struct {
	ID          graphql.ID
	ValueText   *string
	ValueType   *string
	Description *string
}) (*settingResolver, error) {
	s, err := r.store.UpdateSetting(ctx, string(args.ID), args.ValueText, args.ValueType, args.Description)
	if err != nil || s == nil {
		return nil, err
	}
	return &settingResolver{m: s}, nil
}

func (r *Resolver) DeleteSetting(ctx context.Context, args struct{ ID graphql.ID }) (bool, error) {
	return r.store.DeleteSetting(ctx, string(args.ID))
}
