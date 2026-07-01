package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/internal/encoding"
)

func getString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func getBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func getFloat64(args map[string]any, key string, def float64) float64 {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		case int64:
			return float64(n)
		}
	}
	return def
}

func validateStartParams(args map[string]any) (*mcpgo.CallToolResult, error) {
	mode := strings.TrimSpace(getString(args, "mode", "pty"))
	if mode == "" {
		mode = "pty"
	}
	if mode != "pty" && mode != "pipe" {
		return mcpgo.NewToolResultError(fmt.Sprintf("mode must be 'pty' or 'pipe', got %q", mode)), nil
	}
	rows := int(getFloat64(args, "rows", 24))
	if rows < 1 || rows > 1000 {
		return mcpgo.NewToolResultError(fmt.Sprintf("rows must be between 1 and 1000, got %d", rows)), nil
	}
	cols := int(getFloat64(args, "cols", 80))
	if cols < 1 || cols > 1000 {
		return mcpgo.NewToolResultError(fmt.Sprintf("cols must be between 1 and 1000, got %d", cols)), nil
	}
	return nil, nil
}

func jsonResult(data map[string]any) *mcpgo.CallToolResult {
	b, _ := json.Marshal(data)
	return mcpgo.NewToolResultText(string(b))
}

func successResult() *mcpgo.CallToolResult {
	return mcpgo.NewToolResultText(`{"success":true}`)
}

// filterRunning returns only sessions whose Status is SessionRunning.
func filterRunning(in []api.Session) []api.Session {
	out := make([]api.Session, 0, len(in))
	for _, s := range in {
		if s.Status == api.SessionRunning {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) requireSession(sessionID string) (*session.Session, *mcpgo.CallToolResult) {
	sess := s.sessMgr.Get(sessionID)
	if sess == nil {
		return nil, mcpgo.NewToolResultError(fmt.Sprintf("Session '%s' not found", sessionID))
	}
	return sess, nil
}

// requireTerminalShell looks up a session or child shell for terminal I/O operations.
func (s *Server) requireTerminalShell(sessionID string) (session.TerminalShell, *mcpgo.CallToolResult) {
	if sess := s.sessMgr.Get(sessionID); sess != nil {
		return sess, nil
	}
	if cs := s.sessMgr.GetChildShell(sessionID); cs != nil {
		return cs, nil
	}
	return nil, mcpgo.NewToolResultError(fmt.Sprintf("Shell '%s' not found", sessionID))
}

func getStringSlice(args map[string]any, key string) []string {
	if v, ok := args[key]; ok {
		if arr, ok := v.([]any); ok {
			var result []string
			for _, item := range arr {
				if s, ok := item.(string); ok {
					result = append(result, s)
				}
			}
			return result
		}
	}
	return nil
}


// resolveSSHFromArgs returns the ssh_config name, loaded entry, and remote dial settings (nil Remote = built-in loopback).
func (s *Server) resolveSSHFromArgs(args map[string]any) (string, *sshconfig.Entry, *session.RemoteSSH, error) {
	if s.sshConfigs == nil {
		return "", nil, nil, fmt.Errorf("ssh config store not configured")
	}
	name := strings.TrimSpace(getString(args, "ssh_config", ""))
	if name == "" {
		name = "internal"
	}
	ent, err := s.sshConfigs.Load(name)
	if err != nil {
		return "", nil, nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		return name, ent, nil, nil
	}
	r, err := sshconfig.RemoteFromEntry(ent, s.sshConfigs.ConfigDir(name))
	if err != nil {
		return "", nil, nil, err
	}
	return name, ent, r, nil
}

func (s *Server) handleStartSession(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	command := getString(args, "command", "")
	toolArgs := getStringSlice(args, "args")
	if strings.TrimSpace(command) == "" && len(toolArgs) > 0 {
		return mcpgo.NewToolResultError("command is required when args are provided"), nil
	}
	if bad, _ := validateStartParams(args); bad != nil {
		return bad, nil
	}

	cfgName, ent, remote, err := s.resolveSSHFromArgs(args)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	cmd, execArgs := sshconfig.EffectiveCommand(ent, command, toolArgs)
	if strings.TrimSpace(cmd) == "" && len(execArgs) > 0 {
		return mcpgo.NewToolResultError("command is required when args are provided"), nil
	}

	mode := sshconfig.EffectiveMode(ent, getString(args, "mode", ""))

	sessName := strings.TrimSpace(getString(args, "name", ""))
	if sessName == "" {
		sessName = cfgName
	}

	sess, err := s.sessMgr.Create(session.Config{
		Command: cmd,
		Args:    execArgs,
		Mode:    api.SessionMode(mode),
		Name:    sessName,
		Rows:    int(getFloat64(args, "rows", 24)),
		Cols:    int(getFloat64(args, "cols", 80)),
		Remote:  remote,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	time.Sleep(100 * time.Millisecond)

	result := map[string]any{
		"session_id":     sess.ID,
		"pid":            sess.PID,
		"ssh_config":     cfgName,
		"initial_output": "",
	}
	return jsonResult(result), nil
}

func (s *Server) handleSendInput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	text := getString(args, "text", "")
	pressEnter := getBool(args, "press_enter", false)

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.SendTerminalBytes([]byte(text), pressEnter); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleStartSubShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "parent_session_id", "")
	name := getString(args, "name", "")
	command := getString(args, "command", "")
	mode := strings.TrimSpace(getString(args, "mode", "pty"))
	rows := int(getFloat64(args, "rows", 24))
	cols := int(getFloat64(args, "cols", 80))

	sess, bad := s.requireSession(parentID)
	if bad != nil {
		return bad, nil
	}
	cs, err := sess.CreateChildShell(command, nil, mode == "pty", rows, cols, name)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"session_id": cs.ID, "parent_session_id": parentID, "name": cs.Name}), nil
}

