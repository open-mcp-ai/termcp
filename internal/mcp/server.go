package mcp

import (
	"context"
	"os"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// Server wraps the MCP server and tool handlers.
type Server struct {
	mcpServer   *mcpserver.MCPServer
	sseServer   *mcpserver.SSEServer
	stdioServer *mcpserver.StdioServer
	sessMgr     *session.Manager
	msgMgr      *message.Manager
}

// New creates and configures the MCP server with all tools registered.
func New(sessMgr *session.Manager, msgMgr *message.Manager) *Server {
	s := &Server{
		sessMgr: sessMgr,
		msgMgr:  msgMgr,
	}

	mcpServer := mcpserver.NewMCPServer("interactive-process", "0.1.0")

	mcpServer.AddTool(mcpgo.NewTool("start_process",
		mcpgo.WithDescription("Start an interactive process and return its session info. For long-running commands, use background_send + read_output instead of send_and_read to avoid blocking."),
		mcpgo.WithString("command", mcpgo.Required(), mcpgo.Description("Command to execute")),
		mcpgo.WithArray("args", mcpgo.Description("Command arguments"), mcpgo.WithStringItems()),
		mcpgo.WithString("mode", mcpgo.Description("I/O mode: pty or pipe"), mcpgo.DefaultString("pty")),
		mcpgo.WithString("name", mcpgo.Description("Session name")),
		mcpgo.WithNumber("rows", mcpgo.Description("PTY rows"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("PTY columns"), mcpgo.DefaultNumber(80)),
	), withLogging("start_process", s.handleStartProcess))

	mcpServer.AddTool(mcpgo.NewTool("send_input",
		mcpgo.WithDescription("Send text input to a running interactive process without reading output. Pair with read_output to check the result."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Text to send")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("Append newline after text"), mcpgo.DefaultBool(false)),
	), withLogging("send_input", s.handleSendInput))

	mcpServer.AddTool(mcpgo.NewTool("read_output",
		mcpgo.WithDescription("Read new output from an interactive process since last read. Use timeout ≤ 3 seconds when managing multiple sessions — poll each in rotation."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("Remove ANSI escape codes"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Seconds to wait for new output (max 60)"), mcpgo.DefaultNumber(5)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Max lines to return (0 = unlimited)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("reader_id", mcpgo.Description("Reader ID (0 = default shared reader)"), mcpgo.DefaultNumber(0)),
	), withLogging("read_output", s.handleReadOutput))

	mcpServer.AddTool(mcpgo.NewTool("background_send",
		mcpgo.WithDescription("Send input to a process without waiting for output. Returns immediately. Use this instead of send_and_read when you don't need the response right away, especially for long-running commands. Follow up with read_output to check results."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Text to send")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("Append newline after text"), mcpgo.DefaultBool(false)),
	), withLogging("background_send", s.handleBackgroundSend))

	mcpServer.AddTool(mcpgo.NewTool("send_and_read",
		mcpgo.WithDescription("Send input to a process and immediately read its response. WARNING: blocks until output arrives or timeout. For long-running commands (sleep, builds, installs), use background_send + read_output instead to avoid blocking."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("text", mcpgo.Required(), mcpgo.Description("Text to send")),
		mcpgo.WithBoolean("press_enter", mcpgo.Description("Append newline after text"), mcpgo.DefaultBool(false)),
		mcpgo.WithBoolean("strip_ansi", mcpgo.Description("Remove ANSI escape codes"), mcpgo.DefaultBool(true)),
		mcpgo.WithNumber("timeout", mcpgo.Description("Seconds to wait for response (max 60)"), mcpgo.DefaultNumber(5)),
		mcpgo.WithNumber("max_lines", mcpgo.Description("Max lines to return (0 = unlimited)"), mcpgo.DefaultNumber(0)),
		mcpgo.WithNumber("reader_id", mcpgo.Description("Reader ID (0 = default shared reader)"), mcpgo.DefaultNumber(0)),
	), withLogging("send_and_read", s.handleSendAndRead))

	mcpServer.AddTool(mcpgo.NewTool("list_sessions",
		mcpgo.WithDescription("List all interactive process sessions"),
	), withLogging("list_sessions", s.handleListSessions))

	mcpServer.AddTool(mcpgo.NewTool("get_session_info",
		mcpgo.WithDescription("Get detailed information about a session"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
	), withLogging("get_session_info", s.handleGetSessionInfo))

	mcpServer.AddTool(mcpgo.NewTool("terminate_process",
		mcpgo.WithDescription("Terminate an interactive process"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithBoolean("force", mcpgo.Description("Use SIGKILL directly"), mcpgo.DefaultBool(false)),
		mcpgo.WithNumber("grace_period", mcpgo.Description("Seconds to wait after SIGTERM"), mcpgo.DefaultNumber(5)),
	), withLogging("terminate_process", s.handleTerminateProcess))

	mcpServer.AddTool(mcpgo.NewTool("delete_session",
		mcpgo.WithDescription("Delete an exited session from the registry"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
	), withLogging("delete_session", s.handleDeleteSession))

	mcpServer.AddTool(mcpgo.NewTool("resize_pty",
		mcpgo.WithDescription("Resize the PTY terminal dimensions for a session"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithNumber("rows", mcpgo.Description("Row count"), mcpgo.DefaultNumber(24)),
		mcpgo.WithNumber("cols", mcpgo.Description("Column count"), mcpgo.DefaultNumber(80)),
	), withLogging("resize_pty", s.handleResizePty))

	mcpServer.AddTool(mcpgo.NewTool("detect_shell",
		mcpgo.WithDescription("Detect the available shell on the target system. Returns shell path, family (unix/powershell/cmd), and a hint for agents. Use this before start_process to choose the correct command and args for the platform."),
	), withLogging("detect_shell", s.handleDetectShell))

	mcpServer.AddTool(mcpgo.NewTool("list_messages",
		mcpgo.WithDescription("List the message index for a session"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
	), withLogging("list_messages", s.handleListMessages))

	mcpServer.AddTool(mcpgo.NewTool("get_message",
		mcpgo.WithDescription("Get the content of one or more messages"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithArray("message_ids", mcpgo.Description("Message IDs to retrieve"), mcpgo.WithStringItems()),
	), withLogging("get_message", s.handleGetMessage))

	mcpServer.AddTool(mcpgo.NewTool("register_reader",
		mcpgo.WithDescription("Register a new independent reader for a session's output. Each reader has its own cursor."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
	), withLogging("register_reader", s.handleRegisterReader))

	mcpServer.AddTool(mcpgo.NewTool("unregister_reader",
		mcpgo.WithDescription("Unregister a reader when it is no longer needed"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithNumber("reader_id", mcpgo.Required(), mcpgo.Description("Reader ID to unregister")),
	), withLogging("unregister_reader", s.handleUnregisterReader))

	mcpServer.AddTool(mcpgo.NewTool("upload_file",
		mcpgo.WithDescription("Upload a file to the process environment via SFTP. Max 1MB. For large files, use send_input with curl/wget instead."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("content_base64", mcpgo.Required(), mcpgo.Description("File content encoded as base64")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Destination path in the process environment")),
	), withLogging("upload_file", s.handleUploadFile))

	mcpServer.AddTool(mcpgo.NewTool("download_file",
		mcpgo.WithDescription("Download a file from the process environment via SFTP. Text files returned as plain text, binary files as base64. Max 1MB."),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Path of the file to download")),
	), withLogging("download_file", s.handleDownloadFile))

	mcpServer.AddTool(mcpgo.NewTool("list_files",
		mcpgo.WithDescription("List files and directories at a path in the process environment via SFTP"),
		mcpgo.WithString("session_id", mcpgo.Required(), mcpgo.Description("Session ID")),
		mcpgo.WithString("remote_path", mcpgo.Required(), mcpgo.Description("Directory path to list")),
	), withLogging("list_files", s.handleListFiles))

	s.mcpServer = mcpServer
	return s
}

// StartSSE begins serving MCP over SSE on the given address.
func (s *Server) StartSSE(addr string) error {
	s.sseServer = mcpserver.NewSSEServer(s.mcpServer)
	return s.sseServer.Start(addr)
}

// StartStdio begins serving MCP over stdin/stdout.
func (s *Server) StartStdio() error {
	s.stdioServer = mcpserver.NewStdioServer(s.mcpServer)
	return s.stdioServer.Listen(context.Background(), os.Stdin, os.Stdout)
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() error {
	if s.sseServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.sseServer.Shutdown(ctx)
	}
	return nil
}
