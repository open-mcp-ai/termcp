package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
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
	mode := strings.TrimSpace(getString(args, "mode", "pty"))
	if mode == "" {
		mode = "pty"
	}
	if mode != "pty" && mode != "pipe" {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("mode must be 'pty' or 'pipe', got %q", mode)), nil
	}
	rows := int(getFloat64(args, "rows", 24))
	if rows < 1 || rows > 1000 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("rows must be between 1 and 1000, got %d", rows)), nil
	}
	cols := int(getFloat64(args, "cols", 80))
	if cols < 1 || cols > 1000 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("cols must be between 1 and 1000, got %d", cols)), nil
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
		return nil, toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", sessionID))
	}
	return sess, nil
}

// sshClientForSession resolves a session and returns it together with its live SSH
// client. Restored/DEAD sessions keep metadata but no transport; callers receive a
// standard tool error instead of a nil client that would crash SFTP/forward internals.
func (s *Server) sshClientForSession(sessionID string) (*session.Session, *ssh.Client, *mcpgo.CallToolResult) {
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return nil, nil, bad
	}
	cli := sess.SSHClient()
	if cli == nil {
		return nil, nil, toolError(CodeSessionNotRunning, "%s", fmt.Sprintf("Session '%s' is not running or has no active SSH connection", sessionID))
	}
	return sess, cli, nil
}

// sftpClient resolves a session and creates an SFTP client over it.
// Caller must defer Close() on the returned client.
func (s *Server) sftpClient(sessionID string) (*sftp.Client, *mcpgo.CallToolResult) {
	_, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return nil, bad
	}
	cli, err := sftp.NewClient(sshCli)
	if err != nil {
		return nil, toolError(CodeOperationFailed, "%s", fmt.Sprintf("SFTP: %v", err))
	}
	return cli, nil
}

// requireShell looks up a shell by shell_id for terminal I/O (never session_id).
func (s *Server) requireShell(shellID string) (*session.ChildShell, *mcpgo.CallToolResult) {
	if cs := s.sessMgr.GetChildShell(shellID); cs != nil {
		return cs, nil
	}
	return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shellID))
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
		if s.NoInternal {
			return "", nil, nil, fmt.Errorf("ssh_config is required when internal profile is disabled")
		}
		name = "internal"
	}
	ent, err := s.sshConfigs.Load(name)
	if err != nil {
		return "", nil, nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		if s.NoInternal {
			return "", nil, nil, fmt.Errorf("internal profile is disabled")
		}
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
		return toolError(CodeInvalidArgument, "%s", "command is required when args are provided"), nil
	}
	if bad, _ := validateStartParams(args); bad != nil {
		return bad, nil
	}

	cfgName, ent, remote, err := s.resolveSSHFromArgs(args)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}

	cmd, execArgs := sshconfig.EffectiveCommand(ent, command, toolArgs)
	if strings.TrimSpace(cmd) == "" && len(execArgs) > 0 {
		return toolError(CodeInvalidArgument, "%s", "command is required when args are provided"), nil
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
		return toolError(CodeOperationFailed, "%s", sshclient.DescribeDialError(err)), nil
	}

	time.Sleep(100 * time.Millisecond)

	result := map[string]any{
		"session_id": sess.ID,
		"shell_id":   sess.PrimaryShellID(),
		"pid":        sess.PID,
		"ssh_config": cfgName,
	}
	return jsonResult(result), nil
}

func (s *Server) handleSendInput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	text := getString(args, "text", "")

	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.SendTerminalBytes([]byte(text), false); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handlePressKey(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	key := getString(args, "key", "")
	repeat := int(getFloat64(args, "repeat", 1))
	if key == "" {
		return toolError(CodeInvalidArgument, "%s", "key is required"), nil
	}
	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.PressKey(key, repeat); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleStartSubShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "session_id", "")
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
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"shell_id": cs.ID, "session_id": parentID, "name": cs.Name}), nil
}

