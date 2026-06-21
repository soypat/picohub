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

// Hub is a minimal topic-based SSE fan-out. Topics are arbitrary strings;
// "events" carries global device-list/state changes, "console:<id>" carries a
// device's live serial output.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan sseMessage]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: make(map[string]map[chan sseMessage]struct{})}
}

// topicConsole returns the per-device console topic name.
func topicConsole(deviceID string) string { return "console:" + deviceID }

const topicEvents = "events"

// Subscribe registers a buffered channel on a topic and returns it plus an
// unsubscribe function.
func (h *Hub) Subscribe(topic string) (<-chan sseMessage, func()) {
	ch := make(chan sseMessage, 256)
	h.mu.Lock()
	if h.subs[topic] == nil {
		h.subs[topic] = make(map[chan sseMessage]struct{})
	}
	h.subs[topic][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if set := h.subs[topic]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, topic)
			}
		}
		h.mu.Unlock()
		close(ch)
	}
}

// Publish delivers msg to every subscriber of topic. Slow subscribers that have
// filled their buffer are skipped (dropped messages) rather than blocking.
func (h *Hub) Publish(topic string, msg sseMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[topic] {
		select {
		case ch <- msg:
		default:
		}
	}
}

// ServeTopic streams a topic to an HTTP client as text/event-stream until the
// request is cancelled.
func (h *Hub) ServeTopic(w http.ResponseWriter, r *http.Request, topic string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, unsub := h.Subscribe(topic)
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
