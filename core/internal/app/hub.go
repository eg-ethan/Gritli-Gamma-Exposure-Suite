package app

import (
	"encoding/json"
	"sync"
)

// Hub is a tiny SSE-style broadcaster: subscribers get named events with
// pre-marshaled JSON payloads. Slow subscribers drop events rather than
// blocking publishers (the same explicit drop policy the architecture mandates
// for the edge channels — snapshots are periodic, so dropping one is safe).
type Hub struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]chan Event
}

// Event is one broadcast: a name plus the marshaled payload.
type Event struct {
	Name string
	Data []byte
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[uint64]chan Event{}}
}

// Subscribe registers a subscriber and returns its id, the event channel, and
// a cancel func. The channel is buffered; the cancel func must be called on
// disconnect.
func (h *Hub) Subscribe() (uint64, <-chan Event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Event, 16)
	h.subs[id] = ch
	return id, ch, func() { h.Unsubscribe(id) }
}

// Unsubscribe removes a subscriber.
func (h *Hub) Unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// Broadcast marshals payload once and delivers it to every subscriber.
// A subscriber whose buffer is full loses the event (non-blocking send).
func (h *Hub) Broadcast(name string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- Event{Name: name, Data: data}:
		default:
		}
	}
}
