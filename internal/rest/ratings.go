package rest

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/zaentrum/katalog-manager/internal/auth"
	"github.com/zaentrum/katalog-manager/internal/ratings"
)

// A viewer capped at an age (auth.Principal.MaxRating: the max_rating claim
// of the bearer, or the cap of the stream token) is served nothing of a title
// rated above the cap, nor of an unrated one unless the setting
// ratings.unrated_for_capped says show: the artwork, playback and subtitle
// routes answer it as they answer for a title there is not, so the answer
// does not say the title exists. Everyone else is served as before.

// unratedEvery is how long the setting ratings.unrated_for_capped is kept
// before it is read again.
const unratedEvery = 30 * time.Second

// unratedPolicy is ratings.unrated_for_capped as last read.
type unratedPolicy struct {
	mu    sync.Mutex
	show  bool
	until time.Time
}

// hiddenFrom reports whether the caller of r is capped and may not be served
// the item itemID: one there is not, one rated above the cap, or an unrated
// one while ratings.unrated_for_capped does not say show.
func (h *Handlers) hiddenFrom(r *http.Request, itemID string) (bool, error) {
	p, _ := auth.PrincipalFrom(r.Context())
	age, capped := p.MaxRating()
	if !capped {
		return false, nil
	}
	visible, err := h.d.Store.VisibleAt(reqCtx(r), itemID, age, h.showUnrated(reqCtx(r)))
	return !visible, err
}

// showUnrated is the setting ratings.unrated_for_capped, read at most every
// unratedEvery: true when it says show. Unset, unreadable or anything else,
// it hides, so a capped viewer is never served more than the setting allows.
func (h *Handlers) showUnrated(ctx context.Context) bool {
	h.unrated.mu.Lock()
	defer h.unrated.mu.Unlock()
	if time.Now().Before(h.unrated.until) {
		return h.unrated.show
	}
	show := false
	if s, err := h.d.Store.GetSettingByKey(ctx, ratings.UnratedSetting); err != nil {
		log.Printf("ratings: %s could not be read, so unrated titles are hidden from capped viewers: %v", ratings.UnratedSetting, err)
	} else if s != nil {
		show = ratings.ShowUnrated(s.ValueText)
	}
	h.unrated.show, h.unrated.until = show, time.Now().Add(unratedEvery)
	return show
}
