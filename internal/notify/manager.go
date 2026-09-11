package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Channel defines the notification delivery channel.
type Channel string

const (
	ChannelResource Channel = "resource"
	ChannelSampling Channel = "sampling"
)

// Event defines the event type that triggers the notification.
type Event string

const (
	EventExit    Event = "exit"
	EventSilence Event = "silence"
	EventOutput  Event = "output"
)

// Rule represents a registered notification rule (internal state; never copied
// by value — it carries the per-rule timer mutex).
type Rule struct {
	ID         string    `json:"rule_id"`
	SessionID  string    `json:"session_id"`
	ShellID    string    `json:"shell_id"`
	Channel    Channel   `json:"channel"`
	Event      Event     `json:"event"`
	SilenceSec int       `json:"silence_seconds,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	// Target is an opaque, per-registration delivery handle captured from the
	// registering caller (for sampling: the MCP client session). The Sender
	// decides how to interpret it; the notify package never inspects it.
	Target any `json:"-"`

	mu          sync.Mutex
	timer       *time.Timer
	stopped     bool
	lastLeading time.Time // leading-edge throttle: last immediate dispatch for this rule
}

// RuleView is the lock-free JSON/API projection of a Rule. List returns these
// so callers never copy a Rule's mutex or observe its timer state.
type RuleView struct {
	ID         string    `json:"rule_id"`
	SessionID  string    `json:"session_id"`
	ShellID    string    `json:"shell_id"`
	Channel    Channel   `json:"channel"`
	Event      Event     `json:"event"`
	SilenceSec int       `json:"silence_seconds,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r *Rule) view() RuleView {
	return RuleView{
		ID:         r.ID,
		SessionID:  r.SessionID,
		ShellID:    r.ShellID,
		Channel:    r.Channel,
		Event:      r.Event,
		SilenceSec: r.SilenceSec,
		CreatedAt:  r.CreatedAt,
	}
}

// Sender is responsible for sending notifications through the concrete channel.
// target is the opaque per-rule delivery handle captured at registration time.
type Sender interface {
	SendResourceNotification(ctx context.Context, shellID string) error
	SendSamplingNotification(ctx context.Context, shellID string, target any, event Event, status string) error
}

// Manager handles the lifecycle, timers, debouncing, and cooldown gates for notification rules.
type Manager struct {
	mu           sync.RWMutex
	rules        map[string]*Rule               // ruleID -> *Rule
	shellRules   map[string]map[string]*Rule    // shellID -> ruleID -> *Rule
	sessionShell map[string]map[string]struct{} // sessionID -> shellID set
	lastSentAt   time.Time                      // global cooldown gate: last successful send
	cooldown     time.Duration
	trailingSec  time.Duration
	sender       Sender
	onChange     func() // invoked (without locks) after any rule-set mutation
}

// SetOnChange registers a callback fired after rules are added/removed so the
// UI can refresh. The callback runs without the manager lock held.
func (m *Manager) SetOnChange(fn func()) {
	m.mu.Lock()
	m.onChange = fn
	m.mu.Unlock()
}

