package sshserver

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/ssh"
	"github.com/open-mcp-ai/termcp/internal/shell"
	"github.com/pkg/sftp"
	sshstd "golang.org/x/crypto/ssh"
)

// inMemListener is a net.Listener backed by duplex channel pairs for in-process SSH connections.
type inMemListener struct {
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func newInMemListener() *inMemListener {
	return &inMemListener{
		conns: make(chan net.Conn),
		done:  make(chan struct{}),
	}
}

func (l *inMemListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *inMemListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *inMemListener) Addr() net.Addr { return inMemAddr{} }

// Dial creates a full-duplex in-memory connection pair (using io.Pipe pairs to avoid
// net.Pipe's synchronous write deadlock during SSH version exchange), enqueues the
// server side, and returns the client side.
func (l *inMemListener) Dial() (net.Conn, error) {
	server, client := duplexPipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.done:
		server.Close()
		client.Close()
		return nil, net.ErrClosed
	}
}

// duplexConn is a full-duplex net.Conn backed by buffered byte channels.
// Buffered channels prevent the write-then-read deadlock that occurs during SSH
// version exchange when both sides write before either reads (net.Pipe / io.Pipe
// are synchronous and block writes until the other side reads).
type duplexConn struct {
	writeMu   sync.Mutex // serializes close(writeCh) and sends to writeCh
	readCh    <-chan []byte
	writeCh   chan []byte // bidirectional; nil after Close (peer reader gets io.EOF via close)
	closeCh   chan struct{}
	closeOnce sync.Once
	readBuf   []byte
}

func (c *duplexConn) Read(b []byte) (int, error) {
	if len(c.readBuf) > 0 {
		n := copy(b, c.readBuf)
		c.readBuf = c.readBuf[n:]
		return n, nil
	}
	select {
	case data, ok := <-c.readCh:
		if !ok {
			return 0, io.EOF
		}
		n := copy(b, data)
		if n < len(data) {
			c.readBuf = data[n:]
		}
		return n, nil
	case <-c.closeCh:
		return 0, net.ErrClosed
	}
}

func (c *duplexConn) Write(b []byte) (int, error) {
	data := make([]byte, len(b))
	copy(data, b)
	// Hold writeMu across the send so Close cannot close writeCh mid-send
	// (that would be a data race and a "send on closed channel" panic).
	// Close closes closeCh before taking writeMu, so a blocked send here is
	// always released instead of deadlocking the two.
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wch := c.writeCh
	if wch == nil {
		return 0, net.ErrClosed
	}
	select {
	case wch <- data:
		return len(b), nil
	case <-c.closeCh:
		return 0, net.ErrClosed
	}
}

func (c *duplexConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closeCh) // unblock local reads/writes
		c.writeMu.Lock()
		close(c.writeCh) // unblock peer's reader (writeCh is always non-nil inside closeOnce)
		c.writeCh = nil
		c.writeMu.Unlock()
	})
	return nil
}
func (c *duplexConn) LocalAddr() net.Addr              { return inMemAddr{} }
func (c *duplexConn) RemoteAddr() net.Addr             { return inMemAddr{} }
func (c *duplexConn) SetDeadline(time.Time) error      { return nil }
func (c *duplexConn) SetReadDeadline(time.Time) error  { return nil }
func (c *duplexConn) SetWriteDeadline(time.Time) error { return nil }

// duplexPipe creates a pair of connected duplexConns using buffered channels.
// The buffered channels (cap 16) allow the SSH version exchange to complete
// without blocking: each side writes ~15-30 bytes which fits in the buffer.
func duplexPipe() (net.Conn, net.Conn) {
	const bufCap = 16
	a2b := make(chan []byte, bufCap)
	b2a := make(chan []byte, bufCap)

	a := &duplexConn{readCh: b2a, writeCh: a2b, closeCh: make(chan struct{})}
	b := &duplexConn{readCh: a2b, writeCh: b2a, closeCh: make(chan struct{})}
	return a, b
}

type inMemAddr struct{}

func (inMemAddr) Network() string { return "inmem" }
func (inMemAddr) String() string  { return "inmem" }

// Server wraps an internal SSH server.
type Server struct {
	server   *ssh.Server
	listener *inMemListener
	started  atomic.Bool
	mu       sync.Mutex
	// pending maps one-time username -> password. A successful password auth removes the entry.
	pending map[string]string
}

// ptyContextKey is the per-session context key under which the PTY allocated
// for a session is handed from the session request goroutine to the session
// handler. Keying by session keeps concurrent shells multiplexed on one SSH
// connection apart.
type ptyContextKey struct{ sess ssh.Session }

