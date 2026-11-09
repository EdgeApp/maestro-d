package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// EventBus fans events out to SSE subscribers and keeps a small ring buffer so a
// client that reconnects with Last-Event-ID can catch up.
type EventBus struct {
	mu      sync.Mutex
	seq     int64
	ring    []Event
	ringCap int
	subs    map[chan Event]string // channel → device filter ("" = all)
	// observe, when set, sees each event before it is published.
	observe func(Event)
}

// NewEventBus creates an event bus keeping the last ringCap events.
func NewEventBus(ringCap int) *EventBus {
	if ringCap <= 0 {
		ringCap = 512
	}
	return &EventBus{ringCap: ringCap, subs: map[chan Event]string{}}
}

// Publish stamps and broadcasts an event. Slow subscribers drop events
// rather than block the step that produced them.
func (h *EventBus) Publish(ev Event) {
	if h.observe != nil {
		h.observe(ev)
	}
	h.mu.Lock()
	h.seq++
	ev.Seq = h.seq
	if ev.Time == "" {
		ev.Time = time.Now().Format(time.RFC3339Nano)
	}
	h.ring = append(h.ring, ev)
	if len(h.ring) > h.ringCap {
		h.ring = h.ring[len(h.ring)-h.ringCap:]
	}
	for ch, filter := range h.subs {
		if filter != "" && ev.Device != filter {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
	h.mu.Unlock()
}

// Subscribe returns a channel of events (optionally for one device) and
// the events after afterSeq still in the ring buffer.
func (h *EventBus) Subscribe(device string, afterSeq int64) (chan Event, []Event, func()) {
	ch := make(chan Event, 256)
	h.mu.Lock()
	h.subs[ch] = device
	var backlog []Event
	for _, ev := range h.ring {
		if ev.Seq > afterSeq && (device == "" || ev.Device == device) {
			backlog = append(backlog, ev)
		}
	}
	h.mu.Unlock()
	return ch, backlog, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Recent returns the buffered events after afterSeq.
func (h *EventBus) Recent(device string, afterSeq int64) []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Event
	for _, ev := range h.ring {
		if ev.Seq > afterSeq && (device == "" || ev.Device == device) {
			out = append(out, ev)
		}
	}
	return out
}

// ServeSSE streams events as text/event-stream until the client leaves.
func (h *EventBus) ServeSSE(w http.ResponseWriter, r *http.Request) {
	h.ServeSSEView(w, r, nil)
}

// ServeSSEView is ServeSSE with each event passed through view first: an
// event it returns false for is not sent. nil sends every event.
func (h *EventBus) ServeSSEView(w http.ResponseWriter, r *http.Request, view func(Event) (Event, bool)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	var after int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.URL.Query().Get("after"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	ch, backlog, unsub := h.Subscribe(r.URL.Query().Get("device"), after)
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	write := func(ev Event) bool {
		if view != nil {
			var ok bool
			if ev, ok = view(ev); !ok {
				return true
			}
		}
		data, err := json.Marshal(ev)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, ev := range backlog {
		if !write(ev) {
			return
		}
	}
	// Comment line so proxies/clients see bytes immediately.
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !write(ev) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
