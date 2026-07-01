package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/buffer"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/pkg/api"
	"golang.org/x/crypto/ssh"
)

// Lock ordering: mu -> stdinMu. Never acquire in reverse order.

// RemoteSSH selects a user-supplied SSH server instead of the built-in internal one.
// Jump, when non-nil, is a bastion (ProxyJump): the SSH connection to this host
// is tunneled through a direct-tcpip channel opened on the bastion's client.
// Jump chains recursively (Jump.Jump) for multi-hop.
type RemoteSSH struct {
	Host               string
	Port               int
	User               string
	Password           string
	PrivateKey         string
	KeyPassphrase      string
	TrustUnknownHost   bool
	KnownHosts         string
	DialTimeoutSeconds int
	Proxy              *sshclient.Proxy
	Jump               *RemoteSSH
}

// Config holds parameters for creating a new Session.
type Config struct {
	Command string
	Args    []string
	Mode    api.SessionMode
	Name    string
	Rows    int
	Cols    int
	Remote  *RemoteSSH
}

// Session wraps an interactive process session managed over SSH.
type Session struct {
	api.Session
	mu            sync.RWMutex
	stdinMu       sync.Mutex
	terminateOnce sync.Once
	exitOnce      sync.Once
	execSession   *sshclient.ExecSession
	buf           *buffer.Buffer
	readerID      int
	msgMgr         *message.Manager
	onExit         func()
	onChildChange  func() // called when child shells are added/removed

	primaryShell  *ChildShell           // wraps own execSession for unified shell lifecycle
	childShells   map[string]*ChildShell
	childShellsMu sync.RWMutex
}

// New creates and starts a new Session.
// internal must be the built-in sshserver.Server (after Start) when cfg.Remote is nil; it may be nil for remote-only callers.
func New(internal *sshserver.Server, cfg Config, msgMgr *message.Manager) (*Session, error) {
	id := uuid.New().String()[:12]
	name := cfg.Name
	if name == "" {
		name = fmt.Sprintf("session-%s", id)
	}

	usePty := cfg.Mode == api.ModePTY

	var execSession *sshclient.ExecSession
	var sshEndpointPublic string // "internal" | "remote" for MCP / JSON (no host or credentials)

	if cfg.Remote != nil && strings.TrimSpace(cfg.Remote.Host) != "" {
		r := cfg.Remote
		port := r.Port
		if port == 0 {
			port = 22
		}
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("ssh_port must be between 1 and 65535, got %d", port)
		}
		sshEndpointPublic = "remote"

		var err error
		if r.Jump != nil {
			client, closers, derr := buildChainClient(r)
			if derr != nil {
				return nil, derr
			}
			execSession, err = sshclient.StartWithChain(client, closers, cfg.Command, cfg.Args, usePty, cfg.Rows, cfg.Cols)
		} else {
			dialAddr := remoteDialAddr(r)
			clientCfg, cerr := remoteClientConfig(r)
			if cerr != nil {
				return nil, cerr
			}
			execSession, err = sshclient.StartWithConfig(dialAddr, clientCfg, r.Proxy, cfg.Command, cfg.Args, usePty, cfg.Rows, cfg.Cols)
		}
		if err != nil {
			return nil, err
		}
	} else {
		if internal == nil {
			return nil, errors.New("internal ssh server is not configured")
		}
		minted, err := internal.MintClientConfig()
		if err != nil {
			return nil, err
		}
		conn, err := internal.Dial()
		if err != nil {
			return nil, err
		}
		sshEndpointPublic = "internal"
		execSession, err = sshclient.StartWithConn(conn, minted, cfg.Command, cfg.Args, usePty, cfg.Rows, cfg.Cols)
		if err != nil {
			return nil, err
		}
	}

	buf := buffer.New(1024 * 1024)
	rid, _ := buf.NewReader()

	// Determine the target shell family so press_enter sends the right line
	// ending. For internal sessions the target is the local host, so we probe
	// it via the shell detector. For remote sessions the target OS is unknown
	// without an extra round-trip; unix (\n) is the safe default for SSH targets.
	enterCRLF := false
	if sshEndpointPublic == "internal" {
		if _, family, _ := shell.NewDetector().Detect(); family != "unix" {
			enterCRLF = true
		}
	}

	// Wrap the exec session in a ChildShell so root and child shells share the same lifecycle.
	primaryShell := &ChildShell{
		ID:          id,
		Name:        name,
		execSession: execSession,
		buf:         buf,
		done:        make(chan struct{}),
		Status:      api.SessionRunning,
		Rows:        cfg.Rows,
		Cols:        cfg.Cols,
		CreatedAt:   time.Now().UTC(),
		enterCRLF:   enterCRLF,
	}
	primaryShell.startReaders()

	s := &Session{
		Session: api.Session{
			ID:          id,
			Name:        name,
			Command:     cfg.Command,
			Args:        cfg.Args,
			Mode:        cfg.Mode,
			Status:      api.SessionRunning,
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
			Rows:        cfg.Rows,
			Cols:        cfg.Cols,
			SSHEndpoint: sshEndpointPublic,
		},
		execSession: execSession,
		buf:         buf,
		readerID:    rid,
		msgMgr:      msgMgr,
		primaryShell: primaryShell,
	}

	if msgMgr != nil {
		msgMgr.Append(s.ID, api.MsgSystem, "Process started")
	}
	slog.Debug("session started", "session_id", id, "command", cfg.Command, "ssh_endpoint", sshEndpointPublic)

	return s, nil
}

