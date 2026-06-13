// SPDX-License-Identifier: Apache-2.0
// Workspace-level Model Context Protocol (MCP) server.
//
// POST /mcp (alias: POST /v1/mcp) exposes the PandaStack control plane as a
// streamable-HTTP MCP server: one endpoint per workspace, authenticated with
// the standard Authorization: Bearer pds_… API token (or a dashboard JWT).
// Each request is stateless JSON-RPC 2.0 — no SSE stream, no session state —
// which is the subset every MCP host (Claude, Cursor, mcp-cli, …) supports
// for remote servers.
//
// Distinct from the per-sandbox bridge at /v1/sandboxes/{id}/mcp (agent-side,
// tools scoped to one VM): this endpoint manages the whole workspace —
// create/list/delete sandboxes, run commands, files, managed databases,
// app deploys, templates.
//
// The MCP server is open to any authenticated workspace (no tiers, no billing).
// A per-workspace fixed 1-minute-window rate limit on tools/call guards against
// runaway loops: PANDASTACK_MCP_RATE_PER_MIN (default 60).
//
// Tool dispatch is an in-process loopback against the fully-built route mux
// (same pattern as databasesAPI.agentCall): we synthesize a /v1/... request
// carrying the already-authenticated workspace headers and serve it on the
// inner handler, so every tool automatically inherits the
// multinode scheduler.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- JSON-RPC plumbing ----------

type mcpRPCReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type mcpRPCResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpRPCErr      `json:"error,omitempty"`
}

func mcpWriteRPC(w http.ResponseWriter, status int, resp mcpRPCResp) {
	resp.JSONRPC = "2.0"
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func mcpText(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// ---------- server ----------

type mcpAPI struct {
	log   *slog.Logger
	db    *sql.DB
	inner http.Handler // the fully-populated route mux (captured by pointer)

	// ratePerMin is a per-workspace fixed-window rate limit on tools/call to
	// prevent runaway loops. Not a billing tier — purely operational abuse
	// protection, configurable via PANDASTACK_MCP_RATE_PER_MIN.
	ratePerMin int

	mu      sync.Mutex
	windows map[string]*mcpRateWindow
}

type mcpRateWindow struct {
	start time.Time
	count int
}

func mcpEnvInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func newMCPAPI(db *sql.DB, log *slog.Logger, inner http.Handler) *mcpAPI {
	return &mcpAPI{
		log:        log,
		db:         db,
		inner:      inner,
		ratePerMin: mcpEnvInt("PANDASTACK_MCP_RATE_PER_MIN", 60),
		windows:    map[string]*mcpRateWindow{},
	}
}

// allow enforces the per-workspace fixed-window rate limit on tools/call.
// Returns (allowed, limit).
func (m *mcpAPI) allow(workspace string) (bool, int) {
	limit := m.ratePerMin
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Opportunistic prune so the map can't grow unbounded.
	if len(m.windows) > 10000 {
		for k, w := range m.windows {
			if now.Sub(w.start) > 2*time.Minute {
				delete(m.windows, k)
			}
		}
	}
	w := m.windows[workspace]
	if w == nil || now.Sub(w.start) >= time.Minute {
		m.windows[workspace] = &mcpRateWindow{start: now, count: 1}
		return true, limit
	}
	if w.count >= limit {
		return false, limit
	}
	w.count++
	return true, limit
}

func (m *mcpAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", m.handle)
	mux.HandleFunc("POST /v1/mcp", m.handle)
	methodNotAllowed := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "MCP endpoint accepts POST only (stateless streamable-HTTP JSON-RPC)",
		})
	}
	mux.HandleFunc("/mcp", methodNotAllowed)
	mux.HandleFunc("/v1/mcp", methodNotAllowed)
}

