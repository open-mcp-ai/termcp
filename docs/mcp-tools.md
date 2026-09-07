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
| Session（SSH 连接容器） | `session_id` | shell_open、forward、file_*、session_terminate、session_list、session_info |
| Shell（终端 channel） | `shell_id` | shell_input、shell_key、shell_output、shell_resize、shell_reader_register/unregister、shell_close |

`session_start` 返回 **两个不同** 的 id：`session_id` 与 `shell_id`（首个 shell 不与 session 共用 id）。

---

## 会话生命周期

```
ssh_config(action=list)
  → session_start → { session_id, shell_id, ... }
      → shell_input(shell_id, text)         # 只打字，不回车
      → shell_key(shell_id, key="enter")    # 只按键
      → shell_output(shell_id, timeout≤3)
      → shell_open(session_id) → { shell_id, session_id }
      → shell_close(shell_id)
      → forward(session_id, action=local|remote|dynamic, ...)
      → file_*(session_id, ...)
  → session_terminate(session_id)           # 关连接并归档（级联 shell+forward；force=true 强杀）
```

---

## 工具清单

### session_start

启动会话（连接容器）并创建一个主 shell 通道。

> **会话模式选择**：
> - **交互 shell（默认，省略 `command`/`args`）**：用于多步骤任务、带状态的操作（`cd`/环境变量/依赖后续步骤）以及通用 CLI 会话。在同一个会话中持续输入执行，保持工作目录与环境一致，形成连续的审计历史。
> - **专用单次程序（显式传入 `command`/`args`）**：**仅限**以下三种情况使用：
>   1. 交互式专用 REPL 或 TUI 工具（如 `python -i`、`mysql`、`htop`）；
>   2. 长期后台服务或守护进程（如 `npm run dev`、后端服务二进制）；
>   3. 需要进程原生退出码（ExitCode）的独立原子脚本。
> - **反模式**：切勿将多步骤任务拆解为多次 `session_start(command="bash", args=["-c", ...])` 执行。每一步都会丢失环境状态、产生多余 SSH 握手开销并割裂审计历史。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `command` | string | 否 | — | 可执行文件；省略 = 登录 shell（默认）。仅 REPL/服务/独立原子任务需要填写 |
| `args` | string[] | 否 | `[]` | 命令行参数，仅 `command` 非空时有效 |
| `mode` | string | 否 | `"pty"` | `"pty"` 或 `"pipe"` |
| `name` | string | 否 | ssh_config | 会话显示名称 |
| `rows` | number | 否 | `24` | 初始 PTY 行数（1–1000） |
| `cols` | number | 否 | `80` | 初始 PTY 列数（1–1000） |
| `ssh_config` | string | 否 | `"internal"` | profile 名称：`"internal"` = 本机 loopback，其他 = `ssh_configs/<name>/` 下的远端连接 |

**返回**：`{ session_id, shell_id, pid, ssh_config }`

### shell_open

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

### shell_list

列出某会话上的 shell 通道。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | 来自 `session_start` / `session_list` |

**返回**：`{ session_id, shells: [{id, name, status, ...}] }`

### shell_close

按 `shell_id` **删除**一个 shell 通道（手动关闭 = 删除，不是 DEAD；不会留下死态 tab）。不拆会话连接，不影响同会话其它 shell。internal 主 shell 关闭为 no-op（进程可存活于 tab 之外）。pipe 会话的最后一个 shell 被关闭时，容器转为 `exited`（DEAD）；PTY 容器保持 `running` 可再新建 shell。彻底停止会话用 `session_terminate`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | 来自 `session_start` / `shell_open` / `shell_list` |

### shell_input

向 shell stdin **只写文本**，不按回车、不执行命令。执行一行请再调 `shell_key(key="enter")`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `text` | string | **是** | — | UTF-8 文本（不自动追加换行） |

### shell_key

向 shell 发送命名按键。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `key` | string | **是** | — | 见下方白名单 |
| `repeat` | number | 否 | `1` | 重复次数（上限约 20） |

