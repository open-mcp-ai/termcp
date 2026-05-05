package mcp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
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
	slog.SetDefault(slog.New(cap))
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
