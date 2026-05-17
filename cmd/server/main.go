package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/open-mcp-ai/termcp/internal/config"
	mcpmod "github.com/open-mcp-ai/termcp/internal/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

func main() {
	cfg := config.Default()
	var transport string
	flag.StringVar(&transport, "transport", "sse", "Transport mode: stdio or sse")
	flag.StringVar(&cfg.Host, "host", cfg.Host, "HTTP server host")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "HTTP server port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data directory for JSON storage")
	flag.StringVar(&cfg.SSHHost, "ssh-host", cfg.SSHHost, "Internal SSH server host")
	flag.IntVar(&cfg.SSHPort, "ssh-port", cfg.SSHPort, "Internal SSH server port (0 = random)")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log verbosity: debug|info|warn|error")
	flag.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "Log output format: text|json")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid config: %v\n", err)
		os.Exit(2)
	}

	// In stdio mode, force logs to stderr with error level only
	// to avoid polluting stdout (which is the MCP JSON-RPC channel)
	if transport == "stdio" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	} else {
		slog.SetDefault(slog.New(buildLogHandler(cfg)))
	}

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

	// Create MCP server
	mcpSrv := mcpmod.New(sessMgr, msgMgr)

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
		mcpSrv.Stop()
	}()

	if transport == "stdio" {
		// Stdio mode: MCP over stdin/stdout
		if err := mcpSrv.StartStdio(); err != nil {
			slog.Error("stdio server error", "err", err)
			os.Exit(1)
		}
	} else {
		// SSE mode: MCP over HTTP
		addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
		slog.Info("MCP SSE server listening", "addr", addr)
		if err := mcpSrv.StartSSE(addr); err != nil {
			if shuttingDown.Load() && errors.Is(err, http.ErrServerClosed) {
				slog.Info("server stopped")
				return
			}
			slog.Error("failed to start MCP server", "err", err)
			os.Exit(1)
		}
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
