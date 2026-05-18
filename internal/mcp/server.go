package mcp

import (
	"context"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
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
}

// New creates and configures the MCP server with all tools registered.
// sshConfigs may be nil (start_session / list_ssh_configs will error or return empty).
// sseOpts are passed to the underlying mcp-go SSE server (e.g. mcpserver.WithHTTPServer).
func New(sessMgr *session.Manager, msgMgr *message.Manager, sshConfigs *sshconfig.Store, sseOpts ...mcpserver.SSEOption) *Server {
	s := &Server{
		sessMgr:    sessMgr,
		msgMgr:     msgMgr,
		sshConfigs: sshConfigs,
	}

	mcpServer := mcpserver.NewMCPServer("termcp", "0.0.4",
		mcpserver.WithInstructions(mcpServerInstructions),
	)
	mcpServer.AddTool(mcpgo.NewTool("start_session",
		mcpgo.WithDescription("Start a long-lived interactive shell or command on the termcp server. Connection profiles are stored server-side as data-dir/ssh_configs/<ssh_config>/config.json (no file paths in this tool—only the profile folder name). Call list_ssh_configs first to see valid names. Use ssh_config \"internal\" (or omit/empty) for the built-in loopback session on the machine running termcp; use any other name for SSH to a remote host (kind \"remote\" in JSON). Remote file tools (upload_file, download_file, list_files) reuse the same SSH connection and require SFTP to be available on that host. If name is omitted or blank, the session’s display name defaults to ssh_config so lists match the Web UI when a user clicks a connection tile. Returns JSON keys: session_id (opaque id for all later calls), pid, ssh_config; initial_output is always empty—read terminal text with read_output. Leave command and args empty for the remote user’s login shell (SSH) or the server default shell (internal); optional default_shell / default_mode in the profile JSON override defaults."),
		mcpgo.WithString("command", mcpgo.Description("Executable or shell builtin line; leave empty with no args for login shell / profile default_shell")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command; only valid when command is non-empty"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("pty: pseudo-terminal (interactive TUI); pipe: no TTY, line-oriented"), mcpgo.DefaultString("pty")),
		mcpgo.WithString("name", mcpgo.Description("Optional label shown in session lists. If omitted, defaults to ssh_config (same behavior as the Web UI). Set only when you need multiple concurrent sessions per profile with distinct labels.")),
		mcpgo.WithNumber("rows", mcpgo.Description("Initial PTY height (also sent to remote SSH PTY)"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("Initial PTY width"), mcpgo.DefaultNumber(80)),
		mcpgo.WithString("ssh_config", mcpgo.Description("Profile name: subdirectory under data-dir/ssh_configs. Empty or omitted means \"internal\" (loopback on termcp host).")),
	), withLogging("start_session", s.handleStartSession))

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
		mcpgo.WithDescription("Return metadata for every session still in the server registry: running sessions and exited ones until delete_session removes them. Each item includes id, display name (defaults to the ssh profile name), status, ssh_config, and timestamps—use this to pick session_id before read_output or terminate_session."),
	), withLogging("list_sessions", s.handleListSessions))

	mcpServer.AddTool(mcpgo.NewTool("get_session_info",
		mcpgo.WithDescription("Return a JSON document with detailed fields for one session: identifiers, command line, mode, PTY size, ssh_config, remote connection metadata, exit state, etc. Use for debugging or before resize_pty / terminate_session."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
	), withLogging("get_session_info", s.handleGetSessionInfo))

	mcpServer.AddTool(mcpgo.NewTool("terminate_session",
		mcpgo.WithDescription("Stop the remote process / close pipes for this session (SIGTERM then optional SIGKILL path). The session row may remain until you call delete_session to drop registry metadata and free the name slot for bookkeeping."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id returned by start_session")),
		mcpgo.WithBoolean("force", mcpgo.Description("If true, end immediately without honoring grace_period"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to allow after SIGTERM before hard close when force is false (0–60)"), mcpgo.DefaultNumber(5)),
	), withLogging("terminate_session", s.handleTerminateSession))

	mcpServer.AddTool(mcpgo.NewTool("delete_session",
		mcpgo.WithDescription("Remove a session from the in-memory registry after it has exited (or been terminated). Call terminate_session first if the process is still running. Fails if the session is still active—check get_session_info / list_sessions."),
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
