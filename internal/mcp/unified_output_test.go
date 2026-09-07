package mcp

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/history"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func newTestServerWithHistory(t *testing.T) (*Server, *storage.Store, *history.Manager, *session.Manager) {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })

	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	histMgr := history.New(store)
	_ = histMgr.Load()
	sessMgr.SetHistory(histMgr)

	s := New(sessMgr, msgMgr, sshconfig.NewStore(dir), nil)
	s.SetHistory(histMgr)
	return s, store, histMgr, sessMgr
}

// TestUnifiedOutput_TailLinesLive verifies tail_lines on a running pipe shell.
// Pipe mode is deterministic: no prompt echo, no PSReadLine redraw artifacts —
// the buffer contains exactly the command's stdout lines.
func TestUnifiedOutput_TailLinesLive(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	// One command that prints 20 numbered lines then exits (pipe mode).
	var command string
	var args []any
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = testShellArgs("-NoLogo", "-NoProfile", "-Command",
			"1..20 | ForEach-Object { Write-Output (\"LINE_{0:D2}\" -f $_) }")
	} else {
		command = "/bin/sh"
		args = testShellArgs("-c", "i=1; while [ $i -le 20 ]; do echo LINE_$(printf '%02d' $i); i=$((i+1)); done")
	}
	startReq := makeRequest(map[string]any{
		"command": command,
		"args":    args,
		"mode":    "pipe",
	})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v", err)
	}
	mStart := parseResult(t, startRes)
	sessID := mStart["session_id"].(string)
	shellID := mStart["shell_id"].(string)
	t.Cleanup(func() {
		termReq := makeRequest(map[string]any{"session_id": sessID, "force": true})
		_, _ = s.handleTerminateSession(context.Background(), termReq)
	})

	// The command exits quickly; wait until the full stdout (100 raw bytes +
	// newlines) is buffered, using the unified tail read itself.
	lineIn := func(out, want string) bool {
		for _, l := range strings.Split(out, "\n") {
			if l == want {
				return true
			}
		}
		return false
	}
	var m map[string]any
	var out string
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		tailReq := makeRequest(map[string]any{
			"shell_id":   shellID,
			"tail_lines": 3.0,
		})
		res, err := s.handleReadOutput(context.Background(), tailReq)
		if err != nil || res.IsError {
			t.Fatalf("handleReadOutput tail_lines error: %v", err)
		}
		m = parseResult(t, res)
		out = m["output"].(string)
		if lineIn(out, "LINE_20") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !lineIn(out, "LINE_20") {
		t.Fatalf("timed out waiting for LINE_20 output, got: %q", out)
	}
	// tail_lines=3 on a clean pipe stream returns exactly the last 3 lines.
	if !lineIn(out, "LINE_18") || !lineIn(out, "LINE_19") || !lineIn(out, "LINE_20") {
		t.Fatalf("expected exactly the last 3 lines, got: %q", out)
	}
	// ...and must NOT contain output from the beginning.
	if lineIn(out, "LINE_01") || lineIn(out, "LINE_02") {
		t.Fatalf("tail output should NOT contain early lines: %q", out)
	}
	if m["source"].(string) != "live" {
		t.Fatalf("expected source=live, got %v", m["source"])
	}
	if m["total_bytes"].(float64) <= 0 {
		t.Fatalf("expected total_bytes > 0, got %v", m["total_bytes"])
	}
	// The command already exited: status is now exited (still readable).
	if status := m["session_status"].(string); status != "exited" && status != "running" {
		t.Fatalf("expected session_status exited/running, got %v", status)
	}
}

