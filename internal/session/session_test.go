package session

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func testInteractiveShellArgs() []string {
	if runtime.GOOS == "windows" {
		return testShellArgs("-NoLogo", "-NoProfile")
	}
	return nil
}

func testShellInput(s string) string {
	if runtime.GOOS == "windows" {
		return s + "\r\n"
	}
	return s + "\n"
}

func testInteractiveOutputCommand(s string) string {
	if runtime.GOOS == "windows" {
		return "Write-Output " + s
	}
	return "echo " + s
}

func testShellArgs(args ...string) []string {
	return args
}

func testShellEchoArgs(s string) []string {
	if runtime.GOOS == "windows" {
		return testShellArgs("-NoLogo", "-NoProfile", "-Command", "Write-Output "+s)
	}
	return testShellArgs("-c", "echo "+s)
}

func testSleepCommand(seconds string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Start-Sleep -Seconds " + seconds}
	}
	return "sleep", []string{seconds}
}

func testPipeCommand() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "cat"
}

func TestInfo_DeepCopyExitCode(t *testing.T) {
	t.Skip("Session.done() no longer tracks ExitCode; exit code is per-shell")
}

func startTestServer(t *testing.T) *sshserver.Server {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })
	return srv
}

func testConfig(command string, args []string, mode api.SessionMode, name string) Config {
	return Config{
		Command: command,
		Args:    args,
		Mode:    mode,
		Name:    name,
		Rows:    24,
		Cols:    80,
	}
}

func TestSession_CreateAndInfo(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "test-session"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	info := s.Info()
	if info.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if info.Name != "test-session" {
		t.Fatalf("expected name 'test-session', got %q", info.Name)
	}
	if info.Status != api.SessionRunning {
		t.Fatalf("expected status 'running', got %q", info.Status)
	}
	if info.Mode != api.ModePTY {
		t.Fatalf("expected mode 'pty', got %q", info.Mode)
	}
}

func TestSession_SendInputReadOutput(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(200 * time.Millisecond)

	if err := s.SendInput(testShellInput(testInteractiveOutputCommand("session_test")), false); err != nil {
		t.Fatal(err)
	}

	var output string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		chunk, _ := s.ReadOutput(context.Background(), 500*time.Millisecond, true, 0, 0)
		output += chunk
		if strings.Contains(output, "session_test") {
			break
		}
	}
	if !strings.Contains(output, "session_test") {
		t.Fatalf("expected output containing 'session_test', got %q", output)
	}
}

func TestSession_Terminate(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)
	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	info := s.Info()
	if info.Status != api.SessionRunning {
		t.Fatalf("expected 'running', got %q", info.Status)
	}

	m.Terminate(id, false, 2*time.Second)

	if m.Get(id) == nil {
		t.Fatal("expected session to be retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after terminate, got %q", got)
	}
}

func TestSession_ForceTerminate(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)
	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	m.Terminate(id, true, 0)

	if m.Get(id) == nil {
		t.Fatal("expected session to be retained (DEAD) after force terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after force terminate, got %q", got)
	}
}

func TestManager_TerminateKeepsDeadThenDeleteReleases(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)

	var (
		mu    sync.Mutex
		ids   []string
		count int
	)
	m.SetTerminateListener(func(sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		count++
		ids = append(ids, sessionID)
	})

	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	m.Terminate(id, true, 0)
	// Repeating the DEAD path (e.g. Disconnect after Terminate) must not fire
	// resource cleanup or remove the registry entry.
	s.Disconnect()

	mu.Lock()
	if count != 0 {
		t.Fatalf("terminate/disconnect must not fire terminate listener, got %d", count)
	}
	mu.Unlock()

	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", got)
	}

	// Delete is the only release: fires cleanup once and removes the registry entry.
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if count != 1 {
		t.Fatalf("expected resource cleanup once on delete, got %d (ids=%v)", count, ids)
	}
	mu.Unlock()
	if m.Get(id) != nil {
		t.Fatal("expected session removed after delete")
	}
}

func TestManager_DisconnectKeepsDead(t *testing.T) {
	srv := startTestServer(t)

	command, args := testSleepCommand("60")
	m := NewManager(nil, nil, srv)

	var (
		mu    sync.Mutex
		count int
	)
	m.SetTerminateListener(func(sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		count++
	})

	s, err := m.Create(testConfig(command, args, api.ModePipe, ""))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	// SSH abort/Disconnect only DEADs the session; it never releases resources.
	s.Disconnect()

	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after disconnect")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after disconnect, got %q", got)
	}
	mu.Lock()
	if count != 0 {
		t.Fatalf("disconnect must not fire terminate listener, got %d", count)
	}
	mu.Unlock()

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if count != 1 {
		t.Fatalf("expected resource cleanup once on delete, got %d", count)
	}
	mu.Unlock()
	if m.Get(id) != nil {
		t.Fatal("expected session removed after delete")
	}
}