// remoteDialAddr returns host:port for a remote, defaulting port 22.
func remoteDialAddr(r *RemoteSSH) string {
	port := r.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(strings.TrimSpace(r.Host), strconv.Itoa(port))
}

// remoteDialTimeout clamps DialTimeoutSeconds to [30, 120] seconds.
func remoteDialTimeout(r *RemoteSSH) time.Duration {
	toSec := r.DialTimeoutSeconds
	if toSec <= 0 {
		toSec = 30
	}
	if toSec > 120 {
		toSec = 120
	}
	return time.Duration(toSec) * time.Second
}

// remoteClientConfig builds the per-hop SSH client config.
func remoteClientConfig(r *RemoteSSH) (*ssh.ClientConfig, error) {
	return sshclient.BuildClientConfig(sshclient.DialAuth{
		User:              strings.TrimSpace(r.User),
		Password:          r.Password,
		PrivateKey:        r.PrivateKey,
		KeyPassphrase:     r.KeyPassphrase,
		TrustUnknownHost:  r.TrustUnknownHost,
		KnownHostsContent: r.KnownHosts,
		DialTimeout:       remoteDialTimeout(r),
	})
}

// buildChainClient establishes the SSH client for r, recursing through r.Jump
// bastions (ProxyJump). The bastion's *ssh.Client.Dial opens a direct-tcpip
// channel to the next hop; the SSH handshake to each hop runs over that channel.
//
// Returns the final target client plus all intermediate bastion clients (closers)
// that must stay alive for the life of the session. On error, everything opened
// is cleaned up.
//
// Per-hop host-key verification happens locally at termcp; bastions only relay TCP.
// r.Proxy (socks5) only applies at the chain root (the deepest hop, dialed directly);
// non-root hops get their connection from the parent bastion's Dial, so their
// Proxy is ignored.
func buildChainClient(r *RemoteSSH) (*ssh.Client, []io.Closer, error) {
	addr := remoteDialAddr(r)
	cfg, err := remoteClientConfig(r)
	if err != nil {
		return nil, nil, err
	}

	if r.Jump == nil {
		conn, err := sshclient.DialConn(addr, r.Proxy, cfg.Timeout)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh dial %s: %w", addr, err)
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			conn.Close()
			return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
		}
		return ssh.NewClient(c, chans, reqs), nil, nil
	}

	bastion, subClosers, err := buildChainClient(r.Jump)
	if err != nil {
		return nil, nil, err
	}
	conn, err := bastion.Dial("tcp", addr)
	if err != nil {
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("bastion dial %s: %w", addr, err)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		bastion.Close()
		sshclient.DrainClosers(subClosers)
		return nil, nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}
	closers := append(subClosers, io.Closer(bastion))
	return ssh.NewClient(c, chans, reqs), closers, nil
}