func (s *Server) handleListSubshells(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "session_id", "")

	sess, bad := s.requireSession(parentID)
	if bad != nil {
		return bad, nil
	}
	all := sess.ListChildShells()
	return jsonResult(map[string]any{"session_id": parentID, "shells": filterRunning(all)}), nil
}

// handleCloseShell closes a single shell channel without tearing down the parent session.
// For a parent session id: closes the root shell channel only (remote) / no-op (internal);
// the SSH connection and other child shells keep running. For a child shell id: closes
// just that channel. Use session_terminate to fully stop a session.
func (s *Server) handleCloseShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	if shellID == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	// Internal primary shell: tab close is a no-op (process outlives the tab).
	if sess := s.sessMgr.GetByShellID(shellID); sess != nil && sess.PrimaryShellID() == shellID && sess.SSHEndpoint == "internal" {
		return successResult(), nil
	}
	found, err := s.sessMgr.CloseChildShell(shellID)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if !found {
		return toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shellID)), nil
	}
	return successResult(), nil
}

// handleReadOutput is the ONE unified output reader. It serves live shells
// (in-memory buffer), exited-but-retained shells, and archived/restored-DEAD
// sessions (persisted message log) with identical byte-stream cursor semantics.
// Three read modes:
//   - tail_lines > 0, or an archived id with no offset: read the tail of the
//     stream (token-safe default; never a full dump);
//   - offset >= 0: stateless positional read of [offset, offset+max_bytes);
//   - otherwise: live streaming cursor on reader_id (new bytes since last read).
//
// Every response carries start_offset/end_offset/total_bytes/has_more so the
// caller can page the stream without server-side state.
func (s *Server) handleReadOutput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	id := getString(args, "shell_id", "")
	if id == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	stripAnsi := getBool(args, "strip_ansi", true)
	timeout := getFloat64(args, "timeout", 3.0)
	if timeout < 0 || timeout > 60 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("timeout must be between 0 and 60, got %v", timeout)), nil
	}
	maxLines := int(getFloat64(args, "max_lines", 0))
	maxBytes := int(getFloat64(args, "max_bytes", 8192))
	readerID := int(getFloat64(args, "reader_id", 0))
	offset := int64(getFloat64(args, "offset", -1))
	tailLines := int(getFloat64(args, "tail_lines", 0))
	if tailLines < 0 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("tail_lines must be >= 0, got %d", tailLines)), nil
	}
	if offset < -1 {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("offset must be >= -1, got %d", offset)), nil
	}

	src, bad := s.resolveOutputSource(id)
	if bad != nil {
		return bad, nil
	}
	if readerID > 0 && src.live == nil {
		return toolError(CodeInvalidArgument, "%s", "reader_id requires a live shell; archived sessions are read with offset/tail_lines"), nil
	}
	clean := func(raw []byte) string {
		if !stripAnsi {
			return string(raw)
		}
		return ansi.Compact(ansi.Strip(string(raw)))
	}

	var output string
	var start, end, total int64
	var hasMore bool

	switch {
	case tailLines > 0 || (src.live == nil && offset < 0):
		raw, st, tot, err := src.scanTailWindow(tailLines, maxBytes)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		output, start, end, total, hasMore = clean(raw), st, tot, tot, false
	case offset >= 0:
		tot, err := src.Len()
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		total = tot
		max := maxBytes
		if max <= 0 {
			max = int(total - offset)
			if max < 0 {
				max = 0
			}
		}
		raw, _, err := src.ByteRange(offset, max)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		raw = truncateAtLines(raw, maxLines, offset+int64(len(raw)) >= total)
		start, end = offset, offset+int64(len(raw))
		hasMore = end < total
		output = clean(raw)
	default:
		// Live streaming cursor path (unchanged semantics).
		pre := src.live.ReaderCursor(readerID)
		if pre < 0 {
			return toolError(CodeReaderNotRegistered, "%s", fmt.Sprintf("reader_id %d is not registered on this shell", readerID)), nil
		}
		out, err := src.live.ReadTerminalStream(ctx, readerID, time.Duration(timeout*float64(time.Second)), stripAnsi, maxLines, maxBytes)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		output = out
		end = src.live.ReaderCursor(readerID)
		total = src.live.BufferLen()
		start = pre
		hasMore = end < total
	}

	result := map[string]any{
		"output":         output,
		"has_more":       hasMore,
		"lines_returned": strings.Count(output, "\n"),
		"bytes_returned": len(output),
		"start_offset":   start,
		"end_offset":     end,
		"total_bytes":    total,
		"source":         src.source(),
		"session_id":     src.sessID,
		"shell_id":       src.shellID,
		"session_status": string(src.status),
	}
	if src.live != nil {
		result["session_uptime_seconds"] = int(time.Since(src.created).Seconds())
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
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("grace_period must be between 0 and 60, got %v", gracePeriod)), nil
	}

	_, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	s.sessMgr.Terminate(sessionID, force, time.Duration(gracePeriod*float64(time.Second)))
	// Deliberate close: move the session into the history archive and drop it
	// from the live registry (so it is not reloaded as a DEAD tile after restart).
	_ = s.sessMgr.ArchiveAndForget(sessionID, api.ArchiveExplicit)
	return successResult(), nil
}

