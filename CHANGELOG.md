# Changelog

## Unreleased

### Breaking

- **`shell_output` 统一游标读取**：唯一输出读取入口，活会话（内存缓冲）、已退出会话（保留缓冲）、归档/重启恢复会话（磁盘消息流）全部同一套字节流游标语义。新增 `offset`（无状态字节定位，配合 `start_offset`/`end_offset`/`total_bytes`/`has_more` 翻页）与 `tail_lines`（只取末尾 N 行，token 友好）；返回体新增 `source`/`session_id`/`shell_id` 等游标元数据。
- **删除 `history(action=get_transcript)`**：归档输出读取并入 `shell_output`（`shell_id=归档session_id或shell_id`），不再提供全量转录导出，避免一次性把整个会话拖入 LLM 上下文。WebUI 的 `GET /api/history/{id}/transcript` 导出保留。
- **低频工具合并为 action 枚举**：`local_forward` / `remote_forward` / `dynamic_forward` / `list_forwards` / `close_forward` → `forward(action=...)`；`message_list` / `message_get` → `message(action=...)`；7 个 `history_*` 工具 → `history(action=...)`；5 个 `ssh_config_*` 工具 → `ssh_config(action=...)`（写操作通过 `--mcp-manage-ssh-configs` 开关）。
- **删除 9 个低频文件工具**：`file_chmod` / `file_chown` / `file_chtimes` / `file_readlink` / `file_symlink` / `file_link` / `file_truncate` / `file_realpath` / `file_statvfs` → `file_perm` / `file_link` / `file_fs`（各带 `action` 枚举）。
- **工具总数 保持 29**：`history(action=get_transcript)` 移除，`shell_output` 新增 `offset`/`tail_lines` 两参数，`tools/list` 13,585 B → 13,803 B（+1.6%）。

### 改进

- **修复已退出/恢复会话调用文件及转发工具导致 Panic 的问题**：当对已退出（DEAD）或重启后从磁盘恢复的无活跃 SSH 连接的会话调用 `file_write`/`file_read` 等 SFTP 工具或端口转发工具时，`sftpClient` 与端口转发函数补充了 `SSHClient == nil` 的防御性校验，返回规范的 MCP 工具错误，避免了 `pkg/sftp.NewClient(nil)` 空指针解引用崩溃；同时在 `internal/sftp.NewClient` 与 Web UI 文件 API 中增加了对空连接的防守。
- **Web UI 窗口拖拽 resize 体验与 PTY 尺寸同步**：增大拖动手柄触发面积至 30×30px 并提升 z-index 防止被右下角滚动浮标遮挡；改用 Pointer Capture 杜绝甩出窗口时的鼠标丢帧；rAF 节流配合强制重排消除拖动滞后；修复松开鼠标（`onUp`）时活动 shell 通道未派发远程 PTY 尺寸同步的问题。
- **Web UI Sessions 列表批量选择与删除**：标题栏常驻三个纯图标按钮——全选/取消全选（复选框两态图标）、红色垃圾桶批量删除选中（无选中时置灰，气泡提示选中数量）、扫帚一键清理已退出 Dead 会话（弹窗确认后顺序批量删除）；标题栏左侧在选中数 N>0 时实时显示 `[N selected]`。卡片右上角叉号始终可单删；右下角复选框常驻，点击（`stopPropagation`）切换选中态，卡片主体点按仍打开/聚焦终端；选中卡片显示蓝色描边。动态刷新保留已选集合并与全选状态、计数双向联动。
- **引导 Agent 偏好长连接交互会话**：精简 instructions 第 2 条明确指出推荐单个交互会话（保持 cwd/env/审计历史），澄清 `shell_output` 返回的是新增字节（读空 ≠ 没输出，需继续轮询），警告 `session_start` 的 `command/args` 是 run-and-exit 单次程序，不应用于多次分拆 `bash -c`。
- **归档默认读取不再全量倾倒**：无 `offset`/`tail_lines` 的归档读取默认返回末尾最近一块（≤8 KiB，行对齐），配合 `has_more`/`end_offset` 翻页；`max_bytes` 缺省 8192 与 schema 一致（显式 `0` 仍表示不限）。
- **SSH 连接失败可见性**：会话创建失败（如 `ssh dial` 超时/拒绝）现在在 termcp 终端打出 `[ERROR] session create failed`（含目标地址、超时、模式，不含凭据）；MCP 工具错误结果从 Debug 升级为 `[WARN]` 并附带错误预览；Web UI "测试连接"失败同步打 `[WARN]`。连接类错误（超时/拒绝/不可达/重置）自动追加 `Hint:` 诊断提示，MCP 工具结果与 Web UI 响应同样携带。
- **拨号错误上下文**：直连失败错误信息包含目标地址与拨号超时，如 `ssh dial: connect 192.168.0.145:22 (timeout 30s): dial tcp ...`，不再只有裸的 `i/o timeout` / `connectex ...`。

