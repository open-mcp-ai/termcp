# termcp 终端进程通信流程架构

## 一、整体分层

```
┌──────────────────────────────────────────────────────────────┐
│                      AI Agent (MCP Client)                    │
│     SSE (/sse + /message)  或  Streamable HTTP (/stream)      │
│                     同一工具面 / JSON-RPC                      │
└──────────────────────────────────────────────────────────────┘
                               │
    ┌──────────────────────────┼──────────────────────────┐
    │               internal/mcp/ (server.go)               │
    │                                                       │
    │  工具: session_start / shell_open / shell_close, │
    │  shell_input / shell_key / shell_output, │
    │  session_list / session_info / session_terminate, │
    │  shell_resize / shell_reader_register / shell_reader_unregister, │
    │  forward(action=local/remote/dynamic/list/close), │
    │  file_read / file_write / file_stat / file_delete / file_rename / file_mkdir / file_urls, │
    │  shell_detect / ssh_config(action=list) / message(action=list/get) / history(action=...) │
    │                                                       │
    │  logging.go: 每个 handler 包装结构化日志 (耗时/错误)    │
    └──────┬───────────────────────────────┬────────────────┘
           │ session.Manager (注册表)       │ message.Manager (持久化)
           ▼                               ▼
    ┌──────────────────────┐    ┌──────────────────────────┐
    │  internal/session/    │    │  internal/message/        │
    │  manager.go          │    │  message.go              │
    │  Create/Get/Delete/  │    │  Append/List/Get          │
    │  Terminate/ListAll   │    │  每 session 独立 mutex     │
    └──────────┬───────────┘    └──────────┬───────────────┘
               │ session.go                │
               │ New/SendInput/ReadOutput  │
               │ Terminate/ResizePty       │
               │ RegisterReader/Upload...  │
               └──────────┬───────────────┘
                          │
          ┌───────────────┼───────────────┐
          ▼               ▼               ▼
   ┌─────────────┐ ┌────────────┐ ┌──────────────┐
   │ sshclient/  │ │ buffer/    │ │ storage/     │
   │ ExecSession │ │ Buffer     │ │ Store        │
   │ ChildShell  │ │ 缓冲区     │ │ 持久化       │
   └──────┬──────┘ └────────────┘ └──────────────┘
          │ SSH (x/crypto/ssh) over in-memory net.Conn
          ▼
   ┌──────────────────────────────────────────┐
   │        internal/sshserver/                │
   │        charmbracelet/ssh Server           │
   │        (in-process, 无 TCP 监听)          │
   │                                          │
   │  ┌─────────────────────────────────┐     │
   │  │ PTY 分支: charmbracelet/ssh     │     │
   │  │   Pty.Start(cmd) (底层 creack/  │     │
   │  │   pty；Windows 走 ConPTY)       │     │
   │  │   io.Copy(pty, session)         │     │
   │  │   WindowChange → pty.Setsize    │     │
   │  ├─────────────────────────────────┤     │
   │  │ Pipe 分支: cmd.Stdin/Out/Err    │     │
   │  │   直接连接 SSH session          │     │
   │  └─────────────────────────────────┘     │
   │                                          │
   │  sftp subsystem → pkg/sftp (file_*)     │
   │  sshSignalToOSSig: TERM→SIGTERM, ...    │
   └──────────────────┬───────────────────────┘
                      │ exec.Command
                      ▼
             ┌─────────────────┐
             │  实际进程        │
             │  bash/zsh/pwsh  │
             │  python/node/.. │
             └─────────────────┘
```

## 二、进程启动流程（session_start）

