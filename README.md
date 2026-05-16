# termcp

<p align="center">
  <strong>Give AI Agents Interactive Terminal Capabilities</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8.svg" alt="Go 1.21+">
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey" alt="macOS / Linux / Windows">
  <img src="https://img.shields.io/badge/MCP-SSE_Transport-green.svg" alt="MCP SSE">
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

```bash
# Build
go build -o termcp .

# Run (defaults: loopback, port 18765)
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
| `--ssh-host` | `127.0.0.1` | Internal SSH server bind address. |
| `--ssh-port` | `0` (random) | Internal SSH server port. `0` = random; set a fixed port if firewall rules require it. |
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
| **Message persistence** | Session records and I/O messages persisted to local JSON files |
| **ANSI escape code stripping** | Optional automatic removal of terminal control sequences for clean text output |
| **Blocking reads with timeout** | Agents wait for new output up to a configurable timeout; returns promptly via sync.Cond |
| **Cross-platform shell detection** | `detect_shell` probes the termcp host for bash/zsh/fish/pwsh/cmd — useful for mixed Windows/Linux environments |
| **Graceful termination** | SIGTERM first, then SIGKILL after a configurable grace period |
| **PTY resize** | Dynamically adjust terminal rows and columns at runtime |

---

## Architecture

```
┌──────┐  SSE/HTTP  ┌──────────────┐  Internal SSH  ┌──────────┐
│Agent │ ──────────> │ Go Server    │ ──────────────> │ PTY/     │
│(MCP) │             │ - MCP API    │  (localhost)    │ Process  │
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
│   │   ├── handlers.go          # 16 tool handlers
│   │   └── logging.go           # Structured slog logging per tool call
│   ├── webui/                   # Embedded SPA + WebSocket terminal + REST API
│   ├── sshserver/server.go      # Internal SSH server (charmbracelet/ssh)
│   ├── sshclient/client.go      # SSH client (crypto/ssh)
│   ├── sshconfig/               # Server-side SSH profile store
│   ├── session/
│   │   ├── session.go           # Session lifecycle (goroutine-safe)
│   │   └── manager.go           # Thread-safe session registry
│   ├── buffer/buffer.go         # Multi-reader append-only output log
│   ├── storage/store.go         # Atomic JSON file persistence
│   ├── message/message.go       # Message management per session
│   ├── shell/detect.go          # Cross-platform shell detection
│   ├── ansi/strip.go            # ANSI escape code removal
│   └── logansi/handler.go       # Color slog handler
├── pkg/api/types.go             # Public types (Session, Message, SessionMode)
├── go.mod
└── go.sum
```

### Key Design Decisions

1. **Multi-Reader Output Buffer**: One append-only byte log; each reader has an independent read cursor. Prefixes fully consumed by every reader are trimmed to bound memory; there is no fixed per-reader cap or ring overwrite.

2. **Internal SSH Architecture**: The server starts a charmbracelet/ssh server on localhost. Each `start_session` creates an SSH session via crypto/ssh client, leveraging SSH's mature PTY allocation, window resize, signal forwarding, and environment variable passing. On Windows, ConPTY is used for native pseudo-terminal support.

3. **Single HTTP Mux**: Web UI, MCP SSE (`/sse`), MCP streamable HTTP (`/stream`), and WebSocket terminal (`/api/ui/ws`) share one listener on the configured port. Optional admin HTTP on a separate port.

4. **Atomic JSON Persistence**: Session metadata and I/O messages stored via temp-file + fsync + rename:
   - `data/sessions.json` — Session list
   - `data/messages/{session_id}/index.json` — Message index
   - `data/messages/{session_id}/messages/{msg_id}.json` — Message content

5. **Session Lifecycle Safety**: Exit goroutine is the single authority for `Status`/`ExitCode` (via `sync.Once`). Terminate is idempotent. Stdin writes are serialized via a dedicated mutex.

---

## Examples

### Example 1: SSH Remote Operations

```
AI Agent Flow                                   Process Output
─────────────────                              ────────────────

start_session(
  command="ssh",
  args=["deploy@192.168.1.100"],
  ssh_config="my-server"
)
                                    ←    "deploy@192.168.1.100's password: "

send_and_read(
  text="my_secret_pass",
  press_enter=true
)
                                    ←    "Welcome to Ubuntu 22.04 LTS
                                          deploy@web-server:~$ "

send_and_read(
  text="df -h",
  press_enter=true
)
                                    ←    "Filesystem      Size  Used Avail Use% Mounted on
                                          /dev/sda1       100G   45G   55G  45% /
                                          deploy@web-server:~$ "

terminate_session(session_id="abc123")
```

### Example 2: Python REPL Debugging

```
start_session(command="python3", mode="pty")
                                    ←    "Python 3.10.12\n>>> "

send_and_read(text="data = [1, 2, 3, 4, 5]", press_enter=true)
                                    ←    ">>> "

send_and_read(text="sum(data)", press_enter=true)
                                    ←    "15\n>>> "
```

### Example 3: Multi-Agent Collaboration

```
# Agent A starts a monitoring process
start_session(command="top", mode="pty")
  → session_id: "sess-001"

# Agent B joins the same session without stealing output
register_reader(session_id="sess-001")
  → reader_id: 2

# Agent A reads its own cursor
read_output(session_id="sess-001", reader_id=1)
  → "PID USER  PR  NI  VIRT  RES  SHR S %CPU %MEM   TIME+ COMMAND..."

# Agent B reads from the beginning independently
read_output(session_id="sess-001", reader_id=2)
  → "top - 14:32:10 up 3 days,  2:15,  1 user,  load average: 0.52, 0.58, 0.59..."

# Agent B is done
unregister_reader(session_id="sess-001", reader_id=2)

# Agent A terminates the session
terminate_session(session_id="sess-001")
delete_session(session_id="sess-001")
```

### Example 4: Multi-session Parallel Management

```
start_session(command="ping", args=["-c", "5", "google.com"], name="ping-test")
  → session_id: "a1b2c3"

start_session(command="python3", args=["-m", "http.server", "8080"], name="web-server")
  → session_id: "d4e5f6"

list_sessions()
  → [{id: "a1b2c3", status: "running"}, {id: "d4e5f6", status: "running"}]

read_output(session_id="a1b2c3")  → ping statistics

terminate_session(session_id="a1b2c3")
terminate_session(session_id="d4e5f6")
```

---

## Tool Reference

Full tool reference: [`docs/mcp-tools.md`](docs/mcp-tools.md). 16 tools total.

| Category | Tools |
|----------|-------|
| Session lifecycle | `start_session`, `send_input`, `read_output`, `send_and_read`, `background_send`, `list_sessions`, `get_session_info`, `terminate_session`, `delete_session` |
| Multi-agent reading | `register_reader`, `unregister_reader` |
| PTY control | `resize_pty` |
| Server discovery | `detect_shell`, `list_ssh_configs` |
| Message persistence | `list_messages`, `get_message` |

---

## License

MIT
