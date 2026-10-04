package graph

import (
	"context"
	"errors"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// errNotConfigured is returned by service-backed actions when the relevant
// integration is not wired (e.g. no TMDB key).
var errNotConfigured = errors.New("feature not configured")

// Services bundles the integration services a resolver may call. Fields are
// optional (nil-safe); main wires the ones that are configured.
type Services struct {
	Scanner   ScanRunner
	Enricher  Enricher
	People    PeopleRefresher
	Packager  Packager
	Validator Validator
	Remover   Remover
	Pipeline  Pipeline
}

// PeopleRefresher backfills people from TMDB (implemented by tmdb).
type PeopleRefresher interface {
	RefreshPeople(ctx context.Context, all bool) (PeopleRefreshResult, error)
}

// PeopleRefreshResult reports what a RefreshPeople run did.
type PeopleRefreshResult struct {
	TitlesRead          int32 // titles whose TMDB credits were read
	TitlesFailed        int32 // titles whose credits could not be read or stored
	TitlesLocked        int32 // titles that keep their credits (locked): left as they are
	PeopleMatched       int32 // people without a TMDB id who got theirs from a credit
	PeopleCreated       int32 // credited people the catalog did not hold yet
	CreditsAdded        int32 // credits titles gained from TMDB
	CreditsUpdated      int32 // credits whose job, character, order or episodes changed, in place
	CreditsDropped      int32 // credits TMDB no longer lists, gone
	CreditsRelinked     int32 // credits matched to a namesake, now on the person credited
	PeopleDeleted       int32 // people no title credits any more: deleted, in the deletion log
	PeopleFetched       int32 // people whose details and profile were read and stored
	PeopleLocked        int32 // people left alone: their record is locked
	PeopleNotFound      int32 // TMDB ids TMDB does not know (any more)
	PeopleFailed        int32 // people whose details could not be read
	PeopleWithoutTmdbID int32 // people who still have no TMDB id
	StartedAt           time.Time
	FinishedAt          time.Time
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

// Remover deletes an item (a series cascades to episodes) and optionally its
// files on disk, recording every item it removes in the deletion log with the
// caller and the reason (which may be empty). Implemented by itemactions.
type Remover interface {
	RemoveItem(ctx context.Context, id string, deleteFiles, deletePackages bool, reason string) (RemoveResult, error)
}

// RemoveResult reports what a RemoveItem call actually did.
type RemoveResult struct {
	Deleted         bool
	ItemsRemoved    int32
	FilesRemoved    int32
	PackagesRemoved int32
	Errors          []string
}

type Validator interface {
	ValidateItem(ctx context.Context, id string) (ValidateResult, error)
}

// Resolver is the GraphQL root (Query + Mutation). Every root field asks
// allow first (access.go).
type Resolver struct {
	store  *store.Store
	cfg    config.Config
	svc    Services
	access auth.Policy
}

func NewResolver(s *store.Store, cfg config.Config, svc Services) *Resolver {
	return &Resolver{store: s, cfg: cfg, svc: svc, access: cfg.Policy()}
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
	if err := r.allow(ctx, "Query.item"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.items"); err != nil {
		return nil, err
	}
	return r.listItems(ctx, store.ItemFilter{
		Type: args.Type, Genre: args.Genre, Year: args.Year, Search: args.Search,
		Limit: deref32(args.Limit), Offset: deref32(args.Offset),
	})
}

func (r *Resolver) Movies(ctx context.Context, args itemsArgs) ([]*itemResolver, error) {
	if err := r.allow(ctx, "Query.movies"); err != nil {
		return nil, err
	}
	t := "movie"
	return r.listItems(ctx, store.ItemFilter{
		Type: &t, Genre: args.Genre, Year: args.Year, Search: args.Search,
		Limit: deref32(args.Limit), Offset: deref32(args.Offset),
	})
}

func (r *Resolver) Series(ctx context.Context, args itemsArgs) ([]*itemResolver, error) {
	if err := r.allow(ctx, "Query.series"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.episodes"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.albums"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.searchItems"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.scanJob"); err != nil {
		return nil, err
	}
	j, err := r.store.GetScanJob(ctx, string(args.ID))
	if err != nil || j == nil {
		return nil, err
	}
	return &scanJobResolver{m: j}, nil
}

func (r *Resolver) ScanJobs(ctx context.Context, args struct{ Limit *int32 }) ([]*scanJobResolver, error) {
	if err := r.allow(ctx, "Query.scanJobs"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.activity"); err != nil {
		return nil, err
	}
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

func (r *Resolver) Settings(ctx context.Context) ([]*settingResolver, error) {
	if err := r.allow(ctx, "Query.settings"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.genres"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Query.people"); err != nil {
		return nil, err
	}
	ps, err := r.store.ListPeople(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*personResolver, 0, len(ps))
	for _, p := range ps {
		out = append(out, &personResolver{m: p, s: r.store})
	}
	return out, nil
}

func (r *Resolver) Person(ctx context.Context, args struct{ ID graphql.ID }) (*personResolver, error) {
	if err := r.allow(ctx, "Query.person"); err != nil {
		return nil, err
	}
	p, err := r.store.GetPerson(ctx, string(args.ID))
	if err != nil || p == nil {
		return nil, err
	}
	return &personResolver{m: p, s: r.store}, nil
}

func (r *Resolver) EnrichStatus(ctx context.Context) (*enrichStatusResolver, error) {
	if err := r.allow(ctx, "Query.enrichStatus"); err != nil {
		return nil, err
	}
	return &enrichStatusResolver{tmdbEnabled: r.cfg.TMDBEnabled()}, nil
}

func (r *Resolver) EnrichmentStatusCodes(ctx context.Context) ([]*enrichmentStatusCodeResolver, error) {
	if err := r.allow(ctx, "Query.enrichmentStatusCodes"); err != nil {
		return nil, err
	}
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

func (r *Resolver) DeletedItems(ctx context.Context, args struct {
	Since *graphql.Time
	Limit *int32
}) ([]*deletedItemResolver, error) {
	if err := r.allow(ctx, "Query.deletedItems"); err != nil {
		return nil, err
	}
	var since *time.Time
	if args.Since != nil {
		since = &args.Since.Time
	}
	ds, err := r.store.ListDeletedItems(ctx, since, deref32(args.Limit))
	if err != nil {
		return nil, err
	}
	out := make([]*deletedItemResolver, 0, len(ds))
	for _, d := range ds {
		out = append(out, &deletedItemResolver{m: d})
	}
	return out, nil
}

func (r *Resolver) ReferenceSync(ctx context.Context) ([]*referenceSyncResolver, error) {
	if err := r.allow(ctx, "Query.referenceSync"); err != nil {
		return nil, err
	}
	rs, err := r.store.ListReferenceSync(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*referenceSyncResolver, 0, len(rs))
	for _, x := range rs {
		out = append(out, &referenceSyncResolver{m: x})
	}
	return out, nil
}

// ===================== Mutations =====================

func (r *Resolver) TriggerScan(ctx context.Context, args struct{ Source *string }) (*scanJobResolver, error) {
	if err := r.allow(ctx, "Mutation.triggerScan"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.enrichOne"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.identify"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.enrichPending"); err != nil {
		return nil, err
	}
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

// RefreshPeople is the operator's backfill of people from TMDB.
func (r *Resolver) RefreshPeople(ctx context.Context, args struct{ All *bool }) (*peopleRefreshResultResolver, error) {
	if err := r.allow(ctx, "Mutation.refreshPeople"); err != nil {
		return nil, err
	}
	if r.svc.People == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.People.RefreshPeople(ctx, derefBool(args.All))
	if err != nil {
		return nil, err
	}
	return &peopleRefreshResultResolver{m: res}, nil
}

func (r *Resolver) BackfillEpisodeBackdrops(ctx context.Context) (*backfillResultResolver, error) {
	if err := r.allow(ctx, "Mutation.backfillEpisodeBackdrops"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.retryNotFound"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.packageItem"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.validateItem"); err != nil {
		return nil, err
	}
	if r.svc.Validator == nil {
		return nil, errNotConfigured
	}
	res, err := r.svc.Validator.ValidateItem(ctx, string(args.ID))
	if err != nil {
		return nil, err
	}
	return &validateResultResolver{m: res}, nil
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
	if err := r.allow(ctx, "Mutation.createItem"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.updateItem"); err != nil {
		return nil, err
	}
	it, err := r.store.UpdateItem(ctx, string(args.ID), args.Input.toWrite())
	if err != nil || it == nil {
		return nil, err
	}
	return newItemResolver(it, r.store), nil
}

func (r *Resolver) DeleteItem(ctx context.Context, args struct {
	ID             graphql.ID
	DeleteFiles    *bool
	DeletePackages *bool
	Reason         *string
}) (*deleteItemResultResolver, error) {
	if err := r.allow(ctx, "Mutation.deleteItem"); err != nil {
		return nil, err
	}
	if r.svc.Remover == nil {
		return nil, errNotConfigured
	}
	// Nothing leaves the disk unless the caller asks for it: a delete that
	// says nothing of the files keeps the source media and the packages.
	res, err := r.svc.Remover.RemoveItem(ctx, string(args.ID), derefBool(args.DeleteFiles),
		derefBool(args.DeletePackages), strDeref(args.Reason))
	if err != nil {
		return nil, err
	}
	return &deleteItemResultResolver{m: res}, nil
}

func (r *Resolver) SetItemGenres(ctx context.Context, args struct {
	ID     graphql.ID
	Genres []string
}) (*itemResolver, error) {
	if err := r.allow(ctx, "Mutation.setItemGenres"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.setItemTags"); err != nil {
		return nil, err
	}
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
	if err := r.allow(ctx, "Mutation.createSetting"); err != nil {
		return nil, err
	}
	if isSecretSetting(args.Key) {
		return nil, errSecretSetting(args.Key)
	}
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
	if err := r.allow(ctx, "Mutation.updateSetting"); err != nil {
		return nil, err
	}
	if cur, err := r.store.GetSetting(ctx, string(args.ID)); err != nil {
		return nil, err
	} else if cur != nil && isSecretSetting(cur.Key) {
		return nil, errSecretSetting(cur.Key)
	}
	s, err := r.store.UpdateSetting(ctx, string(args.ID), args.ValueText, args.ValueType, args.Description)
	if err != nil || s == nil {
		return nil, err
	}
	return &settingResolver{m: s}, nil
}

func (r *Resolver) DeleteSetting(ctx context.Context, args struct{ ID graphql.ID }) (bool, error) {
	if err := r.allow(ctx, "Mutation.deleteSetting"); err != nil {
		return false, err
	}
	return r.store.DeleteSetting(ctx, string(args.ID))
}
