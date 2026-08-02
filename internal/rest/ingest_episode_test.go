package rest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An episode without coordinates must be REFUSED at the boundary.
//
// Accepting it is not a small inaccuracy: enrichEpisode returns "episode has no
// series parent" -> not_found -> skipped, and the worker advances on
// done|not_found alike, so the item is published as a playable, metadata-less
// orphan named after the show with no error anywhere. Production holds 64 such
// rows, every one of them carrying a primary asset. The 400 is the only thing
// that converts a silent permanent defect into a visible failure.
func TestEpisodeWithoutCoordinatesIsRejected(t *testing.T) {
	h := &Handlers{}
	for _, body := range []map[string]any{
		{"path": "/var/lib/katalog/media/x.mkv", "type": "episode", "title": "Show"},
		{"path": "/var/lib/katalog/media/x.mkv", "type": "episode", "title": "Show", "parentId": "p"},
		{"path": "/var/lib/katalog/media/x.mkv", "type": "episode", "title": "Show",
			"parentId": "p", "seasonNumber": 1},
		{"path": "/var/lib/katalog/media/x.mkv", "type": "episode", "title": "Show",
			"parentId": "", "seasonNumber": 1, "episodeNumber": 2},
	} {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(raw))
		w := httptest.NewRecorder()
		h.ingest(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %v -> %d, want 400 (an unlinked episode becomes a playable orphan)", body, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "orphan") {
			t.Errorf("the rejection does not explain why: %s", w.Body.String())
		}
	}
}

// A movie needs no coordinates and must still be accepted past this guard —
// the check must not become a blanket ingest block.
func TestMovieIsNotBlockedByTheEpisodeGuard(t *testing.T) {
	h := &Handlers{}
	raw, _ := json.Marshal(map[string]any{
		"path": "/definitely/not/under/a/root/x.mkv", "type": "movie", "title": "M"})
	r := httptest.NewRequest(http.MethodPost, "/api/ingest", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	h.ingest(w, r)
	// It still fails — on the ROOT guard, which runs after — but it must not be
	// refused for missing episode coordinates.
	if strings.Contains(w.Body.String(), "orphan") {
		t.Errorf("a movie was rejected by the episode guard: %s", w.Body.String())
	}
}
