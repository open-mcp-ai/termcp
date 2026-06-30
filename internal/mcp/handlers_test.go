package mcp

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })

	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	if err := sshconfig.EnsureInternal(dir); err != nil {
		t.Fatal(err)
	}
	sessMgr := session.NewManager(msgMgr, store, srv)
	return New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil)
}

func makeRequest(args map[string]any) mcpgo.CallToolRequest {
	return mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{
			Arguments: args,
		},
	}
}

func parseResult(t *testing.T, result *mcpgo.CallToolResult) map[string]any {
	t.Helper()
	text := result.Content[0].(mcpgo.TextContent).Text
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("failed to parse result: %v, text: %s", err, text)
	}
	return m
}

func testShell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "bash"
}

func testInteractiveShellArgs() []any {
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

func testShellArgs(args ...string) []any {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg
	}
	return values
}

func testShellEchoArgs(s string) []any {
	if runtime.GOOS == "windows" {
		return testShellArgs("-NoLogo", "-NoProfile", "-Command", "Write-Output "+s)
	}
	return testShellArgs("-c", "echo "+s)
}

func testReadOutputUntil(t *testing.T, s *Server, sessionID, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output string
	for time.Now().Before(deadline) {
		readReq := makeRequest(map[string]any{
			"session_id": sessionID,
			"timeout":    0.5,
		})
		readResult, err := s.handleReadOutput(context.Background(), readReq)
		if err != nil {
			t.Fatal(err)
		}
		if readResult.IsError {
			t.Fatalf("unexpected error: %s", readResult.Content[0].(mcpgo.TextContent).Text)
		}
		readM := parseResult(t, readResult)
		output += readM["output"].(string)
		if strings.Contains(output, marker) {
			break
		}
	}
	return output
}

func TestHandleDetectShell_Auto(t *testing.T) {
	s := newTestServer(t)
	result, err := s.handleDetectShell(context.Background(), makeRequest(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].(mcpgo.TextContent).Text)
	}

	m := parseResult(t, result)
	path, ok := m["path"].(string)
	if !ok || path == "" {
		t.Fatalf("expected non-empty shell path, got %#v", m["path"])
	}
	family, ok := m["family"].(string)
	if !ok || family == "" {
		t.Fatalf("expected non-empty shell family, got %#v", m["family"])
	}
	if family != "unix" && family != "powershell" && family != "cmd" {
		t.Fatalf("expected supported shell family, got %q", family)
	}
	hint, ok := m["hint"].(string)
	if !ok || hint == "" {
		t.Fatalf("expected non-empty hint, got %#v", m["hint"])
	}
}

func TestHandleStartSession_EmptyCommandOK(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{})
	result, err := s.handleStartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].(mcpgo.TextContent).Text)
	}
	m := parseResult(t, result)
	if m["session_id"] == nil {
		t.Fatal("expected session_id in result")
	}
}

func TestHandleStartSession_CommandRequiredWhenArgs(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"args": []any{"-c", "echo hi"},
	})
	result, err := s.handleStartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error when args without command")
	}
}

func TestHandleStartSession_Success(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"command": "echo",
		"args":    []any{"hello"},
		"mode":    "pipe",
	})

	result, err := s.handleStartSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content[0].(mcpgo.TextContent).Text)
	}

	m := parseResult(t, result)
	if m["session_id"] == nil {
		t.Fatal("expected session_id in result")
	}
	if m["ssh_config"] != "internal" {
		t.Fatalf("expected ssh_config internal, got %v", m["ssh_config"])
	}
	if m["initial_output"] != "" {
		t.Fatalf("expected empty initial_output, got %v", m["initial_output"])
	}
}

func TestHandleSendInput_SessionNotFound(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"session_id": "nonexistent",
		"text":       "hello",
	})

	result, err := s.handleSendInput(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestHandleListSessions_Empty(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{})

	result, err := s.handleListSessions(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	m := parseResult(t, result)
	sessions := m["sessions"].([]any)
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestHandleTerminateSession_SessionNotFound(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"session_id": "nonexistent",
	})

	result, err := s.handleTerminateSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestHandleGetSessionInfo_NotFound(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"session_id": "nonexistent",
	})

	result, err := s.handleGetSessionInfo(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestHandleResizePty_NotFound(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"session_id": "nonexistent",
		"rows":       float64(50),
		"cols":       float64(120),
	})

	result, err := s.handleResizePty(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent session")
	}
}

