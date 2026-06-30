package sshclient

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxySchemeSOCKS5 is the only supported proxy URL scheme.
const ProxySchemeSOCKS5 = "socks5"

// Proxy is a parsed outbound proxy URL used for SSH dialing. Only SOCKS5 is supported.
type Proxy struct {
	Host     string
	Port     int
	Username string
	Password string
}

// ParseProxyURL parses a proxy URL like "socks5://user:pass@host:port".
// The scheme defaults to socks5 and the port defaults to 1080 when omitted.
// An empty string returns nil with no error (no proxy configured).
func ParseProxyURL(raw string) (*Proxy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = ProxySchemeSOCKS5 + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != ProxySchemeSOCKS5 {
		return nil, fmt.Errorf("proxy scheme %q unsupported (only %q)", scheme, ProxySchemeSOCKS5)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("proxy host is required")
	}
	portStr := u.Port()
	if portStr == "" {
		portStr = "1080"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid proxy port %q", portStr)
	}
	p := &Proxy{Host: host, Port: port}
	if u.User != nil {
		p.Username = u.User.Username()
		pw, _ := u.User.Password()
		p.Password = pw
	}
	return p, nil
}

// Enabled reports whether a proxy is configured.
func (p *Proxy) Enabled() bool {
	return p != nil && strings.TrimSpace(p.Host) != "" && p.Port > 0
}

// dialProxy opens a net.Conn to targetAddr (host:port) through a SOCKS5 proxy.
func dialProxy(p *Proxy, targetAddr string, timeout time.Duration) (net.Conn, error) {
	proxyAddr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	conn, err := net.DialTimeout("tcp", proxyAddr, timeout)
	if err != nil {
		return nil, fmt.Errorf("proxy dial %s: %w", proxyAddr, err)
	}
	if err := socks5Handshake(conn, p, targetAddr, timeout); err != nil {
		conn.Close()
		return nil, fmt.Errorf("socks5: %w", err)
	}
	return conn, nil
}

func socks5Handshake(conn net.Conn, p *Proxy, targetAddr string, timeout time.Duration) error {
	if timeout > 0 {
		deadline := time.Now().Add(timeout)
		_ = conn.SetDeadline(deadline)
		defer conn.SetDeadline(time.Time{})
	}

	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return fmt.Errorf("split target addr: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid target port %q", portStr)
	}

	user := strings.TrimSpace(p.Username)
	// Method negotiation: offer no-auth (0x00) plus username/password (0x02) when configured.
	if _, err := conn.Write(methodNegotiation(user)); err != nil {
		return fmt.Errorf("write methods: %w", err)
	}
	mr := make([]byte, 2)
	if _, err := io.ReadFull(conn, mr); err != nil {
		return fmt.Errorf("read method: %w", err)
	}
	if mr[0] != 0x05 {
		return fmt.Errorf("bad version 0x%02x", mr[0])
	}
	switch mr[1] {
	case 0x00: // no auth
	case 0x02: // username/password
		if user == "" {
			return errors.New("server requires auth but none configured")
		}
		if err := socks5SendAuth(conn, p); err != nil {
			return err
		}
	default:
		return fmt.Errorf("no acceptable auth method (0x%02x)", mr[1])
	}

	if err := socks5SendConnect(conn, host, port); err != nil {
		return err
	}
	return socks5ReadConnectReply(conn)
}

func methodNegotiation(user string) []byte {
	if user != "" {
		return []byte{0x05, 0x02, 0x00, 0x02}
	}
	return []byte{0x05, 0x01, 0x00}
}

func socks5SendAuth(conn net.Conn, p *Proxy) error {
	u := []byte(strings.TrimSpace(p.Username))
	pw := []byte(p.Password)
	if len(u) > 255 || len(pw) > 255 {
		return errors.New("auth too long")
	}
	buf := []byte{0x01, byte(len(u))}
	buf = append(buf, u...)
	buf = append(buf, byte(len(pw)))
	buf = append(buf, pw...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("write auth: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("read auth: %w", err)
	}
	if resp[0] != 0x01 || resp[1] != 0x00 {
		return errors.New("auth rejected")
	}
	return nil
}

func socks5SendConnect(conn net.Conn, host string, port int) error {
	req := []byte{0x05, 0x01, 0x00} // ver, CONNECT, RSV
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("domain too long (%d)", len(host))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	req = append(req, portBytes...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("write connect: %w", err)
	}
	return nil
}

func socks5ReadConnectReply(conn net.Conn) error {
	rep := make([]byte, 4)
	if _, err := io.ReadFull(conn, rep); err != nil {
		return fmt.Errorf("read reply: %w", err)
	}
	if rep[0] != 0x05 {
		return fmt.Errorf("bad reply version 0x%02x", rep[0])
	}
	if rep[1] != 0x00 {
		return fmt.Errorf("connect failed (code 0x%02x)", rep[1])
	}
	var skip int
	switch rep[3] {
	case 0x01:
		skip = 4
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fmt.Errorf("read bind domain len: %w", err)
		}
		skip = int(l[0])
	case 0x04:
		skip = 16
	default:
		return fmt.Errorf("bad bind addr type 0x%02x", rep[3])
	}
	tail := make([]byte, skip+2) // bind address + port
	if _, err := io.ReadFull(conn, tail); err != nil {
		return fmt.Errorf("read bind addr: %w", err)
	}
	return nil
}
