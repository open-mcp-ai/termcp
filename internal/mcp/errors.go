package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// sshConfigErrCode maps an ssh_config store failure onto its tool error code.
func sshConfigErrCode(err error) string {
	if errors.Is(err, sshconfig.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return CodeSSHConfigNotFound
	}
	return CodeOperationFailed
}

// forwardErrCode maps a forward-manager failure onto its tool error code.
func forwardErrCode(err error) string {
	if errors.Is(err, forward.ErrNotFound) {
		return CodeForwardNotFound
	}
	return CodeOperationFailed
}

// Tool error codes. Like a function's (value, error) return, a failed tool
// result carries a dedicated machine-readable status so callers can branch on
// the failure kind instead of string-matching the message. Codes are stable
// snake_case identifiers; success results never carry one.
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeSessionNotFound     = "session_not_found"
	CodeShellNotFound       = "shell_not_found"
	CodeSessionNotRunning   = "session_not_running"
	CodeReaderNotRegistered = "reader_not_registered"
	CodeHistoryNotFound     = "history_not_found"
	CodeForwardNotFound     = "forward_not_found"
	CodeSSHConfigNotFound   = "ssh_config_not_found"
	CodeRuleNotFound        = "rule_not_found"
	CodeConflict            = "conflict"
	CodeNotConfigured       = "not_configured"
	CodeConnectionFailed    = "connection_failed"
	CodeOperationFailed     = "operation_failed"
	CodeInternalError       = "internal_error"
)

// toolError builds a failed tool result whose text content is a JSON object
// with a dedicated error_code field plus a human-readable message:
//
//	{"error_code":"shell_not_found","error":"Shell 'abc' not found"}
//
// The dedicated field lets agents branch on the failure kind (the moral
// equivalent of a typed error) without parsing prose. Only failed results
// carry it; success results keep their normal payload.
func toolError(code, format string, args ...any) *mcpgo.CallToolResult {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	payload, err := json.Marshal(struct {
		ErrorCode string `json:"error_code"`
		Error     string `json:"error"`
	}{ErrorCode: code, Error: msg})
	if err != nil {
		// json.Marshal of two strings cannot fail; keep a safe fallback.
		payload = []byte(fmt.Sprintf(`{"error_code":%q,"error":"failed to encode tool error"}`, CodeInternalError))
	}
	return &mcpgo.CallToolResult{
		IsError: true,
		Content: []mcpgo.Content{
			mcpgo.NewTextContent(string(payload)),
		},
	}
}
