package history

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func newTestManager(t *testing.T) (*Manager, *storage.Store) {
	t.Helper()
	store := storage.New(t.TempDir())
	m := New(store)
	if err := m.Load(); err != nil {
		t.Fatal(err)
	}
	return m, store
}

func sampleSession(id, name string) api.ArchivedSession {
	return api.ArchivedSession{
		Session: api.Session{ID: id, Name: name, Status: api.SessionArchived, CreatedAt: time.Now().UTC()},
	}
}

func seedMessages(t *testing.T, store *storage.Store, sid string, msgs []api.Message) {
	t.Helper()
	var entries []api.MessageIndexEntry
	for _, m := range msgs {
		if err := store.SaveMessage(sid, m); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, api.MessageIndexEntry{ID: m.ID, ShellID: m.ShellID, Type: m.Type, CreatedAt: m.CreatedAt, ByteSize: len(m.Content)})
	}
	if err := store.SaveMessageIndex(sid, entries); err != nil {
		t.Fatal(err)
	}
}

func TestManager_OutputByteRange(t *testing.T) {
	m, store := newTestManager(t)
	_ = m.Add(sampleSession("s1", "build"))
	seedMessages(t, store, "s1", []api.Message{
		{ID: "i1", SessionID: "s1", Type: api.MsgInput, Content: "run build\n"},
		{ID: "o1", SessionID: "s1", Type: api.MsgOutput, Content: "alpha\n"},
		{ID: "o2", SessionID: "s1", Type: api.MsgOutput, Content: "beta\ngamma\n", CreatedAt: time.Now().UTC().Add(time.Second)},
		{ID: "o3", SessionID: "s1", ShellID: "shell-2", Type: api.MsgOutput, Content: "delta\n", CreatedAt: time.Now().UTC().Add(2 * time.Second)},
	})

	// Merged stream: only MsgOutput, in append order.
	data, total, err := m.OutputByteRange("s1", "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\nbeta\ngamma\ndelta\n"
	if string(data) != want || total != int64(len(want)) {
		t.Fatalf("merged stream: got %q total=%d, want %q total=%d", data, total, want, len(want))
	}

	// Per-shell filter.
	data, total, err = m.OutputByteRange("s1", "shell-2", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "delta\n" || total != 6 {
		t.Fatalf("shell-2 stream: got %q total=%d", data, total)
	}

	// Byte window [6, 11) slices exactly "beta\n".
	data, total, err = m.OutputByteRange("s1", "", 6, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "beta\n" || total != int64(len(want)) {
		t.Fatalf("window: got %q total=%d", data, total)
	}

	// A window spanning message boundaries reassembles seamlessly.
	data, _, err = m.OutputByteRange("s1", "", 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "a\nbeta\ngam" {
		t.Fatalf("spanning window: got %q", data)
	}

	// Out-of-range start yields empty data but the true total.
	data, total, err = m.OutputByteRange("s1", "", 999, 10)
	if err != nil || data != nil || total != int64(len(want)) {
		t.Fatalf("out-of-range: data=%q total=%d err=%v", data, total, err)
	}

	// Unknown session: empty.
	data, total, err = m.OutputByteRange("ghost", "", 0, 0)
	if err != nil || data != nil || total != 0 {
		t.Fatalf("ghost: data=%q total=%d err=%v", data, total, err)
	}
}

func TestManager_AddListGet(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Add(sampleSession("s1", "CTF-A")); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(sampleSession("s2", "CTF-B")); err != nil {
		t.Fatal(err)
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 records, got %d", len(list))
	}
	got, ok := m.Get("s2")
	if !ok || got.Name != "CTF-B" {
		t.Fatalf("unexpected Get result: ok=%v got=%+v", ok, got)
	}
	if _, ok = m.Get("nope"); ok {
		t.Fatal("expected Get of missing id to be false")
	}
}

func TestManager_AddReplacesSameID(t *testing.T) {
	m, _ := newTestManager(t)
	_ = m.Add(sampleSession("s1", "old"))
	a := sampleSession("s1", "new")
	a.Notes = "updated"
	if err := m.Add(a); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("s1")
	if got.Name != "new" || got.Notes != "updated" {
		t.Fatalf("expected replaced record, got %+v", got)
	}
	if len(m.List()) != 1 {
		t.Fatalf("expected 1 record after upsert, got %d", len(m.List()))
	}
}

func TestManager_UpdateRenameNotesTags(t *testing.T) {
	m, _ := newTestManager(t)
	_ = m.Add(sampleSession("s1", "orig"))

	name := "renamed"
	tags := []string{"web", "pwn"}
	if err := m.Update("s1", &name, nil, &tags); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get("s1")
	if got.Name != "renamed" || len(got.Tags) != 2 {
		t.Fatalf("unexpected update result: %+v", got)
	}

	// Nil fields are left untouched.
	if err := m.Update("s1", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = m.Get("s1")
	if got.Name != "renamed" || len(got.Tags) != 2 {
		t.Fatalf("nil update must not clobber existing fields: %+v", got)
	}

	if err := m.Update("missing", &name, nil, nil); err == nil {
		t.Fatal("expected error updating missing session")
	}
}

func TestManager_DeleteRemovesRecordAndMessages(t *testing.T) {
	m, store := newTestManager(t)
	_ = m.Add(sampleSession("s1", "CTF"))
	m1 := api.Message{ID: "mm1", SessionID: "s1", Type: api.MsgOutput, Content: "hello"}
	m2 := api.Message{ID: "mm2", SessionID: "s1", Type: api.MsgInput, Content: "ls -la"}
	seedMessages(t, store, "s1", []api.Message{m1, m2})

	if !store.MessageDirExists("s1") {
		t.Fatal("expected message dir present before delete")
	}

	if err := m.Delete("s1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("s1"); ok {
		t.Fatal("expected record removed")
	}
	if store.MessageDirExists("s1") {
		t.Fatal("expected message dir removed after delete")
	}
}

func TestManager_DeleteUnknownStillClearsMessages(t *testing.T) {
	m, store := newTestManager(t)
	// Delete a session ID that was never archived but has message files on disk:
	// the files must still be cleared.
	msg := api.Message{ID: "mm1", SessionID: "ghost", Type: api.MsgOutput, Content: "x"}
	store.SaveMessage("ghost", msg) //nolint:errcheck
	if err := m.Delete("ghost"); err != nil {
		t.Fatal(err)
	}
	if store.MessageDirExists("ghost") {
		t.Fatal("expected orphaned message files to be cleared")
	}
}

func TestManager_TranscriptTextAndMarkdownAndHTML(t *testing.T) {
	m, store := newTestManager(t)
	_ = m.Add(sampleSession("s1", "CTF"))
	base := time.Now().UTC().Add(-time.Minute)
	msgs := []api.Message{
		{ID: "a", SessionID: "s1", Type: api.MsgInput, Content: "ls -la\n", CreatedAt: base},
		{ID: "b", SessionID: "s1", Type: api.MsgOutput, Content: "file.txt\n", CreatedAt: base.Add(time.Second)},
		{ID: "c", SessionID: "s1", Type: api.MsgSystem, Content: "boot", CreatedAt: base.Add(2 * time.Second)},
	}
	seedMessages(t, store, "s1", msgs)

	text, err := m.Transcript("s1", "text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "$ ls -la") || !strings.Contains(text, "file.txt") {
		t.Fatalf("unexpected text transcript:\n%s", text)
	}
	if strings.Contains(text, "boot") {
		t.Fatalf("system messages should be omitted from transcripts:\n%s", text)
	}

	md, err := m.Transcript("s1", "markdown")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md, "$ ls -la") {
		t.Fatalf("unexpected markdown transcript:\n%s", md)
	}

	html, err := m.Transcript("s1", "html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, "<div class='tcp-in'>") || !strings.Contains(html, "ls -la") {
		t.Fatalf("unexpected html transcript:\n%s", html)
	}
	if !strings.Contains(html, "<div class='tcp-out'>file.txt</div>") {
		t.Fatalf("unexpected html output div:\n%s", html)
	}
}

func TestManager_Search(t *testing.T) {
	m, store := newTestManager(t)
	_ = m.Add(sampleSession("s1", "CTF-A"))
	m2 := sampleSession("s2", "CTF-B")
	_ = m.Add(m2)

	msgs := []api.Message{
		{ID: "a", SessionID: "s1", Type: api.MsgOutput, Content: "the flag is in /root/flag.txt", CreatedAt: time.Now().UTC()},
	}
	seedMessages(t, store, "s1", msgs)
	msgs2 := []api.Message{
		{ID: "b", SessionID: "s2", Type: api.MsgOutput, Content: "nothing here", CreatedAt: time.Now().UTC()},
	}
	seedMessages(t, store, "s2", msgs2)

	hits := m.Search("flag.txt", 0)
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d: %+v", len(hits), hits)
	}
	if hits[0].SessionID != "s1" || hits[0].Name != "CTF-A" {
		t.Fatalf("unexpected hit: %+v", hits[0])
	}

	if len(m.Search("nomatch", 0)) != 0 {
		t.Fatal("expected no matches")
	}
	if len(m.Search("flag.txt", 1)) != 1 {
		t.Fatal("limit=1 should cap results")
	}
	// Empty/whitespace query returns nothing.
	if len(m.Search("   ", 0)) != 0 {
		t.Fatal("expected no hits for blank query")
	}
}

func TestManager_TranscriptEmpty(t *testing.T) {
	m, _ := newTestManager(t)
	_ = m.Add(sampleSession("s1", "CTF"))
	out, err := m.Transcript("s1", "text")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("expected empty transcript, got %q", out)
	}
}

func TestSnippet_UnicodeBoundarySafe(t *testing.T) {
	// 中文/日文 bytes are multi-byte; snippet must not split a rune.
	content := "目标 服务器 端口=8080 flag{hello} 你好世界"
	q := "flag"
	low := strings.ToLower(content)
	idx := strings.Index(low, q)
	if idx < 0 {
		t.Fatal("query not found in fixture")
	}
	snip := snippet(content, idx, 40)
	if snip == "" {
		t.Fatal("expected non-empty snippet")
	}
	if !strings.Contains(snip, q) {
		t.Fatalf("snippet must contain the match, got %q", snip)
	}
	// The snippet must be valid UTF-8 (no split rune producing replacement chars).
	if strings.ContainsRune(snip, '�') {
		t.Fatalf("snippet contains replacement char (rune split), got %q", snip)
	}
}

func TestRuneIndex(t *testing.T) {
	s := "ab中文é"
	if got := runeIndex(s, 0); got != 0 {
		t.Fatalf("expected 0, got %d", got)
	}
	if got := runeIndex(s, 1); got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
	if got := runeIndex(s, 2); got != 2 {
		t.Fatalf("expected 2 (start of 中), got %d", got)
	}
	// Offset landing mid-rune returns -1.
	if got := runeIndex(s, 3); got != -1 {
		t.Fatalf("expected -1 for split rune, got %d", got)
	}
	if got := runeIndex(s, len(s)); got != len([]rune(s)) {
		t.Fatalf("expected end rune count, got %d", got)
	}
}
