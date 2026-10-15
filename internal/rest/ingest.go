package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/events"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/processing"
	"github.com/zaentrum/katalog-manager/internal/store"
)

// ingestRequest registers a file that appeared in the library (e.g. staged by an
// external importer) as a catalog item. NEUTRAL: it is the scanner's create path
// exposed as a machine contract — it knows nothing of how the file got there.
type ingestRequest struct {
	Path        string  `json:"path"`  // absolute source path (must exist under a known root)
	Type        string  `json:"type"`  // movie|episode|track
	Title       string  `json:"title"` // required
	Year        *int32  `json:"year,omitempty"`
	Description *string `json:"description,omitempty"`
	SortTitle   *string `json:"sortTitle,omitempty"`
	SizeBytes   int64   `json:"sizeBytes,omitempty"`
	// MetadataLocked pins the provided metadata against the enricher (default
	// false: let the pipeline enrich/fill artwork from TMDB).
	MetadataLocked *bool `json:"metadataLocked,omitempty"`

	// Episode coordinates. store.ItemWrite has always accepted these and
	// IngestExternalFile has always bound them — only this DTO dropped them, so
	// every externally-ingested episode arrived with a NULL parent.
	//
	// The consequence is not cosmetic: enrichEpisode returns "episode has no
	// series parent" -> not_found -> skipped, and the worker advances on
	// done|not_found alike, so the item becomes a PLAYABLE, metadata-less
	// orphan named after the show, with no error anywhere. Production holds 64
	// such rows, all with a primary asset.
	ParentID      *string `json:"parentId,omitempty"`
	SeasonNumber  *int32  `json:"seasonNumber,omitempty"`
	EpisodeNumber *int32  `json:"episodeNumber,omitempty"`

	// By identity (ADR-0012): an episode's series by its ids in the reference
	// databases rather than by its catalog id (series), the title's own ids
	// (identity: a movie's, or an episode's own), and, for one file of several
	// episodes in a row, the last of them (episodeEnd; the first is
	// episodeNumber). An id is a string, of tmdb, tvdb or imdb.
	Series     map[string]any `json:"series,omitempty"`
	Identity   map[string]any `json:"identity,omitempty"`
	EpisodeEnd *int32         `json:"episodeEnd,omitempty"`
}

// identities are what a body names a title by besides its catalog ids: an
// episode's series' ids (series) and the title's own (own), each source to
// its id.
type identities struct {
	series, own map[string]string
}

// byIdentity reports whether the body names the title by an identity.
func (in identities) byIdentity() bool { return len(in.series) > 0 || len(in.own) > 0 }

// idForms say what an id of each source is, as a record keeps one
// (library.ValidReference).
var idForms = map[string]string{"tmdb": "digits", "tvdb": "digits", "imdb": "tt and digits"}

// identityOf reads the ids the body's field (series, identity) names a title
// by: each of store.IdentitySources at most once, its id a string of the form
// a record keeps it in (an IMDb id's letters in lower case); an empty id, or
// null, is none. It says what is wrong with one it cannot read.
func identityOf(field string, raw map[string]any) (map[string]string, string) {
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]string{}
	for _, k := range keys {
		src := strings.ToLower(strings.TrimSpace(k))
		if !slices.Contains(store.IdentitySources, src) {
			return nil, fmt.Sprintf("%s takes the ids of tmdb, tvdb and imdb, not %q", field, k)
		}
		var id string
		switch v := raw[k].(type) {
		case nil:
			continue
		case string:
			id = strings.TrimSpace(v)
		default:
			return nil, fmt.Sprintf("%s.%s is an id, given as a string", field, src)
		}
		if id == "" {
			continue
		}
		if src == "imdb" {
			id = strings.ToLower(id)
		}
		if _, twice := out[src]; twice {
			return nil, fmt.Sprintf("%s names %s twice", field, src)
		}
		if !library.ValidReference(src, id) {
			return nil, fmt.Sprintf("%s.%s is %s, not %q", field, src, idForms[src], id)
		}
		out[src] = id
	}
	return out, ""
}

// identities reads the identities the body names its title by, saying what
// is wrong with one it cannot read.
func (req *ingestRequest) identities() (identities, string) {
	var in identities
	var why string
	if in.series, why = identityOf("series", req.Series); why != "" {
		return in, why
	}
	in.own, why = identityOf("identity", req.Identity)
	return in, why
}

