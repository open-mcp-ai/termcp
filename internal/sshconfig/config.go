package sshconfig

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"regexp"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
)

const maxConfigFileSize = 256 * 1024

const KindInternal = "internal"
const KindRemote = "remote"

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Entry is stored as data-dir/ssh_configs/<name>/config.toml
type Entry struct {
	Kind               string `toml:"kind"` // "internal" | "remote"
	Host               string `toml:"host,omitempty"`
	Port               int    `toml:"port,omitempty"`
	User               string `toml:"user,omitempty"`
	Password           string `toml:"password,omitempty"`
	PrivateKey string `toml:"private_key,omitempty"`
	KeyPassphrase      string `toml:"key_passphrase,omitempty"`
	TrustUnknownHost   *bool  `toml:"trust_unknown_host,omitempty"`
	KnownHosts         string `toml:"known_hosts,omitempty"`
	DialTimeoutSeconds int    `toml:"dial_timeout_seconds,omitempty"`
	Description        string `toml:"description,omitempty"`
	DefaultShell       string `toml:"default_shell,omitempty"`
	DefaultMode        string `toml:"default_mode,omitempty"`
}

// ValidateName checks directory/config id (reserved: internal is allowed as id).
func ValidateName(name string) error {
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid ssh config name %q (letters, digits, _, -; max 64)", name)
	}
	return nil
}

// ParseAndValidate decodes TOML and validates by kind.
func ParseAndValidate(data []byte) (*Entry, error) {
	if len(data) > maxConfigFileSize {
		return nil, fmt.Errorf("config file too large (max %d bytes)", maxConfigFileSize)
	}
	var e Entry
	if err := toml.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("config toml: %w", err)
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
		if strings.TrimSpace(e.Password) == "" && strings.TrimSpace(e.PrivateKey) == "" {
			return nil, fmt.Errorf("remote config needs password or private_key")
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

// RemoteTemplate returns default TOML for a new remote config directory.
func RemoteTemplate() []byte {
	return []byte("# termcp SSH config (TOML)\nkind = \"remote\"\ndescription = \"Edit this file with real credentials (never commit).\"\nhost = \"ssh.example.com\"\nport = 22\nuser = \"\"\n# auth: password or private_key\npassword = \"\"\nprivate_key = \"\"\"\n\"\"\"\nkey_passphrase = \"\"\ntrust_unknown_host = false\nknown_hosts = \"\"\ndial_timeout_seconds = 30\n")
}


// InternalTemplate is the built-in loopback SSH entry.
func InternalTemplate() []byte {
	return []byte("# termcp loopback SSH config (TOML)\nkind = \"internal\"\ndescription = \"Built-in termcp loopback SSH (no host credentials).\"\n")
}

// RemoteFromEntry converts an sshconfig Entry into session.RemoteSSH dial settings.
func RemoteFromEntry(e *Entry, configDir string) (*session.RemoteSSH, error) {
	pem := strings.TrimSpace(e.PrivateKey)
	trust := true
	if e.TrustUnknownHost != nil {
		trust = *e.TrustUnknownHost
	}
	port := e.Port
	if port == 0 {
		port = 22
	}
	return &session.RemoteSSH{
		Host:               e.Host,
		Port:               port,
		User:               e.User,
		Password:           e.Password,
		PrivateKey:      pem,
		KeyPassphrase:      e.KeyPassphrase,
		TrustUnknownHost:   trust,
		KnownHosts:         e.KnownHosts,
		DialTimeoutSeconds: e.DialTimeoutSeconds,
	}, nil
}
