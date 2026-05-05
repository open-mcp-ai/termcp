package message

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// Manager handles persistence of session messages.
type Manager struct {
	store   *storage.Store
	mu      sync.Mutex
	session map[string]*sync.Mutex
}

// NewManager creates a Manager backed by the given Store.
func NewManager(store *storage.Store) *Manager {
	return &Manager{store: store, session: make(map[string]*sync.Mutex)}
}

func (m *Manager) sessionLock(sessionID string) *sync.Mutex {
	m.mu.Lock()
	mu, ok := m.session[sessionID]
	if !ok {
		mu = &sync.Mutex{}
		m.session[sessionID] = mu
	}
	m.mu.Unlock()
	return mu
}

// Append records a new message and persists it.
func (m *Manager) Append(sessionID string, typ api.MsgType, content string) (*api.Message, error) {
	msg := api.Message{
		ID:        uuid.New().String()[:12],
		SessionID: sessionID,
		Type:      typ,
		Content:   content,
		CreatedAt: time.Now().UTC(),
		ByteSize:  len(content),
	}
	if err := m.store.SaveMessage(sessionID, msg); err != nil {
		return nil, err
	}

	mu := m.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	entries, err := m.store.LoadMessageIndex(sessionID)
	if err != nil {
		entries = []api.MessageIndexEntry{}
	}
	entries = append(entries, api.MessageIndexEntry{
		ID:        msg.ID,
		Type:      msg.Type,
		CreatedAt: msg.CreatedAt,
		ByteSize:  msg.ByteSize,
	})
	if err := m.store.SaveMessageIndex(sessionID, entries); err != nil {
		return nil, err
	}
	return &msg, nil
}

// List returns the message index for a session.
func (m *Manager) List(sessionID string) ([]api.MessageIndexEntry, error) {
	return m.store.LoadMessageIndex(sessionID)
}

// Get returns a single message by ID.
func (m *Manager) Get(sessionID, msgID string) (*api.Message, error) {
	return m.store.LoadMessage(sessionID, msgID)
}

// GetMany returns multiple messages by ID.
func (m *Manager) GetMany(sessionID string, msgIDs []string) ([]api.Message, error) {
	return m.store.LoadMessages(sessionID, msgIDs)
}
