package config

import "fmt"

// Config holds all runtime configuration for the server.
type Config struct {
	Host      string // HTTP server bind address (default: "127.0.0.1")
	Port      int    // HTTP server port, must be 1-65535 (default: 8080)
	DataDir   string // persistent storage directory, must be non-empty (default: "./data")
	SSHHost   string // SSH server bind address (default: "127.0.0.1")
	SSHPort   int    // SSH server port, 0 = random (default: 0)
	LogLevel  string // log verbosity: debug|info|warn|error (default: "info")
	LogFormat string // log output format: text|json (default: "text")
}

// Default returns a Config with sensible defaults.
func Default() *Config {
	return &Config{
		Host:      "127.0.0.1",
		Port:      8080,
		DataDir:   "./data",
		SSHHost:   "127.0.0.1",
		SSHPort:   0,
		LogLevel:  "info",
		LogFormat: "text",
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
	switch c.LogFormat {
	case "", "text", "json":
	default:
		return fmt.Errorf("log_format must be one of text|json, got %q", c.LogFormat)
	}
	return nil
}
