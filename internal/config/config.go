package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvDataDir overrides the default data directory when set.
const EnvDataDir = "TERMCP_DATA_DIR"

// Config holds all runtime configuration for the server.
type Config struct {
	Host                string // HTTP server bind address (default: "127.0.0.1" = loopback; use 0.0.0.0 for all interfaces)
	Port                int    // HTTP server port, must be 1-65535 (default: 18765)
	DataDir             string // persistent storage directory; empty means default ($TERMCP_DATA_DIR or ~/.termcp)
	LogLevel            string // log verbosity: debug|info|warn|error (default: "info")
	NoInternal          bool   // disable the built-in loopback SSH profile
	MCPManageSSHConfigs bool   // enable MCP tools for creating/editing/deleting SSH configs (default: false)
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Host:     "127.0.0.1",
		Port:     18765,
		DataDir:  "", // resolved to $TERMCP_DATA_DIR or ~/.termcp at startup
		LogLevel: "info",
	}
}

// DefaultDataDir resolves the default data directory: $TERMCP_DATA_DIR when
// set, otherwise ~/.termcp. A fixed per-user location keeps sessions and SSH
// configs in one place regardless of where the binary lives or runs from.
func DefaultDataDir() (string, error) {
	if env := strings.TrimSpace(os.Getenv(EnvDataDir)); env != "" {
		return filepath.Clean(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".termcp"), nil
}

// LegacyExeDataDir returns the previous default data dir (<dir of executable>/data).
// It exists only to migrate old installs to the new default.
func LegacyExeDataDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot locate executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "data"), nil
}

// Validate checks that all fields are within valid ranges.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
