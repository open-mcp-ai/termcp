# termcp

<p align="center">
  <strong>Give AI Agents Interactive Terminal Capabilities</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8.svg" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey" alt="macOS / Linux / Windows">
  <img src="https://img.shields.io/badge/MCP-SSE_&_Streamable_HTTP-green.svg" alt="MCP SSE & Streamable HTTP">
  <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="MIT License">
</p>

<p align="center">
  <a href="./README.zh.md"><img src="https://img.shields.io/badge/🌏-中文-blue.svg" alt="中文"></a>
</p>

<p align="center">
  <a href="./README.zh.md">中文</a> | <strong>English</strong>
</p>

---

## Quick Start

### Install

```bash
go install github.com/open-mcp-ai/termcp@latest
termcp --data-dir ./data
```

### Build from source

```bash
go build -o termcp .
./termcp --data-dir ./data
```

Open your browser to `http://127.0.0.1:18765` for the **Web UI**.

The Web UI features:
- **Browser-based terminal** (xterm.js + WebSocket) — start sessions, send input, watch output live
- **Session list** with real-time SSE updates
- **Connection templates** for saved SSH profiles (`internal` loopback or `remote` hosts)
- Full output scrollback replay — reconnect and re-read from the beginning

## Command Line

```
termcp [flags]
termcp ssh-config init <name> -data-dir <dir>   # create a remote SSH config skeleton
termcp ssh-config list -data-dir <dir>          # list all SSH config names
```

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | HTTP bind address. Use `0.0.0.0` to listen on all interfaces. |
| `--port` | `18765` | HTTP port. Web UI, MCP SSE, and MCP streamable HTTP share this port. |
| `--data-dir` | `./data` | Persistent storage (sessions, messages, SSH configs). Created if missing. |
| `--log-level` | `info` | Log verbosity: `debug`, `info`, `warn`, `error`. Use `debug` to inspect MCP tool calls. |
| `--admin-host` | `127.0.0.1` | Admin HTTP API bind address. |
| `--admin-port` | `0` (disabled) | Admin HTTP API port. Requires `--admin-token` when non-zero. |
| `--admin-token` | — | Bearer / `X-Admin-Token` for SSH config management via HTTP. |

### Examples

```bash
# Loopback-only, verbose logs
./termcp --data-dir ./data --log-level debug

# Listen on all interfaces (LAN/WAN — add a reverse proxy for auth)
./termcp --data-dir ./data --host 0.0.0.0

# Enable admin API for SSH config management
./termcp --data-dir ./data --admin-port 9090 --admin-token "my-secret"

# Create a remote SSH config
./termcp ssh-config init my-server --data-dir ./data
# Edit ./data/ssh_configs/my-server/config.json with credentials

# List available SSH configs
./termcp ssh-config list --data-dir ./data
```

## MCP Configuration

### Claude Code

```json
{
  "mcpServers": {
    "termcp": {
      "type": "sse",
      "url": "http://your-server:18765/sse"
    }
  }
}
```

Or via CLI:

```bash
claude mcp add --transport sse termcp http://localhost:18765/sse
```

### Open WebUI (Streamable HTTP)

Point Open WebUI at `http://<host>:18765/stream`.

Example when termcp runs on the same machine: `http://127.0.0.1:18765/stream`. When Open WebUI runs in Docker and termcp on the host: `http://host.docker.internal:18765/stream` (macOS/Windows) or the host LAN IP.

### Other MCP Clients

Any MCP client with **SSE** transport → `http://<host>:<port>/sse`.  
Any MCP client with **streamable HTTP** → `http://<host>:<port>/stream`.

---

## Introduction

`termcp` is an MCP (Model Context Protocol) server that enables AI Agents (like Claude Code) to start, control, and manage **long-running interactive processes**.

### Why Do You Need It?

AI Agents can natively only execute one-shot commands — they run and immediately return results. But many real-world scenarios require **multi-turn interaction**:

- SSH into a remote server, enter a password first, then run commands
- Debug code line by line in a Python REPL
- Answer `[Y/n]` prompts in interactive installers
- Use terminal-dependent commands like `top`, `htop`
- Run security tools (e.g., impacket) for multi-step operations

In these scenarios, the process keeps running, and the AI Agent needs to **repeatedly read and write** the process's I/O across **multiple conversation turns**. `termcp` is the bridge designed precisely for this purpose.

