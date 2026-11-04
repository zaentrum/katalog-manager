package graph

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/graph-gophers/graphql-go"
	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// What the library holds of a title, for the console (platform-library/1):
// whether its record is written and where its folder is, whether its
// originals are held, its versions, its originals, and the events of its
// folder. holdOriginal holds a title's originals, which the retire job then
// leaves whatever library.originals says, or lets them go.

// libraryReady holds the catalogs migration 040 was seen in place in (it is
// not undone under a running service).
var libraryReady sync.Map

func ready(ctx context.Context, s *store.Store) (bool, error) {
	if _, ok := libraryReady.Load(s); ok {
		return true, nil
	}
	ok, err := s.LibraryReady(ctx)
	if err == nil && ok {
		libraryReady.Store(s, true)
	}
	return ok, err
}

// Library is what the library holds of the title; null on a catalog older
// than migration 040.
func (r *itemResolver) Library(ctx context.Context) (*itemLibraryResolver, error) {
	return libraryOf(ctx, r.s, r.lib, r.m.ID)
}

// libraryOf reads what the library holds of the item id.
func libraryOf(ctx context.Context, s *store.Store, lib library.Paths, id string) (*itemLibraryResolver, error) {
	if ok, err := ready(ctx, s); err != nil || !ok {
		return nil, err
	}
	out := &itemLibraryResolver{}
	err := s.Pool().QueryRow(ctx, `SELECT recordedat, retirehold FROM com_nalet_katalog_items WHERE id = $1`, id).
		Scan(&out.recorded, &out.hold)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if out.versions, err = library.VersionsOf(ctx, s.Pool(), id); err != nil {
		return nil, err
	}
	if out.sources, err = library.SourcesOf(ctx, s.Pool(), id); err != nil {
		return nil, err
	}
	if out.superseded, err = library.SupersededOf(ctx, s.Pool(), id); err != nil {
		return nil, err
	}
	pl, err := library.PlaceOf(ctx, s.Pool(), id)
	var unplaced *library.Unplaced
	switch {
	case errors.As(err, &unplaced) || errors.Is(err, library.ErrNoItem):
		return out, nil
	case err != nil:
		return nil, err
	}
	dir := lib.ItemDir(pl)
	out.dir = &dir
	if _, err := os.Stat(dir); err != nil {
		return out, nil // not on storage: no events
	}
	if out.events, err = library.ReadEvents(dir); err != nil {
		return nil, fmt.Errorf("the events of %s: %w", dir, err)
	}
	return out, nil
}

type itemLibraryResolver struct {
	recorded   *time.Time
	hold       bool
	dir        *string
	versions   []*library.Version
	sources    []*library.Source
	superseded map[string]*library.Superseded
	events     []library.Doc
}

func (r *itemLibraryResolver) Recorded() *graphql.Time { return utcTime(r.recorded) }
func (r *itemLibraryResolver) Hold() bool              { return r.hold }
func (r *itemLibraryResolver) Dir() *string            { return r.dir }

func (r *itemLibraryResolver) Versions() []*libraryVersionResolver {
	out := make([]*libraryVersionResolver, 0, len(r.versions))
	for _, v := range r.versions {
		out = append(out, &libraryVersionResolver{v})
	}
	return out
}

func (r *itemLibraryResolver) Sources() []*librarySourceResolver {
	out := make([]*librarySourceResolver, 0, len(r.sources))
	for _, s := range r.sources {
		out = append(out, &librarySourceResolver{m: s, x: r.superseded[s.ID]})
	}
	return out
}

func (r *itemLibraryResolver) Events() []*libraryEventResolver {
	out := make([]*libraryEventResolver, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, &libraryEventResolver{e})
	}
	return out
}

type libraryVersionResolver struct{ m *library.Version }