func TestSession_ResizePty(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if err := s.ResizePty(50, 120); err != nil {
		t.Fatalf("ResizePty failed: %v", err)
	}

	info := s.Info()
	if info.Rows != 50 || info.Cols != 120 {
		t.Fatalf("expected 50x120, got %dx%d", info.Rows, info.Cols)
	}
}

func TestSession_ResizePtyPipeMode(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testPipeCommand(), nil, api.ModePipe, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	err = s.ResizePty(50, 120)
	if err == nil {
		t.Fatal("expected error when resizing PTY in pipe mode")
	}
}

func TestSession_SendInputAfterExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell exit under ConPTY is not deterministic enough for this assertion")
	}
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}

	s.SendInput(testShellInput("exit"), false)

	time.Sleep(1 * time.Second)

	err = s.SendInput("should fail", true)
	if err == nil {
		t.Fatal("expected error sending input to exited process")
	}
}

func TestSession_NaturalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell -Command under ConPTY stays interactive after command completion")
	}
	srv := startTestServer(t)

	s, err := New(srv, Config{Command: testShell(), Args: testShellEchoArgs("hello"), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	// Shell exit does not tear down the session — the session container
	// stays alive for potential new shells (applies to both internal and remote).
	info := s.Info()
	if info.Status != api.SessionRunning {
		t.Fatalf("session should still be running after shell exit, got %q", info.Status)
	}
}

// TestManager_PipeSessionLastExitMarksDead locks the pipe-mode contract: when the
// last (root) pipe shell exits cleanly, the container flips to DEAD (retained, not
// auto-deleted) so it disappears from the running list while keeping its output for
// read-only viewing / manual cleanup. PTY sessions stay running after a shell exit.
func TestManager_PipeSessionLastExitMarksDead(t *testing.T) {
	srv := startTestServer(t)
	mgr := NewManager(nil, nil, srv)

	// A short pipe command (echo) that exits cleanly on its own.
	s, err := mgr.Create(Config{Command: testShell(), Args: testShellEchoArgs("done"), Mode: api.ModePipe, Name: "pipe", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && mgr.Get(id) != nil && mgr.Get(id).Info().Status != api.SessionExited {
		time.Sleep(50 * time.Millisecond)
	}

	got := mgr.Get(id)
	if got == nil {
		t.Fatal("pipe session should be retained (DEAD, not deleted) after last shell exits")
	}
	if got.Info().Status != api.SessionExited {
		t.Fatalf("pipe session should become DEAD after last shell exits, got %q", got.Info().Status)
	}
	// Output must remain readable from the retained (still registered) shell.
	shells := got.ListChildShells()
	if len(shells) == 0 {
		t.Fatal("expected exited shell retained in map for reading output")
	}
}

func TestManager_CreateAndGet(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	// Use a long-running command so auto-delete doesn't fire before we inspect.
	command, args := testSleepCommand("60")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "test", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	got := mgr.Get(s.ID)
	if got == nil {
		t.Fatal("expected to find session")
	}
	if got.ID != s.ID {
		t.Fatalf("expected ID %q, got %q", s.ID, got.ID)
	}
}

func TestManager_ListAll(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	s1, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Terminate(true, 0)
	s2, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Terminate(true, 0)

	all := mgr.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
}

func TestManager_MarkAllDead(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})

	// Shutdown semantics: DEAD every running session in place; nothing is
	// removed from the registry (disconnect ≠ delete).
	mgr.MarkAllDead()

	time.Sleep(500 * time.Millisecond)

	all := mgr.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions retained after MarkAllDead, got %d", len(all))
	}
	for _, s := range all {
		if s.Status != api.SessionExited {
			t.Fatalf("expected session %q exited after MarkAllDead, got %q", s.ID, s.Status)
		}
	}
}

func TestManager_Delete(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("0.1")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "del-me", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}

	sid := s.ID

	// Terminate only turns the session DEAD; it stays registered.
	s.Terminate(true, 0)
	if mgr.Get(sid) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := mgr.Get(sid).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", got)
	}

	// Only Delete removes it from the registry.
	if err := mgr.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(sid) != nil {
		t.Fatal("expected session removed from registry after delete")
	}

	all := mgr.ListAll()
	if len(all) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(all))
	}
}

