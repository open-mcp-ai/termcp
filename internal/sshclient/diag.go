package sshclient

import (
	"errors"
	"net"
	"strings"
)

// DialErrorHint returns a short diagnostic hint for a failed TCP dial, or ""
// when the error is not a transport-level dial failure (auth and handshake
// errors already carry their own precise reason from the SSH layer).
//
// Classification matches on the error string rather than platform-specific
// syscall constants so a single implementation covers Windows (connectex /
// WSA*) and Unix errno messages alike. Hints are user-facing diagnostics, not
// control flow — never branch on them.
func DialErrorHint(err error) string {
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		return ""
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "proxy dial") {
		// dialProxy wraps its own net.OpError, which addresses the SOCKS5 proxy,
		// not the SSH target — a target-oriented hint would mislead.
		return "SOCKS5 proxy unreachable: the TCP connection to the proxy server itself failed. " +
			"Check that the proxy host:port is correct, that the proxy service is running and reachable " +
			"from the machine running termcp (Test-NetConnection <proxy-host> -Port <proxy-port> / nc -zv), " +
			"and that the dial timeout is large enough for the proxy to answer."
	}
	switch {
	case opErr.Timeout() ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "timed out") ||
		strings.Contains(msg, "did not properly respond"):
		return "TCP connect timed out: no reply from the target (packets silently dropped). " +
			"Typical causes: the host is down or on another network (VLAN / Wi-Fi client isolation); " +
			"a firewall silently DROPs this port instead of rejecting; " +
			"fail2ban or sshd source restrictions banned the termcp host after earlier failed attempts; " +
			"or termcp runs in a different network namespace (WSL2 / Docker / another machine) than the shell where plain ssh works. " +
			"Verify from the machine running termcp: Test-NetConnection <host> -Port <port> (Windows) or nc -zv <host> <port>."
	case strings.Contains(msg, "refused"):
		return "connection refused: nothing accepted the connection on that address/port. " +
			"The SSH service may be down, sshd may bind a different interface or port (ListenAddress/Port), or a firewall actively rejected it."
	case strings.Contains(msg, "unreachable") || strings.Contains(msg, "no route"):
		return "network unreachable: no route from the machine running termcp to the target " +
			"(wrong subnet, VPN/interface down, Wi-Fi client isolation, or an IPv6 address " +
			"resolved while no IPv6 route exists)."
	case strings.Contains(msg, "reset") || strings.Contains(msg, "forcibly closed"):
		return "connection reset by peer: the target or a middlebox (firewall/proxy) tore down the connection mid-handshake."
	}
	return ""
}

// DescribeDialError returns the error string extended with a diagnostic hint
// for transport-level (TCP dial) failures. Non-dial errors are returned
// unchanged. Use for user-facing error text (MCP tool results, HTTP bodies,
// connection test results) so bare "i/o timeout" / "connectex ..." messages
// become actionable.
func DescribeDialError(err error) string {
	msg := err.Error()
	if hint := DialErrorHint(err); hint != "" {
		return msg + "\nHint: " + hint
	}
	return msg
}