// guard says why the body cannot be taken in as it stands, "" when it can.
// An episode names its series one way, by its catalog id (parentId) or by
// its identity (series), with its numbers; series and episodeEnd are an
// episode's, identity a movie's or an episode's own.
func (req *ingestRequest) guard(in identities) string {
	if req.Type != "episode" {
		switch {
		case len(in.series) > 0:
			return "series names the series of an episode: its type is episode"
		case req.EpisodeEnd != nil:
			return "episodeEnd is the last episode one file of several covers: its type is episode"
		case len(in.own) > 0 && req.Type != "movie":
			return "identity names a movie or an episode"
		}
		return ""
	}
	parent := req.ParentID != nil && *req.ParentID != ""
	switch {
	case parent && len(in.series) > 0:
		return "an episode names its series one way: by its catalog id (parentId) or by its identity (series)"
	// An episode without coordinates cannot be linked to its series, and the
	// pipeline will happily publish it anyway as an orphan. Reject it at the
	// boundary: a 400 here is the only thing that turns a silent, permanent
	// data defect into a visible failure the caller can fix.
	case !parent && len(in.series) == 0, req.SeasonNumber == nil, req.EpisodeNumber == nil:
		return "an episode requires its series, by parentId or by its identity (series), and seasonNumber and " +
			"episodeNumber; without them it would be ingested as an unlinked orphan"
	case len(in.series) > 0 && (*req.SeasonNumber < 0 || *req.EpisodeNumber < 0):
		return "an episode's seasonNumber and episodeNumber are 0 or more"
	}
	if end, first := req.EpisodeEnd, *req.EpisodeNumber; end != nil && (*end < first || *end-first >= library.MaxCovered) {
		return fmt.Sprintf("episodeEnd is the last episode the file covers, from episodeNumber on: %d to %d, as one file "+
			"covers %d episodes at most", first, first+library.MaxCovered-1, library.MaxCovered)
	}
	return ""
}

// ingest handles POST /api/ingest: create item + primary asset at the path, seed
// the scan step done, and emit catalog.item.discovered so the enrich→analyze→
// transcode→package pipeline runs. Idempotent on the path (re-ingest returns the
// existing item, no re-fire). Returns {itemId, created}.
//
// A title is handed over by its identity too (ADR-0012): an episode by its
// series' ids (series), found among the catalog's series or made as the
// scanner makes one, and a movie with its own ids (identity), which it gets;
// one file of several episodes covers those after its first up to episodeEnd
// (ingestNew). The answer then names the series (seriesId) and the episodes the
// file covers (covered). An identity that contradicts what the catalog holds
// of the path, and a title the library holds already as another item, are
// refused (409), naming the item.
func (h *Handlers) ingest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Path = strings.TrimSpace(req.Path)
	req.Type = strings.TrimSpace(req.Type)
	req.Title = strings.TrimSpace(req.Title)
	if req.Path == "" || req.Type == "" || req.Title == "" {
		writeError(w, http.StatusBadRequest, "path, type and title are required")
		return
	}
	// A disc image is no file the library holds, as the scanner passes over
	// one: refused here, it is no title whose steps would all fail.
	if processing.IsDiscImage(req.Path) {
		writeError(w, http.StatusBadRequest, filepath.Base(req.Path)+" is a "+processing.DiscImageReason)
		return
	}
	in, why := req.identities()
	if why == "" {
		why = req.guard(in)
	}
	if why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}

	// Guard: the path must live under the media OR packages root — an ingest must
	// never point the catalog at an arbitrary host path. With the library's v2
	// layout it lives under the arrivals' root alone, and is told by its size
	// and quick hash among the originals the library knows (ingestKnown).
	set, err := h.settings(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the library's settings could not be read: "+err.Error())
		return
	}
	var fix *fixity
	if set.V2() {
		if !library.Within(h.paths().Arrivals, filepath.Clean(req.Path)) || !filepath.IsAbs(req.Path) {
			writeError(w, http.StatusBadRequest, "path must be under the arrivals' root (ARRIVALS_ROOT)")
			return
		}
		req.Path = filepath.Clean(req.Path)
		if id, done := h.ingestKnown(w, r, req.Path, &fix); done {
			if id != "" {
				h.answerHeld(r.Context(), w, h.d.Store.Pool(), &req, in, id)
			}
			return
		}
	} else if !underRoot(h.d.Cfg.NFSRoot, req.Path) && !underRoot(h.d.Cfg.PackagesRoot, req.Path) {
		writeError(w, http.StatusBadRequest, "path must be under the media or packages root")
		return
	}
	h.ingestNew(r.Context(), w, &req, in, fix)
}

