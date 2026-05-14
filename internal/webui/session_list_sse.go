package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
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

// handleSessionListStream pushes the full session list on connect and after each registry change (SSE).
func (h *Handler) handleSessionListStream(w http.ResponseWriter, r *http.Request) {
	if h.Sessions == nil {
		http.Error(w, "sessions unavailable", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	_, signal, unregister := h.sessionHub().register()
	defer unregister()

	write := func() {
		list := h.Sessions.ListAll()
		data, err := json.Marshal(map[string]any{"sessions": list})
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: sessions\ndata: %s\n\n", string(data))
		fl.Flush()
	}

	write()
	for {
		select {
		case <-ctx.Done():
			return
		case <-signal:
			write()
		}
	}
}
