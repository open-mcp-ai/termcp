package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"

	"golang.org/x/term"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/config"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/logansi"
	mcpmod "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
)

func bindHostIsAll(bind string) bool {
	switch strings.TrimSpace(bind) {
	case "", "0.0.0.0", "::", "[::]":
		return true
	default:
		return false
	}
}

// nonLoopbackUnicastIPv4s lists unique IPv4 addresses on up, non-loopback interfaces.
func nonLoopbackUnicastIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []string
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			s := v4.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

func logHTTPMain(base string, port int, lanIPv4 []string) {
	slog.Info(base + "/")
	for _, ip := range lanIPv4 {
		slog.Info(fmt.Sprintf("http://%s:%d/", ip, port))
	}
}

func main() {
	cfg := config.Default()
	flag.StringVar(&cfg.Host, "host", cfg.Host, "HTTP bind address (127.0.0.1 = loopback default; 0.0.0.0 = all interfaces)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory for JSON storage")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log verbosity: debug|info|warn|error")
	flag.BoolVar(&cfg.NoInternal, "no-internal", cfg.NoInternal, "Disable the built-in loopback SSH profile (no internal connection)")
		flag.BoolVar(&cfg.MCPManageSSHConfigs, "mcp-manage-ssh-configs", cfg.MCPManageSSHConfigs, "Enable MCP tools to create/edit/delete SSH configs (off by default; passwords/keys are never exposed)")
	flag.Parse()

	if args := flag.Args(); len(args) > 0 {
		fmt.Fprintf(os.Stderr, "unknown arguments: %s\n", strings.Join(args, " "))
		os.Exit(2)
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(buildLogHandler(cfg)))
	slog.Info("termcp server started")

	// Start internal SSH server (in-process, no TCP port) unless disabled.
	var sshSrv *sshserver.Server
	if !cfg.NoInternal {
		sshSrv = sshserver.New()
		if err := sshSrv.Start(); err != nil {
			slog.Error("failed to start SSH server", "err", err)
			os.Exit(1)
		}
	}
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	slog.Info("MCP HTTP:")
	slog.Info("    /sse SSE transport")
	slog.Info("    /stream (streamable HTTP per MCP spec, e.g. Open WebUI)")
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	// Initialize storage and managers
	store := storage.New(cfg.DataDir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, sshSrv)

	sshStore := sshconfig.NewStore(cfg.DataDir)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	mux := http.NewServeMux()
	mainSrv := &http.Server{Addr: addr, Handler: mux}

	forwardMgr := forward.NewForwardManager()
	sessMgr.SetTerminateListener(func(sessionID string) { forwardMgr.CloseBySession(sessionID) })

	mcpSrv := mcpmod.New(sessMgr, msgMgr, sshStore, forwardMgr, mcpserver.WithHTTPServer(mainSrv))
	mcpSrv.NoInternal = cfg.NoInternal
	if cfg.MCPManageSSHConfigs {
		mcpSrv.RegisterSSHConfigWriteTools()
	}
	mux.Handle("GET /sse", mcpSrv.SSEHandler())
	mux.Handle("POST /message", mcpSrv.MessageHandler())
	mux.Handle("/stream", mcpSrv.StreamableHTTPHandler())
	(&webui.Handler{Sessions: sessMgr, SSH: sshStore, ForwardMgr: forwardMgr, NoInternal: cfg.NoInternal}).Register(mux)

	host := strings.TrimSpace(cfg.Host)
	base := fmt.Sprintf("http://%s:%d", host, cfg.Port)
	var lan []string
	if bindHostIsAll(host) {
		lan = nonLoopbackUnicastIPv4s()
	}
	logHTTPMain(base, cfg.Port, lan)

	var shuttingDown atomic.Bool

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		shuttingDown.Store(true)
		slog.Info("shutting down")
		sessMgr.CleanupAll(true)
		if sshSrv != nil {
			sshSrv.Stop()
		}
		mcpSrv.Stop()
	}()

	if err := mcpSrv.Start(addr); err != nil {
		if shuttingDown.Load() && errors.Is(err, http.ErrServerClosed) {
			slog.Info("server stopped")
			return
		}
		slog.Error("failed to start MCP server", "err", err)
		os.Exit(1)
	}
}

func buildLogHandler(cfg *config.Config) slog.Handler {
	var minLevel slog.Level
	switch cfg.LogLevel {
	case "debug":
		minLevel = slog.LevelDebug
	case "warn":
		minLevel = slog.LevelWarn
	case "error":
		minLevel = slog.LevelError
	default:
		minLevel = slog.LevelInfo
	}
	color := term.IsTerminal(int(os.Stderr.Fd()))
	return logansi.NewTextHandler(os.Stderr, logansi.Options{MinLevel: minLevel, Color: color})
}
