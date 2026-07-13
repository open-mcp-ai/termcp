# termcp

<p align="center">
  <strong>让 AI Agent 拥有交互式终端能力</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8.svg" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey" alt="macOS / Linux / Windows">
  <img src="https://img.shields.io/badge/MCP-SSE_&_Streamable_HTTP-green.svg" alt="MCP SSE & Streamable HTTP">
  <img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="MIT License">
</p>

<p align="center">
  <a href="https://linux.do/"><img src="https://img.shields.io/badge/🐧-linux.do-ff69b4.svg" alt="linux.do"></a>
  <a href="./README.md"><img src="https://img.shields.io/badge/🌏-English-blue.svg" alt="English"></a>
</p>

<p align="center">
  <strong>中文</strong> | <a href="./README.md">English</a>
</p>

---

## 快速开始

```bash
# 编译
go build -o termcp .

# 运行（默认：loopback，端口 18765）
./termcp --data-dir ./data
```

浏览器打开 `http://127.0.0.1:18765` 即可进入 **Web 界面**。

Web 界面功能：
- **浏览器终端**（xterm.js + WebSocket）—— 启动会话、发送输入、实时查看输出
- **会话列表**，支持 SSE 实时更新
- **连接模板**，支持已保存的 SSH profile（`internal` loopback 或 `remote` 远端主机）
- 完整历史回放 —— 重连后可从开头重读输出

## 命令行

```
termcp [flags]
termcp ssh-config init <名称> -data-dir <目录>   # 创建远端 SSH 配置模板
termcp ssh-config list -data-dir <目录>          # 列出所有 SSH 配置名称
```

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--host` | `127.0.0.1` | HTTP 监听地址。`0.0.0.0` 监听所有网卡。 |
| `--port` | `18765` | HTTP 端口。Web UI、MCP SSE、MCP streamable HTTP 共享此端口。 |
| `--data-dir` | `./data` | 持久化目录（会话、消息、SSH 配置）。不存在时自动创建。 |
| `--log-level` | `info` | 日志级别：`debug`、`info`、`warn`、`error`。用 `debug` 查看 MCP 工具调用详情。 |
| `--admin-host` | `127.0.0.1` | Admin HTTP API 监听地址。 |
| `--admin-port` | `0`（禁用）| Admin HTTP API 端口。非零时需配合 `--admin-token`。 |
| `--admin-token` | — | Bearer / `X-Admin-Token`，用于通过 HTTP 管理 SSH 配置。 |

### 使用示例

```bash
# 仅 loopback，详细日志
./termcp --data-dir ./data --log-level debug

# 监听所有网卡（LAN/WAN — 生产环境请加反向代理做认证）
./termcp --data-dir ./data --host 0.0.0.0

# 启用 Admin API 管理 SSH 配置
./termcp --data-dir ./data --admin-port 9090 --admin-token "my-secret"

# 创建远端 SSH 配置
./termcp ssh-config init my-server --data-dir ./data
# 编辑 ./data/ssh_configs/my-server/config.json 填入凭据

# 列出现有 SSH 配置
./termcp ssh-config list --data-dir ./data
```

## MCP 配置

termcp 在同一端口暴露 **同一套 MCP 工具** 的两种传输。按客户端能力选择，不是两套服务。

| 客户端支持… | 使用 | URL |
|-------------|------|-----|
| **SSE** | SSE | `http://<host>:<port>/sse` |
| **HTTP / streamable HTTP** | Streamable HTTP | `http://<host>:<port>/stream` |

浏览器 Web UI → **API / MCP**（`/api.html`）可按当前 origin 复制 MCP 配置，并查看 HTTP API 列表。

### Claude Code

**SSE**（常用）：

```bash
claude mcp add --transport sse termcp http://localhost:18765/sse
```

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

只配置 `/sse` 即可；SSE 客户端会自动用 `POST /message` 发 JSON-RPC。

**Streamable HTTP**（Claude CLI 的 transport 名是 `http`）：

```bash
claude mcp add --transport http termcp http://localhost:18765/stream
```

```json
{
  "mcpServers": {
    "termcp": {
      "type": "http",
      "url": "http://your-server:18765/stream"
    }
  }
}
```

单路径，**不要**再拼 `/sse` 或 `/message`。

### 其他客户端

- Open WebUI 和其他 HTTP / Streamable HTTP 客户端：`http://<host>:18765/stream`
- SSE 客户端：`http://<host>:18765/sse`
- JSON 配置客户端：使用 Web UI **API / MCP**（`/api.html`）里的 `mcpServers` 片段

两套传输共用工具与 `instructions`。连不上时优先检查路径是否选错（`/sse` vs `/stream`）。

---

## 项目介绍

