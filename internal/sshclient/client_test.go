package sshclient

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"golang.org/x/crypto/ssh"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func ptyShellCmd() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile"}
	}
	return testShell(), nil
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func pipeEchoCommand() (string, []string, string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "$input | Write-Output"}, "hello\r\n"
	}
	return "cat", nil, "hello\n"
}

func longRunningCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds 60"}
	}
	return "sleep", []string{"60"}
}

func failingCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "exit 1"}
	}
	return "false", nil
}

func startTestSSHServer(t *testing.T) (*sshserver.Server, string) {
	t.Helper()
	srv := sshserver.New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv, srv.Addr()
}

func mintClientConfig(t *testing.T, srv *sshserver.Server) *ssh.ClientConfig {
	t.Helper()
	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStart_PipeMode(t *testing.T) {
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	command, args, input := pipeEchoCommand()
	es, err := StartWithConfig(addr, cfg, command, args, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	es.Stdin.Write([]byte(input))
	es.Stdin.Close()

	<-es.Done()

	if es.ExitCode() != 0 {
		t.Fatalf("expected exit code 0, got %d", es.ExitCode())
	}
}

func TestStart_PtyMode(t *testing.T) {
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	sh, shArgs := ptyShellCmd()
	es, err := StartWithConfig(addr, cfg, sh, shArgs, true, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	es.Stdin.Write([]byte(testShellInput("echo pty_test")))

	// Loop-read until we see the expected output
	buf := make([]byte, 4096)
	var allOutput string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "pty_test") {
		n, _ := es.Stdout.Read(buf)
		allOutput += string(buf[:n])
	}
	if !strings.Contains(allOutput, "pty_test") {
		t.Fatalf("expected output containing 'pty_test', got %q", allOutput)
	}

	es.Stdin.Write([]byte(testShellInput("exit")))
	<-es.Done()
}

func TestStart_ResizePty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("interactive PTY tests not stable on Windows ConPTY")
	}
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	sh, shArgs := ptyShellCmd()
	es, err := StartWithConfig(addr, cfg, sh, shArgs, true, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	if err := es.ResizePty(50, 120); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}

	es.Stdin.Write([]byte(testShellInput("exit")))
	<-es.Done()
}

func TestStart_Signal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	command, args := longRunningCommand()
	es, err := StartWithConfig(addr, cfg, command, args, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond) // let server process start
	if err := es.Signal("TERM"); err != nil {
		t.Fatalf("Signal failed: %v", err)
	}
	// Close stdin to unblock server's stdin copy goroutine in pipe mode
	es.Stdin.Close()

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after signal")
	}
}

func TestStart_Close(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("session close is reported as EOF on Windows ConPTY")
	}
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	command, args := longRunningCommand()
	es, err := StartWithConfig(addr, cfg, command, args, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	if err := es.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit after close")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		cmd  string
		args []string
		want string
	}{
		{"echo", []string{"hello"}, `echo hello`},
		{"echo hello", nil, `"echo hello"`},
		{"echo", []string{"a b"}, `echo "a b"`},
		{"echo", []string{`a"b`}, `echo "a\"b"`},
		{"my command", []string{"arg"}, `"my command" arg`},
	}
	for _, tc := range tests {
		got := shellQuote(tc.cmd, tc.args)
		if got != tc.want {
			t.Fatalf("shellQuote(%q, %v) = %q, want %q", tc.cmd, tc.args, got, tc.want)
		}
	}
}

func TestStart_CommandFailure(t *testing.T) {
	srv, addr := startTestSSHServer(t)
	cfg := mintClientConfig(t, srv)

	command, args := failingCommand()
	es, err := StartWithConfig(addr, cfg, command, args, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	// Close stdin to unblock server's stdin copy goroutine in pipe mode
	es.Stdin.Close()

	select {
	case <-es.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	if es.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d", es.ExitCode())
	}
}

func TestDefaultPTYModes(t *testing.T) {
	modes := defaultPTYModes()

	required := map[string]uint8{
		"ECHO":   ssh.ECHO,
		"ICRNL":  ssh.ICRNL,
		"ONLCR":  ssh.ONLCR,
		"OPOST":  ssh.OPOST,
		"ISIG":   ssh.ISIG,
		"ICANON": ssh.ICANON,
	}

	for name, opcode := range required {
		v, ok := modes[opcode]
		if !ok {
			t.Errorf("missing required terminal mode %s (opcode %d)", name, opcode)
			continue
		}
		if v != 1 {
			t.Errorf("terminal mode %s = %d, want 1 (enabled)", name, v)
		}
	}
}
