package session

import (
	"log/slog"
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
)

// TestResult reports a connection probe outcome for an SSH profile.
type TestResult struct {
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// logTestFailure surfaces a connection-test failure on the termcp terminal;
// the Web UI caller otherwise only sees it in the HTTP response.
func logTestFailure(r *RemoteSSH, err error) {
	slog.Warn("ssh connection test failed",
		"host", r.Host,
		"port", r.Port,
		"err", err,
	)
}

// TestConnection dials the full chain described by r (SOCKS5 proxy, bastion
// jumps, target host), verifies that an exec session channel can be opened on
// the target, then closes everything. It is used by the Web UI "test
// connection" feature and never leaves a connection open on success.
func TestConnection(r *RemoteSSH) *TestResult {
	start := time.Now()
	res := &TestResult{}
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	client, closers, err := buildChainClient(r)
	if err != nil {
		// Describe the error (with a hint) for the Web UI caller.
		logTestFailure(r, err)
		res.Error = sshclient.DescribeDialError(err)
		return res
	}
	defer sshclient.DrainClosers(closers)
	defer client.Close()

	// Verify an exec channel can be opened (auth already succeeded during
	// handshake). NewSession is portable across Unix/Windows SSH servers.
	sess, err := client.NewSession()
	if err != nil {
		logTestFailure(r, err)
		res.Error = "open session channel: " + err.Error()
		return res
	}
	sess.Close()
	res.OK = true
	return res
}
