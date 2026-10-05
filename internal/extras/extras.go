// Package extras takes a title's extras in and has them packaged: its
// trailers, teasers, featurettes and other bonus material, each a file of its
// own (db/migrations/039_item_extras.sql).
//
// An extra belongs to a movie or a series, never to an episode, and is no
// item: it is packaged on a chain of its own, keyed by its extraId. The
// service sends the transcoder its trigger (catalog.extra.queued, no itemId),
// the transcoder encodes it with its extras ladder and announces it
// (catalog.extra.transcoded), the packager packages it into
// packages/extras/<aa>/<extraId>/ and reports packaging-complete, which makes
// it ready and announces it (catalog.extra.packaged). The workers report on
// its steps as on an item's (rest/extras.go).
//
// An extra is taken in by an operator (AddExtra: POST /api/extras, GraphQL
// addExtra), or by the scanner's convention (internal/scanner). It waits to
// be sent (pending), and is sent at once when the service has an event bus;
// one that could not be sent, and one whose run failed while it has attempts
// left, is sent again by the sweep (SendDue) a backoff later, by the
// KATALOG_RETRY_* policy. The sweep also takes the extras stuck in their
// packaging for failed runs (Reap) and deletes the packages of removed
// extras a day after their removal (DeleteRemovedPackages). Without an event
// bus extras stay pending.
package extras

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/config"
	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/graph"
	"github.com/zaentrum/katalog-manager/internal/model"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// The timeouts of an extra's packaging: how long it may wait for a worker to
// start (queued for the transcoder, transcoded for the packager), and how
// long a worker may be silent while it runs (transcoding, packaging). The
// workers report a run's start and its end, and an extra's run takes minutes.
const (
	WaitTimeout   = 24 * time.Hour
	SilentTimeout = 2 * time.Hour
)

// RemovedGrace is how long a removed extra's package stays in the package
// store before the sweep deletes it.
const RemovedGrace = 24 * time.Hour

// batch is the most extras a sweep job takes at once.
const batch = 200

// The GraphQL codes of a refusal.
const (
	codeRefused  = "EXTRA_REFUSED"
	codeConflict = "EXTRA_CONFLICT"
	codeNotFound = "NOT_FOUND"
	codeNoTable  = "UNAVAILABLE"
)

// Publisher sends an extra's triggers: events.Producer.
type Publisher interface {
	Enabled() bool
	PublishExtras(ctx context.Context, msgs []events.ExtraMessage) []error
}

// Service takes extras in and has them packaged.
type Service struct {
	st  *store.Store
	cfg config.Config
	pol processing.Policy
	pub Publisher
}

// New is the extras of st, with the files under cfg's roots, retried by pol,
// their triggers sent through pub (nil, or a producer without brokers: none
// is sent, and the extras wait).
func New(st *store.Store, cfg config.Config, pol processing.Policy, pub Publisher) *Service {
	return &Service{st: st, cfg: cfg, pol: pol, pub: pub}
}

var _ graph.Extras = (*Service)(nil)

// bus reports whether triggers can be sent.
func (s *Service) bus() bool { return s.pub != nil && s.pub.Enabled() }

