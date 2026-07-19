// Package stream fans catalog pipeline events out to browsers over SSE.
// A single per-pod Kafka consumer (latest offset — live tail, no history
// replay) publishes thin notifications into an in-process broker; each
// connected client gets them over a text/event-stream response. Events are
// NOTIFICATIONS, not data: {type, itemId, itemType, phase} — the frontend
// debounces and refetches its own queries, so there is no payload schema to
// keep in sync.
//
// Heartbeat comments go out every 20s so idle streams survive the OpenShift
// router's ~30s inactivity timeout. Slow clients are dropped rather than
// blocking the fan-out (they reconnect and refetch, which is the contract
// anyway).
package stream

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	heartbeatEvery = 20 * time.Second
	clientBuffer   = 16 // events buffered per client before it is dropped
)

// Note is the thin notification sent to browsers.
type Note struct {
	Type     string `json:"type"`               // always "catalog.updated"
	ItemID   string `json:"itemId,omitempty"`
	ItemType string `json:"itemType,omitempty"` // movie|series|episode
	Phase    string `json:"phase,omitempty"`    // discovered|enriched|analyzed|transcoded
}

// Broker fans events out to subscribed SSE clients.
type Broker struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func NewBroker() *Broker { return &Broker{subs: map[chan []byte]struct{}{}} }

// Publish sends a note to every subscriber. Non-blocking: a client whose
// buffer is full is dropped (its handler notices the closed channel and ends
// the response; the browser reconnects and refetches).
func (b *Broker) Publish(n Note) {
	n.Type = "catalog.updated"
	payload, err := json.Marshal(n)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- payload:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func (b *Broker) subscribe() chan []byte {
	ch := make(chan []byte, clientBuffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.mu.Unlock()
}

// Clients reports the number of connected subscribers (for logs/metrics).
func (b *Broker) Clients() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Handler serves the SSE stream. Mount it inside an authenticated router
// group — it does no auth of its own.
func (b *Broker) Handler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// An immediate comment confirms the stream to the client (and flushes
	// proxy buffers) before the first real event.
	_, _ = fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

	ch := b.subscribe()
	defer b.unsubscribe(ch)
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-hb.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			fl.Flush()
		case payload, ok := <-ch:
			if !ok {
				return // dropped as a slow client — the browser reconnects
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// PhaseOf maps a catalog topic name to its pipeline phase (the last segment):
// stube.catalog.item.enriched -> "enriched".
func PhaseOf(topic string) string {
	if i := strings.LastIndex(topic, "."); i >= 0 {
		return topic[i+1:]
	}
	return topic
}

// LogClients logs the subscriber count (call at connect/disconnect points or
// on an interval if desired). Kept tiny — no metrics dependency.
func (b *Broker) LogClients(prefix string) {
	log.Printf("%s: %d sse client(s)", prefix, b.Clients())
}