func (s *Server) handleResizePty(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")
	rows := int(getFloat64(args, "rows", 24))
	cols := int(getFloat64(args, "cols", 80))

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.ResizePty(rows, cols); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleListMessages(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")

	entries, err := s.msgMgr.List(sessionID)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	result := map[string]any{"messages": messages}
	return jsonResult(result), nil
}

func (s *Server) handleListHistory(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.historyMgr == nil {
		return jsonResult(map[string]any{"sessions": []any{}}), nil
	}
	return jsonResult(map[string]any{"sessions": s.historyMgr.List()}), nil
}

func (s *Server) handleSearchMessages(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	query := getString(args, "query", "")
	limit := int(getFloat64(args, "limit", 50))
	if s.historyMgr == nil {
		return jsonResult(map[string]any{"hits": []any{}}), nil
	}
	return jsonResult(map[string]any{"query": query, "hits": s.historyMgr.Search(query, limit)}), nil
}

func (s *Server) handleRenameSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	if sess := s.sessMgr.Get(sessionID); sess != nil {
		if err := s.sessMgr.Rename(sessionID, name); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return successResult(), nil
	}
	if s.historyMgr == nil {
		return toolError(CodeNotConfigured, "%s", "history not configured"), nil
	}
	if err := s.historyMgr.Update(sessionID, &name, nil, nil); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleUpdateSessionMeta(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	if s.historyMgr == nil {
		return toolError(CodeNotConfigured, "%s", "history not configured"), nil
	}
	var notes *string
	if v, ok := args["notes"]; ok {
		vs := fmt.Sprintf("%v", v)
		notes = &vs
	}
	var tags *[]string
	if _, ok := args["tags"]; ok {
		t := getStringSlice(args, "tags")
		tags = &t
	}
	if err := s.historyMgr.Update(sessionID, nil, notes, tags); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handlePurgeSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	if s.sessMgr.Get(sessionID) != nil {
		if err := s.sessMgr.Delete(sessionID); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return successResult(), nil
	}
	if s.historyMgr == nil {
		return toolError(CodeNotConfigured, "%s", "history not configured"), nil
	}
	if _, ok := s.historyMgr.Get(sessionID); !ok {
		return toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", sessionID)), nil
	}
	if err := s.historyMgr.Delete(sessionID); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleScreenshot(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	if s.historyMgr == nil {
		return toolError(CodeNotConfigured, "%s", "history not configured"), nil
	}
	if _, ok := s.historyMgr.Get(sessionID); !ok {
		return toolError(CodeHistoryNotFound, "%s", fmt.Sprintf("Session '%s' not found in history", sessionID)), nil
	}
	start := int(getFloat64(args, "start", 0))
	lines := int(getFloat64(args, "lines", 0))
	cols := int(getFloat64(args, "cols", 80))
	theme := getString(args, "theme", "dark")
	q := url.Values{}
	q.Set("start", strconv.Itoa(start))
	q.Set("lines", strconv.Itoa(lines))
	q.Set("cols", strconv.Itoa(cols))
	q.Set("theme", theme)
	u := s.baseURL + "/api/history/" + url.PathEscape(sessionID) + "/screenshot?" + q.Encode()
	return jsonResult(map[string]any{
		"session_id": sessionID,
		"url":        u,
		"note":       "Fetch this URL to download the PNG. Rendered from persisted messages as a fixed-bitmap terminal image (ASCII only).",
	}), nil
}

func (s *Server) handleRegisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	readerID, err := shell.RegisterReader()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	result := map[string]any{"reader_id": readerID}
	return jsonResult(result), nil
}

func (s *Server) handleUnregisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")
	readerID := int(getFloat64(args, "reader_id", 0))

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	shell.UnregisterReader(readerID)
	return successResult(), nil
}

