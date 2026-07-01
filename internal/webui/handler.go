package webui

import (
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"golang.org/x/crypto/ssh"
	"strconv"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/sftp"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

//go:embed assets
var embeddedAssets embed.FS

// embeddedStaticServer serves index.html, inline page script, and vendor/xterm from embed.FS at URL / and /vendor/... .
func embeddedStaticServer() http.Handler {
	root, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic("webui: embed assets: " + err.Error())
	}
	return http.FileServer(http.FS(root))
}

// Handler serves the browser UI and JSON/SSE APIs at / and /api/... .
type Handler struct {
	Sessions   *session.Manager
	SSH        *sshconfig.Store
	ForwardMgr *forward.ForwardManager

	sessHub *sessionListHub
}

// Register mounts /api/... and / (HTML/CSS/JS via embed.FS + http.FileServer).
// Call after MCP routes (/sse, /message) so those paths are not shadowed.
func (h *Handler) Register(mux *http.ServeMux) {
	if h.Sessions != nil {
		h.Sessions.SetSessionListListener(h.sessionHub().broadcast)
	}
	if h.ForwardMgr != nil {
		h.ForwardMgr.SetOnChange(h.sessionHub().broadcast)
	}
	mux.HandleFunc("GET /api/connection-templates", h.handleConnectionTemplates)
	mux.HandleFunc("GET /api/connections", h.handleListConnections)
	mux.HandleFunc("GET /api/connections/{name}", h.handleGetConnection)
	mux.HandleFunc("PUT /api/connections/{name}", h.handlePutConnection)
	mux.HandleFunc("DELETE /api/connections/{name}", h.handleDeleteConnection)
	mux.HandleFunc("GET /api/sessions", h.handleListSessions)
	mux.HandleFunc("GET /api/sessions/stream", h.handleSessionListStream)
	mux.HandleFunc("GET /api/ui/ws", h.handleWebUIWS)
	mux.HandleFunc("POST /api/sessions/start", h.handleStartSession)
	mux.HandleFunc("GET /api/sessions/{id}/stream", h.handleStream)
	mux.HandleFunc("GET /api/sessions/{id}/output-range", h.handleSessionOutputRange)
	mux.HandleFunc("POST /api/sessions/{id}/input", h.handleInput)
	mux.HandleFunc("POST /api/sessions/{id}/resize", h.handleResize)
	mux.HandleFunc("GET /api/sessions/{id}/child-shells", h.handleListChildShells)
	mux.HandleFunc("POST /api/sessions/{id}/terminate", h.handleTerminate)
	mux.HandleFunc("POST /api/sessions/{id}/disconnect", h.handleDisconnect)
	mux.HandleFunc("GET /api/forwards", h.handleListForwards)
	mux.HandleFunc("POST /api/forwards", h.handleCreateForward)
	mux.HandleFunc("DELETE /api/forwards/{id}", h.handleDeleteForward)
	mux.HandleFunc("GET /api/sessions/{id}/files", h.handleListFiles)
	mux.HandleFunc("GET /api/sessions/{id}/files/download", h.handleDownloadFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/upload", h.handleUploadFile)
	mux.HandleFunc("DELETE /api/sessions/{id}/files/delete", h.handleDeleteFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/rename", h.handleRenameFile)
	mux.HandleFunc("POST /api/sessions/{id}/files/mkdir", h.handleMakeDir)
	mux.Handle("/", embeddedStaticServer())
}

func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list := h.Sessions.ListAll()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (h *Handler) handleConnectionTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"remote":   strings.TrimSpace(string(sshconfig.RemoteTemplate())),
		"internal": strings.TrimSpace(string(sshconfig.InternalTemplate())),
	})
}

type connectionSummary struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Host        string `json:"host,omitempty"`
	User        string `json:"user,omitempty"`
	Port        int    `json:"port,omitempty"`
}

func (h *Handler) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		writeJSON(w, http.StatusOK, map[string]any{"connections": []connectionSummary{}})
		return
	}
	names, err := h.SSH.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var list []connectionSummary
	for _, n := range names {
		ent, err := h.SSH.Load(n)
		if err != nil {
			continue
		}
		list = append(list, summarizeConnection(n, ent))
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": list})
}

