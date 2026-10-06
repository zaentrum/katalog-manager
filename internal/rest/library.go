package rest

import (
	"context"

	"github.com/zaentrum/katalog-manager/internal/library"
)

// settings reads the library's settings (library.ReadSettings); handlers
// without a store have the defaults, the legacy layout.
func (h *Handlers) settings(ctx context.Context) (library.Settings, error) {
	if h.d.Store == nil {
		return library.Defaults(), nil
	}
	return library.ReadSettings(ctx, h.d.Store.Pool())
}

// paths are where the configuration puts the library.
func (h *Handlers) paths() library.Paths { return library.PathsOf(h.d.Cfg) }
