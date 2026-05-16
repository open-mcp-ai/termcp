# termcp

<p align="center">
  <strong>让 AI Agent 拥有交互式终端能力</strong>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.21+-00ADD8.svg" alt="Go 1.21+">
  <img src="https://img.shields.io/badge/Platform-macOS%20%7C%20Linux%20%7C%20Windows-lightgrey" alt="macOS / Linux / Windows">
  <img src="https://img.shields.io/badge/MCP-SSE_Transport-green.svg" alt="MCP SSE">
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
| `--ssh-host` | `127.0.0.1` | 内部 SSH server 监听地址。 |
| `--ssh-port` | `0`（随机）| 内部 SSH server 端口。`0` = 随机；防火墙需要固定端口时设置。 |
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

或通过 CLI：

```bash
claude mcp add --transport sse termcp http://localhost:18765/sse
```

### Open WebUI（Streamable HTTP）

Open WebUI 通过 streamable HTTP 连接 MCP，地址为 `http://<host>:18765/stream`。

termcp 与 Open WebUI 同机运行时：`http://127.0.0.1:18765/stream`。Open WebUI 在 Docker 内、termcp 在宿主机时：`http://host.docker.internal:18765/stream`（macOS/Windows）或宿主机 LAN IP。

### 其他 MCP 客户端

支持 **SSE** 传输的客户端 → `http://<host>:<port>/sse`。  
支持 **streamable HTTP** 的客户端 → `http://<host>:<port>/stream`。

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
| **消息持久化** | 会话记录和 I/O 消息持久化到本地 JSON 文件 |
| **ANSI 转义码清除** | 可选自动去除终端控制序列，AI Agent 获得纯净文本 |
| **带超时的阻塞读取** | Agent 可配置超时等待新输出，sync.Cond 保证及时返回 |
| **跨平台 Shell 检测** | `detect_shell` 探测 termcp 宿主机上的 bash/zsh/fish/pwsh/cmd，混合 Windows/Linux 环境适用 |
| **优雅终止** | 先 SIGTERM，等待可配置宽限期后再 SIGKILL |
| **PTY 尺寸调整** | 运行时动态调整终端行列数 |

---

## 架构设计

```
┌──────┐  SSE/HTTP  ┌──────────────┐  内部 SSH   ┌──────────┐
│Agent │ ──────────> │ Go Server    │ ──────────> │ PTY/     │
│(MCP) │             │ - MCP API    │  (localhost) │ Process  │
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
│   │   ├── handlers.go          # 16 个 Tool 处理器
│   │   └── logging.go           # 结构化 slog 日志（逐工具调用记录）
│   ├── webui/                   # 嵌入 SPA + WebSocket 终端 + REST API
│   ├── sshserver/server.go      # 内部 SSH server (charmbracelet/ssh)
│   ├── sshclient/client.go      # SSH client (crypto/ssh)
│   ├── sshconfig/               # 服务端 SSH profile 存储
│   ├── session/
│   │   ├── session.go           # Session 生命周期（goroutine 安全）
│   │   └── manager.go           # 线程安全会话注册表
│   ├── buffer/buffer.go         # 多读者追加式输出日志
│   ├── storage/store.go         # 原子 JSON 文件持久化
│   ├── message/message.go       # 消息管理（每会话互斥锁）
│   ├── shell/detect.go          # 跨平台 Shell 检测
│   ├── ansi/strip.go            # ANSI 转义码清除
│   └── logansi/handler.go       # 彩色 slog handler
├── pkg/api/types.go             # 公共类型 (Session, Message, SessionMode)
├── go.mod
└── go.sum
```

### 关键设计决策

1. **多读者输出缓冲**：一份追加式字节日志，每个 reader 持有独立读游标。所有 reader 都已越过的前缀可裁剪以控制内存；无固定容量上限或环形覆盖。

2. **内部 SSH 架构**：Server 在 localhost 上启动 charmbracelet/ssh server。每次 `start_session` 通过 crypto/ssh client 创建一个 SSH session，利用 SSH 协议成熟的 PTY 分配、窗口调整、信号转发和环境变量传递机制。Windows 下使用 ConPTY 提供原生伪终端支持。

3. **单 HTTP ServeMux**：Web UI、MCP SSE（`/sse`）、MCP streamable HTTP（`/stream`）、WebSocket 终端（`/api/ui/ws`）共享一个端口。可选 Admin HTTP 使用独立端口。

4. **原子 JSON 持久化**：会话元数据和 I/O 消息通过临时文件 + fsync + rename 存储：
   - `data/sessions.json` — 会话列表
   - `data/messages/{session_id}/index.json` — 消息索引
   - `data/messages/{session_id}/messages/{msg_id}.json` — 消息内容

5. **会话生命周期安全**：退出 goroutine 是 `Status`/`ExitCode` 的唯一权威（通过 `sync.Once`）。终止操作是幂等的。标准输入写入通过专用互斥锁串行化。

---

## 效果示例

### 示例 1：SSH 远程操作

```
AI Agent 操作流程                              进程输出
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

### 示例 2：Python REPL 调试

```
start_session(command="python3", mode="pty")
                                    ←    "Python 3.10.12\n>>> "

send_and_read(text="data = [1, 2, 3, 4, 5]", press_enter=true)
                                    ←    ">>> "

send_and_read(text="sum(data)", press_enter=true)
                                    ←    "15\n>>> "
```

### 示例 3：多 Agent 协作

```
# Agent A 启动监控进程
start_session(command="top", mode="pty")
  → session_id: "sess-001"

# Agent B 加入同一会话，不窃取输出
register_reader(session_id="sess-001")
  → reader_id: 2

# Agent A 读取自己的游标位置
read_output(session_id="sess-001", reader_id=1)
  → "PID USER  PR  NI  VIRT  RES  SHR S %CPU %MEM   TIME+ COMMAND..."

# Agent B 从头独立读取
read_output(session_id="sess-001", reader_id=2)
  → "top - 14:32:10 up 3 days,  2:15,  1 user,  load average: 0.52, 0.58, 0.59..."

# Agent B 完成
unregister_reader(session_id="sess-001", reader_id=2)

# Agent A 终止会话
terminate_session(session_id="sess-001")
delete_session(session_id="sess-001")
```

### 示例 4：多会话并行管理

```
start_session(command="ping", args=["-c", "5", "google.com"], name="ping-test")
  → session_id: "a1b2c3"

start_session(command="python3", args=["-m", "http.server", "8080"], name="web-server")
  → session_id: "d4e5f6"

list_sessions()
  → [{id: "a1b2c3", status: "running"}, {id: "d4e5f6", status: "running"}]

read_output(session_id="a1b2c3")  → ping 统计信息

terminate_session(session_id="a1b2c3")
terminate_session(session_id="d4e5f6")
```

---

## 工具参考

完整工具参考见 [`docs/mcp-tools.md`](docs/mcp-tools.md)，共 16 个工具。

| 分组 | 工具 |
|------|------|
| 会话生命周期 | `start_session`、`send_input`、`read_output`、`send_and_read`、`background_send`、`list_sessions`、`get_session_info`、`terminate_session`、`delete_session` |
| 多 Agent 共读 | `register_reader`、`unregister_reader` |
| PTY 控制 | `resize_pty` |
| 服务端发现 | `detect_shell`、`list_ssh_configs` |
| 消息持久化 | `list_messages`、`get_message` |

---

## 社区 / 友联

- [linux.do](https://linux.do/) — 中文技术社区

---

## License

MIT
