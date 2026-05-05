package mcp

import (
	"context"
	"log/slog"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type toolHandler = func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

func withLogging(name string, h toolHandler) toolHandler {
	return func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := time.Now()
		baseAttrs := []any{"tool", name}
		baseAttrs = appendSafeArgs(baseAttrs, request)

		slog.DebugContext(ctx, "tool call start", baseAttrs...)
		result, err := h(ctx, request)
		durMs := time.Since(start).Milliseconds()

		exitAttrs := append([]any{}, baseAttrs...)
		exitAttrs = append(exitAttrs, "duration_ms", durMs)

		if err != nil {
			exitAttrs = append(exitAttrs, "err", err)
			slog.ErrorContext(ctx, "tool call end", exitAttrs...)
		} else {
			if result != nil && result.IsError {
				exitAttrs = append(exitAttrs, "is_error", true)
			}
			slog.DebugContext(ctx, "tool call end", exitAttrs...)
		}
		return result, err
	}
}

func appendSafeArgs(attrs []any, request mcpgo.CallToolRequest) []any {
	args := request.GetArguments()
	if args == nil {
		return attrs
	}
	if v, ok := args["session_id"].(string); ok && v != "" {
		attrs = append(attrs, "session_id", v)
	}
	if v, ok := args["reader_id"].(float64); ok {
		attrs = append(attrs, "reader_id", int64(v))
	}
	return attrs
}
