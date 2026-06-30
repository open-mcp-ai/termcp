package sshclient

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ExecSession wraps an active SSH client session.
type ExecSession struct {
	client    *ssh.Client
	session   *ssh.Session
	Stdin     io.WriteCloser
	Stdout    io.Reader
	Stderr    io.Reader
	done      chan struct{}
	exitCode  int
	err       error
	ownClient bool // if false, Close() does not close the underlying SSH client
}

func closeIfCloser(r io.Reader) {
	if c, ok := r.(io.Closer); ok {
		c.Close()
	}
}

// StartWithConfig dials addr with the given SSH client config and starts a command.
func StartWithConfig(addr string, config *ssh.ClientConfig, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ssh ClientConfig")
	}
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}

	return startSession(client, session, command, args, pty, rows, cols, true)
}

// StartWithConn creates an SSH client over an existing net.Conn (e.g. net.Pipe)
// and starts a command. Used for in-process connections without TCP.
func StartWithConn(conn net.Conn, config *ssh.ClientConfig, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ssh ClientConfig")
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, "inmem", config)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(c, chans, reqs)

	session, err := client.NewSession()
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("new session: %w", err)
	}

	return startSession(client, session, command, args, pty, rows, cols, true)
}

// StartWithClient creates a new ExecSession on an existing SSH client without dialing.
// The returned ExecSession has ownClient=false; Close() will not close the shared client.
func StartWithClient(client *ssh.Client, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	if client == nil {
		return nil, fmt.Errorf("nil ssh Client")
	}
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	return startSession(client, session, command, args, pty, rows, cols, false)
}

// startSession sets up pipes, optional PTY, starts the command, and returns an ExecSession.
// When ownClient is false, error paths close session but not client.
func startSession(client *ssh.Client, session *ssh.Session, command string, args []string, pty bool, rows, cols int, ownClient bool) (*ExecSession, error) {
	closeClient := func() {
		if ownClient {
			client.Close()
		}
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if pty {
		// 远端 tmux/UTF-8 画线字符依赖 LC_CTYPE；多数 sshd 会忽略 Setenv，失败不影响建连。
		lang := strings.TrimSpace(os.Getenv("LANG"))
		if lang == "" {
			lang = "C.UTF-8"
		}
		_ = session.Setenv("LANG", lang)
		_ = session.Setenv("LC_CTYPE", lang)

		if err := session.RequestPty("xterm-256color", rows, cols, defaultPTYModes()); err != nil {
			closeIfCloser(stderr)
			closeIfCloser(stdout)
			stdin.Close()
			session.Close()
			closeClient()
			return nil, fmt.Errorf("request pty: %w", err)
		}
	}

	var startErr error
	if trimmed := strings.TrimSpace(command); trimmed == "" && len(args) == 0 {
		switch {
		case pty:
			if err := session.Shell(); err != nil {
				sh, shArgs := defaultShellExecArgv()
				startErr = session.Start(shellQuote(sh, shArgs))
			}
		default:
			sh, shArgs := defaultShellExecArgv()
			startErr = session.Start(shellQuote(sh, shArgs))
		}
	} else {
		startErr = session.Start(shellQuote(trimmed, args))
	}
	if startErr != nil {
		closeIfCloser(stderr)
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		closeClient()
		return nil, fmt.Errorf("start command: %w", startErr)
	}

	es := &ExecSession{
		client:    client,
		session:   session,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		done:      make(chan struct{}),
		ownClient: ownClient,
	}

	go func() {
		es.err = session.Wait()
		if es.err != nil {
			if exitErr, ok := es.err.(*ssh.ExitError); ok {
				es.exitCode = exitErr.ExitStatus()
			}
		}
		close(es.done)
	}()

	return es, nil
}

// Done returns a channel that closes when the remote process exits.
func (es *ExecSession) Done() <-chan struct{} {
	return es.done
}

// ExitCode returns the process exit code after Done is closed.
func (es *ExecSession) ExitCode() int {
	return es.exitCode
}

// ResizePty sends a window-change request for the session.
func (es *ExecSession) ResizePty(rows, cols int) error {
	return es.session.WindowChange(rows, cols)
}

// Signal sends a signal to the remote process.
func (es *ExecSession) Signal(sig ssh.Signal) error {
	return es.session.Signal(sig)
}

// Close forcefully terminates the session and underlying connection.
// SSHClient returns the underlying SSH client, or nil for internal sessions.
func (es *ExecSession) SSHClient() *ssh.Client { return es.client }

func (es *ExecSession) Close() error {
	es.Stdin.Close()
	var firstErr error
	if err := es.session.Close(); err != nil {
		firstErr = err
	}
	if es.ownClient {
		if err := es.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// CloseSessionOnly closes this session channel without touching the shared SSH client.
func (es *ExecSession) CloseSessionOnly() error {
	es.Stdin.Close()
	return es.session.Close()
}

// defaultShellExecArgv returns the local default shell argv (respects runtime.GOOS).
func defaultShellExecArgv() (string, []string) {
	if runtime.GOOS == "windows" {
		c := strings.TrimSpace(os.Getenv("ComSpec"))
		if c == "" {
			c = "cmd.exe"
		}
		return c, nil
	}
	sh := strings.TrimSpace(os.Getenv("SHELL"))
	if sh == "" {
		return "/bin/sh", nil
	}
	f := strings.Fields(sh)
	if len(f) == 0 {
		return "/bin/sh", nil
	}
	return f[0], f[1:]
}

func shellQuote(command string, args []string) string {
	parts := []string{quoteIfNeeded(command)}
	for _, a := range args {
		parts = append(parts, quoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\n\"'\\$|&;<>(){}[]*?#~`") {
		return strconv.Quote(s)
	}
	return s
}

// defaultPTYModes returns standard terminal modes for PTY sessions.
func defaultPTYModes() ssh.TerminalModes {
	return ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.ICRNL:         1,
		ssh.ONLCR:         1,
		ssh.OPOST:         1,
		ssh.ISIG:          1,
		ssh.ICANON:        1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
}
