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

func TestServer_StartAndStop(t *testing.T) {
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr()
	if addr == "" {
		t.Fatal("expected non-empty address after start")
	}

	// Verify we can connect
	config := ClientConfig()
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	client.Close()

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestServer_PipeSession(t *testing.T) {
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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

func TestServer_SignalTerm(t *testing.T) {
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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
	srv := New("127.0.0.1:0")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()

	config := ClientConfig()
	client, err := ssh.Dial("tcp", srv.Addr(), config)
	if err != nil {
		t.Fatal(err)
	}
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

	if err := session.Start(testShell()); err != nil {
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
