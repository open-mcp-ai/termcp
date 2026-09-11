package notify_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/notify"
)

type mockSender struct {
	mu            sync.Mutex
	resourceCalls []string
	samplingCalls []string
}

func (m *mockSender) SendResourceNotification(ctx context.Context, shellID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resourceCalls = append(m.resourceCalls, shellID)
	return nil
}

func (m *mockSender) SendSamplingNotification(ctx context.Context, shellID string, target any, event notify.Event, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samplingCalls = append(m.samplingCalls, shellID+":"+string(event))
	return nil
}

func (m *mockSender) counts() (resCount, sampCount int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.resourceCalls), len(m.samplingCalls)
}

func TestNotifyManager_RegisterAndList(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	rule1, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}
	if rule1.ID == "" {
		t.Fatal("expected non-empty rule ID")
	}

	rule2, err := mgr.Register("sess-1", "shell-2", notify.ChannelSampling, notify.EventExit, 0, nil)
	if err != nil {
		t.Fatalf("Register error: %v", err)
	}

	all := mgr.List("")
	if len(all) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(all))
	}

	filtered := mgr.List("shell-1")
	if len(filtered) != 1 || filtered[0].ID != rule1.ID {
		t.Fatalf("expected 1 rule for shell-1, got %d", len(filtered))
	}

	if !mgr.Unregister(rule1.ID) {
		t.Fatalf("expected Unregister to return true")
	}
	if mgr.Unregister("non-existent") {
		t.Fatalf("expected false for unknown rule")
	}
	if len(mgr.List("")) != 1 {
		t.Fatalf("expected 1 rule left after unregister")
	}
	_ = rule2
}

func TestNotifyManager_ClearShellAndSession(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender)

	mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	mgr.Register("sess-1", "shell-2", notify.ChannelSampling, notify.EventExit, 0, nil)
	mgr.Register("sess-2", "shell-3", notify.ChannelResource, notify.EventSilence, 2, nil)

	mgr.ClearShell("shell-1")
	if len(mgr.List("shell-1")) != 0 {
		t.Fatal("expected shell-1 rules to be cleared")
	}
	if len(mgr.List("")) != 2 {
		t.Fatalf("expected 2 rules remaining, got %d", len(mgr.List("")))
	}

	mgr.ClearSession("sess-1")
	if len(mgr.List("")) != 1 {
		t.Fatalf("expected 1 rule remaining after ClearSession(sess-1), got %d", len(mgr.List("")))
	}
	if mgr.List("")[0].ShellID != "shell-3" {
		t.Fatalf("expected shell-3 rule to remain, got %s", mgr.List("")[0].ShellID)
	}
}

func TestNotifyManager_ExitOneShotAndAutoCleanup(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender, notify.WithCooldown(10*time.Millisecond))

	_, err := mgr.Register("sess-1", "shell-1", notify.ChannelSampling, notify.EventExit, 0, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	// Also register a silence rule on the same shell to test that OnExit clears ALL rules on that shell
	_, err = mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventSilence, 5, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	exitCode := 0
	mgr.OnExit("shell-1", &exitCode)

	time.Sleep(50 * time.Millisecond)

	_, samp := sender.counts()
	if samp != 1 {
		t.Fatalf("expected 1 sampling notification for exit, got %d", samp)
	}

	// All rules for shell-1 must be auto-cleaned up
	if len(mgr.List("shell-1")) != 0 {
		t.Fatalf("expected all rules on shell-1 to be cleaned up, got %d", len(mgr.List("shell-1")))
	}
}

