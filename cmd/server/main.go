package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/open-mcp-ai/termcp/internal/config"
	mcpmod "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/internal/webui"
)

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
	flag.StringVar(&cfg.Host, "host", cfg.Host, "HTTP server host")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory for JSON storage")
	flag.StringVar(&cfg.SSHHost, "ssh-host", cfg.SSHHost, "Internal SSH server host")
	flag.IntVar(&cfg.SSHPort, "ssh-port", cfg.SSHPort, "Internal SSH server port (0 = random)")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log verbosity: debug|info|warn|error")
	flag.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "Log output format: text|json")
	flag.StringVar(&cfg.AdminHost, "admin-host", cfg.AdminHost, "SSH config admin API bind host")
	flag.IntVar(&cfg.AdminPort, "admin-port", cfg.AdminPort, "SSH config admin HTTP port (0 = disabled; requires admin-token)")
	flag.StringVar(&cfg.AdminToken, "admin-token", cfg.AdminToken, "Bearer / X-Admin-Token for PUT/GET/DELETE /api/ssh-configs")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(buildLogHandler(cfg)))

	// Start internal SSH server
	sshAddr := fmt.Sprintf("%s:%d", cfg.SSHHost, cfg.SSHPort)
	sshSrv := sshserver.New(sshAddr)
	if err := sshSrv.Start(); err != nil {
		slog.Error("failed to start SSH server", "err", err)
		os.Exit(1)
	}
	actualSSHAddr := sshSrv.Addr()
	slog.Info("internal SSH server listening", "addr", actualSSHAddr)

	// Initialize storage and managers
	store := storage.New(cfg.DataDir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(actualSSHAddr, msgMgr, store)

	if err := sshconfig.EnsureInternal(cfg.DataDir); err != nil {
		slog.Error("failed to ensure internal ssh config", "err", err)
		os.Exit(1)
	}
	sshStore := sshconfig.NewStore(cfg.DataDir)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	mux := http.NewServeMux()
	mainSrv := &http.Server{Addr: addr, Handler: mux}
	mcpSrv := mcpmod.New(sessMgr, msgMgr, sshStore, mcpserver.WithHTTPServer(mainSrv))
	mux.Handle("GET /sse", mcpSrv.SSEHandler())
	mux.Handle("POST /message", mcpSrv.MessageHandler())
	(&webui.Handler{Sessions: sessMgr, SSH: sshStore}).Register(mux)

	var adminSrv *http.Server
	if cfg.AdminPort > 0 {
		admin := &sshconfig.AdminHandler{Store: sshStore, Token: cfg.AdminToken}
		adminMux := http.NewServeMux()
		adminMux.Handle("/api/ssh-configs", admin)
		adminMux.Handle("/api/ssh-configs/", admin)
		adminAddr := fmt.Sprintf("%s:%d", cfg.AdminHost, cfg.AdminPort)
		adminSrv = &http.Server{Addr: adminAddr, Handler: adminMux}
		go func() {
			slog.Info("SSH config admin API listening", "addr", adminAddr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("admin API server stopped", "err", err)
			}
		}()
	}

	slog.Info("MCP SSE server listening", "addr", addr)
	slog.Info("web UI", "url", fmt.Sprintf("http://%s/", addr))

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
	var level slog.Level
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.NewJSONHandler(os.Stderr, opts)
	}
	return slog.NewTextHandler(os.Stderr, opts)
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
