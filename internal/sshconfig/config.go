package sshconfig

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const maxConfigFileSize = 256 * 1024

const KindInternal = "internal"
const KindRemote = "remote"

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Entry is stored as data-dir/ssh_configs/<name>/config.json
type Entry struct {
	Kind               string `json:"kind"` // "internal" | "remote"
	Host               string `json:"host,omitempty"`
	Port               int    `json:"port,omitempty"`
	User               string `json:"user,omitempty"`
	Password           string `json:"password,omitempty"`
	PrivateKeyPEM      string `json:"private_key_pem,omitempty"`
	PrivateKeyFile     string `json:"private_key_file,omitempty"`
	KeyPassphrase      string `json:"key_passphrase,omitempty"`
	TrustUnknownHost   *bool  `json:"trust_unknown_host,omitempty"`
	KnownHosts         string `json:"known_hosts,omitempty"`
	DialTimeoutSeconds int    `json:"dial_timeout_seconds,omitempty"`
	Description        string `json:"description,omitempty"`
}

// ValidateName checks directory/config id (reserved: internal is allowed as id).
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid ssh config name %q (letters, digits, _, -; max 64)", name)
	}
	return nil
}

// ParseAndValidate decodes JSON and validates by kind.
func ParseAndValidate(data []byte) (*Entry, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file too large (max %d bytes)", maxConfigFileSize)
	}
	var e Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("config json: %w", err)
	}
	k := strings.TrimSpace(strings.ToLower(e.Kind))
	if k == "" {
		return nil, fmt.Errorf("config must set \"kind\" to %q or %q", KindInternal, KindRemote)
	}
	e.Kind = k
	switch k {
	case KindInternal:
		return &e, nil
	case KindRemote:
		e.Host = strings.TrimSpace(e.Host)
		e.User = strings.TrimSpace(e.User)
		if e.Host == "" {
			return nil, fmt.Errorf("remote config must set \"host\"")
		}
		if e.User == "" {
			return nil, fmt.Errorf("remote config must set \"user\"")
		}
		if strings.TrimSpace(e.Password) == "" && strings.TrimSpace(e.PrivateKeyPEM) == "" && strings.TrimSpace(e.PrivateKeyFile) == "" {
			return nil, fmt.Errorf("remote config needs password, private_key_pem, or private_key_file")
		}
		if e.Port < 0 || e.Port > 65535 {
			return nil, fmt.Errorf("invalid port %d", e.Port)
		}
		return &e, nil
	default:
		return nil, fmt.Errorf("unknown kind %q (use %q or %q)", e.Kind, KindInternal, KindRemote)
	}
}

// RemoteTemplate returns default JSON for a new remote config directory.
func RemoteTemplate() []byte {
	return []byte(`{
  "kind": "remote",
  "description": "Edit this file with real credentials (never commit).",
  "host": "ssh.example.com",
  "port": 22,
  "user": "you",
  "password": "",
  "private_key_pem": "",
  "private_key_file": "",
  "key_passphrase": "",
  "trust_unknown_host": true,
  "known_hosts": "",
  "dial_timeout_seconds": 30
}
`)
}

// InternalTemplate is the built-in loopback SSH entry.
func InternalTemplate() []byte {
	return []byte(`{
  "kind": "internal",
  "description": "Built-in termcp loopback SSH (no host credentials)."
}
`)
}
