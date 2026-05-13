package sshclient

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// ExecSession wraps an active SSH client session.
type ExecSession struct {
	client   *ssh.Client
	session  *ssh.Session
	Stdin    io.WriteCloser
	Stdout   io.Reader
	Stderr   io.Reader
	done     chan struct{}
	exitCode int
	err      error
}

// Start connects to the internal SSH server and starts a command.
// addr is the SSH server address (e.g. "127.0.0.1:2222").
// If pty is true, a pseudo-terminal is requested with the given dimensions.
func Start(addr string, command string, args []string, pty bool, rows, cols int) (*ExecSession, error) {
	return StartWithConfig(addr, sshserver.ClientConfig(), command, args, pty, rows, cols)
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

		modes := ssh.TerminalModes{
			ssh.ECHO:          1,
			ssh.TTY_OP_ISPEED: 14400,
			ssh.TTY_OP_OSPEED: 14400,
		}
		if err := session.RequestPty("xterm-256color", rows, cols, modes); err != nil {
			closeIfCloser(stderr)
			closeIfCloser(stdout)
			stdin.Close()
			session.Close()
			client.Close()
			return nil, fmt.Errorf("request pty: %w", err)
		}
	}

	cmdStr := shellQuote(command, args)
	if err := session.Start(cmdStr); err != nil {
		closeIfCloser(stderr)
		closeIfCloser(stdout)
		stdin.Close()
		session.Close()
		client.Close()
		return nil, fmt.Errorf("start command: %w", err)
	}

	es := &ExecSession{
		client:  client,
		session: session,
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
		done:    make(chan struct{}),
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
	if err := es.client.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// shellQuote builds a shell-safe command string.
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

// SFTPConn wraps an SFTP client and its underlying SSH connection.
type SFTPConn struct {
	Client *sftp.Client
	conn   *ssh.Client
}

// Close shuts down the SFTP client and the underlying SSH connection.
func (c *SFTPConn) Close() error {
	err := c.Client.Close()
	if connErr := c.conn.Close(); connErr != nil && err == nil {
		err = connErr
	}
	return err
}

// NewSFTPClient dials the internal SSH server and opens an SFTP session.
func NewSFTPClient(addr string) (*SFTPConn, error) {
	return NewSFTPClientWithConfig(addr, sshserver.ClientConfig())
}

// NewSFTPClientWithConfig dials with the given config and opens SFTP.
func NewSFTPClientWithConfig(addr string, config *ssh.ClientConfig) (*SFTPConn, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ssh ClientConfig")
	}
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("ssh dial for sftp: %w", err)
	}
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("sftp client: %w", err)
	}
	return &SFTPConn{Client: sftpClient, conn: client}, nil
}
