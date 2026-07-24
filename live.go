package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Live updates: when a block changes in the running world, the affected tiles
// must be re-meshed and the browser told to re-fetch them.
//
// Two pieces live here: a dirty-tile coalescer (a burst of edits — someone
// building — collapses into one flush) and a Server-Sent Events endpoint the
// viewer subscribes to. SSE is a good fit: it's one-way, survives proxies, and
// the browser's EventSource reconnects on its own.

// flushInterval coalesces edit bursts into a single invalidation round.
const flushInterval = 500 * time.Millisecond

// editRadius is how many chunks around an edited chunk get invalidated. A block
// change can affect its neighbours' rendering through face culling and light
// bleed across the chunk border, so re-mesh the 3x3 neighbourhood.
const editRadius = 1

type tileKey [2]int

// liveHub fans tile invalidations out to connected viewers.
type liveHub struct {
	mu    sync.Mutex
	subs  map[chan tileKey]struct{}
	dirty map[tileKey]struct{}
}

func newLiveHub() *liveHub {
	return &liveHub{
		subs:  map[chan tileKey]struct{}{},
		dirty: map[tileKey]struct{}{},
	}
}

// markDirty queues the chunks affected by a block change at world (x, z).
func (h *liveHub) markDirty(x, z int) {
	cx, cz := floorDiv(x, 16), floorDiv(z, 16)
	h.mu.Lock()
	defer h.mu.Unlock()
	for dx := -editRadius; dx <= editRadius; dx++ {
		for dz := -editRadius; dz <= editRadius; dz++ {
			h.dirty[tileKey{cx + dx, cz + dz}] = struct{}{}
		}
	}
}

// subscribe registers a viewer; the returned cancel func removes it.
func (h *liveHub) subscribe() (chan tileKey, func()) {
	ch := make(chan tileKey, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
		close(ch)
	}
}

// takeDirty returns and clears the pending invalidation set.
func (h *liveHub) takeDirty() []tileKey {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.dirty) == 0 {
		return nil
	}
	out := make([]tileKey, 0, len(h.dirty))
	for k := range h.dirty {
		out = append(out, k)
	}
	h.dirty = map[tileKey]struct{}{}
	return out
}

// publish sends a tile invalidation to every subscriber (dropping it for any
// viewer that has fallen behind rather than blocking the flush loop).
func (h *liveHub) publish(k tileKey) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- k:
		default:
		}
	}
}

// runFlusher periodically drops invalidated tiles from the mesh cache and tells
// viewers to re-fetch them.
func (s *server) runFlusher() {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		keys := s.live.takeDirty()
		if len(keys) == 0 {
			continue
		}
		s.mu.Lock()
		for _, k := range keys {
			delete(s.cache, k)
		}
		s.mu.Unlock()
		for _, k := range keys {
			s.live.publish(k)
		}
		log.Printf("live: invalidated %d tiles", len(keys))
	}
}

// handleEvents streams tile invalidations to a viewer as Server-Sent Events.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Defeat proxy buffering so events arrive promptly.
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.live.subscribe()
	defer cancel()

	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Keepalive comments stop idle proxies from dropping the stream.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case k := <-ch:
			fmt.Fprintf(w, "event: tile\ndata: {\"cx\":%d,\"cz\":%d}\n\n", k[0], k[1])
			flusher.Flush()
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// floorDiv is integer division rounding toward negative infinity (correct chunk
// index for negative world coordinates).
func floorDiv(a, b int) int {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}
