package mcp

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func startTestSession(t *testing.T, s *Server) (sessionID, shellID string) {
	t.Helper()
	res, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command": testShell(),
		"args":    testShellEchoArgs("notify_ok"),
		"mode":    "pipe",
	}))
	if err != nil || res.IsError {
		t.Fatalf("start session failed: %v %+v", err, res)
	}
	m := parseResult(t, res)
	return m["session_id"].(string), m["shell_id"].(string)
}

func TestShellNotify_RegisterListUnregister(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	_, shellID := startTestSession(t, s)

	reg, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":   "register",
		"shell_id": shellID,
		"channel":  "resource",
		"event":    "exit",
	}))
	if err != nil || reg.IsError {
		t.Fatalf("register failed: %v %+v", err, reg)
	}
	rm := parseResult(t, reg)
	ruleID, _ := rm["rule_id"].(string)
	if ruleID == "" || rm["channel"] != "resource" || rm["event"] != "exit" {
		t.Fatalf("unexpected register result: %#v", rm)
	}

	lst, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":   "list",
		"shell_id": shellID,
	}))
	if err != nil || lst.IsError {
		t.Fatalf("list failed: %v %+v", err, lst)
	}
	rules, _ := parseResult(t, lst)["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}

	unreg, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":  "unregister",
		"rule_id": ruleID,
	}))
	if err != nil || unreg.IsError {
		t.Fatalf("unregister failed: %v %+v", err, unreg)
	}

	// Second unregister must report the dedicated rule_not_found code.
	again, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":  "unregister",
		"rule_id": ruleID,
	}))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if code, _ := decodeToolError(t, again); code != CodeRuleNotFound {
		t.Fatalf("expected %s, got %q", CodeRuleNotFound, code)
	}
}

func TestShellNotify_DefaultEventIsOutput(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	_, shellID := startTestSession(t, s)

	res, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":   "register",
		"shell_id": shellID,
		"channel":  "resource",
	}))
	if err != nil || res.IsError {
		t.Fatalf("register failed: %v %+v", err, res)
	}
	if got := parseResult(t, res)["event"]; got != "output" {
		t.Fatalf("default event must be output (per spec), got %v", got)
	}
}

func TestShellNotify_UnknownShellReturnsCode(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	res, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":   "register",
		"shell_id": "12d3e2f8-a15",
		"channel":  "resource",
	}))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	if code, _ := decodeToolError(t, res); code != CodeShellNotFound {
		t.Fatalf("expected %s, got %q", CodeShellNotFound, code)
	}
}

func TestShellNotify_InvalidChannelAndEvent(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	_, shellID := startTestSession(t, s)

	for _, args := range []map[string]any{
		{"action": "register", "shell_id": shellID, "channel": "bogus"},
		{"action": "register", "shell_id": shellID, "channel": "resource", "event": "bogus"},
		{"action": "bogus"},
	} {
		res, err := s.handleShellNotifyOps(context.Background(), makeRequest(args))
		if err != nil {
			t.Fatalf("unexpected go error: %v", err)
		}
		if code, _ := decodeToolError(t, res); code != CodeInvalidArgument {
			t.Fatalf("args %v: expected %s, got %q", args, CodeInvalidArgument, code)
		}
	}
}

