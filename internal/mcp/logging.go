package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
				// Tool-level failure (returned as an IsError result, err == nil):
				// log at Warn so it is visible at the default log level instead of
				// vanishing into debug-only output.
				exitAttrs = append(exitAttrs, "is_error", true)
				if preview := extractOutputPreview(result); preview != "" {
					exitAttrs = append(exitAttrs, "error", truncate(preview, 300))
				}
				slog.WarnContext(ctx, "tool call end", exitAttrs...)
				return result, err
			}
			if result != nil {
				if preview := extractOutputPreview(result); preview != "" {
					exitAttrs = append(exitAttrs, "output_preview", truncate(preview, 200))
				}
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
	if v, ok := args["shell_id"].(string); ok && v != "" {
		attrs = append(attrs, "shell_id", v)
	}
	if v, ok := args["key"].(string); ok && v != "" {
		attrs = append(attrs, "key", v)
	}
	if v, ok := args["reader_id"].(float64); ok {
		attrs = append(attrs, "reader_id", int64(v))
	}
	if v, ok := args["args"].([]any); ok && len(v) > 0 {
		if len(v) <= 10 {
			b, _ := json.Marshal(v)
			attrs = append(attrs, "args", string(b))
		} else {
			preview := make([]any, 10)
			copy(preview, v[:10])
			b, _ := json.Marshal(preview)
			attrs = append(attrs, "args", fmt.Sprintf("%s... (%d items total)", string(b), len(v)))
		}
	}
	if v, ok := args["command"].(string); ok && v != "" {
		attrs = append(attrs, "command", v)
	}
	if v, ok := args["mode"].(string); ok && v != "" {
		attrs = append(attrs, "mode", v)
	}
	if v, ok := args["rows"].(float64); ok {
		attrs = append(attrs, "rows", int64(v))
	}
	if v, ok := args["cols"].(float64); ok {
		attrs = append(attrs, "cols", int64(v))
	}
	if v, ok := args["timeout"].(float64); ok {
		attrs = append(attrs, "timeout", v)
	}
	if v, ok := args["repeat"].(float64); ok {
		attrs = append(attrs, "repeat", int64(v))
	}
	if v, ok := args["force"].(bool); ok {
		attrs = append(attrs, "force", v)
	}
	if v, ok := args["grace_period"].(float64); ok {
		attrs = append(attrs, "grace_period", int64(v))
	}
	if v, ok := args["text"].(string); ok && v != "" {
		attrs = append(attrs, "text", truncate(v, 200))
	}
	return attrs
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func extractOutputPreview(result *mcpgo.CallToolResult) string {
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return ""
}
