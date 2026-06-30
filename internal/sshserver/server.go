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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/ssh"
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
	writeMu   sync.Mutex  // serializes close(writeCh) and sends to writeCh
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
	// Snapshot the write channel under the mutex to avoid racing with Close.
	c.writeMu.Lock()
	wch := c.writeCh
	c.writeMu.Unlock()
	if wch == nil {
		return 0, net.ErrClosed
	}

	data := make([]byte, len(b))
	copy(data, b)
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
		ChannelHandlers: map[string]ssh.ChannelHandler{
			"session": ssh.DefaultSessionHandler,
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
		if runtime.GOOS == "windows" {
			com := strings.TrimSpace(os.Getenv("ComSpec"))
			if com == "" {
				com = "cmd.exe"
			}
			cmdArgs = []string{com}
		} else {
			sh := strings.TrimSpace(os.Getenv("SHELL"))
			if sh == "" {
				cmdArgs = []string{"/bin/sh"}
			} else {
				cmdArgs = strings.Fields(sh)
			}
		}
	}


	if len(sess.Command()) == 0 {
		cmdArgs = disableHistoryExpansion(cmdArgs)
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	// Forward signals from client to local process.
	sigCh := make(chan ssh.Signal, 8)
	sess.Signals(sigCh)
	go func() {
		for sig := range sigCh {
			if cmd.Process != nil {
				if osSig := sshSignalToOSSig(sig); osSig != nil {
					cmd.Process.Signal(osSig)
				}
			}
		}
	}()

	ppty, winCh, isPty := sess.Pty()
	if isPty {
		go func() {
			for range winCh {
			}
		}()
		setPtySysProcAttr(cmd)
		cmd.Env = append(os.Environ(), "TERM="+ppty.Term)
		if err := ppty.Start(cmd); err != nil {
			io.WriteString(sess, err.Error()+"\n")
			sess.Exit(1)
			return
		}
	} else {
		cmd.Stdin = sess
		cmd.Stdout = sess
		cmd.Stderr = sess.Stderr()
		if err := cmd.Start(); err != nil {
			io.WriteString(sess, err.Error()+"\n")
			sess.Exit(1)
			return
		}
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
