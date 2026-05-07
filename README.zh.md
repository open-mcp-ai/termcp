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
| **多 Agent 会话共享** | 多个 AI Agent 可同时从同一会话独立读取，各持游标互不干扰 |
| **PTY 和 Pipe 双模式** | PTY 模式模拟真实终端；Pipe 模式适用于简单 stdin/stdout 交互 |
| **远程部署** | SSE over HTTP 传输 — Agent 和 Server 可运行在不同机器上 |
| **多会话管理** | 同时管理多个独立进程，互不干扰 |
| **消息持久化** | 会话记录和 I/O 消息持久化到本地 JSON 文件 |
| **ANSI 转义码清除** | 可选自动去除终端控制序列，AI Agent 获得纯净文本 |
| **带超时的阻塞读取** | Agent 可配置超时等待新输出，sync.Cond 保证及时返回 |
| **原子发送读取** | `send_and_read` 一步完成发送 + 读取 |
| **优雅终止** | 先 SIGTERM，等待可配置宽限期后再 SIGKILL |
| **PTY 尺寸调整** | 运行时动态调整终端行列数 |
| **会话清理** | 删除已退出会话，防止资源累积 |

---

## 架构设计

```
┌──────┐  SSE/HTTP  ┌──────────────┐  内部 SSH   ┌──────────┐
│Agent │ ──────────> │ Go Server    │ ──────────> │ PTY/     │
│(MCP) │             │ - MCP API    │  (localhost) │ Process  │
└──────┘             │ - SSH Server │              └──────────┘
                     └──────────────┘
                            │
                            ▼
                     ┌──────────────┐
                     │ JSON Storage │
                     │ - sessions   │
                     │ - messages   │
                     └──────────────┘
```

### 项目结构

```
.
├── cmd/server/main.go           # 入口
├── internal/
│   ├── config/config.go         # 配置与校验
│   ├── mcp/
│   │   ├── server.go            # MCP SSE server & Tool 注册
│   │   └── handlers.go          # 13 个 Tool 处理器
│   ├── sshserver/server.go      # 内部 SSH server (charmbracelet/ssh)
│   ├── sshclient/client.go      # 内部 SSH client (crypto/ssh)
│   ├── session/
│   │   ├── session.go           # Session 生命周期（goroutine 安全）
│   │   └── manager.go           # 线程安全会话注册表
│   ├── buffer/buffer.go         # 多读者环形缓冲区（每读者 1MB）
│   ├── storage/store.go         # 原子 JSON 文件持久化
│   ├── message/message.go       # 消息管理（每会话互斥锁）
│   └── ansi/strip.go            # ANSI 转义码清除
├── pkg/api/types.go             # 公共类型 (Session, Message, SessionMode)
├── go.mod
└── go.sum
```

### 关键设计决策

1. **多读者环形缓冲区**：每个 Agent 注册为独立读者，拥有独立的 `ringbuffer.RingBuffer` 实例。写入广播到所有读者。慢读者丢失最旧数据（覆盖模式），而非阻塞写入者。

2. **内部 SSH 架构**：Server 在 localhost 上启动 charmbracelet/ssh server。每次 `start_process` 通过 crypto/ssh client 创建一个 SSH session，利用 SSH 协议成熟的 PTY 分配、窗口调整、信号转发和环境变量传递机制。Windows 下使用 ConPTY 提供原生伪终端支持。

3. **SSE over HTTP 传输**：与传统基于 stdio 的 MCP server 不同，本 server 暴露 HTTP 端点，支持 MCP SSE transport。Agent 可远程连接，实现跨机器部署。

4. **原子 JSON 持久化**：会话元数据和 I/O 消息通过临时文件 + fsync + rename 存储，防止崩溃时产生半写文件：
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