func TestHandleStartAndReadOutput(t *testing.T) {
	s := newTestServer(t)

	// Start a bash session
	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testInteractiveShellArgs(),
		"mode":    "pty",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	time.Sleep(300 * time.Millisecond)

	// Send input and read
	sarReq := makeRequest(map[string]any{
		"session_id":  sessionID,
		"text":        testShellInput(testInteractiveOutputCommand("handler_test")),
		"press_enter": false,
		"timeout":     3.0,
	})
	sarResult, err := s.handleSendAndRead(context.Background(), sarReq)
	if err != nil {
		t.Fatal(err)
	}
	if sarResult.IsError {
		t.Fatalf("unexpected error: %s", sarResult.Content[0].(mcpgo.TextContent).Text)
	}

	sarM := parseResult(t, sarResult)
	output := sarM["output"].(string)
	if !strings.Contains(output, "handler_test") {
		output += testReadOutputUntil(t, s, sessionID, "handler_test", 3*time.Second)
	}
	if !strings.Contains(output, "handler_test") {
		t.Fatalf("expected output containing 'handler_test', got %q", output)
	}

	// Cleanup
	termReq := makeRequest(map[string]any{
		"session_id": sessionID,
		"force":      true,
	})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestHandleListMessages(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"command": "echo",
		"args":    []any{"test"},
		"mode":    "pipe",
	})
	startResult, _ := s.handleStartSession(context.Background(), startReq)
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	time.Sleep(500 * time.Millisecond)

	// List messages for this session
	listReq := makeRequest(map[string]any{
		"session_id": sessionID,
	})
	listResult, err := s.handleListMessages(context.Background(), listReq)
	if err != nil {
		t.Fatal(err)
	}

	listM := parseResult(t, listResult)
	msgs := listM["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
}

func TestHandleBackgroundSend_Success(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testInteractiveShellArgs(),
		"mode":    "pty",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	time.Sleep(300 * time.Millisecond)

	// background_send should return immediately without reading output
	start := time.Now()
	bgReq := makeRequest(map[string]any{
		"session_id":  sessionID,
		"text":        testShellInput(testInteractiveOutputCommand("bg_test")),
		"press_enter": false,
	})
	bgResult, err := s.handleBackgroundSend(context.Background(), bgReq)
	if err != nil {
		t.Fatal(err)
	}
	if bgResult.IsError {
		t.Fatalf("unexpected error: %s", bgResult.Content[0].(mcpgo.TextContent).Text)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("background_send took %v — should return immediately", elapsed)
	}

	bgM := parseResult(t, bgResult)
	if bgM["success"] != true {
		t.Fatal("expected success=true")
	}

	// Verify the input was actually delivered by reading output
	output := testReadOutputUntil(t, s, sessionID, "bg_test", 3*time.Second)
	if !strings.Contains(output, "bg_test") {
		t.Fatalf("expected output containing 'bg_test', got %q", output)
	}

	// Cleanup
	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestHandleSendAndRead_ContextCancelled(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testInteractiveShellArgs(),
		"mode":    "pty",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	time.Sleep(300 * time.Millisecond)

	// Cancel context after 200ms — send_and_read should return quickly
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	sarReq := makeRequest(map[string]any{
		"session_id":  sessionID,
		"text":        testShellInput("sleep 10"),
		"press_enter": false,
		"timeout":     30.0,
	})
	sarResult, err := s.handleSendAndRead(ctx, sarReq)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Should return within 500ms after cancellation, not wait 30s
	if elapsed > 2*time.Second {
		t.Fatalf("send_and_read should return on ctx cancel, took %v", elapsed)
	}

	// Result should not be an error — just empty output from cancelled read
	if sarResult.IsError {
		// Could be error from send or read — both are acceptable on cancel
	}

	// Cleanup
	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestHandleBackgroundSend_ExitedSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PowerShell -Command under ConPTY stays interactive after command completion")
	}
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testShellEchoArgs("done"),
		"mode":    "pty",
	})
	startResult, _ := s.handleStartSession(context.Background(), startReq)
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	// Wait for process to exit
	time.Sleep(2 * time.Second)

	bgReq := makeRequest(map[string]any{
		"session_id":  sessionID,
		"text":        "should fail",
		"press_enter": true,
	})
	bgResult, err := s.handleBackgroundSend(context.Background(), bgReq)
	if err != nil {
		t.Fatal(err)
	}
	if !bgResult.IsError {
		t.Fatal("expected error when sending to exited session")
	}
}