// languageRE is a language as the library names one: BCP 47 ("en",
// "pt-BR"), or the ISO 639-2 code a file carried ("eng", "und", "zxx").
var languageRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// AddExtra takes a file in as an extra of a movie or a series and has it
// packaged:
//   - the title is named one way: by its id, by the path of its file (its
//     primary asset), or by its TMDB id and its type; one there is not is
//     NOT_FOUND (404);
//   - it is a movie or a series: an episode has no extras, its series has;
//   - a season is named only for a series, and only one it has episodes of;
//   - the file is an absolute path of an existing video file under the media
//     root, LIBRARY_ROOT or EXTRAS_ROOT, never under the package store, and
//     no title's own file;
//   - its size and quick hash are stored with it;
//   - the same file again for the same title answers its extra, created
//     false; for another title it is refused, EXTRA_CONFLICT (409).
//
// It waits to be sent, and its trigger goes now when there is an event bus.
// Everything else refused is EXTRA_REFUSED (400).
func (s *Service) AddExtra(ctx context.Context, in graph.AddExtraRequest) (graph.AddExtraResult, error) {
	var res graph.AddExtraResult
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if !model.ValidExtraKind(kind) {
		return res, graph.Refused(http.StatusBadRequest, codeRefused, "an extra's kind is one of %s, not %q",
			strings.Join(model.ExtraKinds, ", "), in.Kind)
	}
	var language *string
	if in.Language != nil {
		if l := strings.TrimSpace(*in.Language); l != "" {
			if len(l) > 35 || !languageRE.MatchString(l) {
				return res, graph.Refused(http.StatusBadRequest, codeRefused,
					"an extra's language is BCP 47 or an ISO 639-2 code (en, pt-BR, eng), not %q", *in.Language)
			}
			language = &l
		}
	}
	if in.SeasonNumber != nil && *in.SeasonNumber < 0 {
		return res, graph.Refused(http.StatusBadRequest, codeRefused, "a season is 0 or more, not %d", *in.SeasonNumber)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = model.ExtraKindTitle(kind)
	}
	path, refused := s.file(in.Path)
	if refused != nil {
		return res, refused
	}
	item, err := s.item(ctx, in)
	if err != nil {
		return res, err
	}
	if in.SeasonNumber != nil {
		if item.typ != "series" {
			return res, graph.Refused(http.StatusBadRequest, codeRefused, "a %s's extra names no season: only a series' does", item.typ)
		}
		var has bool
		if err := s.st.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items e
			WHERE e.type = 'episode' AND e.seasonnumber = $2
			  AND (e.parent_id = $1 OR e.parent_id IN (SELECT id FROM com_nalet_katalog_items WHERE parent_id = $1)))`,
			item.id, *in.SeasonNumber).Scan(&has); err != nil {
			return res, err
		}
		if !has {
			return res, graph.Refused(http.StatusBadRequest, codeRefused, "the series %s has no episode in season %d",
				item.id, *in.SeasonNumber)
		}
	}
	var own *string
	if err := s.st.Pool().QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets
		WHERE path = $1 AND isprimary = true LIMIT 1`, path).Scan(&own); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	if own != nil {
		return res, graph.Refused(http.StatusBadRequest, codeRefused, "%s is the file of item %s, no extra", path, *own)
	}
	size, qh1, err := QH1(path)
	if err != nil {
		return res, graph.Refused(http.StatusBadRequest, codeRefused, "%s cannot be read: %v", path, err)
	}
	x, created, err := s.st.AddExtra(ctx, store.ExtraWrite{ItemID: item.id, Kind: kind, Title: title, Language: language,
		SeasonNumber: in.SeasonNumber, SourcePath: path, SourceSize: size, SourceQH1: qh1, RegisteredBy: model.ExtraByAPI,
		By: auth.Actor(ctx, "katalog-manager")})
	var conflict *store.ExtraConflict
	switch {
	case errors.As(err, &conflict):
		r := graph.Refused(http.StatusConflict, codeConflict, "%s is extra %s of item %s already", path,
			conflict.Extra.ID, conflict.Extra.ItemID)
		r.Extra = conflict.Extra
		return res, r
	case errors.Is(err, store.ErrNoExtras):
		return res, graph.Refused(http.StatusServiceUnavailable, codeNoTable, "no extra is taken in: %v", err)
	case err != nil:
		return res, err
	}
	// The trigger goes now, also when the caller stops waiting; an extra
	// taken in before, still waiting to be sent, goes too.
	if _, _, err := s.Send(context.WithoutCancel(ctx), []string{x.ID}, model.ExtraByAPI); err != nil {
		return res, err
	}
	if x, err = s.st.GetExtra(ctx, x.ID); err != nil {
		return res, err
	}
	return graph.AddExtraResult{Extra: x, Created: created}, nil
}

// title is a title an extra is taken in for.
type title struct{ id, typ string }

