package session

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestSession_PtyChildShellsAreIndependent locks the per-session PTY handoff:
// shells multiplexed on one SSH connection must each keep their own PTY, their
// own geometry, and must not steal each other's resize events.
func TestSession_PtyChildShellsAreIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stty is not available on Windows")
	}
	srv := startTestServer(t)

	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	type shell struct {
		cs       *ChildShell
		readerID int
		rows     int
		cols     int
	}
	// Several PTY shells on the same SSH connection, created concurrently, with
	// distinct geometries: a PTY stashed per *connection* instead of per session
	// would hand these shells each other's pty.
	geometries := [][2]int{{30, 100}, {40, 110}, {50, 120}, {60, 130}}
	shells := make([]*shell, len(geometries))
	for i, g := range geometries {
		shells[i] = &shell{rows: g[0], cols: g[1]}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, len(shells))
	for i, sh := range shells {
		wg.Add(1)
		go func(i int, sh *shell) {
			defer wg.Done()
			<-start
			sh.cs, errs[i] = s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, sh.rows, sh.cols, fmt.Sprintf("shell-%d", i))
		}(i, sh)
	}
	close(start)
	wg.Wait()
	for i, sh := range shells {
		if errs[i] != nil {
			t.Fatalf("create child shell %d: %v", i, errs[i])
		}
		sh.readerID, _ = sh.cs.RegisterReader()
	}
	time.Sleep(300 * time.Millisecond)

	drain := func(sh *shell) string {
		out, _ := sh.cs.ReadTerminalStream(context.Background(), sh.readerID, 200*time.Millisecond, true, 0, 0)
		return out
	}
	// Discard startup noise so assertions only see `stty size` output.
	for _, sh := range shells {
		_ = drain(sh)
	}

	askSize := func(sh *shell) string {
		t.Helper()
		want := fmt.Sprintf("%d %d", sh.rows, sh.cols)
		var out string
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := sh.cs.SendTerminalBytes([]byte("stty size\n"), false); err != nil {
				t.Fatalf("send stty: %v", err)
			}
			out += drain(sh)
			if strings.Contains(out, want) {
				return out
			}
		}
		return out
	}

	for _, sh := range shells {
		if out := askSize(sh); !strings.Contains(out, fmt.Sprintf("%d %d", sh.rows, sh.cols)) {
			t.Fatalf("shell %dx%d did not report its own geometry, got %q", sh.rows, sh.cols, out)
		}
	}

	// Resizing one shell must not change any other shell's terminal.
	target := shells[0]
	if err := target.cs.ResizePty(70, 150); err != nil {
		t.Fatalf("resize: %v", err)
	}
	target.rows, target.cols = 70, 150
	for _, sh := range shells {
		// Drop buffered output so the assertion can only see freshly produced
		// `stty size` results for the (possibly leaked) geometry.
		_ = drain(sh)
		if out := askSize(sh); !strings.Contains(out, fmt.Sprintf("%d %d", sh.rows, sh.cols)) {
			t.Fatalf("shell %q reported %q, want %d %d — resizes leaked between shells",
				sh.cs.Name, out, sh.rows, sh.cols)
		}
	}
}
