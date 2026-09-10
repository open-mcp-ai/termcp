package session

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestSession_PtyResizeReachesChild locks the window-change contract: a resize
// requested by the client must reach the child's terminal, so the shell's own
// `stty size` reports the new geometry. The internal SSH server hands every
// window-change to charmbracelet/ssh's own resize handling; draining the
// library's window channel itself silently discarded ~half of all resizes.
func TestSession_PtyResizeReachesChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stty is not available on Windows")
	}
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	time.Sleep(300 * time.Millisecond)

	// askSize runs `stty size` in the shell and returns until the child reports
	// exactly rows x cols (empty when it never does).
	askSize := func(rows, cols int) string {
		t.Helper()
		want := fmt.Sprintf("%d %d", rows, cols)
		var out string
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := s.SendInput(testShellInput("stty size"), false); err != nil {
				t.Fatalf("send stty: %v", err)
			}
			chunk, _ := s.ReadOutput(context.Background(), 300*time.Millisecond, true, 0, 0)
			out += chunk
			if strings.Contains(out, want) {
				return out
			}
		}
		return out
	}

	// Initial geometry is the one the session was created with.
	if out := askSize(24, 80); !strings.Contains(out, "24 80") {
		t.Fatalf("expected initial tty size 24 80, got %q", out)
	}

	// Each resize must be observable by the child, not just tracked client-side.
	for _, size := range []struct{ rows, cols int }{{30, 100}, {40, 110}, {50, 120}} {
		if err := s.ResizePty(size.rows, size.cols); err != nil {
			t.Fatalf("resize %dx%d: %v", size.rows, size.cols, err)
		}
		if out := askSize(size.rows, size.cols); !strings.Contains(out, fmt.Sprintf("%d %d", size.rows, size.cols)) {
			t.Fatalf("window-change %dx%d did not reach the child tty, got %q", size.rows, size.cols, out)
		}
	}
}
