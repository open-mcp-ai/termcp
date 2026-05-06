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
    │  17 个工具: start_process, send_input, read_output,    │
    │  send_and_read, terminate_process, resize_pty,       │
    │  upload_file, download_file, list_files, ...          │
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
   │ SFTPConn    │ │ 多读者环形  │ │ 原子 JSON    │
   └──────┬──────┘ │ 缓冲区     │ │ 持久化       │
          │        └────────────┘ └──────────────┘
          │ SSH (x/crypto/ssh)
          ▼
   ┌──────────────────────────────────────────┐
   │        internal/sshserver/                │
   │        gliderlabs/ssh Server              │
   │                                          │
   │  ┌─────────────────────────────────┐     │
   │  │ PTY 分支: creack/pty            │     │
   │  │   pty.StartWithSize(cmd, size)  │     │
   │  │   io.Copy(pty_fd, session)      │     │
   │  │   pty.Setsize() 窗口调整        │     │
   │  ├─────────────────────────────────┤     │
   │  │ Pipe 分支: cmd.Stdin/Out/Err    │     │
   │  │   直接连接 SSH session          │     │
   │  └─────────────────────────────────┘     │
   │                                          │
   │  sshSignalToOSSig: TERM→SIGTERM, ...    │
   │  SFTP 子系统: pkg/sftp                   │
   └──────────────────┬───────────────────────┘
                      │ exec.Command
                      ▼
             ┌─────────────────┐
             │  实际进程        │
             │  bash/zsh/pwsh  │
             │  python/node/.. │
             └─────────────────┘
```

## 二、进程启动流程（start_process）

```
AI Agent                    MCP Server              Session.Manager        sshclient              sshserver              OS
  │                            │                         │                     │                      │                     │
  │  start_process(            │                         │                     │                      │                     │
  │    command="bash",         │                         │                     │                      │                     │
  │    mode="pty",             │                         │                     │                      │                     │
  │    rows=24, cols=80)       │                         │                     │                      │                     │
  │ ─────────────────────────> │                         │                     │                      │                     │
  │                            │  validateStartParams()  │                     │                      │                     │
  │                            │  sessMgr.Create(cfg) ──>│                     │                      │                     │
  │                            │                         │  session.New()      │                      │                     │
  │                            │                         │  sshclient.Start()──>│                     │                     │
  │                            │                         │                     │  ssh.Dial("tcp")     │                     │
  │                            │                         │                     │ ────────────────────>│ TCP 连接            │
  │                            │                         │                     │  client.NewSession() │                     │
  │                            │                         │                     │  StdinPipe()         │ 打开 stdin 通道      │
  │                            │                         │                     │  StdoutPipe()        │ 打开 stdout 通道     │
  │                            │                         │                     │  StderrPipe()        │ 打开 stderr 通道     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │  [if pty]             │                     │
  │                            │                         │                     │  RequestPty(          │                     │
  │                            │                         │                     │    "xterm-256color",  │  PTY 请求 →          │
  │                            │                         │                     │    24, 80, ...)       │  PtyCallback=true    │
  │                            │                         │                     │ ────────────────────>│                     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │  session.Start(       │  exec channel        │
  │                            │                         │                     │    "bash")            │  请求                │
  │                            │                         │                     │ ────────────────────>│                     │
  │                            │                         │                     │                      │                     │
  │                            │                         │                     │                      │  [if pty]            │
  │                            │                         │                     │                      │  pty.StartWithSize(  │
  │                            │                         │                     │                      │    cmd, 24x80)       │
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
  │                            │                         │  SFTP 连接             │                     │                     │
  │                            │                         │  ← 返回 Session        │                     │                     │
  │                            │  ← Session{ID,PID,...}  │                     │                      │                     │
  │                            │                         │                     │                      │                     │
  │                            │  sleep(100ms)            │                     │                      │                     │
  │                            │  ReadOutput(500ms) ────>│                     │                      │                     │
  │                            │                         │  buf.Read(reader0)   │                      │                     │
  │                            │                         │  ansi.Strip+Compact  │                      │                     │
  │  ← {session_id, pid,      │                         │                     │                      │                     │
  │     initial_output}        │                         │                     │                      │                     │
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
  pipeToBuffer goroutine ── 读取 ──> buffer.Buffer ── 广播 ──> 所有 ringbuffer       │
  (每 stdout/stderr 各一个)           │                                                │
                                     │  ┌─────────┬─────────┬─────────┐              │
                                     │  │reader 0 │reader 3 │reader 7 │ ...          │
                                     │  │ 1024KB  │ 1024KB  │ 1024KB  │              │
                                     │  │pos:1582 │pos:337  │pos:985  │              │
                                     │  └────┬────┘────┬────┘─────────┘              │
                                     │       │         │                              │
                                     ▼       ▼         ▼                              │
                                    read_output 被调用时:                              │
                                     │                                                 │
                                     │  buf.Read(ctx, readerID, timeout)               │
                                     │  ┌─ drain(rb): 读取所有可用字节                 │
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
| 多读者 | 每个 `register_reader` 创建独立 ringbuffer，独立游标 |
| 慢读者不阻塞 | ringbuffer `SetOverwrite(true)`，满时覆盖旧数据 |
| 阻塞等待 | `sync.Cond.Wait()` + 超时 goroutine，支持 context 取消 |
| 输出清洗 | 两次处理：Strip(去ANSI) → Compact(压缩噪音) |