// taken is what an ingest made: the title, the series of an episode handed
// over by its identity (made when seriesMade), and the episodes the title's
// file covers besides it (covered), of which it made those in made.
type taken struct {
	itemID, seriesID string
	seriesMade       bool
	covered, made    []string
}

// refusal is an ingest refused: its status and its answer.
type refusal struct {
	status int
	body   map[string]any
}

// heldAs refuses a title the library holds already as the item itemID (409).
func heldAs(itemID, why string) *refusal {
	return &refusal{status: http.StatusConflict, body: map[string]any{"error": why, "itemId": itemID}}
}

// ingestNew takes the file at the body's path in as a new title, in one
// transaction, and answers. Ingests are taken in one at a time, and one onto
// a file at a time (as one replace onto a file runs at a time): a series
// found or made by its identity, and the episodes a file covers, are made
// once. A path the catalog holds by then is answered as the title held
// (answerHeld). Once committed, the items it made enter the pipeline
// (discovered), whether or not the caller still waits.
func (h *Handlers) ingestNew(ctx context.Context, w http.ResponseWriter, req *ingestRequest, in identities, fix *fixity) {
	fail := func(err error) { writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error()) }
	tx, err := h.d.Store.Pool().Begin(ctx)
	if err != nil {
		fail(err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('com_nalet_katalog_playbackassets:' || $1))`, req.Path); err != nil {
		fail(err)
		return
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('com_nalet_katalog_ingest'))`); err != nil {
		fail(err)
		return
	}
	var held string
	switch err := tx.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`, req.Path).
		Scan(&held); {
	case err == nil:
		h.answerHeld(ctx, w, tx, req, in, held)
		return
	case !errors.Is(err, pgx.ErrNoRows):
		fail(err)
		return
	}
	t, ref, err := h.writeNew(ctx, tx, req, in, fix)
	switch {
	case err != nil:
		fail(err)
		return
	case ref != nil:
		writeJSON(w, ref.status, ref.body)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		fail(err)
		return
	}
	// The series made for it first, then the title, then the episodes made
	// for its file to cover.
	var made []madeItem
	if t.seriesMade {
		made = append(made, madeItem{t.seriesID, "series"})
	}
	made = append(made, madeItem{t.itemID, req.Type})
	for _, id := range t.made {
		made = append(made, madeItem{id, "episode"})
	}
	h.discovered(context.WithoutCancel(ctx), made)
	answer := map[string]any{"itemId": t.itemID, "created": true}
	if len(in.series) > 0 {
		answer["seriesId"] = t.seriesID
	}
	if req.EpisodeEnd != nil {
		answer["covered"] = t.covered
	}
	writeJSON(w, http.StatusOK, answer)
}

// writeNew writes the title of the body in tx: the series of an episode handed
// over by its series' identity, found by it (store.ItemsByIdentity: the
// first of tmdb, tvdb and imdb that finds one decides, the series made first
// of those it finds) or made from it and the body's title and year as the
// scanner makes a series it finds (store.MakeSeries), with a row for each of
// its ids; the title, under its series, with its file (and the file's source
// with the v2 layout) and its own ids (an episode's TMDB id under
// tmdb-episode); and the episodes its file covers up to episodeEnd (coverIn).
// A title the library holds already as another item is refused (heldEpisode,
// heldMovie). An episode handed over by its catalog id is held to nothing it
// was not held to before: a file of several covers the episodes under its
// parent's series.
func (h *Handlers) writeNew(ctx context.Context, tx pgx.Tx, req *ingestRequest, in identities, fix *fixity) (*taken, *refusal, error) {
	t := &taken{}
	iw := store.ItemWrite{
		Type: &req.Type, Title: &req.Title, SortTitle: req.SortTitle,
		Year: req.Year, Description: req.Description, MetadataLocked: req.MetadataLocked,
		ParentID: req.ParentID, SeasonNumber: req.SeasonNumber, EpisodeNumber: req.EpisodeNumber,
	}
	own := in.own
	var series string
	switch {
	case len(in.series) > 0:
		found, err := store.ItemsByIdentity(ctx, tx, "series", in.series)
		if err != nil {
			return nil, nil, err
		}
		if len(found) > 0 {
			series = found[0]
			if ref, err := heldEpisode(ctx, tx, series, *req.SeasonNumber, *req.EpisodeNumber); err != nil || ref != nil {
				return nil, ref, err
			}
		} else {
			if series, err = store.MakeSeries(ctx, tx, req.Title, req.Year, nil); err != nil {
				return nil, nil, fmt.Errorf("its series could not be made: %w", err)
			}
			if err := store.AddExternalIDs(ctx, tx, series, in.series); err != nil {
				return nil, nil, fmt.Errorf("its series' ids could not be kept: %w", err)
			}
			t.seriesMade = true
		}
		t.seriesID, iw.ParentID = series, &series
	case req.Type == "episode" && req.EpisodeEnd != nil:
		var err error
		series, err = library.ParentSeries(ctx, tx, *req.ParentID)
		if errors.Is(err, library.ErrNoItem) {
			return nil, &refusal{status: http.StatusBadRequest, body: map[string]any{"error": "parentId " + *req.ParentID +
				" is no item of the catalog: the episodes a file of several covers are its parent's series'"}}, nil
		}
		if err != nil {
			return nil, nil, err
		}
	case req.Type == "movie" && len(own) > 0:
		if ref, err := heldMovie(ctx, tx, own); err != nil || ref != nil {
			return nil, ref, err
		}
	}
	if req.Type == "episode" {
		own = episodeIDsOf(own)
	}
	itemID, _, err := store.IngestFile(ctx, tx, iw, req.Path, req.SizeBytes)
	if err != nil {
		return nil, nil, err
	}
	t.itemID = itemID
	if fix != nil {
		src, err := h.paths().AddSource(ctx, tx, itemID, req.Path, fix.size, fix.qh1)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET sourceid = $3
				WHERE item_id = $1 AND path = $2`, itemID, req.Path, src.ID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("the source could not be kept: %w", err)
		}
	}
	if err := store.AddExternalIDs(ctx, tx, itemID, own); err != nil {
		return nil, nil, fmt.Errorf("its ids could not be kept: %w", err)
	}
	if req.EpisodeEnd != nil {
		if t.covered, t.made, err = coverIn(ctx, tx, itemID, series, *iw.ParentID, *req.SeasonNumber, *req.EpisodeNumber,
			*req.EpisodeEnd, req.Title, req.Year); err != nil {
			return nil, nil, fmt.Errorf("the episodes its file covers could not be linked: %w", err)
		}
	}
	return t, nil, nil
}

