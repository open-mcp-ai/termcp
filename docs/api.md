# termcp HTTP API

Base URL: `http://localhost:18765`

实时终端 I/O 走 WebSocket (`/api/ui/ws`)，其余操作走 REST。

---

## 1. 连接配置 (Connection Profiles)

### `GET /api/connection-templates`

返回新建连接时的 TOML 模板。

```
Response 200:
{
  "remote":   "# Remote SSH connection\n...",
  "internal": "# Internal loopback\n..."
}
```

### `GET /api/connections`

列出所有连接配置的摘要。

```
Response 200:
{
  "connections": [
    { "name": "pi", "kind": "remote", "host": "192.168.1.100", "user": "pi", "port": 22 }
  ]
}
```

### `GET /api/connections/{name}`

获取单个连接配置的原始 TOML。

### `PUT /api/connections/{name}`

创建或更新连接配置。Body 为 TOML。

```
Response: 204 No Content
```

### `DELETE /api/connections/{name}`

删除连接配置。

```
Response: 204 No Content
```

---

## 2. Session

**概念：** Session = SSH 连接容器，包含 0~N 个 Shell + 0~N 个 Forward。第一个 Shell 的 ID = Session ID。

### `GET /api/sessions`

列出所有活跃 session。

```
Response 200:
{
  "sessions": [
    { "id": "abc123", "name": "pi", "mode": "pty", "status": "running",
      "pid": 12345, "rows": 24, "cols": 80, "ssh_endpoint": "remote", "created_at": "..." }
  ]
}
```

### `POST /api/sessions`

创建新 SSH 连接和 Session（含第一个 Shell）。

```
Request:
{
  "ssh_config": "pi",    // 连接配置名，默认 "internal"
  "command": "",         // 命令，空 = 登录 shell
  "args": [],
  "mode": "pty",         // "pty" | "pipe"
  "name": "my-session",  // 显示名称，默认 = ssh_config
  "rows": 24,
  "cols": 80
}

Response 200:
{ "session_id": "abc123", "pid": 12345, "ssh_config": "pi" }
```

### `GET /api/sessions/{id}`

获取单个 session 详情。

```
Response 200: Session 对象（同列表中的元素）
```

### `DELETE /api/sessions/{id}`

完全断开 Session：关闭所有 Shell → 关闭所有 Forward → 关闭 SSH 连接 → 移除。

```
Response: 204 No Content
```

---

## 3. Shell

Shell 是 Session 下的子资源，ID 全局唯一。

### `GET /api/sessions/{id}/shells`

列出 Session 的所有 Shell。

```
Response 200:
{
  "shells": [
    { "id": "abc123", "name": "pi", "status": "running", ... },
    { "id": "def456", "name": "shell-2", "status": "running", ... }
  ]
}
```

### `POST /api/sessions/{id}/shells`

在已有 Session 上创建新 Shell channel（复用 SSH 连接）。

```
Request:
{ "command": "", "name": "shell-2", "mode": "pty", "rows": 24, "cols": 80 }

Response 200:
{ "session_id": "def456", "parent_session_id": "abc123", "name": "shell-2" }
```

### `DELETE /api/shells/{id}`

关闭指定 Shell channel。不中断 SSH 连接，不影响同 Session 的其他 Shell。

- 传 Session ID → 关闭第一个 Shell
- 传 Shell ID → 关闭该 Shell

```
Response: 204 No Content
```

---

## 4. 终端 I/O

### WebSocket `GET /api/ui/ws`

双向实时通道。

**Client → Server：**

| type | 字段 | 说明 |
|------|------|------|
| `watch_add` | `id` | 订阅终端输出 |
| `watch_remove` | `id` | 取消订阅 |
| `input` | `id`, `d`(base64), `nl` | 发送键盘输入 |
| `resize` | `id`, `rows`, `cols` | PTY 尺寸变更 |

**Server → Client：**

| type | 字段 | 说明 |
|------|------|------|
| `sessions` | `sessions` | Session 列表（连接时 + 变更时） |
| `terminal` | `id`, `d`(base64) | 终端输出块 |
| `terminal_done` | `id` | Shell 已退出 |

