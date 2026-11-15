// Package live: per user change notification channel (Server-Sent Events),
// shared by the ocapps modules.
//
// The client keeps ONE request open against the stream, when something changes
// the server sends a small message with the topic that changed and the client
// reloads THAT data with the normal API (no data is sent over the stream, only
// invalidations). Advantages over the client polling: millisecond latency
// instead of "every N seconds", zero requests when nothing happens, and the
// scanning cost is not multiplied by open tabs.
//
// It is multi-tenant: every subscriber is tied to an owner (oc_id) and only
// receives the events of ITS owner. A hub belongs to the process, not to a
// request.
package live

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// clientHeader is sent by the client in every write request, so that the event
// says which client made the change and that client does not reload what it
// already has. It is optional: without it, everyone reloads.
const clientHeader = "X-Ocapps-Client"

// maxClientIDLen caps the client id: it is reflected in the stream and we do
// not want to relay arbitrarily long strings.
const maxClientIDLen = 64

// heartbeatEvery keeps the connection alive through proxies that close idle
// streams (nginx proxy_read_timeout default: 60 s).
const heartbeatEvery = 25 * time.Second

// Event is a change notification: what changed and who changed it (empty if it
// is unknown or if the server changed it, e.g. a background scan).
type Event struct {
	Topic  string `json:"topic"`
	Client string `json:"client,omitempty"`
}

// Hub distributes events among the open streams.
type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[chan Event]struct{}
	closed bool
	done   chan struct{}
}

// NewHub builds a hub with no subscribers.
func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan Event]struct{}{}, done: make(chan struct{})}
}

// ClientID reads the client id from the request ("" if missing or too long).
func ClientID(r *http.Request) string {
	id := r.Header.Get(clientHeader)
	if len(id) > maxClientIDLen {
		return ""
	}
	return id
}

// Subscribe opens a channel for the owner. It returns the channel, the
// function to close it (idempotent) and false if the hub is already closed.
//
// Subscribe exists for the stream itself and for the tests, modules that want
// to react to in-process events can use it as well.
func (h *Hub) Subscribe(owner string) (<-chan Event, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, func() {}, false
	}
	ch := make(chan Event, 16)
	if h.subs[owner] == nil {
		h.subs[owner] = map[chan Event]struct{}{}
	}
	h.subs[owner][ch] = struct{}{}
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[owner], ch)
			if len(h.subs[owner]) == 0 {
				delete(h.subs, owner)
			}
			close(ch)
		})
	}
	return ch, unsubscribe, true
}

// Publish sends the event to the streams of THAT owner. A subscriber that does
// not read in time loses the message (and will receive the next one): we
// prefer losing an invalidation to blocking a write. Without an owner nothing
// is published.
func (h *Hub) Publish(owner string, ev Event) {
	if owner == "" || ev.Topic == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[owner] {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribers returns how many streams the owner has open (metrics and tests).
func (h *Hub) Subscribers(owner string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[owner])
}

// Close ends all the streams (process shutdown). Without it,
// http.Server.Shutdown waits for the handlers in progress until its budget is
// exhausted, because a stream never ends on its own.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
}

// Serve handles the owner SSE stream. It does not write application data: only
// invalidation events and pings.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, owner string) {
	if owner == "" {
		http.Error(w, "owner required", http.StatusUnauthorized)
		return
	}
	ch, unsubscribe, ok := h.Subscribe(owner)
	if !ok {
		http.Error(w, "server shutting down", http.StatusServiceUnavailable)
		return
	}
	defer unsubscribe()

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	// retry: if the browser reconnects on its own, it waits 3 s.
	if _, err := w.Write([]byte("retry: 3000\n: connected\n\n")); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		var msg []byte
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-beat.C:
			msg = []byte(": ping\n\n")
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			msg = append(append([]byte("data: "), b...), '\n', '\n')
		}
		if _, err := w.Write(msg); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
