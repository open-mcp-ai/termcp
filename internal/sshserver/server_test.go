package sshserver

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "/bin/bash"
}

// ptyShellLine returns the shell command line for PTY tests (suppress Windows profile / slow startup).
func ptyShellLine() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe -NoLogo -NoProfile"
	}
	return testShell()
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func testShellEchoCommand(s string) string {
	if runtime.GOOS == "windows" {
		return "powershell.exe -NoProfile -Command Write-Output " + s
	}
	return "echo " + s
}

func mintCfg(t *testing.T, srv *Server) *ssh.ClientConfig {
	t.Helper()
	c, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// dialServer creates an in-memory SSH client connected to the test server.
func dialServer(t *testing.T, srv *Server, config *ssh.ClientConfig) *ssh.Client {
	t.Helper()
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, "inmem", config)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return ssh.NewClient(c, chans, reqs)
}

func TestMint_OneTimePassword(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	c1 := dialServer(t, srv, cfg)
	_ = c1.Close()

	// Second dial with same one-time config should fail at SSH handshake.
	conn2, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = ssh.NewClientConn(conn2, "inmem", cfg)
	if err == nil {
		t.Fatal("expected second dial with same one-time config to fail")
	}
	// ssh.NewClientConn closes conn2 on handshake failure, so no explicit Close needed.
}

func TestServer_StartAndStop(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	// Verify we can connect
	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	client.Close()

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestServer_PipeSession(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	out, err := session.Output(testShellEchoCommand("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(string(out), "\r\n") != "hello" {
		t.Fatalf("expected hello output, got %q", string(out))
	}
}

func TestServer_SignalTerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.Start("sleep 30"); err != nil {
		t.Fatal(err)
	}

	// Give the process time to start
	time.Sleep(200 * time.Millisecond)

	if err := session.Signal(ssh.SIGTERM); err != nil {
		t.Fatalf("signal failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from SIGTERM")
		}
		if exitErr, ok := err.(*ssh.ExitError); ok {
			// SIGTERM → exit 143 (128+15) on Unix
			if exitErr.ExitStatus() == 0 {
				t.Fatal("expected non-zero exit status from SIGTERM")
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for signal kill")
	}
}

func TestServer_SignalInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signals not supported on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if err := session.Start("sleep 30"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)

	if err := session.Signal(ssh.SIGINT); err != nil {
		t.Fatalf("signal failed: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Wait() }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from SIGINT")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for signal interrupt")
	}
}

func TestServer_ProcessStateNil(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	// Run a nonexistent command — ProcessState will be nil
	out, _ := session.CombinedOutput("this_command_does_not_exist_12345")
	_ = out // server should not panic
}

func TestServer_PtySession(t *testing.T) {
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm", 24, 80, modes); err != nil {
		t.Fatal(err)
	}

	stdin, _ := session.StdinPipe()
	stdout, _ := session.StdoutPipe()

	if err := session.Start(ptyShellLine()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	stdin.Write([]byte(testShellInput("echo test_pty")))

	// Read in a loop until we see "test_pty" or timeout
	deadline := time.Now().Add(5 * time.Second)
	var allOutput string
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "test_pty") {
		n, _ := stdout.Read(buf)
		allOutput += string(buf[:n])
	}
	if !strings.Contains(allOutput, "test_pty") {
		t.Fatalf("expected output containing 'test_pty', got %q", allOutput)
	}

	stdin.Write([]byte(testShellInput("exit")))
	session.Wait()
}

func TestServer_PtyEnviron(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("TERM env var is a Unix concept, not meaningful on Windows")
	}
	srv := New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := mintCfg(t, srv)
	client := dialServer(t, srv, config)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	modes := ssh.TerminalModes{
		ssh.ECHO:          0,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", 24, 80, modes); err != nil {
		t.Fatal(err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := session.Start(ptyShellLine()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	stdin.Write([]byte(testShellInput("echo TERM=$TERM")))

	deadline := time.Now().Add(5 * time.Second)
	var allOutput string
	buf := make([]byte, 4096)
	for time.Now().Before(deadline) && !strings.Contains(allOutput, "TERM=") {
		n, _ := stdout.Read(buf)
		allOutput += string(buf[:n])
	}

	if !strings.Contains(allOutput, "TERM=xterm-256color") {
		t.Fatalf("expected TERM=xterm-256color in output, got %q", allOutput)
	}

	stdin.Write([]byte(testShellInput("exit")))
	session.Wait()
}
