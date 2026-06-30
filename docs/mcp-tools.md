# MCP 工具参考

termcp 暴露 31 个 MCP 工具，通过 SSE（`/sse`）或 Streamable HTTP（`/stream`）访问。

---

## 会话生命周期

```
start_session  →  [send_input / background_send / read_output / send_and_read ...]
               →  terminate_session  →  delete_session
               ↘  register_reader / unregister_reader（多 Agent 共读）

start_subshell →  [send_input / read_output / ...]（复用父会话 SSH 连接）
               →  close_shell（仅关闭该子通道，父会话保持）
               ↘  list_subshells（枚举某父会话的所有子通道）
```

---

## 工具清单

### start_session

启动一个长时交互进程。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `command` | string | 否 | — | 要执行的命令；空 = 登录 shell 或 profile `default_shell` |
| `args` | string[] | 否 | `[]` | 命令行参数，仅 `command` 非空时有效 |
| `mode` | string | 否 | `"pty"` | `"pty"` 或 `"pipe"` |
| `name` | string | 否 | ssh_config | 会话显示名称 |
| `rows` | number | 否 | `24` | 初始 PTY 行数（1–1000） |
| `cols` | number | 否 | `80` | 初始 PTY 列数（1–1000） |
| `ssh_config` | string | 否 | `"internal"` | profile 名称：`"internal"` = 本机 loopback，其他 = `ssh_configs/<name>/` 下的远端连接 |

**返回**：`{ session_id, pid, ssh_config, initial_output }`（`initial_output` 为空，用 `read_output` 读终端文本）

### start_subshell

在已有 SSH 连接上打开新的 shell 通道。复用父会话的 SSH 传输——不新建 TCP 连接、不重新握手。返回的子 shell ID 可像普通 session_id 一样用于 `send_input`/`read_output` 等。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `parent_session_id` | string | **是** | — | 父会话 ID（必须是远端 SSH 会话） |
| `name` | string | 否 | — | 子通道显示名 |
| `command` | string | 否 | — | 可执行文件；空 = 登录 shell |
| `mode` | string | 否 | `"pty"` | `"pty"` 或 `"pipe"` |
| `rows` | number | 否 | `24` | PTY 行数 |
| `cols` | number | 否 | `80` | PTY 列数 |

### list_subshells

列出共享某父会话 SSH 连接的运行中子 shell（通道）。返回 `parent_session_id` 和 `subshells`（每项含 id、name、status、时间戳）。返回的 id 可用于 `send_input`/`read_output`/`close_shell`。`list_sessions` 只返回父会话——枚举通道用本工具。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `parent_session_id` | string | **是** | 来自 `start_session` / `list_sessions` |

### close_shell

只关闭一个 shell 通道，不拆父会话。传父 `session_id` 关闭其根 shell 通道（仅远端——internal 会话 no-op，进程继续运行）；传子 shell id（来自 `list_subshells`）只关该通道。SSH 连接和其他 shell 继续运行。要彻底停止会话用 `terminate_session`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | 父会话 id（关根通道）或子 shell id（关该通道） |

### send_input

向会话 stdin 写入文本，不读回输出。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `text` | string | **是** | — | 原始文本 |
| `press_enter` | boolean | 否 | `false` | true = 追加 `\n` |

### read_output

读取指定 reader 上次读取后的新输出。每个 reader 持有独立游标，多 Agent 互不干扰。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `strip_ansi` | boolean | 否 | `true` | 是否剥离 ANSI 转义码 |
| `timeout` | number | 否 | `5` | 阻塞等待秒数（0.1–60） |
| `max_lines` | number | 否 | `0` | 截断行数；0 = 无限制 |
| `max_bytes` | number | 否 | `8192` | 单次返回最大字节数；0 = 无限制。配合 `has_more` 分页 |
| `reader_id` | number | 否 | `0` | reader id（0 = 默认，与 WebUI 共享） |

**返回**：`{ output, has_more, lines_returned, bytes_returned }`

### send_and_read

`send_input` + `read_output` 原子组合。**注意**：慢命令会让 MCP 调用阻塞整个 timeout，不建议用于长时间构建。优先用 `background_send` + `read_output(timeout≤3)`。

