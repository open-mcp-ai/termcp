# 资源模型与生命周期

## 资源层级

```
SSH 连接
  └── Session（会话容器）
        ├── Shell[]（终端 channel，0..N 个，平等，无根/子区分）
        ├── Forward[]（端口转发 tunnel，0..N 个）
        └── File（SFTP 客户端，Session 级别复用）
```

## Session

**定义**：SSH 连接的管理容器。持有 SSH Client，维护下属 Shell 和 Forward 列表，不负责 I/O。

**创建**：`start_session` → 建立 SSH 连接 → 创建 Session 实例。

**销毁时机**：
- 用户显式 `terminate_session` / `delete_session` / `disconnect`
- SSH 连接意外断开（被动检测，见下文）

**销毁行为**：级联关闭所有 Shell → 关闭所有 Forward → 关闭 SSH Client → 从 registry 移除。

**状态**：
- `running`：SSH 连接存活
- `exited`：SSH 连接已断开，所有资源已释放

## Shell

**定义**：SSH 连接上的一个 terminal channel。所有 Shell 无论何时创建都是同级的，代码中不存在 "根 shell" 或 "主 shell" 的概念。

**创建**：
- `start_session`：建 Session 时同时创建第一个 Shell（由参数 command/args 决定具体行为）
- `start_subshell`：在已有 Session 上创建新 Shell

**I/O**：所有输入输出通过 Shell ID 寻址。`send_input`、`read_output` 的目标都是 Shell。

**销毁时机**：
- 进程自然退出（exit 命令或命令执行完毕）
- 用户显式 `close_shell`
- 所属 Session 终止时级联关闭

**销毁行为**：关闭 channel → 从 Session 的 Shell 列表移除 → 推送 UI 更新。

## Forward

**定义**：基于 SSH 连接的端口转发 tunnel。属于 Session，不绑定特定 Shell。

**创建/销毁**：通过 `forward_port` / `close_forward` 等 MCP 工具操作。

**级联清理**：Session 终止（显式或 SSH 断开）时，Session 持有的所有 Forward 一并关闭。

## File（SFTP）

**定义**：SSH 连接上的 SFTP 客户端。Session 级别复用——首次文件操作时打开，后续操作共用，Session 终止时关闭。属于 Session 持有的持久资源。

**当前状态**：每次文件操作临时 `sftp.NewClient` + `defer Close`，不复用。

## SSH 断线检测

**场景**：SSH 连接因网络故障、服务端超时等原因意外断开。

**当前状态**：无检测机制。SSH 断开后 Session 残留为僵尸，Shell 和 Forward 不释放。

**检测策略**：

Session 持有多种 SSH channel 类型：
- Shell channel（每个 Shell 一个）
- Forward tunnel（每个转发一个）
- SFTP channel（Session 级别，按需开启）

任意 channel 的 `Done()` 事件都能感知到其自身关闭。但单个 Shell 退出不能判定整个 SSH 断开。

**方案**：Session 创建时注册对 SSH Client 底层连接的监控。具体实现：
- Go 的 `ssh.Client` 未导出 `Wait()` 方法，需通过以下方式之一感知：
  1. 开一个专用的 watchdog SSH session，监控其 `Done()` 事件
  2. 用 `ssh.Client.Wait()`（如果 crypto/ssh 版本支持）
  3. 当所有 Shell channel 全部退出 + Forward 全部关闭时，用 `NewSession()` 试探 SSH 是否存活

具体实现方案待确定后写入实现文档。

断开后的清理流程等同于 Session 显式终止：关闭所有 Shell → 关闭所有 Forward → 关闭 SSH Client → 标记 Exited → 从 registry 移除 → 推送 UI 更新。

## 推送机制

### 后端
所有影响 UI 状态的变更统一通过 `notifyListChange()` 推送（WebSocket / SSE）：
- Session 创建/终止
- Shell 创建/关闭/自然退出
- Forward 创建/关闭

当前实现：Session 变更通过 `Manager.notifyListChange()`，Forward 变更通过 `ForwardManager.notifyChange()`，Shell 变更通过 `Session.onChildChange()`。三者合一一同推送到 `sessionHub.broadcast()`。

### 前端
WebSocket `{type: "sessions"}` 消息到达时：
- 刷新 Session Grid（`renderSessionGrid`）
- 刷新 Forward（`loadForwards`，含 tools panel + shell window FW tab）
- 刷新 Shell Tab（`refreshAllWindowTabs`，重新拉取 `/api/sessions/{id}/child-shells`）

## 与当前代码的差异

| 项目 | 当前代码 | 目标状态 |
|------|---------|---------|
| Session 持有 execSession | 是，`sendInput` 直接写 `s.execSession.Stdin` | 剥离，I/O 全部走 Shell |
| Session 持有 buf | 是，`ReadTerminalStream` 读 `s.buf` | 剥离 |
| primaryShell 字段 | 存在，多处引用 | 删除，所有 Shell 平等存储在 `childShells` map |
| rootShell / 根 shell 概念 | 多处特殊处理 | 删除，无此概念 |
| SSH 断线检测 | 无 | 待实现 |
| Shell 自然退出清理 | 不完善 | Shell 退出→从 map 移除→推送 |
| Forward 级联清理 | 有（`CloseBySession`） | 保持 |
