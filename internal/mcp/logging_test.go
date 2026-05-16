package mcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *captureHandler) Level() slog.Level                            { return slog.LevelDebug } // required by Go 1.24+ slog optimization
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(_ string) slog.Handler      { return c }

func (c *captureHandler) snapshot() []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slog.Record, len(c.records))
	copy(out, c.records)
	return out
}

func withCapturedLogger(t *testing.T) *captureHandler {
	t.Helper()
	prev := slog.Default()
	cap := &captureHandler{}
	logger := slog.New(cap)
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return cap
}

func attrValue(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value
			found = true
			return false
		}
		return true
	})
	return v, found
}

func TestWithLogging_PassesThroughResultAndError(t *testing.T) {
	ctx := context.Background()
	wantResult := mcpgo.NewToolResultText("payload")
	wantErr := errors.New("boom")

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return wantResult, wantErr
	}

	wrapped := withLogging("test_tool", h)
	gotResult, gotErr := wrapped(ctx, mcpgo.CallToolRequest{})

	if gotResult != wantResult {
		t.Fatalf("result not preserved: got %v, want %v", gotResult, wantResult)
	}
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error not preserved: got %v, want %v", gotErr, wantErr)
	}
}

func TestWithLogging_LogsDebugEntryAndExit(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records (entry+exit), got %d", len(records))
	}

	entry := records[0]
	if entry.Level != slog.LevelDebug {
		t.Fatalf("entry record: expected Debug level, got %v", entry.Level)
	}
	if v, ok := attrValue(entry, "tool"); !ok || v.String() != "test_tool" {
		t.Fatalf("entry record: expected tool=test_tool, got %v (found=%v)", v, ok)
	}

	exit := records[len(records)-1]
	if exit.Level != slog.LevelDebug {
		t.Fatalf("exit record: expected Debug level, got %v", exit.Level)
	}
	if v, ok := attrValue(exit, "tool"); !ok || v.String() != "test_tool" {
		t.Fatalf("exit record: expected tool=test_tool, got %v (found=%v)", v, ok)
	}
	if _, ok := attrValue(exit, "duration_ms"); !ok {
		t.Fatal("exit record: expected duration_ms attr")
	}
}

func TestWithLogging_LogsErrorOnGoError(t *testing.T) {
	cap := withCapturedLogger(t)

	wantErr := errors.New("kaboom")
	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return nil, wantErr
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); !errors.Is(err, wantErr) {
		t.Fatalf("expected wantErr passthrough, got %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records, got %d", len(records))
	}
	exit := records[len(records)-1]
	if exit.Level != slog.LevelError {
		t.Fatalf("exit record: expected Error level on Go error, got %v", exit.Level)
	}
	v, ok := attrValue(exit, "err")
	if !ok {
		t.Fatal("exit record: expected err attr on Go error")
	}
	if v.String() != "kaboom" {
		t.Fatalf("exit record: expected err=kaboom, got %q", v.String())
	}
}

func TestWithLogging_LogsDebugOnIsErrorResult(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultError("invalid arg"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}

	records := cap.snapshot()
	if len(records) < 2 {
		t.Fatalf("expected at least 2 records, got %d", len(records))
	}
	exit := records[len(records)-1]
	if exit.Level != slog.LevelDebug {
		t.Fatalf("exit record: expected Debug level on IsError result, got %v", exit.Level)
	}
	v, ok := attrValue(exit, "is_error")
	if !ok {
		t.Fatal("exit record: expected is_error attr on IsError result")
	}
	if !v.Bool() {
		t.Fatalf("exit record: expected is_error=true, got %v", v)
	}
}

func TestWithLogging_ExtractsSessionAndReaderIDs(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc-123",
		"reader_id":  float64(7),
		"text":       "ignored",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "session_id")
	if !ok {
		t.Fatal("entry: expected session_id attr")
	}
	if v.String() != "abc-123" {
		t.Fatalf("entry: expected session_id=abc-123, got %q", v.String())
	}

	v, ok = attrValue(entry, "reader_id")
	if !ok {
		t.Fatal("entry: expected reader_id attr")
	}
	if v.Int64() != 7 {
		t.Fatalf("entry: expected reader_id=7, got %v", v)
	}
}

func TestWithLogging_OmitsAbsentSessionAndReaderIDs(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("test_tool", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]
	if _, ok := attrValue(entry, "session_id"); ok {
		t.Fatal("entry: session_id attr should be absent when not in args")
	}
	if _, ok := attrValue(entry, "reader_id"); ok {
		t.Fatal("entry: reader_id attr should be absent when not in args")
	}
}

func TestWithLogging_ExtractsTextParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("send_input", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc-123",
		"text":       "echo hello world",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "text")
	if !ok {
		t.Fatal("entry: expected text attr")
	}
	if v.String() != "echo hello world" {
		t.Fatalf("entry: expected text='echo hello world', got %q", v.String())
	}
}

func TestWithLogging_ExtractsCommandParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("start_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "ssh",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "command")
	if !ok {
		t.Fatal("entry: expected command attr")
	}
	if v.String() != "ssh" {
		t.Fatalf("entry: expected command=ssh, got %q", v.String())
	}
}