// Closing a shell must cascade-clear its notification rules (session hook).
func TestShellNotify_RulesClearedWhenShellClosed(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	_, shellID := startTestSession(t, s)

	if _, err := s.handleShellNotifyOps(context.Background(), makeRequest(map[string]any{
		"action":   "register",
		"shell_id": shellID,
		"channel":  "resource",
		"event":    "output",
	})); err != nil {
		t.Fatal(err)
	}
	if len(s.notifyMgr.List(shellID)) != 1 {
		t.Fatalf("expected 1 rule before close")
	}

	if _, err := s.handleCloseShell(context.Background(), makeRequest(map[string]any{
		"shell_id": shellID,
	})); err != nil {
		t.Fatal(err)
	}
	// removeChildShell fires the close hook synchronously; give it a beat.
	deadline := time.Now().Add(2 * time.Second)
	cleared := false
	for time.Now().Before(deadline) {
		if len(s.notifyMgr.List(shellID)) == 0 {
			cleared = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cleared {
		t.Fatalf("expected rules cleared after shell close, still %d", len(s.notifyMgr.List(shellID)))
	}
	// Let the shell's async exit watcher / message writer settle before the
	// test's TempDir cleanup runs (Windows unlink fails on open handles).
	time.Sleep(200 * time.Millisecond)
}

type mockSamplingHandler struct {
	mu    sync.Mutex
	calls []string
}

func (h *mockSamplingHandler) CreateMessage(_ context.Context, req mcpgo.CreateMessageRequest) (*mcpgo.CreateMessageResult, error) {
	h.mu.Lock()
	h.calls = append(h.calls, req.CreateMessageParams.SystemPrompt)
	h.mu.Unlock()
	return &mcpgo.CreateMessageResult{
		SamplingMessage: mcpgo.SamplingMessage{Role: mcpgo.RoleAssistant, Content: mcpgo.NewTextContent("ok")},
		Model:           "mock-model",
		StopReason:      "endTurn",
	}, nil
}

func (h *mockSamplingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func callReq(name string, args map[string]any) mcpgo.CallToolRequest {
	return mcpgo.CallToolRequest{Params: mcpgo.CallToolParams{Name: name, Arguments: args}}
}

// Channel B end-to-end: a rule registered over the MCP transport must deliver a
// sampling/createMessage request to the client that registered it, even though
// dispatch happens from a timer/goroutine with no client session in its context.
func TestShellNotify_SamplingDispatchToRegisteringClient(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	handler := &mockSamplingHandler{}
	cli, err := client.NewInProcessClientWithSamplingHandler(s.mcpServer, handler)
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "notify-test", Version: "1.0.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	startRes, err := cli.CallTool(ctx, callReq("session_start", map[string]any{
		"command": testShell(),
		"args":    testShellEchoArgs("notify_ok"),
		"mode":    "pipe",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("session_start failed: %v %+v", err, startRes)
	}
	shellID := parseResult(t, startRes)["shell_id"].(string)

	regRes, err := cli.CallTool(ctx, callReq("shell_notify", map[string]any{
		"action":   "register",
		"shell_id": shellID,
		"channel":  "sampling",
		"event":    "output",
	}))
	if err != nil || regRes.IsError {
		t.Fatalf("shell_notify register failed: %v %+v", err, regRes)
	}

	// Drive the terminal-output hook the session reader would normally call.
	s.notifyMgr.OnOutput(shellID)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && handler.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if handler.count() == 0 {
		t.Fatal("expected a sampling/createMessage notification to the registering client")
	}
	handler.mu.Lock()
	gotPrompt := handler.calls[0]
	handler.mu.Unlock()
	if gotPrompt != "termcp notification daemon" {
		t.Fatalf("systemPrompt = %q, want %q", gotPrompt, "termcp notification daemon")
	}
}

// Channel A end-to-end over the Streamable HTTP transport: a resource rule must
// broadcast notifications/resources/updated with uri termcp://shells/<id>.
func TestShellNotify_ResourceDispatchEndToEnd(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	ts := httptest.NewServer(s.StreamableHTTPHandler())
	defer ts.Close()

	cli, err := client.NewStreamableHttpClient(ts.URL, transport.WithContinuousListening())
	if err != nil {
		t.Fatalf("streamable client: %v", err)
	}
	defer cli.Close()

	ctx := context.Background()
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	initReq := mcpgo.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcpgo.Implementation{Name: "notify-test", Version: "1.0.0"}
	if _, err := cli.Initialize(ctx, initReq); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	uriCh := make(chan string, 4)
	cli.OnNotification(func(n mcpgo.JSONRPCNotification) {
		if n.Method != "notifications/resources/updated" {
			return
		}
		uri, _ := n.Params.AdditionalFields["uri"].(string)
		select {
		case uriCh <- uri:
		default:
		}
	})

	startRes, err := cli.CallTool(ctx, callReq("session_start", map[string]any{
		"command": testShell(),
		"args":    testShellEchoArgs("notify_ok"),
		"mode":    "pipe",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("session_start failed: %v %+v", err, startRes)
	}
	shellID := parseResult(t, startRes)["shell_id"].(string)

	regRes, err := cli.CallTool(ctx, callReq("shell_notify", map[string]any{
		"action":   "register",
		"shell_id": shellID,
		"channel":  "resource",
		"event":    "output",
	}))
	if err != nil || regRes.IsError {
		t.Fatalf("shell_notify register failed: %v %+v", err, regRes)
	}

	s.notifyMgr.OnOutput(shellID)

	select {
	case uri := <-uriCh:
		if want := "termcp://shells/" + shellID; uri != want {
			t.Fatalf("resource uri = %q, want %q", uri, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected notifications/resources/updated to reach the client")
	}
}
