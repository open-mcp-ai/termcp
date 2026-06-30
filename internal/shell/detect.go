package shell

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
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
	path, _, hint = d.detectUnix()
	if path == "" {
		return "", "", hint
	}
	return path, "unix", hint
}

// Argv returns the default shell argv (path + optional args) using the same
// priority as Detect. Use this when spawning a shell, so that the actual
// process matches what Detect advertises to agents.
func (d *Detector) Argv() (path string, args []string) {
	if d.GOOS == "windows" {
		p, _, _ := d.detectWindows()
		if p == "" {
			return "cmd.exe", nil
		}
		return p, nil
	}
	p, args, _ := d.detectUnix()
	if p == "" {
		return "/bin/sh", nil
	}
	return p, args
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

func (d *Detector) detectUnix() (path string, args []string, hint string) {
	if shell := d.Getenv("SHELL"); shell != "" {
		if fields := strings.Fields(shell); len(fields) > 0 {
			if p, err := d.LookPath(fields[0]); err == nil {
				return p, fields[1:], fmt.Sprintf("$SHELL: %s", shell)
			}
		}
	}
	for _, sh := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if err := d.Stat(sh); err == nil {
			return sh, nil, fmt.Sprintf("Default: %s", sh)
		}
	}
	return "", nil, "No shell found"
}