func (s *Server) handleCreateSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	host := strings.TrimSpace(getString(args, "host", ""))
	user := strings.TrimSpace(getString(args, "user", ""))
	password := strings.TrimSpace(getString(args, "password", ""))
	privateKey := strings.TrimSpace(getString(args, "private_key", ""))
	keyPassphrase := strings.TrimSpace(getString(args, "key_passphrase", ""))
	port := int(getFloat64(args, "port", 22))
	trustUnknown := getBool(args, "trust_unknown_host", false)
	knownHosts := strings.TrimSpace(getString(args, "known_hosts", ""))
	dialTimeout := int(getFloat64(args, "dial_timeout_seconds", 30))
	proxy := strings.TrimSpace(getString(args, "proxy", ""))
	description := strings.TrimSpace(getString(args, "description", ""))
	defaultShell := strings.TrimSpace(getString(args, "default_shell", ""))
	defaultMode := strings.TrimSpace(getString(args, "default_mode", ""))

	entry := &sshconfig.Entry{
		Kind:         sshconfig.KindRemote,
		Description:  description,
		DefaultShell: defaultShell,
		DefaultMode:  defaultMode,
		DialSpec: sshconfig.DialSpec{
			Host:               host,
			Port:               port,
			User:               user,
			Password:           password,
			PrivateKey:         privateKey,
			KeyPassphrase:      keyPassphrase,
			TrustUnknownHost:   &trustUnknown,
			KnownHosts:         knownHosts,
			DialTimeoutSeconds: dialTimeout,
			Proxy:              proxy,
		},
	}

	// Optional single-level jump
	if jh := strings.TrimSpace(getString(args, "jump_host", "")); jh != "" {
		ju := strings.TrimSpace(getString(args, "jump_user", ""))
		jp := strings.TrimSpace(getString(args, "jump_password", ""))
		jpk := strings.TrimSpace(getString(args, "jump_private_key", ""))
		jkp := strings.TrimSpace(getString(args, "jump_key_passphrase", ""))
		jt := getBool(args, "jump_trust_unknown_host", false)
		jkh := strings.TrimSpace(getString(args, "jump_known_hosts", ""))
		jdt := int(getFloat64(args, "jump_dial_timeout_seconds", 30))
		jpx := strings.TrimSpace(getString(args, "jump_proxy", ""))
		jport := int(getFloat64(args, "jump_port", 22))
		entry.Jump = &sshconfig.JumpSpec{
			DialSpec: sshconfig.DialSpec{
				Host:               jh,
				Port:               jport,
				User:               ju,
				Password:           jp,
				PrivateKey:         jpk,
				KeyPassphrase:      jkp,
				TrustUnknownHost:   &jt,
				KnownHosts:         jkh,
				DialTimeoutSeconds: jdt,
				Proxy:              jpx,
			},
		}
	}

	// Refuse to overwrite an existing profile — use edit_ssh_config or copy_ssh_config.
	if names, err := s.sshConfigs.List(); err == nil {
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return toolError(CodeConflict, "%s", fmt.Sprintf("ssh config %q already exists (use edit_ssh_config or copy_ssh_config)", name)), nil
			}
		}
	}

	body, err := toml.Marshal(entry)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleDeleteSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	name := strings.TrimSpace(getString(request.GetArguments(), "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}
	if err := s.sshConfigs.Delete(name); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleCopySSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	src := strings.TrimSpace(getString(args, "source_name", ""))
	dst := strings.TrimSpace(getString(args, "target_name", ""))
	if src == "" || dst == "" {
		return toolError(CodeInvalidArgument, "%s", "source_name and target_name are required"), nil
	}
	data, err := s.sshConfigs.ReadRaw(src)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(dst, data); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleEditSSHConfig(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return toolError(CodeNotConfigured, "%s", "ssh config store not configured"), nil
	}
	args := request.GetArguments()
	name := strings.TrimSpace(getString(args, "name", ""))
	if name == "" {
		return toolError(CodeInvalidArgument, "%s", "name is required"), nil
	}

	existing, err := s.sshConfigs.Load(name)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}

	// Merge: apply non-empty values from args over existing entry.
	if v := getString(args, "host", ""); v != "" {
		existing.Host = strings.TrimSpace(v)
	}
	if v := getString(args, "user", ""); v != "" {
		existing.User = strings.TrimSpace(v)
	}
	if v := getString(args, "password", ""); v != "" {
		existing.Password = strings.TrimSpace(v)
	}
	if v := getString(args, "private_key", ""); v != "" {
		existing.PrivateKey = strings.TrimSpace(v)
	}
	if v := getString(args, "key_passphrase", ""); v != "" {
		existing.KeyPassphrase = strings.TrimSpace(v)
	}
	if v := getString(args, "description", ""); v != "" {
		existing.Description = strings.TrimSpace(v)
	}
	if v := getString(args, "default_shell", ""); v != "" {
		existing.DefaultShell = strings.TrimSpace(v)
	}
	if v := getString(args, "default_mode", ""); v != "" {
		existing.DefaultMode = strings.TrimSpace(v)
	}
	if v := getString(args, "known_hosts", ""); v != "" {
		existing.KnownHosts = strings.TrimSpace(v)
	}
	if v := getString(args, "proxy", ""); v != "" {
		existing.Proxy = strings.TrimSpace(v)
	}
	if v := getFloat64(args, "port", -1); v >= 0 {
		existing.Port = int(v)
	}
	if v := getFloat64(args, "dial_timeout_seconds", -1); v >= 0 {
		existing.DialTimeoutSeconds = int(v)
	}
	if _, ok := args["trust_unknown_host"]; ok {
		t := getBool(args, "trust_unknown_host", false)
		existing.TrustUnknownHost = &t
	}

	// Jump merge
	if jh := getString(args, "jump_host", ""); jh != "" {
		if existing.Jump == nil {
			existing.Jump = &sshconfig.JumpSpec{}
		}
		existing.Jump.Host = strings.TrimSpace(jh)
		if v := getString(args, "jump_user", ""); v != "" {
			existing.Jump.User = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_password", ""); v != "" {
			existing.Jump.Password = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_private_key", ""); v != "" {
			existing.Jump.PrivateKey = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_key_passphrase", ""); v != "" {
			existing.Jump.KeyPassphrase = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_known_hosts", ""); v != "" {
			existing.Jump.KnownHosts = strings.TrimSpace(v)
		}
		if v := getString(args, "jump_proxy", ""); v != "" {
			existing.Jump.Proxy = strings.TrimSpace(v)
		}
		if v := getFloat64(args, "jump_port", -1); v >= 0 {
			existing.Jump.Port = int(v)
		}
		if v := getFloat64(args, "jump_dial_timeout_seconds", -1); v >= 0 {
			existing.Jump.DialTimeoutSeconds = int(v)
		}
		if _, ok := args["jump_trust_unknown_host"]; ok {
			t := getBool(args, "jump_trust_unknown_host", false)
			existing.Jump.TrustUnknownHost = &t
		}
	}

	body, err := toml.Marshal(existing)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if _, err := sshconfig.ParseAndValidate(body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if err := s.sshConfigs.Save(name, body); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleListSSHConfigs(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.sshConfigs == nil {
		return jsonResult(map[string]any{"ssh_configs": []any{}}), nil
	}
	names, err := s.sshConfigs.List()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	arr := make([]any, 0, len(names))
	for _, n := range names {
		if s.NoInternal && strings.EqualFold(n, "internal") {
			continue
		}
		arr = append(arr, n)
	}
	return jsonResult(map[string]any{"ssh_configs": arr}), nil
}

func (s *Server) handleDetectShell(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	path, family, hint := shell.NewDetector().Detect()
	if path == "" {
		return toolError(CodeOperationFailed, "%s", hint), nil
	}
	result := map[string]any{
		"path":   path,
		"family": family,
		"hint":   hint,
	}
	return jsonResult(result), nil
}

// --- Port forwarding tool handlers ---

func (s *Server) handleLocalForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remoteHost := getString(args, "remote_host", "localhost")
	remotePort := int(getFloat64(args, "remote_port", 0))
	localPort := int(getFloat64(args, "local_port", 0))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePort <= 0 || remotePort > 65535 {
		return toolError(CodeInvalidArgument, "%s", "remote_port required (1-65535)"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	fw, err := s.forwardMgr.CreateLocal(sessionID, sess.Info().Name, remoteHost, remotePort, localPort, sshCli)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{
		"local_port": fw.ListenAddr,
		"forward_id": fw.ForwardID,
	}), nil
}

func (s *Server) handleRemoteForward(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	localHost := getString(args, "local_host", "0.0.0.0")
	localPort := int(getFloat64(args, "local_port", 0))
	remoteHost := getString(args, "remote_host", "")
	remotePort := int(getFloat64(args, "remote_port", 0))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if localPort <= 0 || localPort > 65535 {
		return toolError(CodeInvalidArgument, "%s", "local_port required (1-65535)"), nil
	}
	if remoteHost == "" || remotePort <= 0 {
		return toolError(CodeInvalidArgument, "%s", "remote_host and remote_port required"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	fw, err := s.forwardMgr.CreateRemote(sessionID, sess.Info().Name, localHost, localPort, remoteHost, remotePort, sshCli)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
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
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sess, sshCli, bad := s.sshClientForSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	info := sess.Info()
	fw, err := s.forwardMgr.CreateDynamic(sessionID, info.Name, localPort, sshCli, info.SSHEndpoint == "internal")
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
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
		return toolError(CodeInvalidArgument, "%s", "forward_id required"), nil
	}
	if s.forwardMgr == nil {
		return toolError(CodeNotConfigured, "%s", "forward manager not available"), nil
	}
	if err := s.forwardMgr.Close(forwardID); err != nil {
		return toolError(forwardErrCode(err), "%s", err.Error()), nil
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
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	if mode != "text" && mode != "hex" && mode != "file" {
		return toolError(CodeInvalidArgument, "%s", `mode must be "text", "hex", or "file"`), nil
	}
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.ReadFile(remotePath, offset, length, mode, localPath)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	if localPath == "" && data == "" {
		return toolError(CodeInvalidArgument, "%s", "data or local_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	n, err := sftpCli.WriteFile(remotePath, offset, data, mode, localPath, localOffset, length)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return jsonResult(map[string]any{"ok": true, "bytes_written": n}), nil
}

func (s *Server) handleFileStat(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")

	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatFile(remotePath)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RemoveFile(remotePath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileRename(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	fromPath := getString(args, "from_path", "")
	toPath := getString(args, "to_path", "")
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if fromPath == "" || toPath == "" {
		return toolError(CodeInvalidArgument, "%s", "from_path and to_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.RenameFile(fromPath, toPath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleFileMakeDir(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	if err := sftpCli.MakeDir(remotePath); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handleGetFileURLs(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	remotePath := getString(args, "remote_path", "")
	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	if remotePath == "" {
		return toolError(CodeInvalidArgument, "%s", "remote_path required"), nil
	}
	return jsonResult(map[string]any{
		"download_url": s.baseURL + "/api/sessions/" + sessionID + "/files/download?path=" + url.QueryEscape(remotePath),
		"upload_url":   s.baseURL + "/api/sessions/" + sessionID + "/files/upload",
		"session_id":   sessionID,
		"remote_path":  remotePath,
	}), nil
}

// handleFilePerm dispatches chmod, chown, chtimes.
func (s *Server) handleFilePerm(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "chmod":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "chmod requires remote_path and mode (decimal, e.g. 493 = 0755)"), nil
		}
		if _, ok := args["mode"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chmod requires remote_path and mode (decimal, e.g. 493 = 0755)"), nil
		}
		mode := os.FileMode(getFloat64(args, "mode", 0))
		if err := sftpCli.ChmodFile(remotePath, mode); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "chown":
		remotePath := getString(args, "remote_path", "")
		uid := int(getFloat64(args, "uid", -1))
		gid := int(getFloat64(args, "gid", -1))
		if remotePath == "" || uid < 0 || gid < 0 {
			return toolError(CodeInvalidArgument, "%s", "chown requires remote_path, uid, and gid"), nil
		}
		if err := sftpCli.ChownFile(remotePath, uid, gid); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "chtimes":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		if _, ok := args["atime"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		if _, ok := args["mtime"]; !ok {
			return toolError(CodeInvalidArgument, "%s", "chtimes requires remote_path, atime, and mtime"), nil
		}
		atime := time.Unix(int64(getFloat64(args, "atime", 0)), 0)
		mtime := time.Unix(int64(getFloat64(args, "mtime", 0)), 0)
		if err := sftpCli.ChtimesFile(remotePath, atime, mtime); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be chmod, chown, or chtimes"), nil
	}
	return successResult(), nil
}

// handleFileLinkOp dispatches readlink, symlink, link.
func (s *Server) handleFileLinkOp(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "readlink":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "readlink requires remote_path"), nil
		}
		target, err := sftpCli.ReadLink(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"target": target}), nil
	case "symlink":
		target := getString(args, "target", "")
		linkPath := getString(args, "link_path", "")
		if target == "" || linkPath == "" {
			return toolError(CodeInvalidArgument, "%s", "symlink requires target and link_path"), nil
		}
		if err := sftpCli.SymlinkFile(target, linkPath); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "link":
		existingPath := getString(args, "existing_path", "")
		newPath := getString(args, "new_path", "")
		if existingPath == "" || newPath == "" {
			return toolError(CodeInvalidArgument, "%s", "link requires existing_path and new_path"), nil
		}
		if err := sftpCli.LinkFile(existingPath, newPath); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be readlink, symlink, or link"), nil
	}
	return successResult(), nil
}

// handleFileFsOp dispatches truncate, realpath, statvfs.
func (s *Server) handleFileFsOp(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))
	action := getString(args, "action", "")

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}
	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()

	switch action {
	case "truncate":
		remotePath := getString(args, "remote_path", "")
		size := int64(getFloat64(args, "size", 0))
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "truncate requires remote_path and size"), nil
		}
		if err := sftpCli.TruncateFile(remotePath, size); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
	case "realpath":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "realpath requires remote_path"), nil
		}
		canonical, err := sftpCli.RealPath(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(map[string]any{"canonical_path": canonical}), nil
	case "statvfs":
		remotePath := getString(args, "remote_path", "")
		if remotePath == "" {
			return toolError(CodeInvalidArgument, "%s", "statvfs requires remote_path"), nil
		}
		result, err := sftpCli.StatVFS(remotePath)
		if err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return jsonResult(toMap(result)), nil
	default:
		return toolError(CodeInvalidArgument, "%s", "action must be truncate, realpath, or statvfs"), nil
	}
	return successResult(), nil
}

// handleFileGetwd returns the remote working directory via SSH/SFTP.
func (s *Server) handleFileGetwd(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := strings.TrimSpace(getString(args, "session_id", ""))

	if sessionID == "" {
		return toolError(CodeInvalidArgument, "%s", "session_id required"), nil
	}

	sftpCli, bad := s.sftpClient(sessionID)
	if bad != nil {
		return bad, nil
	}
	defer sftpCli.Close()
	dir, err := sftpCli.Getwd()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
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