func TestManager_DeleteRunningSession(t *testing.T) {
	srv := startTestServer(t)

	mgr := NewManager(nil, nil, srv)

	command, args := testSleepCommand("60")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	sid := s.ID

	if err := mgr.Delete(sid); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(sid) != nil {
		t.Fatal("expected running session to be force-removed by Delete")
	}
}

func TestSession_GoroutinesCleanedUp(t *testing.T) {
	srv := startTestServer(t)

	before := runtime.NumGoroutine()

	s, err := New(srv, Config{Command: testShell(), Args: testInteractiveShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s.Terminate(true, 0)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Info().Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - before
	if leaked > 2 {
		t.Fatalf("leaked %d goroutines after terminate (before=%d, after=%d)", leaked, before, after)
	}
}

func TestSession_ReadOutputWithMaxBytes(t *testing.T) {
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(200 * time.Millisecond)

	// Generate predictable output longer than maxBytes
	longText := strings.Repeat("ABCDEFGHIJ", 100) // 1000 bytes
	cmd := testInteractiveOutputCommand(longText)
	if err := s.SendInput(testShellInput(cmd), false); err != nil {
		t.Fatal(err)
	}

	// Wait for output to accumulate
	time.Sleep(500 * time.Millisecond)

	maxBytes := 100
	output, err := s.ReadOutput(context.Background(), 500*time.Millisecond, true, 0, maxBytes)
	if err != nil {
		t.Fatal(err)
	}

	if len(output) > maxBytes {
		t.Fatalf("expected output <= %d bytes, got %d bytes", maxBytes, len(output))
	}

	// Should still have more data available
	if !s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected HasMoreOutput=true after partial read")
	}
}

func TestSession_ReadOutputWithMaxLinesPreservesUnreadData(t *testing.T) {
	b := buffer.New(1024)
	r, _ := b.NewReader()
	s := &Session{
		Session:  api.Session{ID: "test-session"},
		buf:      b,
		readerID: r,
	}

	if err := b.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}

	output, err := s.ReadOutput(context.Background(), 0, false, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo\n" {
		t.Fatalf("expected first two lines, got %q", output)
	}
	if !s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected unread output after max_lines read")
	}

	output, err = s.ReadOutput(context.Background(), 0, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "three\nfour\n" {
		t.Fatalf("expected remaining lines, got %q", output)
	}
	if s.HasMoreOutput(s.DefaultOutputReaderID()) {
		t.Fatal("expected no unread output after draining")
	}
}

func TestChildShell_ReadTerminalStreamWithMaxLinesPreservesUnreadData(t *testing.T) {
	b := buffer.New(1024)
	r, _ := b.NewReader()
	cs := &ChildShell{buf: b}

	if err := b.Write([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatal(err)
	}

	output, err := cs.ReadTerminalStream(context.Background(), r, 0, false, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "one\ntwo\n" {
		t.Fatalf("expected first two lines, got %q", output)
	}
	if !cs.HasMoreOutput(r) {
		t.Fatal("expected unread output after max_lines read")
	}

	output, err = cs.ReadTerminalStream(context.Background(), r, 0, false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output != "three\nfour\n" {
		t.Fatalf("expected remaining lines, got %q", output)
	}
	if cs.HasMoreOutput(r) {
		t.Fatal("expected no unread output after draining")
	}
}

func TestManager_CloseInternalChildShellKeepsParentSession(t *testing.T) {
	srv := startTestServer(t)
	mgr := NewManager(nil, nil, srv)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Delete(s.ID)

	child, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "child")
	if err != nil {
		t.Fatal(err)
	}

	found, err := mgr.CloseChildShell(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("expected child shell to be found")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && s.GetChildShell(child.ID) != nil {
		time.Sleep(20 * time.Millisecond)
	}

	if s.GetChildShell(child.ID) != nil {
		t.Fatal("expected child shell to be removed")
	}

	time.Sleep(200 * time.Millisecond)
	if mgr.Get(s.ID) == nil {
		t.Fatal("expected parent session to remain registered after closing child shell")
	}
	if s.IsBufferClosed() {
		t.Fatal("expected parent output buffer to remain open after closing child shell")
	}
}

func TestAppendEnter(t *testing.T) {
	if got := appendEnter([]byte("ls"), false); string(got) != "ls\n" {
		t.Fatalf("unix: expected %q, got %q", "ls\n", got)
	}
	if got := appendEnter([]byte("dir"), true); string(got) != "dir\r\n" {
		t.Fatalf("windows: expected %q, got %q", "dir\r\n", got)
	}
	if got := appendEnter(nil, false); string(got) != "\n" {
		t.Fatalf("empty unix: expected %q, got %q", "\n", got)
	}
}
