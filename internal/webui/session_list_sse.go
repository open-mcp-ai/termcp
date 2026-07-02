package webui

import (
	"sync"
)

// sessionListHub fans out coalesced signals to SSE subscribers when the session registry changes.
type sessionListHub struct {
	mu      sync.Mutex
	nextID  uint64
	clients map[uint64]chan struct{}
}

func newSessionListHub() *sessionListHub {
	return &sessionListHub{clients: make(map[uint64]chan struct{})}
}

func (h *sessionListHub) register() (id uint64, signal <-chan struct{}, unregister func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.nextID++
	id = h.nextID
	h.clients[id] = ch
	h.mu.Unlock()
	return id, ch, func() {
		h.mu.Lock()
		delete(h.clients, id)
		h.mu.Unlock()
	}
}

func (h *sessionListHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.clients {
		// Buffered(1): if a wake is already pending, a second default would drop this notify
		// and the client might never pull ListAll() again — drain then enqueue one coalesced wake.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Handler) sessionHub() *sessionListHub {
	if h.sessHub == nil {
		h.sessHub = newSessionListHub()
	}
	return h.sessHub
}

