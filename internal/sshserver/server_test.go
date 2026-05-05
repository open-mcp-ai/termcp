package sshserver

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

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

	out, err := session.Output("echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "hello\n" {
		t.Fatalf("expected 'hello\\n', got %q", string(out))
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

	if err := session.Start("/bin/bash"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	stdin.Write([]byte("echo test_pty\n"))

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

	stdin.Write([]byte("exit\n"))
	session.Wait()
}