> `id` 可以是 Session ID 或任意 Shell ID，Server 自动解析。

### `GET /api/sessions/{id}/output-range`

读取终端输出历史（不推进 reader cursor）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `start` | query | 起始 byte offset (0-based) |
| `max` | query | 最大返回字节，≤512KB，默认 256KB |
| `tail` | query | `1` = 取尾部 max 字节（忽略 start） |

```
Response 200:
{ "start": 0, "end": 1024, "total": 4096, "d": "<base64>" }
```

---

## 5. 端口转发 (Port Forward)

### `GET /api/forwards`

列出所有活跃端口转发。

```
Response 200:
{ "forwards": [{ "forward_id": "L-abc123", "session_id": "abc123", "direction": "local", ... }] }
```

### `GET /api/sessions/{id}/forwards`

列出指定 Session 的端口转发。

### `POST /api/sessions/{id}/forwards`

在指定 Session 上创建端口转发。session_id 来自 URL，无需传 `ssh_config`。

```
Request:
{
  "direction": "local",      // "local" | "remote" | "dynamic"
  "remote_host": "localhost",
  "remote_port": 80,
  "local_host": "0.0.0.0",   // remote 模式必填
  "local_port": 8080          // 0 = 自动分配
}
```

| direction | 必填参数 |
|-----------|---------|
| `local` | `remote_host`, `remote_port` (1-65535) |
| `remote` | `local_host`, `local_port`, `remote_host`, `remote_port` (1-65535) |
| `dynamic` | 无必填 |

```
Response 201: ForwardInfo
```

### `DELETE /api/forwards/{id}`

关闭指定端口转发。

```
Response 200: { "ok": true }
```

---

## 6. 文件操作

所有文件操作通过 Session 的 SFTP 通道（remote）或本地文件系统（internal）。

### `GET /api/sessions/{id}/files`

列出目录内容或获取文件信息。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 路径（必填） |

```
Response 200:
{ "name": "home", "size": 4096, "is_dir": true,
  "children": [{ "name": "file.txt", "size": 1024, "is_dir": false, "mod_time": "..." }] }
```

### `GET /api/sessions/{id}/files/download`

下载文件。支持 HTTP Range 头。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 文件路径 |

```
Response: application/octet-stream（支持 Range/206 Partial Content）
```

### `POST /api/sessions/{id}/files/upload`

上传文件。支持 multipart/form-data 或 raw body。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 目标路径 |
| `offset` | query | 写入起始偏移，默认 0 |
| Content-Range | header | 断点续传 |

```
Response 200: { "bytes_written": 1024 }
```

### `DELETE /api/sessions/{id}/files`

删除文件或空目录。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 路径 |

```
Response 200: { "ok": true }
```

### `PUT /api/sessions/{id}/files`

重命名/移动文件或目录（同文件系统内）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `from` | query | 源路径 |
| `to` | query | 目标路径 |

```
Response 200: { "ok": true }
```

### `POST /api/sessions/{id}/files/dir`

创建目录（含父目录）。

| 参数 | 类型 | 说明 |
|------|------|------|
| `path` | query | 目录路径 |

```
Response 200: { "ok": true }
```

---

## 7. 向后兼容路由

旧路由仍可用，委托到新路由。建议新代码使用上面的规范路径。

| 旧路径 | 规范路径 |
|--------|---------|
| `POST /api/sessions/start` | `POST /api/sessions` |
| `GET /api/sessions/{id}/child-shells` | `GET /api/sessions/{id}/shells` (301) |
| `POST /api/sessions/{id}/terminate` | `DELETE /api/shells/{id}` |
| `POST /api/sessions/{id}/close-shell` | `DELETE /api/shells/{id}` |
| `POST /api/sessions/{id}/disconnect` | `DELETE /api/sessions/{id}` |
| `POST /api/forwards` | `POST /api/sessions/{id}/forwards` |
| `DELETE /api/sessions/{id}/files/delete` | `DELETE /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/rename` | `PUT /api/sessions/{id}/files` |
| `POST /api/sessions/{id}/files/mkdir` | `POST /api/sessions/{id}/files/dir` |
