package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// adoptRun serves POST /api/library/migrations/{run}/adopt, the flip of a
// migration run library-v2-from-catalog.py --platform staged under
// .work/migration/<run>/ (platform-library/1, 7.1-7.3): katalog-manager
// adopts its units, the renames and the database of each item at once, and
// then its staged people. revertRun serves …/revert, which replays the
// adoption backwards while the originals are not purged from the trash.
//
// Both are for the workers' service account and admins. The body may name
// the items to adopt or revert, {"items": ["<itemId>", …]}; without it, every
// unit of the run (and its people). They answer the report, what became of
// each unit (adopted, reverted, skipped, stale, busy, refused, failed, and
// why); 400 for a run's name that is none, 404 for a run that is not staged,
// 409 while another adopt or revert of it runs. A caller that goes away
// stops it between two units, never within one.
func (h *Handlers) adoptRun(w http.ResponseWriter, r *http.Request) {
	h.migrate(w, r, func(ctx context.Context, m *library.Migration, items []string) (any, error) {
		return m.Adopt(ctx, items)
	})
}

func (h *Handlers) revertRun(w http.ResponseWriter, r *http.Request) {
	h.migrate(w, r, func(ctx context.Context, m *library.Migration, items []string) (any, error) {
		return m.Revert(ctx, items)
	})
}

// namesRun serves POST /api/library/migrations/{run}/names, the catalog's
// side of the schemas' library-v2-neutral-names.py, which gives a tree
// written before 2026-10-08 the names the library gives its files and
// changes no database (its run is neutral-names unless it was named
// another): by the run's journal, every row of an item the run finished
// that names a file it renamed or took out of the record names it where it
// is now, and the item's recorded sources have the library's names for
// their files and no place among the arrivals, one transaction an item
// (library.Migration.Names). For the workers' service account and admins.
// The body may name the items, {"items": ["<itemId>", …]}; without it,
// every item of the journal. It answers {"run", "items", "rows", "skipped":
// [{"itemId", "reason"}]}: how many items it took, how many rows it changed
// (none when called again), and the items it left, why; 400 for a run's
// name that is none, 404 for a run that is not there, 409 while an adopt,
// a revert or another of these runs on it, 422 for a journal that names a
// file outside the folder of its item (nothing is changed).
func (h *Handlers) namesRun(w http.ResponseWriter, r *http.Request) {
	h.migrate(w, r, func(ctx context.Context, m *library.Migration, items []string) (any, error) {
		return m.Names(ctx, items)
	})
}

// migrate runs act on the run the path names, for the items the body names.
func (h *Handlers) migrate(w http.ResponseWriter, r *http.Request,
	act func(context.Context, *library.Migration, []string) (any, error)) {
	var body struct {
		Items []string `json:"items"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "the body is no {\"items\": [...]}: "+err.Error())
			return
		}
	}
	m, err := library.NewMigration(h.d.Store.Pool(), h.d.Cfg, chi.URLParam(r, "run"))
	switch {
	case errors.Is(err, library.ErrNoRun):
		writeError(w, http.StatusNotFound, "there is no migration run at "+strings.TrimPrefix(err.Error(), library.ErrNoRun.Error()+": "))
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rep, err := act(reqCtx(r), m, body.Items)
	switch {
	case errors.Is(err, library.ErrMigrationBusy):
		writeError(w, http.StatusConflict, "run "+m.Run+": "+err.Error())
	case errors.Is(err, library.ErrNamesRefused):
		writeError(w, http.StatusUnprocessableEntity, "run "+m.Run+": "+err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "run "+m.Run+": "+err.Error())
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}

// refreshProjections serves POST /api/library/projections: the items' library
// projections (metadata.json and its images) written now from the catalog,
// in either layout, where they are not what the projector would write
// (library.Projector.Refresh: rendered and compared byte for byte) — as the
// migration's verify needs them before the layout is v2, which the
// projector waits for. The body names the items, {"items": ["<itemId>", …]};
// {} (or none) looks at every recorded item. It answers {"projected",
// "unchanged", "failed", "items": [{"itemId", "state", "reason"}]}: each item
// named, or, with none named, each projected or failed; a projected one's
// reason is "behind" (its time marks said so) or "shape" (they did not: a
// projection of a shape from before). 409 while another projection holds
// the projector's lock. For the workers' service account and admins.
func (h *Handlers) refreshProjections(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []string `json:"items"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	if strings.TrimSpace(string(raw)) != "" {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "the body is no {\"items\": [...]}: "+err.Error())
			return
		}
	}
	rep, err := library.NewProjector(h.d.Store.Pool(), h.d.Cfg).Refresh(reqCtx(r), body.Items)
	switch {
	case errors.Is(err, library.ErrProjectorBusy):
		writeError(w, http.StatusConflict, "the projections: "+err.Error()+"; try again")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "the projections: "+err.Error())
	default:
		writeJSON(w, http.StatusOK, rep)
	}
}
