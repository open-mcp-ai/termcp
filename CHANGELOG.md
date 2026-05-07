# Changelog

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
