package session

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

type createLogCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *createLogCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (c *createLogCapture) Level() slog.Level                            { return slog.LevelDebug }
func (c *createLogCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *createLogCapture) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *createLogCapture) WithGroup(_ string) slog.Handler      { return c }

func (c *createLogCapture) find(t *testing.T, level slog.Level, msg string) slog.Record {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level == level && r.Message == msg {
			return r
		}
	}
	t.Fatalf("no %v record with message %q among %d records", level, msg, len(c.records))
	return slog.Record{}
}

func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	found := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value
			found = true
			return false
		}
		return true
	})
	return v, found
}

// TestManager_CreateFailureLogged pins the contract: a failed session create
// (e.g. unreachable SSH host) must be logged at Error level on the termcp
// terminal — the MCP/WebUI callers only surface the error in their own
// response channel, which is otherwise invisible to the operator.
func TestManager_CreateFailureLogged(t *testing.T) {
	cap := &createLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	m := NewManager(nil, nil, nil) // remote path: internalSSH unused
	_, err := m.Create(Config{
		Command: testShell(),
		Mode:    api.ModePipe,
		Name:    "unreachable",
		Remote: &RemoteSSH{
			Host:             "127.0.0.1",
			Port:             1, // nothing listens → fast refusal, no long timeout wait
			User:             "u",
			Password:         "p",
			TrustUnknownHost: true,
		},
	})
	if err == nil {
		t.Fatal("expected dial failure to a closed port")
	}

	rec := cap.find(t, slog.LevelError, "session create failed")
	if v, ok := recordAttr(rec, "remote_addr"); !ok || v.String() != "127.0.0.1:1" {
		t.Fatalf("expected remote_addr=127.0.0.1:1, got %q (found=%v)", v, ok)
	}
	if v, ok := recordAttr(rec, "name"); !ok || v.String() != "unreachable" {
		t.Fatalf("expected name=unreachable, got %q (found=%v)", v, ok)
	}
	if _, ok := recordAttr(rec, "err"); !ok {
		t.Fatal("expected err attr on failure record")
	}

	// The returned error must carry the dial target and the effective timeout.
	if !strings.Contains(err.Error(), "ssh dial: connect 127.0.0.1:1 (timeout 30s):") {
		t.Fatalf("error should name target and timeout, got %q", err)
	}
	// And must never leak the password into the error text.
	if strings.Contains(err.Error(), "p\n") || strings.Contains(err.Error(), " password") {
		t.Fatalf("error must not leak credentials, got %q", err)
	}
}