start_process(
  command="ssh",
  args=["deploy@192.168.1.100"],
  mode="pty"
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

terminate_process(session_id="abc123")
```

### 示例 2：Python REPL 调试

```
start_process(command="python3", mode="pty")
                                    ←    "Python 3.10.12\n>>> "

send_and_read(text="data = [1, 2, 3, 4, 5]", press_enter=true)
                                    ←    ">>> "

send_and_read(text="sum(data)", press_enter=true)
                                    ←    "15\n>>> "
```

### 示例 3：多 Agent 协作

```
# Agent A 启动监控进程
start_process(command="top", mode="pty")
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
terminate_process(session_id="sess-001")
delete_session(session_id="sess-001")
```

### 示例 4：多会话并行管理

```
start_process(command="ping", args=["-c", "5", "google.com"], name="ping-test")
  → session_id: "a1b2c3"

start_process(command="python3", args=["-m", "http.server", "8080"], name="web-server")
  → session_id: "d4e5f6"

list_sessions()
  → [{id: "a1b2c3", status: "running"}, {id: "d4e5f6", status: "running"}]

read_output(session_id="a1b2c3")  → ping 统计信息

terminate_process(session_id="a1b2c3")
terminate_process(session_id="d4e5f6")
```

---

## 工具参考

### `start_process`

启动一个交互式进程。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `command` | string | 是 | — | 要执行的命令 |
| `args` | string[] | 否 | `[]` | 命令参数 |
| `mode` | "pty" \| "pipe" | 否 | `"pty"` | I/O 模式 |
| `name` | string | 否 | 自动生成 | 会话名称 |
| `rows` | integer | 否 | `24` | PTY 行数（1–1000） |
| `cols` | integer | 否 | `80` | PTY 列数（1–1000） |

返回：`{ session_id, pid, initial_output }`

### `send_input`

向进程发送文本。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `text` | string | 是 | — | 要发送的文本 |
| `press_enter` | boolean | 否 | `false` | 是否追加换行 |

### `read_output`

为指定读者读取上次读取后的新输出。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `reader_id` | integer | 否 | `0` | 读者 ID（0 = 默认） |
| `strip_ansi` | boolean | 否 | `true` | 是否清除 ANSI 转义码 |
| `timeout` | number | 否 | `5` | 等待秒数（0.1–60） |
| `max_lines` | integer | 否 | `0` | 最大行数（0 = 无限） |

返回：`{ output, has_more, lines_returned, bytes_returned }`

### `send_and_read`

原子操作：发送输入 + 等待 + 读取输出。参数为 `send_input` 和 `read_output` 的合集。

### `list_sessions`

列出所有会话。返回：`{ sessions: [...] }`

### `get_session_info`

获取会话详情。返回：`{ id, name, command, args, mode, status, exit_code, pid, created_at }`

### `terminate_process`

终止进程。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `force` | boolean | 否 | `false` | 是否直接 SIGKILL |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |

### `delete_session`

从注册表中移除已退出的会话。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |

### `resize_pty`

调整 PTY 尺寸（仅 PTY 模式）。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `rows` | integer | 否 | `24` | 行数 |
| `cols` | integer | 否 | `80` | 列数 |

### `register_reader`

为会话注册一个新的独立读者。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |

返回：`{ reader_id }`

### `unregister_reader`

注销读者以释放资源。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `reader_id` | integer | 是 | — | 读者 ID |

### `list_messages`

列出某个会话的消息索引。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |

返回：`{ messages: [{id, type, created_at, byte_size}, ...] }`

### `get_message`

获取一条或多条消息的内容。

| 参数 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `session_id` | string | 是 | — | 会话 ID |
| `message_ids` | string[] | 否 | — | 要获取的消息 ID |

返回：`{ messages: [{id, session_id, type, content, created_at, byte_size}, ...] }`

---

## 安装

### 从源码编译

```bash
go build -o server ./cmd/server
```

**要求：** Go >= 1.21 / macOS、Linux 或 Windows

### 运行

```bash
./server --host 127.0.0.1 --port 8080 --data-dir ./data
```

启动参数：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--host` | `127.0.0.1` | HTTP server 监听地址 |
| `--port` | `8080` | HTTP server 端口 |
| `--data-dir` | `./data` | JSON 存储目录 |
| `--ssh-host` | `127.0.0.1` | 内部 SSH server 监听地址 |
| `--ssh-port` | `0`（随机） | 内部 SSH server 端口 |

## 配置

### Claude Code

在 `.claude/settings.json` 或 `.mcp.json` 中：

```json
{
  "mcpServers": {
    "interactive-process": {
      "type": "sse",
      "url": "http://your-server:8080/sse"
    }
  }
}
```

或通过 CLI：

```bash
claude mcp add --transport sse interactive-process http://localhost:8080/sse
```

### 其他 MCP 客户端

任何支持 SSE transport 的 MCP 客户端均可连接 `http://<host>:<port>/sse`。

---

## 社区 / 友联

- [linux.do](https://linux.do/) — 中文技术社区

---

## License

MIT
