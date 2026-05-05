package api

import (
	"time"
)

// SessionStatus represents the current state of a session.
type SessionStatus string

const (
	SessionRunning SessionStatus = "running"
	SessionExited  SessionStatus = "exited"
	SessionError   SessionStatus = "error"
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
	Mode      SessionMode   `json:"mode"`     // "pty" | "pipe"
	Status    SessionStatus `json:"status"`   // running | exited | error
	ExitCode  *int          `json:"exit_code"`
	PID       int           `json:"pid"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	Rows      int           `json:"rows"`
	Cols      int           `json:"cols"`
}

// MsgType classifies a message in a session.
type MsgType string

const (
	MsgInput   MsgType = "input"
	MsgOutput  MsgType = "output"
	MsgSystem  MsgType = "system"
)

// Message represents a single input/output record within a session.
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Type      MsgType   `json:"type"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	ByteSize  int       `json:"byte_size"`
}

// MessageIndexEntry is a lightweight reference stored in the index file.
type MessageIndexEntry struct {
	ID        string    `json:"id"`
	Type      MsgType   `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	ByteSize  int       `json:"byte_size"`
}
