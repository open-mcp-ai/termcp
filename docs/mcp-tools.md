# MCP 工具参考

termcp 通过 **SSE** 与 **Streamable HTTP** 两套对等传输暴露同一套 MCP 工具（同一监听端口）。

## 对接方式（传输）

| 传输 | 端点 | 配套 | 说明 |
|------|------|------|------|
| SSE | `GET /sse` | `POST /message` | 客户端只配置 `/sse`；SDK 自动用 `/message` 发 JSON-RPC |
| Streamable HTTP | `/stream` | — | 单路径；不要拼 `/sse` 或 `/message` |

- SSE：`http://<host>:18765/sse`（Claude：`--transport sse` / `type: "sse"`）
- Streamable HTTP：`http://<host>:18765/stream`（Claude：`--transport http` / `type: "http"`）

可复制配置与 HTTP API 速查：Web UI **`/api.html`**。完整客户端样例见 `README.md` / `README.zh.md`。
**ID 规则（硬）：**

| 资源 | 参数名 | 谁用 |
|------|--------|------|
| Session（SSH 连接容器） | `session_id` | start_subshell、forwards、files、terminate、list_sessions、get_session_info |
| Shell（终端 channel） | `shell_id` | send_input、press_key、read_output、resize_pty、register/unregister_reader、close_shell |

`start_session` 返回 **两个不同** 的 id：`session_id` 与 `shell_id`（首个 shell 不再与 session 共用 id）。

---

## 会话生命周期

```
list_ssh_configs
  → start_session → { session_id, shell_id, ... }
      → send_input(shell_id, text)          # 只打字，不回车
      → press_key(shell_id, key="enter")    # 只按键
      → read_output(shell_id, timeout≤3)
      → start_subshell(session_id) → { shell_id, session_id }
      → close_shell(shell_id)
      → local_forward / remote_forward / dynamic_forward(session_id, ...)
      → file_*(session_id, ...)
  → terminate_session(session_id)           # 关连接并移除（级联 shell+forward；force=true 强杀）
```

---

## 工具清单

### start_session

启动会话（连接容器）并创建一个主 shell 通道。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `command` | string | 否 | — | 要执行的命令；空 = 登录 shell 或 profile `default_shell` |
| `args` | string[] | 否 | `[]` | 命令行参数，仅 `command` 非空时有效 |
| `mode` | string | 否 | `"pty"` | `"pty"` 或 `"pipe"` |
| `name` | string | 否 | ssh_config | 会话显示名称 |
| `rows` | number | 否 | `24` | 初始 PTY 行数（1–1000） |
| `cols` | number | 否 | `80` | 初始 PTY 列数（1–1000） |
| `ssh_config` | string | 否 | `"internal"` | profile 名称：`"internal"` = 本机 loopback，其他 = `ssh_configs/<name>/` 下的远端连接 |

**返回**：`{ session_id, shell_id, pid, ssh_config }`

### start_subshell

在已有会话连接上打开另一个 shell 通道（复用 SSH 传输）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | 父会话 ID |
| `name` | string | 否 | — | 子通道显示名 |
| `command` | string | 否 | — | 可执行文件；空 = 登录 shell |
| `mode` | string | 否 | `"pty"` | `"pty"` 或 `"pipe"` |
| `rows` | number | 否 | `24` | PTY 行数 |
| `cols` | number | 否 | `80` | PTY 列数 |

**返回**：`{ shell_id, session_id, name }`

### list_subshells

列出某会话上的 shell 通道。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | 来自 `start_session` / `list_sessions` |

**返回**：`{ session_id, shells: [{id, name, status, ...}] }`

### close_shell

按 `shell_id` 关闭一个 shell 通道，不拆会话连接。internal 主 shell 关闭为 no-op（进程可存活于 tab 之外）。彻底停止会话用 `terminate_session`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | 来自 `start_session` / `start_subshell` / `list_subshells` |

### send_input

向 shell stdin **只写文本**，不按回车、不执行命令。执行一行请再调 `press_key(key="enter")`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `text` | string | **是** | — | UTF-8 文本（不自动追加换行） |

### press_key

向 shell 发送命名按键。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `key` | string | **是** | — | 见下方白名单 |
| `repeat` | number | 否 | `1` | 重复次数（上限约 20） |

**支持的 key：** `enter`, `tab`, `esc`, `up`, `down`, `left`, `right`, `backspace`, `delete`, `home`, `end`, `ctrl+c`, `ctrl+d`, `ctrl+z`, `ctrl+l`, `ctrl+u`, `ctrl+w`。

`enter`：PTY 下为 `\r`；pipe 下按 shell family 为 `\n` 或 `\r\n`。

### read_output

