<div id="top">



<p align="center">
    <img src="./docs/assets/logo.png"></img>
  <h1 align="center">termcp</h1>
  <p align="center"><em>Give AI Agents interactive terminal capabilities.</em></p>
</p>



<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge" alt="Forks">
  </a>
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="Platform">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-green?style=for-the-badge" alt="MIT License">
  </a>
</p>



<p align="center">
  <strong>English</strong> | <a href="./README.zh.md">中文</a>
</p>



---

## Introduction

`termcp` is an MCP server written in Go that exposes interactive programs to AI Agents as persistent **SSH** sessions, letting Agents continuously manage and drive them. On top of that, termcp ships a dedicated session management UI that gives you full visibility into the Agent's behavior. You can also interact directly with the controlled machine — or adjust the Agent's behavior — just as you would over a normal SSH connection.


https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c


## Why termcp

### Breaking the Boundary

Agents can natively only execute one-shot commands — they run and return. But a huge amount of real-world work is **multi-turn interaction**, for example:

- SSH into a host: enter a password first, _then_ run commands.
- Debug code line by line in a Python REPL.
- Answer a `[Y/n]` prompt buried deep inside an installer.
- Drive terminal-dependent tools like `top`, `htop`, or impacket.

In these scenarios the process keeps running, and the Agent must **read and write the process's I/O across multiple conversation turns**. Plenty of specialized MCPs have sprung up to handle these — but why not just give the Agent hands so it can interact directly? `termcp` breaks that boundary for AI Agents: no more writing or installing a separate MCP for every interactive tool. The Agent can directly and continuously manage and drive interactive programs like **TUIs**, **REPLs**, **GDB**, **msfconsole**, **vim**, and more.

### Visual Management

`termcp` provides a session management UI that gives you and the Agent a clear view of everything happening inside the processes:

- **Multi-session dashboard**: every running session lives here, distinguished by name — switch between them or take over at any time.
- **Real-time Agent behavior observation**: just like a local terminal, watch `htop`'s live display, `vim`'s editing process, or an installer's colorful prompts right in the browser — no more guessing at a "black box".
- **Tab-based management**: under a single SSH session you can open multiple operating shells, each rendered as an independent tab in the UI. The Agent can debug in tab A and tail logs in tab B without interference.
- **Port forwarding at a glance**: every port-forwarding rule tied to a session is listed in the panel — local/remote ports and protocols, all visible at a glance.
- **File management**: browse directories, upload/download, rename, and create folders directly from the management UI.
- **Centralized connection templates**: a unified SSH config store. If you'd rather not expose the actual SSH credentials to the Agent, just tell it the name of the SSH config to use.

## Quick Navigation

- [Features](#features)
- [Quick Start](#quick-start)
- [Usage](#usage)
- [Connecting MCP Clients](#connecting-mcp-clients)
- [Examples](#examples)
- [Tool Reference](#tool-reference)
- [Known Limitations](#known-limitations)

## Features

- **🟦 Multi-turn interaction** — The process keeps running; the Agent can drive it across multiple conversation turns instead of a one-shot call-and-return.
- **🟪 Real terminal environment** — A fully emulated real terminal, so programs that depend on terminal features like `vim`, `top`, `gdb` all run correctly, with cross-platform compatibility.
- **🟧 Built-in visual UI** — Access live terminals, session lists, and output-history replay straight from a browser. Served from a single port, no extra deployment needed.
- **🟨 Multiple Agents, no conflicts** — Multiple Agents can read the same session simultaneously, each maintaining its own independent cursor, with no output stealing.
- **🟩 Remote operations, all integrated** — Command execution, file transfer, and port forwarding all over a single SSH connection, with no need to re-establish connections.

## Quick Start

### Download

Head to the Releases page and download the pre-built binary for your platform:

| Platform            | File                       |
| :------------------ | :------------------------- |
| Linux (x86_64)      | `termcp-linux-amd64`       |
| Linux (ARM64)       | `termcp-linux-arm64`       |
| macOS (Intel)       | `termcp-darwin-amd64`      |
| macOS (Apple Silicon) | `termcp-darwin-arm64`    |
| Windows (x86_64)    | `termcp-windows-amd64.exe` |
| Windows (ARM64)     | `termcp-windows-arm64.exe` |

### Build

```bash
# Clone
git clone https://github.com/open-mcp-ai/termcp.git
cd termcp

# Build
go build -o termcp .

# Run (defaults: loopback, port 18765)
./termcp --data-dir ./data
```

Open `http://127.0.0.1:18765` in your browser to enter the **Web UI**.

## Usage

### Command Line

```text
termcp [flags]
termcp ssh-config init <name> -data-dir <dir>   # Create a remote SSH config template
termcp ssh-config list -data-dir <dir>          # List existing SSH config names
```

| Flag            | Default       | Description                                                              |
| --------------- | ------------- | ------------------------------------------------------------------------ |
| `--host`        | `127.0.0.1`   | HTTP bind address. `0.0.0.0` listens on all interfaces.                  |
| `--port`        | `18765`       | HTTP port. Shared by the Web UI, MCP SSE, and MCP streamable HTTP.       |
| `--data-dir`    | `./data`      | Persistence directory (sessions, messages, SSH configs). Auto-created.   |
| `--log-level`   | `info`        | Log level: `debug` / `info` / `warn` / `error`. `debug` shows MCP tool calls. |
| `--admin-host`  | `127.0.0.1`   | Admin HTTP API bind address.                                             |
| `--admin-port`  | `0` (off)     | Admin HTTP API port. Requires `--admin-token` when non-zero.             |
| `--admin-token` | —             | Bearer / `X-Admin-Token` for managing SSH configs.                       |

### Examples

```bash
# Listen on all interfaces
./termcp --data-dir ./data --host 0.0.0.0

# Enable the admin token
./termcp --data-dir ./data --admin-port 9090 --admin-token "my-secret"

# Create an SSH config template
./termcp ssh-config init my-server --data-dir ./data

# List available SSH configs
./termcp ssh-config list --data-dir ./data
```

## Connecting MCP Clients

### Claude Code (SSE)

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

- Same machine: `http://127.0.0.1:18765/stream`.
- Open WebUI inside Docker, termcp on the host: `http://host.docker.internal:18765/stream` (macOS/Windows), or the host's LAN IP.

### Other MCP Clients

- SSE transport → `http://<host>:<port>/sse`
- Streamable HTTP → `http://<host>:<port>/stream`


---