func (r *libraryVersionResolver) ID() graphql.ID              { return gid(r.m.ID) }
func (r *libraryVersionResolver) State() string               { return r.m.State }
func (r *libraryVersionResolver) PackageID() *graphql.ID      { return gidp(r.m.PackageID) }
func (r *libraryVersionResolver) Dir() *string                { return r.m.Dir }
func (r *libraryVersionResolver) CompletedAt() *graphql.Time  { return utcTime(r.m.CompletedAt) }
func (r *libraryVersionResolver) VerifiedAt() *graphql.Time   { return utcTime(r.m.VerifiedAt) }
func (r *libraryVersionResolver) VerifiedLevel() *string      { return r.m.VerifiedLevel }
func (r *libraryVersionResolver) SupersededBy() *graphql.ID   { return gidp(r.m.SupersededBy) }
func (r *libraryVersionResolver) SupersededAt() *graphql.Time { return utcTime(r.m.SupersededAt) }
func (r *libraryVersionResolver) RemovedAt() *graphql.Time    { return utcTime(r.m.RemovedAt) }
func (r *libraryVersionResolver) CreatedAt() graphql.Time {
	return graphql.Time{Time: r.m.CreatedAt.UTC()}
}
func (r *libraryVersionResolver) SourceIds() []graphql.ID {
	out := make([]graphql.ID, 0, len(r.m.SourceIDs))
	for _, id := range r.m.SourceIDs {
		out = append(out, gid(id))
	}
	return out
}

// librarySourceResolver is a source, with what the catalog holds of it as a
// superseded original (x; nil for one none took the place of).
type librarySourceResolver struct {
	m *library.Source
	x *library.Superseded
}

func (r *librarySourceResolver) SupersededBy() *graphql.ID {
	if r.x == nil || r.x.By == "" {
		return nil
	}
	return gidp(&r.x.By)
}
func (r *librarySourceResolver) SupersededAt() *graphql.Time {
	if r.x == nil {
		return nil
	}
	return utcTime(r.x.At)
}
func (r *librarySourceResolver) Hold() *string {
	if r.x == nil {
		return nil
	}
	return r.x.Hold
}
func (r *librarySourceResolver) HoldFor() *graphql.ID {
	if r.x == nil {
		return nil
	}
	return gidp(r.x.HoldFor)
}
func (r *librarySourceResolver) HoldLosses() *[]string {
	if r.x == nil || r.x.Losses == nil {
		return nil
	}
	return &r.x.Losses
}
func (r *librarySourceResolver) Decision() *string {
	if r.x == nil {
		return nil
	}
	return r.x.Decision
}
func (r *librarySourceResolver) AcceptedLosses() *[]string {
	if r.x == nil || r.x.Accepted == nil {
		return nil
	}
	return &r.x.Accepted
}
func (r *librarySourceResolver) DecidedBy() *string {
	if r.x == nil {
		return nil
	}
	return r.x.DecidedBy
}
func (r *librarySourceResolver) DecidedAt() *graphql.Time {
	if r.x == nil {
		return nil
	}
	return utcTime(r.x.DecidedAt)
}

// supersededOriginalResolver is a superseded original as an admin lists it.
type supersededOriginalResolver struct{ m *library.SupersededOriginal }

func (r *supersededOriginalResolver) ItemID() graphql.ID { return gid(r.m.ItemID) }
func (r *supersededOriginalResolver) Title() string      { return r.m.Title }
func (r *supersededOriginalResolver) Source() *librarySourceResolver {
	return &librarySourceResolver{m: &library.Source{ID: r.m.SourceID, ItemID: r.m.ItemID, Filename: r.m.Filename,
		ArrivalPath: r.m.Path, State: r.m.State, SizeBytes: r.m.SizeBytes}, x: &r.m.Superseded}
}