读取指定 reader 上次读取后的新输出。每个 reader 持有独立游标。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `strip_ansi` | boolean | 否 | `true` | 是否剥离 ANSI 转义码 |
| `timeout` | number | 否 | `3` | 阻塞等待秒数（0.1–60）；多 shell 轮询建议 ≤3 |
| `max_lines` | number | 否 | `0` | 按换行分页：最多返回 N 行；未返回字节保留在 reader 游标，`has_more` 为 true；0 = 无限制 |
| `max_bytes` | number | 否 | `8192` | 单次返回最大字节数；0 = 无限制。配合 `has_more` 分页 |
| `reader_id` | number | 否 | `0` | reader id（0 = 默认） |

**返回**：`{ output, has_more, lines_returned, bytes_returned, session_status, session_uptime_seconds }`

`max_lines` / `max_bytes` 都在 buffer 层限制游标推进：未返回的数据可继续读，不会被静默丢弃。

### list_sessions

列出注册表中所有父会话。子 shell 不包含——用 `list_subshells`。无参数。

**返回**：`{ sessions: [{id, name, status, ssh_endpoint, ...}] }`

### get_session_info

获取单个会话详细信息。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

### terminate_session

终止并移除会话：关闭全部 shell，并关闭 SSH 连接（级联 forwards）。`force=true` 立即强杀；只关一个通道用 `close_shell`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `force` | boolean | 否 | `false` | true = 跳过 grace_period 直接强杀 |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |


### resize_pty

调整 shell 的 PTY 行列数（会传播到远端 SSH）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `rows` | number | 否 | `24` | 新行数 |
| `cols` | number | 否 | `80` | 新列数 |

### register_reader

为 shell 注册独立 reader，返回新的 `reader_id`。新 reader 游标起点 = 当前缓冲末尾（**无历史 backlog**）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |

**返回**：`{ reader_id }`

### unregister_reader

释放 reader。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |
| `reader_id` | number | **是** | 非零 reader id（来自 `register_reader`） |

---

## 服务端发现

### detect_shell

探测 termcp **宿主机**（不是 ssh_config 目标）的可用交互 shell。无参数。

**返回**：`{ path, family, hint }`

### list_ssh_configs

返回可用的 profile 名称列表（不含密码/host/完整 JSON）。无参数。

**返回**：`{ configs: ["internal", "my-server", ...] }`

---

## 端口转发（OpenSSH 命名）

所有转发基于 SSH 通道，复用 `start_session` 建立的连接。参数均为 `session_id`。

| 工具 | OpenSSH | 语义 |
|------|---------|------|
| `local_forward` | `-L` | termcp 侧监听本地端口，隧道到远端目标 |
| `remote_forward` | `-R` | 远端监听端口，隧道回 termcp 侧目标 |
| `dynamic_forward` | `-D` | 本机 SOCKS5 代理 |

### local_forward（ssh -L）

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `remote_host` | string | 否 | `"localhost"` | 目标主机（相对远端） |
| `remote_port` | number | **是** | — | 远端目标端口 |
| `local_port` | number | 否 | `0` | 本地监听端口（0=随机） |

### remote_forward（ssh -R）

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `local_host` | string | 否 | `"0.0.0.0"` | 远端监听绑定地址 |
| `local_port` | number | **是** | — | 远端监听端口 |
| `remote_host` | string | **是** | — | 目标主机（相对 termcp） |
| `remote_port` | number | **是** | — | 目标端口（相对 termcp） |

### dynamic_forward（ssh -D）

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `local_port` | number | 否 | `0` | 本地 SOCKS5 监听端口（0=随机） |

### list_forwards / close_forward

- `list_forwards`：无参数，返回 `{ forwards: [{forward_id, direction, listen_addr, target_addr, status}, ...] }`
- `close_forward`：`forward_id` 必填

---

## 文件（SFTP，session 级）

`file_*` / `get_file_urls` 一律用 **`session_id`**（连接级，不绑 shell）。

`file_getwd` 返回 **SFTP cwd**，不是交互 shell 的 `pwd`；要 shell 工作目录请在对应 shell 里执行命令。

---

## 已移除（硬切换，无别名）

| 旧工具/参数 | 替代 |
|-------------|------|
| `send_and_read` | `send_input` + `press_key(enter)` + `read_output` |
| `background_send` | `send_input`（本身即立即返回） |
| `press_enter` 参数 | `press_key(key="enter")` |
| `forward_port` | `local_forward`（ssh -L） |
| 旧 `local_forward`（曾错误实现为 -R） | 现为 -L；原 -R 能力见 `remote_forward` |
| I/O 参数名 `session_id` | `shell_id` |
| `start_subshell` 的 `parent_session_id` | `session_id` |
| `initial_output` 返回字段 | 删除；用 `read_output` |