func (s *Server) handleListSubshells(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "parent_session_id", "")

	sess, bad := s.requireSession(parentID)
	if bad != nil {
		return bad, nil
	}
	all := sess.ListChildShells()
	return jsonResult(map[string]any{"parent_session_id": parentID, "subshells": filterRunning(all)}), nil
}

// handleCloseShell closes a single shell channel without tearing down the parent session.
// For a parent session id: closes the root shell channel only (remote) / no-op (internal);
// the SSH connection and other child shells keep running. For a child shell id: closes
// just that channel. Use terminate_session to fully stop a session.
func (s *Server) handleCloseShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "session_id", "")

	if sess := s.sessMgr.Get(shellID); sess != nil {
		sess.TerminateShellOnly()
		s.sessMgr.NotifyChange()
		return successResult(), nil
	}
	found, err := s.sessMgr.CloseChildShell(shellID)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	if !found {
		return mcpgo.NewToolResultError(fmt.Sprintf("Shell '%s' not found", shellID)), nil
	}
	// CloseChildShell already triggers onChildChange → notifyListChange.
	return successResult(), nil
}

func (s *Server) handleReadOutput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	stripAnsi := getBool(args, "strip_ansi", true)
	timeout := getFloat64(args, "timeout", 5.0)
	if timeout < 0.1 || timeout > 60 {
		return mcpgo.NewToolResultError(fmt.Sprintf("timeout must be between 0.1 and 60, got %v", timeout)), nil
	}
	maxLines := int(getFloat64(args, "max_lines", 0)); maxBytes := int(getFloat64(args, "max_bytes", 0))
	readerID := int(getFloat64(args, "reader_id", 0))

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	output, err := shell.ReadTerminalStream(ctx, readerID, time.Duration(timeout*float64(time.Second)), stripAnsi, maxLines, maxBytes)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	info := shell.Info()
		result := map[string]any{
			"output":                output,
			"has_more":              shell.HasMoreOutput(readerID),
			"lines_returned":        strings.Count(output, "\n"),
			"bytes_returned":        len(output),
			"session_status":        string(info.Status),
			"session_uptime_seconds": int(time.Since(info.CreatedAt).Seconds()),
		}
	return jsonResult(result), nil
}

func (s *Server) handleSendAndRead(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	text := getString(args, "text", "")
	pressEnter := getBool(args, "press_enter", false)
	stripAnsi := getBool(args, "strip_ansi", true)
	timeout := getFloat64(args, "timeout", 5.0)
	if timeout < 0.1 || timeout > 60 {
		return mcpgo.NewToolResultError(fmt.Sprintf("timeout must be between 0.1 and 60, got %v", timeout)), nil
	}
	maxLines := int(getFloat64(args, "max_lines", 0)); maxBytes := int(getFloat64(args, "max_bytes", 0))
	readerID := int(getFloat64(args, "reader_id", 0))

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.SendTerminalBytes([]byte(text), pressEnter); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	output, err := shell.ReadTerminalStream(ctx, readerID, time.Duration(timeout*float64(time.Second)), stripAnsi, maxLines, maxBytes)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{
		"output":         output,
		"has_more":       shell.HasMoreOutput(readerID),
		"lines_returned": strings.Count(output, "\n"),
		"bytes_returned": len(output),
	}
	return jsonResult(result), nil
}