参数为 `send_input` 和 `read_output` 的合集（不含 `max_bytes`）。

### background_send

fire-and-forget 版的 `send_input`，立即返回。配合 `read_output` 轮询适用于长时间任务。

| 参数 | 与 `send_input` 相同 |

### list_sessions

列出注册表中所有父会话（运行 + 已退出未删除）。子 shell 不包含——用 `list_subshells`。无参数。

**返回**：`{ sessions: [{id, name, status, ssh_config, ssh_endpoint, ...}] }`

### get_session_info

获取单个会话详细信息。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

**返回**：`{ id, name, command, args, mode, rows, cols, status, exit_code, pid, ssh_config, remote_user, remote_host, created_at }`

### terminate_session

终止进程（SIGTERM → 等待 grace_period → SIGKILL）并关闭 SSH 连接。会话退出后自动从注册表移除。只关一个通道用 `close_shell`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `force` | boolean | 否 | `false` | true = 跳过 grace_period 直接强杀 |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |

### delete_session

从注册表移除已退出会话。会话退出后自动移除，故通常不需要；仅在丢弃仍注册的会话时使用。**必须先 `terminate_session`**，否则会报错。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

### resize_pty

调整 PTY 行列数（仅 PTY 模式；会传播到远端 SSH）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `rows` | number | 否 | `24` | 新行数 |
| `cols` | number | 否 | `80` | 新列数 |

### register_reader

为会话注册独立 reader，返回新的 `reader_id`。新 reader 游标起点 = 当前 master 末尾（**无历史 backlog**）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

**返回**：`{ reader_id }`

### unregister_reader

释放 reader。用完后务必注销，避免资源泄漏。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `reader_id` | number | **是** | 非零 reader id（来自 `register_reader`） |

---

## 服务端发现

### detect_shell

探测 termcp **宿主机**（不是 ssh_config 目标）的可用交互 shell。

无参数。

**返回**：`{ path, family, hint }`
- `family`：`"unix"` | `"powershell"` | `"cmd"` | `"unknown"`
- `hint`：人可读提示（如 "found bash" 或 "no shell found"）

**跨平台行为**：
| 平台 | 探测顺序 | 返回示例 |
|------|----------|----------|
| macOS/Linux | bash → zsh → fish → sh | `{"/bin/bash", "unix", "found /bin/bash"}` |
| Windows | pwsh → powershell → cmd | `{"C:\\Program Files\\PowerShell\\7\\pwsh.exe", "powershell", "found pwsh"}` |
| 全部失败 | — | `{"", "unknown", "no interactive shell found; try cmd.exe or /bin/sh manually"}` |

### list_ssh_configs

返回可用的 profile 名称列表（不含密码/host/完整 JSON）。每项对应 `data-dir/ssh_configs/` 下一个目录，加上内置 `"internal"` profile。

无参数。

**返回**：`{ configs: ["internal", "my-server", ...] }`

---

## 端口转发

所有转发工具基于 SSH 通道（`ssh -L` / `-R` / `-D` 语义），复用 `start_session` 建立的 SSH 连接。

### forward_port

本地端口转发（`ssh -L`）。termcp 监听本地端口，通过 SSH 隧道把流量转发到远端目标。`local_port=0` 随机选空闲端口。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | 来自 `start_session` |
| `remote_host` | string | 否 | `"localhost"` | 目标主机（相对远端） |
| `remote_port` | number | **是** | — | 远端目标端口 |
| `local_port` | number | 否 | `0` | 本地监听端口（0=随机） |

### local_forward

远端端口转发（`ssh -R`）。远端监听一个端口，把流量隧道回 termcp 侧目标。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `local_host` | string | 否 | `"0.0.0.0"` | 远端监听绑定地址 |
| `local_port` | number | **是** | — | 远端监听端口 |
| `remote_host` | string | **是** | — | 目标主机（相对 termcp） |
| `remote_port` | number | **是** | — | 目标端口（相对 termcp） |

### dynamic_forward

SOCKS5 代理（`ssh -D`）。termcp 监听本地端口，通过 SSH 连接代理 TCP 连接。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `local_port` | number | 否 | `0` | 本地 SOCKS5 监听端口（0=随机） |

