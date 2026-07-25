<div id="top">



<p align="center">
    <img src="./docs/assets/logo.png"></img>
  <h1 align="center">termcp</h1>
  <p align="center"><em>让 AI Agent 拥有交互式终端能力。</em></p>
</p>



<p align="center">
  <a href="https://github.com/open-mcp-ai/termcp/stargazers">
    <img src="https://img.shields.io/github/stars/open-mcp-ai/termcp?label=Stars&logo=github&style=for-the-badge" alt="Stars">
  </a>
  <a href="https://github.com/open-mcp-ai/termcp/forks">
    <img src="https://img.shields.io/github/forks/open-mcp-ai/termcp?label=Forks&logo=github&style=for-the-badge" alt="Forks">
  </a>
  <img src="https://img.shields.io/badge/平台-macOS%20%7C%20Linux%20%7C%20Windows-2786ff?style=for-the-badge" alt="平台">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=for-the-badge&logo=go&logoColor=white" alt="Go 1.25+">
  <a href="./LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-green?style=for-the-badge" alt="MIT License">
  </a>
</p>



<p align="center">
  <strong>中文</strong> | <a href="./README.md">English</a>
</p>



---

## 简介

`termcp `是一个go语言编写的包含MCP服务器，以**SSH**会话的形式，让AI Agent能够持续地管理、调度交互式程序。此外，termcp还有专门用于管理这些会话的界面，使得用户能够完整地观测AI Agent的行为。同时用户能够像使用SSH一样，直接与受控机器进行交互或调整AI Agent行为。

https://github.com/user-attachments/assets/d06a3c36-250a-4eeb-aefa-e80d13d1551c

## 为什么选 termcp

### 打破边界

Agent 原生只能执行一次性命令，运行完就返回。但现实中有大量工作是**多轮交互**的，例如：

- SSH 登录一台主机，先输密码，_再_执行命令。
- 在 Python REPL 里逐行调试代码。
- 回答安装程序里深埋的 `[Y/n]` 提示。
- 驱动 `top`、`htop`、或 impacket 这类终端依赖型工具。

这些场景里进程持续运行，Agent 必须在**多个对话轮次间读写进程的 I/O**。由此诞生了许多专门的MCP，但是为什么不直接赋予Agent双手，让他能够直接交互呢？`termcp`让 AI Agent打破了进程交互的边界，不再需要为每个交互工具安装编写单独的mcp，使其能够直接地持续管理、调度交互式程序，如**TUI**、**REPL**、**GDB**、**msfconsole**、**vim**等。

### 可视化管理

`termcp` 提供了一个会话管理界面，让你和 Agent 对进程里正在发生的一切一目了然:

- **多会话仪表盘**:所有正在运行的会话都在这里，以名称区分，随时切换，随时接管。
- **实时 AI Agent 行为观测**:像操作本地终端一样，直接在浏览器里看到 `htop` 的动态界面、`vim` 的编辑过程，或者安装程序弹出的彩色提示，不再对着"黑盒"猜测。
- **标签化管理**:一个 SSH 会话下可开多个操作shell，每个 shell 在 UI 里是独立标签页，Agent 在 A 标签调试、在 B 标签查日志，互不干扰。
- **端口转发可视化**：会话相关的所有端口转发等功能参数都列在面板里，本地/远程端口、协议一目了然。
- **文件管理**：在管理界面里直接浏览目录、上传下载、重命名、建目录。
- **连接模板集中托管**：提供统一的SSH配置管理，如果不想让Agent知道ssh具体配置，只需要告知Agent需要使用的ssh配置文件。

## 快速导航

