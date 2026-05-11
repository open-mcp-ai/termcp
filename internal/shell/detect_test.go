package shell

import (
	"errors"
	"testing"
)

func newFakeDetector(getenv func(string) string, lookPath func(string) (string, error), stat func(string) error, goos string) *Detector {
	return &Detector{
		Getenv:   getenv,
		LookPath: lookPath,
		Stat:     stat,
		GOOS:     goos,
	}
}

func TestDetect_UsesShellEnvVar(t *testing.T) {
	d := newFakeDetector(
		func(key string) string {
			if key == "SHELL" {
				return "/bin/zsh"
			}
			return ""
		},
		func(file string) (string, error) {
			if file == "/bin/zsh" {
				return file, nil
			}
			return "", errors.New("not found")
		},
		nil,
		"linux",
	)

	path, family, hint := d.Detect()
	if path != "/bin/zsh" {
		t.Fatalf("expected path=/bin/zsh, got %q", path)
	}
	if family != "unix" {
		t.Fatalf("expected family=unix, got %q", family)
	}
	if hint == "" {
		t.Fatal("expected non-empty hint")
	}
}

func TestDetect_UnixFallbackOrder(t *testing.T) {
	t.Run("falls back to /bin/zsh when SHELL empty", func(t *testing.T) {
		d := newFakeDetector(
			func(key string) string { return "" },
			nil,
			func(path string) error {
				if path == "/bin/zsh" {
					return nil
				}
				return errors.New("not found")
			},
			"linux",
		)
		path, family, _ := d.Detect()
		if path != "/bin/zsh" {
			t.Fatalf("expected /bin/zsh, got %q", path)
		}
		if family != "unix" {
			t.Fatalf("expected unix, got %q", family)
		}
	})

	t.Run("falls back to /bin/bash when zsh missing", func(t *testing.T) {
		d := newFakeDetector(
			func(key string) string { return "" },
			nil,
			func(path string) error {
				if path == "/bin/bash" {
					return nil
				}
				return errors.New("not found")
			},
			"linux",
		)
		path, family, _ := d.Detect()
		if path != "/bin/bash" {
			t.Fatalf("expected /bin/bash, got %q", path)
		}
		if family != "unix" {
			t.Fatalf("expected unix, got %q", family)
		}
	})

	t.Run("falls back to /bin/sh as last resort", func(t *testing.T) {
		d := newFakeDetector(
			func(key string) string { return "" },
			nil,
			func(path string) error {
				if path == "/bin/sh" {
					return nil
				}
				return errors.New("not found")
			},
			"linux",
		)
		path, family, _ := d.Detect()
		if path != "/bin/sh" {
			t.Fatalf("expected /bin/sh, got %q", path)
		}
		if family != "unix" {
			t.Fatalf("expected unix, got %q", family)
		}
	})
}

func TestDetect_WindowsFallbackOrder(t *testing.T) {
	t.Run("prefers pwsh.exe", func(t *testing.T) {
		d := newFakeDetector(
			nil,
			func(file string) (string, error) {
				if file == "pwsh.exe" {
					return "C:\\Program Files\\PowerShell\\pwsh.exe", nil
				}
				return "", errors.New("not found")
			},
			nil,
			"windows",
		)
		path, family, _ := d.Detect()
		if path != "C:\\Program Files\\PowerShell\\pwsh.exe" {
			t.Fatalf("expected pwsh.exe path, got %q", path)
		}
		if family != "powershell" {
			t.Fatalf("expected powershell family, got %q", family)
		}
	})

	t.Run("falls back to powershell.exe", func(t *testing.T) {
		d := newFakeDetector(
			nil,
			func(file string) (string, error) {
				if file == "powershell.exe" {
					return "C:\\Windows\\System32\\WindowsPowerShell\\powershell.exe", nil
				}
				return "", errors.New("not found")
			},
			nil,
			"windows",
		)
		path, family, _ := d.Detect()
		if path != "C:\\Windows\\System32\\WindowsPowerShell\\powershell.exe" {
			t.Fatalf("expected powershell.exe path, got %q", path)
		}
		if family != "powershell" {
			t.Fatalf("expected powershell family, got %q", family)
		}
	})

	t.Run("falls back to cmd.exe", func(t *testing.T) {
		d := newFakeDetector(
			nil,
			func(file string) (string, error) {
				if file == "cmd.exe" {
					return "C:\\Windows\\System32\\cmd.exe", nil
				}
				return "", errors.New("not found")
			},
			nil,
			"windows",
		)
		path, family, _ := d.Detect()
		if path != "C:\\Windows\\System32\\cmd.exe" {
			t.Fatalf("expected cmd.exe path, got %q", path)
		}
		if family != "cmd" {
			t.Fatalf("expected cmd family, got %q", family)
		}
	})
}

func TestDetect_NoShellFound(t *testing.T) {
	d := newFakeDetector(
		func(key string) string { return "" },
		func(file string) (string, error) { return "", errors.New("not found") },
		func(path string) error { return errors.New("not found") },
		"linux",
	)

	path, family, hint := d.Detect()
	if path != "" {
		t.Fatalf("expected empty path, got %q", path)
	}
	if family != "" {
		t.Fatalf("expected empty family, got %q", family)
	}
	if hint == "" {
		t.Fatal("expected non-empty error hint")
	}
}
