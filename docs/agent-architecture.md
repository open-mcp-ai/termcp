# termcp Agent Architecture

## 概述

termcp Agent 是一个远程节点代理程序，通过 WebSocket 连接到 termcp Server，在远程机器上提供终端、端口转发和 SFTP 文件操作能力。Agent 是独立二进制，与 Server 解耦——Server 不再内置 agent 连接管理逻辑。

## 协议栈

```
┌──────────────────────────┐
│     Application          │
│  (SSH / PTY / SFTP)      │
├──────────────────────────┤
│   SSH over smux stream   │
├──────────────────────────┤
│  smux (stream multiplex) │
├──────────────────────────┤
│      WebSocket           │
└──────────────────────────┘
```

1. **WebSocket**：Agent 与 Server 之间的持久双向连接
2. **smux**：在单条 WebSocket 连接上多路复用多条逻辑流（每个 SSH session / forward 一条流）
3. **SSH over smux stream**：每条 smux 流上运行 SSH 协议（Server 作为 SSH client，Agent 作为 SSH server）
4. **应用层**：PTY 终端、SFTP 文件操作、端口转发

## Server 端

### 端点

| 端点 | 用途 |
|------|------|
| `GET /ws/agent` | Agent WebSocket 连接入口 |

### 连接流程

1. Agent 发起 WebSocket 连接到 `wss://<server>/ws/agent`
2. Agent 发送注册消息（包含 agent_id、hostname 等元数据）
3. Server 建立 smux session
4. Server 在 SSH config store 中创建动态 entry（kind=agent）
5. 此后 MCP 工具和 Web UI 可通过 `ssh_config=<agent_id>` 在该 agent 上创建 session/forward/file 操作

### 生命周期

```
Agent 连接 → 注册 → smux session 建立 → SSH config 注入
    ↓
会话/转发创建（通过 smux stream bridge）
    ↓
Agent 断开 → 级联清理（终止所有关联 session，关闭所有 forward）
```

## Agent 端

### 构建

```bash
go build -o termcp-agent ./cmd/agent
```

### 运行

```bash
./termcp-agent \
  --agent-id my-remote-node \
  --server-url wss://termcp-server.example.com/ws/agent \
  --reconnect-delay 5
```

### 参数

| 参数 | 必需 | 说明 |
|------|:---:|------|
| `--agent-id` | ✅ | Agent 唯一标识，注册后作为 `ssh_config` 名称 |
| `--server-url` | ✅ | termcp Server 的 WebSocket 地址 |
| `--reconnect-delay` | ❌ | 断线重连间隔（秒），默认 5 |
| `--ssh-listen` | ❌ | 可选 SSH 监听地址（Agent 上启动内嵌 SSH server） |

### 内部流程

1. 解析参数，建立 WebSocket 连接
2. 发送注册消息：`{"type":"register","agent_id":"...","hostname":"..."}`
3. 等待 Server 确认注册：`{"type":"registered"}`
4. 建立 smux client session
5. 循环接受 smux stream：
   - SSH session stream：桥接到 Agent 本地 SSH server（start_session）
   - Forward stream：根据 header 中的 host:port，桥接 TCP 连接到目标（forward_port）

## 端口转发

### 三种模式

| 方向 | 对应 SSH 参数 | 行为 |
|------|:---:|------|
| Local Forward | `-L` | termcp 监听本地端口 → Agent 侧 smux → 目标 |
| Remote Forward | `-R` | Agent 侧 listen → smux → termcp 侧目标 |
| Dynamic Forward | `-D` | SOCKS5 代理，termcp 监听 → 通过 Agent/SSH 隧道代理 TCP |

### 数据路径

**Agent 路径（已移除）**：直接使用 smux stream 桥接，不需要 SSH Client。

**SSH 路径（当前实现）**：复用已有 session 的 SSH Client，通过 `ssh.Client.Dial("tcp", target)` 建立 direct-tcpip 通道。

## SFTP 文件操作

通过 SSH session 的 SFTP 子系统实现，支持：

- `file_read` — 读取文件（支持 offset/length 分片，text/hex/file 模式）
- `file_write` — 写入文件（支持 offset 定位，inline data 或从本地文件读取）
- `file_stat` — 获取文件/目录信息
- `list_files` — 列出目录
- `download` — 流式下载（支持 Range header 断点续传）
- `upload` — 流式上传（支持 Content-Range header）
- `rename` — 重命名/移动
- `mkdir` — 创建目录
- `delete` — 删除文件/目录

## 与 Server 的解耦

Agent 相关代码已从 Server 主代码库中移除：

- **删除**：`internal/agent/manager.go`、`types.go`、`wsagent.go`、`run.go`
- **删除**：`cmd/agent/main.go`（独立 Agent 二进制）
- **移除引用**：`main.go`、`mcp/server.go`、`mcp/handlers.go`、`webui/handler.go`、`sshconfig/config.go`、`sshconfig/store.go`

Agent 作为独立项目维护，通过 WebSocket 协议与 Server 通信。Server 端保留 `internal/agent/forward.go`（端口转发管理器）和 `internal/agent/file.go`（SFTP 客户端封装），这些是 SSH 路径共用的基础能力。
