package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	"github.com/open-mcp-ai/termcp/pkg/api"
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

func testReadOutputUntil(t *testing.T, s *Server, shellID, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output string
	for time.Now().Before(deadline) {
		readReq := makeRequest(map[string]any{
			"shell_id": shellID,
			"timeout":  0.5,
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

func testRunLine(t *testing.T, s *Server, shellID, text string) {
	t.Helper()
	sendReq := makeRequest(map[string]any{"shell_id": shellID, "text": text})
	sendResult, err := s.handleSendInput(context.Background(), sendReq)
	if err != nil {
		t.Fatal(err)
	}
	if sendResult.IsError {
		t.Fatalf("send_input error: %s", sendResult.Content[0].(mcpgo.TextContent).Text)
	}
	keyReq := makeRequest(map[string]any{"shell_id": shellID, "key": "enter"})
	keyResult, err := s.handlePressKey(context.Background(), keyReq)
	if err != nil {
		t.Fatal(err)
	}
	if keyResult.IsError {
		t.Fatalf("press_key error: %s", keyResult.Content[0].(mcpgo.TextContent).Text)
	}
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
	if m["shell_id"] == nil || m["shell_id"] == "" {
		t.Fatal("expected shell_id in result")
	}
	if m["shell_id"] == m["session_id"] {
		t.Fatal("shell_id must differ from session_id")
	}
}

func TestHandleSendInput_ShellNotFound(t *testing.T) {
	s := newTestServer(t)
	req := makeRequest(map[string]any{
		"shell_id": "nonexistent",
		"text":     "hello",
	})

	result, err := s.handleSendInput(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent shell")
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
		"shell_id": "nonexistent",
		"rows":     float64(50),
		"cols":     float64(120),
	})

	result, err := s.handleResizePty(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent shell")
	}
}

func TestHandleStartSendPressKeyRead(t *testing.T) {
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
	shellID := m["shell_id"].(string)

	time.Sleep(300 * time.Millisecond)

	testRunLine(t, s, shellID, testInteractiveOutputCommand("handler_test"))
	output := testReadOutputUntil(t, s, shellID, "handler_test", 3*time.Second)
	if !strings.Contains(output, "handler_test") {
		t.Fatalf("expected output containing 'handler_test', got %q", output)
	}

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

func TestHandleSendInput_ReturnsImmediately(t *testing.T) {
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
	shellID := m["shell_id"].(string)

	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	sendReq := makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     testInteractiveOutputCommand("bg_test"),
	})
	sendResult, err := s.handleSendInput(context.Background(), sendReq)
	if err != nil {
		t.Fatal(err)
	}
	if sendResult.IsError {
		t.Fatalf("unexpected error: %s", sendResult.Content[0].(mcpgo.TextContent).Text)
	}
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("send_input took %v — should return immediately", elapsed)
	}

	keyReq := makeRequest(map[string]any{"shell_id": shellID, "key": "enter"})
	if _, err := s.handlePressKey(context.Background(), keyReq); err != nil {
		t.Fatal(err)
	}

	output := testReadOutputUntil(t, s, shellID, "bg_test", 3*time.Second)
	if !strings.Contains(output, "bg_test") {
		t.Fatalf("expected output containing 'bg_test', got %q", output)
	}

	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestHandleReadOutput_ContextCancelled(t *testing.T) {
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
	shellID := m["shell_id"].(string)

	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	readReq := makeRequest(map[string]any{
		"shell_id": shellID,
		"timeout":  30.0,
	})
	readResult, err := s.handleReadOutput(ctx, readReq)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("read_output should return on ctx cancel, took %v", elapsed)
	}
	_ = readResult

	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestHandleSendInput_ExitedShell(t *testing.T) {
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
	shellID := m["shell_id"].(string)

	time.Sleep(2 * time.Second)

	sendReq := makeRequest(map[string]any{
		"shell_id": shellID,
		"text":     "should fail",
	})
	sendResult, err := s.handleSendInput(context.Background(), sendReq)
	if err != nil {
		t.Fatal(err)
	}
	if !sendResult.IsError {
		t.Fatal("expected error when sending to exited shell")
	}
}

func TestHandlePressKey_UnknownKey(t *testing.T) {
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
	shellID := m["shell_id"].(string)

	keyReq := makeRequest(map[string]any{"shell_id": shellID, "key": "f99"})
	keyResult, err := s.handlePressKey(context.Background(), keyReq)
	if err != nil {
		t.Fatal(err)
	}
	if !keyResult.IsError {
		t.Fatal("expected error for unknown key")
	}

	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
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
	shellID := m["shell_id"].(string)

	for _, timeout := range []float64{-1, 61, 999} {
		req := makeRequest(map[string]any{
			"shell_id": shellID,
			"timeout":  timeout,
		})
		result, _ := s.handleReadOutput(context.Background(), req)
		if !result.IsError {
			t.Fatalf("expected error for timeout %v", timeout)
		}
	}

	// Zero is an explicit non-blocking read and must not require a retry.
	zeroReq := makeRequest(map[string]any{
		"shell_id": shellID,
		"timeout":  0.0,
	})
	zeroResult, _ := s.handleReadOutput(context.Background(), zeroReq)
	if zeroResult.IsError {
		t.Fatalf("timeout=0 should be accepted: %s", zeroResult.Content[0].(mcpgo.TextContent).Text)
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
	shellID := m["shell_id"].(string)

	time.Sleep(300 * time.Millisecond)

	readReq := makeRequest(map[string]any{
		"shell_id": shellID,
		"timeout":  1.0,
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
func TestHandleFileOpsDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SFTP mode semantics differ on Windows")
	}
	s := newTestServer(t)
	startReq := makeRequest(map[string]any{"command": "echo", "mode": "pipe"})
	startResult, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	m := parseResult(t, startResult)
	sessionID := m["session_id"].(string)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Run("chmod", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"session_id":  sessionID,
			"action":      "chmod",
			"remote_path": path,
			"mode":        float64(0600),
		})
		res, err := s.handleFilePerm(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("chmod error: %s", res.Content[0].(mcpgo.TextContent).Text)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("expected 0600, got %o", fi.Mode().Perm())
		}

		noMode := makeRequest(map[string]any{
			"session_id":  sessionID,
			"action":      "chmod",
			"remote_path": path,
		})
		noModeRes, _ := s.handleFilePerm(context.Background(), noMode)
		if !noModeRes.IsError {
			t.Fatal("expected error when mode missing")
		}

		// chmod 000 is a valid operation (strip all permissions) and must be accepted.
		zero := makeRequest(map[string]any{
			"session_id":  sessionID,
			"action":      "chmod",
			"remote_path": path,
			"mode":        float64(0),
		})
		zeroRes, _ := s.handleFilePerm(context.Background(), zero)
		if zeroRes.IsError {
			t.Fatalf("chmod 000 should be accepted: %s", zeroRes.Content[0].(mcpgo.TextContent).Text)
		}
		fi, err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0 {
			t.Fatalf("expected perm 0000, got %o", fi.Mode().Perm())
		}
		// restore perms for later subtests
		rm := makeRequest(map[string]any{
			"session_id":  sessionID,
			"action":      "chmod",
			"remote_path": path,
			"mode":        float64(0644),
		})
		rmRes, _ := s.handleFilePerm(context.Background(), rm)
		if rmRes.IsError {
			t.Fatalf("restore chmod failed: %s", rmRes.Content[0].(mcpgo.TextContent).Text)
		}
	})

	t.Run("realpath", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"session_id":  sessionID,
			"action":      "realpath",
			"remote_path": dir,
		})
		res, err := s.handleFileFsOp(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Fatalf("realpath error: %s", res.Content[0].(mcpgo.TextContent).Text)
		}
		m := parseResult(t, res)
		gotPath, ok := m["canonical_path"].(string)
		if !ok || gotPath == "" {
			t.Fatalf("expected non-empty canonical_path, got %v", m["canonical_path"])
		}
		if !strings.HasSuffix(gotPath, filepath.Base(dir)) {
			t.Fatalf("canonical_path %q should end with %q", gotPath, filepath.Base(dir))
		}
	})

	t.Run("unknown action rejected", func(t *testing.T) {
		calls := map[string]func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error){
			"file_perm": s.handleFilePerm,
			"file_link": s.handleFileLinkOp,
			"file_fs":   s.handleFileFsOp,
		}
		for name, h := range calls {
			req := makeRequest(map[string]any{
				"session_id":  sessionID,
				"action":      "nope",
				"remote_path": path,
			})
			res, _ := h(context.Background(), req)
			if !res.IsError {
				t.Fatalf("%s: expected error", name)
			}
		}
	})

	termReq := makeRequest(map[string]any{"session_id": sessionID, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}
func TestGroupDispatch(t *testing.T) {
	s := newTestServer(t)

	// unknown actions rejected on every unified tool
	dispatchers := map[string]func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error){
		"message":    s.handleMessageOps,
		"history":    s.handleHistoryOps,
		"forward":    s.handleForwardOps,
		"ssh_config": s.handleSSHConfigOps,
	}
	for name, h := range dispatchers {
		req := makeRequest(map[string]any{"action": "bogus"})
		res, _ := h(context.Background(), req)
		if !res.IsError {
			t.Fatalf("%s: expected error for unknown action", name)
		}
	}

	// ssh_config write actions gated behind RegisterSSHConfigWriteTools
	req := makeRequest(map[string]any{"action": "create"})
	res, _ := s.handleSSHConfigOps(context.Background(), req)
	if !res.IsError {
		t.Fatal("expected create to be rejected before RegisterSSHConfigWriteTools")
	}
	s.RegisterSSHConfigWriteTools()
	req = makeRequest(map[string]any{"action": "list"})
	res, err := s.handleSSHConfigOps(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("list should work after upgrade: %s", res.Content[0].(mcpgo.TextContent).Text)
	}

	// forward(action=list) on empty registry
	fwdReq := makeRequest(map[string]any{"action": "list"})
	fwdRes, err := s.handleForwardOps(context.Background(), fwdReq)
	if err != nil {
		t.Fatal(err)
	}
	fm := parseResult(t, fwdRes)
	if _, ok := fm["forwards"].([]any); !ok {
		t.Fatalf("expected forwards array, got %v", fm["forwards"])
	}

	// history(action=list) works without args
	hisReq := makeRequest(map[string]any{"action": "list"})
	hisRes, err := s.handleHistoryOps(context.Background(), hisReq)
	if err != nil {
		t.Fatal(err)
	}
	if hisRes.IsError {
		t.Fatalf("history list error: %s", hisRes.Content[0].(mcpgo.TextContent).Text)
	}

	// message(action=list) needs a session (per-session index)
	startReq := makeRequest(map[string]any{"command": "echo", "mode": "pipe"})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil {
		t.Fatal(err)
	}
	sm := parseResult(t, startRes)
	sid := sm["session_id"].(string)
	msgReq := makeRequest(map[string]any{"action": "list", "session_id": sid})
	msgRes, err := s.handleMessageOps(context.Background(), msgReq)
	if err != nil {
		t.Fatal(err)
	}
	if msgRes.IsError {
		t.Fatalf("message list error: %s", msgRes.Content[0].(mcpgo.TextContent).Text)
	}
	mm := parseResult(t, msgRes)
	if _, ok := mm["messages"].([]any); !ok {
		t.Fatalf("expected messages array, got %v", mm["messages"])
	}
	termReq := makeRequest(map[string]any{"session_id": sid, "force": true})
	s.handleTerminateSession(context.Background(), termReq)
}

