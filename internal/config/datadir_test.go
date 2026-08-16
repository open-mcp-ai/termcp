package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultDataDir_EnvOverrideWins(t *testing.T) {
	t.Setenv(EnvDataDir, " /tmp/custom-data ")

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir() error: %v", err)
	}
	if want := filepath.Clean("/tmp/custom-data"); got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultDataDir_BlankEnvFallsBackToHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME env override is unreliable on windows")
	}
	t.Setenv(EnvDataDir, "   ")
	t.Setenv("HOME", "/tmp/fakehome")

	got, err := DefaultDataDir()
	if err != nil {
		t.Fatalf("DefaultDataDir() error: %v", err)
	}
	if want := filepath.Join("/tmp/fakehome", ".termcp"); got != want {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultDataDir_NoHomeErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME env override is unreliable on windows")
	}
	t.Setenv(EnvDataDir, "")
	t.Setenv("HOME", "")

	if _, err := DefaultDataDir(); err == nil {
		t.Fatal("expected error when home directory cannot be determined")
	}
}

func TestLegacyExeDataDir_IsExeDirPlusData(t *testing.T) {
	dir, err := LegacyExeDataDir()
	if err != nil {
		t.Fatalf("LegacyExeDataDir() error: %v", err)
	}
	if want := "data"; filepath.Base(dir) != want {
		t.Fatalf("LegacyExeDataDir() = %q, want base %q", dir, want)
	}
}
