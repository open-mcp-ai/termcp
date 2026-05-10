package sshserver

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/ssh"
	"github.com/pkg/sftp"
	sshstd "golang.org/x/crypto/ssh"
)

const internalPassword = "interactive-process-internal"

// Server wraps an internal SSH server.
type Server struct {
	addr     string
	server   *ssh.Server
	listener net.Listener
	started  atomic.Bool
}

// New creates an internal SSH server listening on addr.
// If addr is empty, it defaults to "127.0.0.1:0" (random port).
func New(addr string) *Server {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	s := &Server{addr: addr}
	srv := &ssh.Server{
		Addr: addr,
		Handler: func(sess ssh.Session) {
			s.handleSession(sess)
		},
		PasswordHandler: func(ctx ssh.Context, password string) bool {
			return password == internalPassword
		},
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": func(sess ssh.Session) {
				server, err := sftp.NewServer(sess)
				if err != nil {
					slog.Error("sftp server init failed", "err", err)
					return
				}
				if err := server.Serve(); err == io.EOF {
					server.Close()
				} else if err != nil {
					slog.Error("sftp server failed", "err", err)
				}
			},
		},
	}
	_ = srv.SetOption(ssh.AllocatePty())
	s.server = srv
	return s
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
		io.WriteString(sess, "no command\n")
		sess.Exit(1)
		return
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

// InternalPassword returns the hardcoded password used for client auth.
func InternalPassword() string {
	return internalPassword
}

// ClientConfig returns a pre-configured ssh.ClientConfig for connecting to the internal server.
func ClientConfig() *sshstd.ClientConfig {
	return &sshstd.ClientConfig{
		User: "internal",
		Auth: []sshstd.AuthMethod{
			sshstd.Password(internalPassword),
		},
		HostKeyCallback: sshstd.InsecureIgnoreHostKey(),
	}
}