`termcp` 是一个基于 MCP (Model Context Protocol) 协议的服务端，让 AI Agent（如 Claude Code）能够启动、操控和管理**长时间运行的交互式进程**。

### 为什么需要它？

AI Agent 原生只能执行一次性命令——执行完毕后立刻返回结果。但现实中大量场景需要**多轮交互**：

- SSH 到远程服务器，先输密码，再执行命令
- Python REPL 中逐行调试代码
- 交互式安装程序中回答 `[Y/n]` 提示
- 使用 `top`、`htop` 等需要终端的命令
- 运行安全工具（如 impacket）进行多步骤操作

这些场景下，进程持续运行，AI Agent 需要在**多个对话轮次中反复读写**进程的输入输出。`termcp` 正是为此而设计的桥梁。

### 核心特性

| 特性 | 说明 |
|------|------|
| **Web 界面** | 浏览器终端（xterm.js + WebSocket）；会话列表、连接模板、输出回放 |
| **多 Agent 会话共享** | 多个 AI Agent 可同时从同一会话独立读取，各持游标互不干扰 |
| **PTY 和 Pipe 双模式** | PTY 模式模拟真实终端；Pipe 模式适用于简单 stdin/stdout 交互 |
| **远程部署** | SSE over HTTP 传输 — Agent 和 Server 可运行在不同机器上 |
| **服务端 SSH profile** | SSH 连接信息存为 `{data-dir}/ssh_configs/<名称>/config.json`；MCP 工具只传名称 |
| **多会话管理** | 同时管理多个独立进程，互不干扰 |
| **Shell 通道复用** | `start_subshell` 在同一 SSH 连接上开新通道——无新 TCP 握手；`close_shell` 只关一个通道，不拆会话 |
| **端口转发** | 基于 SSH 通道的 `ssh -L` / `-R` / `-D` 本地、远端、动态（SOCKS5）转发 |
| **SFTP 文件操作** | `file_read` / `file_write` / `file_stat` / `file_delete` / `file_rename` / `file_mkdir`，外加 `get_file_urls` 提供 HTTP 上传下载 URL |
| **消息持久化** | 会话记录和 I/O 消息持久化到本地 JSON 文件 |
| **ANSI 转义码清除** | 可选自动去除终端控制序列，AI Agent 获得纯净文本 |
| **带超时的阻塞读取** | Agent 可配置超时等待新输出，sync.Cond 保证及时返回 |
| **跨平台 Shell 检测** | `detect_shell` 探测 termcp 宿主机上的 bash/zsh/fish/pwsh/cmd，混合 Windows/Linux 环境适用 |
| **优雅终止** | 先 SIGTERM，等待可配置宽限期后再 SIGKILL |
| **PTY 尺寸调整** | 运行时动态调整终端行列数 |
| **安全的 Shell 启动** | 自动禁用历史展开（`!`）；TERM 正确传播给子进程 |

---

## 架构设计