// TestUnifiedOutput_OffsetStatelessLive verifies offset-based stateless pagination on live shells.
func TestUnifiedOutput_OffsetStatelessLive(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	startReq := makeRequest(map[string]any{
		"command": testShell(),
		"args":    testInteractiveShellArgs(),
		"mode":    "pty",
	})
	startRes, err := s.handleStartSession(context.Background(), startReq)
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v", err)
	}
	mStart := parseResult(t, startRes)
	sessID := mStart["session_id"].(string)
	shellID := mStart["shell_id"].(string)
	t.Cleanup(func() {
		termReq := makeRequest(map[string]any{"session_id": sessID, "force": true})
		_, _ = s.handleTerminateSession(context.Background(), termReq)
	})
	testRunLine(t, s, shellID, testInteractiveOutputCommand("ALPHA_BRAVO_CHARLIE"))
	_ = testReadOutputUntil(t, s, shellID, "ALPHA_BRAVO_CHARLIE", 5*time.Second)

	// Read from offset 0 with small max_bytes
	req1 := makeRequest(map[string]any{
		"shell_id":  shellID,
		"offset":    0.0,
		"max_bytes": 10.0,
	})
	res1, err := s.handleReadOutput(context.Background(), req1)
	if err != nil || res1.IsError {
		t.Fatalf("read offset 0 error: %v", err)
	}
	m1 := parseResult(t, res1)
	end1 := int64(m1["end_offset"].(float64))
	if end1 <= 0 {
		t.Fatalf("expected end_offset > 0, got %v", end1)
	}
	if tot := int64(m1["total_bytes"].(float64)); tot <= 0 {
		t.Fatalf("expected total_bytes > 0 on offset read, got %d", tot)
	}
	if !m1["has_more"].(bool) {
		t.Fatalf("expected has_more=true on first page")
	}

	// Read next chunk from end1
	req2 := makeRequest(map[string]any{
		"shell_id":  shellID,
		"offset":    float64(end1),
		"max_bytes": 50.0,
	})
	res2, err := s.handleReadOutput(context.Background(), req2)
	if err != nil || res2.IsError {
		t.Fatalf("read offset 2 error: %v", err)
	}
	m2 := parseResult(t, res2)
	if m2["start_offset"].(float64) != float64(end1) {
		t.Fatalf("expected start_offset=%d, got %v", end1, m2["start_offset"])
	}
}