- **`ReadOutput` timeout 修复**：接受 `timeout=0`（非阻塞轮询），下限从 0.1 改为 0。
- **`ssh_config` 写操作双保险**：schema 层面 `action` enum 默认仅 `list`；dispatcher 层面即使客户端绕过 schema 也会拒绝。
- **统一 dispatch 架构**：新增 `group_handlers.go`，4 个 action dispatcher 复用现有 handler。
- **指令精简**：12 条 → 7 条（3,054 B → 1,467 B）。

---

## v0.1.12 — 2026-08-17

### Breaking

- **MCP 工具全面改名**：所有工具改为两级 `<group>_<action>` 命名（如 `session_start`、`shell_send_input`），与 resource model 对齐。旧名称无别名。
- **默认数据目录改为 `~/.termcp`**：优先级 `--data-dir` > `$TERMCP_DATA_DIR` > `~/.termcp`。目录权限收紧为 `0700`（SSH 配置属敏感数据）。

### 新功能

- **断开 ≠ 删除，会话保留只读历史**：会话异常退出后保留为只读 DEAD 磁贴（缓冲与输出不丢，重启后仍可恢复查看）；主动关闭的会话归档后不再残留 DEAD 磁贴。仅显式删除才会清理缓冲与消息文件。
- **会话历史工具**：新增 `list_history` / `get_transcript` / `search_messages` / `rename_session` / `update_session_meta` / `purge_session` / `screenshot` 七个 MCP 工具，及配套 `/api/history` REST 端点，支持检索、重命名、清理历史记录与终端截图。
- **Web UI 平铺工作区**：终端窗口可一键平铺为网格布局（iTerm2 风格），支持单窗格最大化、活动窗格高亮、自动/列/行/双列四种排布策略。
- **平铺/浮动单按钮切换**：切换按钮并入标签栏右侧，与 eye（隐藏全部）、排布策略下拉同行；平铺时按钮蓝色高亮提示当前状态。
- **SSH 连接测试按钮**：新建连接前可直接测试连通性。
- **离开页面保护**：关闭页面前提醒，防止误关正在运行的会话；终端新增"回到最新输出"按钮。

### 改进

- **构建**：新增平台感知 Makefile；默认按 release 模式构建（`-s -w -trimpath`）。
- 精简全部 MCP 工具描述与服务端 instructions（这些内容每轮注入模型上下文，显著降低 token 占用），并修正多处描述的歧义表述。

## v0.1.11 — 2026-08-02

### Breaking

- **删除 admin HTTP API**：移除 `--admin-host` / `--admin-port` / `--admin-token` 开关与独立 admin 端口。SSH 配置管理改为 MCP 工具（用 `--mcp-manage-ssh-configs` 启用）。
- **删除 `ssh-config init` / `ssh-config list` CLI 子命令**（从未出现在 `--help`）。SSH 配置改由 Web UI、`list_ssh_configs` 及 `--mcp-manage-ssh-configs` 的 MCP 工具管理。

### 新功能

