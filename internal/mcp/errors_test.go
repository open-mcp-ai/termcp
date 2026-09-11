package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

func decodeToolError(t *testing.T, result *mcpgo.CallToolResult) (code, message string) {
	t.Helper()
	if result == nil || !result.IsError {
		t.Fatalf("expected an error result, got %+v", result)
	}
	if len(result.Content) != 1 {
		t.Fatalf("expected exactly one content block, got %d", len(result.Content))
	}
	tc, ok := result.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", result.Content[0])
	}
	var payload struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(tc.Text), &payload); err != nil {
		t.Fatalf("error content is not JSON: %v (%q)", err, tc.Text)
	}
	return payload.ErrorCode, payload.Error
}

func TestToolError_CarriesDedicatedCodeField(t *testing.T) {
	result := toolError(CodeShellNotFound, "Shell '%s' not found", "abc-123")
	code, msg := decodeToolError(t, result)

	if code != "shell_not_found" {
		t.Fatalf("expected code shell_not_found, got %q", code)
	}
	if msg != "Shell 'abc-123' not found" {
		t.Fatalf("unexpected message %q", msg)
	}
}

func TestToolError_NoFormatArgs(t *testing.T) {
	code, msg := decodeToolError(t, toolError(CodeInvalidArgument, "shell_id is required"))
	if code != "invalid_argument" || msg != "shell_id is required" {
		t.Fatalf("got (%q, %q)", code, msg)
	}
}

func TestExtractErrorInfo_CodedAndPlain(t *testing.T) {
	code, msg := extractErrorInfo(toolError(CodeSessionNotFound, "Session '%s' not found", "s1"))
	if code != CodeSessionNotFound || msg != "Session 's1' not found" {
		t.Fatalf("coded extraction = (%q, %q)", code, msg)
	}

	// Third-party / plain-text errors must still surface their text.
	code, msg = extractErrorInfo(mcpgo.NewToolResultError("invalid arg"))
	if code != "" || msg != "invalid arg" {
		t.Fatalf("plain extraction = (%q, %q)", code, msg)
	}
}

func TestWithLogging_LogsErrorCodeOnCodedFailure(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return toolError(CodeShellNotFound, "Shell '%s' not found", "12d3e2f8-a15"), nil
	}
	wrapped := withLogging("shell_output", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected entry+exit records, got %d", len(records))
	}
	exit := records[len(records)-1]
	if exit.Level != slog.LevelWarn {
		t.Fatalf("expected Warn level, got %v", exit.Level)
	}
	if v, ok := attrValue(exit, "error_code"); !ok || v.String() != "shell_not_found" {
		t.Fatalf("expected error_code=shell_not_found, got %v (found=%v)", v, ok)
	}
	if v, ok := attrValue(exit, "error"); !ok || v.String() != "Shell '12d3e2f8-a15' not found" {
		t.Fatalf("expected human message in error attr, got %v (found=%v)", v, ok)
	}
}

// End-to-end: shell_output on an unknown shell id returns the dedicated
// shell_not_found code instead of only prose (the reported WARN log).
func TestHandleReadOutput_UnknownShellReturnsCode(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)
	res, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
		"shell_id": "12d3e2f8-a15",
	}))
	if err != nil {
		t.Fatalf("unexpected go error: %v", err)
	}
	code, msg := decodeToolError(t, res)
	if code != CodeShellNotFound {
		t.Fatalf("expected %s, got %q (%s)", CodeShellNotFound, code, msg)
	}
}
