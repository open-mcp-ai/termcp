package mcp

import (
	"context"
	"net"
	"net/http"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// mcpServerInstructions is returned in initialize (MCP "instructions") so clients may
// inject it into the model context. This nudges weak models to chain tool calls instead
// of stopping after narrative plans; compliance still depends on the host client + model.
const mcpServerInstructions = `termcp agent rules (follow in order until the user's task is done or a tool returns a hard error):

0) Tool groups (2-level naming <group>_<action>): session_* — connection lifecycle (start/list/info/terminate); shell_* — terminal channels (open/list/close/input/key/output/resize/detect, reader_register/reader_unregister); forward_* — port forwarding (local/remote/dynamic/list/close); file_* — SFTP file operations (read/write/stat/delete/rename/mkdir/chmod/...); ssh_config_* — SSH profile management (list/create/edit/copy/delete); message_* — stored message history (list/get).

1) IDs: session_id is the connection container (forwards, files, terminate, shell_open). shell_id is a terminal channel (shell_input, shell_key, shell_output, shell_resize, shell_close, readers). Never invent either; take them from session_start / shell_open / list_*.

2) Run a command: shell_input(shell_id, text) types the command text, then shell_key(shell_id, key="enter") sends the enter key to execute it, then shell_output(shell_id, timeout≤3) reads the result. Prefer shell_key for sending special keys (enter, ctrl+c, arrows, etc.) instead of embedding raw control sequences in shell_input text. The only input/output tools are shell_input, shell_key, and shell_output.

3) After session_list, ssh_config_list, session_start, or any discovery tool, immediately proceed with concrete tool calls. A discovery result should be followed by the next action, not a prose summary.

4) Shell output is not visible until shell_output. For long commands, shell_input + shell_key(enter), then poll shell_output with short timeouts (≤3s). When managing multiple shells, poll in round-robin.

5) Verify a remote command succeeded by checking the terminal output from shell_output or an explicit success field in the tool result.

6) Lifecycle: session_terminate(session_id) closes the connection (cascades shells + forwards) and removes the session. Use force=true for immediate kill. shell_close only closes one channel.

7) Passwords and secrets: If shell_output shows a password prompt, sudo password, passphrase, MFA/2FA, or SSH keyboard-interactive challenge, stop automated input and tell the user to type the secret in the termcp Web UI terminal for that same shell. Only the user can enter secrets; the agent must not attempt to guess or paste them. Continue with non-secret commands only after the user confirms they entered it.

8) Use shell_key for enter, tab, esc, arrows, backspace, delete, home, end, ctrl+c/d/z/l/u/w. For keys not in shell_key's list, use JSON \\u001b escape sequences in shell_input — not raw \\x1b or other literal byte escapes.

9) Crash-loop detection: if shell_output returns a large traceback repeating the same error pattern (e.g. Python _pyrepl / fancy_termios with termios.error or recursion), the process is in an unrecoverable loop. Immediately session_terminate, then retry with PYTHON_BASIC_REPL=1 in the process environment. If a command succeeds but then hangs (output goes silent while session stays running), check session_info for status and decide whether to wait or terminate.

10) Forwards (OpenSSH names): forward_local = ssh -L (listen local, target remote); forward_remote = ssh -R (listen remote, target local/termcp); forward_dynamic = ssh -D (SOCKS5). All take session_id.

11) SSH configs: ssh_config_list returns only profile names (no host/user/secrets). When enabled, ssh_config_create / ssh_config_edit / ssh_config_copy / ssh_config_delete manage profiles. NEVER echo, quote, log, or restate password, private_key, key_passphrase, or proxy credentials from tool arguments or results — write secrets into tools only, do not surface them in chat. There is no read/get tool for full config bodies by design; do not invent one or try to dump secrets via shell/file tools.
`

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
	NoInternal   bool   // when true, hide and refuse the built-in loopback profile
}

