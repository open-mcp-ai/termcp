package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Lock ordering: mu -> stdinMu. Never acquire in reverse order.

// Config holds parameters for creating a new Session.
type Config struct {
	Command string
	Args    []string
	Mode    api.SessionMode
	Name    string
	Rows    int
	Cols    int
}

// Session wraps an interactive process session managed over SSH.
type Session struct {
	api.Session
	mu            sync.RWMutex
	stdinMu       sync.Mutex
	terminateOnce sync.Once
	exitOnce      sync.Once
	execSession   *sshclient.ExecSession
	sftpConn      *sshclient.SFTPConn
	sftpClose     chan struct{}
	sftpCloseOnce sync.Once
	buf           *buffer.Buffer
	readerID      int
	msgMgr        *message.Manager
	sshAddr       string
	done          chan struct{}
}

// New creates and starts a new Session.
func New(sshAddr string, cfg Config, msgMgr *message.Manager) (*Session, error) {
	id := uuid.New().String()[:12]
	name := cfg.Name
	if name == "" {
		name = fmt.Sprintf("session-%s", id)
	}

	usePty := cfg.Mode == api.ModePTY
	execSession, err := sshclient.Start(sshAddr, cfg.Command, cfg.Args, usePty, cfg.Rows, cfg.Cols)
	if err != nil {
		return nil, err
	}

	buf := buffer.New(1024 * 1024)
	rid, _ := buf.NewReader()

	s := &Session{
		Session: api.Session{
			ID:        id,
			Name:      name,
			Command:   cfg.Command,
			Args:      cfg.Args,
			Mode:      cfg.Mode,
			Status:    api.SessionRunning,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
			Rows:      cfg.Rows,
			Cols:      cfg.Cols,
		},
		execSession: execSession,
		buf:         buf,
		readerID:    rid,
		msgMgr:      msgMgr,
		sshAddr:     sshAddr,
		done:        make(chan struct{}),
		sftpClose:   make(chan struct{}),
	}

	s.startReaders()

	sftpClient, err := sshclient.NewSFTPClient(sshAddr)
	if err != nil {
		execSession.Close()
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	s.sftpConn = sftpClient

	if msgMgr != nil {
		msgMgr.Append(s.ID, api.MsgSystem, "Process started")
	}
	slog.Debug("session started", "session_id", id, "command", cfg.Command)

	return s, nil
}

func (s *Session) pipeToBuffer(r io.Reader) {
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				s.buf.Write(data)
			}
			if err != nil {
				return
			}
			select {
			case <-s.done:
				return
			default:
			}
		}
	}()
}

func (s *Session) startReaders() {
	s.pipeToBuffer(s.execSession.Stdout)
	s.pipeToBuffer(s.execSession.Stderr)

	go func() {
		<-s.execSession.Done()
		close(s.done)
		s.exitOnce.Do(func() {
			s.mu.Lock()
			s.Status = api.SessionExited
			code := s.execSession.ExitCode()
			s.ExitCode = &code
			s.UpdatedAt = time.Now().UTC()
			s.mu.Unlock()
			s.buf.Close()
			if s.msgMgr != nil {
				s.msgMgr.Append(s.ID, api.MsgSystem, fmt.Sprintf("Process exited with code %d", code))
			}
			slog.Debug("session exited", "session_id", s.ID, "exit_code", code)
		})
		// Delay SFTP close so agent can download files after process exits.
		go func() {
			select {
			case <-time.After(60 * time.Second):
			case <-s.sftpClose:
			}
			s.mu.Lock()
			if s.sftpConn != nil {
				s.sftpConn.Close()
				s.sftpConn = nil
			}
			s.mu.Unlock()
		}()
	}()
}

// SendInput writes text to the process stdin.
func (s *Session) SendInput(text string, pressEnter bool) error {
	s.mu.RLock()
	running := s.Status == api.SessionRunning
	s.mu.RUnlock()
	if !running {
		return fmt.Errorf("process has %s, cannot send input", s.Status)
	}
	if pressEnter {
		if runtime.GOOS == "windows" {
			text += "\r\n"
		} else {
			text += "\n"
		}
	}
	s.stdinMu.Lock()
	_, err := s.execSession.Stdin.Write([]byte(text))
	s.stdinMu.Unlock()
	if err != nil {
		return err
	}
	if s.msgMgr != nil {
		s.msgMgr.Append(s.ID, api.MsgInput, text)
	}
	return nil
}

func (s *Session) readOutput(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int) (string, error) {
	data, err := s.buf.Read(ctx, readerID, timeout)
	if err != nil && err != io.EOF {
		return "", err
	}
	output := string(data)
	if stripAnsi {
		output = ansi.Strip(output)
		output = ansi.Compact(output)
	}
	if maxLines > 0 {
		lines := strings.Split(output, "\n")
		if len(lines) > maxLines {
			output = strings.Join(lines[:maxLines], "\n")
		}
	}
	if output != "" && s.msgMgr != nil {
		s.msgMgr.Append(s.ID, api.MsgOutput, output)
	}
	return output, nil
}

// ReadOutput reads new output using the default reader.
func (s *Session) ReadOutput(ctx context.Context, timeout time.Duration, stripAnsi bool, maxLines int) (string, error) {
	return s.readOutput(ctx, s.readerID, timeout, stripAnsi, maxLines)
}

// ReadOutputForReader reads new output for a specific reader ID.
func (s *Session) ReadOutputForReader(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int) (string, error) {
	return s.readOutput(ctx, readerID, timeout, stripAnsi, maxLines)
}

