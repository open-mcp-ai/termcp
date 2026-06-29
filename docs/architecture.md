# termcp 终端进程通信流程架构

## 一、整体分层

```
┌──────────────────────────────────────────────────────────────┐
│                      AI Agent (MCP Client)                    │
│                     SSE over HTTP (JSON-RPC)                  │
└──────────────────────────────────────────────────────────────┘
                               │
    ┌──────────────────────────┼──────────────────────────┐
    │               internal/mcp/ (server.go)               │
    │                                                       │
    │  31 个工具: start_session / start_subshell / close_shell, │
    │  send_input / read_output / send_and_read / background_send, │
    │  list_sessions / get_session_info / terminate_session / delete_session, │
    │  resize_pty / register_reader / unregister_reader,    │
    │  forward_port / local_forward / dynamic_forward / list_forwards / close_forward, │
    │  file_read / file_write / file_stat / file_delete / file_rename / file_mkdir / get_file_urls, │
    │  detect_shell / list_ssh_configs / list_messages / get_message │
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

## 二、进程启动流程（start_session）

```
AI Agent                    MCP Server              Session.Manager        sshclient              sshserver              OS
  │                            │                         │                     │                      │                     │
  │  start_session(            │                         │                     │                      │                     │
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
  │  ← {session_id, pid,       │                         │                     │                      │                     │
  │     ssh_config,            │                         │                     │                      │                     │
  │     initial_output:""}   │                         │                     │                      │                     │
  │     (首包不读终端输出)      │                         │                     │                      │                     │
```

## 三、输入流向（send_input）

```
AI Agent                    Session                    sshclient              sshserver              OS/进程
  │                            │                          │                      │                     │
  │  send_input(               │                          │                      │                     │
  │    session_id, text,       │                          │                      │                     │
  │    press_enter=true)       │                          │                      │                     │
  │ ─────────────────────────> │                          │                      │                     │
  │                            │  SendInput(text, true)   │                      │                     │
  │                            │  ┌─ stdinMu.Lock()       │                      │                     │
  │                            │  │  确保串行写入          │                      │                     │
  │                            │  └─ ExecSession.Stdin ──>│                      │                     │
  │                            │                          │  session.StdinPipe   │                     │
  │                            │                          │ ───── SSH data ────>│                     │
  │                            │                          │                      │  [PTY] io.Copy(f, sess)
  │                            │                          │                      │  [pipe] cmd.Stdin    │
  │                            │                          │                      │ ───── stdin ───────>│
  │                            │                          │                      │                     │
  │                            │  message.Append(Input)   │                      │                     │
  │                            │ ──────────────> storage  │                      │                     │
  │  ← {"success":true}        │                          │                      │                     │
```

**关键设计**：`stdinMu` 串行化所有 stdin 写入，防止并发 Agent 交替写入导致输入错乱。

## 四、输出流向（read_output）

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
                                    read_output 被调用时:                              │
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
| 多读者 | 每个 `register_reader` 独立 readPos；共享一条 append-only master |
| 内存 | 全员已读过的前缀可整体丢弃；无固定容量环、不按读者覆盖旧数据 |
| 阻塞等待 | `sync.Cond.Wait()` + 超时 goroutine，支持 context 取消 |
| 输出清洗 | 两次处理：Strip(去ANSI) → Compact(压缩噪音) |

## 五、信号/终止流向（terminate_session）

```
AI Agent                Session                    sshclient              sshserver              OS
  │                        │                          │                      │                     │
  │  terminate_session(   │                          │                      │                     │
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

## 六、PTY 调整大小流向（resize_pty）

```
AI Agent                Session                    sshclient              sshserver              OS
  │                        │                          │                      │                     │
  │  resize_pty(           │                          │                      │                     │
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
  │  start_session() ────────>│                     │
  │  ← reader_id:0 (默认)     │                     │
  │                            │                     │
  │  read_output(             │                     │
  │    reader_id=0) ─────────>│                     │
  │                            │ buf.Read(ctx,0,...) │
  │  ← output                  │                     │
  │                            │                     │
  │                            │  register_reader() ─┤
  │                            │ ─────────────────>  │
  │                            │  ← reader_id:3      │
  │                            │                     │
  │                            │  read_output(       │
  │                            │    reader_id=3) ───>│
  │                            │ buf.Read(ctx,3,...) │
  │                            │ ← output (从头开始)  │
  │                            │                     │
  │  read_output(reader_id=0)──┤                     │
  │  ← 新输出                  │                     │
```

**关键**：两个 Agent 各自有独立 readPos，互不干扰。Agent B 注册时游标起点 = 当时的 master 末尾（**无历史 backlog**），只能看到此后产生的新输出；历史不再因单读者环形容量被截断。`start_subshell` 可在同一 SSH 连接上为 Agent B 开独立 shell 通道，彻底避免共用 reader 的游标协调问题。


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

```
                  start_session()
                       │
                       ▼
               ┌──────────────┐
               │   running    │
               └──┬───────┬───┘
                  │       │
    进程自行退出  │       │  terminate_session()
    (startReaders │       │
     goroutine    │       │
     检测退出)    │       │
                  │       │
                  ▼       ▼
              ┌──────────────┐
              │   exited     │──── delete_session() ────> [从注册表移除]
              └──────────────┘
                  │
         启动失败时
                  │
                  ▼
              ┌──────────────┐
              │    error     │
              └──────────────┘
```

**exitOnce 保证**：无论是进程自然退出还是 terminate 触发，Status/ExitCode 只设置一次，不会竞态覆盖。
