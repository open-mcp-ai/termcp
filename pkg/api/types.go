package api

import (
	"time"
)

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	SessionRunning  SessionStatus = "running"
	SessionExited   SessionStatus = "exited"
	SessionError    SessionStatus = "error"
	SessionArchived SessionStatus = "archived"
)

// SessionMode represents the execution mode for a session.
type SessionMode string

const (
	ModePTY  SessionMode = "pty"
	ModePipe SessionMode = "pipe"
)

// Session holds metadata for an interactive process session.
type Session struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Command   string        `json:"command"`
	Args      []string      `json:"args"`
	Mode      SessionMode   `json:"mode"`   // "pty" | "pipe"
	Status    SessionStatus `json:"status"` // running | exited | error | archived
	ExitCode  *int          `json:"exit_code"`
	PID       int           `json:"pid"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Rows      int           `json:"rows"`
	Cols      int           `json:"cols"`
	// SSHEndpoint is a coarse hint for clients: "internal" (built-in loopback SSH) or "remote" (no host/user/port exposed).
	SSHEndpoint string `json:"ssh_endpoint,omitempty"`
	// Shells is a per-shell metadata snapshot, populated only when the session is
	// persisted/restored so a DEAD session can still render its tabs after a
	// restart. Never set on a live running session's Info().
	Shells []Session `json:"shells,omitempty"`
}

// MsgType classifies a message in a session.
type MsgType string

const (
	MsgInput  MsgType = "input"
	MsgOutput MsgType = "output"
	MsgSystem MsgType = "system"
)

// Message represents a single input/output record within a session.
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	ShellID   string    `json:"shell_id,omitempty"`
	Type      MsgType   `json:"type"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	ByteSize  int       `json:"byte_size"`
}

// MessageIndexEntry is a lightweight reference stored in the index file.
type MessageIndexEntry struct {
	ID        string    `json:"id"`
	ShellID   string    `json:"shell_id,omitempty"`
	Type      MsgType   `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	ByteSize  int       `json:"byte_size"`
}

// ArchiveReason classifies why a session was archived.
type ArchiveReason string

const (
	ArchiveExplicit ArchiveReason = "explicit" // user/AI requested terminate
	ArchiveCrash    ArchiveReason = "crash"    // abnormal SSH disconnect detected
	ArchiveExited   ArchiveReason = "exited"   // normal shell/process exit
	ArchiveShutdown ArchiveReason = "shutdown" // termcp server shutdown
)

// ArchivedSession is a retained record of a finished session. It embeds the
// session metadata snapshot plus writeup-oriented annotations. Messages live
// under data-dir/messages/{ID} independent of this record and survive restarts.
type ArchivedSession struct {
	Session
	Shells []Session     `json:"shells,omitempty"`
	Notes  string        `json:"notes,omitempty"`
	Tags   []string      `json:"tags,omitempty"`
	Reason ArchiveReason `json:"reason,omitempty"`
}
