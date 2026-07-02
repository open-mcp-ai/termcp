package mcp

import (
	"context"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/forward"
)

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. This nudges weak models to chain tool calls instead
// of stopping after narrative plans; compliance still depends on the host client + model.
const mcpServerInstructions = `termcp agent rules (follow in order until the user's task is done or a tool returns a hard error):

1) Multi-step work: after list_sessions, list_ssh_configs, start_session, or any discovery tool, you MUST continue with concrete tool calls (send_input, read_output, send_and_read, background_send, etc.). Do not end the turn with only a prose "here is what you would run" plan.

2) Never invent session_id. Use session_id from list_sessions or start_session only.

3) Shell output is not visible until you call read_output (or send_and_read). If a command may run longer than a few seconds, use background_send + read_output with a short timeout instead of send_and_read with a long timeout.

4) Do not assert that a remote command succeeded unless tool results contain the terminal output or an explicit success signal.

5) When managing multiple sessions, poll with read_output timeout ≤ 3s in round-robin; terminate_session then delete_session when finished.

6) Passwords and secrets: If read_output shows a password prompt, sudo password, passphrase, MFA/2FA, or SSH keyboard-interactive challenge, do NOT guess, brute-force, or paste secrets you do not have. Stop automated send_input/background_send for that secret and tell the user to type it in the termcp Web UI terminal for that same session (the browser session tied to that session_id). Only continue with non-secret commands after the user confirms they entered it.

7) send_input control bytes (function keys, ESC, arrows, Ctrl): JSON has no \\xNN escapes — never use "\\x1b" in text (invalid JSON or four literal characters). Use JSON \\uXXXX only (e.g. F12 xterm-256color: \\u001b[24~). press_enter must be false for key sequences.

8) Crash-loop detection: if read_output returns a large traceback repeating the same error pattern (e.g. Python _pyrepl / fancy_termios with termios.error or recursion), the process is in an unrecoverable loop. Immediately terminate_session, then retry with PYTHON_BASIC_REPL=1 in the process environment. If a command succeeds but then hangs (output goes silent while session stays running), check get_session_info for status and decide whether to wait or terminate.`

// Server wraps the MCP SSE server, streamable HTTP handler, and tool handlers.
type Server struct {
	mcpServer    *mcpserver.MCPServer
	sseServer    *mcpserver.SSEServer
	streamServer *mcpserver.StreamableHTTPServer
	sessMgr      *session.Manager
	msgMgr       *message.Manager
	sshConfigs   *sshconfig.Store
	forwardMgr   *forward.ForwardManager
	baseURL      string // http://host:port, set from Start()
}

