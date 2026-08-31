package sshclient

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// dialOpError builds the transport-level error shape every classifier matches
// on: *net.OpError from a dial, optionally wrapped by callers (proxy, fmt).
func dialOpError(ip net.IP, port int, err error) *net.OpError {
	return &net.OpError{
		Op:   "dial",
		Net:  "tcp",
		Addr: &net.TCPAddr{IP: ip, Port: port},
		Err:  err,
	}
}

func TestDialErrorHint_Timeout(t *testing.T) {
	// Windows connectex timeout text: no Timeout() method on Err, so this
	// exercises the string fallback rather than opErr.Timeout().
	err := dialOpError(net.IPv4(192, 168, 0, 145), 22,
		fmt.Errorf("connectex: A connection attempt failed because the connected party did not properly respond after a period of time, or established connection failed because connected host has failed to respond"))
	got := DialErrorHint(err)
	if got == "" {
		t.Fatal("expected a hint for a timeout-shaped OpError")
	}
	if !strings.Contains(got, "timed out") {
		t.Fatalf("hint should mention the timeout, got %q", got)
	}
}

func TestDialErrorHint_ProxyDialTimeout(t *testing.T) {
	// dialProxy wraps the OpError that addresses the proxy, not the SSH target.
	opErr := dialOpError(net.IPv4(127, 0, 0, 1), 1080, errors.New("i/o timeout"))
	wrapped := fmt.Errorf("proxy dial 127.0.0.1:1080: %w", opErr)
	got := DialErrorHint(wrapped)
	if !strings.Contains(got, "proxy") {
		t.Fatalf("hint should point at the proxy, got %q", got)
	}
	if strings.Contains(got, "target is down") {
		t.Fatalf("hint must not misattribute a proxy failure to the SSH target, got %q", got)
	}
}

func TestDialErrorHint_Refused(t *testing.T) {
	err := dialOpError(net.IPv4(127, 0, 0, 1), 22, errors.New("connection refused"))
	got := DialErrorHint(err)
	if !strings.Contains(got, "refused") {
		t.Fatalf("hint should mention refusal, got %q", got)
	}
}

func TestDialErrorHint_Unreachable(t *testing.T) {
	err := dialOpError(net.IPv4(10, 255, 255, 1), 22, errors.New("no route to host"))
	got := DialErrorHint(err)
	if !strings.Contains(got, "unreachable") {
		t.Fatalf("hint should mention unreachable, got %q", got)
	}
}

func TestDialErrorHint_NonDialErrorIsEmpty(t *testing.T) {
	if got := DialErrorHint(errors.New("ssh handshake: handshake failed")); got != "" {
		t.Fatalf("expected no hint for handshake errors, got %q", got)
	}
	if got := DialErrorHint(nil); got != "" {
		t.Fatalf("expected no hint for nil, got %q", got)
	}
}

func TestDescribeDialError_AppendsHint(t *testing.T) {
	err := dialOpError(net.IPv4(192, 168, 0, 145), 22, errors.New("i/o timeout"))
	got := DescribeDialError(err)
	if !strings.HasPrefix(got, "dial tcp") || !strings.Contains(got, "i/o timeout") {
		t.Fatalf("original error text must be preserved as prefix, got %q", got)
	}
	if !strings.Contains(got, "\nHint: ") {
		t.Fatalf("expected appended Hint line, got %q", got)
	}
}

func TestDescribeDialError_PlainErrorUnchanged(t *testing.T) {
	err := errors.New("ssh_user is required for remote SSH")
	if got := DescribeDialError(err); got != err.Error() {
		t.Fatalf("non-dial error should be unchanged, got %q", got)
	}
}

func TestDialErrorHint_RealDialTimeout(t *testing.T) {
	// Dial an unroutable TEST-NET address with a short timeout to exercise the
	// real net.OpError path end to end.
	_, err := net.DialTimeout("tcp", "192.0.2.1:9", 300*time.Millisecond)
	if err == nil {
		t.Skip("test network unexpectedly reachable")
	}
	got := DialErrorHint(err)
	if got == "" {
		t.Fatalf("expected a hint for real dial error %v", err)
	}
}
