package sshclient

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
)

func startTestSSHServer(t *testing.T) (*sshserver.Server, string) {
	t.Helper()
	srv := sshserver.New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv, srv.Addr()
}

func TestStart_PipeMode(t *testing.T) {
	_, addr := startTestSSHServer(t)

	// Use cat so server's stdin copy goroutine unblocks when we close stdin
	es, err := Start(addr, "cat", nil, false, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	es.Stdin.Write([]byte("hello\n"))
	es.Stdin.Close()

	<-es.Done()

	if es.ExitCode() != 0 {
		t.Fatalf("expected exit code 0, got %d", es.ExitCode())
	}
}

func TestStart_PtyMode(t *testing.T) {
	_, addr := startTestSSHServer(t)

	es, err := Start(addr, "bash", nil, true, 24, 80)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	es.Stdin.Write([]byte("echo pty_test\n"))

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

	es.Stdin.Write([]byte("exit\n"))
	<-es.Done()
}

func TestStart_ResizePty(t *testing.T) {
	_, addr := startTestSSHServer(t)

	es, err := Start(addr, "bash", nil, true, 24, 80)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	if err := es.ResizePty(50, 120); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}

	es.Stdin.Write([]byte("exit\n"))
	<-es.Done()
}

func TestStart_Signal(t *testing.T) {
	_, addr := startTestSSHServer(t)

	es, err := Start(addr, "sleep", []string{"60"}, false, 24, 80)
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
	_, addr := startTestSSHServer(t)

	es, err := Start(addr, "sleep", []string{"60"}, false, 24, 80)
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
	_, addr := startTestSSHServer(t)

	es, err := Start(addr, "false", nil, false, 24, 80)
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