// SupersededOriginals lists the superseded originals, those waiting for an
// admin first (held: those alone), at most first (50, up to 500).
func (r *Resolver) SupersededOriginals(ctx context.Context, args struct {
	Held  *bool
	First *int32
}) ([]*supersededOriginalResolver, error) {
	if err := r.allow(ctx, "Query.supersededOriginals"); err != nil {
		return nil, err
	}
	if ok, err := ready(ctx, r.store); err != nil || !ok {
		return []*supersededOriginalResolver{}, err
	}
	n := 50
	if args.First != nil && *args.First > 0 {
		n = min(int(*args.First), 500)
	}
	list, err := library.SupersededOriginals(ctx, r.store.Pool(), derefBool(args.Held), n)
	if err != nil {
		return nil, err
	}
	out := make([]*supersededOriginalResolver, 0, len(list))
	for _, x := range list {
		out = append(out, &supersededOriginalResolver{x})
	}
	return out, nil
}

// DecideSupersededOriginal notes an admin's decision of a superseded
// original (library.DecideSuperseded): accept has the retire job retire it,
// keep keeps it. It is logged with who decided it.
func (r *Resolver) DecideSupersededOriginal(ctx context.Context, args struct {
	SourceID graphql.ID
	Decision string
}) (*supersededOriginalResolver, error) {
	if err := r.allow(ctx, "Mutation.decideSupersededOriginal"); err != nil {
		return nil, err
	}
	if ok, err := ready(ctx, r.store); err != nil {
		return nil, err
	} else if !ok {
		return nil, Refused(http.StatusServiceUnavailable, "UNAVAILABLE",
			"migration 040 (db/migrations/040_library_v2.sql) is not applied: the catalog holds no library")
	}
	decision := strings.ToLower(args.Decision)
	by := auth.Actor(ctx, "katalog-manager")
	x, err := library.DecideSuperseded(ctx, r.store.Pool(), string(args.SourceID), decision, by)
	var none *library.ErrNotSuperseded
	switch {
	case errors.As(err, &none):
		return nil, Refused(http.StatusNotFound, "NOT_FOUND", "%s", none.Error())
	case err != nil:
		return nil, err
	}
	p, _ := auth.PrincipalFrom(ctx)
	log.Printf("decideSupersededOriginal: %s decided %s of the superseded original %s of item %s, which gives up %s",
		r.access.Describe(p), decision, x.SourceID, x.ItemID, orNothing(x.Losses))
	list, err := library.SupersededOriginals(ctx, r.store.Pool(), false, 500)
	if err != nil {
		return nil, err
	}
	for _, o := range list {
		if o.SourceID == x.SourceID {
			return &supersededOriginalResolver{o}, nil
		}
	}
	return &supersededOriginalResolver{&library.SupersededOriginal{Superseded: *x}}, nil
}

// orNothing writes names for a log: "nothing", or the names; "what no record
// tells" for none known (nil).
func orNothing(names []string) string {
	switch {
	case names == nil:
		return "what no record tells"
	case len(names) == 0:
		return "nothing"
	}
	return strings.Join(names, ", ")
}

func (r *librarySourceResolver) ID() graphql.ID             { return gid(r.m.ID) }
func (r *librarySourceResolver) Filename() string           { return r.m.Filename }
func (r *librarySourceResolver) State() string              { return r.m.State }
func (r *librarySourceResolver) ArrivalPath() *string       { return r.m.ArrivalPath }
func (r *librarySourceResolver) LibraryPath() *string       { return r.m.LibraryPath }
func (r *librarySourceResolver) SizeBytes() float64         { return float64(r.m.SizeBytes) }
func (r *librarySourceResolver) Qh1() *string               { return r.m.QH1 }
func (r *librarySourceResolver) RecordedAt() *graphql.Time  { return utcTime(r.m.RecordedAt) }
func (r *librarySourceResolver) RetireEventID() *graphql.ID { return gidp(r.m.RetireEventID) }
func (r *librarySourceResolver) DeletedAt() *graphql.Time   { return utcTime(r.m.DeletedAt) }
func (r *librarySourceResolver) DeletedBy() *string         { return r.m.DeletedBy }
func (r *librarySourceResolver) Error() *string             { return r.m.Error }