- **MCP SSH 配置管理**（`--mcp-manage-ssh-configs`，默认关闭）：
  - `create_ssh_config`：结构化参数创建 remote SSH profile（host/user/password/private_key/jump）。
  - `edit_ssh_config`：增量修补已有 profile，省略字段保持原值（含凭据）。
  - `copy_ssh_config`：服务端复制 profile（含凭据），凭据不会经过 AI。
  - `delete_ssh_config`：按名称删除 profile。
  - 凭据写入后不可读取，日志不记录敏感字段。
- **WebUI 面板折叠**：Entries/Sessions 面板可折叠，状态持久化到 localStorage。
- **连接删除确认弹窗**：样式与整体 UI 统一。

### 修复

- 修正用户可见字符串中的配置文件名。

### 文档

- 新增 Docker 部署文档；README 全面重写；新增演示视频。

## v0.1.10 — 2026-07-20

### 修复

- **会话级联清理**：会话退出时级联回收子 shell 与转发资源，并对齐 shell API 行为。

## v0.1.9 — 2026-07-15

### 修复

- **转发生命周期**：端口转发创建时与会话绑定，UI 入口统一封装，不再出现孤儿转发。

## v0.1.8 — 2026-07-14

### 文档

- **Web UI `api.html`**：首页标题栏新增 **API / MCP** 入口；精简为 MCP 可复制配置 + HTTP/WebSocket API 速查表，含 `claude mcp add --transport http`、通用 `mcpServers` JSON 与 URL-only 说明；`/sse` 与 `/stream` 对等说明同步补齐（README / README.zh / `docs/mcp-tools.md` / `docs/api.md` / architecture）。

## v0.1.7 — 2026-07-11

### Breaking（MCP 重设计）

- **Session / Shell 双 ID**：`start_session` 返回互不相同的 `session_id`（连接容器）与 `shell_id`（终端通道）。首个 shell 不再与 session 共用 id。
- **I/O 只认 `shell_id`**：`send_input` / `press_key` / `read_output` / `resize_pty` / `register_reader` / `unregister_reader` / `close_shell` 参数改为 `shell_id`。连接级操作（forwards、files、terminate、delete、start_subshell）仍用 `session_id`。
- **删除工具**：`send_and_read`、`background_send`、`delete_session`（硬切换，无别名）。
- **删除参数**：`send_input.press_enter`。执行命令改为 `send_input` + `press_key(key="enter")`。
- **新增 `press_key`**：命名按键白名单（enter/tab/esc/方向键/backspace/delete/home/end/ctrl+c|d|z|l|u|w），可选 `repeat`。
- **转发 OpenSSH 命名**：`local_forward` = ssh `-L`；新增 `remote_forward` = ssh `-R`；删除 `forward_port`。旧版 `local_forward` 曾错误实现为 `-R`，现已纠正。
- **`start_subshell` / `list_subshells`**：参数 `parent_session_id` → `session_id`；返回字段对齐 `shell_id` / `shells`。
- **去掉恒空 `initial_output`**；`read_output` 默认 `timeout` 从 5 改为 3。
- **生命周期叙事**：MCP 只保留 `terminate_session`；`force=true` 立即强杀，HTTP `DELETE /api/sessions/{id}` 仍保留。

### 修复

- **`read_output.max_lines` 丢数据**：行数限制改为 buffer 层按换行截断游标；未返回行保留且 `has_more=true`（原先先消费再字符串截断）。
- **internal `close_shell` 误拆会话**：子 shell 主动关闭设 `deliberateClose`，退出 watcher 不再把 intentional channel close 当 SSH 断连。

### 文档

- 重写 MCP `instructions`、`docs/mcp-tools.md`、CLAUDE multi-session 规则；对齐 resource-model。

## v0.1.6 — 2026-07-11

### 新功能

- **SSH profile 改名**：配置文件同步迁移，引用不断链。
- **internal profile 虚拟化**：内置 loopback 连接不再落盘，且禁止编辑/删除。
- **`--no-internal` 开关**：禁用内建 loopback SSH profile。

## v0.1.5 — 2026-07-10

### 修复

- 密码框眼睛按钮纵向居中；密码/私钥字段自动 trim，防止首尾空格干扰认证。

## v0.1.4 — 2026-07-10