func (m *mcpAPI) handle(w http.ResponseWriter, r *http.Request) {
	workspace := r.Header.Get("X-Fcs-Workspace")
	if workspace == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		mcpWriteRPC(w, http.StatusBadRequest, mcpRPCResp{Error: &mcpRPCErr{Code: -32700, Message: "request too large or unreadable"}})
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		mcpWriteRPC(w, http.StatusBadRequest, mcpRPCResp{Error: &mcpRPCErr{Code: -32600, Message: "batch requests are not supported"}})
		return
	}
	var req mcpRPCReq
	if err := json.Unmarshal(trimmed, &req); err != nil {
		mcpWriteRPC(w, http.StatusBadRequest, mcpRPCResp{Error: &mcpRPCErr{Code: -32700, Message: "parse error"}})
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		mcpWriteRPC(w, http.StatusBadRequest, mcpRPCResp{ID: req.ID, Error: &mcpRPCErr{Code: -32600, Message: "invalid jsonrpc version"}})
		return
	}

	switch {
	case req.Method == "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		pv := params.ProtocolVersion
		if pv == "" {
			pv = "2025-03-26"
		}
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Result: map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    "pandastack",
				"version": "1.0.0",
			},
			"instructions": "PandaStack workspace MCP server. Create Firecracker sandboxes, run commands, manage files, managed Postgres databases, and app deploys for workspace " + workspace + ".",
		}})

	case strings.HasPrefix(req.Method, "notifications/"):
		// Streamable HTTP: notifications are accepted with no body.
		w.WriteHeader(http.StatusAccepted)

	case req.Method == "ping":
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Result: map[string]any{}})

	case req.Method == "tools/list":
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Result: map[string]any{
			"tools": mcpWorkspaceTools(),
		}})

	case req.Method == "tools/call":
		if ok, limit := m.allow(workspace); !ok {
			mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Error: &mcpRPCErr{
				Code:    -32000,
				Message: fmt.Sprintf("rate limit exceeded: %d tool calls per minute for this workspace", limit),
			}})
			return
		}
		m.handleToolCall(w, r, req)

	default:
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Error: &mcpRPCErr{Code: -32601, Message: "method not found: " + req.Method}})
	}
}

// ---------- loopback dispatch ----------

// call serves a synthesized request on the inner mux, inheriting the caller's
// authenticated workspace. Returns (status, body-as-text).
func (m *mcpAPI) call(r *http.Request, method, path string, body []byte, contentType string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return 0, "internal error: " + err.Error()
	}
	req.Header.Set("X-Fcs-Workspace", r.Header.Get("X-Fcs-Workspace"))
	uid := r.Header.Get("X-Pandastack-User-Id")
	if uid == "" {
		uid = "_mcp"
	}
	req.Header.Set("X-Pandastack-User-Id", uid)
	req.Header.Set("X-Pandastack-Auth-Method", "mcp")
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	m.inner.ServeHTTP(rr, req)
	res := rr.Result()
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, strings.TrimSpace(string(b))
}

// mcpSafeID rejects IDs that could change the meaning of a constructed path.
func mcpSafeID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	if strings.ContainsAny(s, "/\\?#%& \t\r\n") || strings.Contains(s, "..") {
		return false
	}
	return true
}