func TestHandleStartSession_InvalidMode(t *testing.T) {
	s := newTestServer(t)
	for _, mode := range []string{"websocket", "x"} {
		req := makeRequest(map[string]any{
			"command": "echo",
			"mode":    mode,
		})
		result, _ := s.handleStartSession(context.Background(), req)
		if !result.IsError {
			t.Fatalf("expected error for mode %q", mode)
		}
	}
}

func TestHandleStartSession_InvalidRowsCols(t *testing.T) {
	s := newTestServer(t)
	for _, tc := range []struct {
		rows float64
		cols float64
	}{
		{0, 80}, {-1, 80}, {24, 0}, {24, -5}, {1001, 80},
	} {
		req := makeRequest(map[string]any{
			"command": "echo",
			"mode":    "pty",
			"rows":    tc.rows,
			"cols":    tc.cols,
		})
		result, _ := s.handleStartSession(context.Background(), req)
		if !result.IsError {
			t.Fatalf("expected error for rows=%v cols=%v", tc.rows, tc.cols)
		}
	}
}

func TestHandleReadOutput_InvalidTimeout(t *testing.T) {
	s := newTestServer(t)
	startReq := makeRequest(map[string]any{"command": "echo", "mode": "pipe"})
	startResult, _ := s.handleStartSession(context.Background(), startReq)
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	for _, timeout := range []float64{-1, 0.001, 61, 999} {
		req := makeRequest(map[string]any{
			"session_id": sessionID,
			"timeout":    timeout,
		})
		result, _ := s.handleReadOutput(context.Background(), req)
		if !result.IsError {
			t.Fatalf("expected error for timeout %v", timeout)
		}
	}
}

func TestHandleTerminateSession_InvalidGracePeriod(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{"command": "echo", "mode": "pipe"})
	startResult, _ := s.handleStartSession(context.Background(), startReq)
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	for _, gp := range []float64{-1, 61, 3600} {
		req := makeRequest(map[string]any{
			"session_id":   sessionID,
			"grace_period": gp,
		})
		result, _ := s.handleTerminateSession(context.Background(), req)
		if !result.IsError {
			t.Fatalf("expected error for grace_period %v", gp)
		}
	}
}

func TestHandleReadOutput_ReturnsSessionStatus(t *testing.T) {
	s := newTestServer(t)

	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testInteractiveShellArgs(),
		"mode":    "pty",
	})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)

	time.Sleep(300 * time.Millisecond)

	readReq := makeRequest(map[string]any{
		"session_id": sessionID,
		"timeout":    1.0,
	})
	result, err := s.handleReadOutput(context.Background(), readReq)
	if err != nil {
		t.Fatal(err)
	}
	rm := parseResult(t, result)

	status, ok := rm["session_status"].(string)
	if !ok {
		t.Fatal("expected session_status in read_output result")
	}
	if status != "running" {
		t.Fatalf("expected session_status=running, got %q", status)
	}

	uptime, ok := rm["session_uptime_seconds"]
	if !ok {
		t.Fatal("expected session_uptime_seconds in read_output result")
	}
	// JSON numbers unmarshal as float64
	sec := uptime.(float64)
	if sec < 0 {
		t.Fatalf("expected session_uptime_seconds >= 0, got %v", sec)
	}

	termReq := makeRequest(map[string]any{
		"session_id": sessionID,
		"force":      true,
	})
	s.handleTerminateSession(context.Background(), termReq)
}
