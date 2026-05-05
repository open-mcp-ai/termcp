package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager is a thread-safe registry of sessions with persistence.
type Manager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
	sshAddr  string
	msgMgr   *message.Manager
	store    *storage.Store
}

// NewManager creates a Manager.
func NewManager(sshAddr string, msgMgr *message.Manager, store *storage.Store) *Manager {
	return &Manager{
		sessions: make(map[string]*Session),
		sshAddr:  sshAddr,
		msgMgr:   msgMgr,
		store:    store,
	}
}

// Create starts a new session and registers it.
func (m *Manager) Create(cfg Config) (*Session, error) {
	s, err := New(m.sshAddr, cfg, m.msgMgr)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	m.persist()
	return s, nil
}

// Get returns a session by ID.
func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// ListAll returns metadata for all sessions.
func (m *Manager) ListAll() []api.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]api.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s.Info())
	}
	return result
}

// Terminate stops a session.
func (m *Manager) Terminate(id string, force bool, gracePeriod time.Duration) {
	m.mu.RLock()
	s := m.sessions[id]
	m.mu.RUnlock()
	if s != nil {
		s.Terminate(force, gracePeriod)
		m.persist()
	}
}

// Delete removes an exited session from the registry.
// Returns error if the session is still running.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return nil
	}
	if s.Info().Status == api.SessionRunning {
		m.mu.Unlock()
		return fmt.Errorf("cannot delete running session %q, terminate it first", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	m.persist()
	return nil
}

// CleanupAll terminates all running sessions and waits for them to exit.
func (m *Manager) CleanupAll(force bool) {
	m.mu.RLock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	// Capture session metadata while holding lock to avoid re-acquiring.
	sessions := m.ListAll()
	m.mu.RUnlock()
	for _, s := range list {
		s.Terminate(force, 0)
	}
	// Wait for exit goroutines to update status before persisting.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allExited := true
		for _, s := range list {
			if s.Info().Status == api.SessionRunning {
				allExited = false
				break
			}
		}
		if allExited {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.persist(sessions)
}

func (m *Manager) persist(sessions ...[]api.Session) {
	if m.store == nil {
		return
	}
	var list []api.Session
	if len(sessions) > 0 {
		list = sessions[0]
	} else {
		list = m.ListAll()
	}
	_ = m.store.SaveSessions(list)
}
