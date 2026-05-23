# Changelog

## v0.0.4 — 2026-05-23

### 新功能

- **Web UI**：浏览器端终端（xterm.js + WebSocket），支持实时会话列表（SSE）、全量输出回放、连接模板一键启动。单端口 18765 同时服务 MCP 和 Web UI。

- **服务端 SSH 配置**：SSH 连接信息存储在 `data/ssh_configs/<name>/config.json`，MCP 工具只需传配置名。支持 `internal`（loopback）和 `remote`（远端 SSH）两种类型，可选 `default_shell` 和 `default_mode`。新增 `ssh-config init` / `ssh-config list` CLI 子命令和可选 Admin HTTP API。

- **Streamable HTTP 传输**：新增 `/stream` 端点（MCP Streamable HTTP 规范），兼容 Open WebUI 等客户端。

- **read_output / send_and_read 增强**：返回 `session_status`、`session_uptime_seconds`；新增 `max_bytes` 参数（默认 8KB）配合 `has_more` 实现大输出分页。

- **MCP Agent 规则注入**：`initialize` 返回 `instructions`，引导 Agent 正确使用工具链（多步执行、crash-loop 检测、密码安全等）。

### 改进

- **PTY 标准终端模式**：SSH 客户端请求 PTY 时设置完整 termios（`ICANON`/`ICRNL`/`ONLCR`/`OPOST`/`ISIG`），修复 Python 3.13 pyrepl 崩溃等交互式程序异常。

- **输出缓冲区重构**：从 ring buffer 改为 append-only 多读者缓冲。每个 reader 独立游标，已消费前缀自动压缩，无固定容量覆盖。

- **TERM 环境变量传播**：PTY 请求的 `TERM` 值（如 `xterm-256color`）正确传递给子进程环境，修复 CI 中 `TERM=dumb` 导致的测试失败。

### 修复

- **Shell 历史展开干扰**：交互式 shell 启动时自动禁用 `!` 历史展开（zsh: `-o NO_BANG_HIST`，bash/sh: `+o histexpand`），防止含 `!` 的密码、URL 执行失败。

- **移除 SFTP 工具**：删除 `upload_file`/`download_file`/`list_files`，清理 sftp subsystem 残留，`trust_unknown_host` 默认 `false`。

- **Windows 跳过 TERM 测试**：`TestServer_PtyEnviron` 在 Windows 跳过，TERM 是 Unix 概念。

- **优化 .gitignore**：防范二进制文件误提交。

## v0.0.3 — 2026-05-11

### 新功能

- **detect_shell MCP 工具**：探测 termcp 主机上的可用交互 shell（bash/zsh/fish/pwsh/cmd），返回路径、family 和提示。跨平台混合环境中 Agent 可据此选择正确的命令语法。

### 改进

- **Shell 检测重构**：提取为可注入的 `Detector` 结构体，测试时可替换为固定实现，消除环境依赖。

- **Debug 日志增强**：MCP 工具调用时记录请求参数和输出预览，便于排查 Agent 行为。

### 修复

- **Kali sudo 密码提示泄漏**：修复 sudo 密码提示输出到父进程 TTY 的问题。

## v0.0.2 — 2026-05-07

### 新功能

- **Windows 平台支持**：全平台 Windows 支持，通过 ConPTY 运行 PowerShell PTY 会话。charmbracelet/ssh 提供原生伪终端分配，输入编码（CRLF）和输出规范化自动处理。

- **跨平台 CI**：测试 workflow 覆盖三平台 — `ubuntu-latest`、`macos-latest`、`windows-latest`，PR 和 push 到 `main`/`dev` 均触发。

- **多平台构建发布**：Release workflow 构建 6 个目标 — `linux/amd64`、`linux/arm64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`windows/arm64`。

### 改进

- **SSH 库替换**：`gliderlabs/ssh` → `charmbracelet/ssh`（`39e85e4`）。charmbracelet/ssh 内置 `AllocatePty()` 自动管理 PTY 生命周期，移除手工 `pty.StartWithSize`/`io.Copy`/`pty.Setsize` 代码。公共 API（`New`/`Start`/`Stop`/`Addr` 等）签名不变。

- **跨平台输入处理**：`SendInput` 在 `press_enter=true` 时自动选择平台换行符 — Windows 用 CRLF（`\r\n`），Unix 用 LF（`\n`）（`83bfd12`）。

- **SFTP 路径规范化**：远程路径自动将反斜杠转为正斜杠，避免 Windows 路径分隔符导致 SSH 文件操作失败（`83bfd12`）。

- **PowerShell 优化**：交互测试使用 `powershell.exe -NoLogo -NoProfile` 抑制启动横幅，用 `Write-Output`（原生 cmdlet）替代 `echo` 别名确保 ConPTY 下输出稳定（`3411511`、`5816374`）。

- **信号转发验证**：新增 `TestServer_SignalTerm` 和 `TestServer_SignalInterrupt` 验证 SIGTERM/SIGINT 通过 SSH 通道正确转发至目标进程（`96ba75c`）。

### 修复

- **Windows PTY 交互输出为空**：ARM64 上 PowerShell/ConPTY 输出时序问题导致首次读取为空。改为 `Write-Output` 原生命令配合 marker 轮询读取（`testReadOutputUntil`）解决（`5816374`）。

- **Windows 进程退出行为不确定**：PowerShell `-Command` 在 ConPTY 下完成命令后保持交互态，自然退出测试在 Windows 跳过（`3411511`、`e501489`）。

- **Windows 跳过 POSIX signal 测试**：`SIGTERM`/`SIGINT` 测试在 Windows 跳过，Windows 不支持 POSIX 信号（`e501489`）。
