package sshclient

import (
	"fmt"
	"io"
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

	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		client.Close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		stdin.Close()
		session.Close()
		client.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	stderr, err := session.StderrPipe()
	if err != nil {
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		client.Close()
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
			client.Close()
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
		client.Close()
		return nil, fmt.Errorf("start command: %w", startErr)
	}

	es := &ExecSession{
		client:    client,
		session:   session,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		done:      make(chan struct{}),
		ownClient: true,
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

// shellQuote builds a shell-safe command string.
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