// episodeIDsOf are an episode's own ids, each source to its id, by the sources
// the catalog keeps them under (store.EpisodeSource).
func episodeIDsOf(ids map[string]string) map[string]string {
	out := make(map[string]string, len(ids))
	for src, id := range ids {
		out[store.EpisodeSource(src)] = id
	}
	return out
}

// heldEpisode refuses an episode handed over by its series' identity that the
// library holds already as another item: an episode of the series of that
// season and number with a file of its own, or one another episode's file
// covers. One with neither holds no file and refuses nothing: the ingest
// makes the episode of its file, as the scanner does.
func heldEpisode(ctx context.Context, q store.Querier, series string, season, n int32) (*refusal, error) {
	eps, err := library.EpisodesNumbered(ctx, q, series, season, n, "")
	if err != nil {
		return nil, err
	}
	code := fmt.Sprintf("S%02dE%02d", season, n)
	for _, e := range eps {
		switch {
		case e.OwnFile:
			return heldAs(e.ID, fmt.Sprintf("%s of series %s is in the library already: item %s, with a file of its own",
				code, series, e.ID)), nil
		case e.By != nil:
			return heldAs(e.ID, fmt.Sprintf("%s of series %s is in the library already: item %s, which the file of episode %s covers",
				code, series, e.ID, *e.By)), nil
		}
	}
	return nil, nil
}