- [功能特性](#功能特性)
- [快速开始](#快速开始)
- [使用](#使用)
- [接入 MCP 客户端](#接入-mcp-客户端)
- [示例](#示例)
- [工具参考](#工具参考)
- [已知限制](#已知限制)

## 功能特性

- **🟦 支持多轮交互** —— 进程持续运行，Agent 可跨多个对话轮次驱动，而非一次性调用即返回。
- **🟪 真实终端环境** —— 完整模拟真实终端，`vim`、`top`、`gdb` 等依赖终端特性的程序均可正常运行，跨平台兼容。
- **🟧 内建可视化界面** —— 浏览器即可访问实时终端、会话列表、历史输出回放，单端口提供服务，无需额外部署。
- **🟨 多 Agent 并行不冲突** —— 多个 Agent 可同时读取同一会话，各自维护独立游标，输出互不抢占。
- **🟩 远程操作一体集成** —— 单条 SSH 连接内完成命令执行、文件传输与端口转发，无需重复建立连接。

## 快速开始

### 下载

前往 Releases 页面,下载对应平台的预编译二进制:

| 平台                  | 文件                       |
| :-------------------- | :------------------------- |
| Linux (x86_64)        | `termcp-linux-amd64`       |
| Linux (ARM64)         | `termcp-linux-arm64`       |
| macOS (Intel)         | `termcp-darwin-amd64`      |
| macOS (Apple Silicon) | `termcp-darwin-arm64`      |
| Windows (x86_64)      | `termcp-windows-amd64.exe` |
| Windows (ARM64)       | `termcp-windows-arm64.exe` |

### 编译

```bash
# 克隆
git clone https://github.com/open-mcp-ai/termcp.git
cd termcp

# 编译
go build -o termcp .

# 运行（默认：loopback，端口 18765）
./termcp --data-dir ./data
```

浏览器打开 `http://127.0.0.1:18765` 即可进入 **Web 界面**。

## 使用

### 命令行

```text
termcp [flags]
termcp ssh-config init <name> -data-dir <dir>   # 创建远端 SSH 配置模板
termcp ssh-config list -data-dir <dir>          # 列出已存的 SSH 配置名
```

| Flag            | 默认值      | 说明                                                         |
| --------------- | ----------- | ------------------------------------------------------------ |
| `--host`        | `127.0.0.1` | HTTP 绑定地址。`0.0.0.0` 监听所有网卡。                      |
| `--port`        | `18765`     | HTTP 端口。Web UI、MCP SSE、MCP streamable HTTP 共用。       |
| `--data-dir`    | `./data`    | 持久化目录（会话、消息、SSH 配置）。不存在则自动创建。       |
| `--log-level`   | `info`      | 日志级别：`debug` / `info` / `warn` / `error`。`debug` 显示 MCP 工具调用。 |
| `--admin-host`  | `127.0.0.1` | 管理 HTTP API 绑定地址。                                     |
| `--admin-port`  | `0`（关闭） | 管理 HTTP API 端口。非零时必须配 `--admin-token`。           |
| `--admin-token` | —           | 管理 SSH 配置的 Bearer / `X-Admin-Token`。                   |

### 示例

```bash
# 监听所有网卡
./termcp --data-dir ./data --host 0.0.0.0

# 启用管理员Token
./termcp --data-dir ./data --admin-port 9090 --admin-token "my-secret"

# 创建 SSH 配置模板
./termcp ssh-config init my-server --data-dir ./data

# 列出可用 SSH 配置
./termcp ssh-config list --data-dir ./data
```

## 接入 MCP 客户端

### Claude Code（SSE）

```json
{
  "mcpServers": {
    "termcp": {
      "type": "sse"，
      "url": "http://your-server:18765/sse"
    }
  }
}
```

或用 CLI：

```bash
claude mcp add --transport sse termcp http://localhost:18765/sse
```

### Open WebUI（Streamable HTTP）

将 Open WebUI 指向 `http://<host>:18765/stream`。

- 同机：`http://127.0.0.1:18765/stream`。
- Open WebUI 在 Docker 内、termcp 在宿主机：`http://host.docker.internal:18765/stream`（macOS/Windows），或宿主机局域网 IP。

### 其他 MCP 客户端

- SSE 传输 → `http://<host>:<port>/sse`
- Streamable HTTP → `http://<host>:<port>/stream`


---