// SendInput writes text to the process stdin and records it in the message log.
func (s *Session) SendInput(text string, pressEnter bool) error {
	return s.sendInput([]byte(text), pressEnter, true)
}

// SendTerminalBytes writes raw keystrokes to stdin without appending to the message log (web UI).
func (s *Session) SendTerminalBytes(data []byte, pressEnter bool) error {
	return s.primaryShell.SendTerminalBytes(data, pressEnter)
}

// appendEnter returns data with the line ending appropriate for the shell family.
func appendEnter(data []byte, crlf bool) []byte {
	if crlf {
		return append(append([]byte(nil), data...), '\r', '\n')
	}
	return append(append([]byte(nil), data...), '\n')
}

func (s *Session) sendInput(data []byte, pressEnter bool, persist bool) error {
	s.mu.RLock()
	running := s.Status == api.SessionRunning
	s.mu.RUnlock()
	if !running {
		return fmt.Errorf("process has %s, cannot send input", s.Status)
	}
	crlf := s.primaryShell.enterCRLF
	var toWrite []byte
	if pressEnter {
		toWrite = appendEnter(data, crlf)
	} else {
		toWrite = data
	}
	s.stdinMu.Lock()
	_, err := s.execSession.Stdin.Write(toWrite)
	s.stdinMu.Unlock()
	if err != nil {
		return err
	}
	if persist && s.msgMgr != nil {
		var logged string
		if pressEnter {
			if crlf {
				logged = string(data) + "\r\n"
			} else {
				logged = string(data) + "\n"
			}
		} else {
			logged = string(data)
		}
		s.msgMgr.Append(s.ID, api.MsgInput, logged)
	}
	return nil
}

func (s *Session) readOutput(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, persist bool, maxBytes int) (string, error) {
	data, err := s.buf.Read(ctx, readerID, timeout, maxBytes)
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
	if output != "" && persist && s.msgMgr != nil {
		s.msgMgr.Append(s.ID, api.MsgOutput, output)
	}
	return output, nil
}

// ReadOutput reads new output using the default reader.
// maxBytes limits the returned output in bytes; 0 means no limit.
func (s *Session) ReadOutput(ctx context.Context, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	return s.readOutput(ctx, s.readerID, timeout, stripAnsi, maxLines, true, maxBytes)
}

// ReadOutputForReader reads new output for a specific reader ID.
// maxBytes limits the returned output in bytes; 0 means no limit.
func (s *Session) ReadOutputForReader(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	return s.readOutput(ctx, readerID, timeout, stripAnsi, maxLines, true, maxBytes)
}

// ReadTerminalStream reads PTY output for a reader without appending to the message log (high-frequency UI streaming).
// If maxBytes > 0, each call returns at most that many raw bytes (for WebSocket/SSE chunking); 0 means one full drain to end of buffer.
func (s *Session) ReadTerminalStream(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	return s.primaryShell.ReadTerminalStream(ctx, readerID, timeout, stripAnsi, maxLines, maxBytes)
}

// OutputByteRange returns a copy of retained raw output bytes [start, start+max) and total retained length.
func (s *Session) OutputByteRange(start int64, max int) ([]byte, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf == nil {
		return nil, 0, fmt.Errorf("output buffer unavailable")
	}
	data, total := s.buf.ByteRange(start, max)
	return data, total, nil
}

// BufferLen returns retained raw output length in bytes (for tail slicing).
func (s *Session) BufferLen() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.buf == nil {
		return 0
	}
	return s.buf.Len()
}

