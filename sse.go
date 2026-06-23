package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// sseMessage is a single Server-Sent Event: Event is matched by HTMX's
// sse-swap="<Event>" attribute, Data is the (already HTML-safe) payload.
type sseMessage struct {
	Event string
	Data  string
}

// Hub is a minimal SSE fan-out: every client subscribes to one global stream
// and routing is done by event name rather than by connection. The "devices"
// event carries device-list/state changes; "console:<id>" and "flash:<id>"
// carry a single device's live output. Each page reacts only to the event
// names it declares via sse-swap, so one connection serves the whole UI.
type Hub struct {
	mu   sync.Mutex
	subs map[chan sseMessage]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[chan sseMessage]struct{})}
}

// SSE event names. Per-device events are namespaced by id so a page can swap
// only its own device's output off the shared stream.
const eventDevices = "devices"

func consoleEvent(deviceID string) string { return "console:" + deviceID }
func flashEvent(deviceID string) string   { return "flash:" + deviceID }

// Subscribe registers a buffered channel on the stream and returns it plus an
// unsubscribe function.
func (h *Hub) Subscribe() (<-chan sseMessage, func()) {
	ch := make(chan sseMessage, 256)
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

// Publish delivers msg to every subscriber. Slow subscribers that have filled
// their buffer are skipped (dropped messages) rather than blocking.
func (h *Hub) Publish(msg sseMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Serve streams the global event stream to an HTTP client as text/event-stream
// over a single connection until the request is cancelled.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := h.Subscribe()
	defer unsub()

	// Tell the client to retry quickly if the connection drops.
	fmt.Fprint(w, "retry: 1000\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			writeSSE(w, msg)
			flusher.Flush()
		}
	}
}

// writeSSE emits one event frame. Multi-line data is split into multiple
// "data:" lines per the SSE spec.
func writeSSE(w http.ResponseWriter, msg sseMessage) {
	if msg.Event != "" {
		fmt.Fprintf(w, "event: %s\n", msg.Event)
	}
	for _, line := range strings.Split(msg.Data, "\n") {
		fmt.Fprintf(w, "data: %s\n", line)
	}
	fmt.Fprint(w, "\n")
}