// New creates and configures the MCP server with all tools registered.
// sshConfigs may be nil (session_start / ssh_config_list will error or return empty).
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
	mcpServer.AddTool(mcpgo.NewTool("session_start",
		mcpgo.WithDescription("Start a session (connection container) with one primary shell channel. Profiles live under data-dir/ssh_configs/<name>/config.toml. Use ssh_config \"internal\" (or omit) for loopback on the termcp host; other names are remote SSH. Returns session_id (connection) and shell_id (terminal I/O). Leave command/args empty for login shell / profile defaults."),
		mcpgo.WithString("command", mcpgo.Description("Executable or shell builtin line; leave empty with no args for login shell / profile default_shell")),
		mcpgo.WithArray("args", mcpgo.Description("Argv after command; only valid when command is non-empty"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("pty: pseudo-terminal (interactive TUI); pipe: no TTY, line-oriented"), mcpgo.DefaultString("pty")),
		mcpgo.WithString("name", mcpgo.Description("Optional label shown in session lists. If omitted, defaults to ssh_config.")),
		mcpgo.WithNumber("rows", mcpgo.Description("Initial PTY height"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("Initial PTY width"), mcpgo.DefaultNumber(80)),
		mcpgo.WithString("ssh_config", mcpgo.Description("Profile name under data-dir/ssh_configs. Empty/omitted means \"internal\".")),
	), withLogging("session_start", s.handleStartSession))

	mcpServer.AddTool(mcpgo.NewTool("shell_open",
		mcpgo.WithDescription("Open another shell channel on an existing session connection (reuses SSH transport). Returns shell_id for I/O and session_id of the parent."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("name", mcpgo.Description("Optional display name for this shell tab")),
		mcpgo.WithString("command", mcpgo.Description("Executable; leave empty for login shell")),
		mcpgo.WithString("mode", mcpgo.Description("pty (default) or pipe"), mcpgo.DefaultString("pty")),
		mcpgo.WithNumber("rows", mcpgo.Description("PTY rows"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("PTY cols"), mcpgo.DefaultNumber(80)),
	), withLogging("shell_open", s.handleStartSubShell))

	mcpServer.AddTool(mcpgo.NewTool("shell_list",
		mcpgo.WithDescription("List shell channels on a session. Returns session_id and shells (each with id, name, status, timestamps). Use shell ids with shell_input/shell_key/shell_output/shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start / session_list")),
	), withLogging("shell_list", s.handleListSubshells))

	mcpServer.AddTool(mcpgo.NewTool("shell_close",
		mcpgo.WithDescription("Close one shell channel by shell_id without tearing down the session connection. Other shells and forwards stay up. For internal primary shell, close is a no-op (process outlives the tab). Use session_terminate to stop the whole session."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open / shell_list")),
	), withLogging("shell_close", s.handleCloseShell))

	mcpServer.AddTool(mcpgo.NewTool("shell_input",
		mcpgo.WithDescription("Write text bytes to a shell's stdin. Follow with shell_key(key=\"enter\") to execute the typed line, then shell_output for the result."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("UTF-8 text to write (no automatic newline)")),
	), withLogging("shell_input", s.handleSendInput))

	mcpServer.AddTool(mcpgo.NewTool("shell_key",
		mcpgo.WithDescription("Send a named key to a shell. Supported: enter, tab, esc, up, down, left, right, backspace, delete, home, end, ctrl+c, ctrl+d, ctrl+z, ctrl+l, ctrl+u, ctrl+w. Use enter after shell_input to execute a command."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
		mcpgo.WithString("key", mcpgo.Required(), mcpgo.Description("Named key (e.g. enter, ctrl+c, up)")),
		mcpgo.WithNumber("repeat", mcpgo.Description("Times to send the key (1–20)"), mcpgo.DefaultNumber(1)),
	), withLogging("shell_key", s.handlePressKey))

	mcpServer.AddTool(mcpgo.NewTool("shell_output",
		mcpgo.WithDescription("Return newly produced stdout/stderr since the last read on the given reader_id. Prefer timeout ≤ 3 when polling multiple shells. Use timeout=0 for non-blocking (available data only). Response JSON: output, has_more, lines_returned, bytes_returned, session_status, session_uptime_seconds."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("If true, strip ANSI SGR/cursor escapes for plain-text logs"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Blocking wait for new output, in seconds (0.1–60); 0 = non-blocking; prefer ≤3 for multi-shell polling"), mcpgo.DefaultNumber(3)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Return at most N newline-terminated lines; remaining bytes stay unread so has_more stays true; 0 = no line limit"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Max bytes to return per call; 0 = no limit. Use with has_more to paginate large output."), mcpgo.DefaultNumber(8192)),
		mcpgo.WithNumber("reader_id", mcpgo.Description("0 = default reader; use shell_reader_register for independent cursors"), mcpgo.DefaultNumber(0)),
	), withLogging("shell_output", s.handleReadOutput))

	mcpServer.AddTool(mcpgo.NewTool("session_list",
		mcpgo.WithDescription("Return metadata for every running parent session in the registry. Exited sessions are removed automatically. Child shells are NOT included — use shell_list."),
	), withLogging("session_list", s.handleListSessions))
	mcpServer.AddTool(mcpgo.NewTool("session_info",
		mcpgo.WithDescription("Return a JSON document with detailed fields for one session: identifiers, command line, mode, PTY size, remote connection metadata, exit state, etc."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
	), withLogging("session_info", s.handleGetSessionInfo))

	mcpServer.AddTool(mcpgo.NewTool("session_terminate",
		mcpgo.WithDescription("Stop and remove a session: terminate all shells, close the SSH connection, cascade attached forwards, and drop the registry entry. force=true kills immediately; force=false waits grace_period after SIGTERM. To close only one shell channel, use shell_close."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithBoolean("force", mcpgo.Description("If true, end immediately without honoring grace_period"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to allow after SIGTERM before hard close when force is false (0–60)"), mcpgo.DefaultNumber(5)),
	), withLogging("session_terminate", s.handleTerminateSession))

	mcpServer.AddTool(mcpgo.NewTool("shell_resize",
		mcpgo.WithDescription("Update PTY rows/cols for a shell channel (propagates to SSH remote PTY when applicable)."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
		mcpgo.WithNumber("rows", mcpgo.Description("New row count (typical 24–60)"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("New column count (typical 80–200)"), mcpgo.DefaultNumber(80)),
	), withLogging("shell_resize", s.handleResizePty))

	mcpServer.AddTool(mcpgo.NewTool("shell_detect",
		mcpgo.WithDescription("Probe the termcp host only (not an arbitrary ssh_config) for a suitable interactive shell: returns executable path, family enum (unix, powershell, cmd), and a short hint string."),
	), withLogging("shell_detect", s.handleDetectShell))

	mcpServer.AddTool(mcpgo.NewTool("ssh_config_list",
		mcpgo.WithDescription("Return the sorted list of profile names that may be passed as ssh_config to session_start—one entry per directory under data-dir/ssh_configs plus the built-in \"internal\" profile. Does not return JSON bodies, secrets, or hostnames."),
	), withLogging("ssh_config_list", s.handleListSSHConfigs))

	mcpServer.AddTool(mcpgo.NewTool("message_list",
		mcpgo.WithDescription("List stored MCP/chat message index entries associated with a session_id. Returns message ids and metadata for later message_get calls."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
	), withLogging("message_list", s.handleListMessages))

	mcpServer.AddTool(mcpgo.NewTool("message_get",
		mcpgo.WithDescription("Fetch full message payloads for one or more message_ids under a session."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithArray("message_ids", mcpgo.Description("List of message id strings from message_list"), mcpgo.WithStringItems()),
	), withLogging("message_get", s.handleGetMessage))

	mcpServer.AddTool(mcpgo.NewTool("shell_reader_register",
		mcpgo.WithDescription("Allocate a new output reader_id for this shell. That reader only observes bytes written after registration (cursor starts at buffer end). Pair every shell_output(..., reader_id) with the id returned here."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
	), withLogging("shell_reader_register", s.handleRegisterReader))

	mcpServer.AddTool(mcpgo.NewTool("shell_reader_unregister",
		mcpgo.WithDescription("Release a reader_id previously returned by shell_reader_register."),
		mcpgo.WithString("shell_id", mcpgo.Required(), mcpgo.Description("shell_id from session_start / shell_open")),
		mcpgo.WithNumber("reader_id", mcpgo.Required(), mcpgo.Description("Non-zero reader id from shell_reader_register")),
	), withLogging("shell_reader_unregister", s.handleUnregisterReader))

	// --- Port forwarding (OpenSSH names) ---
	mcpServer.AddTool(mcpgo.NewTool("forward_local",
		mcpgo.WithDescription("Local port forward (ssh -L). termcp listens on a local port and tunnels traffic through SSH to the remote target. local_port=0 picks a random free port."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_host", mcpgo.Description("Target host as reachable from the remote SSH server (usually localhost)"), mcpgo.DefaultString("localhost")),
		mcpgo.WithNumber("remote_port", mcpgo.Required(), mcpgo.Description("Target port on remote host")),
		mcpgo.WithNumber("local_port", mcpgo.Description("Local port to listen on (0=random)"), mcpgo.DefaultNumber(0)),
	), withLogging("forward_local", s.handleLocalForward))

	mcpServer.AddTool(mcpgo.NewTool("forward_remote",
		mcpgo.WithDescription("Remote port forward (ssh -R). Note the naming: local_host/local_port configure the listener that the REMOTE SSH server opens; remote_host/remote_port configure the target dialed from the termcp host (a service reachable from the machine running termcp). Example: forward_remote(session_id, local_port=8080, remote_host=\"127.0.0.1\", remote_port=9000) makes the SSH server listen on its own :8080 and tunnel connections to 127.0.0.1:9000 on the termcp host."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("local_host", mcpgo.Description("Host for the remote side to listen on"), mcpgo.DefaultString("0.0.0.0")),
		mcpgo.WithNumber("local_port", mcpgo.Required(), mcpgo.Description("Port for the remote side to listen on")),
		mcpgo.WithString("remote_host", mcpgo.Required(), mcpgo.Description("Target host (relative to termcp)")),
		mcpgo.WithNumber("remote_port", mcpgo.Required(), mcpgo.Description("Target port (relative to termcp)")),
	), withLogging("forward_remote", s.handleRemoteForward))

	mcpServer.AddTool(mcpgo.NewTool("forward_dynamic",
		mcpgo.WithDescription("Start a SOCKS5 proxy (ssh -D). termcp listens on a local port, proxies TCP connections through the agent/SSH connection."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithNumber("local_port", mcpgo.Description("Local port for SOCKS5 proxy (0=random)"), mcpgo.DefaultNumber(0)),
	), withLogging("forward_dynamic", s.handleDynamicForward))

	mcpServer.AddTool(mcpgo.NewTool("forward_list",
		mcpgo.WithDescription("List all active port forwards (local, remote, and dynamic). Returns forward_id, direction, listen_addr, target_addr, status."),
	), withLogging("forward_list", s.handleListForwards))

	mcpServer.AddTool(mcpgo.NewTool("forward_close",
		mcpgo.WithDescription("Close an active port forward by forward_id, releasing the listener."),
		mcpgo.WithString("forward_id", mcpgo.Required(), mcpgo.Description("Forward ID from forward_list")),
	), withLogging("forward_close", s.handleCloseForward))

	// --- File operation tools (session-scoped SFTP) ---
	mcpServer.AddTool(mcpgo.NewTool("file_read",
		mcpgo.WithDescription("Read a remote file or file segment via SSH/SFTP. Mode 'text' returns readable text with \\xHH escapes for non-printable bytes. Mode 'hex' returns hex dump. Mode 'file' downloads to a file on the termcp host (the machine running the termcp server — NOT the remote SSH host). Omit offset/length for whole file read. Example: read first 1KB of /var/log/syslog in hex — {\"session_id\":\"...\",\"remote_path\":\"/var/log/syslog\",\"mode\":\"hex\",\"offset\":0,\"length\":1024}."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Start byte offset (0-based)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read (0=all)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("mode", mcpgo.Description("Output mode: text, hex, or file"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex", "file")),
		mcpgo.WithString("local_path", mcpgo.Description("Download destination path on the termcp host (the machine running termcp, e.g. your local machine) — not the remote SSH host. Only used with mode=file.")),
	), withLogging("file_read", s.handleFileRead))

	mcpServer.AddTool(mcpgo.NewTool("file_write",
		mcpgo.WithDescription("Write to a remote file via SSH/SFTP. Use inline data (text mode with \\xHH escapes, or hex mode) for small writes; use local_path + local_offset + length to stream from a file on the termcp host (the machine running the termcp server — NOT the remote SSH host) for large/binary writes. Example: write \"hello\\n\" to /tmp/note.txt — {\"session_id\":\"...\",\"remote_path\":\"/tmp/note.txt\",\"mode\":\"text\",\"data\":\"hello\\n\"}."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("offset", mcpgo.Description("Write start offset: 0 rewrites the file from the beginning (truncates); >0 writes at that byte position without truncating (to append, set offset to the current file size from file_stat)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithString("data", mcpgo.Description("Inline data to write (text or hex per mode)")),
		mcpgo.WithString("mode", mcpgo.Description("Data encoding: text (default, supports \\xHH) or hex"), mcpgo.DefaultString("text"), mcpgo.Enum("text", "hex")),
		mcpgo.WithString("local_path", mcpgo.Description("Source file path on the termcp host (the machine running termcp, e.g. your local machine) — not the remote SSH host")),
		mcpgo.WithNumber("local_offset", mcpgo.Description("Read start offset in local file"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("length", mcpgo.Description("Bytes to read from local file (0=all)"), mcpgo.DefaultNumber(0)),
	), withLogging("file_write", s.handleFileWrite))

	mcpServer.AddTool(mcpgo.NewTool("file_stat",
		mcpgo.WithDescription("Get file or directory info from remote via SSH/SFTP. Returns name, size, is_dir, mod_time, and children list for directories."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_stat", s.handleFileStat))

	mcpServer.AddTool(mcpgo.NewTool("file_delete",
		mcpgo.WithDescription("Delete a remote file or empty directory via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path to delete")),
	), withLogging("file_delete", s.handleFileDelete))

	mcpServer.AddTool(mcpgo.NewTool("file_rename",
		mcpgo.WithDescription("Move or rename a remote file/directory via SSH/SFTP (same filesystem)."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("from_path", mcpgo.Required(), mcpgo.Description("Current remote path")),
		mcpgo.WithString("to_path", mcpgo.Required(), mcpgo.Description("New remote path")),
	), withLogging("file_rename", s.handleFileRename))

	mcpServer.AddTool(mcpgo.NewTool("file_mkdir",
		mcpgo.WithDescription("Create a directory (and parents) on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote directory path to create")),
	), withLogging("file_mkdir", s.handleFileMakeDir))

	mcpServer.AddTool(mcpgo.NewTool("file_urls",
		mcpgo.WithDescription("Get HTTP download/upload URLs for a remote file path under a session. Use these URLs for direct curl/wget/browser access."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
	), withLogging("file_urls", s.handleGetFileURLs))

	mcpServer.AddTool(mcpgo.NewTool("file_chmod",
		mcpgo.WithDescription("Change file permissions on the remote via SSH/SFTP. mode is a decimal Unix permission (e.g. 493 = 0755)."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("mode", mcpgo.Required(), mcpgo.Description("Unix permission mode as decimal integer (e.g. 493 for 0755)")),
	), withLogging("file_chmod", s.handleFileChmod))

	mcpServer.AddTool(mcpgo.NewTool("file_chown",
		mcpgo.WithDescription("Change file owner and group on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("uid", mcpgo.Required(), mcpgo.Description("User ID (numeric)")),
		mcpgo.WithNumber("gid", mcpgo.Required(), mcpgo.Description("Group ID (numeric)")),
	), withLogging("file_chown", s.handleFileChown))

	mcpServer.AddTool(mcpgo.NewTool("file_chtimes",
		mcpgo.WithDescription("Change file access and modification timestamps on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("atime", mcpgo.Required(), mcpgo.Description("Access time as Unix timestamp (seconds)")),
		mcpgo.WithNumber("mtime", mcpgo.Required(), mcpgo.Description("Modification time as Unix timestamp (seconds)")),
	), withLogging("file_chtimes", s.handleFileChtimes))

	mcpServer.AddTool(mcpgo.NewTool("file_readlink",
		mcpgo.WithDescription("Read the target of a symbolic link on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote symlink path")),
	), withLogging("file_readlink", s.handleFileReadlink))

	mcpServer.AddTool(mcpgo.NewTool("file_symlink",
		mcpgo.WithDescription("Create a symbolic link on the remote via SSH/SFTP. target is the existing path, link_path is the new symlink to create (like 'ln -s target link_path')."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("target", mcpgo.Required(), mcpgo.Description("The existing file/directory to point to")),
		mcpgo.WithString("link_path", mcpgo.Required(), mcpgo.Description("The new symlink path to create")),
	), withLogging("file_symlink", s.handleFileSymlink))

	mcpServer.AddTool(mcpgo.NewTool("file_link",
		mcpgo.WithDescription("Create a hard link on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("existing_path", mcpgo.Required(), mcpgo.Description("The existing file to link to")),
		mcpgo.WithString("new_path", mcpgo.Required(), mcpgo.Description("The new hard link path to create")),
	), withLogging("file_link", s.handleFileLink))

	mcpServer.AddTool(mcpgo.NewTool("file_truncate",
		mcpgo.WithDescription("Truncate a remote file to a given size via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file path")),
		mcpgo.WithNumber("size", mcpgo.Required(), mcpgo.Description("New file size in bytes")),
	), withLogging("file_truncate", s.handleFileTruncate))

	mcpServer.AddTool(mcpgo.NewTool("file_realpath",
		mcpgo.WithDescription("Resolve the canonical absolute path on the remote via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path")),
	), withLogging("file_realpath", s.handleFileRealpath))

	mcpServer.AddTool(mcpgo.NewTool("file_statvfs",
		mcpgo.WithDescription("Get filesystem statistics (disk space, inodes) for a remote path via SSH/SFTP."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Remote file or directory path (must exist)")),
	), withLogging("file_statvfs", s.handleFileStatVFS))

	mcpServer.AddTool(mcpgo.NewTool("file_getwd",
		mcpgo.WithDescription("Get the SFTP working directory for this session connection. This is not the interactive shell's cwd (pwd); use a shell command for that."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("session_id from session_start")),
	), withLogging("file_getwd", s.handleFileGetwd))

	s.mcpServer = mcpServer
	s.sseServer = mcpserver.NewSSEServer(mcpServer, sseOpts...)
	// Streamable HTTP (MCP spec): mount at /stream for clients such as Open WebUI.
	// Do not use WithStreamableHTTPServer(mainSrv) here — Shutdown must not close the shared listener.
	s.streamServer = mcpserver.NewStreamableHTTPServer(mcpServer)
	return s
}

// RegisterSSHConfigWriteTools adds ssh_config_create, ssh_config_edit, ssh_config_copy,
// and ssh_config_delete tools. Call only when -mcp-manage-ssh-configs is set.
func (s *Server) RegisterSSHConfigWriteTools() {
	s.mcpServer.AddTool(mcpgo.NewTool("ssh_config_create",
		mcpgo.WithDescription("Create a new remote SSH profile under data-dir/ssh_configs/<name>/config.toml. Fails if name already exists. Requires host, user, and password or private_key. Never echo password/private_key/key_passphrase/proxy credentials back in chat."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Profile name (letters, digits, _, -; max 64). Passed later as ssh_config to session_start.")),
		mcpgo.WithString("host", mcpgo.Required(), mcpgo.Description("SSH hostname or IP")),
		mcpgo.WithString("user", mcpgo.Required(), mcpgo.Description("SSH username")),
		mcpgo.WithNumber("port", mcpgo.Description("SSH port"), mcpgo.DefaultNumber(22)),
		mcpgo.WithString("password", mcpgo.Description("Password auth (omit if using private_key)")),
		mcpgo.WithString("private_key", mcpgo.Description("PEM private key content (omit if using password)")),
		mcpgo.WithString("key_passphrase", mcpgo.Description("Passphrase for encrypted private_key")),
		mcpgo.WithBoolean("trust_unknown_host", mcpgo.Description("Accept unknown host keys"), mcpgo.DefaultBool(false)),
		mcpgo.WithString("known_hosts", mcpgo.Description("known_hosts content or path")),
		mcpgo.WithNumber("dial_timeout_seconds", mcpgo.Description("Dial timeout"), mcpgo.DefaultNumber(30)),
		mcpgo.WithString("proxy", mcpgo.Description("SOCKS5 proxy URL, e.g. socks5://user:pass@host:port")),
		mcpgo.WithString("description", mcpgo.Description("Human-readable description")),
		mcpgo.WithString("default_shell", mcpgo.Description("Default command when session_start leaves command empty")),
		mcpgo.WithString("default_mode", mcpgo.Description("Default mode: pty or pipe")),
		mcpgo.WithString("jump_host", mcpgo.Description("Optional bastion host (ProxyJump)")),
		mcpgo.WithString("jump_user", mcpgo.Description("Bastion username")),
		mcpgo.WithNumber("jump_port", mcpgo.Description("Bastion port"), mcpgo.DefaultNumber(22)),
		mcpgo.WithString("jump_password", mcpgo.Description("Bastion password")),
		mcpgo.WithString("jump_private_key", mcpgo.Description("Bastion PEM private key")),
		mcpgo.WithString("jump_key_passphrase", mcpgo.Description("Bastion key passphrase")),
		mcpgo.WithBoolean("jump_trust_unknown_host", mcpgo.Description("Bastion trust unknown host"), mcpgo.DefaultBool(false)),
		mcpgo.WithString("jump_known_hosts", mcpgo.Description("Bastion known_hosts")),
		mcpgo.WithNumber("jump_dial_timeout_seconds", mcpgo.Description("Bastion dial timeout"), mcpgo.DefaultNumber(30)),
		mcpgo.WithString("jump_proxy", mcpgo.Description("Bastion SOCKS5 proxy URL")),
	), withLogging("ssh_config_create", s.handleCreateSSHConfig))

	s.mcpServer.AddTool(mcpgo.NewTool("ssh_config_edit",
		mcpgo.WithDescription("Patch an existing remote SSH profile. Only provided (non-empty) fields are updated; omitted fields keep their stored values including secrets. Never returns config body or credentials. Never expose secrets in chat."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Existing profile name to edit")),
		mcpgo.WithString("host", mcpgo.Description("SSH hostname or IP")),
		mcpgo.WithString("user", mcpgo.Description("SSH username")),
		mcpgo.WithNumber("port", mcpgo.Description("SSH port")),
		mcpgo.WithString("password", mcpgo.Description("Replace password (omit to keep existing)")),
		mcpgo.WithString("private_key", mcpgo.Description("Replace PEM private key (omit to keep existing)")),
		mcpgo.WithString("key_passphrase", mcpgo.Description("Replace key passphrase")),
		mcpgo.WithBoolean("trust_unknown_host", mcpgo.Description("Accept unknown host keys")),
		mcpgo.WithString("known_hosts", mcpgo.Description("known_hosts content or path")),
		mcpgo.WithNumber("dial_timeout_seconds", mcpgo.Description("Dial timeout")),
		mcpgo.WithString("proxy", mcpgo.Description("SOCKS5 proxy URL")),
		mcpgo.WithString("description", mcpgo.Description("Human-readable description")),
		mcpgo.WithString("default_shell", mcpgo.Description("Default command for empty session_start command")),
		mcpgo.WithString("default_mode", mcpgo.Description("Default mode: pty or pipe")),
		mcpgo.WithString("jump_host", mcpgo.Description("Set/replace bastion host (creates jump section if missing)")),
		mcpgo.WithString("jump_user", mcpgo.Description("Bastion username")),
		mcpgo.WithNumber("jump_port", mcpgo.Description("Bastion port")),
		mcpgo.WithString("jump_password", mcpgo.Description("Bastion password")),
		mcpgo.WithString("jump_private_key", mcpgo.Description("Bastion PEM private key")),
		mcpgo.WithString("jump_key_passphrase", mcpgo.Description("Bastion key passphrase")),
		mcpgo.WithBoolean("jump_trust_unknown_host", mcpgo.Description("Bastion trust unknown host")),
		mcpgo.WithString("jump_known_hosts", mcpgo.Description("Bastion known_hosts")),
		mcpgo.WithNumber("jump_dial_timeout_seconds", mcpgo.Description("Bastion dial timeout")),
		mcpgo.WithString("jump_proxy", mcpgo.Description("Bastion SOCKS5 proxy URL")),
	), withLogging("ssh_config_edit", s.handleEditSSHConfig))

	s.mcpServer.AddTool(mcpgo.NewTool("ssh_config_copy",
		mcpgo.WithDescription("Duplicate an existing SSH profile (including secrets) to a new name on the server — a server-side copy, secrets never reach the agent. Use then ssh_config_edit to change host/user without re-providing keys. Fails when target_name already exists."),
		mcpgo.WithString("source_name", mcpgo.Required(), mcpgo.Description("Existing profile to copy from")),
		mcpgo.WithString("target_name", mcpgo.Required(), mcpgo.Description("New profile name (must not already exist)")),
	), withLogging("ssh_config_copy", s.handleCopySSHConfig))

	s.mcpServer.AddTool(mcpgo.NewTool("ssh_config_delete",
		mcpgo.WithDescription("Delete a remote SSH profile by name. The built-in \"internal\" profile cannot be deleted."),
		mcpgo.WithString("name", mcpgo.Required(), mcpgo.Description("Profile name to delete")),
	), withLogging("ssh_config_delete", s.handleDeleteSSHConfig))
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