### Key Features

| Feature | Description |
|---------|-------------|
| **Web UI** | Browser-based terminal with xterm.js + WebSocket; session list, connection templates, output replay |
| **Multi-agent session sharing** | Multiple AI agents read from the same session simultaneously, each with an independent cursor — no output stealing |
| **PTY and Pipe dual mode** | PTY mode emulates a real terminal; Pipe mode for simple stdin/stdout interaction |
| **Remote deployment** | SSE over HTTP transport — Agent and Server can run on different machines |
| **Service-side SSH profiles** | SSH connection details stored server-side as `{data-dir}/ssh_configs/<name>/config.json`; MCP tools only pass the name |
| **Multi-session management** | Manage multiple independent processes simultaneously without interference |
| **Shell channel multiplexing** | `start_subshell` opens extra shell channels on one SSH connection — no new TCP handshake; `close_shell` closes one channel without tearing down the session |
| **Port forwarding** | `ssh -L` / `-R` / `-D` style local, remote, and dynamic (SOCKS5) forwards over an existing SSH session |
| **SFTP file operations** | `file_read` / `file_write` / `file_stat` / `file_delete` / `file_rename` / `file_mkdir` plus HTTP download/upload URLs via `get_file_urls` |
| **Message persistence** | Session records and I/O messages persisted to local JSON files |
| **ANSI escape code stripping** | Optional automatic removal of terminal control sequences for clean text output |
| **Blocking reads with timeout** | Agents wait for new output up to a configurable timeout; returns promptly via sync.Cond |
| **Cross-platform shell detection** | `detect_shell` probes the termcp host for bash/zsh/fish/pwsh/cmd — useful for mixed Windows/Linux environments |
| **Graceful termination** | SIGTERM first, then SIGKILL after a configurable grace period |
| **PTY resize** | Dynamically adjust terminal rows and columns at runtime |
| **Safe shell startup** | History expansion (`!`) disabled automatically; TERM propagated to child processes |

---

## Architecture

```
┌──────┐  SSE/HTTP  ┌──────────────┐  In-memory SSH  ┌──────────┐
│Agent │ ──────────> │ Go Server    │ ──────────────> │ PTY/     │
│(MCP) │             │ - MCP API    │  (no TCP port)  │ Process  │
└──────┘             │ - Web UI     │                 └──────────┘
                     │ - SSH Server │
                     └──────────────┘
                            │
                            ▼
                     ┌──────────────┐
                     │ JSON Storage │
                     │ - sessions   │
                     │ - messages   │
                     │ - ssh_configs│
                     └──────────────┘
```

### Project Structure

```
.
├── main.go                      # Entry point
├── internal/
│   ├── config/config.go         # Configuration with validation
│   ├── mcp/
│   │   ├── server.go            # MCP SSE server & tool registration
│   │   ├── handlers.go          # 31 tool handlers
│   │   └── logging.go           # Structured slog logging per tool call
│   ├── webui/                   # Embedded SPA + WebSocket terminal + REST API
│   ├── sshserver/server.go      # In-memory SSH server (charmbracelet/ssh)
│   ├── sshclient/               # SSH client (crypto/ssh) + ChildShell multiplex
│   ├── sshconfig/               # Server-side SSH profile store
│   ├── session/                 # Session lifecycle + thread-safe registry
│   ├── buffer/buffer.go         # Multi-reader append-only output log
│   ├── storage/store.go         # Atomic JSON file persistence
│   ├── message/message.go       # Message management per session
│   ├── forward/forward.go       # Port forward manager (-L / -R / -D)
│   ├── sftp/sftp.go             # SFTP client wrapper for file tools
│   ├── shell/detect.go          # Cross-platform shell detection
│   ├── ansi/strip.go            # ANSI escape code removal
│   ├── encoding/                # \xHH / hex data encoding helpers
│   └── logansi/handler.go       # Color slog handler
├── pkg/api/types.go             # Public types (Session, Message, SessionMode)
├── go.mod
└── go.sum
```

### Key Design Decisions

1. **Multi-Reader Output Buffer**: One append-only byte log; each reader has an independent read cursor. Prefixes fully consumed by every reader are trimmed to bound memory; there is no fixed per-reader cap or ring overwrite.