// Terminate gracefully or forcefully stops the process.
// The exit goroutine is the single authority for final Status/ExitCode.
func (s *Session) Terminate(force bool, gracePeriod time.Duration) {
	s.terminateOnce.Do(func() {
		if !force {
			s.execSession.Signal(ssh.SIGTERM)
			select {
			case <-s.execSession.Done():
				s.terminateChildren()
				return
			case <-time.After(gracePeriod):
			}
		}

		// Terminate child shells, then close the session channel.
		// For remote sessions the SSH client stays alive for multiplexing (Disconnect() closes it).
		// For internal sessions there's no multiplexing, so close everything.
		s.terminateChildren()
		if s.SSHEndpoint == "internal" {
			s.execSession.Close()
		} else {
			s.execSession.CloseSessionOnly()
		}

		select {
		case <-s.execSession.Done():
		case <-time.After(2 * time.Second):
		}

		// Remote sessions: exit processing skipped (connection stays alive).
		// Internal sessions: normal exit processing.
		if s.SSHEndpoint == "internal" {
			s.exitOnce.Do(func() {
				code := -1
				s.markExited(&code, "Process terminated (no exit code)", "session terminated")
			})
		}
	})
}

// markExited transitions the session to SessionExited, closes its output buffer,
// optionally appends a system message, logs, and invokes the onExit callback.
// exitCode is stored when non-nil (nil leaves it unset). Must run inside s.exitOnce.
func (s *Session) markExited(exitCode *int, sysMsg, logLabel string) {
	s.mu.Lock()
	s.Status = api.SessionExited
	if exitCode != nil {
		v := *exitCode
		s.ExitCode = &v
	}
	s.UpdatedAt = time.Now().UTC()
	s.mu.Unlock()
	s.buf.Close()
	if sysMsg != "" && s.msgMgr != nil {
		s.msgMgr.Append(s.ID, api.MsgSystem, sysMsg)
	}
	logArgs := []any{"session_id", s.ID}
	if exitCode != nil {
		logArgs = append(logArgs, "exit_code", *exitCode)
	}
	slog.Debug(logLabel, logArgs...)
	if fn := s.onExit; fn != nil {
		fn()
	}
}

// TerminateShellOnly closes the root tab shell channel. For internal sessions this is a no-op
// — the process outlives the tab (like detaching from screen/tmux). For remote sessions only the
// SSH session channel is closed; child shells on the same TCP connection are unaffected.
func (s *Session) TerminateShellOnly() {
	if s.SSHEndpoint == "internal" {
		return // tab close doesn't kill the process
	}
	s.primaryShell.TerminateShell()
}

// Disconnect closes the underlying SSH client and marks the session as fully exited.
func (s *Session) Disconnect() {
	s.exitOnce.Do(func() {
		s.markExited(nil, "", "session disconnected")
	})
	if s.execSession != nil {
		if cli := s.execSession.SSHClient(); cli != nil {
			cli.Close()
		}
	}
}

// terminateChildren closes all child shells. Called during parent session termination.
// Holds write lock long enough to snapshot+clear the map, preventing races with Create/Close.
func (s *Session) terminateChildren() {
	s.childShellsMu.Lock()
	children := make([]*ChildShell, 0, len(s.childShells))
	for k, cs := range s.childShells {
		children = append(children, cs)
		delete(s.childShells, k)
	}
	s.childShellsMu.Unlock()

	for _, cs := range children {
		cs.TerminateShell()
	}
}