// New creates and configures the MCP server with all tools registered.
// sshConfigs may be nil (start_session / list_ssh_configs will error or return empty).
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
	mcpServer.AddTool(mcpgo.NewTool("start_session",
		mcpgo.WithDescription("Start a long-lived interactive shell or command on the termcp server. Connection profiles are stored server-side as data-dir/ssh_configs/<ssh_config>/config.json (no file paths in this tool—only the profile folder name). Call list_ssh_configs first to see valid names. Use ssh_config \"internal\" (or omit/empty) for the built-in loopback session on the machine running termcp; use any other name for SSH to a remote host (kind \"remote\" in JSON). If name is omitted or blank, the session’s display name defaults to ssh_config so lists match the Web UI when a user clicks a connection tile. Returns JSON keys: session_id (opaque id for all later calls), pid, ssh_config; initial_output is always empty—read terminal text with read_output. Leave command and args empty for the remote user’s login shell (SSH) or the server default shell (internal); optional default_shell / default_mode in the profile JSON override defaults."),
		mcpgo.WithString("command", mcpgo.Description("Executable or shell builtin line; leave empty with no args for login shell / profile default_shell")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command; only valid when command is non-empty"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("pty: pseudo-terminal (interactive TUI); pipe: no TTY, line-oriented"), mcpgo.DefaultString("pty")),
		mcpgo.WithString("name", mcpgo.Description("Optional label shown in session lists. If omitted, defaults to ssh_config (same behavior as the Web UI). Set only when you need multiple concurrent sessions per profile with distinct labels.")),
		mcpgo.WithNumber("rows", mcpgo.Description("Initial PTY height (also sent to remote SSH PTY)"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("Initial PTY width"), mcpgo.DefaultNumber(80)),
		mcpgo.WithString("ssh_config", mcpgo.Description("Profile name: subdirectory under data-dir/ssh_configs. Empty or omitted means \"internal\" (loopback on termcp host).")),
	), withLogging("start_session", s.handleStartSession))

	mcpServer.AddTool(mcpgo.NewTool("start_subshell",
		mcpgo.WithDescription("Open a new shell channel on an existing SSH connection. The parent session's SSH transport is reused — no new TCP connection or handshake. Returns the child shell ID which can be used with send_input/read_output/etc. just like a regular session ID."),
		mcpgo.WithString("parent_session_id", mcpgo.Required(), mcpgo.Description("Parent session ID (must be a remote SSH session)")),
		mcpgo.WithString("name", mcpgo.Description("Optional display name for this shell tab")),
		mcpgo.WithString("command", mcpgo.Description("Executable; leave empty for login shell")),
		mcpgo.WithString("mode", mcpgo.Description("pty (default) or pipe"), mcpgo.DefaultString("pty")),
		mcpgo.WithNumber("rows", mcpgo.Description("PTY rows"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("PTY cols"), mcpgo.DefaultNumber(80)),
	), withLogging("start_subshell", s.handleStartSubShell))

	mcpServer.AddTool(mcpgo.NewTool("list_subshells",
		mcpgo.WithDescription("List the running child shells (channels) sharing a parent session's SSH connection. Returns parent_session_id and subshells (each with id, name, status, timestamps). Use the returned ids with send_input/read_output/close_shell. list_sessions only returns parent sessions — call this to enumerate a session's channels."),
		mcpgo.WithString("parent_session_id", mcpgo.Required(), mcpgo.Description("Parent session ID from start_session / list_sessions")),
	), withLogging("list_subshells", s.handleListSubshells))

	mcpServer.AddTool(mcpgo.NewTool("close_shell",
		mcpgo.WithDescription("Close one shell channel without tearing down the parent session. Pass a parent session_id to close its root shell channel (remote only — no-op for internal sessions, the process keeps running); pass a child shell id (from list_subshells) to close just that channel. The SSH connection and other shells keep running. To fully stop a session, use terminate_session instead."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("A parent session id (closes root channel) or a child shell id (closes that channel)")),
	), withLogging("close_shell", s.handleCloseShell))

	mcpServer.AddTool(mcpgo.NewTool("send_input",
		mcpgo.WithDescription("Write bytes to the session’s stdin only; does not wait for or return output. Use read_output (same reader_id) afterward. For PTY sessions, press_enter appends a newline so the shell executes the line. Terminal control bytes: server rule 7."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to write")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("If true, append \\n after text (common for shell commands)"), mcpgo.DefaultBool(false)),
	), withLogging("send_input", s.handleSendInput))

	mcpServer.AddTool(mcpgo.NewTool("read_output",
		mcpgo.WithDescription("Return newly produced stdout/stderr since the last read on the given reader_id. Each reader_id maintains its own cursor so multiple agents can observe the same session without stealing each other’s data (register_reader for ids > 0). When juggling several sessions, use timeout ≤ 3s and poll sessions in round-robin so one slow session does not block others. Response JSON: output (string), has_more (bool), lines_returned, bytes_returned."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("If true, strip ANSI SGR/cursor escapes for plain-text logs"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Blocking wait for new output, in seconds (0.1–60)"), mcpgo.DefaultNumber(5)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Truncate after N newline-delimited lines; 0 = no line limit"), mcpgo.DefaultNumber(0)),
			mcpgo.WithNumber("max_bytes", mcpgo.Description("Max bytes to return per call; 0 = no limit. Use with has_more to paginate large output."), mcpgo.DefaultNumber(8192)),
		mcpgo.WithNumber("reader_id", mcpgo.Description("0 = default reader shared with Web UI stream unless you registered another reader"), mcpgo.DefaultNumber(0)),
	), withLogging("read_output", s.handleReadOutput))

	mcpServer.AddTool(mcpgo.NewTool("background_send",
		mcpgo.WithDescription("Same as send_input but explicitly intended for fire-and-forget writes: returns immediately after enqueueing. Prefer this plus read_output over send_and_read for long-running commands (builds, package installs) so the MCP call does not block for the full timeout."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to write")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("If true, append \\n after text"), mcpgo.DefaultBool(false)),
	), withLogging("background_send", s.handleBackgroundSend))

	mcpServer.AddTool(mcpgo.NewTool("send_and_read",
		mcpgo.WithDescription("Convenience: send_input then read_output in one tool call. Blocks until the first chunk of new output or timeout—dangerous for slow commands because the model waits the entire timeout. For anything that may run longer than a few seconds, use background_send + read_output with short timeouts instead."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to send before reading")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("If true, append \\n after text"), mcpgo.DefaultBool(false)),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("Passed through to read_output"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Seconds to wait for output after send (0.1–60)"), mcpgo.DefaultNumber(5)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Passed through to read_output; 0 = unlimited"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("reader_id", mcpgo.Description("Passed through to read_output"), mcpgo.DefaultNumber(0)),
	), withLogging("send_and_read", s.handleSendAndRead))

	mcpServer.AddTool(mcpgo.NewTool("list_sessions",
		mcpgo.WithDescription("Return metadata for every running parent session currently in the server registry. Exited sessions are removed automatically — no need to call delete_session after terminate. Each entry includes id, name, status, ssh_endpoint, and timestamps. Child shells (created via start_subshell) are NOT included — use list_subshells to enumerate a session's channels."),
	), withLogging("list_sessions", s.handleListSessions))
	mcpServer.AddTool(mcpgo.NewTool("get_session_info",
		mcpgo.WithDescription("Return a JSON document with detailed fields for one session: identifiers, command line, mode, PTY size, ssh_config, remote connection metadata, exit state, etc. Use for debugging or before resize_pty / terminate_session."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
	), withLogging("get_session_info", s.handleGetSessionInfo))

	mcpServer.AddTool(mcpgo.NewTool("terminate_session",
		mcpgo.WithDescription("Fully stop a session: SIGTERM (then SIGKILL after grace_period) the process and close the SSH connection. The session is auto-removed from the registry once it exits. To close just one shell channel and keep the session alive, use close_shell instead."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithBoolean("force", mcpgo.Description("If true, end immediately without honoring grace_period"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to allow after SIGTERM before hard close when force is false (0–60)"), mcpgo.DefaultNumber(5)),
	), withLogging("terminate_session", s.handleTerminateSession))

	mcpServer.AddTool(mcpgo.NewTool("delete_session",
		mcpgo.WithDescription("Remove a session from the in-memory registry. Sessions are auto-removed when they exit, so this is rarely needed — use it only to drop a still-registered session that should be gone. Fails if the session is still running (terminate_session first)."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
	), withLogging("delete_session", s.handleDeleteSession))

	mcpServer.AddTool(mcpgo.NewTool("resize_pty",
		mcpgo.WithDescription("Update PTY rows/cols for an existing PTY session (propagates to SSH remote PTY when applicable). Call when the agent’s logical terminal size changes; harmless for pipe mode sessions depending on server validation."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithNumber("rows", mcpgo.Description("New row count (typical 24–60)"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("New column count (typical 80–200)"), mcpgo.DefaultNumber(80)),
	), withLogging("resize_pty", s.handleResizePty))

	mcpServer.AddTool(mcpgo.NewTool("detect_shell",
		mcpgo.WithDescription("Probe the termcp host (not an arbitrary ssh_config) for a suitable interactive shell: returns executable path, family enum (unix, powershell, cmd), and a short hint string. Use before crafting start_session command/args when targeting mixed Windows/Linux environments from the same MCP client."),
	), withLogging("detect_shell", s.handleDetectShell))

	mcpServer.AddTool(mcpgo.NewTool("list_ssh_configs",
		mcpgo.WithDescription("Return the sorted list of profile names that may be passed as ssh_config to start_session—one entry per directory under data-dir/ssh_configs plus the built-in \"internal\" profile. Does not return JSON bodies, secrets, or hostnames; only safe names for discovery."),
	), withLogging("list_ssh_configs", s.handleListSSHConfigs))

	mcpServer.AddTool(mcpgo.NewTool("list_messages",
		mcpgo.WithDescription("List stored MCP/chat message index entries associated with a session_id (message persistence feature). Returns message ids and metadata for later get_message calls."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
	), withLogging("list_messages", s.handleListMessages))

	mcpServer.AddTool(mcpgo.NewTool("get_message",
		mcpgo.WithDescription("Fetch full message payloads for one or more message_ids under a session. Provide message_ids as a JSON array of strings, or a single message_id if your client maps scalar args."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithArray("message_ids", mcpgo.Description("List of message id strings from list_messages"), mcpgo.WithStringItems()),
	), withLogging("get_message", s.handleGetMessage))

	mcpServer.AddTool(mcpgo.NewTool("register_reader",
		mcpgo.WithDescription("Allocate a new output reader_id for this session. That reader only observes bytes written to the PTY **after** registration (cursor starts at the current end of the buffer—no historical backlog). Use when a second agent must not share reader 0’s cursor with another consumer; pair every read_output(..., reader_id) with the id returned here. The Web UI stream uses a different internal API to also see prior output."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
	), withLogging("register_reader", s.handleRegisterReader))

	mcpServer.AddTool(mcpgo.NewTool("unregister_reader",
		mcpgo.WithDescription("Release a reader_id previously returned by register_reader. Always unregister when done to avoid leaking buffers/state server-side."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithNumber("reader_id", mcpgo.Required(), mcpgo.Description("Non-zero reader id from register_reader")),
	), withLogging("unregister_reader", s.handleUnregisterReader))

	// --- Port forwarding tools ---
	mcpServer.AddTool(mcpgo.NewTool("forward_port",
		mcpgo.WithDescription("Local port forward (ssh -L). termcp listens on a local port and tunnels traffic through SSH to the remote target. local_port=0 picks a random free port."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_host", mcpgo.Description("Target host (relative to the remote side)"), mcpgo.DefaultString("localhost")),
		mcpgo.WithNumber("remote_port", mcpgo.Required(), mcpgo.Description("Target port on remote host")),
		mcpgo.WithNumber("local_port", mcpgo.Description("Local port to listen on (0=random)"), mcpgo.DefaultNumber(0)),
	), withLogging("forward_port", s.handleForwardPort))

	mcpServer.AddTool(mcpgo.NewTool("local_forward",
		mcpgo.WithDescription("Remote port forward (ssh -R). The remote side listens on a port and tunnels traffic back to a termcp-side target."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("local_host", mcpgo.Description("Host for the remote side to listen on"), mcpgo.DefaultString("0.0.0.0")),
		mcpgo.WithNumber("local_port", mcpgo.Required(), mcpgo.Description("Port for the remote side to listen on")),
		mcpgo.WithString("remote_host", mcpgo.Required(), mcpgo.Description("Target host (relative to termcp)")),
		mcpgo.WithNumber("remote_port", mcpgo.Required(), mcpgo.Description("Target port (relative to termcp)")),
	), withLogging("local_forward", s.handleLocalForward))

	mcpServer.AddTool(mcpgo.NewTool("dynamic_forward",
		mcpgo.WithDescription("Start a SOCKS5 proxy (ssh -D). termcp listens on a local port, proxies TCP connections through the agent/SSH connection."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithNumber("local_port", mcpgo.Description("Local port for SOCKS5 proxy (0=random)"), mcpgo.DefaultNumber(0)),
	), withLogging("dynamic_forward", s.handleDynamicForward))

	mcpServer.AddTool(mcpgo.NewTool("list_forwards",
		mcpgo.WithDescription("List all active port forwards (local, remote, and dynamic directions). Returns forward_id, direction, listen_addr, target_addr, status."),
	), withLogging("list_forwards", s.handleListForwards))

	mcpServer.AddTool(mcpgo.NewTool("close_forward",
		mcpgo.WithDescription("Close an active port forward by forward_id, releasing the listener."),
		mcpgo.WithString("forward_id", mcpgo.Required(), mcpgo.Description("Forward ID from list_forwards")),
	), withLogging("close_forward", s.handleCloseForward))

	// --- File operation tools ---
	mcpServer.AddTool(mcpgo.NewTool("file_read",
		mcpgo.WithDescription("Read a remote file or file segment via SSH/SFTP. Mode 'text' returns readable text with \\xHH escapes for non-printable bytes. Mode 'hex' returns hex dump. Mode 'file' writes to a local file on termcp. Omit offset/length for whole file read."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Start byte offset (0-based)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read (0=all)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("mode", mcpgo.Description("Output mode: text, hex, or file"), mcpgo.DefaultString("text")),
		mcpgo.WithString("local_path", mcpgo.Description("Local file path for mode=file")),
	), withLogging("file_read", s.handleFileRead))

	mcpServer.AddTool(mcpgo.NewTool("file_write",
		mcpgo.WithDescription("Write to a remote file via SSH/SFTP. Use inline data (text mode with \\xHH escapes, or hex mode) for small writes; use local_path + local_offset + length to stream from a termcp-side file for large/binary writes."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Write start offset (0=beginning, truncates if 0)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("data", mcpgo.Description("Inline data to write (text or hex per mode)")),
		mcpgo.WithString("mode", mcpgo.Description("Data encoding: text (default, supports \\xHH) or hex"), mcpgo.DefaultString("text")),
		mcpgo.WithString("local_path", mcpgo.Description("Termcp-side file path to read data from")),
		mcpgo.WithNumber("local_offset", mcpgo.Description("Read start offset in local file"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read from local file (0=all)"), mcpgo.DefaultNumber(0)),
	), withLogging("file_write", s.handleFileWrite))

	mcpServer.AddTool(mcpgo.NewTool("file_stat",
		mcpgo.WithDescription("Get file or directory info from remote via SSH/SFTP. Returns name, size, is_dir, mod_time, and children list for directories."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_stat", s.handleFileStat))

	mcpServer.AddTool(mcpgo.NewTool("file_delete",
		mcpgo.WithDescription("Delete a remote file or empty directory via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path to delete")),
	), withLogging("file_delete", s.handleFileDelete))

	mcpServer.AddTool(mcpgo.NewTool("file_rename",
		mcpgo.WithDescription("Move or rename a remote file/directory via SSH/SFTP (same filesystem)."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("from_path", mcpgo.Required(), mcpgo.Description("Current remote path")),
		mcpgo.WithString("to_path", mcpgo.Required(), mcpgo.Description("New remote path")),
	), withLogging("file_rename", s.handleFileRename))

	mcpServer.AddTool(mcpgo.NewTool("file_mkdir",
		mcpgo.WithDescription("Create a directory (and parents) on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote directory path to create")),
	), withLogging("file_mkdir", s.handleFileMakeDir))

	mcpServer.AddTool(mcpgo.NewTool("get_file_urls",
		mcpgo.WithDescription("Get HTTP download/upload URLs for a remote file path under a session. Use these URLs for direct curl/wget/browser access."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
	), withLogging("get_file_urls", s.handleGetFileURLs))

	mcpServer.AddTool(mcpgo.NewTool("file_chmod",
		mcpgo.WithDescription("Change file permissions on the remote via SSH/SFTP. mode is a decimal Unix permission (e.g. 493 = 0755)."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("mode", mcpgo.Required(), mcpgo.Description("Unix permission mode as decimal integer (e.g. 493 for 0755)")),
	), withLogging("file_chmod", s.handleFileChmod))

	mcpServer.AddTool(mcpgo.NewTool("file_chown",
		mcpgo.WithDescription("Change file owner and group on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("uid", mcpgo.Required(), mcpgo.Description("User ID (numeric)")),
		mcpgo.WithNumber("gid", mcpgo.Required(), mcpgo.Description("Group ID (numeric)")),
	), withLogging("file_chown", s.handleFileChown))

	mcpServer.AddTool(mcpgo.NewTool("file_chtimes",
		mcpgo.WithDescription("Change file access and modification timestamps on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("atime", mcpgo.Required(), mcpgo.Description("Access time as Unix timestamp (seconds)")),
		mcpgo.WithNumber("mtime", mcpgo.Required(), mcpgo.Description("Modification time as Unix timestamp (seconds)")),
	), withLogging("file_chtimes", s.handleFileChtimes))

	mcpServer.AddTool(mcpgo.NewTool("file_readlink",
		mcpgo.WithDescription("Read the target of a symbolic link on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote symlink path")),
	), withLogging("file_readlink", s.handleFileReadlink))

	mcpServer.AddTool(mcpgo.NewTool("file_symlink",
		mcpgo.WithDescription("Create a symbolic link on the remote via SSH/SFTP. target is the existing path, link_path is the new symlink to create (like 'ln -s target link_path')."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description("The existing file/directory to point to")),
		mcpgo.WithString("link_path", mcpgo.Required(), mcpgo.Description("The new symlink path to create")),
	), withLogging("file_symlink", s.handleFileSymlink))

	mcpServer.AddTool(mcpgo.NewTool("file_link",
		mcpgo.WithDescription("Create a hard link on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("existing_path", mcpgo.Required(), mcpgo.Description("The existing file to link to")),
		mcpgo.WithString("new_path", mcpgo.Required(), mcpgo.Description("The new hard link path to create")),
	), withLogging("file_link", s.handleFileLink))

	mcpServer.AddTool(mcpgo.NewTool("file_truncate",
		mcpgo.WithDescription("Truncate a remote file to a given size via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("size", mcpgo.Required(), mcpgo.Description("New file size in bytes")),
	), withLogging("file_truncate", s.handleFileTruncate))

	mcpServer.AddTool(mcpgo.NewTool("file_realpath",
		mcpgo.WithDescription("Resolve the canonical absolute path on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_realpath", s.handleFileRealpath))

	mcpServer.AddTool(mcpgo.NewTool("file_statvfs",
		mcpgo.WithDescription("Get filesystem statistics (disk space, inodes) for a remote path via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path (must exist)")),
	), withLogging("file_statvfs", s.handleFileStatVFS))

	mcpServer.AddTool(mcpgo.NewTool("file_getwd",
		mcpgo.WithDescription("Get the remote working directory via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from start_session")),
	), withLogging("file_getwd", s.handleFileGetwd))

	s.mcpServer = mcpServer
	s.sseServer = mcpserver.NewSSEServer(mcpServer, sseOpts...)
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	// Do not use WithStreamableHTTPServer(mainSrv) here — Shutdown must not close the shared listener.
	s.streamServer = mcpserver.NewStreamableHTTPServer(mcpServer)
	return s
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