func (m *Manager) notifyChange() {
	m.mu.RLock()
	fn := m.onChange
	m.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Option allows configuring Manager parameters (e.g. for testing).
type Option func(*Manager)

// WithCooldown configures the cooldown window (default 1s).
func WithCooldown(d time.Duration) Option {
	return func(m *Manager) {
		m.cooldown = d
	}
}

// WithTrailingDelay configures the trailing edge delay for output events (default 2s).
func WithTrailingDelay(d time.Duration) Option {
	return func(m *Manager) {
		m.trailingSec = d
	}
}

// NewManager creates an instance of Manager.
func NewManager(sender Sender, opts ...Option) *Manager {
	m := &Manager{
		rules:        make(map[string]*Rule),
		shellRules:   make(map[string]map[string]*Rule),
		sessionShell: make(map[string]map[string]struct{}),
		cooldown:     time.Second,
		trailingSec:  2 * time.Second,
		sender:       sender,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Register adds a new notification rule for a shell. target is an opaque
// delivery handle (for sampling: the MCP client session that registered it);
// it may be nil.
func (m *Manager) Register(sessionID, shellID string, channel Channel, event Event, silenceSec int, target any) (*Rule, error) {
	if shellID == "" {
		return nil, fmt.Errorf("shell_id is required")
	}
	if channel != ChannelResource && channel != ChannelSampling {
		return nil, fmt.Errorf("invalid channel %q: must be resource or sampling", channel)
	}
	if event == "" {
		event = EventOutput
	}
	if event != EventExit && event != EventSilence && event != EventOutput {
		return nil, fmt.Errorf("invalid event %q: must be exit, silence, or output", event)
	}
	if event == EventSilence {
		if silenceSec <= 0 {
			silenceSec = 3
		}
		if silenceSec > 300 {
			silenceSec = 300
		}
	} else {
		// silence_seconds is meaningful only for the silence event; keep it out
		// of the API projection for output/exit rules.
		silenceSec = 0
	}

	m.mu.Lock()

	ruleID := "notif_" + uuid.New().String()[:12]
	rule := &Rule{
		ID:         ruleID,
		SessionID:  sessionID,
		ShellID:    shellID,
		Channel:    channel,
		Event:      event,
		SilenceSec: silenceSec,
		CreatedAt:  time.Now().UTC(),
		Target:     target,
	}

	m.rules[ruleID] = rule
	if m.shellRules[shellID] == nil {
		m.shellRules[shellID] = make(map[string]*Rule)
	}
	m.shellRules[shellID][ruleID] = rule

	if sessionID != "" {
		if m.sessionShell[sessionID] == nil {
			m.sessionShell[sessionID] = make(map[string]struct{})
		}
		m.sessionShell[sessionID][shellID] = struct{}{}
	}
	m.mu.Unlock()
	m.notifyChange()

	return rule, nil
}

// Unregister removes a notification rule by ID and stops any pending timer.
func (m *Manager) Unregister(ruleID string) bool {
	m.mu.Lock()
	rule, ok := m.rules[ruleID]
	if !ok {
		m.mu.Unlock()
		return false
	}
	m.removeRuleLocked(rule)
	m.mu.Unlock()
	m.notifyChange()
	return true
}

func (m *Manager) removeRuleLocked(rule *Rule) {
	rule.mu.Lock()
	rule.stopped = true
	if rule.timer != nil {
		rule.timer.Stop()
		rule.timer = nil
	}
	rule.mu.Unlock()

	delete(m.rules, rule.ID)
	if sr, ok := m.shellRules[rule.ShellID]; ok {
		delete(sr, rule.ID)
		if len(sr) == 0 {
			delete(m.shellRules, rule.ShellID)
			// Drop the shell from its session index once it has no rules left,
			// so ClearSession never accumulates stale shell ids.
			if shells, ok := m.sessionShell[rule.SessionID]; ok {
				delete(shells, rule.ShellID)
				if len(shells) == 0 {
					delete(m.sessionShell, rule.SessionID)
				}
			}
		}
	}
}

// List returns the active notification rules, optionally filtered by shellID.
func (m *Manager) List(shellID string) []RuleView {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]RuleView, 0, len(m.rules))
	for _, r := range m.rules {
		if shellID == "" || r.ShellID == shellID {
			result = append(result, r.view())
		}
	}
	return result
}

// ClearShell removes all notification rules registered for the given shellID and cancels active timers.
func (m *Manager) ClearShell(shellID string) {
	m.mu.Lock()
	rules, ok := m.shellRules[shellID]
	if ok {
		for _, rule := range rules {
			m.removeRuleLocked(rule)
		}
		delete(m.shellRules, shellID)
	}
	m.mu.Unlock()
	if ok {
		m.notifyChange()
	}
}

// ClearSession removes all notification rules registered across all shells for the given sessionID.
func (m *Manager) ClearSession(sessionID string) {
	m.mu.Lock()
	shells, ok := m.sessionShell[sessionID]
	if !ok {
		m.mu.Unlock()
		return
	}
	delete(m.sessionShell, sessionID)
	m.mu.Unlock()

	for shellID := range shells {
		m.ClearShell(shellID)
	}
}

// OnOutput is called when a shell produces output.
// It executes:
// 1. For EventOutput rules:
//   - Action 1: Attempts immediate notification (subject to Cooldown Gate)
//   - Action 2: Cancels existing trailing timer and sets a new trailing timer (2s)
//
// 2. For EventSilence rules:
//   - Cancels existing silence timer and sets a new silence timer (SilenceSec)
func (m *Manager) OnOutput(shellID string) {
	m.mu.RLock()
	rulesMap := m.shellRules[shellID]
	if len(rulesMap) == 0 {
		m.mu.RUnlock()
		return
	}
	// Copy slice under read lock to iterate safely
	rules := make([]*Rule, 0, len(rulesMap))
	for _, r := range rulesMap {
		rules = append(rules, r)
	}
	m.mu.RUnlock()

	for _, rule := range rules {
		rule.mu.Lock()
		if rule.stopped {
			rule.mu.Unlock()
			continue
		}

		switch rule.Event {
		case EventOutput:
			// Action 1: leading edge. Throttle at the source so a fast producer
			// (e.g. 4 KiB chunks from a busy build) cannot spawn one goroutine per
			// chunk; the global cooldown gate still decides the actual send.
			if time.Since(rule.lastLeading) >= m.cooldown {
				rule.lastLeading = time.Now()
				go m.dispatchWithCooldown(rule, EventOutput, "running", false)
			}

			// Action 2: reset the trailing-edge timer (2s after output stops).
			if rule.timer == nil {
				ruleCopy := rule
				rule.timer = time.AfterFunc(m.trailingSec, func() {
					m.dispatchWithCooldown(ruleCopy, EventOutput, "running", false)
				})
			} else {
				rule.timer.Reset(m.trailingSec)
			}

		case EventSilence:
			// Reset the silence timer (one-shot once output stays quiet long enough).
			delay := time.Duration(rule.SilenceSec) * time.Second
			if delay <= 0 {
				delay = 3 * time.Second
			}
			if rule.timer == nil {
				ruleCopy := rule
				rule.timer = time.AfterFunc(delay, func() {
					m.dispatchWithCooldown(ruleCopy, EventSilence, "running", true)
				})
			} else {
				rule.timer.Reset(delay)
			}
		}
		rule.mu.Unlock()
	}
}

// OnExit is called when a shell exits or is aborted.
// It executes One-Shot notification for EventExit rules, followed by automatic cleanup.
func (m *Manager) OnExit(shellID string, exitCode *int) {
	m.mu.RLock()
	rulesMap := m.shellRules[shellID]
	rules := make([]*Rule, 0, len(rulesMap))
	for _, r := range rulesMap {
		rules = append(rules, r)
	}
	m.mu.RUnlock()

	status := "exited"
	if exitCode != nil {
		status = fmt.Sprintf("exited with code %d", *exitCode)
	}

	for _, rule := range rules {
		rule.mu.Lock()
		if rule.stopped {
			rule.mu.Unlock()
			continue
		}
		isExitRule := rule.Event == EventExit
		rule.mu.Unlock()

		if isExitRule {
			// One-shot exit notification. Dispatch on its own goroutine: a sampling
			// request blocks until the client answers (up to the 10s timeout), and
			// the exit watcher must not be stalled — the DEAD transition and UI
			// refresh follow this call.
			r := rule
			go m.dispatchWithCooldown(r, EventExit, status, true)
		}
	}

	// Clean up all remaining rules for this shell
	m.ClearShell(shellID)
}

// dispatchWithCooldown handles the final cooldown gate check and dispatches the notification.
// If oneShot is true, the rule is automatically unregistered.
func (m *Manager) dispatchWithCooldown(rule *Rule, event Event, status string, oneShot bool) {
	if oneShot {
		defer m.Unregister(rule.ID)
	}

	m.mu.Lock()
	now := time.Now()
	if now.Sub(m.lastSentAt) < m.cooldown {
		elapsed := now.Sub(m.lastSentAt)
		m.mu.Unlock()
		slog.Debug("notification dropped by cooldown gate", "shell_id", rule.ShellID, "channel", rule.Channel, "elapsed", elapsed)
		return
	}
	m.lastSentAt = now
	m.mu.Unlock()

	if m.sender == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch rule.Channel {
	case ChannelResource:
		if err := m.sender.SendResourceNotification(ctx, rule.ShellID); err != nil {
			slog.Debug("failed to send resource notification", "shell_id", rule.ShellID, "err", err)
		}
	case ChannelSampling:
		if err := m.sender.SendSamplingNotification(ctx, rule.ShellID, rule.Target, event, status); err != nil {
			slog.Debug("failed to send sampling notification", "shell_id", rule.ShellID, "err", err)
		}
	}
}