// sessionPTY is the per-session PTY handoff cell.
type sessionPTY struct {
	// mu guards the PTY against concurrent use from the handler goroutine and
	// the request goroutine. charmbracelet/ssh closes the PTY from the session
	// request goroutine when the client closes the channel, which can land in
	// the middle of the handler's fork/exec — and os.File.Fd is documented as
	// unsafe to call concurrently with Close (the child could inherit a
	// recycled descriptor). Taking mu around pty.Start in the handler and
	// around the library's closer removes that interleaving.
	mu  sync.Mutex
	pty ssh.Pty
	ok  bool
}

// stashPTY records the PTY the client requested for sess. It runs on the
// session request goroutine — the same goroutine that applies "window-change"
// updates — so this is the one place where the session's PTY struct can be read
// without racing charmbracelet/ssh's unlocked `sess.pty.Window` write.
func stashPTY(sess ssh.Session) {
	ps, _ := sess.Context().Value(ptyContextKey{sess}).(*sessionPTY)
	if ps == nil {
		return
	}
	if pty, _, ok := sess.Pty(); ok {
		ps.pty, ps.ok = pty, true
	}
}

// takePTY returns the PTY stashed for sess by stashPTY, if any, and clears the
// stash so the connection context does not retain the session (and its pty).
func takePTY(sess ssh.Session) (*sessionPTY, bool) {
	key := ptyContextKey{sess}
	ps, _ := sess.Context().Value(key).(*sessionPTY)
	if ps == nil || !ps.ok {
		return nil, false
	}
	sess.Context().SetValue(key, nil)
	return ps, true
}

// New creates an internal SSH server that communicates in-process via net.Pipe (no TCP port).
func New() *Server {
	s := &Server{
		pending: make(map[string]string),
	}
	srv := &ssh.Server{
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			return s.passwordOK(ctx.User(), password)
		},
		LocalPortForwardingCallback: func(ctx ssh.Context, dHost string, dPort uint32) bool {
			return true
		},
		// Capture the allocated PTY on the request goroutine (see stashPTY) so the
		// session handler never reads the session's PTY struct while a
		// window-change may be updating it.
		SessionRequestCallback: func(sess ssh.Session, _ string) bool {
			stashPTY(sess)
			return true
		},
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session":      ssh.DefaultSessionHandler,
			"direct-tcpip": ssh.DirectTCPIPHandler,
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(sess ssh.Session) {
				srv, err := sftp.NewServer(sess)
				if err != nil {
					slog.Error("sftp server start", "err", err)
					return
				}
				srv.Serve()
			},
		},
	}
	_ = srv.SetOption(ssh.AllocatePty())
	// Wrap the allocator so the PTY it creates is closed under the same lock the
	// session handler holds while forking the command onto it (see sessionPTY).
	allocPty := srv.PtyHandler
	srv.PtyHandler = func(ctx ssh.Context, sess ssh.Session, pty ssh.Pty) (func() error, error) {
		ps := &sessionPTY{}
		key := ptyContextKey{sess}
		sess.Context().SetValue(key, ps)
		if allocPty == nil {
			return func() error { return nil }, nil
		}
		closer, err := allocPty(ctx, sess, pty)
		if err != nil {
			sess.Context().SetValue(key, nil)
			return nil, err
		}
		return func() error {
			ps.mu.Lock()
			defer ps.mu.Unlock()
			return closer()
		}, nil
	}
	s.server = srv
	return s
}

func (s *Server) passwordOK(user, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expPass, ok := s.pending[user]
	if !ok {
		return false
	}
	if len(password) != len(expPass) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(password), []byte(expPass)) != 1 {
		return false
	}
	delete(s.pending, user)
	return true
}

func (s *Server) mintCreds() (user, pass string, err error) {
	userRand := make([]byte, 10)
	if _, err := rand.Read(userRand); err != nil {
		return "", "", err
	}
	passRand := make([]byte, 32)
	if _, err := rand.Read(passRand); err != nil {
		return "", "", err
	}
	user = "t" + hex.EncodeToString(userRand)
	pass = base64.RawURLEncoding.EncodeToString(passRand)
	return user, pass, nil
}

// MintClientConfig registers a new one-time username/password and returns a dial config.
// The entry is removed on the first successful SSH password authentication (single use).
// Call only after Start().
func (s *Server) MintClientConfig() (*sshstd.ClientConfig, error) {
	user, pass, err := s.mintCreds()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.pending[user] = pass
	s.mu.Unlock()
	return &sshstd.ClientConfig{
		User:            user,
		Auth:            []sshstd.AuthMethod{sshstd.Password(pass)},
		HostKeyCallback: sshstd.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}, nil
}

// Dial creates a new in-memory connection to this server. The returned net.Conn
// is the client side of a net.Pipe(); the server side is handed to the SSH server
// goroutine for handshake and session handling.
func (s *Server) Dial() (net.Conn, error) {
	if !s.started.Load() {
		return nil, net.ErrClosed
	}
	return s.listener.Dial()
}

