package config

import (
	"fmt"
)

// Config holds all runtime configuration for the server.
type Config struct {
	Host             string // HTTP server bind address (default: "127.0.0.1" = loopback; use 0.0.0.0 for all interfaces)
	Port             int    // HTTP server port, must be 1-65535 (default: 18765)
	DataDir          string // persistent storage directory; empty means default "<exe_dir>/data"
	LogLevel         string // log verbosity: debug|info|warn|error (default: "info")
	NoInternal       bool   // disable the built-in loopback SSH profile
	MCPManageSSHConfigs bool // enable MCP tools for creating/editing/deleting SSH configs (default: false)
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Host:     "127.0.0.1",
		Port:     18765,
		DataDir:  "", // resolved to <exe_dir>/data at startup
		LogLevel: "info",
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
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