func summarizeConnection(name string, ent *sshconfig.Entry) connectionSummary {
	cs := connectionSummary{Name: name, Kind: ent.Kind, Description: ent.Description}
	if ent.Kind == sshconfig.KindRemote {
		cs.Host = strings.TrimSpace(ent.Host)
		cs.User = strings.TrimSpace(ent.User)
		cs.Port = ent.Port
		if cs.Port == 0 {
			cs.Port = 22
		}
	}
	return cs
}

func (h *Handler) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	data, err := h.SSH.ReadRaw(name)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handlePutConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.SSH.Save(name, body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if err := h.SSH.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type startSessionBody struct {
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	Mode            string   `json:"mode"`
	Name            string   `json:"name"`
	Rows            int      `json:"rows"`
	Cols            int      `json:"cols"`
	SSHConfig       string   `json:"ssh_config"`
	ParentSessionID string   `json:"parent_session_id"`
}

func (h *Handler) handleStartSession(w http.ResponseWriter, r *http.Request) {
	var body startSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Command) == "" && len(body.Args) > 0 {
		http.Error(w, "command is required when args are provided", http.StatusBadRequest)
		return
	}

	// Child shell path: reuse parent's SSH connection.
	if pid := strings.TrimSpace(body.ParentSessionID); pid != "" {
		parent := h.Sessions.Get(pid)
		if parent == nil {
			http.Error(w, "parent session not found", http.StatusNotFound)
			return
		}
		rows := body.Rows
		if rows < 1 {
			rows = 24
		}
		cols := body.Cols
		if cols < 1 {
			cols = 80
		}
		sessName := strings.TrimSpace(body.Name)
		mode := body.Mode
		if mode == "" {
			mode = "pty"
		}
		cs, err := parent.CreateChildShell(body.Command, body.Args, mode == "pty", rows, cols, sessName)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":        cs.ID,
			"parent_session_id": pid,
			"name":              cs.Name,
		})
		return
	}

	cfgName, ent, remote, err := h.resolveSSH(body.SSHConfig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cmd, args := sshconfig.EffectiveCommand(ent, body.Command, body.Args)
	if strings.TrimSpace(cmd) == "" && len(args) > 0 {
		http.Error(w, "command is required when args are provided", http.StatusBadRequest)
		return
	}

	mode := sshconfig.EffectiveMode(ent, body.Mode)
	if mode != "pty" && mode != "pipe" {
		http.Error(w, "mode must be pty or pipe", http.StatusBadRequest)
		return
	}
	sessName := strings.TrimSpace(body.Name)
	if sessName == "" {
		sessName = cfgName
	}
	rows := body.Rows
	if rows < 1 {
		rows = 24
	}
	if rows > 1000 {
		http.Error(w, "rows out of range", http.StatusBadRequest)
		return
	}
	cols := body.Cols
	if cols < 1 {
		cols = 80
	}
	if cols > 1000 {
		http.Error(w, "cols out of range", http.StatusBadRequest)
		return
	}

	sess, err := h.Sessions.Create(session.Config{
		Command: cmd,
		Args:    args,
		Mode:    api.SessionMode(mode),
		Name:    sessName,
		Rows:    rows,
		Cols:    cols,
		Remote:  remote,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	time.Sleep(100 * time.Millisecond)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"pid":        sess.PID,
		"ssh_config": cfgName,
	})
}