// Start begins serving SSH connections on the in-memory listener.
func (s *Server) Start() error {
	pemBytes, err := generateHostKeyPEM()
	if err != nil {
		return fmt.Errorf("generate host key: %w", err)
	}
	if err := s.server.SetOption(ssh.HostKeyPEM(pemBytes)); err != nil {
		return fmt.Errorf("set host key: %w", err)
	}

	s.listener = newInMemListener()
	s.started.Store(true)

	go func() {
		if err := s.server.Serve(s.listener); err != nil {
			slog.Info("ssh server stopped", "err", err)
		}
	}()
	return nil
}

// Stop shuts down the SSH server.
func (s *Server) Stop() error {
	if !s.started.Load() {
		return nil
	}
	s.mu.Lock()
	s.pending = make(map[string]string)
	s.mu.Unlock()
	_ = s.listener.Close()
	return s.server.Close()
}

func sshSignalToOSSig(sig ssh.Signal) os.Signal {
	switch sig {
	case "TERM":
		return syscall.SIGTERM
	case "INT":
		return syscall.SIGINT
	case "KILL":
		return syscall.SIGKILL
	case "HUP":
		return syscall.SIGHUP
	default:
		return nil
	}
}

// disableHistoryExpansion prepends shell-specific flags to suppress ! history expansion.
func disableHistoryExpansion(args []string) []string {
	if len(args) == 0 {
		return args
	}
	switch filepath.Base(args[0]) {
	case "zsh":
		return append([]string{args[0], "-o", "NO_BANG_HIST"}, args[1:]...)
	case "bash", "sh":
		return append([]string{args[0], "+o", "histexpand"}, args[1:]...)
	default:
		return args
	}
}

func (s *Server) handleSession(sess ssh.Session) {
	cmdArgs := sess.Command()
	if len(cmdArgs) == 0 {
		sh, shArgs := shell.NewDetector().Argv()
		cmdArgs = append([]string{sh}, shArgs...)
	}

	if len(sess.Command()) == 0 {
		cmdArgs = disableHistoryExpansion(cmdArgs)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	// Forward signals from client to local process. Started after cmd.Start() so
	// cmd.Process is already set — reading it here before Start() would race with
	// the concurrent write in Start().
	sigCh := make(chan ssh.Signal, 8)
	sess.Signals(sigCh)

	// The PTY info comes from stashPTY rather than sess.Pty(): the request loop
	// writes sess.pty.Window for every window-change without holding the session
	// lock, so reading that struct here would race it. Window changes are applied
	// by the library's own drain goroutine (installed by AllocatePty).
	ps, hasPty := takePTY(sess)
	var started bool
	if hasPty {
		setPtySysProcAttr(cmd)
		cmd.Env = append(os.Environ(), "TERM="+ps.pty.Term)
		// Serialized with the library's PTY teardown: see sessionPTY.
		ps.mu.Lock()
		startErr := ps.pty.Start(cmd)
		ps.mu.Unlock()
		if startErr != nil {
			io.WriteString(sess, startErr.Error()+"\n")
			sess.Exit(1)
			return
		}
		started = true
	} else {
		// Pipe mode (no TTY). Use StdinPipe so exec.Cmd does not spawn a stdin
		// copy goroutine that cmd.Wait() would block on forever. We copy the SSH
		// stream to the process stdin in our own goroutine, which unblocks only on
		// client EOF — independent of cmd.Wait(). Without this, any non-interactive
		// command (echo, ls, …) deadlocks: cmd.Wait() waits for the stdin copy to
		// finish, which waits on sess.Read(), which never returns because the
		// channel only closes after sess.Exit() below (which never runs).
		in, err := cmd.StdinPipe()
		if err != nil {
			io.WriteString(sess, err.Error()+"\n")
			sess.Exit(1)
			return
		}
		cmd.Stdout = sess
		cmd.Stderr = sess.Stderr()
		if err := cmd.Start(); err != nil {
			_ = in.Close()
			io.WriteString(sess, err.Error()+"\n")
			sess.Exit(1)
			return
		}
		started = true
		go func() {
			_, _ = io.Copy(in, sess)
			in.Close()
		}()
	}

	// Forward signals from client to local process. Now that cmd.Start() has
	// populated cmd.Process (and pty.Start calls it too), reading it here cannot
	// race with the Start() write.
	if started {
		go func() {
			for sig := range sigCh {
				if cmd.Process != nil {
					if osSig := sshSignalToOSSig(sig); osSig != nil {
						cmd.Process.Signal(osSig)
					}
				}
			}
		}()
	}

	cmd.Wait()

	exitCode := 127
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	sess.Exit(exitCode)
}

func generateHostKeyPEM() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	b := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: b,
	}
	return pem.EncodeToMemory(block), nil
}