func (s *Server) handleListSessions(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	all := s.sessMgr.ListAll()
	return jsonResult(map[string]any{"sessions": filterRunning(all)}), nil
}

func (s *Server) handleGetSessionInfo(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	sess, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	info := sess.Info()
	data, _ := json.Marshal(info)
	return mcpgo.NewToolResultText(string(data)), nil
}

func (s *Server) handleTerminateSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	force := getBool(args, "force", false)
	gracePeriod := getFloat64(args, "grace_period", 5.0)
	if gracePeriod < 0 || gracePeriod > 60 {
		return mcpgo.NewToolResultError(fmt.Sprintf("grace_period must be between 0 and 60, got %v", gracePeriod)), nil
	}

	_, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	s.sessMgr.Terminate(sessionID, force, time.Duration(gracePeriod*float64(time.Second)))
	if sess := s.sessMgr.Get(sessionID); sess != nil {
		sess.Disconnect()
	}
	return successResult(), nil
}

func (s *Server) handleDeleteSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id is required"), nil
	}

	// Terminate + disconnect first (keep-alive sessions won't delete while "running").
	s.sessMgr.Terminate(sessionID, true, 0)
	if sess := s.sessMgr.Get(sessionID); sess != nil {
		sess.Disconnect()
	}

	if err := s.sessMgr.Delete(sessionID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleResizePty(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	rows := int(getFloat64(args, "rows", 24))
	cols := int(getFloat64(args, "cols", 80))

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.ResizePty(rows, cols); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleListMessages(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	entries, err := s.msgMgr.List(sessionID)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{"messages": entries}
	return jsonResult(result), nil
}

func (s *Server) handleGetMessage(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	msgIDs := getStringSlice(args, "message_ids")

	if len(msgIDs) == 0 {
		if id := getString(args, "message_id", ""); id != "" {
			msgIDs = append(msgIDs, id)
		}
	}

	messages, err := s.msgMgr.GetMany(sessionID, msgIDs)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{"messages": messages}
	return jsonResult(result), nil
}

func (s *Server) handleRegisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	readerID, err := shell.RegisterReader()
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{"reader_id": readerID}
	return jsonResult(result), nil
}

func (s *Server) handleUnregisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	readerID := int(getFloat64(args, "reader_id", 0))

	shell, bad := s.requireTerminalShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	shell.UnregisterReader(readerID)
	return successResult(), nil
}

func (s *Server) handleBackgroundSend(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return s.handleSendInput(ctx, request)
}

func (s *Server) handleListSSHConfigs(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return jsonResult(map[string]any{"ssh_configs": []any{}}), nil
	}
	names, err := s.sshConfigs.List()
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	arr := make([]any, len(names))
	for i, n := range names {
		arr[i] = n
	}
	return jsonResult(map[string]any{"ssh_configs": arr}), nil
}

func (s *Server) handleDetectShell(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	path, family, hint := shell.NewDetector().Detect()
	if path == "" {
		return mcpgo.NewToolResultError(hint), nil
	}
	result := map[string]any{
		"path":   path,
		"family": family,
		"hint":   hint,
	}
	return jsonResult(result), nil
}

// --- Port forwarding tool handlers ---

