package shell

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Detector detects the available shell on the target system.
// Fields are injectable for testing; use NewDetector() for production.
type Detector struct {
	LookPath func(string) (string, error)
	Getenv   func(string) string
	Stat     func(string) error
	GOOS     string
}

// NewDetector returns a Detector backed by real OS calls.
func NewDetector() *Detector {
	return &Detector{
		LookPath: exec.LookPath,
		Getenv:   os.Getenv,
		Stat: func(path string) error {
			_, err := os.Stat(path)
			return err
		},
		GOOS: runtime.GOOS,
	}
}

// Detect returns the shell path, family, and a hint for agents.
func (d *Detector) Detect() (path, family, hint string) {
	if d.GOOS == "windows" {
		return d.detectWindows()
	}
	return d.detectUnix()
}

func (d *Detector) detectWindows() (path, family, hint string) {
	for _, sh := range []string{"pwsh.exe", "powershell.exe", "cmd.exe"} {
		if p, err := d.LookPath(sh); err == nil {
			family := "powershell"
			if sh == "cmd.exe" {
				family = "cmd"
			}
			return p, family, fmt.Sprintf("Found: %s", sh)
		}
	}
	return "", "", "No shell found on Windows"
}

func (d *Detector) detectUnix() (path, family, hint string) {
	shell := d.Getenv("SHELL")
	if shell != "" {
		if p, err := d.LookPath(shell); err == nil {
			return p, "unix", fmt.Sprintf("$SHELL: %s", shell)
		}
	}
	for _, sh := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if err := d.Stat(sh); err == nil {
			return sh, "unix", fmt.Sprintf("Default: %s", sh)
		}
	}
	return "", "", "No shell found"
}