// Terminate gracefully or forcefully stops the process.
// The exit goroutine is the single authority for final Status/ExitCode.
func (s *Session) Terminate(force bool, gracePeriod time.Duration) {
	s.terminateOnce.Do(func() {
		if !force {
			s.execSession.Signal(ssh.SIGTERM)
			select {
			case <-s.execSession.Done():
				return
			case <-time.After(gracePeriod):
			}
		}

		s.execSession.Close()

		select {
		case <-s.execSession.Done():
		case <-time.After(2 * time.Second):
		}

		s.exitOnce.Do(func() {
			s.mu.Lock()
			s.Status = api.SessionExited
			code := -1
			s.ExitCode = &code
			s.UpdatedAt = time.Now().UTC()
			s.mu.Unlock()
			s.buf.Close()
			if s.msgMgr != nil {
				s.msgMgr.Append(s.ID, api.MsgSystem, "Process terminated (no exit code)")
			}
			slog.Debug("session terminated", "session_id", s.ID, "forced", force)
		})
	})
}

// ResizePty adjusts the terminal dimensions (pty mode only).
func (s *Session) ResizePty(rows, cols int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != api.SessionRunning {
		return fmt.Errorf("process not running")
	}
	if s.Mode != api.ModePTY {
		return fmt.Errorf("PTY resize only available in pty mode")
	}
	if err := s.execSession.ResizePty(rows, cols); err != nil {
		return err
	}
	s.Rows = rows
	s.Cols = cols
	return nil
}

// Info returns a deep copy of the session metadata.
func (s *Session) Info() api.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.Session
	if cp.ExitCode != nil {
		v := *cp.ExitCode
		cp.ExitCode = &v
	}
	return cp
}

// RegisterReader creates a new independent reader and returns its ID.
func (s *Session) RegisterReader() (int, error) {
	return s.buf.NewReader()
}

// UnregisterReader removes a reader by ID.
func (s *Session) UnregisterReader(id int) {
	s.buf.Unregister(id)
}

// HasMoreOutput returns whether the given reader has unread data.
func (s *Session) HasMoreOutput(readerID int) bool {
	return s.buf.HasMore(readerID)
}

const maxFileSize = 1 << 20 // 1MB — keeps transfers within MCP message bounds

// getSFTPClient returns the SFTP client or an error if unavailable.
func (s *Session) getSFTPClient() (*sftp.Client, error) {
	s.mu.RLock()
	conn := s.sftpConn
	s.mu.RUnlock()
	if conn == nil {
		return nil, fmt.Errorf("SFTP not available (session closed)")
	}
	return conn.Client, nil
}

// UploadFile decodes base64 content and writes it to the container filesystem.
func (s *Session) UploadFile(contentBase64 string, remotePath string) (int, error) {
	sc, err := s.getSFTPClient()
	if err != nil {
		return 0, err
	}

	if len(contentBase64) > base64.StdEncoding.EncodedLen(maxFileSize) {
		return 0, fmt.Errorf("file too large (max %d bytes). Use shell commands (curl/wget) for large files", maxFileSize)
	}

	data, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return 0, fmt.Errorf("content_base64: %w", err)
	}
	if len(data) > maxFileSize {
		return 0, fmt.Errorf("file too large (%d bytes, max %d). Use shell commands (curl/wget) for large files", len(data), maxFileSize)
	}

	dir := filepath.Dir(remotePath)
	if dir != "." && dir != "/" {
		if err := sc.MkdirAll(dir); err != nil {
			return 0, fmt.Errorf("create directory %q: %w", dir, err)
		}
	}

	f, err := sc.Create(remotePath)
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", remotePath, err)
	}
	defer f.Close()

	n, err := f.Write(data)
	if err != nil {
		return 0, fmt.Errorf("write %q: %w", remotePath, err)
	}
	return n, nil
}

// FileEntry represents a file or directory in a listing.
type FileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime string `json:"mod_time"`
}

// DownloadResult represents a downloaded file's content.
type DownloadResult struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"` // "text" or "base64"
	Size     int    `json:"size"`
}

// DownloadFile retrieves a file from the container. Binary content is
// base64-encoded; text is returned verbatim to save tokens.
func (s *Session) DownloadFile(remotePath string) (*DownloadResult, error) {
	sc, err := s.getSFTPClient()
	if err != nil {
		return nil, err
	}

	stat, err := sc.Stat(remotePath)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", remotePath, err)
	}
	if stat.Size() > int64(maxFileSize) {
		return nil, fmt.Errorf("file too large (%d bytes, max %d). Use shell commands to transfer", stat.Size(), maxFileSize)
	}

	f, err := sc.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", remotePath, err)
	}
	defer f.Close()

	data := make([]byte, stat.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, fmt.Errorf("read %q: %w", remotePath, err)
	}

	if bytes.IndexByte(data, 0) < 0 {
		return &DownloadResult{Content: string(data), Encoding: "text", Size: len(data)}, nil
	}
	return &DownloadResult{Content: base64.StdEncoding.EncodeToString(data), Encoding: "base64", Size: len(data)}, nil
}

// ListFiles enumerates a remote directory so the agent can discover
// files without shell commands.
func (s *Session) ListFiles(remotePath string) ([]FileEntry, error) {
	sc, err := s.getSFTPClient()
	if err != nil {
		return nil, err
	}

	entries, err := sc.ReadDir(remotePath)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", remotePath, err)
	}

	result := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, FileEntry{
			Name:    e.Name(),
			Size:    e.Size(),
			IsDir:   e.IsDir(),
			ModTime: e.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return result, nil
}

// CloseSFTP tears down the SFTP connection early instead of waiting
// for the 60-second delayed close.
func (s *Session) CloseSFTP() {
	s.sftpCloseOnce.Do(func() { close(s.sftpClose) })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sftpConn != nil {
		s.sftpConn.Close()
		s.sftpConn = nil
	}
}