// heldMovie refuses a movie handed over by its identity that the library
// holds already as another item: a movie the identity finds
// (store.ItemsByIdentity) with a file (a playback row but a trailer's).
func heldMovie(ctx context.Context, q store.Querier, ids map[string]string) (*refusal, error) {
	found, err := store.ItemsByIdentity(ctx, q, "movie", ids)
	if err != nil || len(found) == 0 {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT item_id FROM com_nalet_katalog_playbackassets
		WHERE item_id = ANY($1) AND COALESCE(kind, 'primary') <> 'trailer'`, found)
	if err != nil {
		return nil, err
	}
	withFile, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, id := range found {
		if slices.Contains(withFile, id) {
			return heldAs(id, fmt.Sprintf("the movie of %s is in the library already: item %s", identityText(ids), id)), nil
		}
	}
	return nil, nil
}

// identityText says an identity: each source and its id, in the order of
// store.IdentitySources.
func identityText(ids map[string]string) string {
	var parts []string
	for _, src := range store.IdentitySources {
		if id := ids[src]; id != "" {
			parts = append(parts, src+" "+id)
		}
	}
	return strings.Join(parts, ", ")
}

// coverIn links in tx the episodes the file of the episode holder covers
// besides it, the numbers after first up to last of season, as the catalog
// links the episodes a file's name numbers (library/covers.go, the scanner's
// linkCovering): the series' episode of that number, under it or under a
// season of it, or, when it has none, one made for it under parent as the
// ingest makes the holder (the body's title and year, no file); one with a
// file of its own is left alone, its own file wins, and so is one the file
// of another episode covers already, each said in the log. It answers the
// episodes linked, in episode order, and those it made. Without migration
// 045 the file covers its first episode alone, said in the log.
func coverIn(ctx context.Context, tx pgx.Tx, holder, series, parent string, season, first, last int32, title string,
	year *int32) (covered, made []string, err error) {
	covered = []string{}
	if last <= first {
		return covered, nil, nil
	}
	ready, err := library.CoversReady(ctx, tx)
	if err != nil || !ready {
		if err == nil {
			log.Printf("ingest: migration 045 (db/migrations/045_multi_episode_files.sql) is not applied: the file of "+
				"episode %s covers episodes %d to %d of its season, and is the first one's alone until it is", holder, first, last)
		}
		return covered, nil, err
	}
	episode := "episode"
	for n := first + 1; n <= last; n++ {
		code := fmt.Sprintf("S%02dE%02d", season, n)
		eps, err := library.EpisodesNumbered(ctx, tx, series, season, n, holder)
		if err != nil {
			return nil, nil, err
		}
		if own := slices.IndexFunc(eps, func(e library.Numbered) bool { return e.OwnFile }); own >= 0 {
			log.Printf("ingest: %s has a file of its own (item %s), which wins: the file of episode %s does not cover it",
				code, eps[own].ID, holder)
			continue
		}
		var id string
		switch {
		case len(eps) > 0 && eps[0].By != nil:
			log.Printf("ingest: %s (item %s) is covered by the file of episode %s already, and the file of episode %s "+
				"does not cover it", code, eps[0].ID, *eps[0].By, holder)
			continue
		case len(eps) > 0:
			id = eps[0].ID
		default:
			number := n
			if id, err = store.InsertIngested(ctx, tx, store.ItemWrite{Type: &episode, Title: &title, Year: year,
				ParentID: &parent, SeasonNumber: &season, EpisodeNumber: &number}); err != nil {
				return nil, nil, err
			}
			made = append(made, id)
		}
		if _, err := library.Link(ctx, tx, holder, id); err != nil {
			return nil, nil, err
		}
		covered = append(covered, id)
	}
	return covered, made, nil
}

// madeItem is an item an ingest made, and its type.
type madeItem struct{ id, typ string }

// discovered seeds the scan step of each item an ingest made done and emits
// its discovered event, exactly as the scanner does on a fresh insert, so
// that the pipeline runs: best-effort on the step (never abort the ingest),
// the event naming the item's identities.
func (h *Handlers) discovered(ctx context.Context, made []madeItem) {
	ids := make([]string, len(made))
	for i, m := range made {
		ids[i] = m.id
		_ = h.d.Steps.Upsert(ctx, m.id, "scan", processing.StatusDone, nil, nil)
	}
	if !h.d.Events.Enabled() {
		return
	}
	known, err := store.IdentitiesOf(ctx, h.d.Store.Pool(), ids)
	if err != nil {
		log.Printf("ingest: the identities of %s could not be read, and their events name none: %v", strings.Join(ids, ", "), err)
	}
	for _, m := range made {
		ev := events.NewItemEvent(m.id)
		ev.Type = m.typ
		ev.Step = "tmdb"
		ev.Source = "ingest"
		ev.Identity = known[m.id].Event()
		h.d.Events.EmitItem(ctx, events.TopicDiscovered, ev)
	}
}

// answerHeld answers an ingest of a path the catalog holds already, the item
// held, as it reads in q: itemId, created false, and nothing happens (a
// re-ingest). A body that names its title by an identity is refused (409)
// when that contradicts what the catalog holds of the path (contradiction):
// the title is never relinked to it. The answer names the series of an
// episode handed over by its series' identity (seriesId), and the episodes
// its file covers besides it (covered) when the body says where its file
// ends.
func (h *Handlers) answerHeld(ctx context.Context, w http.ResponseWriter, q store.Querier, req *ingestRequest, in identities, held string) {
	answer := map[string]any{"itemId": held, "created": false}
	if !in.byIdentity() && req.EpisodeEnd == nil {
		writeJSON(w, http.StatusOK, answer)
		return
	}
	fail := func(err error) { writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error()) }
	known, err := store.IdentitiesOf(ctx, q, []string{held})
	if err != nil {
		fail(err)
		return
	}
	cur := known[held]
	if why, err := contradiction(ctx, q, req, in, held, cur); err != nil {
		fail(err)
		return
	} else if why != "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": why, "itemId": held})
		return
	}
	if len(in.series) > 0 {
		answer["seriesId"] = cur.SeriesID
	}
	if req.EpisodeEnd != nil {
		covered, err := library.CoveredOf(ctx, q, held)
		if err != nil {
			fail(err)
			return
		}
		ids := []string{}
		for _, c := range covered {
			ids = append(ids, c.ID)
		}
		answer["covered"] = ids
	}
	writeJSON(w, http.StatusOK, answer)
}

// contradiction says how what the catalog holds of the title at the body's
// path, held (whose identity is cur), contradicts the identity the body names
// it by; "" when it does not. The title is of the body's type; an episode
// handed over by its series' identity is of the series the identity names
// (identifies), with the body's numbers; a movie is the one its identity
// names; and of an episode's own ids the catalog holds none of a source the
// body names another of.
func contradiction(ctx context.Context, q store.Querier, req *ingestRequest, in identities, held string, cur store.Identity) (string, error) {
	path := req.Path
	if !in.byIdentity() {
		return "", nil
	}
	switch {
	case cur.Type == "":
		return fmt.Sprintf("%s is held by item %s, which the catalog holds no more", path, held), nil
	case cur.Type != req.Type:
		return fmt.Sprintf("%s is item %s, %s, and not %s", path, held, aType(cur.Type), aType(req.Type)), nil
	}
	if len(in.series) > 0 {
		if cur.SeriesID == "" {
			return fmt.Sprintf("%s is item %s, an episode of no series", path, held), nil
		}
		if ok, why, err := identifies(ctx, q, "series", cur.SeriesID, cur.SeriesExternalIDs, in.series); err != nil || !ok {
			return fmt.Sprintf("%s is item %s, an episode of series %s, which %s", path, held, cur.SeriesID, why), err
		}
		if !sameNumber(cur.SeasonNumber, req.SeasonNumber) || !sameNumber(cur.EpisodeNumber, req.EpisodeNumber) {
			return fmt.Sprintf("%s is item %s, %s of its series, and not %s", path, held,
				episodeCode(cur.SeasonNumber, cur.EpisodeNumber), episodeCode(req.SeasonNumber, req.EpisodeNumber)), nil
		}
	}
	if len(in.own) == 0 {
		return "", nil
	}
	if req.Type == "movie" {
		if ok, why, err := identifies(ctx, q, "movie", held, cur.ExternalIDs, in.own); err != nil || !ok {
			return fmt.Sprintf("%s is item %s, which %s", path, held, why), err
		}
		return "", nil
	}
	given := episodeIDsOf(in.own)
	srcs := make([]string, 0, len(given))
	for src := range given {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		if have := cur.ExternalIDs[src]; have != "" && have != given[src] {
			return fmt.Sprintf("%s is item %s, which is %s %s, not %s", path, held, src, have, given[src]), nil
		}
	}
	return "", nil
}

// identifies says whether the identity ids names the item id of the type
// typ, which holds the ids have: the items of its type the identity finds
// (store.ItemsByIdentity, the first source that finds any deciding) are it
// among others, or the identity finds none and the item holds no other id of
// a source it names. It says what the item is otherwise.
func identifies(ctx context.Context, q store.Querier, typ, id string, have, ids map[string]string) (bool, string, error) {
	found, err := store.ItemsByIdentity(ctx, q, typ, ids)
	if err != nil {
		return false, "", err
	}
	if len(found) > 0 {
		if slices.Contains(found, id) {
			return true, "", nil
		}
		return false, fmt.Sprintf("the identity does not name: it names %s %s", typ, found[0]), nil
	}
	for _, src := range store.IdentitySources {
		if given := ids[src]; given != "" && have[src] != "" && have[src] != given {
			return false, fmt.Sprintf("is %s %s, not %s", src, have[src], given), nil
		}
	}
	return true, "", nil
}

// aType is an item of the type typ, said with its article: an episode, a
// movie.
func aType(typ string) string {
	if typ != "" && strings.ContainsRune("aeiou", rune(typ[0])) {
		return "an " + typ
	}
	return "a " + typ
}

// sameNumber reports whether two numbers are the same, none being the same
// as none.
func sameNumber(a, b *int32) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// episodeCode is an episode's numbers as SxxEyy, "S?E?" where it has none.
func episodeCode(season, episode *int32) string {
	if season == nil || episode == nil {
		return "S?E?"
	}
	return fmt.Sprintf("S%02dE%02d", *season, *episode)
}

// underRoot reports whether path lies inside root (both cleaned). Local copy so
// this file stands alone; mirrors itemactions.underRoot.
func underRoot(root, path string) bool {
	root = strings.TrimRight(strings.TrimSpace(root), "/")
	path = strings.TrimSpace(path)
	if root == "" || root == "/" || path == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// fixity is what tells a file: its size and its quick hash.
type fixity struct {
	size int64
	qh1  string
}

// ingestKnown tells the file at path among the originals the library knows,
// as the scanner does: a file the catalog has at that path already is that
// title (re-ingested: nothing happens); one whose original moved here takes
// its title along; a copy of one still in its place, and one deleted after
// packaging, are refused (409), saying whose; one that cannot be read is
// refused (400). It answers the title the file is when done says the answer
// is given (the id empty when a refusal was written), and fix the file's size
// and quick hash otherwise.
func (h *Handlers) ingestKnown(w http.ResponseWriter, r *http.Request, path string, fix **fixity) (string, bool) {
	ctx := r.Context()
	pool := h.d.Store.Pool()
	var existing string
	if err := pool.QueryRow(ctx, `SELECT item_id FROM com_nalet_katalog_playbackassets WHERE path = $1 LIMIT 1`, path).
		Scan(&existing); err == nil {
		return existing, true
	}
	size, qh1, err := library.QH1(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "there is no file to take in at "+path+": "+err.Error())
		return "", true
	}
	*fix = &fixity{size: size, qh1: qh1}
	known, err := library.SourcesByFixity(ctx, pool, size, qh1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
		return "", true
	}
	for _, k := range known {
		switch k.State {
		case library.SourcePresent:
			if k.ArrivalPath != nil {
				if fi, err := os.Stat(*k.ArrivalPath); err == nil && fi.Mode().IsRegular() {
					writeJSON(w, http.StatusConflict, map[string]any{"error": path + " is a copy of the original of item " +
						k.ItemID + " at " + *k.ArrivalPath, "itemId": k.ItemID})
					return "", true
				}
			}
			old := ""
			if k.ArrivalPath != nil {
				old = *k.ArrivalPath
			}
			if _, err := pool.Exec(ctx, `UPDATE com_nalet_katalog_playbackassets SET path = $3, sourceid = $2
				WHERE item_id = $1 AND COALESCE(kind, 'primary') = 'primary' AND (sourceid = $2 OR (sourceid IS NULL AND path = $4))`,
				k.ItemID, k.ID, path, old); err != nil {
				writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
				return "", true
			}
			if err := h.paths().MoveSource(ctx, pool, k, path); err != nil {
				writeError(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
				return "", true
			}
			return k.ItemID, true
		case library.SourceRetiring, library.SourceDeleted:
			writeJSON(w, http.StatusConflict, map[string]any{"error": path + " is already in the library as item " + k.ItemID +
				" (retired); use replaceSource to make it a new version", "itemId": k.ItemID})
			return "", true
		}
	}
	return "", false
}
