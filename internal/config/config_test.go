package config

import "testing"

func TestDefault_HostBindsAllInterfaces(t *testing.T) {
	cfg := Default()
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("expected Host 127.0.0.1, got %q", cfg.Host)
	}
	if cfg.Port != 18765 {
		t.Fatalf("expected Port 18765, got %d", cfg.Port)
	}
}

func TestDefault_HasInfoLevel(t *testing.T) {
	cfg := Default()
	if cfg.LogLevel != "info" {
		t.Fatalf("expected LogLevel \"info\", got %q", cfg.LogLevel)
	}
}

func TestValidate_Valid(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_InvalidPort(t *testing.T) {
	for _, port := range []int{-1, 0, 65536, 99999} {
		cfg := &Config{Host: "127.0.0.1", Port: port, DataDir: "/tmp/data"}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected error for port %d", port)
		}
	}
}

func TestValidate_EmptyDataDir(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: ""}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty DataDir")
	}
}

func TestValidate_RejectsUnknownLogLevel(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", LogLevel: "bogus"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for unknown LogLevel")
	}
}

func TestValidate_AcceptsAllValidLevels(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "error"} {
		cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", LogLevel: level}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected %s to validate, got: %v", level, err)
		}
	}
}

func TestValidate_AdminPortRequiresToken(t *testing.T) {
	cfg := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", AdminPort: 9090, AdminToken: "   "}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error when admin_port set with empty admin_token")
	}
	cfg2 := &Config{Host: "127.0.0.1", Port: 8080, DataDir: "/tmp/data", AdminPort: 9090, AdminToken: "secret"}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