**支持的 key：** `enter`, `tab`, `esc`, `up`, `down`, `left`, `right`, `backspace`, `delete`, `home`, `end`, `ctrl+c`, `ctrl+d`, `ctrl+z`, `ctrl+l`, `ctrl+u`, `ctrl+w`。

`enter`：PTY 下为 `\r`；pipe 下按 shell family 为 `\n` 或 `\r\n`。

### shell_output

**统一输出读取工具**：活会话（内存缓冲）、已退出会话（保留缓冲）、归档会话（磁盘消息流）全部用同一套字节流游标语义读取。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | shell_id 或 session_id 均可；归档会话也可用 shell_id 定位单个 shell 的输出流 |
| `strip_ansi` | boolean | 否 | `true` | 是否剥离 ANSI 转义码并压缩终端噪音 |
| `timeout` | number | 否 | `3` | 仅活会话：阻塞等待秒数（0–60）；0 = 非阻塞；多 shell 轮询建议 ≤3 |
| `offset` | number | 否 | `-1` | 无状态字节游标：从该原始字节位置向后读；-1 = 默认模式（见下） |
| `tail_lines` | number | 否 | `0` | 只返回流末尾最后 N 行（优先于 offset）；0 = 关闭 |
| `max_lines` | number | 否 | `0` | 最多返回 N 个完整行（窗口内裁切）；0 = 无限制 |
| `max_bytes` | number | 否 | `8192` | 单次返回最大原始字节数；0 = 无限制 |
| `reader_id` | number | 否 | `0` | 仅活会话流式游标；归档会话不支持 |

**读取模式（三选一）**：

1. **流式游标（默认，活会话）**：返回 reader 上次读取后的新输出，游标前移、不重复。`shell_input → shell_key(enter) → shell_output` 轮询循环的原有语义，完全兼容。
2. **`offset >= 0`（无状态绝对定位）**：读字节区间 `[offset, offset+max_bytes)`。任意时刻从头/任意位置翻页；每次调用显式传 `offset`（用返回的 `end_offset` 续读），服务器不保存状态，活会话与归档会话一视同仁。
3. **`tail_lines > 0`（末尾截取）**：反向取流末尾最后 N 行——只读最近输出，绝不拖入整段历史（token 友好）。无 `offset`/`tail_lines` 且目标是归档/死亡会话时，默认也取末尾最近一块（≤8 KiB），不会全量导出。

**返回**：`{ output, has_more, lines_returned, bytes_returned, start_offset, end_offset, total_bytes, source, session_id, shell_id, session_status, session_uptime_seconds? }`

- `start_offset` / `end_offset`：本次返回的原始字节区间；`total_bytes`：流总长；`has_more = end_offset < total_bytes`。
- `source`：`"live"`（内存缓冲）或 `"persisted"`（磁盘消息流）。
- 活会话流式读（模式 1）时 `start_offset`/`end_offset` 反映 reader 游标位置。

> 例：只读归档会话最后 10 行 → `shell_output(shell_id=归档session_id, tail_lines=10)`；从头翻页 → `shell_output(shell_id, offset=0, max_bytes=8000)` 后用 `end_offset` 续读。

### session_list

列出注册表中所有父会话。子 shell 不包含——用 `shell_list`。无参数。

**返回**：`{ sessions: [{id, name, status, ssh_endpoint, ...}] }`

### session_info

获取单个会话详细信息。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |

### session_terminate

终止并移除会话：关闭全部 shell，并关闭 SSH 连接（级联 forwards）。`force=true` 立即强杀；只关一个通道用 `shell_close`。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `session_id` | string | **是** | — | |
| `force` | boolean | 否 | `false` | true = 跳过 grace_period 直接强杀 |
| `grace_period` | number | 否 | `5` | SIGTERM 后等待秒数（0–60） |


### shell_resize

