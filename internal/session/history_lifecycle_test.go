package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/history"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func newHistoryManager(t *testing.T, store *storage.Store) *history.Manager {
	t.Helper()
	hm := history.New(store)
	if err := hm.Load(); err != nil {
		t.Fatal(err)
	}
	return hm
}

func archLen(t *testing.T, hm *history.Manager) int {
	t.Helper()
	return len(hm.List())
}

// Terminate / Disconnect must NOT archive the session nor remove it from the
// registry: it becomes a retained DEAD (exited) session whose message history
// stays on disk for read-only replay. Nothing is moved into history.json.
func TestManager_TerminateKeepsSessionDead(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	hm := newHistoryManager(t, store)

	m := NewManager(mm, store, srv)
	m.SetHistory(hm)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "ctf-box"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	m.Terminate(id, true, 0)

	// Retained in the active registry, moved to DEAD (exited), not archived.
	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after terminate, got %q", got)
	}
	if archLen(t, hm) != 0 {
		t.Fatalf("expected 0 archived records for a DEAD session, got %d", archLen(t, hm))
	}
	// Message history must survive the DEAD transition for read-only replay.
	if !store.MessageDirExists(id) {
		t.Fatal("expected message history retained after terminate")
	}
}

// Manual delete is the only release: it removes the session from the registry,
// clears the history record, and purges the persisted messages.
func TestManager_DeletePurgesHistoryAndMessages(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	hm := newHistoryManager(t, store)

	m := NewManager(mm, store, srv)
	m.SetHistory(hm)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "ctf-box"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}

	if m.Get(id) != nil {
		t.Fatal("expected session removed from registry after delete")
	}
	time.Sleep(200 * time.Millisecond)
	if archLen(t, hm) != 0 {
		t.Fatalf("expected history cleared, got %d record(s)", archLen(t, hm))
	}
	if store.MessageDirExists(id) {
		t.Fatal("expected message history purged after delete")
	}
}

// A DEAD session is persisted to sessions.json and reconstructed as a read-only
// session by a fresh manager (restart). No SSH transport is created.
func TestManager_RestoreDeadAfterRestart(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	hm := newHistoryManager(t, store)

	m1 := NewManager(mm, store, srv)
	m1.SetHistory(hm)

	command, args := testSleepCommand("60")
	s, err := m1.Create(testConfig(command, args, api.ModePipe, "persistent"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	// Write input so a message exists on disk, then DEAD the session (persists
	// sessions.json via onDead → manager.persist).
	if _, err := mm.AppendShell(id, s.PrimaryShellID(), api.MsgInput, "hello\n"); err != nil {
		t.Fatal(err)
	}
	m1.Terminate(id, true, 0)
	if m1.Get(id).Info().Status != api.SessionExited {
		t.Fatal("expected session DEAD before restart")
	}

	// "Restart": fresh manager over the same store.
	m2 := NewManager(mm, store, srv)
	m2.SetHistory(hm)
	if err := m2.RestoreDead(); err != nil {
		t.Fatal(err)
	}

	restored := m2.Get(id)
	if restored == nil {
		t.Fatal("expected restored DEAD session in registry")
	}
	if got := restored.Info().Status; got != api.SessionExited {
		t.Fatalf("expected restored status 'exited', got %q", got)
	}
	if restored.SSHClient() != nil {
		t.Fatal("expected restored session to hold no live SSH transport")
	}
	// Shell snapshot is repopulated so tabs can render.
	if len(restored.ShellsForView()) == 0 {
		t.Fatal("expected restored session to expose its persisted shell metadata")
	}
	if !store.MessageDirExists(id) {
		t.Fatal("expected persisted messages to survive restart")
	}
}

// Deliberate close (ArchiveAndForget): the session is terminated, moved into the
// history archive with a reason, dropped from the live registry (so it is NOT
// reloaded as a DEAD tile after restart), and its on-disk message files are
// retained so transcript/screenshot/search still work. Only a later purge erases.
func TestManager_ArchiveAndForgetMovesToHistory_NotDeadTile(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	hm := newHistoryManager(t, store)

	m := NewManager(mm, store, srv)
	m.SetHistory(hm)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "ctf-box"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	if _, err := mm.AppendShell(id, s.PrimaryShellID(), api.MsgInput, "hello\n"); err != nil {
		t.Fatal(err)
	}

	if err := m.ArchiveAndForget(id, api.ArchiveExplicit); err != nil {
		t.Fatal(err)
	}

	// Removed from the live registry: no DEAD tile, and a restart cannot reload
	// it into the registry as one (sessions.json no longer contains the id).
	if m.Get(id) != nil {
		t.Fatal("expected session removed from registry after deliberate close")
	}

	// Archived with the explicit reason.
	a, ok := hm.Get(id)
	if !ok {
		t.Fatal("expected an archived history record after deliberate close")
	}
	if a.Reason != api.ArchiveExplicit {
		t.Fatalf("expected reason 'explicit', got %q", a.Reason)
	}

	// Message history retained so transcript/screenshot/search keep working.
	if !store.MessageDirExists(id) {
		t.Fatal("expected on-disk message history retained after deliberate close")
	}
}

// ArchiveAndForget on an unknown id is a no-op error and must not panic.
func TestManager_ArchiveAndForget_UnknownSession(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	hm := newHistoryManager(t, store)
	m := NewManager(mm, store, srv)
	m.SetHistory(hm)

	if err := m.ArchiveAndForget("nope", api.ArchiveExplicit); err == nil {
		t.Fatal("expected error for unknown session")
	}
}
