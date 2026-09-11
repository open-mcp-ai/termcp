package mcp

import (
	"bytes"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/history"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// outputSource abstracts ONE shell's raw output byte stream, regardless of the
// shell's lifecycle state:
//   - live shells (incl. exited-but-retained ones): stream from the in-memory
//     buffer;
//   - archived / restored-DEAD sessions: stream is the persisted MsgOutput log
//     of the shell (or the merged session stream), reconstructed in append
//     order — the same byte sequence the in-memory buffer once held.
//
// Both expose the same cursor semantics (Len + ByteRange), which is what makes
// shell_output a single unified read tool for every state.
type outputSource struct {
	live    *session.ChildShell // non-nil ⇒ live source
	sessID  string
	shellID string           // resolved shell id; "" = merged persisted stream
	hist    *history.Manager // non-nil ⇒ persisted source
	status  api.SessionStatus
	created time.Time // live sources only
}

const (
	SourceLive      = "live"
	SourcePersisted = "persisted"
	tailScanCeiling = 8 << 20 // cap backward tail scan at 8 MiB of raw bytes
	defaultTailCap  = 8192    // archived default read = last 8 KiB
)

func (o *outputSource) source() string {
	if o.live != nil {
		return SourceLive
	}
	return SourcePersisted
}

// Len returns the total raw stream length in bytes.
func (o *outputSource) Len() (int64, error) {
	if o.live != nil {
		return o.live.BufferLen(), nil
	}
	_, total, err := o.hist.OutputByteRange(o.sessID, o.shellID, 0, 0)
	return total, err
}

// ByteRange copies raw bytes [start, start+max) of the stream. No cursors or
// reader state are touched, so positional reads are safe for concurrent
// readers and for archived sessions alike.
func (o *outputSource) ByteRange(start int64, max int) ([]byte, int64, error) {
	if o.live != nil {
		return o.live.OutputByteRange(start, max)
	}
	return o.hist.OutputByteRange(o.sessID, o.shellID, start, max)
}

// resolveOutputSource maps an id onto the unified output stream. The id may be:
//   - a shell_id: that shell (live, or archived/restored per-shell stream);
//   - a session_id: the primary shell (live), or the merged output stream
//     (archived / restored DEAD session).
//
// Resolution order: live shells → live registry sessions (incl. restored DEAD)
// → archived history (merged, then per-shell).
func (s *Server) resolveOutputSource(id string) (*outputSource, *mcpgo.CallToolResult) {
	if cs := s.sessMgr.GetChildShell(id); cs != nil {
		info := cs.Info()
		sessID := id
		if parent := s.sessMgr.GetByShellID(id); parent != nil {
			sessID = parent.ID
		}
		return &outputSource{live: cs, sessID: sessID, shellID: cs.ID, status: info.Status, created: info.CreatedAt}, nil
	}
	if sess := s.sessMgr.Get(id); sess != nil {
		if cs := sess.PrimaryShell(); cs != nil {
			info := cs.Info()
			return &outputSource{live: cs, sessID: sess.ID, shellID: cs.ID, status: info.Status, created: info.CreatedAt}, nil
		}
		// Restored DEAD session: no live shell objects, but its on-disk message
		// log can still serve a merged persisted stream.
		if s.historyMgr == nil {
			return nil, toolError(CodeNotConfigured, "%s", "history not configured")
		}
		return &outputSource{sessID: sess.ID, hist: s.historyMgr, status: sess.Info().Status}, nil
	}
	if sess := s.sessMgr.GetSessionByShellID(id); sess != nil {
		if s.historyMgr == nil {
			return nil, toolError(CodeNotConfigured, "%s", "history not configured")
		}
		return &outputSource{sessID: sess.ID, shellID: id, hist: s.historyMgr, status: sess.Info().Status}, nil
	}
	if s.historyMgr != nil {
		if a, ok := s.historyMgr.Get(id); ok {
			status := a.Status
			if status == "" {
				status = api.SessionArchived
			}
			return &outputSource{sessID: a.ID, hist: s.historyMgr, status: status}, nil
		}
		for _, a := range s.historyMgr.List() {
			for _, sh := range a.Shells {
				if sh.ID == id {
					status := a.Status
					if status == "" {
						status = api.SessionArchived
					}
					return &outputSource{sessID: a.ID, shellID: id, hist: s.historyMgr, status: status}, nil
				}
			}
		}
	}
	return nil, toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", id))
}

// countLines counts newline-delimited lines in raw bytes; a trailing partial
// line (no closing '\n') counts as one line.
func countLines(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	n := int64(bytes.Count(b, []byte{'\n'}))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// truncateAtLines keeps only complete newline-terminated lines up to maxLines.
// Buffer parity: when the window does not reach the end of the stream, an
// incomplete trailing line stays in the stream for the next page.
func truncateAtLines(raw []byte, maxLines int, _ bool) []byte {
	if maxLines <= 0 {
		return raw
	}
	seen := 0
	for i, c := range raw {
		if c != '\n' {
			continue
		}
		seen++
		if seen == maxLines {
			return raw[:i+1]
		}
	}
	// Match buffer.ReadLimited: if the window contains fewer than maxLines
	// newline boundaries, return the whole window (including a final partial
	// line) so a long line cannot produce a zero-progress page.
	return raw
}

// scanTailWindow returns the last tailLines lines of the stream (or, when
// tailLines == 0, the last maxBytes bytes aligned to a line start) via a
// bounded backward scan, plus the raw start offset of the returned window.
// The returned window always ends at the stream end.
func (o *outputSource) scanTailWindow(tailLines, maxBytes int) (raw []byte, start, total int64, err error) {
	var raw0 []byte
	total, err = o.Len()
	if err != nil || total == 0 {
		return nil, 0, total, err
	}
	if tailLines <= 0 {
		cap := int64(maxBytes)
		if cap <= 0 {
			if o.live != nil {
				// Explicit max_bytes=0 on a live tail: still bounded — dump the
				// whole stream is never a safe default.
				cap = defaultTailCap
			} else {
				cap = total
			}
		}
		if cap > total {
			cap = total
		}
		start = total - cap
		raw0, _, err = o.ByteRange(start, int(cap))
		if err != nil {
			return nil, 0, total, err
		}
		// Align to a line start so the tail does not begin mid-line.
		if start > 0 {
			if idx := bytes.IndexByte(raw0, '\n'); idx >= 0 {
				raw0 = raw0[idx+1:]
				start += int64(idx + 1)
			}
			// If the bounded window contains no newline, return the partial
			// final line rather than an empty, non-progressing result.
		}
		return raw0, start, total, nil
	}

	window := int64(64 * 1024)
	for {
		if window > tailScanCeiling {
			window = tailScanCeiling
		}
		start = total - window
		if start < 0 {
			start = 0
		}
		raw0, _, err = o.ByteRange(start, int(window))
		if err != nil {
			return nil, 0, total, err
		}
		if countLines(raw0) >= int64(tailLines) || start == 0 || window >= tailScanCeiling {
			break
		}
		window *= 2
	}

	if n := countLines(raw0); n > int64(tailLines) {
		raw0 = takeLastLines(raw0, int64(tailLines))
	}
	// maxBytes caps the returned tail (align to a line start).
	if maxBytes > 0 && int64(len(raw0)) > int64(maxBytes) {
		cut := int64(len(raw0)) - int64(maxBytes)
		if idx := bytes.IndexByte(raw0[cut:], '\n'); idx >= 0 {
			// Prefer a complete line when one starts within the byte budget.
			raw0 = raw0[cut+int64(idx)+1:]
		} else {
			// A single line may exceed max_bytes; return its final bytes rather
			// than an empty, non-progressing page.
			raw0 = raw0[cut:]
		}
	}
	start = total - int64(len(raw0))
	return raw0, start, total, nil
}

// takeLastLines keeps only the final N newline-terminated lines of raw.
// A trailing '\n' terminates the last line, so it is not counted as a boundary
// of a following (empty) line: takeLastLines("A\nB\nC\n", 2) == "B\nC\n".
func takeLastLines(raw []byte, n int64) []byte {
	if n <= 0 || len(raw) == 0 {
		return nil
	}
	remaining := n
	end := len(raw)
	if raw[end-1] == '\n' {
		end--
	}
	for i := end - 1; i >= 0; i-- {
		if raw[i] != '\n' {
			continue
		}
		remaining--
		if remaining <= 0 {
			return raw[i+1:]
		}
	}
	return raw
}
