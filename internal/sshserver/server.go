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

	"github.com/creack/pty"
	gliderssh "github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const internalPassword = "interactive-process-internal"

// Server wraps a gliderlabs SSH server for internal use.
type Server struct {
	addr     string
	server   *gliderssh.Server
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
	s.server = &gliderssh.Server{
		Addr: addr,
		Handler: func(sess gliderssh.Session) {
			s.handleSession(sess)
		},
		PasswordHandler: func(ctx gliderssh.Context, password string) bool {
			return password == internalPassword
		},
		PtyCallback: func(ctx gliderssh.Context, pty gliderssh.Pty) bool {
			return true
		},
		SubsystemHandlers: map[string]gliderssh.SubsystemHandler{
			"sftp": func(sess gliderssh.Session) {
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
	if err := s.server.SetOption(gliderssh.HostKeyPEM(pemBytes)); err != nil {
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

func sshSignalToOSSig(sig gliderssh.Signal) os.Signal {
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

func (s *Server) handleSession(sess gliderssh.Session) {
	cmdArgs := sess.Command()
	if len(cmdArgs) == 0 {
		io.WriteString(sess, "no command\n")
		sess.Exit(1)
		return
	}

	ptyReq, winCh, isPty := sess.Pty()

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)

	// Forward signals from client to local process.
	sigCh := make(chan gliderssh.Signal, 8)
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

	var exitCode int
	if isPty {
		f, err := pty.StartWithSize(cmd, &pty.Winsize{
			Rows: uint16(ptyReq.Window.Height),
			Cols: uint16(ptyReq.Window.Width),
		})
		if err != nil {
			io.WriteString(sess, err.Error()+"\n")
			sess.Exit(1)
			return
		}
		defer f.Close()

		go func() {
			for win := range winCh {
				if err := pty.Setsize(f, &pty.Winsize{
					Rows: uint16(win.Height),
					Cols: uint16(win.Width),
				}); err != nil {
					slog.Warn("pty setsize failed", "err", err)
				}
			}
		}()

		go func() { io.Copy(f, sess) }()
		io.Copy(sess, f)

		cmd.Wait()
	} else {
		cmd.Stdin = sess
		cmd.Stdout = sess
		cmd.Stderr = sess.Stderr()
		cmd.Run()
	}

	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else {
		exitCode = 127 // command not found
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
func ClientConfig() *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: "internal",
		Auth: []ssh.AuthMethod{
			ssh.Password(internalPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
}
