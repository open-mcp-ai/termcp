package config

import (
	"fmt"
	"strings"
)

// Config holds all runtime configuration for the server.
type Config struct {
	Host       string // HTTP server bind address (default: "127.0.0.1" = loopback; use 0.0.0.0 for all interfaces)
	Port       int    // HTTP server port, must be 1-65535 (default: 18765)
	DataDir    string // persistent storage directory, must be non-empty (default: "./data")
	SSHHost    string // SSH server bind address (default: "127.0.0.1")
	SSHPort    int    // SSH server port, 0 = random (default: 0)
	LogLevel   string // log verbosity: debug|info|warn|error (default: "info")
	AdminHost  string // admin HTTP API bind (default: "127.0.0.1")
	AdminPort  int    // admin HTTP port; 0 = disabled (default: 0)
	AdminToken string // bearer / X-Admin-Token for PUT/GET/DELETE /api/ssh-configs; required when AdminPort != 0
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Host:      "127.0.0.1",
		Port:      18765,
		DataDir:   "./data",
		SSHHost:   "127.0.0.1",
		SSHPort:   0,
		LogLevel:  "info",
		AdminHost: "127.0.0.1",
		AdminPort: 0,
	}
}

// Validate checks that all fields are within valid ranges.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	if c.AdminPort < 0 || c.AdminPort > 65535 {
		return fmt.Errorf("admin_port must be between 0 and 65535, got %d", c.AdminPort)
	}
	if c.AdminPort != 0 && strings.TrimSpace(c.AdminToken) == "" {
		return fmt.Errorf("admin_token is required when admin_port is non-zero")
	}
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
