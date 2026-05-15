package webui

import (
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
	"strconv"
	"strings"
	"time"

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
	Sessions *session.Manager
	SSH      *sshconfig.Store

	sessHub *sessionListHub
}

// Register mounts /api/... and / (HTML/CSS/JS via embed.FS + http.FileServer).
// Call after MCP routes (/sse, /message) so those paths are not shadowed.
func (h *Handler) Register(mux *http.ServeMux) {
	if h.Sessions != nil {
		h.Sessions.SetSessionListListener(h.sessionHub().broadcast)
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
	mux.HandleFunc("POST /api/sessions/{id}/terminate", h.handleTerminate)
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
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	Mode      string   `json:"mode"`
	Name      string   `json:"name"`
	Rows      int      `json:"rows"`
	Cols      int      `json:"cols"`
	SSHConfig string   `json:"ssh_config"`
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
	sess := h.Sessions.Get(id)
	if sess == nil {
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
		total := sess.BufferLen()
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

	data, total, err := sess.OutputByteRange(start, max)
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

func (h *Handler) handleTerminate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if h.Sessions.Get(id) == nil {
		http.NotFound(w, r)
		return
	}
	// Web UI「断开」期望立即结束，不走 SIGTERM 长等待（与 MCP 可配置优雅退出区分）
	h.Sessions.Terminate(id, true, 0)
	w.WriteHeader(http.StatusNoContent)
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
	r, err := remoteFromEntry(ent)
	if err != nil {
		return "", nil, nil, err
	}
	return name, ent, r, nil
}

func remoteFromEntry(e *sshconfig.Entry) (*session.RemoteSSH, error) {
	pem := strings.TrimSpace(e.PrivateKeyPEM)
	if fn := strings.TrimSpace(e.PrivateKeyFile); fn != "" {
		b, err := os.ReadFile(filepath.Clean(fn))
		if err != nil {
			return nil, fmt.Errorf("private_key_file %q: %w", fn, err)
		}
		pem = string(b)
	}
	trust := true
	if e.TrustUnknownHost != nil {
		trust = *e.TrustUnknownHost
	}
	port := e.Port
	if port == 0 {
		port = 22
	}
	return &session.RemoteSSH{
		Host:               e.Host,
		Port:               port,
		User:               e.User,
		Password:           e.Password,
		PrivateKeyPEM:      pem,
		KeyPassphrase:      e.KeyPassphrase,
		TrustUnknownHost:   trust,
		KnownHosts:         e.KnownHosts,
		DialTimeoutSeconds: e.DialTimeoutSeconds,
	}, nil
}
