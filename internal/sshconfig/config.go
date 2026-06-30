package sshconfig

import (
	"fmt"
	"github.com/BurntSushi/toml"
	"regexp"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
)

const maxConfigFileSize = 256 * 1024

const KindInternal = "internal"
const KindRemote = "remote"

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// DialSpec holds the dial-time SSH fields shared by a remote Entry and a JumpSpec.
// It is embedded with toml:",squash" so TOML keys stay flat (host = ..., not [dialspec]).
type DialSpec struct {
	Host               string `toml:"host,omitempty"`
	Port               int    `toml:"port,omitempty"`
	User               string `toml:"user,omitempty"`
	Password           string `toml:"password,omitempty"`
	PrivateKey         string `toml:"private_key,omitempty"`
	KeyPassphrase      string `toml:"key_passphrase,omitempty"`
	TrustUnknownHost   *bool  `toml:"trust_unknown_host,omitempty"`
	KnownHosts         string `toml:"known_hosts,omitempty"`
	DialTimeoutSeconds int    `toml:"dial_timeout_seconds,omitempty"`
	Proxy              string `toml:"proxy,omitempty"` // e.g. "socks5://user:pass@host:port"
}

// Entry is stored as data-dir/ssh_configs/<name>/config.toml
type Entry struct {
	Kind         string `toml:"kind"` // "internal" | "remote"
	Description  string `toml:"description,omitempty"`
	DefaultShell string `toml:"default_shell,omitempty"`
	DefaultMode  string `toml:"default_mode,omitempty"`
	DialSpec     `toml:",squash"`
	Jump         *JumpSpec `toml:"jump,omitempty"` // optional bastion chain (ProxyJump)
}

// JumpSpec is a self-contained bastion hop: a DialSpec plus a nested Jump for
// multi-hop chains. It lacks Entry's session-level fields (kind/shell/mode) —
// only the final target runs a shell. Stored inline so the config is
// self-contained at the cost of duplicated bastion credentials across entries.
type JumpSpec struct {
	DialSpec `toml:",squash"`
	Jump     *JumpSpec `toml:"jump,omitempty"`
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
		if err := validateDialSpec(&e.DialSpec, "remote config"); err != nil {
			return nil, err
		}
		if err := validateJump(e.Jump); err != nil {
			return nil, err
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

// validateDialSpec checks the shared dial fields. label prefixes error messages
// (e.g. "remote config", `jump "host"`).
func validateDialSpec(d *DialSpec, label string) error {
	d.Host = strings.TrimSpace(d.Host)
	d.User = strings.TrimSpace(d.User)
	if d.Host == "" {
		return fmt.Errorf("%s must set \"host\"", label)
	}
	if d.User == "" {
		return fmt.Errorf("%s must set \"user\"", label)
	}
	if strings.TrimSpace(d.Password) == "" && strings.TrimSpace(d.PrivateKey) == "" {
		return fmt.Errorf("%s needs password or private_key", label)
	}
	if d.Port < 0 || d.Port > 65535 {
		return fmt.Errorf("%s invalid port %d", label, d.Port)
	}
	if _, err := sshclient.ParseProxyURL(d.Proxy); err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	return nil
}

// validateJump recursively validates a bastion chain. nil = no jump.
func validateJump(j *JumpSpec) error {
	if j == nil {
		return nil
	}
	j.Host = strings.TrimSpace(j.Host)
	label := fmt.Sprintf("jump %q", j.Host)
	if err := validateDialSpec(&j.DialSpec, label); err != nil {
		return err
	}
	return validateJump(j.Jump)
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
	return []byte("# termcp SSH config (TOML)\n" +
		"kind = \"remote\"\n" +
		"description = \"Edit this file with real credentials (never commit).\"\n" +
		"host = \"ssh.example.com\"\n" +
		"port = 22\n" +
		"user = \"\"\n" +
		"# auth: password or private_key\n" +
		"password = \"\"\n" +
		"private_key = \"\"\"\n\"\"\"\n" +
		"key_passphrase = \"\"\n" +
		"trust_unknown_host = false\n" +
		"known_hosts = \"\"\n" +
		"dial_timeout_seconds = 30\n" +
		"\n# Optional: tunnel SSH through a SOCKS5 proxy.\n" +
		"# proxy = \"socks5://user:pass@127.0.0.1:1080\"\n" +
		"proxy = \"\"\n" +
		"\n# Optional: bastion / ProxyJump chain (self-contained, inline).\n" +
		"# [jump]\n" +
		"# host = \"bastion.example\"\n" +
		"# port = 22\n" +
		"# user = \"\"\n" +
		"# password = \"\"\n" +
		"# proxy = \"\"\n" +
		"# trust_unknown_host = true\n" +
		"# [jump.jump]   # deeper hop\n" +
		"# host = \"...\"\n")
}


// InternalTemplate is the built-in loopback SSH entry.
func InternalTemplate() []byte {
	return []byte("# termcp loopback SSH config (TOML)\nkind = \"internal\"\ndescription = \"Built-in termcp loopback SSH (no host credentials).\"\n")
}

// RemoteFromEntry converts an sshconfig Entry into session.RemoteSSH dial settings,
// including the recursive bastion chain from Entry.Jump.
func RemoteFromEntry(e *Entry, configDir string) (*session.RemoteSSH, error) {
	r := dialSpecToRemote(&e.DialSpec)
	r.Jump = jumpToRemote(e.Jump)
	return r, nil
}

func dialSpecToRemote(d *DialSpec) *session.RemoteSSH {
	pem := strings.TrimSpace(d.PrivateKey)
	trust := true
	if d.TrustUnknownHost != nil {
		trust = *d.TrustUnknownHost
	}
	port := d.Port
	if port == 0 {
		port = 22
	}
	proxy, _ := sshclient.ParseProxyURL(d.Proxy) // validated upstream by ParseAndValidate
	return &session.RemoteSSH{
		Host:               d.Host,
		Port:               port,
		User:               d.User,
		Password:           d.Password,
		PrivateKey:         pem,
		KeyPassphrase:      d.KeyPassphrase,
		TrustUnknownHost:   trust,
		KnownHosts:         d.KnownHosts,
		DialTimeoutSeconds: d.DialTimeoutSeconds,
		Proxy:              proxy,
	}
}

func jumpToRemote(j *JumpSpec) *session.RemoteSSH {
	if j == nil {
		return nil
	}
	r := dialSpecToRemote(&j.DialSpec)
	r.Jump = jumpToRemote(j.Jump)
	return r
}