// ResizePty adjusts the terminal dimensions (pty mode only).
func (s *Session) ResizePty(rows, cols int) error {
	if s.Mode != api.ModePTY {
		return fmt.Errorf("PTY resize only available in pty mode")
	}
	if err := s.primaryShell.ResizePty(rows, cols); err != nil {
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

// DefaultOutputReaderID is the first ring-buffer reader created with the session.
// MCP read_output defaults to this ID. The Web UI loads older bytes via GET /output-range and streams new output with RegisterReader().
func (s *Session) DefaultOutputReaderID() int {
	return s.readerID
}

// RegisterReaderSeededFromDefault registers a new output reader seeded from the default reader
// (atomic under buffer lock) so the web stream does not compete with MCP read_output on reader 0.
func (s *Session) RegisterReaderSeededFromDefault() (int, error) {
	return s.buf.NewReaderSeededFrom(s.readerID)
}

// RegisterReaderFromBufferStart registers a reader at the start of the retained transcript
// so Web UI / SSE clients replay full in-memory scrollback after reconnect.
func (s *Session) RegisterReaderFromBufferStart() (int, error) {
	return s.buf.NewReaderFromStart()
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
// SSHClient returns the underlying SSH client for remote sessions, or nil for internal/loopback.
func (s *Session) SSHClient() *ssh.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.execSession == nil {
		return nil
	}
	return s.execSession.SSHClient()
}

func (s *Session) HasMoreOutput(readerID int) bool {
	return s.buf.HasMore(readerID)
}

func (s *Session) IsBufferClosed() bool {
	return s.buf.IsClosed()
}

// TerminalShell is the interface used by WebSocket/REST handlers for terminal I/O.
// Both *Session and *ChildShell implement this interface.
type TerminalShell interface {
	Info() api.Session
	SendTerminalBytes(data []byte, pressEnter bool) error
	ResizePty(rows, cols int) error
	RegisterReader() (int, error)
	RegisterReaderFromBufferStart() (int, error)
	UnregisterReader(id int)
	ReadTerminalStream(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error)
	HasMoreOutput(readerID int) bool
	IsBufferClosed() bool
	OutputByteRange(start int64, max int) ([]byte, int64, error)
	BufferLen() int64
}

// ChildShell is a lightweight shell channel sharing the parent Session's SSH connection.
// It implements TerminalShell so it can be used interchangeably with *Session in WebSocket handlers.
type ChildShell struct {
	ID          string
	Name        string
	parent      *Session // nil for the root shell (primaryShell), set for child shells
	execSession *sshclient.ExecSession
	buf         *buffer.Buffer
	done        chan struct{}
	closeOnce   sync.Once
	cleanupOnce sync.Once // guards removeChildShell to prevent double push from explicit close + natural exit
	mu          sync.RWMutex
	stdinMu     sync.Mutex
	Status      api.SessionStatus
	CreatedAt   time.Time
	ExitCode    *int
	Rows        int
	Cols        int
	// enterCRLF selects the byte sequence appended on press_enter: true.
	// It reflects the target shell family (unix vs cmd/powershell), not the
	// termcp host OS, so cross-OS SSH sessions send the right line ending.
	enterCRLF bool
}

// Info returns a snapshot of the child shell's public metadata.
func (cs *ChildShell) Info() api.Session {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	s := api.Session{
		ID:        cs.ID,
		Name:      cs.Name,
		Mode:      api.ModePTY,
		Status:    cs.Status,
		Rows:      cs.Rows,
		Cols:      cs.Cols,
		CreatedAt: cs.CreatedAt,
		UpdatedAt: time.Now().UTC(),
	}
	if cs.ExitCode != nil {
		v := *cs.ExitCode
		s.ExitCode = &v
	}
	return s
}

// Done returns a channel that closes when the child shell process exits.
func (cs *ChildShell) Done() <-chan struct{} {
	return cs.done
}

// SendTerminalBytes writes raw keystrokes to the child shell's stdin.
func (cs *ChildShell) SendTerminalBytes(data []byte, pressEnter bool) error {
	cs.mu.RLock()
	running := cs.Status == api.SessionRunning
	cs.mu.RUnlock()
	if !running {
		return fmt.Errorf("process has %s, cannot send input", cs.Status)
	}
	var toWrite []byte
	if pressEnter {
		toWrite = appendEnter(data, cs.enterCRLF)
	} else {
		toWrite = data
	}
	cs.stdinMu.Lock()
	_, err := cs.execSession.Stdin.Write(toWrite)
	cs.stdinMu.Unlock()
	return err
}

// ResizePty adjusts the child shell's terminal dimensions.
func (cs *ChildShell) ResizePty(rows, cols int) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.Status != api.SessionRunning {
		return fmt.Errorf("process not running")
	}
	if err := cs.execSession.ResizePty(rows, cols); err != nil {
		return err
	}
	cs.Rows = rows
	cs.Cols = cols
	return nil
}

// RegisterReader creates a new output reader for this child shell.
func (cs *ChildShell) RegisterReader() (int, error) {
	return cs.buf.NewReader()
}

// RegisterReaderFromBufferStart creates a reader seeded at the start of the retained transcript.
func (cs *ChildShell) RegisterReaderFromBufferStart() (int, error) {
	return cs.buf.NewReaderFromStart()
}

// UnregisterReader removes a reader by ID.
func (cs *ChildShell) UnregisterReader(id int) {
	cs.buf.Unregister(id)
}

// ReadTerminalStream reads PTY output for a reader without appending to the message log.
func (cs *ChildShell) ReadTerminalStream(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, maxBytes int) (string, error) {
	data, err := cs.buf.Read(ctx, readerID, timeout, maxBytes)
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
	return output, nil
}

// HasMoreOutput returns whether the given reader has unread data.
func (cs *ChildShell) HasMoreOutput(readerID int) bool {
	return cs.buf.HasMore(readerID)
}

func (cs *ChildShell) IsBufferClosed() bool {
	return cs.buf.IsClosed()
}

// OutputByteRange returns a copy of retained raw output bytes [start, start+max) and total retained length.
func (cs *ChildShell) OutputByteRange(start int64, max int) ([]byte, int64, error) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.buf == nil {
		return nil, 0, fmt.Errorf("output buffer unavailable")
	}
	data, total := cs.buf.ByteRange(start, max)
	return data, total, nil
}