// Lost is what the deletion of the original gave up (the deletion gate it
// accepted), none while it is kept.
func (r *librarySourceResolver) Lost() []string {
	out := []string{}
	if len(r.m.Lost) > 0 {
		if v, err := library.Decode(r.m.Lost); err == nil {
			if list, ok := v.([]any); ok {
				for _, e := range list {
					if s, ok := e.(string); ok {
						out = append(out, s)
					}
				}
			}
		}
	}
	return out
}

type libraryEventResolver struct{ d library.Doc }

func (r *libraryEventResolver) str(key string) *string {
	v, _ := r.d.Get(key)
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

func (r *libraryEventResolver) id(key string) *graphql.ID { return gidp(r.str(key)) }

func (r *libraryEventResolver) ID() graphql.ID         { return gid(deref(r.str("eventId"))) }
func (r *libraryEventResolver) Kind() string           { return deref(r.str("kind")) }
func (r *libraryEventResolver) By() *string            { return r.str("by") }
func (r *libraryEventResolver) VersionID() *graphql.ID { return r.id("versionId") }
func (r *libraryEventResolver) SourceID() *graphql.ID  { return r.id("sourceId") }
func (r *libraryEventResolver) PackageID() *graphql.ID { return r.id("packageId") }
func (r *libraryEventResolver) ExtraID() *graphql.ID   { return r.id("extraId") }
func (r *libraryEventResolver) Reason() *string        { return r.str("reason") }

// SupersededBy is the version a package-superseded event names as the one
// that took over.
func (r *libraryEventResolver) SupersededBy() *graphql.ID {
	v, _ := r.d.Get("supersededBy")
	by, ok := v.(library.Doc)
	if !ok {
		return nil
	}
	x, _ := by.Get("versionId")
	if s, ok := x.(string); ok {
		return gidp(&s)
	}
	return nil
}

// At is the event's moment.
func (r *libraryEventResolver) At() *graphql.Time {
	t, err := time.Parse(time.RFC3339, deref(r.str("at")))
	if err != nil {
		return nil
	}
	return &graphql.Time{Time: t}
}

// Accepted is what the original's deletion accepted; null for an event of
// another kind.
func (r *libraryEventResolver) Accepted() *[]string {
	v, ok := r.d.Get("accepted")
	if !ok {
		return nil
	}
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return &out
}

// utcTime is t in UTC, as the catalog's other times are written.
func utcTime(t *time.Time) *graphql.Time {
	if t == nil {
		return nil
	}
	return &graphql.Time{Time: t.UTC()}
}

func gidp(s *string) *graphql.ID {
	if s == nil {
		return nil
	}
	id := gid(*s)
	return &id
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// HoldOriginal holds the title's originals (hold: the retire job leaves
// them, whatever library.originals says) or lets them go; a series' hold is
// its episodes' too. A retirement claimed and not recorded yet is called off.
// It answers what the library holds of the title.
func (r *Resolver) HoldOriginal(ctx context.Context, args struct {
	ID   graphql.ID
	Hold bool
}) (*itemLibraryResolver, error) {
	if err := r.allow(ctx, "Mutation.holdOriginal"); err != nil {
		return nil, err
	}
	if ok, err := ready(ctx, r.store); err != nil {
		return nil, err
	} else if !ok {
		return nil, Refused(http.StatusServiceUnavailable, "UNAVAILABLE",
			"migration 040 (db/migrations/040_library_v2.sql) is not applied: the catalog holds no library")
	}
	id := string(args.ID)
	tag, err := r.store.Pool().Exec(ctx, `UPDATE com_nalet_katalog_items SET retirehold = $2
		WHERE id = $1 OR (type = 'episode' AND (parent_id = $1
			OR parent_id IN (SELECT s.id FROM com_nalet_katalog_items s WHERE s.parent_id = $1 AND s.type = 'season'))
			AND EXISTS (SELECT 1 FROM com_nalet_katalog_items t WHERE t.id = $1 AND t.type = 'series'))`, id, args.Hold)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, Refused(http.StatusNotFound, "NOT_FOUND", "unknown item: %s", id)
	}
	return libraryOf(ctx, r.store, r.lib, id)
}
