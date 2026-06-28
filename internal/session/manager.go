package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager is a thread-safe registry of sessions with persistence.
type Manager struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	sshAddr     string
	internalSSH *sshserver.Server // built-in server for Mint + internal sessions; nil in some tests
	msgMgr      *message.Manager
	store       *storage.Store

	listChangeMu sync.RWMutex
	onListChange func()
}

// NewManager creates a Manager. internalSSH must be the built-in sshserver.Server (after Start) when using internal profiles; may be nil if only remote sessions are used in tests.
func NewManager(sshAddr string, msgMgr *message.Manager, store *storage.Store, internalSSH *sshserver.Server) *Manager {
	return &Manager{
		sessions:    make(map[string]*Session),
		sshAddr:     sshAddr,
		internalSSH: internalSSH,
		msgMgr:      msgMgr,
		store:       store,
	}
}

// SetSessionListListener registers a callback invoked without holding Manager locks whenever
// the session set or a session's lifecycle state may have changed (create, delete, exit, terminate).
func (m *Manager) SetSessionListListener(fn func()) {
	m.listChangeMu.Lock()
	m.onListChange = fn
	m.listChangeMu.Unlock()
}

func (m *Manager) notifyListChange() {
	m.listChangeMu.RLock()
	fn := m.onListChange
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// NotifyChange triggers the session list change callback (for WebSocket/SSE push).
func (m *Manager) NotifyChange() {
	m.notifyListChange()
}

// Create starts a new session and registers it.
func (m *Manager) Create(cfg Config) (*Session, error) {
	cfg.OnExit = func() {
		m.persist()
		m.notifyListChange()
	}
	s, err := New(m.sshAddr, m.internalSSH, cfg, m.msgMgr)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	m.persist()
	m.notifyListChange()
	return s, nil
}

// Get returns a session by ID.
func (m *Manager) Get(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// GetChildShell searches all sessions for a child shell with the given ID.
// Returns nil if no matching child shell is found.
func (m *Manager) GetChildShell(id string) *ChildShell {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if cs := s.GetChildShell(id); cs != nil {
			return cs
		}
	}
	return nil
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
		m.notifyListChange()
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
	m.notifyListChange()
	return nil
}

// CleanupAll terminates all running sessions and waits for them to exit.
// FindActiveBySSHConfig returns the first running session that matches the given
// ssh_config name (by session name or ssh_endpoint), or nil if none found.
func (m *Manager) FindActiveBySSHConfig(sshConfig string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		info := s.Info()
		if info.Status != api.SessionRunning {
			continue
		}
		// Match by ssh_endpoint first, then by session name.
		if info.SSHEndpoint == sshConfig || info.Name == sshConfig {
			return s
		}
	}
	return nil
}

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
	m.notifyListChange()
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
