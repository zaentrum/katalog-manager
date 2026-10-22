package rest

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/languages"
	"github.com/zaentrum/katalog-manager/internal/library"
	"github.com/zaentrum/katalog-manager/internal/webvtt"
)

// The attach seam: a subtitle attached to a title once its version is in the
// library — its package completed, or its original taken in without one —
// from whoever makes or finds one (an addon, the platform's subtitle step of
// ADR-0014, an admin). Its file goes into the subs/ folder of the version it
// follows (library.AttachableVersion), beside the package or where the
// package of a version taken in is added later, and its row makes every
// player list it and play it, through the catalog's subtitles as any other:
// nothing is packaged again.

// maxAttachBody is the most a POST of the attach seam may send: a subtitle's
// text, as JSON, of a long film with every line twice.
const maxAttachBody = 8 << 20

// attachedItem is the item a call of the attach seam acts on: the one the
// path names, or, for an episode another's file covers, the episode whose
// file it is (its subtitles are that file's). 404 for an id the catalog
// holds no movie or episode of, answered here.
func (h *Handlers) attachedItem(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	id := chi.URLParam(r, "itemId")
	ctx := reqCtx(r)
	pool := h.d.Store.Pool()
	holder, err := library.HolderOrSelf(ctx, pool, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the title could not be read: "+err.Error())
		return "", "", false
	}
	var typ *string
	err = pool.QueryRow(ctx, `SELECT type FROM com_nalet_katalog_items WHERE id = $1`, holder).Scan(&typ)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "no such title")
		return "", "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the title could not be read: "+err.Error())
		return "", "", false
	}
	return holder, deref(typ), true
}

// getItemSubtitles answers GET /api/library/items/{itemId}/subtitles: the
// title's subtitles, its own and the ones attached to it, the version a
// subtitle is attached to now (attachable: complete, or taken in; else why
// not), and with
// ?fingerprint=true what subtitle sources match its original by: its size,
// its Sum64 and what its name claimed (the source record's labels), or why
// there is none.
func (h *Handlers) getItemSubtitles(w http.ResponseWriter, r *http.Request) {
	id, typ, ok := h.attachedItem(w, r)
	if !ok {
		return
	}
	ctx := reqCtx(r)
	pool := h.d.Store.Pool()
	subs, err := library.SubtitlesOf(ctx, pool, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the title's subtitles could not be read: "+err.Error())
		return
	}
	answer := map[string]any{"itemId": id, "type": typ, "versionId": nil, "attachable": false, "subtitles": subs}
	v, err := library.AttachableVersion(ctx, pool, id)
	var no *library.NotAttachable
	switch {
	case errors.As(err, &no):
		answer["reason"] = no.Reason
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the title's version could not be read: "+err.Error())
		return
	default:
		answer["versionId"], answer["attachable"] = v.ID, true
	}
	if r.URL.Query().Get("fingerprint") == "true" {
		original, note := h.originalFingerprint(r, id, v)
		answer["original"] = original
		if note != "" {
			answer["originalNote"] = note
		}
	}
	writeJSON(w, http.StatusOK, answer)
}

// originalFingerprint is what subtitle sources match the original of the
// item id by — {sizeBytes, sum64, labels} — or nil and why there is none.
// It reads 128 KiB of the original and its source record, nothing else.
func (h *Handlers) originalFingerprint(r *http.Request, id string, v *library.Version) (map[string]any, string) {
	ctx := reqCtx(r)
	pool := h.d.Store.Pool()
	o, err := library.OriginalOf(ctx, pool, id)
	if err != nil {
		return nil, "what the title has of its original could not be read: " + err.Error()
	}
	if o.Gone() {
		return nil, o.Why()
	}
	var path string
	err = pool.QueryRow(ctx, `SELECT path FROM com_nalet_katalog_playbackassets WHERE item_id = $1 AND isprimary = true
		ORDER BY path LIMIT 1`, id).Scan(&path)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "the title has no original"
	}
	if err != nil {
		return nil, "the title's original could not be found: " + err.Error()
	}
	size, sum, err := library.Sum64(path)
	if err != nil {
		return nil, "the original cannot be read: " + err.Error()
	}
	out := map[string]any{"sizeBytes": size, "sum64": sum}
	if labels := h.sourceLabels(r, id, v); labels != nil {
		out["labels"] = labels
	}
	return out, ""
}

// sourceLabels are what the name of the version's source claimed, as its
// source record keeps it (quality, medium, resolution, edition); nil when
// there is no record to read.
func (h *Handlers) sourceLabels(r *http.Request, id string, v *library.Version) map[string]any {
	if v == nil || len(v.SourceIDs) == 0 {
		return nil
	}
	pl, err := library.PlaceOf(reqCtx(r), h.d.Store.Pool(), id)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(library.SourceDir(h.paths().ItemDir(pl), v.SourceIDs[0]), "source.json"))
	if err != nil {
		return nil
	}
	var rec struct {
		Labels map[string]any `json:"labels"`
	}
	if json.Unmarshal(b, &rec) != nil || len(rec.Labels) == 0 {
		return nil
	}
	return rec.Labels
}