// item finds the title a request names, one way, and checks it may have
// extras.
func (s *Service) item(ctx context.Context, in graph.AddExtraRequest) (title, error) {
	var t title
	ways := 0
	for _, named := range []bool{strings.TrimSpace(in.ItemID) != "", strings.TrimSpace(in.ItemPath) != "", in.TmdbID != nil} {
		if named {
			ways++
		}
	}
	if ways != 1 {
		return t, graph.Refused(http.StatusBadRequest, codeRefused,
			"name the title one way: itemId, itemPath (the path of its file), or tmdbId and itemType")
	}
	var ids []title
	var err error
	switch {
	case strings.TrimSpace(in.ItemID) != "":
		id := strings.TrimSpace(in.ItemID)
		ids, err = titles(ctx, s.st, `SELECT id, type FROM com_nalet_katalog_items WHERE id = $1`, id)
		if err == nil && len(ids) == 0 {
			return t, graph.Refused(http.StatusNotFound, codeNotFound, "unknown item: %s", id)
		}
	case strings.TrimSpace(in.ItemPath) != "":
		path := strings.TrimSpace(in.ItemPath)
		ids, err = titles(ctx, s.st, `SELECT DISTINCT i.id, i.type FROM com_nalet_katalog_playbackassets p
			JOIN com_nalet_katalog_items i ON i.id = p.item_id
			WHERE p.path = $1 AND p.isprimary = true ORDER BY i.id`, path)
		if err == nil && len(ids) == 0 {
			return t, graph.Refused(http.StatusNotFound, codeNotFound, "no item has the file %s", path)
		}
	default:
		typ := strings.ToLower(strings.TrimSpace(in.ItemType))
		if typ != "movie" && typ != "series" {
			return t, graph.Refused(http.StatusBadRequest, codeRefused,
				"a TMDB id names a movie or a series: itemType is movie or series, not %q", in.ItemType)
		}
		ids, err = titles(ctx, s.st, `SELECT DISTINCT i.id, i.type FROM com_nalet_katalog_items i
			JOIN com_nalet_katalog_itemexternalids x ON x.item_id = i.id AND x.source = 'tmdb'
			WHERE x.externalid = $1 AND i.type = $2 ORDER BY i.id`, strconv.FormatInt(*in.TmdbID, 10), typ)
		if err == nil && len(ids) == 0 {
			return t, graph.Refused(http.StatusNotFound, codeNotFound, "no %s has the TMDB id %d", typ, *in.TmdbID)
		}
	}
	if err != nil {
		return t, err
	}
	if len(ids) > 1 {
		var names []string
		for _, x := range ids {
			names = append(names, x.id)
		}
		return t, graph.Refused(http.StatusConflict, codeConflict, "the title is not one: items %s", strings.Join(names, ", "))
	}
	t = ids[0]
	switch strings.ToLower(t.typ) {
	case "movie", "series":
		t.typ = strings.ToLower(t.typ)
		return t, nil
	case "episode":
		return t, graph.Refused(http.StatusBadRequest, codeRefused, "item %s is an episode, and an episode has no extras: its series has", t.id)
	}
	return t, graph.Refused(http.StatusBadRequest, codeRefused, "item %s is a %s: only a movie or a series has extras", t.id, t.typ)
}