## 五、信号/终止流向（terminate_process）

```
AI Agent                Session                    sshclient              sshserver              OS
  │                        │                          │                      │                     │
  │  terminate_process(   │                          │                      │                     │
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
  │  start_process() ────────>│                     │
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

**关键**：两个 Agent 各自有独立游标，互不干扰。Agent B 注册时获得一个新的空 ringbuffer，但从那一刻起的输出都能看到。

## 八、SFTP 文件传输流程

```
AI Agent                    Session                     SFTPConn(独立SSH连接)      OS 文件系统
  │                            │                              │                      │
  │  upload_file(             │                              │                      │
  │    session_id,            │                              │                      │
  │    content_base64,        │                              │                      │
  │    remote_path)           │                              │                      │
  │ ─────────────────────────>│                              │                      │
  │                            │  UploadFile(base64, path)    │                      │
  │                            │  ┌─ base64 decode            │                      │
  │                            │  ├─ 大小检查 max 1MB         │                      │
  │                            │  ├─ MkdirAll(dir) ──────────>│ ── SSH SFTP ───────> │ mkdir -p
  │                            │  ├─ Create(file) ───────────>│ ── SSH SFTP ───────> │ 创建文件
  │                            │  └─ Write(data) ────────────>│ ── SSH SFTP ───────> │ 写入
  │                            │                              │                      │
  │  ← {status:"uploaded",    │                              │                      │
  │     remote_path, size}     │                              │                      │
  │                            │                              │                      │
  │  download_file(            │                              │                      │
  │    session_id,             │                              │                      │
  │    remote_path)            │                              │                      │
  │ ─────────────────────────>│                              │                      │
  │                            │  DownloadFile(path)           │                      │
  │                            │  ┌─ Stat(path) ─────────────>│ ── SSH SFTP ───────> │ stat
  │                            │  ├─ 大小检查 max 1MB         │                      │
  │                            │  ├─ Open(path) ─────────────>│ ── SSH SFTP ───────> │ 打开
  │                            │  ├─ ReadFull(data) ─────────>│ ── SSH SFTP ───────> │ 读取
  │                            │  └─ 检测 null byte:           │                      │
  │                            │      无 → text (原样返回)     │                      │
  │                            │      有 → base64 编码         │                      │
  │  ← {content, encoding,    │                              │                      │
  │     size}                  │                              │                      │
```

**注意**：SFTP 使用**独立的 SSH 连接**（不与命令执行的 session 共享），进程退出后 SFTP 连接延迟 60 秒关闭，以便 Agent 在进程结束后仍能下载文件。

## 九、消息持久化流程

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
                  start_process()
                       │
                       ▼
               ┌──────────────┐
               │   running    │
               └──┬───────┬───┘
                  │       │
    进程自行退出  │       │  terminate_process()
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
