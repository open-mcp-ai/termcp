package sshconfig

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// AdminHandler serves GET/PUT/DELETE under /api/ssh-configs.
type AdminHandler struct {
	Store *Store
	Token string
}

func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil || strings.TrimSpace(h.Token) == "" {
		http.NotFound(w, r)
		return
	}
	if !h.checkAuth(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	base := "/api/ssh-configs"
	if r.URL.Path != base && !strings.HasPrefix(r.URL.Path, base+"/") {
		http.NotFound(w, r)
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, base)
	suffix = strings.TrimPrefix(suffix, "/")
	suffix = strings.TrimSpace(suffix)

	switch r.Method {
	case http.MethodGet:
		if suffix != "" {
			http.NotFound(w, r)
			return
		}
		names, err := h.Store.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		arr := make([]any, len(names))
		for i, n := range names {
			arr[i] = n
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ssh_configs": arr})
	case http.MethodPut:
		if suffix == "" || strings.Contains(suffix, "/") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxConfigFileSize+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxConfigFileSize {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := h.Store.Save(suffix, body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if suffix == "" || strings.Contains(suffix, "/") {
			http.NotFound(w, r)
			return
		}
		if err := h.Store.Delete(suffix); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *AdminHandler) checkAuth(r *http.Request) bool {
	tok := strings.TrimSpace(h.Token)
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		got := strings.TrimSpace(auth[7:])
		return constantTimeEqual(got, tok)
	}
	if got := r.Header.Get("X-Admin-Token"); got != "" {
		return constantTimeEqual(strings.TrimSpace(got), tok)
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
