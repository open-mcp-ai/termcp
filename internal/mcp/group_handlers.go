package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// handleMessageOps is the low-frequency message entry point. The action keeps
// list/get in one tool while the existing handlers retain their behavior.
func (s *Server) handleMessageOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	switch getString(request.GetArguments(), "action", "") {
	case "list":
		return s.handleListMessages(ctx, request)
	case "get":
		return s.handleGetMessage(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be list or get"), nil
	}
}

// handleHistoryOps is the low-frequency archived-session entry point.
func (s *Server) handleHistoryOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	switch getString(request.GetArguments(), "action", "") {
	case "list":
		return s.handleListHistory(ctx, request)
	case "search_messages":
		return s.handleSearchMessages(ctx, request)
	case "rename_session":
		return s.handleRenameSession(ctx, request)
	case "update_session_meta":
		return s.handleUpdateSessionMeta(ctx, request)
	case "purge":
		return s.handlePurgeSession(ctx, request)
	case "screenshot":
		return s.handleScreenshot(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be list, search_messages, rename_session, update_session_meta, purge, or screenshot"), nil
	}
}

// handleForwardOps is the low-frequency port-forward entry point.
func (s *Server) handleForwardOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	switch getString(request.GetArguments(), "action", "") {
	case "local":
		return s.handleLocalForward(ctx, request)
	case "remote":
		return s.handleRemoteForward(ctx, request)
	case "dynamic":
		return s.handleDynamicForward(ctx, request)
	case "list":
		return s.handleListForwards(ctx, request)
	case "close":
		return s.handleCloseForward(ctx, request)
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be local, remote, dynamic, list, or close"), nil
	}
}

// handleSSHConfigOps dispatches profile listing and, when enabled, profile
// management. The write-capable schema is installed by
// RegisterSSHConfigWriteTools; the default schema exposes list only.
func (s *Server) handleSSHConfigOps(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	action := getString(request.GetArguments(), "action", "")
	switch action {
	case "list":
		return s.handleListSSHConfigs(ctx, request)
	case "create", "edit", "copy", "delete":
		if !s.sshConfigWrites {
			return toolError(CodeOperationFailed, "%s", "SSH config write actions are disabled; start termcp with --mcp-manage-ssh-configs"), nil
		}
		switch action {
		case "create":
			return s.handleCreateSSHConfig(ctx, request)
		case "edit":
			return s.handleEditSSHConfig(ctx, request)
		case "copy":
			return s.handleCopySSHConfig(ctx, request)
		default: // delete
			return s.handleDeleteSSHConfig(ctx, request)
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be list, create, edit, copy, or delete"), nil
	}
}