调整 shell 的 PTY 行列数（会传播到远端 SSH）。

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `shell_id` | string | **是** | — | |
| `rows` | number | 否 | `24` | 新行数 |
| `cols` | number | 否 | `80` | 新列数 |

### shell_reader_register

为 shell 注册独立 reader，返回新的 `reader_id`。新 reader 游标起点 = 当前缓冲末尾（**无历史 backlog**）。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |

**返回**：`{ reader_id }`

### shell_reader_unregister

释放 reader。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `shell_id` | string | **是** | |
| `reader_id` | number | **是** | 非零 reader id（来自 `shell_reader_register`） |

---

## 服务端发现与配置

### shell_detect

探测 termcp **宿主机**（不是 ssh_config 目标）的可用交互 shell。无参数。

**返回**：`{ path, family, hint }`

### ssh_config（统一入口）

SSH 连接 profile 管理。默认只暴露 `action=list`；write actions 需启动 termcp 时加 `--mcp-manage-ssh-configs`。

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `action` | string | **是** | `list` / `create` / `edit` / `copy` / `delete`（后四者需 flag） |
| `name` | string | 条件 | create/edit/delete：profile 名（`[A-Za-z0-9_-]`，最长 64） |
| `host` / `user` | string | 条件 | create 必填；edit 可选（仅更新传入字段） |
| `port` | number | 否 | 默认 22 |
| `password` / `private_key` / `key_passphrase` | string | 条件 | create 二选一；edit 省略保持原值 |
| `trust_unknown_host` | bool | 否 | 默认 false |
| `known_hosts` | string | 否 | 内容或路径 |
| `dial_timeout_seconds` | number | 否 | 默认 30 |
| `proxy` | string | 否 | SOCKS5 代理 URL |
| `description` / `default_shell` / `default_mode` | string | 否 | 会话默认值 |
| `jump_*` | — | 否 | 单层 bastion（ProxyJump）：`jump_host` / `jump_user` / `jump_port` / `jump_password` / `jump_private_key` / `jump_key_passphrase` / `jump_trust_unknown_host` / `jump_known_hosts` / `jump_dial_timeout_seconds` / `jump_proxy` |
| `source_name` / `target_name` | string | 条件 | copy：源与目标（目标须不存在） |

**注意**：password/private_key/key_passphrase/proxy 凭据**写入后不可读取**；不要在聊天中回显。write actions 未启用时调用返回错误提示启动 flag。

---

## 端口转发：forward（统一入口）

所有转发基于 SSH 通道，复用 `session_start` 建立的连接，参数均为 `session_id`。`action` 对应 OpenSSH 语义：

| action | OpenSSH | 语义 |
|--------|---------|------|
| `local` | `-L` | termcp 侧监听本地端口，隧道到远端目标 |
| `remote` | `-R` | 远端监听端口，隧道回 termcp 侧目标 |
| `dynamic` | `-D` | 本机 SOCKS5 代理 |
| `list` | — | 列出全部转发 |
| `close` | — | 按 `forward_id` 关闭 |

| 参数 | 类型 | 必填 | 默认 | 说明 |
|------|------|------|------|------|
| `action` | string | **是** | — | `local` / `remote` / `dynamic` / `list` / `close` |
| `session_id` | string | 条件 | — | local/remote/dynamic 必填 |
| `remote_host` | string | 否 | `"localhost"` | local：目标主机（相对远端） |
| `remote_port` | number | 条件 | — | local：远端目标端口；remote：termcp 侧目标端口 |
| `local_port` | number | 条件 | `0` | local/dynamic：本地监听端口（0=随机）；remote：远端监听端口（必填） |
| `local_host` | string | 否 | `"0.0.0.0"` | remote：远端监听绑定地址 |
| `forward_id` | string | 条件 | — | close：来自 `action=list` |

**返回**：local/dynamic → `{ local_port, forward_id }`；remote → `{ remote_port, forward_id }`；list → `{ forwards: [...] }`；close → `{ "success": true }`

---

## 文件（SFTP，session 级）

