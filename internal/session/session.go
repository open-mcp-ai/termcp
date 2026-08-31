package session

import (
	"bytes"
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
	"sync/atomic"
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

// Lock ordering: shellStateMu -> mu -> stdinMu (and shellStateMu -> cs.mu).
// Never acquire in reverse order. s.mu and cs.mu are leaves; shellStateMu is
// only taken by session-level transitions (close/DEAD/terminate, new shells).
// The manager-assigned callbacks and the per-shell closed flag are atomics and
// need no lock (see field docs below).

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
// Session is a connection container; terminal I/O is addressed by shell_id via ChildShell.
type Session struct {
	api.Session
	mu             sync.RWMutex
	shellStateMu   sync.Mutex // serializes child registration with close/DEAD transition
	closing        bool       // guarded by shellStateMu; blocks new child channels
	stdinMu        sync.Mutex
	terminateOnce  sync.Once
	deadOnce       sync.Once
	exitOnce       sync.Once
	execSession    *sshclient.ExecSession
	buf            *buffer.Buffer
	readerID       int
	msgMgr         *message.Manager
	onDead         atomic.Pointer[func()] // invoked once when the session turns DEAD; assigned by the manager right after New(), while exit watchers may already be reading
	onChildChange  atomic.Pointer[func()] // invoked when child shells are added/removed; assigned under the same constraint
	enterCRLF      bool                   // line-ending for pipe-mode enter (\r\n for cmd/powershell, \n for unix)
	primaryShellID string                 // first shell id (≠ session id); used for legacy Session-level helpers

	shells sync.Map // *ChildShell by ID
	// shellHistory retains the last-known per-shell metadata (id, name, status)
	// so a DEAD session can still render per-shell tabs after shells leave the
	// live map on exit. Also persisted to sessions.json for restart restore.
	shellHistory sync.Map // string → api.Session
	// doneWG tracks live output pipe goroutines so markDead can flush their final
	// message Appends before the session is observed as exited.
	doneWG sync.WaitGroup
}

// New creates and starts a new Session.
// internal must be the built-in sshserver.Server (after Start) when cfg.Remote is nil; it may be nil for remote-only callers.
func New(internal *sshserver.Server, cfg Config, msgMgr *message.Manager) (*Session, error) {
	sessionID := uuid.New().String()[:12]
	shellID := uuid.New().String()[:12]
	name := cfg.Name
	if name == "" {
		name = fmt.Sprintf("session-%s", sessionID)
	}

	usePty := cfg.Mode == api.ModePTY

	var execSession *sshclient.ExecSession
	var sshEndpointPublic string // "internal" | "remote" for MCP / JSON (no host or credentials)

	if isRemote(cfg) {
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

	// First shell is a peer in shells map; its id is never equal to session id.
	root := &ChildShell{
		ID:          shellID,
		Name:        name,
		execSession: execSession,
		buf:         buf,
		done:        make(chan struct{}),
		Status:      api.SessionRunning,
		Rows:        cfg.Rows,
		Cols:        cfg.Cols,
		CreatedAt:   time.Now().UTC(),
		enterCRLF:   enterCRLF,
		mode:        cfg.Mode,
	}

	s := &Session{
		Session: api.Session{
			ID:          sessionID,
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
		enterCRLF:      enterCRLF,
		execSession:    execSession,
		buf:            buf,
		readerID:       rid,
		msgMgr:         msgMgr,
		primaryShellID: shellID,
	}

	if msgMgr != nil {
		msgMgr.Append(s.ID, api.MsgSystem, "Process started")
	}
	// Attach the root to the session BEFORE starting its readers so the output
	// pipe and exit watcher see a live parent + message manager from the first
	// byte. Starting readers first used to drop early output (never persisted).
	root.parent = s
	s.shells.Store(root.ID, root)
	s.shellHistory.Store(root.ID, root.Info())
	root.startReaders()

	slog.Debug("session started", "session_id", sessionID, "shell_id", shellID, "command", cfg.Command, "ssh_endpoint", sshEndpointPublic)

	return s, nil
}

// isRemote reports whether cfg selects a user-supplied SSH server instead of
// the built-in internal one. Single source of truth used by New and the
// create-failure logger in Manager.Create.
func isRemote(cfg Config) bool {
	return cfg.Remote != nil && strings.TrimSpace(cfg.Remote.Host) != ""
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

// PrimaryShellID returns the first shell created with this session.
func (s *Session) PrimaryShellID() string {
	return s.primaryShellID
}

// PrimaryShell returns the first shell if still registered.
func (s *Session) PrimaryShell() *ChildShell {
	return s.GetChildShell(s.primaryShellID)
}

// SendTerminalBytes writes raw keystrokes to the primary shell stdin (web UI / legacy).
func (s *Session) SendTerminalBytes(data []byte, pressEnter bool) error {
	cs := s.PrimaryShell()
	if cs == nil {
		return fmt.Errorf("session shell has exited")
	}
	return cs.SendTerminalBytes(data, pressEnter)
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
	crlf := s.enterCRLF
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
		s.msgMgr.AppendShell(s.ID, s.primaryShellID, api.MsgInput, logged)
	}
	return nil
}

func (s *Session) readOutput(ctx context.Context, readerID int, timeout time.Duration, stripAnsi bool, maxLines int, persist bool, maxBytes int) (string, error) {
	data, err := s.buf.ReadLimited(ctx, readerID, timeout, maxBytes, maxLines)
	if err != nil && err != io.EOF {
		return "", err
	}
	output := string(data)
	if stripAnsi {
		output = ansi.Strip(output)
		output = ansi.Compact(output)
	}
	// Output is archived at the write source (pipeToBuffer), not here, so it is
	// recorded exactly once regardless of which reader consumes it.
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
	cs := s.PrimaryShell()
	if cs == nil {
		return "", fmt.Errorf("session shell has exited")
	}
	return cs.ReadTerminalStream(ctx, readerID, timeout, stripAnsi, maxLines, maxBytes)
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

// Terminate ends the remote process/transport and marks the session DEAD
// (exited) in place. The session object, its shells' metadata and retained
// buffers, and its message history are all kept for read-only viewing; nothing
// is removed from the registry or purged. Only Manager.Delete/finalize release
// resources.
func (s *Session) Terminate(force bool, gracePeriod time.Duration) {
	s.terminateOnce.Do(func() {
		s.beginClosing()
		es := s.execSession
		if es == nil {
			// Restored (restart) placeholder with no live transport.
			s.markDead()
			return
		}
		if !force {
			es.Signal(ssh.SIGTERM)
			select {
			case <-es.Done():
				s.markDead()
				return
			case <-time.After(gracePeriod):
			}
		}

		// Close the session's own transport, not sibling session channels.
		// For remote sessions the shared SSH client is left for Disconnect(); for
		// internal loopback there's no multiplexing so close everything here.
		if s.SSHEndpoint == "internal" {
			es.Close()
		} else {
			es.CloseSessionOnly()
		}
		s.markDead()
	})
}

// markDead transitions a Running session to exited (DEAD) in place, retaining
// the object in the registry, its retained buffers, and its message history.
// Idempotent (runs once). It never removes the registry entry, forgets
// messages, or closes retained buffers; only Manager.Delete → finalize do that.
func (s *Session) markDead() {
	s.markDeadWithMessage("")
}

func (s *Session) markDeadWithMessage(systemMessage string) {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()
	// A deliberate close may race with an exit watcher that already observed
	// the transport error. Do not turn that intentional close into a network-loss
	// notification.
	if systemMessage != "" && s.closing {
		return
	}
	s.markDeadLocked(systemMessage)
}

// markDeadIfNoShells performs the zero-live-shell check under the same lock used
// by CreateChildShell, so a new channel cannot appear during the DEAD transition.
func (s *Session) markDeadIfNoShells(systemMessage string) {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()
	if s.liveShellCount() != 0 {
		return
	}
	s.markDeadLocked(systemMessage)
}

// markDeadLocked requires shellStateMu.
func (s *Session) markDeadLocked(systemMessage string) {
	s.closing = true
	s.deadOnce.Do(func() {
		// Flush output pipe goroutines so the last bytes are appended to the
		// message log before DEAD becomes observable.
		flushDone := make(chan struct{})
		go func() { s.doneWG.Wait(); close(flushDone) }()
		select {
		case <-flushDone:
		case <-time.After(500 * time.Millisecond):
		}

		if systemMessage != "" && s.msgMgr != nil {
			_, _ = s.msgMgr.Append(s.ID, api.MsgSystem, systemMessage)
		}

		s.mu.Lock()
		if s.Status == api.SessionRunning {
			s.Status = api.SessionExited
			s.UpdatedAt = time.Now().UTC()
		}
		s.mu.Unlock()

		slog.Debug("session DEAD", "session_id", s.ID)
		if fn := s.onDead.Load(); fn != nil {
			(*fn)()
		}
	})
}

// finalize releases the session's resources: remaining shells, buffers, and
// transport. Its only caller is Manager.Delete. Runs once.
func (s *Session) finalize() {
	s.exitOnce.Do(func() {
		s.beginClosing()
		s.terminateChildren()
		if s.buf != nil {
			s.buf.Close()
		}
		if s.execSession != nil {
			_ = s.execSession.Close()
		}
	})
}

// TerminateShellOnly closes the primary shell channel by shell id. For internal sessions this is a
// no-op — the process outlives the tab (like detaching from screen/tmux). For remote sessions only
// that SSH session channel is closed; other shells on the same TCP connection are unaffected.
func (s *Session) TerminateShellOnly() {
	if s.SSHEndpoint == "internal" {
		return // tab close doesn't kill the process
	}
	if cs := s.PrimaryShell(); cs != nil {
		cs.TerminateShell()
	}
}

// Disconnect closes the session transport and marks the session DEAD in place,
// preserving the object, buffers, and history for read-only viewing.
func (s *Session) Disconnect() {
	s.beginClosing()
	if s.execSession != nil {
		s.execSession.Close()
	}
	s.markDead()
}

func (s *Session) beginClosing() {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()
	if s.closing {
		return
	}
	s.closing = true
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		cs.mu.Lock()
		cs.deliberateClose = true
		cs.mu.Unlock()
		return true
	})
}

// terminateChildren closes all child shells. Called during parent session termination.
// Holds write lock long enough to snapshot+clear the map, preventing races with Create/Close.
func (s *Session) terminateChildren() {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()

	var children []*ChildShell
	s.shells.Range(func(k, v any) bool {
		s.shells.Delete(k)
		children = append(children, v.(*ChildShell))
		return true
	})
	for _, cs := range children {
		cs.TerminateShell()
	}
}

// ResizePty adjusts the terminal dimensions (pty mode only) on the primary shell.
func (s *Session) ResizePty(rows, cols int) error {
	if s.Mode != api.ModePTY {
		return fmt.Errorf("PTY resize only available in pty mode")
	}
	cs := s.PrimaryShell()
	if cs == nil {
		return fmt.Errorf("session shell has exited")
	}
	if err := cs.ResizePty(rows, cols); err != nil {
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
// SSHClient returns the underlying SSH client. All sessions (including internal/loopback)
// use SSH transport, so this never returns nil for a running session.
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
	parent      *Session // nil if not yet attached to a session
	execSession *sshclient.ExecSession
	buf         *buffer.Buffer
	done        chan struct{}
	closeOnce   sync.Once
	cleanupOnce sync.Once // guards explicit removal from the parent shell map
	mu          sync.RWMutex
	stdinMu     sync.Mutex
	Status      api.SessionStatus
	CreatedAt   time.Time
	ExitCode    *int
	Rows        int
	Cols        int
	// enterCRLF selects the byte sequence for pipe-mode enter.
	// It reflects the target shell family (unix vs cmd/powershell), not the
	// termcp host OS, so cross-OS SSH sessions send the right line ending.
	enterCRLF bool
	mode      api.SessionMode // pty or pipe; affects press_key("enter")
	// deliberateClose is set by TerminateShell/CloseChildShell so the exit
	// watcher does not treat an intentional channel close as SSH disconnect.
	deliberateClose bool
	// closed marks a shell the user explicitly closed (CloseChildShell). A
	// closed shell is a DELETE, not a DEAD transition: it is removed from the
	// live map AND the per-shell history snapshot. Atomic so the exit watcher
	// and TerminateShell can check it without extra locking; see
	// CloseChildShell and the watcher's store-then-recheck in startReaders.
	closed atomic.Bool
}

// Info returns a snapshot of the child shell's public metadata.
func (cs *ChildShell) Info() api.Session {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	s := api.Session{
		ID:        cs.ID,
		Name:      cs.Name,
		Mode:      cs.mode,
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
// pressEnter is kept for WebUI NL flag; MCP should use PressKey instead.
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

// PressKey writes a named key sequence (enter, ctrl+c, arrows, …) repeat times.
func (cs *ChildShell) PressKey(key string, repeat int) error {
	if repeat < 1 {
		repeat = 1
	}
	if repeat > 20 {
		return fmt.Errorf("repeat must be between 1 and 20, got %d", repeat)
	}
	seq, err := KeyBytes(key, cs.mode == api.ModePTY, cs.enterCRLF)
	if err != nil {
		return err
	}
	cs.mu.RLock()
	running := cs.Status == api.SessionRunning
	cs.mu.RUnlock()
	if !running {
		return fmt.Errorf("process has %s, cannot send input", cs.Status)
	}
	payload := bytes.Repeat(seq, repeat)
	cs.stdinMu.Lock()
	_, err = cs.execSession.Stdin.Write(payload)
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
	data, err := cs.buf.ReadLimited(ctx, readerID, timeout, maxBytes, maxLines)
	if err != nil && err != io.EOF {
		return "", err
	}
	output := string(data)
	if stripAnsi {
		output = ansi.Strip(output)
		output = ansi.Compact(output)
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
	cs.deliberateClose = true
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
	cs.mu.RLock()
	parent := cs.parent
	cs.mu.RUnlock()
	if parent != nil {
		cs.retainHistory(parent)
	}
	cs.buf.Close()
	// Ensure Done() channel is closed for any waiters (closeOnce prevents races with the exit watcher goroutine).
	cs.closeOnce.Do(func() { close(cs.done) })
}

// pipeChildToBuffer pipes child shell output into the buffer.
func (cs *ChildShell) pipeToBuffer(r io.Reader) {
	p := cs.parent
	if p != nil {
		p.doneWG.Add(1)
	}
	go func() {
		if p != nil {
			defer p.doneWG.Done()
		}
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				cs.buf.Write(buf[:n])
				// Archive output once at the source so every session (WebUI stream and
				// MCP read alike) leaves a transcript — regardless of which reader
				// consumes it. Never double-recorded because each write fires once.
				// Tagged with the originating shell so archived history can split tabs.
				if p := cs.parent; p != nil && p.msgMgr != nil {
					p.msgMgr.AppendShell(p.ID, cs.ID, api.MsgOutput, string(buf[:n]))
				}
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

// retainHistory keeps the shell's final metadata for the archived per-shell
// tabs, unless the shell was explicitly closed — closed shells are deleted,
// never retained.
func (cs *ChildShell) retainHistory(p *Session) {
	if cs.closed.Load() {
		return
	}
	p.shellHistory.Store(cs.ID, cs.Info())
}

// notifyChildChange invokes the on-child-change UI callback. The callback is
// assigned by Manager.Create after New returns, possibly while root-shell exit
// watchers are already running; it is an atomic pointer, so reads never race
// the assignment (a watcher firing in the assignment window just sees nil).
func (s *Session) notifyChildChange() {
	if fn := s.onChildChange.Load(); fn != nil {
		(*fn)()
	}
}

// removeChildShell deletes a shell from the parent's map and triggers UI notification.
func (s *Session) removeChildShell(id string) {
	s.shells.Delete(id)
	s.notifyChildChange()
}

// liveShellCount returns the number of child shells whose processes are still running.
// Exited shells stay in the map so MCP/WebUI readers can drain retained output.
func (s *Session) liveShellCount() int {
	n := 0
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		cs.mu.RLock()
		running := cs.Status == api.SessionRunning
		cs.mu.RUnlock()
		if running {
			n++
		}
		return true
	})
	return n
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
		// Retain this shell's final metadata for the archived per-shell tabs —
		// unless the user explicitly closed it. Store-then-recheck: a manual close
		// racing this store re-deletes the entry below, so a closed shell can
		// never survive in the retained snapshot (no lock needed: CloseChildShell
		// always sets closed before its purge).
		if p := cs.parent; p != nil {
			cs.retainHistory(p)
			if cs.closed.Load() {
				p.shellHistory.Delete(cs.ID)
			}
		}
		cs.buf.Close()
		// Keep the exited ChildShell in the parent map until explicit close/Delete.
		// This preserves its closed buffer so shell_output and WebUI can drain
		// final output after a fast pipe command has already exited.
		if p := cs.parent; p != nil {
			p.notifyChildChange()
		}
		// If the shell ended due to SSH disconnect (not deliberate close and not
		// clean process exit), tear down the session. exitOnce ensures once.
		cs.mu.RLock()
		deliberate := cs.deliberateClose
		reparent := cs.parent
		cs.mu.RUnlock()
		if reparent != nil && !deliberate && cs.execSession.Aborted() {
			slog.Debug("session DEAD via transport abort", "session_id", reparent.ID, "child_shell_id", cs.ID)
			reparent.markDeadWithMessage("❌ SSH connection lost — network disconnected")
		} else if reparent != nil && !deliberate && reparent.Mode == api.ModePipe {
			// A clean exit of the last pipe shell would otherwise leave a running
			// container with zero shells. Flip the parent to DEAD so the container
			// disappears from the running list; output/history stays for manual
			// cleanup (clear-dead button). PTY sessions remain reusable after a
			// shell exits, preserving the interactive-session contract.
			slog.Debug("pipe shell exited cleanly; checking parent", "session_id", reparent.ID, "child_shell_id", cs.ID)
			reparent.markDeadIfNoShells("Session ended — last shell exited")
		}
		slog.Debug("child shell exited", "child_shell_id", cs.ID, "exit_code", code)
	}()
}

// CreateChildShell opens a new SSH session channel on the parent's existing SSH connection.
func (s *Session) CreateChildShell(command string, args []string, pty bool, rows, cols int, name string) (*ChildShell, error) {
	s.shellStateMu.Lock()
	defer s.shellStateMu.Unlock()

	s.mu.RLock()
	running := s.Status == api.SessionRunning
	s.mu.RUnlock()
	if s.closing || !running {
		return nil, fmt.Errorf("session has exited")
	}

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

	mode := api.ModePipe
	if pty {
		mode = api.ModePTY
	}
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
		enterCRLF:   s.enterCRLF,
		mode:        mode,
	}

	s.shells.Store(id, cs)
	s.shellHistory.Store(id, cs.Info())
	cs.startReaders()
	s.notifyChildChange()

	slog.Debug("child shell created", "parent_id", s.ID, "child_shell_id", id)
	return cs, nil
}

// CloseChildShell terminates and removes a child shell from the parent.
// Manual close is a DELETE, not a DEAD transition: the shell is dropped from
// the live map, the per-shell history snapshot, and any persisted restore, so
// it never reappears as a dead/"end" tab. A pipe container whose last shell is
// closed flips to DEAD (same contract as a clean last-shell exit); PTY
// containers stay running and can spawn new shells.
func (s *Session) CloseChildShell(id string) error {
	v, ok := s.shells.Load(id)
	if !ok {
		return fmt.Errorf("child shell %q not found", id)
	}
	cs := v.(*ChildShell)

	// Mark closed first: TerminateShell and the exit watcher never retain a
	// closed shell, and the watcher's store-then-recheck (startReaders) deletes
	// any entry that raced the purge below. No lock needed between this store,
	// the purge, and the watcher: closed is an atomic bool, and the purge is
	// ordered after it (program order + atomic release/acquire).
	cs.closed.Store(true)
	cs.TerminateShell()
	cs.cleanupOnce.Do(func() {
		s.shellHistory.Delete(id)
		s.removeChildShell(id)
	})
	if s.Mode == api.ModePipe {
		s.markDeadIfNoShells("")
	}
	slog.Debug("child shell closed", "parent_id", s.ID, "child_shell_id", id)
	return nil
}

// GetChildShell returns a child shell by ID, or nil if not found.
func (s *Session) GetChildShell(id string) *ChildShell {
	v, _ := s.shells.Load(id)
	if v == nil {
		return nil
	}
	return v.(*ChildShell)
}

// ListChildShells returns public metadata for all shells of this session.
func (s *Session) ListChildShells() []api.Session {
	type entry struct {
		info      api.Session
		createdAt time.Time
	}
	var entries []entry
	s.shells.Range(func(_, v any) bool {
		cs := v.(*ChildShell)
		entries = append(entries, entry{info: cs.Info(), createdAt: cs.CreatedAt})
		return true
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].createdAt.Before(entries[j].createdAt) })
	out := make([]api.Session, len(entries))
	for i, e := range entries {
		out[i] = e.info
	}
	return out
}

// SnapshotShells returns the last-known per-shell metadata (still populated for
// shells dropped from the live map on exit), sorted by creation time. This is
// what gets persisted and what a DEAD session renders into its tabs.
func (s *Session) SnapshotShells() []api.Session {
	var out []api.Session
	s.shellHistory.Range(func(_, v any) bool {
		out = append(out, v.(api.Session))
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ShellsForView returns shell metadata for session rendering. Running sessions
// report only their live channel children: naturally exited shells stay in the
// live map for output draining, and deliberately closed shells are deleted, so
// there is never a snapshot fallback here — a closed shell can never reappear
// as a dead/"end" tab in a live session. DEAD/restored sessions fall back to
// the retained shell snapshot (natural exits / transport aborts only) so their
// tabs survive transport teardown or a restart.
func (s *Session) ShellsForView() []api.Session {
	s.mu.RLock()
	status := s.Status
	s.mu.RUnlock()
	if status == api.SessionRunning {
		return s.ListChildShells()
	}
	return s.SnapshotShells()
}
