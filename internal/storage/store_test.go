package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestStore_SaveLoadSessions(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	sessions := []api.Session{
		{ID: "abc123", Command: "bash", Mode: api.ModePTY, Status: api.SessionRunning},
		{ID: "def456", Command: "cat", Mode: api.ModePipe, Status: api.SessionExited},
	}

	if err := s.SaveSessions(sessions); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(loaded))
	}
	if loaded[0].ID != "abc123" {
		t.Fatalf("expected first session ID 'abc123', got %q", loaded[0].ID)
	}
}

func TestStore_LoadSessionsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	loaded, err := s.LoadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(loaded))
	}
}

func TestStore_SaveLoadMessageIndex(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	entries := []api.MessageIndexEntry{
		{ID: "msg001", Type: api.MsgInput, ByteSize: 10},
		{ID: "msg002", Type: api.MsgOutput, ByteSize: 50},
	}

	if err := s.SaveMessageIndex("sess1", entries); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadMessageIndex("sess1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(loaded))
	}
	if loaded[0].ID != "msg001" {
		t.Fatalf("expected first entry ID 'msg001', got %q", loaded[0].ID)
	}
}

func TestStore_SaveLoadMessage(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	msg := api.Message{
		ID:        "msg001",
		SessionID: "sess1",
		Type:      api.MsgInput,
		Content:   "hello world",
	}

	if err := s.SaveMessage("sess1", msg); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadMessage("sess1", "msg001")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Content != "hello world" {
		t.Fatalf("expected 'hello world', got %q", loaded.Content)
	}
}

func TestStore_LoadMessageNotFound(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	_, err := s.LoadMessage("sess1", "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent message")
	}
}

func TestStore_CreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	// Saving a message should create nested directories
	msg := api.Message{ID: "m1", SessionID: "s1", Type: api.MsgOutput, Content: "x"}
	if err := s.SaveMessage("s1", msg); err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(dir, "messages", "s1", "messages", "m1.json")
	if _, err := os.Stat(expected); os.IsNotExist(err) {
		t.Fatalf("expected file %q to exist", expected)
	}
}

func TestStore_PathTraversalSessionID(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	entries := []api.MessageIndexEntry{{ID: "m1", Type: api.MsgOutput, ByteSize: 1}}

	traversalIDs := []string{
		"../../etc",
		"../..",
		"sess/../../etc",
		"sess\x00ion",
		"",
	}
	for _, id := range traversalIDs {
		err := s.SaveMessageIndex(id, entries)
		if err == nil {
			t.Fatalf("expected error for sessionID %q, got nil", id)
		}
	}
}

func TestStore_PathTraversalLoadMessage(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	_, err := s.LoadMessage("../../etc/passwd", "m1")
	if err == nil {
		t.Fatal("expected error for traversal sessionID")
	}

	_, err = s.LoadMessage("s1", "../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for traversal msgID")
	}
}

func TestStore_PathTraversalSaveMessage(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	msg := api.Message{ID: "../../evil", SessionID: "s1", Type: api.MsgOutput, Content: "x"}
	err := s.SaveMessage("s1", msg)
	if err == nil {
		t.Fatal("expected error for traversal msg.ID")
	}

	msg2 := api.Message{ID: "m1", SessionID: "s1", Type: api.MsgOutput, Content: "x"}
	err = s.SaveMessage("../../etc", msg2)
	if err == nil {
		t.Fatal("expected error for traversal sessionID")
	}
}

func TestStore_PathTraversalLoadMessageIndex(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)

	_, err := s.LoadMessageIndex("../../etc")
	if err == nil {
		t.Fatal("expected error for traversal sessionID")
	}
}