`file_*` 一律用 **`session_id`**（连接级，不绑 shell）。

### 高频独立入口

| 工具 | 说明 |
|------|------|
| `file_read` | 读文件（text/hex 或下载到 termcp 主机） |
| `file_write` | 写文件（内联数据或从 host 文件流式写入） |
| `file_stat` | 文件/目录元信息（size、is_dir、children） |
| `file_delete` | 删除文件或空目录 |
| `file_rename` | 移动/重命名 |
| `file_mkdir` | 创建目录（含父目录） |
| `file_urls` | 获取 HTTP 下载/上传 URL |
| `file_getwd` | SFTP 工作目录（不是 shell 的 `pwd`） |

### 低频操作：分组入口（action 枚举）

低频文件操作合并为 3 个入口，用 `action` 参数区分具体操作，避免每个操作一个工具占用模型上下文。

**file_perm** — 权限/属主/时间戳

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `chmod` / `chown` / `chtimes` |
| `remote_path` | string | **是** | |
| `mode` | number | 条件 | chmod：十进制 Unix 权限（493 = 0755） |
| `uid` / `gid` | number | 条件 | chown：数字 uid/gid |
| `atime` / `mtime` | number | 条件 | chtimes：Unix 秒 |

**file_link** — 符号链接/硬链接

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `readlink` / `symlink` / `link` |
| `remote_path` | string | 条件 | readlink：要读取的链接路径 |
| `target` / `link_path` | string | 条件 | symlink：目标 + 新链接路径（`ln -s target link_path`） |
| `existing_path` / `new_path` | string | 条件 | link：已有文件 + 新硬链接路径 |

**file_fs** — 路径/文件系统

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `session_id` | string | **是** | |
| `action` | string | **是** | `truncate` / `realpath` / `statvfs` |
| `remote_path` | string | **是** | |
| `size` | number | 条件 | truncate：新字节数 |

---

## 已移除（硬切换，无别名）

| 旧工具/参数 | 替代 |
|-------------|------|
| `send_and_read` | `shell_input` + `shell_key(enter)` + `shell_output` |
| `background_send` | `shell_input`（本身即立即返回） |
| `press_enter` 参数 | `shell_key(key="enter")` |
| `forward_port` | `forward(action=local)`（ssh -L） |
| 旧 `local_forward`（曾错误实现为 -R） | 现为 `forward(action=local)`；原 -R 能力见 `forward(action=remote)` |
| I/O 参数名 `session_id` | `shell_id`（shell_* 工具） |
| `shell_open` 的 `parent_session_id` | `session_id` |
| `initial_output` 返回字段 | 删除；用 `shell_output` |
| `file_chmod` / `file_chown` / `file_chtimes` | `file_perm(action=chmod\|chown\|chtimes)` |
| `file_readlink` / `file_symlink` / `file_link` | `file_link(action=readlink\|symlink\|link)` |
| `file_truncate` / `file_realpath` / `file_statvfs` | `file_fs(action=truncate\|realpath\|statvfs)` |
| `local_forward` / `remote_forward` / `dynamic_forward` / `list_forwards` / `close_forward` | `forward(action=local\|remote\|dynamic\|list\|close)` |
| `message_list` / `message_get` | `message(action=list\|get)` |
| `history_list` / `history_search_messages` / `history_rename_session` / `history_update_session_meta` / `history_purge` / `history_screenshot` | `history(action=list\|search_messages\|rename_session\|update_session_meta\|purge\|screenshot)`；归档输出读取改由 `shell_output` 承担（`tail_lines`/`offset`） |
| `history(action=get_transcript)` | 删除；归档/死亡会话输出改用 `shell_output(shell_id=归档session_id或shell_id, tail_lines=N / offset)`，与活会话同一套游标语义 |
| `ssh_config_list` / `ssh_config_create` / `ssh_config_edit` / `ssh_config_copy` / `ssh_config_delete` | `ssh_config(action=list\|create\|edit\|copy\|delete)` |
