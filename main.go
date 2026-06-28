package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/forward"

	"github.com/open-mcp-ai/termcp/internal/config"
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

// sshURLFromListenAddr turns "host:port" into ssh://host:port (IPv6 host gets brackets).
func sshURLFromListenAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "ssh://" + addr
	}
	if strings.Contains(host, ":") {
		return fmt.Sprintf("ssh://[%s]:%s", host, port)
	}
	return fmt.Sprintf("ssh://%s:%s", host, port)
}

func logHTTPMain(base string, port int, lanIPv4 []string) {
	slog.Info(base + "/")
	for _, ip := range lanIPv4 {
		slog.Info(fmt.Sprintf("http://%s:%d/", ip, port))
	}
}

func main() {
if len(os.Args) >= 2 && os.Args[1] == "ssh-config" {
		base := filepath.Base(os.Args[0])
		if len(os.Args) < 3 {
			printSSHConfigUsage(base)
			os.Exit(2)
		}
		switch os.Args[2] {
		case "init":
			if len(os.Args) < 4 {
				printSSHConfigUsage(base)
				os.Exit(2)
			}
			runSSHConfigInit(os.Args[3:])
		case "list":
			runSSHConfigList(os.Args[3:])
		default:
			printSSHConfigUsage(base)
			os.Exit(2)
		}
		return
	}

	cfg := config.Default()
	flag.StringVar(&cfg.Host, "host", cfg.Host, "HTTP bind address (127.0.0.1 = loopback default; 0.0.0.0 = all interfaces)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory for JSON storage")
	flag.StringVar(&cfg.SSHHost, "ssh-host", cfg.SSHHost, "Internal SSH server host")
	flag.IntVar(&cfg.SSHPort, "ssh-port", cfg.SSHPort, "Internal SSH server port (0 = random)")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log verbosity: debug|info|warn|error")
	flag.StringVar(&cfg.AdminHost, "admin-host", cfg.AdminHost, "SSH config admin API bind host")
	flag.IntVar(&cfg.AdminPort, "admin-port", cfg.AdminPort, "SSH config admin HTTP port (0 = disabled; requires admin-token)")
	flag.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "Bearer / X-Admin-Token for PUT/GET/DELETE /api/ssh-configs")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(buildLogHandler(cfg)))
	slog.Info("termcp server started")

	// Start internal SSH server
	sshAddr := fmt.Sprintf("%s:%d", cfg.SSHHost, cfg.SSHPort)
	sshSrv := sshserver.New(sshAddr)
	if err := sshSrv.Start(); err != nil {
		slog.Error("failed to start SSH server", "err", err)
		os.Exit(1)
	}
	actualSSHAddr := sshSrv.Addr()
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")
	slog.Info("SSH local server:")
	slog.Info("    " + sshURLFromListenAddr(actualSSHAddr))
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	slog.Info("MCP HTTP:")
	slog.Info("    /sse SSE transport")
	slog.Info("    /stream (streamable HTTP per MCP spec, e.g. Open WebUI)")
	slog.Info("- - - - - - - - - - - - - - - - - - - - - - - - - - - - - - ")

	// Initialize storage and managers
	store := storage.New(cfg.DataDir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(actualSSHAddr, msgMgr, store, sshSrv)

	if err := sshconfig.EnsureInternal(cfg.DataDir); err != nil {
		slog.Error("failed to ensure internal ssh config", "err", err)
		os.Exit(1)
	}
	sshStore := sshconfig.NewStore(cfg.DataDir)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	mux := http.NewServeMux()
	mainSrv := &http.Server{Addr: addr, Handler: mux}

	forwardMgr := forward.NewForwardManager()

	mcpSrv := mcpmod.New(sessMgr, msgMgr, sshStore, forwardMgr, mcpserver.WithHTTPServer(mainSrv))
	mux.Handle("GET /sse", mcpSrv.SSEHandler())
	mux.Handle("POST /message", mcpSrv.MessageHandler())
	mux.Handle("/stream", mcpSrv.StreamableHTTPHandler())
	(&webui.Handler{Sessions: sessMgr, SSH: sshStore, ForwardMgr: forwardMgr}).Register(mux)

	var adminSrv *http.Server
	if cfg.AdminPort > 0 {
		admin := &sshconfig.AdminHandler{Store: sshStore, Token: cfg.AdminToken}
		adminMux := http.NewServeMux()
		adminMux.Handle("/api/ssh-configs", admin)
		adminMux.Handle("/api/ssh-configs/", admin)
		adminAddr := fmt.Sprintf("%s:%d", cfg.AdminHost, cfg.AdminPort)
		adminSrv = &http.Server{Addr: adminAddr, Handler: adminMux}
		go func() {
			slog.Info("http admin", "listen", adminAddr, "paths", "/api/ssh-configs")
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin API server stopped", "err", err)
			}
		}()
	}

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
		sshSrv.Stop()
		if adminSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = adminSrv.Shutdown(ctx)
			cancel()
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

func printSSHConfigUsage(program string) {
	fmt.Fprintf(os.Stderr, "usage:\n")
	fmt.Fprintf(os.Stderr, "  %s ssh-config init <name> [-data-dir path]\n", program)
	fmt.Fprintf(os.Stderr, "  %s ssh-config list [-data-dir path]\n", program)
}

func parseSSHConfigDataDir(setName string, args []string) string {
	fs := flag.NewFlagSet(setName, flag.ExitOnError)
	d := fs.String("data-dir", config.Default().DataDir, "termcp data directory")
	_ = fs.Parse(args)
	return *d
}

func runSSHConfigInit(args []string) {
	if len(args) < 1 {
		printSSHConfigUsage(filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	name := args[0]
	dataDir := parseSSHConfigDataDir("ssh-config init", args[1:])
	if err := sshconfig.InitRemoteSkeleton(dataDir, name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	p := filepath.Join(dataDir, "ssh_configs", name, "config.json")
	fmt.Println("created", p)
}

func runSSHConfigList(args []string) {
	dataDir := parseSSHConfigDataDir("ssh-config list", args)
	if err := sshconfig.EnsureInternal(dataDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	store := sshconfig.NewStore(dataDir)
	names, err := store.List()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, n := range names {
		fmt.Println(n)
	}
}

