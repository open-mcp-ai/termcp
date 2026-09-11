package mcp

import (
	"context"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/history"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. Keep it terse because clients may include it
// in every model turn.
const mcpServerInstructions = `termcp agent rules:

0) Tools: session_*/shell_* are standalone; forward/message/history/ssh_config and file_perm/file_link/file_fs take an "action" parameter (enum in each schema).
1) IDs: session_id = connection container (forwards, files, terminate, shell_open); shell_id = terminal channel (input/key/output/resize/close, readers). Never invent them; take from session_start / shell_open / list tools.
2) Mode selection:
   - Interactive shell (omit command/args, DEFAULT): For multi-step tasks, stateful work (cd/env), and CLI sessions. Drive: loop shell_input(shell_id,text) + shell_key(shell_id,key="enter") + shell_output(shell_id,timeout≤3) until prompt. shell_output returns ONLY new bytes: empty read ≠ done, keep polling (echo precedes output).
   - Dedicated command (set command/args): ONLY for: (a) interactive REPL/TUI (python, psql, htop); (b) long-running daemon/server (npm start, server binary); (c) isolated atomic script needing process exit code.
   - Anti-pattern: Never split sequential steps into multiple session_start(bash -c) calls (loses cwd/env, wastes SSH handshakes, fragments history).
3) After discovery, act with concrete calls, not prose. Verify success via output or an explicit success field.
4) Lifecycle: session_terminate closes shells+forwards and archives; archived output is read with shell_output (same cursor semantics as live, use tail_lines/offset); history(action=screenshot) renders archived output as PNG; history(action=purge) deletes it. force=true = immediate kill. shell_close closes one channel.
5) Password/sudo/passphrase/MFA prompt: stop and ask user to type it in termcp Web UI. Never guess, paste, or echo secrets.
6) Other keys use JSON \u001b escapes in shell_input. Repeating traceback → session_terminate, retry with PYTHON_BASIC_REPL=1. Silent hang → session_info.
7) forward(action=local/remote/dynamic) = ssh -L/-R/-D, all take session_id. ssh_config(action=list) only returns names; never expose credentials.`

// Server wraps the MCP SSE server, streamable HTTP handler, and tool handlers.
type Server struct {
	mcpServer       *mcpserver.MCPServer
	sseServer       *mcpserver.SSEServer
	streamServer    *mcpserver.StreamableHTTPServer
	sessMgr         *session.Manager
	msgMgr          *message.Manager
	historyMgr      *history.Manager
	sshConfigs      *sshconfig.Store
	forwardMgr      *forward.ForwardManager
	baseURL         string // http://host:port, set from Start()
	NoInternal      bool   // when true, hide and refuse the built-in loopback profile
	sshConfigWrites bool   // expose write actions on the unified ssh_config tool
}

// SetHistory attaches the archived-session history manager (list/transcript/search/purge tools).
func (s *Server) SetHistory(h *history.Manager) {
	s.historyMgr = h
}

// New creates and configures the MCP server with all tools registered.
// sshConfigs may be nil (session_start / ssh_config(action=list) will error or return empty).
// sseOpts are passed to the underlying mcp-go SSE server (e.g. mcpserver.WithHTTPServer).
func New(sessMgr *session.Manager, msgMgr *message.Manager, sshConfigs *sshconfig.Store, forwardMgr *forward.ForwardManager, sseOpts ...mcpserver.SSEOption) *Server {
	s := &Server{
		sessMgr:    sessMgr,
		msgMgr:     msgMgr,
		sshConfigs: sshConfigs,
		forwardMgr: forwardMgr,
	}

	mcpServer := mcpserver.NewMCPServer("termcp", "0.0.4",
		mcpserver.WithInstructions(mcpServerInstructions),
	)
	mcpServer.AddTool(newTool("session_start",
		mcpgo.WithDescription("Start a session (connection container) plus its primary shell. ssh_config \"internal\" (default) = termcp host loopback; otherwise a remote profile name. Empty command/args = login shell / profile defaults. WARNING: command/args = single run-and-exit program; for multi-step or stateful work omit them and drive an interactive shell instead. Returns session_id and shell_id."),
		mcpgo.WithString("command", mcpgo.Description("Executable line; empty with no args = login shell / profile default_shell")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("\"pty\" (default, interactive TUI) or \"pipe\" (no TTY, line-oriented)"), mcpgo.DefaultString("pty")),
		mcpgo.WithString("name"),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
		mcpgo.WithString("ssh_config"),
	), withLogging("session_start", s.handleStartSession))

	mcpServer.AddTool(newTool("shell_open",
		mcpgo.WithDescription("Open another shell channel on an existing session connection (reuses SSH transport). Returns shell_id for I/O and session_id of the parent."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("name"),
		mcpgo.WithString("command", mcpgo.Description("Executable; leave empty for login shell")),
		mcpgo.WithString("mode", mcpgo.Description("pty (default) or pipe"), mcpgo.DefaultString("pty")),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
	), withLogging("shell_open", s.handleStartSubShell))

	mcpServer.AddTool(newTool("shell_list",
		mcpgo.WithDescription("List shell channels on a session: session_id + shells (id, name, status, timestamps). Use ids with shell_input/shell_key/shell_output/shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("shell_list", s.handleListSubshells))

	mcpServer.AddTool(newTool("shell_close",
		mcpgo.WithDescription("Close one shell channel by shell_id without tearing down the session. For internal primary shell, close is a no-op (process outlives the tab). Use session_terminate to stop the whole session."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
	), withLogging("shell_close", s.handleCloseShell))

	mcpServer.AddTool(newTool("shell_input",
		mcpgo.WithDescription("Write text bytes to a shell's stdin. Follow with shell_key(key=\"enter\") to execute the typed line, then shell_output for the result."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to write (no automatic newline)")),
	), withLogging("shell_input", s.handleSendInput))

	mcpServer.AddTool(newTool("shell_key",
		mcpgo.WithDescription("Send a named key to a shell. Supported: enter, tab, esc, up/down/left/right, backspace, delete, home, end, ctrl+c/d/z/l/u/w. Use enter after shell_input to run a command."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("Named key (e.g. enter, ctrl+c, up)")),
		mcpgo.WithNumber("repeat", mcpgo.Description("Times to send the key (1–20)"), mcpgo.DefaultNumber(1)),
	), withLogging("shell_key", s.handlePressKey))

	mcpServer.AddTool(newTool("shell_output",
		mcpgo.WithDescription("Unified output reader for live AND archived/dead shells with one byte-stream cursor model. shell_id may be a shell_id or session_id. Default: live = new output since the last read on reader_id (blocking up to timeout); archived = recent tail. tail_lines=N returns the last N lines; offset>=0 reads raw bytes from that position (stateless paging with has_more). Returns {output, has_more, lines_returned, bytes_returned, start_offset, end_offset, total_bytes, source, session_id, shell_id, session_status, session_uptime_seconds?}."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("If true, strip ANSI SGR/cursor escapes and compress terminal noise"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Blocking wait for new output on LIVE shells, in seconds (0–60); 0 = non-blocking; ignored for archived reads"), mcpgo.DefaultNumber(3)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Return at most N newline-terminated lines (from the read window); 0 = no line limit"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Max raw bytes per call (from offset or tail); 0 = no limit. Use with start_offset/end_offset/has_more to paginate"), mcpgo.DefaultNumber(8192)),
		mcpgo.WithNumber("offset", mcpgo.Description("Raw byte position to start reading; -1 = reader cursor (live, default) / tail (archived)"), mcpgo.DefaultNumber(-1)),
		mcpgo.WithNumber("tail_lines", mcpgo.Description("Return only the last N lines of the stream (overrides offset); 0 = off"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("reader_id", mcpgo.DefaultNumber(0)),
	), withLogging("shell_output", s.handleReadOutput))

	mcpServer.AddTool(newTool("session_list",
		mcpgo.WithDescription("Return metadata for every running parent session (exited ones are auto-removed). Child shells excluded — use shell_list."),
	), withLogging("session_list", s.handleListSessions))
	mcpServer.AddTool(newTool("session_info",
		mcpgo.WithDescription("Return a JSON document with detailed fields for one session: identifiers, command line, mode, PTY size, remote connection metadata, exit state, etc."),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("session_info", s.handleGetSessionInfo))

	mcpServer.AddTool(newTool("session_terminate",
		mcpgo.WithDescription("Stop and archive a session: terminate all shells, close SSH, cascade forwards, drop registry entry. Output stays readable via shell_output (same cursor semantics as live; tail_lines/offset); history(action=purge) deletes it permanently. force=true = immediate kill; force=false waits grace_period after SIGTERM. To close one shell only, use shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithBoolean("force", mcpgo.Description("If true, end immediately without honoring grace_period"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to allow after SIGTERM before hard close when force is false (0–60)"), mcpgo.DefaultNumber(5)),
	), withLogging("session_terminate", s.handleTerminateSession))

	mcpServer.AddTool(newTool("shell_resize",
		mcpgo.WithDescription("Update PTY rows/cols for a shell channel (propagates to SSH remote PTY when applicable)."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithNumber("rows", mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.DefaultNumber(80)),
	), withLogging("shell_resize", s.handleResizePty))

	mcpServer.AddTool(newTool("shell_detect",
		mcpgo.WithDescription("Probe the termcp host (not a remote ssh_config) for an interactive shell: returns path, family (unix/powershell/cmd), and a hint."),
	), withLogging("shell_detect", s.handleDetectShell))

	mcpServer.AddTool(newTool("ssh_config",
		mcpgo.WithDescription("SSH connection profiles: action=list returns usable profile names for session_start (never secrets or hostnames)."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list")),
	), withLogging("ssh_config", s.handleSSHConfigOps))

	mcpServer.AddTool(newTool("message",
		mcpgo.WithDescription("Stored session messages: action=list returns the message index; action=get returns full payloads for message_ids."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list", "get")),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithArray("message_ids", mcpgo.Description("get: ids from message(action=list)"), mcpgo.WithStringItems()),
	), withLogging("message", s.handleMessageOps))

	mcpServer.AddTool(newTool("shell_reader_register",
		mcpgo.WithDescription("Allocate a new output reader_id for a shell, observing only bytes written after registration (no backlog). Pair every shell_output(..., reader_id) with the returned id."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
	), withLogging("shell_reader_register", s.handleRegisterReader))

	mcpServer.AddTool(newTool("history",
		mcpgo.WithDescription("Archived sessions: action(list) all; search_messages (query); rename_session; update_session_meta (notes/tags); purge (delete permanently); screenshot (PNG URL, start/lines/cols/theme). Output reading is shell_output's job (it works on archived shells)."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("list", "search_messages", "rename_session", "update_session_meta", "purge", "screenshot")),
		mcpgo.WithString("session_id", mcpgo.Description("Archived session id; required except list/search_messages")),
		mcpgo.WithString("query", mcpgo.Description("search_messages: case-insensitive substring")),
		mcpgo.WithNumber("limit", mcpgo.Description("search_messages: max snippets"), mcpgo.DefaultNumber(50)),
		mcpgo.WithString("name", mcpgo.Description("rename_session: new display name")),
		mcpgo.WithString("notes", mcpgo.Description("update_session_meta: notes to set")),
		mcpgo.WithArray("tags", mcpgo.Description("update_session_meta: tags to set"), mcpgo.WithStringItems()),
		mcpgo.WithNumber("start", mcpgo.Description("screenshot: first display line"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("lines", mcpgo.Description("screenshot: lines to render; 0 = all"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("cols", mcpgo.Description("screenshot: terminal width"), mcpgo.DefaultNumber(80)),
		mcpgo.WithString("theme", mcpgo.Description("screenshot: dark|light"), mcpgo.DefaultString("dark")),
	), withLogging("history", s.handleHistoryOps))

	mcpServer.AddTool(newTool("shell_reader_unregister",
		mcpgo.WithDescription("Release a reader_id previously returned by shell_reader_register."),
		mcpgo.WithString("shell_id", mcpgo.Required()),
		mcpgo.WithNumber("reader_id", mcpgo.Required(), mcpgo.Description("Non-zero reader id from shell_reader_register")),
	), withLogging("shell_reader_unregister", s.handleUnregisterReader))

	// --- Port forwarding: one entry, action selects mode (OpenSSH names) ---
	mcpServer.AddTool(newTool("forward",
		mcpgo.WithDescription("SSH port forwarding: action(local) = ssh -L termcp hears on local_port→remote_host:remote_port; action(remote) = ssh -R (server listens on local_host:local_port → remote_host:remote_port reachable from termcp); action(dynamic) = ssh -D SOCKS5 (local_port, 0 = random); action(list) all forwards; action(close) by forward_id."),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("local", "remote", "dynamic", "list", "close")),
		mcpgo.WithString("session_id", mcpgo.Description("For local/remote/dynamic; from session_start")),
		mcpgo.WithString("remote_host", mcpgo.Description("local: target host relative to the SSH server"), mcpgo.DefaultString("localhost")),
		mcpgo.WithNumber("remote_port", mcpgo.Description("local: target port on remote host")),
		mcpgo.WithNumber("local_port", mcpgo.Description("local/dynamic: local listen port (0 = random)")),
		mcpgo.WithString("local_host", mcpgo.Description("remote: bind address for the SSH server listener"), mcpgo.DefaultString("0.0.0.0")),
		mcpgo.WithString("forward_id", mcpgo.Description("close: id from action(list)")),
	), withLogging("forward", s.handleForwardOps))
	// --- File operation tools (session-scoped SFTP) ---
	mcpServer.AddTool(newTool("file_read",
		mcpgo.WithDescription("Read a remote file via SSH/SFTP. mode text = printable with \\xHH escapes; hex = hex dump; file = download to the termcp host. text/hex reads return at most 8 MiB per call: page with offset + has_more/total_size from the result. mode=file streams the whole file (omit offset/length). Example: read first 1KB hex of /var/log/syslog — {session_id, remote_path, mode:\"hex\", offset:0, length:1024}."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Start byte offset (0-based)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read (0 = rest of file; text/hex capped at 8 MiB per call)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("mode", mcpgo.Description("Output mode: text, hex, or file"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex", "file")),
		mcpgo.WithString("local_path", mcpgo.Description("Download destination path on the termcp host. Only used with mode=file.")),
	), withLogging("file_read", s.handleFileRead))

	mcpServer.AddTool(newTool("file_write",
		mcpgo.WithDescription("Write a remote file via SSH/SFTP. Small writes: inline data (text with \\xHH, or hex). Large/binary: local_path + local_offset + length streams from a file on the termcp host. offset>0 writes without truncating (use file_stat size to append)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Write start offset: 0 rewrites the file from the beginning (truncates); >0 writes at that byte position without truncating (to append, set offset to the current file size from file_stat)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("data", mcpgo.Description("Inline data to write (text or hex per mode)")),
		mcpgo.WithString("mode", mcpgo.Description("Data encoding: text (default, supports \\xHH) or hex"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex")),
		mcpgo.WithString("local_path", mcpgo.Description("Source file path on the termcp host")),
		mcpgo.WithNumber("local_offset", mcpgo.Description("Read start offset in local file"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read from local file (0=all)"), mcpgo.DefaultNumber(0)),
	), withLogging("file_write", s.handleFileWrite))

	mcpServer.AddTool(newTool("file_stat",
		mcpgo.WithDescription("Get file or directory info from remote via SSH/SFTP. Returns name, size, is_dir, mod_time, and children list for directories."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_stat", s.handleFileStat))

	mcpServer.AddTool(newTool("file_delete",
		mcpgo.WithDescription("Delete a remote file or empty directory via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path to delete")),
	), withLogging("file_delete", s.handleFileDelete))

	mcpServer.AddTool(newTool("file_rename",
		mcpgo.WithDescription("Move or rename a remote file/directory via SSH/SFTP (same filesystem)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("from_path", mcpgo.Required(), mcpgo.Description("Current remote path")),
		mcpgo.WithString("to_path", mcpgo.Required(), mcpgo.Description("New remote path")),
	), withLogging("file_rename", s.handleFileRename))

	mcpServer.AddTool(newTool("file_mkdir",
		mcpgo.WithDescription("Create a directory (and parents) on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote directory path to create")),
	), withLogging("file_mkdir", s.handleFileMakeDir))

	mcpServer.AddTool(newTool("file_urls",
		mcpgo.WithDescription("Get HTTP download/upload URLs for a remote file path under a session. Use these URLs for direct curl/wget/browser access."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
	), withLogging("file_urls", s.handleGetFileURLs))

	// --- Low-frequency file operations: one tool per parameter family, ---
	// dispatched by the `action` enum. This keeps rare operations available
	// (SFTP works uniformly on Windows/Unix) without bloating tools/list.
	mcpServer.AddTool(newTool("file_perm",
		mcpgo.WithDescription("Ownership/metadata ops on a remote path: chmod (mode, decimal perms), chown (uid+gid), chtimes (atime+mtime Unix seconds)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("chmod", "chown", "chtimes")),
		mcpgo.WithString("remote_path", mcpgo.Required()),
		mcpgo.WithNumber("mode", mcpgo.Description("chmod: decimal Unix perms, e.g. 493 = 0755")),
		mcpgo.WithNumber("uid", mcpgo.Description("chown: numeric user ID")),
		mcpgo.WithNumber("gid", mcpgo.Description("chown: numeric group ID")),
		mcpgo.WithNumber("atime", mcpgo.Description("chtimes: access time, Unix seconds")),
		mcpgo.WithNumber("mtime", mcpgo.Description("chtimes: modification time, Unix seconds")),
	), withLogging("file_perm", s.handleFilePerm))

	mcpServer.AddTool(newTool("file_link",
		mcpgo.WithDescription("Link ops: readlink (remote_path → {target}), symlink (target + link_path), link/hardlink (existing_path + new_path)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("readlink", "symlink", "link")),
		mcpgo.WithString("remote_path", mcpgo.Description("readlink: symlink path")),
		mcpgo.WithString("target", mcpgo.Description("symlink: existing path to point to")),
		mcpgo.WithString("link_path", mcpgo.Description("symlink: new symlink path")),
		mcpgo.WithString("existing_path", mcpgo.Description("link: existing file")),
		mcpgo.WithString("new_path", mcpgo.Description("link: new hard link path")),
	), withLogging("file_link", s.handleFileLinkOp))

	mcpServer.AddTool(newTool("file_fs",
		mcpgo.WithDescription("Path/filesystem ops on a remote path: truncate (size bytes), realpath ({canonical_path}), statvfs (space/inodes)."),
		mcpgo.WithString("session_id", mcpgo.Required()),
		mcpgo.WithString("action", mcpgo.Required(), mcpgo.Enum("truncate", "realpath", "statvfs")),
		mcpgo.WithString("remote_path", mcpgo.Required()),
		mcpgo.WithNumber("size", mcpgo.Description("truncate: new size in bytes")),
	), withLogging("file_fs", s.handleFileFsOp))

	mcpServer.AddTool(newTool("file_getwd",
		mcpgo.WithDescription("Get the SFTP working directory for this session connection. This is not the interactive shell's cwd (pwd); use a shell command for that."),
		mcpgo.WithString("session_id", mcpgo.Required()),
	), withLogging("file_getwd", s.handleFileGetwd))

	s.mcpServer = mcpServer
	s.sseServer = mcpserver.NewSSEServer(mcpServer, sseOpts...)
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	// Do not use WithStreamableHTTPServer(mainSrv) here — Shutdown must not close the shared listener.
	s.streamServer = mcpserver.NewStreamableHTTPServer(mcpServer)
	return s
}

// RegisterSSHConfigWriteTools upgrades the ssh_config tool schema in place with
// the write-capable actions (create/edit/copy/delete). Call only when
// -mcp-manage-ssh-configs is set. The dispatcher (handleSSHConfigOps) already
// routes these actions; without the upgrade only action=list is advertised and
// the others are rejected at the schema layer.
func (s *Server) RegisterSSHConfigWriteTools() {
	s.sshConfigWrites = true
	fields := func() []mcpgo.ToolOption {
		var o []mcpgo.ToolOption
		o = append(o, mcpgo.WithString("host"), mcpgo.WithString("user"))
		o = append(o,
			mcpgo.WithString("password", mcpgo.Description("create: password auth (or private_key); edit: replace, omit to keep")),
			mcpgo.WithString("private_key", mcpgo.Description("create/edit: PEM private key content")),
			mcpgo.WithString("key_passphrase", mcpgo.Description("create/edit: passphrase for encrypted private_key")),
			mcpgo.WithBoolean("trust_unknown_host", mcpgo.DefaultBool(false)),
			mcpgo.WithString("known_hosts"),
			mcpgo.WithNumber("dial_timeout_seconds", mcpgo.DefaultNumber(30)),
			mcpgo.WithString("proxy", mcpgo.Description("SOCKS5 proxy URL, e.g. socks5://user:pass@host:port")),
			mcpgo.WithString("description"),
			mcpgo.WithString("default_shell"),
			mcpgo.WithString("default_mode", mcpgo.Description("Default mode: pty or pipe")),
			mcpgo.WithString("jump_host"),
			mcpgo.WithString("jump_user"),
			mcpgo.WithNumber("jump_port", mcpgo.DefaultNumber(22)),
			mcpgo.WithString("jump_password"),
			mcpgo.WithString("jump_private_key"),
			mcpgo.WithString("jump_key_passphrase"),
			mcpgo.WithBoolean("jump_trust_unknown_host", mcpgo.DefaultBool(false)),
			mcpgo.WithString("jump_known_hosts"),
			mcpgo.WithNumber("jump_dial_timeout_seconds", mcpgo.DefaultNumber(30)),
			mcpgo.WithString("jump_proxy"),
		)
		return o
	}

	writeActions := []string{"list", "create", "edit", "copy", "delete"}
	extra := []mcpgo.ToolOption{
		// Widen the action enum in place.
		func(t *mcpgo.Tool) {
			if prop, ok := t.InputSchema.Properties["action"].(map[string]any); ok {
				prop["enum"] = writeActions
			}
		},
		mcpgo.WithString("name", mcpgo.Description("create/edit/delete: profile name ([A-Za-z0-9_-], max 64)")),
		mcpgo.WithString("source_name", mcpgo.Description("copy: existing profile to copy from")),
		mcpgo.WithString("target_name", mcpgo.Description("copy: new profile name (must not exist)")),
	}
	extra = append(extra, fields()...)

	// Apply the extra properties onto the existing registered tool schema.
	tools := s.mcpServer.ListTools()
	st, ok := tools["ssh_config"]
	if !ok {
		panic("ssh_config tool must be registered before RegisterSSHConfigWriteTools")
	}
	t := st.Tool
	for _, opt := range extra {
		opt(&t)
	}
	t.Description = "SSH connection profiles: action=list (names only, never secrets), create, edit (patch; omitted fields keep stored values incl. secrets), copy (server-side, secrets never reach the agent), delete. create requires host+user and password or private_key."
	s.mcpServer.DeleteTools("ssh_config")
	s.mcpServer.AddTool(t, withLogging("ssh_config", s.handleSSHConfigOps))
}

// SSEHandler exposes the MCP SSE endpoint for mounting on a shared mux.
func (s *Server) SSEHandler() http.Handler {
	return s.sseServer.SSEHandler()
}

// MessageHandler exposes the MCP JSON-RPC message endpoint for mounting on a shared mux.
func (s *Server) MessageHandler() http.Handler {
	return s.sseServer.MessageHandler()
}

// StreamableHTTPHandler exposes the MCP streamable-HTTP endpoint (POST/GET/DELETE on one path).
// Mount at "/stream" (or another path with a matching wrapper); clients use e.g. http://host:port/stream.
func (s *Server) StreamableHTTPHandler() http.Handler {
	return s.streamServer
}

// Start begins serving MCP over SSE on the given address.
func (s *Server) Start(addr string) error {
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8080"
	}
	s.baseURL = "http://" + net.JoinHostPort(host, port)
	return s.sseServer.Start(addr)
}

// Stop gracefully shuts down the SSE server.
func (s *Server) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.streamServer != nil {
		_ = s.streamServer.Shutdown(ctx)
	}
	return s.sseServer.Shutdown(ctx)
}
