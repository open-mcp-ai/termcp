package session

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

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

func testEchoCommand(s string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoProfile", "-Command", "Write-Output " + s}
	}
	return "echo", []string{s}
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
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: testShell(), Args: testInteractiveShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	s.Terminate(true, 0)

	// Wait for exit
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Info().Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	info1 := s.Info()
	info2 := s.Info()

	if info1.ExitCode == nil || info2.ExitCode == nil {
		t.Fatal("expected ExitCode to be set")
	}
	if info1.ExitCode == info2.ExitCode {
		t.Fatal("Info() should return independent ExitCode pointers")
	}
	if *info1.ExitCode != *info2.ExitCode {
		t.Fatalf("ExitCode values should match: %d vs %d", *info1.ExitCode, *info2.ExitCode)
	}
}

func startTestServer(t *testing.T) *sshserver.Server {
	t.Helper()
	srv := sshserver.New("127.0.0.1:0")
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
	addr := srv.Addr()

	s, err := New(addr, srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "test-session"), nil)
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
	addr := srv.Addr()

	s, err := New(addr, srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
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
		chunk, _ := s.ReadOutput(context.Background(), 500*time.Millisecond, true, 0)
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
	addr := srv.Addr()

	command, args := testSleepCommand("60")
	s, err := New(addr, srv, testConfig(command, args, api.ModePipe, ""), nil)
	if err != nil {
		t.Fatal(err)
	}

	info := s.Info()
	if info.Status != api.SessionRunning {
		t.Fatalf("expected 'running', got %q", info.Status)
	}

	s.Terminate(false, 2*time.Second)

	time.Sleep(200 * time.Millisecond)

	info = s.Info()
	if info.Status != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", info.Status)
	}
	if info.ExitCode == nil {
		t.Fatal("expected non-nil exit code")
	}
}

func TestSession_ForceTerminate(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	command, args := testSleepCommand("60")
	s, err := New(addr, srv, testConfig(command, args, api.ModePipe, ""), nil)
	if err != nil {
		t.Fatal(err)
	}

	s.Terminate(true, 0)

	time.Sleep(200 * time.Millisecond)
	info := s.Info()
	if info.Status != api.SessionExited {
		t.Fatalf("expected 'exited' after force terminate, got %q", info.Status)
	}
}

func TestSession_ResizePty(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	s, err := New(addr, srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
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
	addr := srv.Addr()

	s, err := New(addr, srv, testConfig(testPipeCommand(), nil, api.ModePipe, ""), nil)
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
	addr := srv.Addr()

	s, err := New(addr, srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
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
	addr := srv.Addr()

	s, err := New(addr, srv, Config{Command: testShell(), Args: testShellEchoArgs("hello"), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	info := s.Info()
	if info.Status != api.SessionExited {
		t.Fatalf("expected 'exited', got %q", info.Status)
	}
	if info.ExitCode == nil || *info.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", info.ExitCode)
	}
}

func TestManager_CreateAndGet(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	mgr := NewManager(addr, nil, nil, srv)

	command, args := testEchoCommand("hi")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "test", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}

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
	addr := srv.Addr()

	mgr := NewManager(addr, nil, nil, srv)

	commandA, argsA := testEchoCommand("a")
	commandB, argsB := testEchoCommand("b")
	mgr.Create(Config{Command: commandA, Args: argsA, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	mgr.Create(Config{Command: commandB, Args: argsB, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})

	all := mgr.ListAll()
	if len(all) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(all))
	}
}

func TestManager_CleanupAll(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	mgr := NewManager(addr, nil, nil, srv)

	command, args := testSleepCommand("60")
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s1", Rows: 24, Cols: 80})
	mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "s2", Rows: 24, Cols: 80})

	mgr.CleanupAll(true)

	time.Sleep(500 * time.Millisecond)

	for _, s := range mgr.ListAll() {
		if s.Status != api.SessionExited {
			t.Fatalf("expected all sessions exited, got %q for %s", s.Status, s.ID)
		}
	}
}

func TestManager_Delete(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	mgr := NewManager(addr, nil, nil, srv)

	command, args := testSleepCommand("0.1")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Name: "del-me", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}

	// Terminate and wait for exited status
	s.Terminate(true, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Info().Status != api.SessionRunning {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if mgr.Get(s.ID) == nil {
		t.Fatal("expected session to exist")
	}

	if err := mgr.Delete(s.ID); err != nil {
		t.Fatal(err)
	}

	if mgr.Get(s.ID) != nil {
		t.Fatal("expected session to be deleted")
	}

	all := mgr.ListAll()
	if len(all) != 0 {
		t.Fatalf("expected 0 sessions after delete, got %d", len(all))
	}
}

func TestManager_DeleteRunningSession(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	mgr := NewManager(addr, nil, nil, srv)

	command, args := testSleepCommand("60")
	s, err := mgr.Create(Config{Command: command, Args: args, Mode: api.ModePipe, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	err = mgr.Delete(s.ID)
	if err == nil {
		t.Fatal("expected error when deleting running session")
	}

	if mgr.Get(s.ID) == nil {
		t.Fatal("running session should not be removed from registry")
	}
}

func TestSession_GoroutinesCleanedUp(t *testing.T) {
	srv := startTestServer(t)
	addr := srv.Addr()

	before := runtime.NumGoroutine()

	s, err := New(addr, srv, Config{Command: testShell(), Args: testInteractiveShellArgs(), Mode: api.ModePTY, Rows: 24, Cols: 80}, nil)
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

	s.CloseSFTP()
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	leaked := after - before
	if leaked > 2 {
		t.Fatalf("leaked %d goroutines after terminate (before=%d, after=%d)", leaked, before, after)
	}
}