// BufferLen returns retained raw output length in bytes.
func (cs *ChildShell) BufferLen() int64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.buf == nil {
		return 0
	}
	return cs.buf.Len()
}

// TerminateShell closes the child shell's exec session channel without touching the
// shared SSH client (CloseSessionOnly). The client lifetime is managed by the parent Session.
func (cs *ChildShell) TerminateShell() {
	cs.mu.Lock()
	if cs.Status != api.SessionRunning {
		cs.mu.Unlock()
		return // already terminated
	}
	cs.mu.Unlock()

	cs.execSession.CloseSessionOnly()
	select {
	case <-cs.execSession.Done():
	case <-time.After(2 * time.Second):
	}
	cs.mu.Lock()
	cs.Status = api.SessionExited
	code := -1
	cs.ExitCode = &code
	cs.mu.Unlock()
	cs.buf.Close()
	// Ensure Done() channel is closed for any waiters (closeOnce prevents races with the exit watcher goroutine).
	cs.closeOnce.Do(func() { close(cs.done) })
}

// pipeChildToBuffer pipes child shell output into the buffer.
func (cs *ChildShell) pipeToBuffer(r io.Reader) {
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				cs.buf.Write(buf[:n])
			}
			if err != nil {
				return
			}
			select {
			case <-cs.done:
				return
			default:
			}
		}
	}()
}

// removeChildShell deletes a child shell from the parent's map and triggers UI notification.
func (s *Session) removeChildShell(id string) {
	s.childShellsMu.Lock()
	delete(s.childShells, id)
	s.childShellsMu.Unlock()
	if s.onChildChange != nil {
		s.onChildChange()
	}
}