```
AI Agent                    MCP Server              Session.Manager        sshclient              sshserver              OS
  │                            │                         │                     │                      │                     │
  │  session_start(            │                         │                     │                      │                     │
  │    command="bash",         │                         │                     │                      │                     │
  │    mode="pty",             │                         │                     │                      │                     │
  │    rows=24, cols=80)       │                         │                     │                      │                     │
  │ ─────────────────────────> │                         │                     │                      │                     │
  │                            │  validateStartParams()  │                     │                      │                     │
  │                            │  sessMgr.Create(cfg) ──>│                     │                      │                     │
  │                            │                         │  session.New()      │                      │                     │
  │                            │                         │  sshclient.Start()──>│                     │                     │
  │                            │                         │                     │  in-memory Dial      │                     │
  │                            │                         │                     │  (net.Conn 对，无 TCP)│  Accept() 取得 conn │
  │                            │                         │                     │ ────────────────────>│                     │
  │                            │                         │                     │  client.NewSession() │                     │
  │                            │                         │                     │  StdinPipe()         │ 打开 stdin 通道      │
  │                            │                         │                     │  StdoutPipe()        │ 打开 stdout 通道     │
  │                            │                         │                     │  StderrPipe()        │ 打开 stderr 通道     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │  [if pty]             │                     │
  │                            │                         │                     │  RequestPty(          │                     │
  │                            │                         │                     │    "xterm-256color",  │  PTY 请求 →          │
  │                            │                         │                     │    24, 80, ...)       │  AllocatePty 接收    │
  │                            │                         │                     │ ────────────────────>│                     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │  session.Start(       │  exec channel        │
  │                            │                         │                     │    "bash")            │  请求                │
  │                            │                         │                     │ ────────────────────>│                     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │                      │  [if pty]            │
  │                            │                         │                     │                      │  ppty.Start(cmd)     │
  │                            │                         │                     │                      │  (charmbracelet/ssh │
  │                            │                         │                     │                      │   → creack/pty /     │
  │                            │                         │                     │                      │   ConPTY on Windows) │
  │                            │                         │                     │                      │  ───── ioctl ──────> │ 创建 PTY
  │                            │                         │                     │                      │                     │ fork/exec bash
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │                      │  [if pipe]           │
  │                            │                         │                     │                      │  cmd.Stdin=sess      │
  │                            │                         │                     │                      │  cmd.Stdout=sess     │
  │                            │                         │                     │                      │  cmd.Run()           │
  │                            │                         │                     │                      │ ─── fork/exec ────> │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │  Wait() goroutine     │                     │
  │                            │                         │                     │  ← ExitCode           │                     │
  │                            │                         │                     │                      │                     │
  │                            │                         │  ExecSession 返回     │                     │                     │
  │                            │                         │  Buffer 创建           │                     │                     │
  │                            │                         │  startReaders()       │                     │                     │
  │                            │                         │  pipeToBuffer(stdout)  │                     │                     │
  │                            │                         │  pipeToBuffer(stderr)  │                     │                     │
  │                            │                         │  ← 返回 Session        │                     │                     │
  │                            │  ← Session{ID,PID,...}  │                     │                      │                     │
  │                            │                         │                     │                      │                     │
  │                            │  sleep(100ms)            │                     │                      │                     │
  │  ← {session_id, shell_id,  │                         │                     │                      │                     │
  │     pid, ssh_config}       │                         │                     │                      │                     │
  │     (双 ID：连接 vs 终端)   │                         │                     │                      │                     │
```

## 三、输入流向（shell_input + shell_key）

```
AI Agent                    ChildShell                 sshclient              sshserver              OS/进程
  │                            │                          │                      │                     │
  │  shell_input(shell_id,text) │                          │                      │                     │
  │ ─────────────────────────> │  SendTerminalBytes       │                      │                     │
  │  shell_key(shell_id,enter) │  PressKey → \r / \n      │                      │                     │
  │ ─────────────────────────> │ ── Stdin.Write ─────────>│ ───── SSH data ────>│ ───── stdin ───────>│
  │  ← {"success":true}        │                          │                      │                     │
```

**关键设计**：shell 级 stdin 串行写入，防止并发 Agent 交替写入导致输入错乱。I/O 一律按 `shell_id`，不再用 session id 当默认 shell。

## 四、输出流向（shell_output）

```
进程 stdout/stderr                                                               AI Agent
  │                                                                                 ▲
  │ 每个 4096 字节 chunk                                                             │
  ▼                                                                                 │
  pipeToBuffer goroutine ── 读取 ──> buffer.Buffer ── 追加 master ──> 每 reader 独立 readPos │
  (每 stdout/stderr 各一个)           │                                                │
                                     │  ┌─────────┬─────────┬─────────┐              │
                                     │  │reader 0 │reader 3 │reader 7 │ ...          │
                                     │  │readPos │readPos │readPos │              │
                                     │  └────┬────┘────┬────┘─────────┘              │
                                     │       │         │                              │
                                     ▼       ▼         ▼                              │
                                    shell_output 被调用时:                              │
                                     │                                                 │
                                     │  buf.Read(ctx, readerID, timeout)               │
                                     │  ┌─ drain: 拷贝 master[readPos:] 并推进 readPos   │
                                     │  ├─ 无数据且未关闭: Cond.Wait(timeout)          │
                                     │  └─ closed: 返回 io.EOF                        │
                                     │                                                 │
                                     ▼                                                 │
                                    ansi.Strip(data)  ── 移除 ANSI 转义码              │
                                    │  CSI序列、OSC序列、字符集切换                     │
                                    ▼                                                 │
                                    ansi.Compact(data) ── 终端噪音压缩                 │
                                    │  ┌─ 控制字符清理 (保留 \r\n\t)                    │
                                    │  ├─ CRLF → LF                                   │
                                    │  ├─ \r覆盖 → 最后一行（进度条处理）               │
                                    │  ├─ 尾随空白去重                                 │
                                    │  └─ 3+空行 → 2空行                              │
                                    │  典型 git clone: ~11500字节 → ~200字节 (98%)     │
                                    ▼                                                 │
                                    message.Append(Output) → storage 持久化            │
                                    │                                                 │
                                    ▼                                                 │
                                    {output, has_more, lines_returned, bytes_returned} │
                                    ──────────────────────────────────────────────────>
```