2. **In-Memory SSH Architecture**: The server runs a charmbracelet/ssh server fully in-process — no TCP listener. Each `start_session` dials an in-memory net.Conn pair and creates an SSH session via crypto/ssh client, leveraging SSH's mature PTY allocation, window resize, signal forwarding, and environment variable passing. On Windows, ConPTY (via creack/pty) is used for native pseudo-terminal support. `start_subshell` reuses the same SSH client for additional shell channels with no new handshake.

3. **Single HTTP Mux**: Web UI, MCP SSE (`/sse`), MCP streamable HTTP (`/stream`), and WebSocket terminal (`/api/ui/ws`) share one listener on the configured port. Optional admin HTTP on a separate port.

4. **Atomic JSON Persistence**: Session metadata and I/O messages stored via temp-file + fsync + rename:
   - `data/sessions.json` — Session list
   - `data/messages/{session_id}/index.json` — Message index
   - `data/messages/{session_id}/messages/{msg_id}.json` — Message content

5. **Session Lifecycle Safety**: Exit goroutine is the single authority for `Status`/`ExitCode` (via `sync.Once`). Terminate is idempotent. Stdin writes are serialized via a dedicated mutex.

6. **Safe Shell Environment**: Interactive shells start with history expansion disabled (zsh: `NO_BANG_HIST`, bash: `+o histexpand`) to prevent `!` characters in passwords and URLs from causing errors. PTY-requested `TERM` is propagated to child processes.

---

## Examples

### Example 1: SSH Remote Operations

```
AI Agent Flow                                   Process Output
─────────────────                              ────────────────

start_session(ssh_config="my-server")
  → session_id, shell_id
                                    ←    (read_output for prompts)

send_input(shell_id, text="df -h")
press_key(shell_id, key="enter")
read_output(shell_id, timeout=3)
                                    ←    "Filesystem ... Use% Mounted on ..."

terminate_session(session_id)
```

### Example 2: Python REPL Debugging

```
start_session(command="python3", mode="pty")
  → session_id, shell_id

send_input(shell_id, text="data = [1, 2, 3, 4, 5]")
press_key(shell_id, key="enter")
read_output(shell_id)

send_input(shell_id, text="sum(data)")
press_key(shell_id, key="enter")
read_output(shell_id)
                                    ←    "15"
```

### Example 3: Multi-Agent Collaboration

```
# Agent A starts a monitoring process
start_session(command="top", mode="pty")
  → session_id, shell_id

# Agent B joins the same shell without stealing output
register_reader(shell_id=...)
  → reader_id: 2

# Agent A reads its own cursor
read_output(shell_id=..., reader_id=1)
  → "PID USER  PR  NI  VIRT  RES  SHR S %CPU %MEM   TIME+ COMMAND..."

# Agent B reads independently
read_output(shell_id=..., reader_id=2)
  → "top - 14:32:10 up 3 days,  2:15,  1 user,  load average: 0.52, 0.58, 0.59..."

# Agent B is done
unregister_reader(shell_id=..., reader_id=2)

# Agent A terminates the session
terminate_session(session_id=...)
```

### Example 4: Multi-session Parallel Management

```
start_session(command="ping", args=["-c", "5", "google.com"], name="ping-test")
  → session_id, shell_id

start_session(command="python3", args=["-m", "http.server", "8080"], name="web-server")
  → session_id, shell_id

list_sessions()
  → [{id: "...", status: "running"}, ...]

read_output(shell_id=..., timeout=1)  # poll shells, timeout ≤ 3

terminate_session(session_id=...)
```

---

## Tool Reference

Full tool reference: [`docs/mcp-tools.md`](docs/mcp-tools.md).

| Category | Tools |
|----------|-------|
| Session lifecycle | `start_session`, `list_sessions`, `get_session_info`, `terminate_session` |
| Shell I/O | `send_input`, `press_key`, `read_output`, `resize_pty` |
| Shell multiplexing | `start_subshell`, `list_subshells`, `close_shell` |
| Multi-agent reading | `register_reader`, `unregister_reader` |
| Port forwarding | `local_forward` (-L), `remote_forward` (-R), `dynamic_forward` (-D), `list_forwards`, `close_forward` |
| File operations (SFTP) | `file_read`, `file_write`, `file_stat`, `file_delete`, `file_rename`, `file_mkdir`, `get_file_urls`, … |
| Server discovery | `detect_shell`, `list_ssh_configs` |
| Message persistence | `list_messages`, `get_message` |

---

## License

MIT