func TestWithLogging_ExtractsModeParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("start_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"mode":       "pipe",
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "mode")
	if !ok {
		t.Fatal("entry: expected mode attr")
	}
	if v.String() != "pipe" {
		t.Fatalf("entry: expected mode=pipe, got %q", v.String())
	}
}

func TestWithLogging_ExtractsRowsAndCols(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("start_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"rows":       float64(40),
		"cols":       float64(120),
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	for _, key := range []string{"rows", "cols"} {
		v, ok := attrValue(entry, key)
		if !ok {
			t.Fatalf("entry: expected %s attr", key)
		}
		if v.Int64() <= 0 {
			t.Fatalf("entry: expected %s > 0, got %v", key, v)
		}
	}
}

func TestWithLogging_ExtractsTimeoutAndPressEnter(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("send_and_read", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id":  "abc",
		"timeout":     float64(3.5),
		"press_enter": true,
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	if v, ok := attrValue(entry, "timeout"); !ok {
		t.Fatal("entry: expected timeout attr")
	} else if v.Float64() != 3.5 {
		t.Fatalf("entry: expected timeout=3.5, got %v", v)
	}

	if v, ok := attrValue(entry, "press_enter"); !ok {
		t.Fatal("entry: expected press_enter attr")
	} else if !v.Bool() {
		t.Fatal("entry: expected press_enter=true")
	}
}

func TestWithLogging_ExtractsForceAndGracePeriod(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("terminate_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id":   "abc",
		"force":        true,
		"grace_period": float64(3),
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]

	if v, ok := attrValue(entry, "force"); !ok {
		t.Fatal("entry: expected force attr")
	} else if !v.Bool() {
		t.Fatal("entry: expected force=true")
	}

	if v, ok := attrValue(entry, "grace_period"); !ok {
		t.Fatal("entry: expected grace_period attr")
	} else if v.Int64() != 3 {
		t.Fatalf("entry: expected grace_period=3, got %v", v)
	}
}

func TestWithLogging_ExtractsArgsParam(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("start_session", h)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "ping",
		"args":       []any{"-c", "5", "google.com"},
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "args")
	if !ok {
		t.Fatal("entry: expected args attr")
	}
	if v.String() != `["-c","5","google.com"]` {
		t.Fatalf("entry: expected args JSON, got %q", v.String())
	}
}

func TestWithLogging_TruncatesArgsOver10(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("start_session", h)

	raw := make([]any, 15)
	for i := range raw {
		raw[i] = "arg"
	}

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"command":    "cmd",
		"args":       raw,
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	entry := records[0]
	v, ok := attrValue(entry, "args")
	if !ok {
		t.Fatal("entry: expected args attr")
	}
	// Should contain "... (15 items total)"
	if !strings.Contains(v.String(), "15 items total") {
		t.Fatalf("entry: expected truncated args with item count, got %q", v.String())
	}
}

func TestWithLogging_LogsOutputPreviewOnExit(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("hello from process"), nil
	}
	wrapped := withLogging("read_output", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	exit := records[len(records)-1]
	v, ok := attrValue(exit, "output_preview")
	if !ok {
		t.Fatal("exit: expected output_preview attr")
	}
	if v.String() != "hello from process" {
		t.Fatalf("exit: expected output_preview='hello from process', got %q", v.String())
	}
}

func TestWithLogging_TruncatesLongOutputPreview(t *testing.T) {
	cap := withCapturedLogger(t)

	longOutput := strings.Repeat("y", 250)
	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText(longOutput), nil
	}
	wrapped := withLogging("read_output", h)

	if _, err := wrapped(context.Background(), mcpgo.CallToolRequest{}); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	exit := records[len(records)-1]
	v, ok := attrValue(exit, "output_preview")
	if !ok {
		t.Fatal("exit: expected output_preview attr")
	}
	logged := v.String()
	if len(logged) > 203 || len(logged) < 198 {
		t.Fatalf("expected truncated length ~200, got %d: %q", len(logged), logged)
	}
	if !strings.HasSuffix(logged, "...") {
		t.Fatalf("expected truncated output ending with '...', got %q", logged)
	}
}

func TestWithLogging_TruncatesLongText(t *testing.T) {
	cap := withCapturedLogger(t)

	h := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	wrapped := withLogging("send_input", h)

	longText := strings.Repeat("x", 250)

	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"session_id": "abc",
		"text":       longText,
	}

	if _, err := wrapped(context.Background(), req); err != nil {
		t.Fatalf("wrapped returned error: %v", err)
	}

	records := cap.snapshot()
	if len(records) < 1 {
		t.Fatal("expected entry record")
	}
	entry := records[0]

	v, ok := attrValue(entry, "text")
	if !ok {
		t.Fatal("entry: expected text attr")
	}
	logged := v.String()
	if len(logged) > 203 || len(logged) < 198 {
		t.Fatalf("expected truncated length ~200, got %d: %q", len(logged), logged)
	}
	if !strings.HasSuffix(logged, "...") {
		t.Fatalf("expected truncated text ending with '...', got %q", logged)
	}
}