// attachRequest is the body of POST /api/library/items/{itemId}/subtitles.
type attachRequest struct {
	Language *string `json:"language"`
	Label    *string `json:"label"`
	Forced   bool    `json:"forced"`
	SDH      bool    `json:"sdh"`
	Origin   *string `json:"origin"`
	Format   string  `json:"format"`
	Content  *string `json:"content"`
}

// attachSubtitle answers POST /api/library/items/{itemId}/subtitles: it
// attaches the subtitle of the body to the title (library.Attach). The
// content is WebVTT or SubRip text, UTF-8 (format says which; without it the
// text does), kept as WebVTT. 201 with the subtitle, 200 with the one there
// is for the same file of the same language and flags; 400 for a body that
// does not say what the subtitle is, 409 for a title with no version to
// attach to (neither packaged nor taken in), 413 for one too large, 422 for
// content that is no subtitle.
func (h *Handlers) attachSubtitle(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.attachedItem(w, r)
	if !ok {
		return
	}
	var req attachRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAttachBody)).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "the subtitle is larger than the 8 MiB a body may be")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Language == nil || req.Origin == nil || req.Content == nil {
		writeError(w, http.StatusBadRequest, "language, origin and content are required")
		return
	}
	lang, ok := languages.Tag(*req.Language)
	if !ok {
		writeError(w, http.StatusBadRequest, "language must be a BCP 47 tag of a language, as de, en or zh-Hans")
		return
	}
	if !slices.Contains(library.Origins, *req.Origin) {
		writeError(w, http.StatusBadRequest, "origin must be one of "+strings.Join(library.Origins, ", "))
		return
	}
	format, ok := webvtt.FormatOf(req.Format)
	if !ok {
		writeError(w, http.StatusBadRequest, "format must be webvtt or srt")
		return
	}
	vtt, _, err := webvtt.Normalize([]byte(*req.Content), format)
	var bad *webvtt.Invalid
	if errors.As(err, &bad) {
		writeError(w, http.StatusUnprocessableEntity, bad.Reason)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p, _ := auth.PrincipalFrom(reqCtx(r))
	sub, created, err := library.Attach(reqCtx(r), h.d.Store.Pool(), id, library.Attachment{VTT: vtt, Language: lang,
		Label: req.Label, Forced: req.Forced, SDH: req.SDH, Origin: *req.Origin, By: attacher(p)}, time.Now())
	var no *library.NotAttachable
	if errors.As(err, &no) {
		writeError(w, http.StatusConflict, no.Reason)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the subtitle could not be attached: "+err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"itemId": id, "created": created, "subtitle": sub})
}

// detachSubtitle answers DELETE /api/library/items/{itemId}/subtitles/{id}:
// it removes a subtitle attached to the title (library.Detach), one the
// caller attached, or, for an admin, any. 204, also when the title has no
// subtitle of that id (removed already, or never there: either way it is
// not attached); 409 for one of the title's own, 403 for one another client
// attached.
func (h *Handlers) detachSubtitle(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.attachedItem(w, r)
	if !ok {
		return
	}
	p, _ := auth.PrincipalFrom(reqCtx(r))
	err := library.Detach(reqCtx(r), h.d.Store.Pool(), id, chi.URLParam(r, "subtitleId"), attacher(p),
		h.d.Cfg.Policy().IsAdmin(p))
	switch {
	case errors.Is(err, library.ErrNoSuchSubtitle):
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, library.ErrNotAttached):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, library.ErrAttachedByOther):
		writeError(w, http.StatusForbidden, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the subtitle could not be removed: "+err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// The settings that name the instance's subtitle languages, the first one
// set deciding: its own (ADR-0014), else the languages its players show.
var languageSettings = []string{"subtitles.languages", "packager.language_whitelist"}

// getSubtitleLanguages answers GET /api/library/subtitles/languages: the
// languages every title of the instance is to have a subtitle in, each as
// languages.Tag spells it, in the order the setting names them, and the
// setting they come from — subtitles.languages, else the packager's language
// whitelist, the languages a player shows. None when neither is set.
func (h *Handlers) getSubtitleLanguages(w http.ResponseWriter, r *http.Request) {
	ss, err := h.d.Store.ListSettings(reqCtx(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the settings could not be read: "+err.Error())
		return
	}
	values := map[string]string{}
	for _, s := range ss { // ListSettings orders by key; the first row of a key wins
		key := strings.TrimSpace(s.Key)
		if _, twin := values[key]; !twin {
			values[key] = s.ValueText
		}
	}
	out := map[string]any{"languages": []string{}, "from": nil}
	for _, key := range languageSettings {
		var langs []string
		for _, part := range strings.Split(values[key], ",") {
			if tag, ok := languages.Tag(part); ok && !slices.Contains(langs, tag) {
				langs = append(langs, tag)
			}
		}
		if len(langs) > 0 {
			out["languages"], out["from"] = langs, key
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// attacher is who attaches a subtitle, as its row keeps it: the client of
// the caller's token (an addon's service account), else its subject.
func attacher(p *auth.Principal) string {
	switch {
	case p == nil:
		return ""
	case p.Client != "":
		return p.Client
	}
	return p.Subject
}