func titles(ctx context.Context, st *store.Store, sql string, args ...any) ([]title, error) {
	rows, err := st.Pool().Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []title
	for rows.Next() {
		var t title
		if err := rows.Scan(&t.id, &t.typ); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RemoveExtra removes the extra id, as the caller and reason say: it stays,
// removed, and its package is deleted RemovedGrace later. nil when there is
// no such extra.
func (s *Service) RemoveExtra(ctx context.Context, id, reason string) (*model.Extra, error) {
	x, err := s.st.RemoveExtra(ctx, id, auth.Actor(ctx, "katalog-manager"), reason, RemovedGrace)
	if errors.Is(err, store.ErrNoExtras) {
		return nil, nil
	}
	return x, err
}

// PackageExtra packages the extra id again (packageAgain).
func (s *Service) PackageExtra(ctx context.Context, id string) (graph.ExtraPackagingResult, error) {
	x, err := s.st.GetExtra(ctx, id)
	if err != nil {
		return graph.ExtraPackagingResult{}, err
	}
	if x == nil || x.RemovedAt != nil {
		return graph.ExtraPackagingResult{}, graph.Refused(http.StatusNotFound, codeNotFound, "unknown extra: %s", id)
	}
	return s.packageAgain(ctx, []string{id}, false)
}

// PackageExtras packages every extra of the title itemID again
// (packageAgain).
func (s *Service) PackageExtras(ctx context.Context, itemID string) (graph.ExtraPackagingResult, error) {
	var exists bool
	if err := s.st.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM com_nalet_katalog_items WHERE id = $1)`, itemID).
		Scan(&exists); err != nil {
		return graph.ExtraPackagingResult{}, err
	}
	if !exists {
		return graph.ExtraPackagingResult{}, graph.Refused(http.StatusNotFound, codeNotFound, "unknown item: %s", itemID)
	}
	xs, err := s.st.ExtrasByItem(ctx, itemID, false)
	if err != nil {
		return graph.ExtraPackagingResult{}, err
	}
	ids := make([]string, 0, len(xs))
	for _, x := range xs {
		ids = append(ids, x.ID)
	}
	return s.packageAgain(ctx, ids, true)
}

// packageAgain packages the extras of ids again with the pipeline's current
// settings: each waits to be sent afresh, its failures in a row cleared, and
// its trigger goes now when there is an event bus; its package plays until
// the new one is in place. One in its packaging within its timeout is left
// alone (a second run would race the first), and so is one whose file is
// missing, until the file is back.
func (s *Service) packageAgain(ctx context.Context, ids []string, many bool) (graph.ExtraPackagingResult, error) {
	var res graph.ExtraPackagingResult
	if len(ids) == 0 {
		res.Extras = []*model.Extra{}
		res.Message = "the title has no extras"
		return res, nil
	}
	// A caller that stops waiting leaves no extra reset without its trigger.
	ctx = context.WithoutCancel(ctx)
	now := time.Now()
	_, whys, err := s.st.ResetExtras(ctx, ids, auth.Actor(ctx, "katalog-manager"), func(x *model.Extra) string {
		return s.busy(x, now)
	})
	if err != nil {
		return res, err
	}
	var reset []string
	for _, id := range ids {
		if _, left := whys[id]; !left {
			reset = append(reset, id)
		}
	}
	sent, notSent, err := s.Send(ctx, reset, "reencode")
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		x, err := s.st.GetExtra(ctx, id)
		if err != nil {
			return res, err
		}
		if x != nil && x.RemovedAt == nil {
			res.Extras = append(res.Extras, x)
		}
	}
	if res.Extras == nil {
		res.Extras = []*model.Extra{}
	}
	res.Queued, res.Busy, res.NotSent = int32(len(reset)), int32(len(whys)), int32(notSent)
	res.Message = s.packagingMessage(res, len(ids), sent, many, firstWhy(ids, whys))
	return res, nil
}

func firstWhy(ids []string, whys map[string]string) string {
	for _, id := range ids {
		if w, ok := whys[id]; ok {
			return w
		}
	}
	return ""
}

// busy says why an extra is left alone by a re-encode, "" to take it: one in
// its packaging within its timeout (waiting for a worker, or one running), or
// one whose file is missing and still gone.
func (s *Service) busy(x *model.Extra, now time.Time) string {
	since := func(t ...*time.Time) time.Time {
		for _, v := range t {
			if v != nil {
				return *v
			}
		}
		return x.ModifiedAt
	}
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	switch x.State {
	case model.ExtraMissing:
		if x.SourcePath != nil {
			if _, refused := s.file(*x.SourcePath); refused == nil {
				return ""
			}
		}
		return "its file is missing (" + strOf(x.SourcePath) + ")"
	case model.ExtraQueued, model.ExtraTranscoded:
		t := since(x.DispatchedAt, x.HeartbeatAt)
		if now.Sub(t) < WaitTimeout {
			who := "the transcoder"
			if x.State == model.ExtraTranscoded {
				who = "the packager"
			}
			return fmt.Sprintf("it waits for %s since %s, within its timeout of %s", who, at(t), label(WaitTimeout))
		}
	case model.ExtraTranscoding, model.ExtraPackaging:
		t := since(x.HeartbeatAt)
		if now.Sub(t) < SilentTimeout {
			return fmt.Sprintf("it is %s: its worker last reported at %s, within its timeout of %s", x.State, at(t), label(SilentTimeout))
		}
	}
	return ""
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// packagingMessage says what a re-encode did.
func (s *Service) packagingMessage(res graph.ExtraPackagingResult, looked, sent int, many bool, why string) string {
	if !many && res.Queued == 0 {
		return "left alone: " + why
	}
	what := "it"
	if many {
		what = fmt.Sprintf("%d of its %d extras", res.Queued, looked)
		if res.Queued == 0 {
			what = fmt.Sprintf("none of its %d extras", looked)
		}
	}
	parts := []string{"packaging " + what + " again"}
	switch {
	case res.Queued == 0:
	case !s.bus():
		parts[0] += ": there is no event bus, so it waits, pending, until one sends it"
	case sent > 0:
		parts[0] += fmt.Sprintf(" (%s sent); the package it has plays until the new one is in place", events.TopicExtraQueued)
	}
	if many && res.Busy > 0 {
		parts = append(parts, fmt.Sprintf("%d left alone (the first: %s)", res.Busy, why))
	}
	if res.NotSent > 0 {
		parts = append(parts, fmt.Sprintf("%d could not be sent: pending, and sent again a backoff later", res.NotSent))
	}
	return strings.Join(parts, "; ")
}

// label writes d as short as it reads: 24h, 2h, 1h30m, 90s.
func label(d time.Duration) string {
	out := d.Round(time.Second).String()
	if strings.HasSuffix(out, "m0s") {
		out = strings.TrimSuffix(out, "0s")
	}
	if strings.HasSuffix(out, "h0m") {
		out = strings.TrimSuffix(out, "0m")
	}
	return out
}