func (h *Handler) handleSessionOutputRange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	var shell session.TerminalShell
	if sess := h.Sessions.Get(id); sess != nil {
		shell = sess
	} else if cs := h.Sessions.GetChildShell(id); cs != nil {
		shell = cs
	}
	if shell == nil {
		http.NotFound(w, r)
		return
	}

	const hardMax = 512 * 1024
	max, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("max")))
	if err != nil || max <= 0 {
		max = 256 * 1024
	}
	if max > hardMax {
		max = hardMax
	}

	tail := strings.TrimSpace(r.URL.Query().Get("tail")) == "1"
	var start int64
	if tail {
		total := shell.BufferLen()
		t := total - int64(max)
		if t < 0 {
			t = 0
		}
		start = t
	} else {
		start, err = strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("start")), 10, 64)
		if err != nil || start < 0 {
			http.Error(w, "invalid start", http.StatusBadRequest)
			return
		}
	}

	data, total, err := shell.OutputByteRange(start, max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	end := start + int64(len(data))
	writeJSON(w, http.StatusOK, map[string]any{
		"start": start,
		"end":   end,
		"total": total,
		"d":     base64.StdEncoding.EncodeToString(data),
	})
}

func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	// Reader at current buffer end: stream only new bytes (older scrollback via GET output-range).
	rid, err := sess.RegisterReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer sess.UnregisterReader(rid)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	for {
		if ctx.Err() != nil {
			return
		}

		out, err := sess.ReadTerminalStream(ctx, rid, 250*time.Millisecond, false, 0, terminalOutputChunkBytes)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Debug("webui stream read", "err", err)
			continue
		}
		if out != "" {
			line := base64.StdEncoding.EncodeToString([]byte(out))
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}

		info := sess.Info()
		if info.Status != api.SessionRunning && !sess.HasMoreOutput(rid) {
			_, _ = fmt.Fprint(w, "event: done\ndata: {}\n\n")
			fl.Flush()
			return
		}
	}
}