### list_forwards

列出所有活动的端口转发（local / remote / dynamic）。无参数。

**返回**：`{ forwards: [{forward_id, direction, listen_addr, target_addr, status}, ...] }`

### close_forward

按 `forward_id` 关闭一个活动端口转发，释放监听器。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `forward_id` | string | **是** | 来自 `list_forwards` |

---

## 文件操作（SFTP）

通过 SSH session 的 SFTP 子系统在远端操作文件。`session_id` 必须是已建立的（远端 SSH 或 internal）会话。

### file_read

读取远端文件或文件片段。`text` 模式返回可读文本（不可打印字节用 `\xHH` 转义）；`hex` 返回十六进制 dump；`file` 写入 termcp 本地文件。省略 `offset`/`length` 读全文件。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `remote_path` | string | **是** | — | 远端文件路径 |
| `offset` | number | 否 | `0` | 起始字节偏移（0-based） |
| `length` | number | 否 | `0` | 读取字节数（0=全部） |
| `mode` | string | 否 | `"text"` | `text` / `hex` / `file` |
| `local_path` | string | 否 | — | `mode=file` 时的本地落盘路径 |

### file_write

写入远端文件。小数据用 inline `data`（text 模式支持 `\xHH`，或 hex 模式）；大文件/二进制用 `local_path` + `local_offset` + `length` 从 termcp 本地文件流式写入。`offset=0` 截断。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `remote_path` | string | **是** | — | |
| `offset` | number | 否 | `0` | 写入起始偏移（0=开头并截断） |
| `data` | string | 否 | — | inline 数据（按 `mode` 编码） |
| `mode` | string | 否 | `"text"` | `text` / `hex` |
| `local_path` | string | 否 | — | termcp 本地源文件路径 |
| `local_offset` | number | 否 | `0` | 本地文件读取起始偏移 |
| `length` | number | 否 | `0` | 从本地文件读取的字节数（0=全部） |

### file_stat

获取远端文件/目录信息。目录返回 `children` 列表。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `remote_path` | string | **是** | |

**返回**：`{ name, size, is_dir, mod_time, children }`

### file_delete

删除远端文件或空目录。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `remote_path` | string | **是** | |

### file_rename

移动或重命名远端文件/目录（同一文件系统内）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `from_path` | string | **是** | 当前路径 |
| `to_path` | string | **是** | 新路径 |

### file_mkdir

在远端创建目录（含父目录）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `remote_path` | string | **是** | 远端目录路径 |

### get_file_urls

获取某会话下远端文件路径的 HTTP 下载/上传 URL。用这些 URL 直接 curl/wget/浏览器访问。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `remote_path` | string | **是** | |

---

## 消息持久化

### list_messages

列出某会话的已存储消息索引。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

**返回**：`{ messages: [{id, type, created_at, byte_size}, ...] }`

### get_message

获取一条或多条消息的完整内容。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `message_ids` | string[] | 否 | 消息 ID 列表（可传 JSON 数组或单个 scalar） |

**返回**：`{ messages: [{id, session_id, type, content, created_at, byte_size}, ...] }`

---

## 多 Agent 协作模式

```
Agent A                          Agent B
────────                         ────────
start_session(...)
  → session_id + reader_id=0

                                 register_reader(session_id)
                                   → reader_id=2 (独立游标，起点=当前 master 末尾)

read_output(session_id, reader_id=0)
  → 看到游标 0 位置

                                 read_output(session_id, reader_id=2)
                                   → 看到游标 2 位置（互不干扰，仅此后新输出）

                                 unregister_reader(session_id, reader_id=2)

terminate_session(session_id)
delete_session(session_id)
```

**规则**：
- 一个 task = 一个 session（或一个 profile + 一个 session）
- `read_output` 的 `timeout ≤ 3`，永不长时间阻塞
- 轮询多 session：对每个 `read_output(timeout=1)`，谁有输出就处理谁
- 完成后 `terminate_session` → `delete_session`，不留僵尸
- 多通道复用同一 SSH 连接用 `start_subshell`，单通道关闭用 `close_shell`