```
┌──────┐  SSE/HTTP  ┌──────────────┐  内存 SSH   ┌──────────┐
│Agent │ ──────────> │ Go Server    │ ──────────> │ PTY/     │
│(MCP) │             │ - MCP API    │ (无 TCP 端口)│ Process  │
└──────┘             │ - Web UI     │              └──────────┘
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

### 项目结构

```
.
├── main.go                      # 入口
├── internal/
│   ├── config/config.go         # 配置与校验
│   ├── mcp/
│   │   ├── server.go            # MCP SSE server & Tool 注册
│   │   ├── handlers.go          # 31 个 Tool 处理器
│   │   └── logging.go           # 结构化 slog 日志（逐工具调用记录）
│   ├── webui/                   # 嵌入 SPA + WebSocket 终端 + REST API
│   ├── sshserver/server.go      # 内存 SSH server (charmbracelet/ssh)
│   ├── sshclient/               # SSH client (crypto/ssh) + ChildShell 通道复用
│   ├── sshconfig/               # 服务端 SSH profile 存储
│   ├── session/                 # Session 生命周期 + 线程安全注册表
│   ├── buffer/buffer.go         # 多读者追加式输出日志
│   ├── storage/store.go         # 原子 JSON 文件持久化
│   ├── message/message.go       # 消息管理（每会话互斥锁）
│   ├── forward/forward.go       # 端口转发管理器 (-L / -R / -D)
│   ├── sftp/sftp.go             # SFTP 客户端封装（文件工具）
│   ├── shell/detect.go          # 跨平台 Shell 检测
│   ├── ansi/strip.go            # ANSI 转义码清除
│   ├── encoding/                # \xHH / hex 数据编解码
│   └── logansi/handler.go       # 彩色 slog handler
├── pkg/api/types.go             # 公共类型 (Session, Message, SessionMode)
├── go.mod
└── go.sum
```

### 关键设计决策

1. **多读者输出缓冲**：一份追加式字节日志，每个 reader 持有独立读游标。所有 reader 都已越过的前缀可裁剪以控制内存；无固定容量上限或环形覆盖。

2. **内存 SSH 架构**：Server 在进程内运行 charmbracelet/ssh server——**不监听 TCP 端口**。每次 `start_session` 拨号一对内存 net.Conn，通过 crypto/ssh client 创建 SSH session，利用 SSH 协议成熟的 PTY 分配、窗口调整、信号转发和环境变量传递机制。Windows 下使用 ConPTY（经 creack/pty）提供原生伪终端支持。`start_subshell` 复用同一 SSH client 开新 shell 通道，无需重新握手。

3. **单 HTTP ServeMux**：Web UI、MCP SSE（`/sse`）、MCP streamable HTTP（`/stream`）、WebSocket 终端（`/api/ui/ws`）共享一个端口。可选 Admin HTTP 使用独立端口。

4. **原子 JSON 持久化**：会话元数据和 I/O 消息通过临时文件 + fsync + rename 存储：
   - `data/sessions.json` — 会话列表
   - `data/messages/{session_id}/index.json` — 消息索引
   - `data/messages/{session_id}/messages/{msg_id}.json` — 消息内容

5. **会话生命周期安全**：退出 goroutine 是 `Status`/`ExitCode` 的唯一权威（通过 `sync.Once`）。终止操作是幂等的。标准输入写入通过专用互斥锁串行化。

6. **安全的 Shell 环境**：交互式 shell 启动时自动禁用历史展开（zsh: `NO_BANG_HIST`，bash: `+o histexpand`），防止密码、URL 中的 `!` 字符导致命令失败。PTY 请求的 `TERM` 值正确传播给子进程。

---

## 效果示例

### 示例 1：SSH 远程操作

```
AI Agent 操作流程                              进程输出
─────────────────                              ────────────────

start_session(ssh_config="my-server")
  → session_id, shell_id
                                    ←    (用 read_output 读提示)

send_input(shell_id, text="df -h")
press_key(shell_id, key="enter")
read_output(shell_id, timeout=3)
                                    ←    "Filesystem ... Use% Mounted on ..."

terminate_session(session_id)
```

### 示例 2：Python REPL 调试

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

### 示例 3：多 Agent 协作

```
# Agent A 启动监控进程
start_session(command="top", mode="pty")
  → session_id, shell_id

# Agent B 加入同一 shell，不窃取输出
register_reader(shell_id=...)
  → reader_id: 2

# Agent A 读取自己的游标位置
read_output(shell_id=..., reader_id=1)
  → "PID USER  PR  NI  VIRT  RES  SHR S %CPU %MEM   TIME+ COMMAND..."

# Agent B 从头独立读取
read_output(shell_id=..., reader_id=2)
  → "top - 14:32:10 up 3 days,  2:15,  1 user,  load average: 0.52, 0.58, 0.59..."

# Agent B 完成
unregister_reader(shell_id=..., reader_id=2)

# Agent A 终止会话
terminate_session(session_id=...)
```

### 示例 4：多会话并行管理

```
start_session(command="ping", args=["-c", "5", "google.com"], name="ping-test")
  → session_id, shell_id

start_session(command="python3", args=["-m", "http.server", "8080"], name="web-server")
  → session_id, shell_id

list_sessions()
  → [{id: "...", status: "running"}, ...]

read_output(shell_id=..., timeout=1)  # 轮询各 shell，timeout ≤ 3

terminate_session(session_id=...)
```

---

## 工具参考

完整工具参考见 [`docs/mcp-tools.md`](docs/mcp-tools.md)。

| 分组 | 工具 |
|------|------|
| 会话生命周期 | `start_session`、`list_sessions`、`get_session_info`、`terminate_session` |
| Shell I/O | `send_input`、`press_key`、`read_output`、`resize_pty` |
| Shell 通道复用 | `start_subshell`、`list_subshells`、`close_shell` |
| 多 Agent 共读 | `register_reader`、`unregister_reader` |
| 端口转发 | `local_forward`（-L）、`remote_forward`（-R）、`dynamic_forward`（-D）、`list_forwards`、`close_forward` |
| 文件操作（SFTP） | `file_read`、`file_write`、`file_stat`、`file_delete`、`file_rename`、`file_mkdir`、`get_file_urls` 等 |
| 服务端发现 | `detect_shell`、`list_ssh_configs` |
| 消息持久化 | `list_messages`、`get_message` |

---

## 社区 / 友联

- [linux.do](https://linux.do/) — 中文技术社区

---

## License

MIT
