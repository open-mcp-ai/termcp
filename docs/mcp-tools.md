# MCP 工具参考

termcp 暴露 16 个 MCP 工具，通过 SSE（`/sse`）或 Streamable HTTP（`/stream`）访问。

---

## 会话生命周期

```
start_session  →  [send_input / background_send / read_output / send_and_read ...]
               →  terminate_session  →  delete_session
               ↘  register_reader / unregister_reader（多 Agent 共读）
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
| `reader_id` | number | 否 | `0` | reader id（0 = 默认，与 WebUI 共享） |

**返回**：`{ output, has_more, lines_returned, bytes_returned }`

### send_and_read

`send_input` + `read_output` 原子组合。**注意**：慢命令会让 MCP 调用阻塞整个 timeout，不建议用于长时间构建。优先用 `background_send` + `read_output(timeout≤3)`。

参数为 `send_input` 和 `read_output` 的合集。

### background_send

fire-and-forget 版的 `send_input`，立即返回。配合 `read_output` 轮询适用于长时间任务。

| 参数 | 与 `send_input` 相同 |

### list_sessions

列出注册表中所有会话（运行 + 已退出未删除）。无参数。

**返回**：`{ sessions: [{id, name, status, ssh_config, ...}] }`

### get_session_info

获取单个会话详细信息。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

**返回**：`{ id, name, command, args, mode, rows, cols, status, exit_code, pid, ssh_config, remote_user, remote_host, created_at }`

### terminate_session

终止进程（SIGTERM → 等待 grace_period → SIGKILL）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `force` | boolean | 否 | `false` | true = 跳过 grace_period 直接强杀 |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |

### delete_session

从注册表移除已退出会话。**必须先 `terminate_session`**，否则会报错。

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

为会话注册独立 reader，返回新的 `reader_id`。新 reader 游标起点 = 当前 master 末尾（无历史 backlog）。

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

返回可用的 profile 名称列表（不含密码/host/完整 JSON）。

无参数。

**返回**：`{ configs: ["internal", "my-server", ...] }`

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
                                   → reader_id=2 (独立游标)

read_output(session_id, reader_id=0)
  → 看到游标 0 位置

                                 read_output(session_id, reader_id=2)
                                   → 看到游标 2 位置（互不干扰）

                                 unregister_reader(session_id, reader_id=2)

terminate_session(session_id)
delete_session(session_id)
```

**规则**：
- 一个 task = 一个 session（或一个 profile + 一个 session）
- `read_output` 的 `timeout ≤ 3`，永不长时间阻塞
- 轮询多 session：对每个 `read_output(timeout=1)`，谁有输出就处理谁
- 完成后 `terminate_session` → `delete_session`，不留僵尸