**关键设计**：

| 特性 | 实现 |
|------|------|
| 多读者 | 每个 `shell_reader_register` 独立 readPos；共享一条 append-only master |
| 内存 | 全员已读过的前缀可整体丢弃；无固定容量环、不按读者覆盖旧数据 |
| 阻塞等待 | `sync.Cond.Wait()` + 超时 goroutine，支持 context 取消 |
| 输出清洗 | 两次处理：Strip(去ANSI) → Compact(压缩噪音) |
| 统一游标 | `shell_output` 是唯一输出读取入口：活/死/归档会话一律按字节流读取。活会话走 reader 增量游标；`offset` 无状态定位 & `tail_lines` 末尾截取对两种流同样生效；归档流由磁盘 MsgOutput 按序重建，与内存字节流同构（同 start_offset/end_offset/total_bytes/has_more 协议） |

## 五、信号/终止流向（session_terminate）

```
AI Agent                Session                    sshclient              sshserver              OS
  │                        │                          │                      │                     │
  │  session_terminate(   │                          │                      │                     │
  │    session_id,        │                          │                      │                     │
  │    force=false,       │                          │                      │                     │
  │    grace_period=5)    │                          │                      │                     │
  │ ─────────────────────>│                          │                      │                     │
  │                        │  terminateOnce.Do()      │                      │                     │
  │                        │                          │                      │                     │
  │                        │  [非强制]                 │                      │                     │
  │                        │  Signal(ssh.SIGTERM) ──>│                      │                     │
  │                        │                          │  session.Signal("TERM")
  │                        │                          │ ── SSH signal msg ──>│                     │
  │                        │                          │                      │  sshSignalToOSSig    │
  │                        │                          │                      │  "TERM" → SIGTERM    │
  │                        │                          │                      │  Process.Signal() ──>│ SIGTERM
  │                        │                          │                      │                     │
  │                        │                          │                      │                     │
  │                        │  [[ 等待 grace_period ]]                      │                     │
  │                        │  select: Done() | timeout                     │                     │
  │                        │                          │                      │                     │
  │                        │  [进程未退出 或 force]                          │                      │
  │                        │  ExecSession.Close() ──>│                      │                     │
  │                        │                          │  Stdin.Close()       │                     │
  │                        │                          │  session.Close()     │                     │
  │                        │                          │ ── SSH disconnect ──>│ 连接断开             │
  │                        │                          │                      │                     │
  │                        │  [[ 等待 2s ]]            │                      │                     │
  │                        │  exitOnce.Do():           │                      │                     │
  │                        │    Status=exited          │                      │                     │
  │                        │    ExitCode=-1            │                      │                     │
  │                        │    buffer.Close()         │                      │                     │
  │  ← {"success":true}    │                          │                      │                     │
```

**关键设计**：

| 概念 | 说明 |
|------|------|
| `terminateOnce` | 保证终止只执行一次，重复调用无副作用 |
| `exitOnce` | 保证 Status/ExitCode 只写一次，退出 goroutine 是单一权威 |
| 两阶段终止 | SIGTERM（优雅）→ Close（强制）→ 2s hard timeout |

## 六、PTY 调整大小流向（shell_resize）

```
AI Agent                Session                    sshclient              sshserver              OS
  │                        │                          │                      │                     │
  │  shell_resize(           │                          │                      │                     │
  │    session_id,         │                          │                      │                     │
  │    rows=40, cols=120)  │                          │                      │                     │
  │ ──────────────────────>│                          │                      │                     │
  │                        │  ResizePty(40,120)        │                      │                     │
  │                        │  ┌─ 检查 mode=pty         │                      │                     │
  │                        │  └─ WindowChange(40,120)─>│                      │                     │
  │                        │                          │  SSH window-change──>│                     │
  │                        │                          │                      │  pty.Setsize(f,      │
  │                        │                          │                      │    40x120)           │
  │                        │                          │                      │  ── ioctl ─────────>│ TIOCSWINSZ
  │                        │  s.Rows=40, s.Cols=120   │                      │                     │
  │  ← {"success":true}    │                          │                      │                     │
```

