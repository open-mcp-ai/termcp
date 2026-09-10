package session

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestPipeShellDrainsOutputBeforeSealingBuffer locks the drain contract: the SSH
// session reports exit-status as soon as the process is gone, which can happen
// before the trailing stdout has been read off the channel. The retained buffer
// and the archived transcript must not be sealed until every byte has been
// drained, otherwise fast commands silently lose their tail.
func TestPipeShellDrainsOutputBeforeSealingBuffer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell loop to emit many short lines")
	}
	const lines = 200
	const lineBytes = len("LINE_000\n")

	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)
	m := NewManager(mm, store, srv)

	script := fmt.Sprintf("i=1; while [ $i -le %d ]; do echo LINE_$(printf '%%03d' $i); i=$((i+1)); done", lines)
	s, err := m.Create(Config{Command: "/bin/sh", Args: []string{"-c", script}, Mode: api.ModePipe, Name: "drain", Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}

	shells := s.ListChildShells()
	if len(shells) != 1 {
		t.Fatalf("expected exactly 1 shell, got %d", len(shells))
	}
	cs := s.GetChildShell(shells[0].ID)
	if cs == nil {
		t.Fatal("child shell vanished from the session")
	}

	// The command exits on its own; wait for the buffer to be sealed, which is
	// the point where the transcript is final.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !cs.IsBufferClosed() {
		time.Sleep(20 * time.Millisecond)
	}
	if !cs.IsBufferClosed() {
		t.Fatal("shell buffer was never sealed after the process exited")
	}

	// Let the DEAD transition finish: it appends a lifecycle message to the same
	// store, and racing the temp-dir teardown with that write fails the cleanup.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && s.Info().Status == api.SessionRunning {
		time.Sleep(20 * time.Millisecond)
	}

	raw, total, err := cs.OutputByteRange(0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if total != int64(lines*lineBytes) {
		t.Fatalf("retained %d bytes, want %d (tail = %q)", total, lines*lineBytes, tailOf(got, 120))
	}
	for i := 1; i <= lines; i++ {
		want := fmt.Sprintf("LINE_%03d", i)
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s after exit (total=%d, tail = %q)", want, total, tailOf(got, 120))
		}
	}

	// Archive parity: the persisted transcript must hold the same stream, since
	// every buffered write is archived at the source.
	entries, err := mm.List(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	var archived int64
	for _, e := range entries {
		if e.Type == api.MsgOutput {
			archived += int64(e.ByteSize)
		}
	}
	if archived != total {
		t.Fatalf("archived %d bytes, retained %d — transcript is truncated", archived, total)
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
