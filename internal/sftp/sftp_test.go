package sftp

import (
	"testing"
)

func TestNewClient_NilSSHClient(t *testing.T) {
	cli, err := NewClient(nil)
	if err == nil {
		t.Fatal("expected error when passing nil ssh.Client, got nil")
	}
	if cli != nil {
		t.Fatalf("expected nil Client, got %+v", cli)
	}
}