// startChildReaders starts the stdout/stderr pipe goroutines and exit watcher for a child shell.
func (cs *ChildShell) startReaders() {
	cs.pipeToBuffer(cs.execSession.Stdout)
	cs.pipeToBuffer(cs.execSession.Stderr)

	go func() {
		<-cs.execSession.Done()
		cs.closeOnce.Do(func() { close(cs.done) })
		cs.mu.Lock()
		cs.Status = api.SessionExited
		code := cs.execSession.ExitCode()
		cs.ExitCode = &code
		cs.mu.Unlock()
		cs.buf.Close()
		// Clean up from parent's map and notify UI (only once).
		cs.cleanupOnce.Do(func() {
			if cs.parent != nil {
				cs.parent.removeChildShell(cs.ID)
			}
		})
		slog.Debug("child shell exited", "child_shell_id", cs.ID, "exit_code", code)
	}()
}

// CreateChildShell opens a new SSH session channel on the parent's existing SSH connection.
func (s *Session) CreateChildShell(command string, args []string, pty bool, rows, cols int, name string) (*ChildShell, error) {
	sshClient := s.SSHClient()
	if sshClient == nil {
		return nil, fmt.Errorf("sub-shell multiplexing requires an SSH connection; internal loopback sessions do not support multiple channels")
	}

	id := uuid.New().String()[:12]
	if name == "" {
		name = fmt.Sprintf("shell-%s", id)
	}

	execSession, err := sshclient.StartWithClient(sshClient, command, args, pty, rows, cols)
	if err != nil {
		return nil, fmt.Errorf("create child shell: %w", err)
	}

	buf := buffer.New(1024 * 1024)
	buf.NewReader() // default reader 0 for MCP read_output

	cs := &ChildShell{
		ID:          id,
		Name:        name,
		parent:      s,
		execSession: execSession,
		buf:         buf,
		done:        make(chan struct{}),
		Status:      api.SessionRunning,
		CreatedAt:   time.Now().UTC(),
		Rows:        rows,
		Cols:        cols,
		enterCRLF:   s.primaryShell.enterCRLF,
	}

	cs.startReaders()

	s.childShellsMu.Lock()
	if s.childShells == nil {
		s.childShells = make(map[string]*ChildShell)
	}
	s.childShells[id] = cs
	s.childShellsMu.Unlock()
	if s.onChildChange != nil {
		s.onChildChange()
	}

	slog.Debug("child shell created", "parent_id", s.ID, "child_shell_id", id)
	return cs, nil
}

// CloseChildShell terminates and removes a child shell from the parent.
func (s *Session) CloseChildShell(id string) error {
	s.childShellsMu.Lock()
	cs, ok := s.childShells[id]
	if !ok {
		s.childShellsMu.Unlock()
		return fmt.Errorf("child shell %q not found", id)
	}
	s.childShellsMu.Unlock()

	cs.TerminateShell()
	cs.cleanupOnce.Do(func() {
		s.removeChildShell(id)
	})
	slog.Debug("child shell closed", "parent_id", s.ID, "child_shell_id", id)
	return nil
}

// GetChildShell returns a child shell by ID, or nil if not found.
func (s *Session) GetChildShell(id string) *ChildShell {
	s.childShellsMu.RLock()
	defer s.childShellsMu.RUnlock()
	return s.childShells[id]
}

	// ListChildShells returns public metadata for all shells (root + children) of this session.
	func (s *Session) ListChildShells() []api.Session {
		s.childShellsMu.RLock()
		defer s.childShellsMu.RUnlock()
		type entry struct {
			info      api.Session
			createdAt time.Time
		}
		entries := make([]entry, 0, len(s.childShells)+1)
		entries = append(entries, entry{info: s.primaryShell.Info(), createdAt: s.primaryShell.CreatedAt})
		for _, cs := range s.childShells {
			entries = append(entries, entry{info: cs.Info(), createdAt: cs.CreatedAt})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].createdAt.Before(entries[j].createdAt) })
		out := make([]api.Session, len(entries))
		for i, e := range entries {
			out[i] = e.info
		}
		return out
	}
