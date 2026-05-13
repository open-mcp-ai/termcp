package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
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
	mode := getString(args, "mode", "pty")
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

func (s *Server) requireSession(sessionID string) (*session.Session, *mcpgo.CallToolResult) {
	sess := s.sessMgr.Get(sessionID)
	if sess == nil {
		return nil, mcpgo.NewToolResultError(fmt.Sprintf("Session '%s' not found", sessionID))
	}
	return sess, nil
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

func remoteFromEntry(e *sshconfig.Entry) (*session.RemoteSSH, error) {
	pem := strings.TrimSpace(e.PrivateKeyPEM)
	if fn := strings.TrimSpace(e.PrivateKeyFile); fn != "" {
		b, err := os.ReadFile(filepath.Clean(fn))
		if err != nil {
			return nil, fmt.Errorf("private_key_file %q: %w", fn, err)
		}
		pem = string(b)
	}
	trust := true
	if e.TrustUnknownHost != nil {
		trust = *e.TrustUnknownHost
	}
	port := e.Port
	if port == 0 {
		port = 22
	}
	return &session.RemoteSSH{
		Host:               e.Host,
		Port:               port,
		User:               e.User,
		Password:           e.Password,
		PrivateKeyPEM:      pem,
		KeyPassphrase:      e.KeyPassphrase,
		TrustUnknownHost:   trust,
		KnownHosts:         e.KnownHosts,
		DialTimeoutSeconds: e.DialTimeoutSeconds,
	}, nil
}

// resolveSSHFromArgs returns the ssh_config name and remote dial settings (nil Remote = built-in loopback).
func (s *Server) resolveSSHFromArgs(args map[string]any) (string, *session.RemoteSSH, error) {
	if s.sshConfigs == nil {
		return "", nil, fmt.Errorf("ssh config store not configured")
	}
	name := strings.TrimSpace(getString(args, "ssh_config", ""))
	if name == "" {
		name = "internal"
	}
	ent, err := s.sshConfigs.Load(name)
	if err != nil {
		return "", nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		return name, nil, nil
	}
	r, err := remoteFromEntry(ent)
	if err != nil {
		return "", nil, err
	}
	return name, r, nil
}

func (s *Server) handleStartSession(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	command := getString(args, "command", "")
	if command == "" {
		return mcpgo.NewToolResultError("command is required"), nil
	}
	if bad, _ := validateStartParams(args); bad != nil {
		return bad, nil
	}

	cfgName, remote, err := s.resolveSSHFromArgs(args)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	sess, err := s.sessMgr.Create(session.Config{
		Command: command,
		Args:    getStringSlice(args, "args"),
		Mode:    api.SessionMode(getString(args, "mode", "pty")),
		Name:    getString(args, "name", ""),
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

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := sess.SendInput(text, pressEnter); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
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
	maxLines := int(getFloat64(args, "max_lines", 0))
	readerID := int(getFloat64(args, "reader_id", 0))

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	output, err := sess.ReadOutputForReader(ctx, readerID, time.Duration(timeout*float64(time.Second)), stripAnsi, maxLines)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{
		"output":         output,
		"has_more":       sess.HasMoreOutput(readerID),
		"lines_returned": strings.Count(output, "\n"),
		"bytes_returned": len(output),
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
	maxLines := int(getFloat64(args, "max_lines", 0))
	readerID := int(getFloat64(args, "reader_id", 0))

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := sess.SendInput(text, pressEnter); err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	output, err := sess.ReadOutputForReader(ctx, readerID, time.Duration(timeout*float64(time.Second)), stripAnsi, maxLines)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	result := map[string]any{
		"output":         output,
		"has_more":       sess.HasMoreOutput(readerID),
		"lines_returned": strings.Count(output, "\n"),
		"bytes_returned": len(output),
	}
	return jsonResult(result), nil
}

func (s *Server) handleListSessions(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	sessions := s.sessMgr.ListAll()
	result := map[string]any{"sessions": sessions}
	return jsonResult(result), nil
}

func (s *Server) handleGetSessionInfo(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	sess, bad := s.requireSession(sessionID)
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
	return successResult(), nil
}

func (s *Server) handleDeleteSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	if sessionID == "" {
		return mcpgo.NewToolResultError("session_id is required"), nil
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

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := sess.ResizePty(rows, cols); err != nil {
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

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	readerID, err := sess.RegisterReader()
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

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	sess.UnregisterReader(readerID)
	return successResult(), nil
}

func (s *Server) handleBackgroundSend(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return s.handleSendInput(ctx, request)
}

func (s *Server) handleUploadFile(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	contentBase64 := getString(args, "content_base64", "")
	remotePath := getString(args, "remote_path", "")

	if contentBase64 == "" {
		return mcpgo.NewToolResultError("content_base64 is required"), nil
	}
	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path is required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}

	n, err := sess.UploadFile(contentBase64, remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	result := map[string]any{
		"status":      "uploaded",
		"remote_path": remotePath,
		"size":        n,
	}
	return jsonResult(result), nil
}

func (s *Server) handleDownloadFile(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	remotePath := getString(args, "remote_path", "")

	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path is required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}

	result, err := sess.DownloadFile(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	return jsonResult(map[string]any{
		"content":  result.Content,
		"encoding": result.Encoding,
		"size":     result.Size,
	}), nil
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

func (s *Server) handleListFiles(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	remotePath := getString(args, "remote_path", "")

	if remotePath == "" {
		return mcpgo.NewToolResultError("remote_path is required"), nil
	}

	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}

	entries, err := sess.ListFiles(remotePath)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	return jsonResult(map[string]any{
		"path":    remotePath,
		"entries": entries,
	}), nil
}
