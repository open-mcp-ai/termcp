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
	// DefaultShell: when the client omits command+args, split with strings.Fields and use as argv (optional).
	DefaultShell string `json:"default_shell,omitempty"`
	// DefaultMode: when the client omits mode, use "pty" or "pipe" (optional; empty = pty).
	DefaultMode string `json:"default_mode,omitempty"`
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
		if err := validateDefaultMode(&e); err != nil {
			return nil, err
		}
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
		if err := validateDefaultMode(&e); err != nil {
			return nil, err
		}
		return &e, nil
	default:
		return nil, fmt.Errorf("unknown kind %q (use %q or %q)", e.Kind, KindInternal, KindRemote)
	}
}

func validateDefaultMode(e *Entry) error {
	dm := strings.TrimSpace(strings.ToLower(e.DefaultMode))
	if dm == "" {
		e.DefaultMode = ""
		return nil
	}
	if dm != "pty" && dm != "pipe" {
		return fmt.Errorf("default_mode must be \"pty\" or \"pipe\"")
	}
	e.DefaultMode = dm
	return nil
}

// EffectiveCommand resolves command+args when the client sends nothing.
// If the client provides a non-empty command or any args, ent is ignored for the command.
func EffectiveCommand(ent *Entry, cmd string, args []string) (string, []string) {
	cmd = strings.TrimSpace(cmd)
	if cmd != "" || len(args) > 0 {
		return cmd, args
	}
	if ent != nil {
		ds := strings.TrimSpace(ent.DefaultShell)
		if ds != "" {
			f := strings.Fields(ds)
			if len(f) > 0 {
				return f[0], f[1:]
			}
		}
	}
	return "", nil
}

// EffectiveMode returns pty or pipe; default is pty unless ent.DefaultMode overrides when mode is empty.
func EffectiveMode(ent *Entry, mode string) string {
	m := strings.TrimSpace(strings.ToLower(mode))
	if m != "" {
		return m
	}
	if ent != nil {
		dm := strings.TrimSpace(strings.ToLower(ent.DefaultMode))
		if dm == "pty" || dm == "pipe" {
			return dm
		}
	}
	return "pty"
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
