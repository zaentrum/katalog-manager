package rest

import (
	"net/http"
	"sort"
	"strings"

	"github.com/zaentrum/katalog-manager/internal/model"
)

// workerSetting is a setting as the workers read it.
type workerSetting struct {
	ValueText string `json:"valueText"`
	ValueType string `json:"valueType"`
}

// getSettings serves the settings that are no secret to the workers, as the
// packager reads them: {"<key>": {"valueText": "...", "valueType": "..."}},
// e.g. its language whitelist (packager.language_whitelist, comma-separated)
// and whether a single track of another language stays visible
// (packager.keep_original_if_single). A key is given without the spaces around
// it, and a key the table holds twice once, its row of the lowest id. A secret
// (model.IsSecretSetting) is left out, set or not: it is write-only, here as in
// GraphQL.
func (h *Handlers) getSettings(w http.ResponseWriter, r *http.Request) {
	ss, err := h.d.Store.ListSettings(reqCtx(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "settings lookup failed")
		return
	}
	sort.SliceStable(ss, func(i, j int) bool {
		ki, kj := strings.TrimSpace(ss[i].Key), strings.TrimSpace(ss[j].Key)
		if ki != kj {
			return ki < kj
		}
		return ss[i].ID < ss[j].ID
	})
	out := map[string]workerSetting{}
	for _, s := range ss {
		key := strings.TrimSpace(s.Key)
		if key == "" || model.IsSecretSetting(key) {
			continue
		}
		if _, twin := out[key]; twin {
			continue
		}
		out[key] = workerSetting{ValueText: s.ValueText, ValueType: s.ValueType}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
