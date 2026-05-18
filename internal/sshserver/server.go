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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/ssh"
	sshstd "golang.org/x/crypto/ssh"
)

// Server wraps an internal SSH server.
type Server struct {
	addr     string
	server   *ssh.Server
	listener net.Listener
	started  atomic.Bool
	mu       sync.Mutex
	// pending maps one-time username -> password. A successful password auth removes the entry.
	pending map[string]string
}

// New creates an internal SSH server listening on addr.
// If addr is empty, it defaults to "127.0.0.1:0" (random port).
func New(addr string) *Server {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	s := &Server{
		addr:    addr,
		pending: make(map[string]string),
	}
	srv := &ssh.Server{
		Addr: addr,
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			return s.passwordOK(ctx.User(), password)
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

// Addr returns the actual listener address after Start.
func (s *Server) Addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Start begins listening and serving SSH connections.
func (s *Server) Start() error {
	pemBytes, err := generateHostKeyPEM()
	if err != nil {
		return fmt.Errorf("generate host key: %w", err)
	}
	if err := s.server.SetOption(ssh.HostKeyPEM(pemBytes)); err != nil {
		return fmt.Errorf("set host key: %w", err)
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	s.listener = ln
	s.started.Store(true)

	go func() {
		if err := s.server.Serve(ln); err != nil {
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
