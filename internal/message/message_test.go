package message

import (
	"fmt"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestManager_AppendAndList(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	mgr := NewManager(store)

	_, err := mgr.Append("s1", api.MsgInput, "hello")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mgr.Append("s1", api.MsgOutput, "world")
	if err != nil {
		t.Fatal(err)
	}

	entries, err := mgr.List("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Type != api.MsgInput {
		t.Fatalf("expected first entry type 'input', got %q", entries[0].Type)
	}
	if entries[1].Type != api.MsgOutput {
		t.Fatalf("expected second entry type 'output', got %q", entries[1].Type)
	}
}

func TestManager_Get(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	mgr := NewManager(store)

	msg, err := mgr.Append("s1", api.MsgSystem, "Process started")
	if err != nil {
		t.Fatal(err)
	}

	got, err := mgr.Get("s1", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "Process started" {
		t.Fatalf("expected 'Process started', got %q", got.Content)
	}
}

func TestManager_GetMany(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	mgr := NewManager(store)

	msg1, _ := mgr.Append("s1", api.MsgInput, "a")
	msg2, _ := mgr.Append("s1", api.MsgOutput, "b")
	mgr.Append("s1", api.MsgSystem, "c") // not fetched

	msgs, err := mgr.GetMany("s1", []string{msg1.ID, msg2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
}

func TestManager_AppendRace(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	mgr := NewManager(store)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mgr.Append("s1", api.MsgInput, fmt.Sprintf("msg-%d", n))
		}(i)
	}
	wg.Wait()

	entries, err := mgr.List("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(entries))
	}
}

func TestManager_ListEmpty(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	mgr := NewManager(store)

	entries, err := mgr.List("nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}