func TestNotifyManager_OutputDualEdgeAndCooldown(t *testing.T) {
	sender := &mockSender{}
	// Cooldown 40ms, trailing 60ms
	mgr := notify.NewManager(sender,
		notify.WithCooldown(40*time.Millisecond),
		notify.WithTrailingDelay(60*time.Millisecond),
	)

	_, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// 1st output: Immediate notification should fire
	mgr.OnOutput("shell-1")
	time.Sleep(10 * time.Millisecond)
	res, _ := sender.counts()
	if res != 1 {
		t.Fatalf("expected immediate leading-edge notification, got %d", res)
	}

	// 2nd output within cooldown (10ms later): immediate should be dropped by cooldown,
	// and trailing timer should be reset to +60ms from now
	time.Sleep(10 * time.Millisecond)
	mgr.OnOutput("shell-1")

	// Wait 15ms (total ~35ms from 1st output, still within cooldown of 40ms)
	time.Sleep(15 * time.Millisecond)
	res, _ = sender.counts()
	if res != 1 {
		t.Fatalf("cooldown should have prevented second immediate notification, got %d", res)
	}

	// Wait for trailing timer (60ms from 2nd output = fires at ~80ms from 1st output, well past cooldown of 40ms)
	time.Sleep(80 * time.Millisecond)
	res, _ = sender.counts()
	if res != 2 {
		t.Fatalf("expected trailing edge notification to fire once output stopped, got %d", res)
	}
}

func TestNotifyManager_SilenceOneShot(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender,
		notify.WithCooldown(10*time.Millisecond),
	)

	rule, err := mgr.Register("sess-1", "shell-1", notify.ChannelSampling, notify.EventSilence, 1, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	mgr.OnOutput("shell-1")

	// Within 500ms, silence should not have triggered yet
	time.Sleep(400 * time.Millisecond)
	_, samp := sender.counts()
	if samp != 0 {
		t.Fatalf("silence should not have triggered yet, got %d", samp)
	}

	// Wait for 1s silence timer to expire
	time.Sleep(800 * time.Millisecond)
	_, samp = sender.counts()
	if samp != 1 {
		t.Fatalf("expected silence notification, got %d", samp)
	}

	// One-shot silence rule should be automatically unregistered
	if len(mgr.List("shell-1")) != 0 {
		t.Fatalf("expected silence rule to be auto-unregistered, still found %d", len(mgr.List("shell-1")))
	}
	_ = rule
}

func TestNotifyManager_OutputBurstCollapsesToOneLeadingAndOneTrailing(t *testing.T) {
	sender := &mockSender{}
	mgr := notify.NewManager(sender,
		notify.WithCooldown(50*time.Millisecond),
		notify.WithTrailingDelay(80*time.Millisecond),
	)
	if _, err := mgr.Register("sess-1", "shell-1", notify.ChannelResource, notify.EventOutput, 0, nil); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// A fast producer: 200 chunks in a tight loop. The leading edge must be
	// throttled at the source and the trailing timer must collapse to one.
	for i := 0; i < 200; i++ {
		mgr.OnOutput("shell-1")
	}

	time.Sleep(30 * time.Millisecond)
	if res, _ := sender.counts(); res != 1 {
		t.Fatalf("burst should yield exactly 1 leading notification, got %d", res)
	}

	// Wait past the trailing delay; the trailing edge fires once.
	time.Sleep(120 * time.Millisecond)
	if res, _ := sender.counts(); res != 2 {
		t.Fatalf("burst should yield 1 leading + 1 trailing, got %d", res)
	}
}

func TestNotifyManager_SilenceSecondsOnlyForSilenceEvent(t *testing.T) {
	mgr := notify.NewManager(&mockSender{})
	if _, err := mgr.Register("s", "sh-out", notify.ChannelResource, notify.EventOutput, 9, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("s", "sh-exit", notify.ChannelResource, notify.EventExit, 9, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Register("s", "sh-sil", notify.ChannelResource, notify.EventSilence, 9, nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range mgr.List("") {
		if r.Event == notify.EventSilence {
			if r.SilenceSec != 9 {
				t.Fatalf("silence rule should keep silence_seconds=9, got %d", r.SilenceSec)
			}
			continue
		}
		if r.SilenceSec != 0 {
			t.Fatalf("%s rule should not expose silence_seconds, got %d", r.Event, r.SilenceSec)
		}
	}
}
