package session

import (
	"encoding/base64"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func sftpTestShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func sftpTestShellArgs() []string {
	if runtime.GOOS == "windows" {
		return []string{"-NoLogo", "-NoProfile"}
	}
	return nil
}

func sftpTestInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func joinRemotePath(dir, name string) string {
	return strings.TrimRight(strings.ReplaceAll(dir, "\\", "/"), "/") + "/" + name
}

func TestSFTP_UploadDownloadRoundTrip(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	tmpDir := t.TempDir()
	remotePath := joinRemotePath(tmpDir, "test.txt")
	content := "hello sftp world"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	n, err := s.UploadFile(encoded, remotePath)
	if err != nil {
		t.Fatalf("UploadFile failed: %v", err)
	}
	if n != len(content) {
		t.Fatalf("expected %d bytes written, got %d", len(content), n)
	}

	result, err := s.DownloadFile(remotePath)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	if result.Encoding != "text" {
		t.Fatalf("expected encoding 'text', got %q", result.Encoding)
	}
	if result.Content != content {
		t.Fatalf("expected content %q, got %q", content, result.Content)
	}
}

func TestSFTP_ListFiles(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	tmpDir := t.TempDir()
	encoded := base64.StdEncoding.EncodeToString([]byte("list-test"))
	remotePath := joinRemotePath(tmpDir, "listed.txt")
	if _, err := s.UploadFile(encoded, remotePath); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListFiles(tmpDir)
	if err != nil {
		t.Fatalf("ListFiles failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Name != "listed.txt" {
		t.Fatalf("expected name 'listed.txt', got %q", entries[0].Name)
	}
	if entries[0].IsDir {
		t.Fatal("expected file, not directory")
	}
}

func TestSFTP_BinaryDownload(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	tmpDir := t.TempDir()
	remotePath := joinRemotePath(tmpDir, "binary.bin")

	// Write binary file with null byte directly via local FS (simulating pre-existing file)
	if err := os.WriteFile(remotePath, []byte{0x00, 0x01, 0x02, 0xFF}, 0644); err != nil {
		t.Fatal(err)
	}

	result, err := s.DownloadFile(remotePath)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	if result.Encoding != "base64" {
		t.Fatalf("expected encoding 'base64', got %q", result.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(result.Content)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if len(decoded) != 4 {
		t.Fatalf("expected 4 bytes, got %d", len(decoded))
	}
}

func TestSFTP_UploadTooLarge(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	bigData := make([]byte, maxFileSize+1)
	encoded := base64.StdEncoding.EncodeToString(bigData)

	remotePath := joinRemotePath(t.TempDir(), "too-large.bin")
	_, err = s.UploadFile(encoded, remotePath)
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
}

func TestSFTP_DownloadNonExistent(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	_, err = s.DownloadFile(joinRemotePath(t.TempDir(), "no-such-file-12345"))
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestSFTP_PostExitDownload(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: sftpTestShell(), Args: sftpTestShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	remotePath := joinRemotePath(tmpDir, "result.txt")
	encoded := base64.StdEncoding.EncodeToString([]byte("post-exit-data"))
	if _, err := s.UploadFile(encoded, remotePath); err != nil {
		t.Fatal(err)
	}

	s.SendInput(sftpTestInput("exit"), false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Info().Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	result, err := s.DownloadFile(remotePath)
	if err != nil {
		t.Fatalf("post-exit download failed: %v", err)
	}
	if result.Content != "post-exit-data" {
		t.Fatalf("expected 'post-exit-data', got %q", result.Content)
	}
}