func mcpArgStr(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func mcpArgInt(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok && v > 0 {
		return int(v)
	}
	return def
}

func (m *mcpAPI) handleToolCall(w http.ResponseWriter, r *http.Request, req mcpRPCReq) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Error: &mcpRPCErr{Code: -32602, Message: "invalid params"}})
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	args := params.Arguments

	fail := func(msg string) {
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Result: mcpText(msg, true)})
	}
	done := func(status int, body string) {
		if body == "" {
			body = http.StatusText(status)
		}
		mcpWriteRPC(w, 200, mcpRPCResp{ID: req.ID, Result: mcpText(body, status >= 400)})
	}
	needID := func(key string) (string, bool) {
		id := mcpArgStr(args, key)
		if !mcpSafeID(id) {
			fail(key + " is required (a valid id)")
			return "", false
		}
		return id, true
	}

	switch params.Name {

	// --- sandboxes ---
	case "create_sandbox":
		template := mcpArgStr(args, "template")
		if template == "" {
			template = "code-interpreter"
		}
		if !mcpSafeID(template) {
			fail("invalid template name")
			return
		}
		payload := map[string]any{"template": template}
		if ttl := mcpArgInt(args, "ttl_seconds", 0); ttl > 0 {
			payload["ttl_seconds"] = ttl
		}
		if md, ok := args["metadata"].(map[string]any); ok && len(md) > 0 {
			payload["metadata"] = md
		}
		b, _ := json.Marshal(payload)
		done(m.call(r, "POST", "/v1/sandboxes", b, "", 90*time.Second))

	case "list_sandboxes":
		done(m.call(r, "GET", "/v1/sandboxes", nil, "", 30*time.Second))

	case "delete_sandbox":
		id, ok := needID("sandbox_id")
		if !ok {
			return
		}
		done(m.call(r, "DELETE", "/v1/sandboxes/"+url.PathEscape(id), nil, "", 60*time.Second))

	case "run_command":
		id, ok := needID("sandbox_id")
		if !ok {
			return
		}
		cmd := mcpArgStr(args, "command")
		if cmd == "" {
			fail("command is required")
			return
		}
		timeoutSec := mcpArgInt(args, "timeout_seconds", 60)
		if timeoutSec > 300 {
			timeoutSec = 300
		}
		b, _ := json.Marshal(map[string]any{"cmd": cmd, "timeout_seconds": timeoutSec})
		done(m.call(r, "POST", "/v1/sandboxes/"+url.PathEscape(id)+"/exec", b, "",
			time.Duration(timeoutSec+15)*time.Second))

	// --- files ---
	case "read_file":
		id, ok := needID("sandbox_id")
		if !ok {
			return
		}
		path := mcpArgStr(args, "path")
		if path == "" {
			fail("path is required")
			return
		}
		q := url.Values{"path": {path}}
		done(m.call(r, "GET", "/v1/sandboxes/"+url.PathEscape(id)+"/fs?"+q.Encode(), nil, "", 60*time.Second))

	case "write_file":
		id, ok := needID("sandbox_id")
		if !ok {
			return
		}
		path := mcpArgStr(args, "path")
		if path == "" {
			fail("path is required")
			return
		}
		content := mcpArgStr(args, "content")
		q := url.Values{"path": {path}}
		status, body := m.call(r, "PUT", "/v1/sandboxes/"+url.PathEscape(id)+"/fs?"+q.Encode(),
			[]byte(content), "application/octet-stream", 60*time.Second)
		if status < 400 && body == "" {
			body = fmt.Sprintf("wrote %d bytes to %s", len(content), path)
		}
		done(status, body)

	case "list_dir":
		id, ok := needID("sandbox_id")
		if !ok {
			return
		}
		path := mcpArgStr(args, "path")
		if path == "" {
			path = "/workspace"
		}
		q := url.Values{"path": {path}}
		done(m.call(r, "GET", "/v1/sandboxes/"+url.PathEscape(id)+"/fs/dir?"+q.Encode(), nil, "", 60*time.Second))

	// --- managed databases ---
	case "create_database":
		payload := map[string]any{}
		if label := mcpArgStr(args, "label"); label != "" {
			payload["label"] = label
		}
		b, _ := json.Marshal(payload)
		// Database create blocks until Postgres is ready (30-90s typical).
		done(m.call(r, "POST", "/v1/databases", b, "", 180*time.Second))

	case "list_databases":
		done(m.call(r, "GET", "/v1/databases", nil, "", 30*time.Second))

	case "delete_database":
		id, ok := needID("database_id")
		if !ok {
			return
		}
		done(m.call(r, "DELETE", "/v1/databases/"+url.PathEscape(id), nil, "", 60*time.Second))

	// --- apps ---
	case "list_apps":
		done(m.call(r, "GET", "/v1/apps", nil, "", 30*time.Second))

	case "deploy_app":
		id, ok := needID("app_id")
		if !ok {
			return
		}
		payload := map[string]any{}
		if ref := mcpArgStr(args, "git_ref"); ref != "" {
			payload["git_ref"] = ref
		}
		b, _ := json.Marshal(payload)
		done(m.call(r, "POST", "/v1/apps/"+url.PathEscape(id)+"/deploys", b, "", 60*time.Second))

	// --- templates ---
	case "list_templates":
		done(m.call(r, "GET", "/v1/templates", nil, "", 30*time.Second))

	default:
		fail("unknown tool: " + params.Name)
	}
}

