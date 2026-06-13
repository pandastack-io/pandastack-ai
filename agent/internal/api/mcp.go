// SPDX-License-Identifier: Apache-2.0
// Package api — Model Context Protocol (MCP) bridge.
//
// Exposes each sandbox as a JSON-RPC 2.0 endpoint at
//   POST /sandboxes/{id}/mcp
// implementing the subset of MCP that LLM hosts (Claude Desktop, mcp-cli,
// continue.dev, etc.) actually use: initialize, tools/list, tools/call.
//
// Tools exposed:
//   - shell        : run a command in the guest, return {stdout, stderr, code}
//   - read_file    : read file contents (utf-8 best-effort, base64 fallback)
//   - write_file   : write file (utf-8 string)
//   - list_dir     : list directory entries
//
// Wire this once per sandbox by pointing your MCP client at:
//   https://api.pandastack.ai/v1/sandboxes/<id>/mcp
// authenticated with the standard Bearer token. Each request is stateless
// JSON-RPC; we do not hold sessions, so calls survive sandbox hibernation
// (auto-wake handles the rest).
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/pandastack/agent/internal/sandbox"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

func writeRPC(w http.ResponseWriter, status int, resp rpcResp) {
	resp.JSONRPC = "2.0"
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func registerMCP(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("POST /sandboxes/{id}/mcp", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req rpcReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRPC(w, 400, rpcResp{Error: &rpcErr{Code: -32700, Message: "parse error"}})
			return
		}
		if req.JSONRPC != "" && req.JSONRPC != "2.0" {
			writeRPC(w, 400, rpcResp{ID: req.ID, Error: &rpcErr{Code: -32600, Message: "invalid jsonrpc version"}})
			return
		}

		switch req.Method {
		case "initialize":
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "pandastack-sandbox",
					"version": "0.1.0",
				},
			}})

		case "notifications/initialized", "notifications/cancelled":
			// MCP notifications: no response body.
			w.WriteHeader(204)

		case "tools/list":
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: map[string]any{
				"tools": mcpToolDescriptors(),
			}})

		case "tools/call":
			handleMCPToolCall(w, r, mgr, id, req)

		case "ping":
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: map[string]any{}})

		default:
			writeRPC(w, 200, rpcResp{ID: req.ID, Error: &rpcErr{Code: -32601, Message: "method not found: " + req.Method}})
		}
	})
}

func mcpToolDescriptors() []map[string]any {
	return []map[string]any{
		{
			"name":        "shell",
			"description": "Run a shell command inside the sandbox. Returns stdout, stderr, and exit code.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    map[string]any{"type": "string", "description": "Command to execute (sh -c)."},
					"timeout_ms": map[string]any{"type": "integer", "description": "Optional timeout in ms (default 30000)."},
				},
				"required": []string{"command"},
			},
		},
		{
			"name":        "read_file",
			"description": "Read a file from the sandbox filesystem.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
		{
			"name":        "write_file",
			"description": "Write a UTF-8 string to a path in the sandbox.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			"name":        "list_dir",
			"description": "List directory entries.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Defaults to /workspace."},
				},
			},
		},
	}
}

func mcpTextResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
		"isError": isError,
	}
}

func handleMCPToolCall(w http.ResponseWriter, r *http.Request, mgr *sandbox.Manager, id string, req rpcReq) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeRPC(w, 200, rpcResp{ID: req.ID, Error: &rpcErr{Code: -32602, Message: "invalid params"}})
		return
	}

	gc, err := mgr.Guest(id)
	if err != nil {
		writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("sandbox not available: "+err.Error(), true)})
		return
	}

	switch params.Name {
	case "shell":
		cmd, _ := params.Arguments["command"].(string)
		if cmd == "" {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("command is required", true)})
			return
		}
		timeoutMs := 30000
		if v, ok := params.Arguments["timeout_ms"].(float64); ok && v > 0 {
			timeoutMs = int(v)
		}
		ctx := r.Context()
		ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
		res, runErr := gc.Exec(ctx, cmd)
		if runErr != nil {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("exec failed: "+runErr.Error(), true)})
			return
		}
		text := res.Stdout
		if res.Stderr != "" {
			text += "\n[stderr]\n" + res.Stderr
		}
		if res.ExitCode != 0 {
			text += "\n[exit code " + strconv.Itoa(res.ExitCode) + "]"
		}
		writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult(text, res.ExitCode != 0)})

	case "read_file":
		path, _ := params.Arguments["path"].(string)
		if path == "" {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("path required", true)})
			return
		}
		data, err := gc.ReadFile(r.Context(), path)
		if err != nil {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("read failed: "+err.Error(), true)})
			return
		}
		if utf8.Valid(data) {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult(string(data), false)})
		} else {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("[binary; base64]\n"+base64.StdEncoding.EncodeToString(data), false)})
		}

	case "write_file":
		path, _ := params.Arguments["path"].(string)
		content, _ := params.Arguments["content"].(string)
		if path == "" {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("path required", true)})
			return
		}
		if err := gc.WriteFile(r.Context(), path, []byte(content)); err != nil {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("write failed: "+err.Error(), true)})
			return
		}
		writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("wrote "+strconv.Itoa(len(content))+" bytes to "+path, false)})

	case "list_dir":
		path, _ := params.Arguments["path"].(string)
		if path == "" {
			path = "/workspace"
		}
		entries, err := gc.ListDir(r.Context(), path)
		if err != nil {
			writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("list failed: "+err.Error(), true)})
			return
		}
		b, _ := json.MarshalIndent(map[string]any{"path": path, "entries": entries}, "", "  ")
		writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult(string(b), false)})

	default:
		writeRPC(w, 200, rpcResp{ID: req.ID, Result: mcpTextResult("unknown tool: "+params.Name, true)})
	}
}

// (helpers moved to standard library: strconv.Itoa, context.WithTimeout)