## 七、多 Agent 共享 Session 流程

```
Agent A (reader 0)           Session              Agent B (新加入)
  │                            │                     │
  │  session_start() ────────>│                     │
  │  ← reader_id:0 (默认)     │                     │
  │                            │                     │
  │  shell_output(             │                     │
  │    reader_id=0) ─────────>│                     │
  │                            │ buf.Read(ctx,0,...) │
  │  ← output                  │                     │
  │                            │                     │
  │                            │  shell_reader_register() ─┤
  │                            │ ─────────────────>  │
  │                            │  ← reader_id:3      │
  │                            │                     │
  │                            │  shell_output(       │
  │                            │    reader_id=3) ───>│
  │                            │ buf.Read(ctx,3,...) │
  │                            │ ← output (从头开始)  │
  │                            │                     │
  │  shell_output(reader_id=0)──┤                     │
  │  ← 新输出                  │                     │
```

**关键**：两个 Agent 各自有独立 readPos，互不干扰。Agent B 注册时游标起点 = 当时的 master 末尾（**无历史 backlog**），只能看到此后产生的新输出；历史不再因单读者环形容量被截断。`shell_open` 可在同一 SSH 连接上为 Agent B 开独立 shell 通道，彻底避免共用 reader 的游标协调问题。


```
每次 SendInput / readOutput / 系统事件
  │
  ▼
message.Manager.Append(sessionID, type, content)
  │
  ├─ 生成 UUID 前 12 位作为 message ID
  ├─ per-session mutex（防止索引并发损坏）
  │
  ├─ storage.SaveMessage(sessionID, msgID, Message{...})
  │   └─ atomicWriteFile: temp → fsync → rename
  │       data/messages/{session_id}/messages/{msg_id}.json
  │
  └─ storage.SaveMessageIndex(sessionID, entries)
      └─ atomicWriteFile: temp → fsync → rename
          data/messages/{session_id}/index.json
```

## 十、会话生命周期状态机

Session 的 `status` 描述的是连接容器，而 Shell 有自己独立的 `status`。当前状态的含义如下：

- `running`：Session 未关闭，SSH transport 可用；这是 Session 的正常在线状态。Shell 可以是 `running`，也可以是自然退出后仍保留在 channel 列表中的 `exited`。
- `exited`（代码/界面常称 DEAD）：Session 已结束（显式 terminate、SSH transport 异常断线、server shutdown，或 pipe 模式最后一个 shell 自然退出/被关闭）。Session 仍保留在 registry，缓冲区、消息和自然退出 shell 快照用于只读查看，直到显式 `DELETE`。
- `error`：保留给启动失败等错误；当前启动失败直接返回错误，不会创建 Session。
- `archived`：只用于 `history.json` 中的历史记录，不是当前 registry 中 Session 的在线状态。

```
                              session_start()
                                   │
                                   ▼
                           ┌──────────────┐
                           │   running    │
                           │  (在线容器)  │
                           └──┬─────┬─────┘
                              │     │
           Session terminate/ │     │  传输异常断线
           disconnect/shutdown│     │  或 pipe 最后 shell 结束
                              │     │
                              └──┬──┘
                                 ▼
                           ┌──────────────┐
                           │   exited     │
                           │ DEAD / 只读  │
                           └──────┬───────┘
                                  │ DELETE
                                  ▼
                               [gone]

      running ── shell 自然退出 ──> running（PTY；shell 留在列表供 drain）
      running ── shell 手动关闭 ──> running（PTY；shell 直接删除）
      running ── 最后 pipe shell 手动关闭 ──> exited（shell 直接删除）
```

**关键不变量：**

1. Session 未关闭时，`GET /api/sessions` 显示 `status: "running"`；不要因为某个 shell 退出或手动关闭就把仍可用的 PTY Session 标成 DEAD。
2. 手动关闭 Shell 是删除操作：从 live map、shell history snapshot 和持久化 `sessions.json` 中移除；它不会产生 `exited` shell，也不会被 UI 渲染成 `end` tab。
3. 只有自然退出或 transport 异常断线的 shell，才会保留 `exited` 元数据供 DEAD/只读视图使用。
4. Session 转为 `exited` 后不能创建新 shell；显式 `DELETE` 才释放对象、buffer、transport 并从 registry 移除。

**exitOnce / closeOnce 保证**：无论是进程自然退出还是 terminate/手动关闭触发，状态转换和 Done channel 都只执行一次；手动关闭与自然退出并发时，关闭标记优先，避免 shell 被重新写回历史快照。
