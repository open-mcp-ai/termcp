## Security & Privacy

禁止在项目中出现以下敏感信息和敏感文件：

- 本机 hostname、本地用户名称
- 真实密钥、API keys、tokens、passwords
- 邮箱地址、手机号等个人信息
- .env 文件、私钥文件（PEM、SSH key 等）
- 禁止出现 `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>` 相关字样

## Agent skills

### Issue tracker

Issues are tracked in GitHub Issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default vocabulary: `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout: `CONTEXT.md` + `docs/adr/` at repo root. See `docs/agents/domain.md`.

## Multi-session parallel work

Agents using interactive-process MCP tools must follow these rules for non-blocking, multi-session operation.

### Rules

1. **One task = one session.** Start a new session per independent task via `start_process`. Use the `name` param for tracking.
2. **Never block on reads.** Always use `read_output` with `timeout` ≤ 3. A long timeout blocks the entire agent — other sessions go unserviced.
3. **Prefer `send_and_read`.** For send-then-read patterns, use `send_and_read` instead of separate `send_input` + `read_output` calls. Same timeout rule applies.
4. **Poll in rotation.** When managing N sessions, loop through all of them: `read_output(timeout=1)` each, act on whichever has output, repeat.
5. **Clean up.** `terminate_process` then `delete_session` when done. Never leave zombie sessions.

### Multi-agent shared session

When multiple agents need to observe the same process:

1. Agent A: `start_process(...)` → session_id, default reader_id=0
2. Agent B: `register_reader(session_id=...)` → gets its own reader_id
3. Each agent calls `read_output(session_id=..., reader_id=<theirs>)` — independent cursors, no output stealing
4. Agent B leaves: `unregister_reader(session_id=..., reader_id=...)`

### Anti-patterns

- ❌ `read_output(timeout=30)` — blocks 30s, other sessions starve
- ❌ Waiting for session A to finish before starting session B — start both, poll both
- ❌ Multiple agents using the same reader_id — output gets consumed, others miss it
- ❌ Forgetting `delete_session` after `terminate_process` — stale metadata accumulates
