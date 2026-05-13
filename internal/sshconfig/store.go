package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store manages dataDir/ssh_configs/<name>/config.json
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

func (s *Store) configPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.json"), nil
}

// EnsureInternal creates data-dir/ssh_configs/internal/config.json if missing.
func EnsureInternal(dataDir string) error {
	s := NewStore(dataDir)
	p, err := s.configPath("internal")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err == nil {
		if _, err := ParseAndValidate(data); err == nil {
			return nil
		}
		// corrupt or invalid template: overwrite
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(p, InternalTemplate(), 0600)
}

// Load reads and validates a named config.
func (s *Store) Load(name string) (*Entry, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ssh config %q not found (use data-dir/ssh_configs/%s/config.json or run the server binary: ssh-config init)", name, name)
		}
		return nil, err
	}
	return ParseAndValidate(data)
}

// ReadRaw returns the raw config.json bytes for a name (file must exist and parse as valid Entry).
func (s *Store) ReadRaw(name string) ([]byte, error) {
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

// List returns sorted config names that have config.json.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root(), 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		cfg := filepath.Join(s.root(), name, "config.json")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save writes config JSON for a name (validates first).
func (s *Store) Save(name string, data []byte) error {
	if _, err := ParseAndValidate(data); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	if strings.EqualFold(name, "internal") {
		ent, err := ParseAndValidate(data)
		if err != nil {
			return err
		}
		if ent.Kind != KindInternal {
			return fmt.Errorf("name %q is reserved for kind %q only", name, KindInternal)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := filepath.Join(s.root(), name, "config.json")
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

// InitRemoteSkeleton creates ssh_configs/<name>/config.json from template.
func InitRemoteSkeleton(dataDir, name string) error {
	if strings.EqualFold(name, "internal") {
		return fmt.Errorf("name %q is reserved; use the auto-created internal config", name)
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

// Delete removes a remote config; internal cannot be deleted.
func (s *Store) Delete(name string) error {
	if strings.EqualFold(name, "internal") {
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
