package session

import (
	"time"

	"github.com/open-mcp-ai/termcp/internal/sshclient"
)

// TestResult reports a connection probe outcome for an SSH profile.
type TestResult struct {
	OK         bool   `json:"ok"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
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
		res.Error = err.Error()
		return res
	}
	defer sshclient.DrainClosers(closers)
	defer client.Close()

	// Verify an exec channel can be opened (auth already succeeded during
	// handshake). NewSession is portable across Unix/Windows SSH servers.
	sess, err := client.NewSession()
	if err != nil {
		res.Error = "open session channel: " + err.Error()
		return res
	}
	sess.Close()
	res.OK = true
	return res
}