// ---------- tool catalog ----------

func mcpWorkspaceTools() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	num := func(desc string) map[string]any {
		return map[string]any{"type": "integer", "description": desc}
	}
	return []map[string]any{
		{
			"name":        "create_sandbox",
			"description": "Create a new Firecracker microVM sandbox. Returns the sandbox JSON including its id. Templates: base, code-interpreter, agent, browser.",
			"inputSchema": obj(map[string]any{
				"template":    str("Template name (default: code-interpreter)."),
				"ttl_seconds": num("Optional auto-delete TTL in seconds."),
			}),
		},
		{
			"name":        "list_sandboxes",
			"description": "List all sandboxes in this workspace.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "delete_sandbox",
			"description": "Delete a sandbox permanently.",
			"inputSchema": obj(map[string]any{
				"sandbox_id": str("Sandbox id."),
			}, "sandbox_id"),
		},
		{
			"name":        "run_command",
			"description": "Run a shell command inside a sandbox. Returns stdout, stderr, and exit code as JSON.",
			"inputSchema": obj(map[string]any{
				"sandbox_id":      str("Sandbox id."),
				"command":         str("Command to execute (sh -c)."),
				"timeout_seconds": num("Optional timeout in seconds (default 60, max 300)."),
			}, "sandbox_id", "command"),
		},
		{
			"name":        "read_file",
			"description": "Read a file from a sandbox filesystem.",
			"inputSchema": obj(map[string]any{
				"sandbox_id": str("Sandbox id."),
				"path":       str("Absolute path inside the sandbox."),
			}, "sandbox_id", "path"),
		},
		{
			"name":        "write_file",
			"description": "Write a UTF-8 string to a file inside a sandbox (creates or overwrites).",
			"inputSchema": obj(map[string]any{
				"sandbox_id": str("Sandbox id."),
				"path":       str("Absolute path inside the sandbox."),
				"content":    str("File content."),
			}, "sandbox_id", "path", "content"),
		},
		{
			"name":        "list_dir",
			"description": "List directory entries inside a sandbox.",
			"inputSchema": obj(map[string]any{
				"sandbox_id": str("Sandbox id."),
				"path":       str("Directory path (default /workspace)."),
			}, "sandbox_id"),
		},
		{
			"name":        "create_database",
			"description": "Create a managed PostgreSQL 16 database (dedicated microVM + durable volume). Blocks until ready (~30-90s); returns connection details.",
			"inputSchema": obj(map[string]any{
				"label": str("Optional human-readable label."),
			}),
		},
		{
			"name":        "list_databases",
			"description": "List managed databases in this workspace.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "delete_database",
			"description": "Delete a managed database permanently (irreversible).",
			"inputSchema": obj(map[string]any{
				"database_id": str("Database id."),
			}, "database_id"),
		},
		{
			"name":        "list_apps",
			"description": "List git-driven hosted apps in this workspace.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "deploy_app",
			"description": "Trigger a new deployment for an app (blue-green). Returns the deployment record; poll its status via list_apps.",
			"inputSchema": obj(map[string]any{
				"app_id":  str("App id."),
				"git_ref": str("Optional git ref (branch, tag, or commit) to deploy."),
			}, "app_id"),
		},
		{
			"name":        "list_templates",
			"description": "List available sandbox templates.",
			"inputSchema": obj(map[string]any{}),
		},
	}
}
