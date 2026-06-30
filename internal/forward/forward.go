package forward

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// ForwardManager manages port forwards for SSH connections.
type ForwardManager struct {
	mu       sync.Mutex
	forwards map[string]*forwardState
	onChange func()
}

// NewForwardManager creates a ForwardManager.
func NewForwardManager() *ForwardManager {
	return &ForwardManager{
		forwards: make(map[string]*forwardState),
	}
}

// SetOnChange sets a callback invoked when forwards are added or removed.
func (fm *ForwardManager) SetOnChange(fn func()) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.onChange = fn
}

func (fm *ForwardManager) notifyChange() {
	fm.mu.Lock()
	fn := fm.onChange
	fm.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ForwardDirection describes the type of a port forward.
type ForwardDirection string

const (
	DirectionLocal   ForwardDirection = "local"
	DirectionRemote  ForwardDirection = "remote"
	DirectionDynamic ForwardDirection = "dynamic"
)

// ForwardInfo is the public-facing view of a port forward (returned in JSON APIs).
type ForwardInfo struct {
	ForwardID  string           `json:"forward_id"`
	Direction  ForwardDirection `json:"direction"`
	SSHConfig  string           `json:"ssh_config"`
	ListenAddr string           `json:"listen_addr"`
	TargetAddr string           `json:"target_addr"`
	Status     string           `json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
}

// forwardState holds the runtime state of a port forward.
type forwardState struct {
	ForwardInfo
	listener   net.Listener
	cancelFunc context.CancelFunc
}

var forwardCounter struct {
	mu    sync.Mutex
	count int
}

// ForwardID generates a unique forward id.
func ForwardID(direction ForwardDirection) string {
	forwardCounter.mu.Lock()
	defer forwardCounter.mu.Unlock()
	forwardCounter.count++
	return fmt.Sprintf("fw-%s-%d-%d", direction, time.Now().UnixMilli(), forwardCounter.count)
}
// tunnel connections back via smux to termcp's remoteHost:remotePort.
// LocalForwardSSH creates a local forward (ssh -L): termcp listens on a local port and
// tunnels each connection through the SSH client's direct-tcpip channel to the remote target.
// The ctx is monitored; when cancelled all active connections are closed.
// Returns a local listener and a cancel function for lifecycle management.
func LocalForwardSSH(ctx context.Context, client *ssh.Client, remoteHost string, remotePort int, localPort int) (*ForwardInfo, net.Listener, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, nil, err
		}
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}

	localListener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return nil, nil, err
	}

	actualPort := localListener.Addr().(*net.TCPAddr).Port
	target := net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort))

	fwID := ForwardID(DirectionLocal)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionLocal,
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
		TargetAddr: target,
		Status:     "active",
		CreatedAt:  time.Now(),
	}

	go func() {
		defer localListener.Close()
		for {
			localConn, err := localListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer localConn.Close()
				remoteConn, err := client.Dial("tcp", target)
				if err != nil {
					slog.Error("ssh forward dial failed", "target", target, "err", err)
					return
				}
				defer remoteConn.Close()
				// Close both sides when context is cancelled (forward deleted).
				go func() {
					<-ctx.Done()
					localConn.Close()
					remoteConn.Close()
				}()
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { io.Copy(remoteConn, localConn); wg.Done() }()
				go func() { io.Copy(localConn, remoteConn); wg.Done() }()
				wg.Wait()
			}()
		}
	}()

	return fw, localListener, nil
}

// RemoteForwardSSH creates a remote forward (ssh -R): the remote SSH server listens on
// agentHost:agentPort and tunnels connections back through SSH to termcp's remoteHost:remotePort.
// The ctx is monitored; when cancelled all active connections are closed.
// Returns the local tunnel listener for lifecycle management.
func RemoteForwardSSH(ctx context.Context, client *ssh.Client, agentHost string, agentPort int, remoteHost string, remotePort int) (*ForwardInfo, net.Listener, error) {
	listener, err := client.Listen("tcp", fmt.Sprintf("%s:%d", agentHost, agentPort))
	if err != nil {
		return nil, nil, fmt.Errorf("ssh local forward: %w", err)
	}

	fwID := ForwardID(DirectionRemote)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionRemote,
		ListenAddr: fmt.Sprintf("%s:%d", agentHost, agentPort),
		TargetAddr: fmt.Sprintf("%s:%d", remoteHost, remotePort),
		Status:     "active",
		CreatedAt:  time.Now(),
	}

	go func() {
		defer listener.Close()
		for {
			agentConn, err := listener.Accept()
			if err != nil {
				return
			}
			remoteConn, err := net.Dial("tcp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", remotePort)))
			if err != nil {
				agentConn.Close()
				continue
			}
			go func() {
				defer agentConn.Close()
				defer remoteConn.Close()
				go func() {
					<-ctx.Done()
					agentConn.Close()
					remoteConn.Close()
				}()
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { io.Copy(remoteConn, agentConn); wg.Done() }()
				go func() { io.Copy(agentConn, remoteConn); wg.Done() }()
				wg.Wait()
			}()
		}
	}()

	return fw, listener, nil
}

// CreateLocal creates a remote forward via the best available path.
func (fm *ForwardManager) CreateLocal(sshConfig string, remoteHost string, remotePort int, localPort int, sshClient *ssh.Client) (*ForwardInfo, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := LocalForwardSSH(ctx, sshClient, remoteHost, remotePort, localPort)
	if err != nil {
		cancel()
		return nil, err
	}
	fw.SSHConfig = sshConfig
	fm.mu.Lock()
	fm.forwards[fw.ForwardID] = &forwardState{ForwardInfo: *fw, listener: ln, cancelFunc: cancel}
	fm.mu.Unlock()
	return fw, nil
}

// CreateRemote creates a local forward via the best available path.
func (fm *ForwardManager) CreateRemote(sshConfig string, localHost string, localPort int, remoteHost string, remotePort int, sshClient *ssh.Client) (*ForwardInfo, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("no SSH client available for %q", sshConfig)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fw, ln, err := RemoteForwardSSH(ctx, sshClient, localHost, localPort, remoteHost, remotePort)
	if err != nil {
		cancel()
		return nil, err
	}
	fw.SSHConfig = sshConfig
	fm.mu.Lock()
	fm.forwards[fw.ForwardID] = &forwardState{ForwardInfo: *fw, listener: ln, cancelFunc: cancel}
	fm.mu.Unlock()
	return fw, nil
}

// List returns all forward infos.
func (fm *ForwardManager) List() []ForwardInfo {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	out := make([]ForwardInfo, 0, len(fm.forwards))
	for _, fw := range fm.forwards {
		out = append(out, fw.ForwardInfo)
	}
	return out
}

// DynamicForwardSSH starts a SOCKS5 proxy using an SSH client's dialer.
// The ctx is used to stop the SOCKS5 server and close active connections.
// Returns a listener for lifecycle management.
func DynamicForwardSSH(ctx context.Context, client *ssh.Client, localPort int) (*ForwardInfo, net.Listener, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil { return nil, nil, err }
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil { return nil, nil, err }
	actualPort := ln.Addr().(*net.TCPAddr).Port
	slog.Info("SOCKS5 listener started", "addr", ln.Addr().String(), "port", actualPort)

	fwID := ForwardID(DirectionDynamic)
	fw := &ForwardInfo{
		ForwardID:  fwID,
		Direction:  DirectionDynamic,
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
		TargetAddr: "SOCKS5",
		Status:     "active",
		CreatedAt:  time.Now(),
	}
	go serveSOCKS5(ctx, ln, func(target string) (net.Conn, error) {
		conn, err := client.Dial("tcp", target)
		slog.Info("SOCKS5 dial result", "target", target, "err", err)
		return conn, err
	})
	return fw, ln, nil
}

// serveSOCKS5 accepts connections and proxies them through the dialer function.
func serveSOCKS5(ctx context.Context, ln net.Listener, dialer func(target string) (net.Conn, error)) {
	defer ln.Close()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := ln.Accept()
		if err != nil {
			slog.Error("SOCKS5 accept failed", "err", err)
			return
		}
		go func() {
			defer conn.Close()
			// Read SOCKS5 request without sending response yet
			target, err := socks5ReadRequest(conn)
			if err != nil {
				slog.Error("SOCKS5 read request failed", "err", err)
				return
			}
			// Dial target first
			remote, err := dialer(target)
			if err != nil {
				slog.Error("SOCKS5 dial failed", "target", target, "err", err)
				conn.Write(socks5Reply(1, nil)) // general failure
				return
			}
			defer remote.Close()
			// Send success response AFTER dial succeeds
			if _, err := conn.Write(socks5Reply(0, remote.LocalAddr())); err != nil {
				slog.Error("SOCKS5 reply write failed", "err", err)
				return
			}
			// Close both sides when context is cancelled.
			go func() {
				<-ctx.Done()
				conn.Close()
				remote.Close()
			}()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { io.Copy(remote, conn); wg.Done() }()
			go func() { io.Copy(conn, remote); wg.Done() }()
			wg.Wait()
		}()
	}
}

func socks5ReadRequest(conn net.Conn) (string, error) {
	buf := make([]byte, 263)
	// Read auth methods
	if _, err := io.ReadFull(conn, buf[:2]); err != nil { return "", err }
	if buf[0] != 5 { return "", fmt.Errorf("not SOCKS5") }
	nmethods := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil { return "", err }
	// Reply: no auth
	if _, err := conn.Write([]byte{5, 0}); err != nil { return "", err }
	// Read request
	if _, err := io.ReadFull(conn, buf[:4]); err != nil { return "", err }
	if buf[1] != 1 { return "", fmt.Errorf("only CONNECT supported") }
	var host string
	switch buf[3] {
	case 1: // IPv4
		if _, err := io.ReadFull(conn, buf[:4]); err != nil { return "", err }
		host = net.IP(buf[:4]).String()
	case 3: // Domain
		if _, err := io.ReadFull(conn, buf[:1]); err != nil { return "", err }
		l := int(buf[0])
		if _, err := io.ReadFull(conn, buf[:l]); err != nil { return "", err }
		host = string(buf[:l])
	case 4: // IPv6
		if _, err := io.ReadFull(conn, buf[:16]); err != nil { return "", err }
		host = net.IP(buf[:16]).String()
	default:
		return "", fmt.Errorf("unsupported address type %d", buf[3])
	}
	if _, err := io.ReadFull(conn, buf[:2]); err != nil { return "", err }
	port := int(buf[0])<<8 | int(buf[1])
	return net.JoinHostPort(host, fmt.Sprintf("%d", port)), nil
}

func socks5Reply(rep byte, addr net.Addr) []byte {
	// Always return IPv4 0.0.0.0:0 for simplicity
	return []byte{5, rep, 0, 1, 0, 0, 0, 0, 0, 0}
}

// DynamicForwardLocal starts a SOCKS5 proxy using net.Dial from termcp directly.
func (fm *ForwardManager) DynamicForwardLocal(localPort int) (*ForwardInfo, error) {
	if localPort == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil { return nil, err }
		localPort = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil { return nil, err }
	actualPort := ln.Addr().(*net.TCPAddr).Port
	slog.Info("SOCKS5 local listener started", "addr", ln.Addr().String())

	fwID := ForwardID(DirectionDynamic)
	ctx, cancel := context.WithCancel(context.Background())
	fw := &forwardState{
		ForwardInfo: ForwardInfo{
			ForwardID: fwID, Direction: DirectionDynamic,
			SSHConfig: "internal", ListenAddr: fmt.Sprintf("127.0.0.1:%d", actualPort),
			TargetAddr: "SOCKS5", Status: "active", CreatedAt: time.Now(),
		},
		listener: ln, cancelFunc: cancel,
	}
	go serveSOCKS5(ctx, ln, func(target string) (net.Conn, error) {
		conn, err := net.Dial("tcp", target)
		slog.Info("SOCKS5 local dial", "target", target, "err", err)
		return conn, err
	})
	fm.mu.Lock()
	fm.forwards[fwID] = fw
	fm.mu.Unlock()
	return &fw.ForwardInfo, nil
}

// RegisterForward adds an externally-created forward to the manager (for list_forwards visibility).
func (fm *ForwardManager) RegisterForward(fw *ForwardInfo) {
	fm.mu.Lock()
	fm.forwards[fw.ForwardID] = &forwardState{ForwardInfo: *fw}
	fm.mu.Unlock()
	fm.notifyChange()
}

// RegisterForwardFull registers a forward with its listener and cancel function for proper cleanup.
func (fm *ForwardManager) RegisterForwardFull(fw *ForwardInfo, ln net.Listener, cancel context.CancelFunc) {
	fm.mu.Lock()
	fm.forwards[fw.ForwardID] = &forwardState{ForwardInfo: *fw, listener: ln, cancelFunc: cancel}
	fm.mu.Unlock()
	fm.notifyChange()
}

// Close closes a forward by ID and cleans up resources.
func (fm *ForwardManager) Close(forwardID string) error {
	fm.mu.Lock()
	fw, ok := fm.forwards[forwardID]
	if !ok {
		fm.mu.Unlock()
		slog.Warn("forward close: not found", "forward_id", forwardID)
		return fmt.Errorf("forward %q not found", forwardID)
	}
	delete(fm.forwards, forwardID)
	fm.mu.Unlock()

	slog.Info("forward close: removed from map", "forward_id", forwardID, "has_cancel", fw.cancelFunc != nil, "has_listener", fw.listener != nil)
	if fw.cancelFunc != nil {
		fw.cancelFunc()
		slog.Info("forward close: cancel called", "forward_id", forwardID)
	}
	if fw.listener != nil {
		slog.Info("forward close: closing listener", "forward_id", forwardID, "listen_addr", fmt.Sprintf("%v", fw.listener.Addr()))
		err := fw.listener.Close()
		slog.Info("forward close: listener closed", "forward_id", forwardID, "err", err)
	}
	return nil
}