### 修复

- **`go install` 后 Web UI 资源缺失**：`vendor` 目录重命名为 `static`，避免 Go module zip 默认排除 `vendor` 路径导致 xterm.js 等静态资源丢失。

## v0.1.3 — 2026-07-09

### 修复

- 新建连接弹窗中 Key Passphrase 字段随 Auth 方式切换显隐，仅在 Private Key 时显示。

## v0.1.2 — 2026-07-03

### 新功能

- **SFTP 文件工具套件**：新增 10 个文件管理 MCP 工具（读写、列目录、删除、重命名、建目录等），统一走 SSH 路径。

### 改进

- **REST API 统一**：清理冗余端点，统一资源式设计。
- **转发/子 shell 变更实时推送**：创建与删除后通过 WebSocket 自动刷新 UI，无需手动刷新。
- Shell 平等化与会话层重构（sync.Map、SSH 断线检测）。

### 修复

- **exited session panic**：`SendTerminalBytes` 增加 nil 检查，会话退出后写入不再崩溃。

## v0.1.1 — 2026-07-01

### 新功能

- **SOCKS5 代理与跳板链**：SSH 配置支持 SOCKS5 代理与 ProxyJump 多级跳板。

### 修复

- **Enter 按目标系统发送**：换行符按目标 shell 所属平台（Windows CRLF / Unix LF）而非 termcp 本机 OS 决定，修复从 Windows 管理 Unix 主机时的输入异常。

## v0.1.0 — 2026-06-30

### 新功能

- **单会话多 shell 通道**：SSH shell 通道多路复用（统一 ChildShell 模型），一个会话内可开多个终端通道。
- **`close_shell` / `list_subshells` MCP 工具**：父子 shell 生命周期拆分，支持显式关闭单个 shell 与列出全部子 shell。
- **MCP 文件工具回归**：重新引入 SFTP 文件读写工具。

### 改进

- internal SSH 支持 SFTP subsystem；SSH 配置迁移至 TOML；包结构与命名重构、清理死代码。

### 修复

- **root shell 自然退出不再带垮 internal session**：主 shell 退出时正确区分连接关闭与整体断开。
- WebUI 连接加载指示器纵向居中，清理启动日志。

### 文档

- 文档与代码同步（31 个 MCP 工具清单、内存 SSH 架构说明）。

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

## v0.0.3 — 2026-05-12

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

- **SSH 库替换**：`gliderlabs/ssh` → `charmbracelet/ssh`。charmbracelet/ssh 内置 `AllocatePty()` 自动管理 PTY 生命周期，移除手工 `pty.StartWithSize`/`io.Copy`/`pty.Setsize` 代码。公共 API（`New`/`Start`/`Stop`/`Addr` 等）签名不变。

- **跨平台输入处理**：`SendInput` 在 `press_enter=true` 时自动选择平台换行符 — Windows 用 CRLF（`\r\n`），Unix 用 LF（`\n`）。

- **SFTP 路径规范化**：远程路径自动将反斜杠转为正斜杠，避免 Windows 路径分隔符导致 SSH 文件操作失败。

- **信号转发验证**：新增 `TestServer_SignalTerm` 和 `TestServer_SignalInterrupt` 验证 SIGTERM/SIGINT 通过 SSH 通道正确转发至目标进程。

### 修复

- **Windows PTY 交互输出为空**：ARM64 上 PowerShell/ConPTY 输出时序问题导致首次读取为空。改为 `Write-Output` 原生命令配合 marker 轮询读取（`testReadOutputUntil`）解决。

- **Windows 进程退出行为不确定**：PowerShell `-Command` 在 ConPTY 下完成命令后保持交互态，自然退出测试在 Windows 跳过。

- **Windows 跳过 POSIX signal 测试**：`SIGTERM`/`SIGINT` 测试在 Windows 跳过，Windows 不支持 POSIX 信号。

## v0.0.1 — 2026-05-05

### 初始发布

- termcp 首个版本：把交互式程序作为持久 SSH 会话暴露给 AI Agent 的 MCP server。
