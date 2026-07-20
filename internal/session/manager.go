package session

import (
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager is a thread-safe registry of sessions with persistence.
type Manager struct {
	sessions    sync.Map // string → *Session
	internalSSH *sshserver.Server
	msgMgr      *message.Manager
	store       *storage.Store

	listChangeMu sync.RWMutex
	onListChange func()
	onTerminate  func(sessionID string)
}

// NewManager creates a Manager. internalSSH must be the built-in sshserver.Server (after Start) when using internal profiles; may be nil if only remote sessions are used in tests.
func NewManager(msgMgr *message.Manager, store *storage.Store, internalSSH *sshserver.Server) *Manager {
	return &Manager{
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

// SetTerminateListener registers a callback for session resource-tree teardown.
// It is invoked exactly once per session ID when the session root is finally closed
// (explicit terminate/delete/disconnect, or abnormal SSH disconnect that ends the session).
// Use this for child resources that cannot outlive a session (forwards, etc.).
func (m *Manager) SetTerminateListener(fn func(sessionID string)) {
	m.listChangeMu.Lock()
	m.onTerminate = fn
	m.listChangeMu.Unlock()
}

// notifySessionClosed runs the terminate listener for sessionID.
// Callers must ensure this runs at most once (Session.done is exitOnce-guarded).
func (m *Manager) notifySessionClosed(sessionID string) {
	m.listChangeMu.RLock()
	fn := m.onTerminate
	m.listChangeMu.RUnlock()
	if fn != nil {
		fn(sessionID)
	}
}

// NotifyChange triggers the session list change callback (for WebSocket/SSE push).
func (m *Manager) NotifyChange() {
	m.notifyListChange()
}

// Create starts a new session and registers it.
// The session is auto-removed from the registry only when the session root is
// closed (Terminate/Disconnect/abnormal SSH disconnect). Natural root-shell
// exit alone leaves the session in the registry (connection container may still
// host other shells/forwards), marking just the shell exited.
func (m *Manager) Create(cfg Config) (*Session, error) {
	s, err := New(m.internalSSH, cfg, m.msgMgr)
	if err != nil {
		return nil, err
	}

	m.sessions.Store(s.ID, s)

	sid := s.ID
	s.onExit = func() {
		// Session is the resource-tree root: remove it and release child resources.
		m.sessions.Delete(sid)
		m.persist()
		m.notifyListChange()
		if m.msgMgr != nil {
			m.msgMgr.ForgetSession(sid)
		}
		m.notifySessionClosed(sid)
	}
	s.onChildChange = m.notifyListChange

	m.persist()
	m.notifyListChange()
	return s, nil
}

// Get returns a session by ID.
func (m *Manager) Get(id string) *Session {
	v, ok := m.sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session)
}

// GetChildShell searches all sessions for a child shell with the given ID.
// Returns nil if no matching child shell is found.
func (m *Manager) GetChildShell(id string) *ChildShell {
	var found *ChildShell
	m.sessions.Range(func(_, v any) bool {
		if cs := v.(*Session).GetChildShell(id); cs != nil {
			found = cs
			return false
		}
		return true
	})
	return found
}

// GetByShellID returns the parent session that owns the given shell_id.
func (m *Manager) GetByShellID(shellID string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(shellID) != nil {
			found = s
			return false
		}
		return true
	})
	return found
}

// CloseChildShell terminates a child shell by its ID and removes it from the owning
// parent session's map. Returns found=false if no such child shell exists.
// The parent session and its SSH connection are unaffected.
func (m *Manager) CloseChildShell(id string) (bool, error) {
	var parent *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		if s.GetChildShell(id) != nil {
			parent = s
			return false
		}
		return true
	})
	if parent == nil {
		return false, nil
	}
	return true, parent.CloseChildShell(id)
}

// ListAll returns metadata for all sessions.
func (m *Manager) ListAll() []api.Session {
	var result []api.Session
	m.sessions.Range(func(_, v any) bool {
		result = append(result, v.(*Session).Info())
		return true
	})
	return result
}

// Terminate stops a session and tears down its resource tree.
// Shells are signalled/closed first, then Disconnect closes the SSH client and
// fires onExit (registry removal + one-shot child-resource cleanup).
func (m *Manager) Terminate(id string, force bool, gracePeriod time.Duration) {
	v, ok := m.sessions.Load(id)
	if !ok {
		return
	}
	s := v.(*Session)
	s.Terminate(force, gracePeriod)
	// Always close the session root so child resources cannot outlive it.
	s.Disconnect()
}

// Delete forcefully removes a session. Running sessions are terminated and disconnected first.
func (m *Manager) Delete(id string) error {
	if m.Get(id) == nil {
		return nil
	}
	m.Terminate(id, true, 0)
	return nil
}

// FindActiveBySSHConfig returns the first running session that matches the given
// ssh_config name (by session name or ssh_endpoint), or nil if none found.
func (m *Manager) FindActiveBySSHConfig(sshConfig string) *Session {
	var found *Session
	m.sessions.Range(func(_, v any) bool {
		s := v.(*Session)
		info := s.Info()
		if info.Status != api.SessionRunning {
			return true
		}
		if info.SSHEndpoint == sshConfig || info.Name == sshConfig {
			found = s
			return false
		}
		return true
	})
	return found
}

func (m *Manager) CleanupAll(force bool) {
	// Snapshot all session IDs, then terminate each one via Manager.Terminate
	// so the session resource tree (including forwards) is released once.
	var ids []string
	m.sessions.Range(func(k, _ any) bool {
		ids = append(ids, k.(string))
		return true
	})
	for _, id := range ids {
		m.Terminate(id, force, 0)
	}

	// Wait for exit goroutines to update status before persisting.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allExited := true
		for _, id := range ids {
			if s := m.Get(id); s != nil && s.Info().Status == api.SessionRunning {
				allExited = false
				break
			}
		}
		if allExited {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	m.persist(m.ListAll())
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
