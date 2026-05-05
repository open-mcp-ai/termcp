package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

var (
	ErrInvalidID = errors.New("storage: invalid ID")
	validIDRe    = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func validateID(id string) error {
	if !validIDRe.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// Store handles JSON file persistence for sessions and messages.
type Store struct {
	dataDir string
	mu      sync.RWMutex
}

// New creates a Store rooted at dataDir.
func New(dataDir string) *Store {
	return &Store{dataDir: dataDir}
}

// initDir ensures the directory exists.
func (s *Store) initDir(path string) error {
	return os.MkdirAll(path, 0700)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// SaveSessions writes the full session list.
func (s *Store) SaveSessions(sessions []api.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.initDir(s.dataDir); err != nil {
		return err
	}
	path := filepath.Join(s.dataDir, "sessions.json")
	data, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0644)
}

// LoadSessions reads the full session list.
func (s *Store) LoadSessions() ([]api.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.dataDir, "sessions.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []api.Session{}, nil
		}
		return nil, err
	}
	var sessions []api.Session
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

// SaveMessageIndex writes the message index for a session.
func (s *Store) SaveMessageIndex(sessionID string, entries []api.MessageIndexEntry) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dataDir, "messages", sessionID)
	if err := s.initDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, "index.json")
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0644)
}

// LoadMessageIndex reads the message index for a session.
func (s *Store) LoadMessageIndex(sessionID string) ([]api.MessageIndexEntry, error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.dataDir, "messages", sessionID, "index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []api.MessageIndexEntry{}, nil
		}
		return nil, err
	}
	var entries []api.MessageIndexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// SaveMessage writes a single message content.
func (s *Store) SaveMessage(sessionID string, msg api.Message) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(msg.ID); err != nil {
		return fmt.Errorf("message ID: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Join(s.dataDir, "messages", sessionID, "messages")
	if err := s.initDir(dir); err != nil {
		return err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.json", msg.ID))
	data, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0644)
}

// LoadMessage reads a single message content.
func (s *Store) LoadMessage(sessionID, msgID string) (*api.Message, error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	if err := validateID(msgID); err != nil {
		return nil, fmt.Errorf("message ID: %w", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := filepath.Join(s.dataDir, "messages", sessionID, "messages", fmt.Sprintf("%s.json", msgID))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var msg api.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// LoadMessages reads multiple messages by ID.
func (s *Store) LoadMessages(sessionID string, msgIDs []string) ([]api.Message, error) {
	var result []api.Message
	for _, id := range msgIDs {
		msg, err := s.LoadMessage(sessionID, id)
		if err != nil {
			continue
		}
		result = append(result, *msg)
	}
	return result, nil
}