func TestDeadSessionOperationsNoPanic(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, nil)
	s := New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil)

	// Persist a session to disk and restore it so it is in the registry without an SSH connection.
	deadSession := api.Session{
		ID:          "dead-sess-1",
		Name:        "dead-session",
		Status:      api.SessionExited,
		SSHEndpoint: "remote",
	}
	if err := store.SaveSessions([]api.Session{deadSession}); err != nil {
		t.Fatal(err)
	}
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatal(err)
	}

	// 1. File write on dead session must return tool error without panic.
	writeReq := makeRequest(map[string]any{
		"session_id":  "dead-sess-1",
		"remote_path": "/tmp/test.txt",
		"data":        "hello",
	})
	writeRes, err := s.handleFileWrite(context.Background(), writeReq)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !writeRes.IsError {
		t.Fatal("expected error result on dead session file write")
	}

	// 2. File read on dead session must return tool error without panic.
	readReq := makeRequest(map[string]any{
		"session_id":  "dead-sess-1",
		"remote_path": "/tmp/test.txt",
	})
	readRes, err := s.handleFileRead(context.Background(), readReq)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !readRes.IsError {
		t.Fatal("expected error result on dead session file read")
	}

	// 3. Local forward on dead session must return tool error without panic.
	fwdReq := makeRequest(map[string]any{
		"session_id":  "dead-sess-1",
		"remote_port": float64(8080),
		"local_port":  float64(8080),
	})
	fwdRes, err := s.handleLocalForward(context.Background(), fwdReq)
	if err != nil {
		t.Fatalf("unexpected handler error: %v", err)
	}
	if !fwdRes.IsError {
		t.Fatal("expected error result on dead session forward")
	}
}

