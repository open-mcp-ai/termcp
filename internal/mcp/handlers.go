package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/sftp"
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

// sftpClient resolves a session and creates an SFTP client over it.
// Caller must defer Close() on the returned client.
func (s *Server) sftpClient(sessionID string) (*sftp.Client, *mcpgo.CallToolResult) {
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return nil, bad
	}
	cli, err := sftp.NewClient(sess.SSHClient())
	if err != nil {
		return nil, mcpgo.NewToolResultError(fmt.Sprintf("SFTP: %v", err))
	}
	return cli, nil
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

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
	return bad, nil
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

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
	return bad, nil
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

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
	return bad, nil
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
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
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
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
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
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
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

// handleFileChmod changes file permissions via SSH/SFTP.
func (s *Server) handleFileChmod(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	mode := os.FileMode(getFloat64(args, "mode", 0))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.ChmodFile(remotePath, mode); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileChown changes file owner and group via SSH/SFTP.
func (s *Server) handleFileChown(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	uid := int(getFloat64(args, "uid", -1))
	gid := int(getFloat64(args, "gid", -1))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.ChownFile(remotePath, uid, gid); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileChtimes changes file access and modification times via SSH/SFTP.
func (s *Server) handleFileChtimes(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	atimeSec := getFloat64(args, "atime", 0)
	mtimeSec := getFloat64(args, "mtime", 0)

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	atime := time.Unix(int64(atimeSec), 0)
	mtime := time.Unix(int64(mtimeSec), 0)

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.ChtimesFile(remotePath, atime, mtime); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileReadlink reads the target of a symbolic link via SSH/SFTP.
func (s *Server) handleFileReadlink(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	target, err := sftpCli.ReadLink(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"target": target}), nil
}

// handleFileSymlink creates a symbolic link via SSH/SFTP.
func (s *Server) handleFileSymlink(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	target := getString(args, "target", "")
	linkPath := getString(args, "link_path", "")

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if target == "" {
		return mcpgo.NewToolResultError("target required"), nil
	}
	if linkPath == "" {
		return mcpgo.NewToolResultError("link_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.SymlinkFile(target, linkPath); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileLink creates a hard link via SSH/SFTP.
func (s *Server) handleFileLink(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	existingPath := getString(args, "existing_path", "")
	newPath := getString(args, "new_path", "")

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if existingPath == "" {
		return mcpgo.NewToolResultError("existing_path required"), nil
	}
	if newPath == "" {
		return mcpgo.NewToolResultError("new_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.LinkFile(existingPath, newPath); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileTruncate truncates a file to a specified size via SSH/SFTP.
func (s *Server) handleFileTruncate(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	size := int64(getFloat64(args, "size", 0))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.TruncateFile(remotePath, size); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return successResult(), nil
}

// handleFileRealpath resolves the canonical absolute path via SSH/SFTP.
func (s *Server) handleFileRealpath(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	canonical, err := sftpCli.RealPath(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"canonical_path": canonical}), nil
}

// handleFileStatVFS returns filesystem statistics via SSH/SFTP.
func (s *Server) handleFileStatVFS(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatVFS(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(toMap(result)), nil
}

// handleFileGetwd returns the remote working directory via SSH/SFTP.
func (s *Server) handleFileGetwd(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))

	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	dir, err := sftpCli.Getwd()
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return jsonResult(map[string]any{"directory": dir}), nil
}

// toMap converts a struct to map[string]any via JSON round-trip.
func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}