func (h *Handler) handleInput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("newline") == "1" {
		if err := sess.SendTerminalBytes(body, true); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		if err := sess.SendTerminalBytes(body, false); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

type resizeBody struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

func (h *Handler) handleResize(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	var body resizeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if body.Rows < 1 || body.Cols < 1 {
		http.Error(w, "rows and cols must be >= 1", http.StatusBadRequest)
		return
	}
	if err := sess.ResizePty(body.Rows, body.Cols); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleListChildShells(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	// Return all shells: the root shell (if alive) + child shells.
	var shells []api.Session
	if !sess.IsBufferClosed() {
		shells = append(shells, sess.Info())
	}
	shells = append(shells, sess.ListChildShells()...)
	if shells == nil {
		shells = []api.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shells": shells})
}

func (h *Handler) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	h.Sessions.Terminate(id, true, 0) // close all shell channels
	sess.Disconnect()                  // close SSH client, mark exited
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Check regular session first.
	if sess := h.Sessions.Get(id); sess != nil {
		sess.TerminateShellOnly()
		h.Sessions.NotifyChange()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cs := h.Sessions.GetChildShell(id); cs != nil {
		h.Sessions.CloseChildShell(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) resolveSSH(name string) (cfgName string, ent *sshconfig.Entry, remote *session.RemoteSSH, err error) {
	if h.SSH == nil {
		return "", nil, nil, fmt.Errorf("ssh config store not configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "internal"
	}
	ent, err = h.SSH.Load(name)
	if err != nil {
		return "", nil, nil, err
	}
	if ent.Kind == sshconfig.KindInternal {
		return name, ent, nil, nil
	}
	r, err := sshconfig.RemoteFromEntry(ent, h.SSH.ConfigDir(name))
	if err != nil {
		return "", nil, nil, err
	}
	return name, ent, r, nil
}



func (h *Handler) handleListForwards(w http.ResponseWriter, r *http.Request) {
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"forwards": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": h.ForwardMgr.List()})
}

func (h *Handler) handleDeleteForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	slog.Info("handleDeleteForward called", "forward_id", id)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "forward id required"})
		return
	}
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "forward manager not available"})
		return
	}
	if err := h.ForwardMgr.Close(id); err != nil {
		slog.Error("handleDeleteForward: close failed", "forward_id", id, "err", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	slog.Info("handleDeleteForward: success", "forward_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleCreateForward(w http.ResponseWriter, r *http.Request) {
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "forward manager not available"})
		return
	}
	var req struct {
		SSHConfig  string `json:"ssh_config"`
		Direction  string `json:"direction"`
		RemoteHost string `json:"remote_host"`
		RemotePort int    `json:"remote_port"`
		LocalHost  string `json:"local_host"`
		LocalPort  int    `json:"local_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if req.Direction == "" {
		req.Direction = "local"
	}
	if req.SSHConfig == "" {
		req.SSHConfig = "internal"
	}

	var fw *forward.ForwardInfo
	var fwErr error

	// Find an active session with this ssh_config and reuse its SSH client.
	if h.Sessions == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no session manager available"})
		return
	}
	slog.Info("create forward", "ssh_config", req.SSHConfig, "direction", req.Direction, "local_port", req.LocalPort)
	sess := h.Sessions.FindActiveBySSHConfig(req.SSHConfig)
	if sess == nil {
		slog.Error("create forward: no active session", "ssh_config", req.SSHConfig)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("no active session for %q — connect first, then add port forward", req.SSHConfig)})
		return
	}
	slog.Info("create forward: found session", "session_id", sess.Info().ID)
	sshClient := sess.SSHClient()
	slog.Info("create forward: ssh client", "has_client", sshClient != nil)

	if sshClient == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("no SSH client for %q — session may not be ready", req.SSHConfig)})
		return
	}

	sessID := sess.Info().ID
	if req.Direction == "dynamic" {
		// For internal sessions, use net.Dial directly (bypasses SSH direct-tcpip)
		if req.SSHConfig == "internal" {
			fw, fwErr = h.ForwardMgr.DynamicForwardLocal(req.LocalPort)
		} else {
			ctx, cancel := context.WithCancel(context.Background())
			fw2, ln, err2 := forward.DynamicForwardSSH(ctx, sshClient, req.LocalPort)
			fw, fwErr = fw2, err2
			if fwErr == nil && h.ForwardMgr != nil {
				fw2.SSHConfig = req.SSHConfig
				fw2.SessionID = sessID
				h.ForwardMgr.RegisterForwardFull(fw2, ln, cancel)
			} else if cancel != nil {
				cancel()
			}
		}
	} else if req.Direction == "local" {
		fw, fwErr = h.ForwardMgr.CreateLocal(req.SSHConfig, req.RemoteHost, req.RemotePort, req.LocalPort, sshClient)
	} else {
		fw, fwErr = h.ForwardMgr.CreateRemote(req.SSHConfig, req.LocalHost, req.LocalPort, req.RemoteHost, req.RemotePort, sshClient)
	}
	if fwErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fwErr.Error()})
		return
	}
	fw.SessionID = sessID
	writeJSON(w, http.StatusCreated, fw)
}

func (h *Handler) getFileSession(sessionID string, w http.ResponseWriter) (*session.Session, *ssh.Client) {
	sess := h.Sessions.Get(sessionID)
	if sess == nil {
		return nil, nil
	}
	sshClient := sess.SSHClient()
	return sess, sshClient
}

func (h *Handler) handleListFiles(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	slog.Debug("handleListFiles", "session", sid, "path", path)
	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		// Internal: use local filesystem
		fi, err := os.Stat(path)
		if err != nil {
			slog.Error("file stat", "path", path, "err", err)
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		result := map[string]any{"name": filepath.Base(path), "size": fi.Size(), "is_dir": fi.IsDir()}
		if fi.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				slog.Error("file readdir", "path", path, "err", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			var children []map[string]any
			for _, e := range entries {
				info, _ := e.Info()
				ch := map[string]any{"name": e.Name(), "is_dir": e.IsDir()}
				if info != nil { ch["size"] = info.Size(); ch["mod_time"] = info.ModTime().UTC().Format(time.RFC3339) }
				children = append(children, ch)
			}
			result["children"] = children
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	// Remote: use SFTP
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		slog.Error("sftp client", "path", path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	result, err := sftpCli.StatFile(path)
	if err != nil {
		slog.Error("sftp stat", "path", path, "err", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}

	// Resolve offset/length: Range header takes priority, then ?offset=&length= query params.
	var offset, length int64
	var isRange bool
	if rh := r.Header.Get("Range"); rh != "" {
		isRange = true
	} else {
		offset, _ = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		length, _ = strconv.ParseInt(r.URL.Query().Get("length"), 10, 64)
	}

	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		// Internal / local: http.ServeFile handles Range + If-Modified-Since natively.
		// Only use manual path when explicit ?offset=&length= query params are set.
		if !isRange && r.URL.Query().Get("offset") == "" {
			http.ServeFile(w, r, path)
			return
		}
		if isRange {
			http.ServeFile(w, r, path)
			return
		}
		// Explicit ?offset=&length= for local files — manual partial stream.
		f, err := os.Open(path)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if length <= 0 || offset+length > fi.Size() {
			length = fi.Size() - offset
		}
		if offset > 0 {
			f.Seek(offset, io.SeekStart)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		io.CopyN(w, f, length)
		return
	}

	// Remote — SFTP streaming.
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()

	stat, err := sftpCli.StatFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if stat.IsDir {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path is a directory"})
		return
	}

	// Resolve Range header for SFTP.
	if isRange {
		var ok bool
		offset, length, ok = parseRange(r.Header.Get("Range"), stat.Size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", stat.Size))
			http.Error(w, "Requested Range Not Satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}
	if length <= 0 || offset+length > stat.Size {
		length = stat.Size - offset
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filepath.Base(path)))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
	w.Header().Set("Accept-Ranges", "bytes")
	if isRange {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, stat.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	if _, err := sftpCli.StreamReadTo(w, path, offset, length); err != nil {
		slog.Error("download stream failed", "path", path, "err", err)
	}
}

func (h *Handler) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}

	// Resolve offset: Content-Range header takes priority, then ?offset= query param.
	var offset int64
	if cr := r.Header.Get("Content-Range"); cr != "" {
		var start, end, total int64
		if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &total); err == nil {
			offset = start
		}
	} else {
		offset, _ = strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	}

	// Extract file content: parse multipart form if present, otherwise use raw body.
	var reader io.Reader = r.Body
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err == nil {
			if f, fh, err := r.FormFile("file"); err == nil {
				reader = f
				defer f.Close()
				_ = fh // suppress unused warning
			}
		}
	}

	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		// Internal / local filesystem.
		flag := os.O_RDWR | os.O_CREATE
		if offset <= 0 {
			flag |= os.O_TRUNC
		}
		f, err := os.OpenFile(path, flag, 0644)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		defer f.Close()
		if offset > 0 {
			f.Seek(offset, io.SeekStart)
		}
		n, err := io.Copy(f, reader)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
		return
	}

	// Remote — SFTP streaming.
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()

	n, err := sftpCli.StreamWriteFrom(reader, path, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytes_written": n})
}

func (h *Handler) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		if err := os.Remove(path); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.RemoveFile(path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	from := strings.TrimSpace(r.URL.Query().Get("from"))
	to := strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" || to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "from and to query params required"})
		return
	}
	if from == to {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "source and destination are the same"})
		return
	}

	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		if err := os.Rename(from, to); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.RenameFile(from, to); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleMakeDir(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path query param required"})
		return
	}

	sess, sshClient := h.getFileSession(sid, w)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	if sshClient == nil {
		if err := os.MkdirAll(path, 0755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	defer sftpCli.Close()
	if err := sftpCli.MakeDir(path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// parseRange parses an HTTP Range header value (e.g. "bytes=0-1023", "bytes=1024-",
// "bytes=-512") and returns the start offset and length for a file of the given size.
// Returns ok=false when the range is unsatisfiable.
func parseRange(s string, size int64) (start, length int64, ok bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(s, prefix) {
		return 0, 0, false
	}
	s = s[len(prefix):]

	if strings.HasPrefix(s, "-") {
		// Suffix range: "bytes=-N" → last N bytes.
		n, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, n > 0
	}

	// "bytes=start-end" or "bytes=start-"
	parts := strings.SplitN(s, "-", 2)
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}

	if parts[1] == "" {
		// Open-ended: from start to end-of-file.
		return start, size - start, true
	}

	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || end < start || start >= size {
		return 0, 0, false
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}
