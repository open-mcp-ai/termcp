package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store manages dataDir/ssh_configs/<name>/config.toml for remote profiles.
// The built-in "internal" profile is virtual: it is never written to disk.
type Store struct {
	dataDir string
	mu      sync.Mutex
}

// NewStore returns a Store rooted at dataDir (always absolute path when Abs succeeds).
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		abs = d
	}
	return &Store{dataDir: abs}
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "ssh_configs")
}

// ConfigDir returns the directory containing config.toml for a named profile.
// For the virtual internal profile this path is not used for I/O.
func (s *Store) ConfigDir(name string) string {
	return filepath.Join(s.root(), name)
}

func (s *Store) configPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

func isInternalName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "internal")
}

// InternalEntry returns the built-in loopback profile (not loaded from disk).
func InternalEntry() *Entry {
	ent, err := ParseAndValidate(InternalTemplate())
	if err != nil {
		// Template is compile-time constant; fallback if somehow invalid.
		return &Entry{Kind: KindInternal}
	}
	return ent
}

// Load reads and validates a named config.
// name "internal" is always the virtual built-in entry (disk leftovers are ignored).
func (s *Store) Load(name string) (*Entry, error) {
	name = strings.TrimSpace(name)
	if isInternalName(name) {
		return InternalEntry(), nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ssh config %q not found (use data-dir/ssh_configs/%s/config.toml)", name, name)
		}
		return nil, err
	}
	return ParseAndValidate(data)
}

// ReadRaw returns the raw config.toml bytes for a name.
// For the virtual internal profile it returns InternalTemplate().
func (s *Store) ReadRaw(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if isInternalName(name) {
		return InternalTemplate(), nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if _, err := ParseAndValidate(data); err != nil {
		return nil, err
	}
	return data, nil
}

// List returns sorted config names: virtual "internal" plus remote profiles on disk.
// Leftover ssh_configs/internal/ directories are ignored.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root(), 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{"internal"}, nil
		}
		return nil, err
	}
	names := []string{"internal"}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if isInternalName(name) {
			continue // disk leftover; virtual entry already listed
		}
		cfg := filepath.Join(s.root(), name, "config.toml")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save writes config for a remote name (validates first).
// The virtual internal profile cannot be saved.
func (s *Store) Save(name string, data []byte) error {
	if _, err := ParseAndValidate(data); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if isInternalName(name) {
		return fmt.Errorf("cannot save reserved virtual ssh config %q", "internal")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.root(), name, "config.toml")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-cfg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, p)
}

// InitRemoteSkeleton creates ssh_configs/<name>/config.toml from template.
func InitRemoteSkeleton(dataDir, name string) error {
	if isInternalName(name) {
		return fmt.Errorf("name %q is reserved for the built-in virtual profile", name)
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	s := NewStore(dataDir)
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("config already exists: %s", p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, RemoteTemplate(), 0600)
}

// Rename moves a remote config directory to a new validated name.
func (s *Store) Rename(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if isInternalName(oldName) || isInternalName(newName) {
		return fmt.Errorf("cannot rename reserved ssh config %q", "internal")
	}
	oldPath, err := s.configPath(oldName)
	if err != nil {
		return err
	}
	newPath, err := s.configPath(newName)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	oldDir := filepath.Dir(oldPath)
	newDir := filepath.Dir(newPath)
	if st, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("ssh config %q not found", oldName)
		}
		return err
	} else if st.IsDir() {
		return fmt.Errorf("ssh config %q is not a file", oldName)
	}
	// Case-insensitive collision check (Windows/macOS).
	entries, err := os.ReadDir(s.root())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != oldName && strings.EqualFold(e.Name(), newName) {
			return fmt.Errorf("config already exists: %s", filepath.Join(s.root(), e.Name(), "config.toml"))
		}
	}
	return os.Rename(oldDir, newDir)
}

// Delete removes a remote config; the virtual internal profile cannot be deleted.
func (s *Store) Delete(name string) error {
	if isInternalName(name) {
		return fmt.Errorf("cannot delete reserved ssh config %q", name)
	}
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(dir)
	return nil
}
