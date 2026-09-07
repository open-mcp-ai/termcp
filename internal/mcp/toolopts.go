package mcp

import mcpgo "github.com/mark3labs/mcp-go/mcp"

// annotationNone overrides mcp-go's default hint block. NewTool otherwise
// serializes four default booleans for every tool; they carry no termcp
// specific information and cost roughly 80 bytes per definition.
var annotationNone = mcpgo.WithToolAnnotation(mcpgo.ToolAnnotation{})

// Compact descriptions keep the wire representation useful without repeating
// the same lifecycle and ID guidance in every tool. The original descriptions
// remain next to registrations as source documentation; only the description
// advertised to MCP clients is reduced.
var compactToolDescriptions = map[string]string{
	"session_start":           "Start a session; returns session_id + shell_id. DEFAULT: omit command/args to drive an interactive shell (multi-step/stateful work). command/args only for REPL/TUI, daemons, or single atomic scripts — never for sequential steps.",
	"shell_open":              "Open another shell channel on a session; returns shell_id.",
	"shell_list":              "List shell channels for a session.",
	"shell_close":             "Close one shell channel; use session_terminate for the whole session.",
	"shell_input":             "Write text to shell stdin without executing; use shell_key(enter) to run it.",
	"shell_key":               "Send a named key to a shell.",
	"shell_output":            "Read output of a live or archived shell; empty read != no output (poll with timeout<=3). Unified cursor (offset/tail_lines/reader_id).",
	"session_list":            "List live sessions.",
	"session_info":            "Get detailed information for a session.",
	"session_terminate":       "Terminate and archive a session; closes its shells and forwards.",
	"shell_resize":            "Resize a shell PTY.",
	"shell_detect":            "Detect an interactive shell on the termcp host.",
	"shell_reader_register":   "Register an output reader starting at the current buffer end.",
	"shell_reader_unregister": "Release an output reader.",
	"forward":                 "SSH port forwards: local (-L), remote (-R), dynamic (-D SOCKS5), list, close.",
	"file_read":               "Read a remote file via SFTP as text, hex, or a host-side download.",
	"file_write":              "Write a remote file via SFTP inline or from a host-side file.",
	"file_stat":               "Get remote file or directory metadata.",
	"file_delete":             "Delete a remote file or empty directory.",
	"file_rename":             "Move or rename a remote path.",
	"file_mkdir":              "Create a remote directory and its parents.",
	"file_urls":               "Get direct HTTP download/upload URLs for a remote path.",
	"file_perm":               "Ownership/metadata ops: chmod, chown, or chtimes.",
	"file_link":               "Link ops: readlink, symlink, or hard link.",
	"file_fs":                 "Path/filesystem ops: truncate, realpath, or statvfs.",
	"file_getwd":              "Get the SFTP working directory for a session.",
	"message":                 "Stored session messages: list the index or fetch payloads.",
	"history":                 "Archived sessions: list, search, rename, meta, purge, screenshot. Read output via shell_output.",
	"ssh_config":              "SSH profiles: action=list names, or (if enabled) create/edit/copy/delete.",
}

// newTool applies termcp's compact wire representation to a tool definition.
// Tool metadata is sent to the model with every tool listing, so repeated
// parameter explanations are kept only where they add semantics beyond the
// parameter name, type, and compact tool description.
func newTool(name string, opts ...mcpgo.ToolOption) mcpgo.Tool {
	opts = append([]mcpgo.ToolOption{annotationNone}, opts...)
	t := mcpgo.NewTool(name, opts...)
	if desc, ok := compactToolDescriptions[name]; ok {
		t.Description = desc
	}
	compactToolSchema(&t)
	return t
}

// These fields are either identifiers, paths, coordinates, or self-evident
// SSH profile fields. Their descriptions repeat information already present in
// the tool name/instructions and are not useful enough to justify their cost.
var omitPropertyDescriptions = map[string]struct{}{
	"session_id": {}, "shell_id": {}, "reader_id": {},
	"name": {}, "ssh_config": {}, "rows": {}, "cols": {},
	"remote_path": {}, "from_path": {}, "to_path": {},
	"target": {}, "link_path": {}, "existing_path": {}, "new_path": {},
	"forward_id": {}, "source_name": {}, "target_name": {},
	"host": {}, "user": {}, "port": {}, "local_host": {}, "local_port": {},
	"remote_host": {}, "remote_port": {}, "uid": {}, "gid": {},
	"atime": {}, "mtime": {}, "size": {},
	"description": {}, "default_shell": {},
	"trust_unknown_host": {}, "known_hosts": {}, "dial_timeout_seconds": {},
	"jump_host": {}, "jump_user": {}, "jump_port": {}, "jump_password": {},
	"jump_private_key": {}, "jump_key_passphrase": {},
	"jump_trust_unknown_host": {}, "jump_known_hosts": {},
	"jump_dial_timeout_seconds": {}, "jump_proxy": {},
}

var keepPropertyDescriptions = map[string]bool{
	"file_perm": true,
	"file_link": true, // grouped tool with action-dependent params
	"file_fs":   true, // grouped tool with action-dependent params
}

func compactToolSchema(t *mcpgo.Tool) {
	if keepPropertyDescriptions[t.Name] {
		return
	}
	for name, raw := range t.InputSchema.Properties {
		prop, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, omit := omitPropertyDescriptions[name]; omit {
			delete(prop, "description")
		}
	}
}