// TestUnifiedOutput_ArchivedSessionReadsViaShellOutput verifies that an archived session
// is transparently readable through shell_output using session_id or shell_id,
// and honors tail_lines to protect against token blowups.
func TestUnifiedOutput_ArchivedSessionReadsViaShellOutput(t *testing.T) {
	s, store, histMgr, _ := newTestServerWithHistory(t)

	archID := "archived-100"
	shellID := "shell-xyz"

	// Create an archived session record
	arch := api.ArchivedSession{
		Session: api.Session{
			ID:        archID,
			Name:      "Build Task",
			Status:    api.SessionArchived,
			CreatedAt: time.Now().Add(-10 * time.Minute).UTC(),
		},
		Shells: []api.Session{
			{ID: shellID, Name: "main", Status: api.SessionArchived},
		},
		Reason: api.ArchiveExplicit,
	}
	if err := histMgr.Add(arch); err != nil {
		t.Fatalf("Add archived session failed: %v", err)
	}

	// Persist 50 output messages representing shell output
	var idxEntries []api.MessageIndexEntry
	baseTime := time.Now().Add(-10 * time.Minute).UTC()
	for i := 1; i <= 50; i++ {
		text := fmt.Sprintf("Build step [%02d]: finished successfully\n", i)
		m := api.Message{
			ID:        fmt.Sprintf("msg-%03d", i),
			SessionID: archID,
			ShellID:   shellID,
			Type:      api.MsgOutput,
			Content:   text,
			CreatedAt: baseTime.Add(time.Duration(i) * time.Second),
			ByteSize:  len(text),
		}
		idxEntries = append(idxEntries, api.MessageIndexEntry{
			ID:        m.ID,
			ShellID:   m.ShellID,
			Type:      m.Type,
			CreatedAt: m.CreatedAt,
			ByteSize:  m.ByteSize,
		})
		if err := store.SaveMessage(archID, m); err != nil {
			t.Fatalf("SaveMessage failed: %v", err)
		}
	}
	if err := store.SaveMessageIndex(archID, idxEntries); err != nil {
		t.Fatalf("SaveMessageIndex failed: %v", err)
	}

	// 1. Reading by session_id with tail_lines=3
	t.Run("by_session_id_tail_3", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"shell_id":   archID, // unified: session_id also accepted
			"tail_lines": 3.0,
		})
		res, err := s.handleReadOutput(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("handleReadOutput error: %v", err)
		}
		m := parseResult(t, res)
		out := m["output"].(string)
		if !strings.Contains(out, "Build step [50]") || !strings.Contains(out, "Build step [49]") || !strings.Contains(out, "Build step [48]") {
			t.Fatalf("expected last 3 steps in output, got: %q", out)
		}
		if strings.Contains(out, "Build step [01]") {
			t.Fatalf("tail should not contain step 01: %q", out)
		}
		if m["source"].(string) != "persisted" {
			t.Fatalf("expected source=persisted, got %v", m["source"])
		}
		if m["session_status"].(string) != "archived" {
			t.Fatalf("expected session_status=archived, got %v", m["session_status"])
		}
	})

	// 2. Reading by shell_id with offset paging
	t.Run("by_shell_id_offset_paging", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"shell_id":  shellID,
			"offset":    0.0,
			"max_lines": 2.0,
		})
		res, err := s.handleReadOutput(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("handleReadOutput error: %v", err)
		}
		m := parseResult(t, res)
		out := m["output"].(string)
		if !strings.Contains(out, "Build step [01]") || !strings.Contains(out, "Build step [02]") {
			t.Fatalf("expected first 2 steps, got: %q", out)
		}
		if strings.Contains(out, "Build step [03]") {
			t.Fatalf("max_lines=2 should not contain step 03: %q", out)
		}
		if !m["has_more"].(bool) {
			t.Fatalf("expected has_more=true")
		}
		end := int64(m["end_offset"].(float64))

		// Page 2: continue from end_offset
		req2 := makeRequest(map[string]any{
			"shell_id":  shellID,
			"offset":    float64(end),
			"max_lines": 2.0,
		})
		res2, err := s.handleReadOutput(context.Background(), req2)
		if err != nil || res2.IsError {
			t.Fatalf("handleReadOutput page 2 error: %v", err)
		}
		m2 := parseResult(t, res2)
		out2 := m2["output"].(string)
		if !strings.Contains(out2, "Build step [03]") {
			t.Fatalf("page 2 should contain step 03, got: %q", out2)
		}
	})

	// 3. Default read on archived (no offset, no tail_lines) must NOT dump entire file
	t.Run("archived_default_reads_tail_safely", func(t *testing.T) {
		req := makeRequest(map[string]any{
			"shell_id": archID,
		})
		res, err := s.handleReadOutput(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("default archived read error: %v", err)
		}
		m := parseResult(t, res)
		out := m["output"].(string)
		if !strings.Contains(out, "Build step [50]") {
			t.Fatalf("expected recent steps in default archived tail, got: %q", out)
		}
		// Must not dump starting from 0 if it's very large
		bytesRet := int64(m["bytes_returned"].(float64))
		if bytesRet > 8192 {
			t.Fatalf("default read should be bounded by 8KB, got %d", bytesRet)
		}
	})

	// 4. Input validation for negative offset and tail_lines
	t.Run("invalid_params", func(t *testing.T) {
		for _, tc := range []map[string]any{
			{"shell_id": archID, "tail_lines": -2.0},
			{"shell_id": archID, "offset": -5.0},
		} {
			res, _ := s.handleReadOutput(context.Background(), makeRequest(tc))
			if !res.IsError {
				t.Fatalf("expected error for params %v, got success", tc)
			}
		}
	})
}