func (s *Server) handleForwardPort(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remoteHost := getString(args, "remote_host", "localhost")
	remotePort := int(getFloat64(args, "remote_port", 0))
	localPort := int(getFloat64(args, "local_port", 0))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePort <= 0 || remotePort > 65535 {
		return mcpgo.NewToolResultError("remote_port required (1-65535)"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		return mcpgo.NewToolResultError("session has no SSH client (use an SSH session, not internal)"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := forward.LocalForwardSSH(ctx, sshClient, remoteHost, remotePort, localPort)
	if err != nil {
		cancel()
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	fw.SSHConfig = sess.Info().Name
	fw.SessionID = sessionID
	s.forwardMgr.RegisterForwardFull(fw, ln, cancel)
	return jsonResult(map[string]any{
		"local_port": fw.ListenAddr,
		"forward_id": fw.ForwardID,
	}), nil
}

func (s *Server) handleLocalForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	localHost := getString(args, "local_host", "0.0.0.0")
	localPort := int(getFloat64(args, "local_port", 0))
	remoteHost := getString(args, "remote_host", "")
	remotePort := int(getFloat64(args, "remote_port", 0))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if localPort <= 0 || localPort > 65535 {
		return mcpgo.NewToolResultError("local_port required (1-65535)"), nil
	}
	if remoteHost == "" || remotePort <= 0 {
		return mcpgo.NewToolResultError("remote_host and remote_port required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		return mcpgo.NewToolResultError("session has no SSH client (use an SSH session, not internal)"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := forward.RemoteForwardSSH(ctx, sshClient, localHost, localPort, remoteHost, remotePort)
	if err != nil {
		cancel()
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	fw.SSHConfig = sess.Info().Name
	fw.SessionID = sessionID
	s.forwardMgr.RegisterForwardFull(fw, ln, cancel)
	return jsonResult(map[string]any{
		"remote_port": localPort,
		"forward_id":  fw.ForwardID,
	}), nil
}

func (s *Server) handleDynamicForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	localPort := int(getFloat64(args, "local_port", 0))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil { return mcpgo.NewToolResultError("session has no SSH client (use an SSH session, not internal)"), nil }
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := forward.DynamicForwardSSH(ctx, sshClient, localPort)
	if err != nil { cancel(); return mcpgo.NewToolResultError(err.Error()), nil }
	fw.SSHConfig = sess.Info().Name
	fw.SessionID = sessionID
	s.forwardMgr.RegisterForwardFull(fw, ln, cancel)
	return jsonResult(map[string]any{"local_port": fw.ListenAddr, "forward_id": fw.ForwardID}), nil
}

func (s *Server) handleListForwards(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.forwardMgr == nil {
		return jsonResult(map[string]any{"forwards": []any{}}), nil
	}
	fws := s.forwardMgr.List()
	arr := make([]any, len(fws))
	for i, fw := range fws {
		arr[i] = fw
	}
	return jsonResult(map[string]any{"forwards": arr}), nil
}

func (s *Server) handleCloseForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	forwardID := getString(args, "forward_id", "")
	if forwardID == "" {
		return mcpgo.NewToolResultError("forward_id required"), nil
	}
	if s.forwardMgr == nil {
		return mcpgo.NewToolResultError("forward manager not available"), nil
	}
	if err := s.forwardMgr.Close(forwardID); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// --- File operation tool handlers ---

func (s *Server) handleFileRead(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
		sessionID := strings.TrimSpace(getString(args, "session_id", ""))
remotePath := getString(args, "remote_path", "")
	offset := int64(getFloat64(args, "offset", 0))
	length := int64(getFloat64(args, "length", 0))
	mode := getString(args, "mode", "text")
	localPath := getString(args, "local_path", "")

	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}
	if mode != "text" && mode != "hex" && mode != "file" {
		return mcpgo.NewToolResultError(`mode must be "text", "hex", or "file"`), nil
	}
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		// Internal: read local file directly.
		return s.fileReadLocal(remotePath, offset, length, mode, localPath)
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.ReadFile(remotePath, offset, length, mode, localPath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(toMap(result)), nil
}

func (s *Server) handleFileWrite(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
		sessionID := strings.TrimSpace(getString(args, "session_id", ""))
remotePath := getString(args, "remote_path", "")
	offset := int64(getFloat64(args, "offset", 0))
	data := getString(args, "data", "")
	mode := getString(args, "mode", "text")
	localPath := getString(args, "local_path", "")
	localOffset := int64(getFloat64(args, "local_offset", 0))
	length := int64(getFloat64(args, "length", 0))

	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}
	if localPath == "" && data == "" {
		return mcpgo.NewToolResultError("data or local_path required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		return s.fileWriteLocal(remotePath, offset, data, mode, localPath, localOffset, length)
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	n, err := sftpCli.WriteFile(remotePath, offset, data, mode, localPath, localOffset, length)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"ok": true, "bytes_written": n}), nil
}

func (s *Server) handleFileStat(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
		sessionID := strings.TrimSpace(getString(args, "session_id", ""))
remotePath := getString(args, "remote_path", "")

	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		return s.fileStatLocal(remotePath)
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatFile(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	m := toMap(result)
	m["download_url"] = s.baseURL + "/api/sessions/" + sessionID + "/files/download?path=" + url.QueryEscape(remotePath)
	m["upload_url"] = s.baseURL + "/api/sessions/" + sessionID + "/files/upload"
	m["session_id"] = sessionID
	return jsonResult(m), nil
}


// --- Helpers for file operations on internal/local ---

func (s *Server) fileReadLocal(remotePath string, offset, length int64, mode, localPath string) (*mcpgo.CallToolResult, error) {
	f, err := os.Open(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	totalSize := fi.Size()
	if offset < 0 { offset = 0 }
	if length <= 0 || offset+length > totalSize { length = totalSize - offset }
	hasMore := offset+length < totalSize

	if mode == "file" {
		if localPath == "" {
			return mcpgo.NewToolResultError("local_path required for file mode"), nil
		}
		f.Seek(offset, io.SeekStart)
		lf, err := os.Create(localPath)
		if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
		n, err := io.CopyN(lf, f, length)
		lf.Close()
		if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
		return jsonResult(map[string]any{
			"mode": "file", "total_size": totalSize, "bytes_read": n,
			"local_path": localPath, "has_more": hasMore, "offset": offset, "length": length,
		}), nil
	}

	f.Seek(offset, io.SeekStart)
	buf := make([]byte, length)
	n, _ := io.ReadFull(f, buf)
	segment := buf[:n]
	var data string
	if mode == "hex" {
		data = fmt.Sprintf("%x", segment)
	} else {
		data = encoding.EncodeText(segment)
	}
	return jsonResult(map[string]any{
		"data": data, "mode": mode, "total_size": totalSize,
		"has_more": hasMore, "offset": offset, "length": int64(n), "bytes_read": int64(n),
	}), nil
}

func (s *Server) fileWriteLocal(remotePath string, offset int64, data, mode, localPath string, localOffset, length int64) (*mcpgo.CallToolResult, error) {
	flag := os.O_RDWR | os.O_CREATE
	if offset <= 0 { flag |= os.O_TRUNC }
	f, err := os.OpenFile(remotePath, flag, 0644)
	if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
	defer f.Close()

	var raw []byte
	if localPath != "" {
		lf, err := os.Open(localPath)
		if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
		defer lf.Close()
		fi, _ := lf.Stat()
		if length <= 0 || localOffset+length > fi.Size() { length = fi.Size() - localOffset }
		lf.Seek(localOffset, io.SeekStart)
		f.Seek(offset, io.SeekStart)
		n, err := io.CopyN(f, lf, length)
		if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
		return jsonResult(map[string]any{"ok": true, "bytes_written": n}), nil
	}

	switch mode {
	case "hex":
		raw, err = encoding.HexDecode(data)
		if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
	default:
		raw = encoding.DecodeText(data)
	}
	f.Seek(offset, io.SeekStart)
	n, err := f.Write(raw)
	if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
	return jsonResult(map[string]any{"ok": true, "bytes_written": n}), nil
}

func (s *Server) fileStatLocal(remotePath string) (*mcpgo.CallToolResult, error) {
	fi, err := os.Stat(remotePath)
	if err != nil { return mcpgo.NewToolResultError(err.Error()), nil }
	result := map[string]any{
		"name": filepath.Base(remotePath), "size": fi.Size(), "is_dir": fi.IsDir(),
		"mod_time": fi.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if fi.IsDir() {
		entries, err := os.ReadDir(remotePath)
		if err == nil {
			var children []map[string]any
			for _, e := range entries {
				info, _ := e.Info()
				child := map[string]any{"name": e.Name(), "is_dir": e.IsDir()}
				if info != nil {
					child["size"] = info.Size()
					child["mod_time"] = info.ModTime().UTC().Format("2006-01-02T15:04:05Z")
				}
				children = append(children, child)
			}
			result["children"] = children
		}
	}
	return jsonResult(result), nil
}



func (s *Server) handleFileDelete(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}
	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		if err := os.Remove(remotePath); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return successResult(), nil
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RemoveFile(remotePath); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileRename(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	fromPath := getString(args, "from_path", "")
	toPath := getString(args, "to_path", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if fromPath == "" || toPath == "" {
		return mcpgo.NewToolResultError("from_path and to_path required"), nil
	}
	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		if err := os.Rename(fromPath, toPath); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return successResult(), nil
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RenameFile(fromPath, toPath); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileMakeDir(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}
	sess, bad := s.requireSession(sessionID)
	if bad != nil { return bad, nil }
	sshClient := sess.SSHClient()
	if sshClient == nil {
		if err := os.MkdirAll(remotePath, 0755); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		return successResult(), nil
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err)), nil
	}
	defer sftpCli.Close()
	if err := sftpCli.MakeDir(remotePath); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleGetFileURLs(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}
	return jsonResult(map[string]any{
		"download_url": s.baseURL + "/api/sessions/" + sessionID + "/files/download?path=" + url.QueryEscape(remotePath),
		"upload_url":   s.baseURL + "/api/sessions/" + sessionID + "/files/upload",
		"session_id":   sessionID,
		"remote_path":  remotePath,
	}), nil
}

// toMap converts a struct to map[string]any via JSON round-trip.
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
