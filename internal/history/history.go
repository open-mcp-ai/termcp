package history

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager manages the retained record of finished ("archived") sessions plus
// their on-disk message history. Records live in data-dir/history.json;
// message payloads live under data-dir/messages/{id}/. Both survive restarts.
type Manager struct {
	store *storage.Store
	mu    sync.RWMutex
	list  []api.ArchivedSession
}

func New(store *storage.Store) *Manager {
	return &Manager{store: store}
}

// Load reads persisted history into memory. Call once at boot.
func (m *Manager) Load() error {
	list, err := m.store.LoadHistory()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.list = list
	m.mu.Unlock()
	return nil
}

func (m *Manager) persistLocked() error {
	if m.store == nil {
		return nil
	}
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].CreatedAt.Before(m.list[j].CreatedAt) })
	return m.store.SaveHistory(m.list)
}

// List returns a copy of all archived sessions.
func (m *Manager) List() []api.ArchivedSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]api.ArchivedSession, len(m.list))
	copy(out, m.list)
	return out
}

// Get returns a copy of one archived session.
func (m *Manager) Get(id string) (api.ArchivedSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.list {
		if a.ID == id {
			return a, true
		}
	}
	return api.ArchivedSession{}, false
}

// Add upserts an archived session record (byte-for-byte replace by ID).
func (m *Manager) Add(a api.ArchivedSession) error {
	if a.ID == "" {
		return fmt.Errorf("history: archived session requires an ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID == a.ID {
			m.list[i] = a
			return m.persistLocked()
		}
	}
	m.list = append(m.list, a)
	return m.persistLocked()
}

// Update renames and/or annotates an archived session.
func (m *Manager) Update(id string, name *string, notes *string, tags *[]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.list {
		if m.list[i].ID != id {
			continue
		}
		if name != nil {
			m.list[i].Name = *name
		}
		if notes != nil {
			m.list[i].Notes = *notes
		}
		if tags != nil {
			m.list[i].Tags = append([]string(nil), (*tags)...)
		}
		return m.persistLocked()
	}
	return fmt.Errorf("history: session %q not found", id)
}

// Delete removes an archived session record and its on-disk messages. The
// message directory is cleared even when the session was never retained in the
// history list (e.g. purging a live session mid-flight), so destructive delete
// always fully erases the session.
func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	removed := false
	var kept []api.ArchivedSession
	for _, a := range m.list {
		if a.ID == id {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	m.list = kept
	m.mu.Unlock()

	if m.store != nil {
		if err := m.store.DeleteSessionMessages(id); err != nil {
			return err
		}
	}
	if !removed {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistLocked()
}

// Messages loads and returns all persisted messages for a session, in message
// order (index sequence, not strictly chronological).
func (m *Manager) Messages(id string) ([]api.Message, error) {
	if m.store == nil {
		return nil, nil
	}
	entries, err := m.store.LoadMessageIndex(id)
	if err != nil {
		return nil, err
	}
	var out []api.Message
	for _, e := range entries {
		msg, err := m.store.LoadMessage(id, e.ID)
		if err != nil {
			continue
		}
		out = append(out, *msg)
	}
	return out, nil
}

func promptFor(msg api.Message, prev *api.Message) string {
	if msg.Type == api.MsgInput {
		s := ansi.Strip(msg.Content)
		if strings.HasSuffix(s, "\n") {
			s = strings.TrimSuffix(s, "\n")
		}
		return s
	}
	return msg.Content
}

// Transcript renders an interleaved input/output timeline for a session.
// format: "text" | "markdown" | "html".
// When shellID is non-empty, only messages from that shell are rendered (an
// empty shell id keeps the whole merged stream, for downloads/screenshots).
func (m *Manager) Transcript(id string, format string, shellID ...string) (string, error) {
	msgs, err := m.Messages(id)
	if err != nil {
		return "", err
	}
	if len(msgs) == 0 {
		return "", nil
	}
	if len(shellID) > 0 && shellID[0] != "" {
		var kept []api.Message
		for _, msg := range msgs {
			if msg.ShellID == shellID[0] {
				kept = append(kept, msg)
			}
		}
		msgs = kept
		if len(msgs) == 0 {
			return "", nil
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].CreatedAt.Before(msgs[j].CreatedAt) })

	var b strings.Builder
	for _, msg := range msgs {
		content := ansi.Strip(msg.Content)
		content = strings.TrimRight(content, "\r\n")
		switch format {
		case "html":
			esc := htmlEscape(content)
			if msg.Type == api.MsgInput {
				fmt.Fprintf(&b, "<div class='tcp-in'><span class='tcp-prompt'>$ </span>%s</div>\n", esc)
			} else if msg.Type == api.MsgOutput {
				fmt.Fprintf(&b, "<div class='tcp-out'>%s</div>\n", esc)
			}
		case "markdown":
			if msg.Type == api.MsgInput {
				fmt.Fprintf(&b, "```\n$ %s\n```\n", content)
			} else if msg.Type == api.MsgOutput {
				if strings.TrimSpace(content) != "" {
					fmt.Fprintf(&b, "    %s\n", strings.ReplaceAll(content, "\n", "\n    "))
				}
			}
		default:
			if msg.Type == api.MsgInput {
				fmt.Fprintf(&b, "$ %s\n", content)
			} else if msg.Type == api.MsgOutput && strings.TrimSpace(content) != "" {
				b.WriteString(content)
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

// SearchHit is one matching message interval within a session.
type SearchHit struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Snippet   string `json:"snippet"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Search returns, per archived session, matches of query within message content.
func (m *Manager) Search(query string, limit int) []SearchHit {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	var hits []SearchHit
	for _, a := range m.List() {
		msgs, err := m.Messages(a.ID)
		if err != nil {
			continue
		}
		for _, msg := range msgs {
			content := ansi.Strip(msg.Content)
			low := strings.ToLower(content)
			idx := strings.Index(low, q)
			if idx < 0 {
				continue
			}
			snip := snippet(content, idx, 120)
			hits = append(hits, SearchHit{
				SessionID: a.ID,
				Name:      a.Name,
				Type:      string(msg.Type),
				Snippet:   snip,
			})
			if limit > 0 && len(hits) >= limit {
				return hits
			}
		}
	}
	return hits
}

func snippet(s string, center, radius int) string {
	idx := runeIndex(s, center)
	if idx < 0 {
		return strings.TrimSpace(s)
	}
	runes := []rune(s)
	half := radius / 2
	if idx > half {
		idx -= half
	} else {
		idx = 0
	}
	end := idx + radius
	if end > len(runes) {
		end = len(runes)
	}
	return strings.TrimSpace(string(runes[idx:end]))
}

// runeIndex returns the rune index of the byte offset, or -1 if offset splits a rune.
func runeIndex(s string, byteOffset int) int {
	runes := []rune(s)
	if byteOffset < 0 || byteOffset > len(s) {
		return -1
	}
	if byteOffset == len(s) {
		return len(runes)
	}
	pos := 0
	for i, r := range s {
		if i == byteOffset {
			return pos
		}
		pos++
		if i+len(string(r)) > byteOffset {
			return -1
		}
	}
	return -1
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
