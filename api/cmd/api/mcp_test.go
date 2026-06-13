// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestMCP returns an mcpAPI whose loopback handler is a stub /v1 router
// and whose tier lookup is canned per workspace.
func newTestMCP(t *testing.T, inner http.Handler) *mcpAPI {
	t.Helper()
	if inner == nil {
		inner = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no stub route", http.StatusNotFound)
		})
	}
	return newMCPAPI(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), inner)
}

func mcpDo(m *mcpAPI, workspace, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	if workspace != "" {
		r.Header.Set("X-Fcs-Workspace", workspace)
	}
	w := httptest.NewRecorder()
	m.handle(w, r)
	return w
}

func decodeRPC(t *testing.T, w *httptest.ResponseRecorder) mcpRPCResp {
	t.Helper()
	var resp mcpRPCResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rpc response: %v (body %q)", err, w.Body.String())
	}
	return resp
}

func TestMCPInitialize(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want echo of client version", res["protocolVersion"])
	}
	si := res["serverInfo"].(map[string]any)
	if si["name"] != "pandastack" {
		t.Errorf("serverInfo.name = %v", si["name"])
	}
}

func TestMCPUnauthenticated(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMCPOpenToAnyWorkspace(t *testing.T) {
	// OSS build: no tiers, no gate. Any authenticated workspace is allowed.
	m := newTestMCP(t, nil)
	w := mcpDo(m, "anyone", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200 (MCP is open in OSS build)", w.Code)
	}
}

func TestMCPToolsList(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	tools := resp.Result.(map[string]any)["tools"].([]any)
	if len(tools) < 13 {
		t.Fatalf("tools = %d, want >= 13", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"create_sandbox", "run_command", "read_file", "write_file",
		"create_database", "delete_database", "deploy_app", "list_templates"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestMCPBatchRejected(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	resp := decodeRPC(t, w)
	if resp.Error == nil || resp.Error.Code != -32600 {
		t.Fatalf("want -32600 batch rejection, got %+v", resp.Error)
	}
}

func TestMCPParseError(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `{not json`)
	resp := decodeRPC(t, w)
	if resp.Error == nil || resp.Error.Code != -32700 {
		t.Fatalf("want -32700 parse error, got %+v", resp.Error)
	}
}

func TestMCPMethodNotFound(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	resp := decodeRPC(t, w)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("want -32601, got %+v", resp.Error)
	}
}

func TestMCPNotificationAccepted(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
}

// stubMux records loopback requests and serves canned bodies.
type stubCall struct {
	method, path, body, workspace, authMethod string
}

func stubRouter(t *testing.T, calls *[]stubCall, status int, respBody string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		*calls = append(*calls, stubCall{
			method:     r.Method,
			path:       r.URL.RequestURI(),
			body:       string(b),
			workspace:  r.Header.Get("X-Fcs-Workspace"),
			authMethod: r.Header.Get("X-Pandastack-Auth-Method"),
		})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	})
}

func toolCallBody(name string, args map[string]any) string {
	p, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	var buf bytes.Buffer
	fmt.Fprintf(&buf, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":%s}`, p)
	return buf.String()
}

func toolResultText(t *testing.T, resp mcpRPCResp) (string, bool) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error: %+v", resp.Error)
	}
	res := resp.Result.(map[string]any)
	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func TestMCPToolCallCreateSandboxLoopback(t *testing.T) {
	var calls []stubCall
	m := newTestMCP(t, stubRouter(t, &calls, 201, `{"id":"sbx-1","status":"running"}`))
	w := mcpDo(m, "acme", toolCallBody("create_sandbox", map[string]any{"template": "base", "ttl_seconds": float64(600)}))
	text, isErr := toolResultText(t, decodeRPC(t, w))
	if isErr {
		t.Fatalf("isError = true, text = %s", text)
	}
	if !strings.Contains(text, "sbx-1") {
		t.Errorf("text = %s, want sandbox json", text)
	}
	if len(calls) != 1 {
		t.Fatalf("loopback calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.method != "POST" || c.path != "/v1/sandboxes" {
		t.Errorf("loopback = %s %s", c.method, c.path)
	}
	if c.workspace != "acme" || c.authMethod != "mcp" {
		t.Errorf("headers: workspace=%s auth=%s", c.workspace, c.authMethod)
	}
	if !strings.Contains(c.body, `"template":"base"`) || !strings.Contains(c.body, `"ttl_seconds":600`) {
		t.Errorf("body = %s", c.body)
	}
}

func TestMCPToolCallRunCommand(t *testing.T) {
	var calls []stubCall
	m := newTestMCP(t, stubRouter(t, &calls, 200, `{"stdout":"hello\n","stderr":"","exit_code":0}`))
	w := mcpDo(m, "acme", toolCallBody("run_command", map[string]any{
		"sandbox_id": "sbx-1", "command": "echo hello"}))
	text, isErr := toolResultText(t, decodeRPC(t, w))
	if isErr || !strings.Contains(text, "hello") {
		t.Fatalf("text=%s isErr=%v", text, isErr)
	}
	c := calls[0]
	if c.path != "/v1/sandboxes/sbx-1/exec" {
		t.Errorf("path = %s", c.path)
	}
	if !strings.Contains(c.body, `"cmd":"echo hello"`) {
		t.Errorf("body = %s", c.body)
	}
}

func TestMCPToolCallUpstreamErrorMapsToIsError(t *testing.T) {
	var calls []stubCall
	m := newTestMCP(t, stubRouter(t, &calls, 429, `{"error":"quota exceeded"}`))
	w := mcpDo(m, "acme", toolCallBody("list_sandboxes", nil))
	text, isErr := toolResultText(t, decodeRPC(t, w))
	if !isErr {
		t.Fatalf("want isError for upstream 429, text = %s", text)
	}
}

func TestMCPToolCallRejectsUnsafeIDs(t *testing.T) {
	var calls []stubCall
	m := newTestMCP(t, stubRouter(t, &calls, 200, "{}"))
	for _, bad := range []string{"", "../../etc", "a/b", "a b", "x?y=1"} {
		w := mcpDo(m, "acme", toolCallBody("delete_sandbox", map[string]any{"sandbox_id": bad}))
		_, isErr := toolResultText(t, decodeRPC(t, w))
		if !isErr {
			t.Errorf("id %q: want isError", bad)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("unsafe ids must not reach the router; got %d calls", len(calls))
	}
}

func TestMCPToolCallUnknownTool(t *testing.T) {
	m := newTestMCP(t, nil)
	w := mcpDo(m, "acme", toolCallBody("rm_rf_slash", nil))
	text, isErr := toolResultText(t, decodeRPC(t, w))
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("text=%s isErr=%v", text, isErr)
	}
}

func TestMCPRateLimit(t *testing.T) {
	var calls []stubCall
	m := newTestMCP(t, stubRouter(t, &calls, 200, "[]"))
	m.ratePerMin = 3

	var limited int
	for i := 0; i < 5; i++ {
		w := mcpDo(m, "spam", toolCallBody("list_sandboxes", nil))
		resp := decodeRPC(t, w)
		if resp.Error != nil && resp.Error.Code == -32000 {
			limited++
		}
	}
	if limited != 2 {
		t.Errorf("limited = %d of 5 (limit 3), want 2", limited)
	}

	// A different workspace gets its own independent window.
	limited = 0
	for i := 0; i < 3; i++ {
		w := mcpDo(m, "other", toolCallBody("list_sandboxes", nil))
		resp := decodeRPC(t, w)
		if resp.Error != nil && resp.Error.Code == -32000 {
			limited++
		}
	}
	if limited != 0 {
		t.Errorf("other workspace: limited = %d of 3 (limit 3), want 0", limited)
	}

	// Rate limit applies to tools/call only — tools/list is never limited.
	w := mcpDo(m, "spam", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if resp := decodeRPC(t, w); resp.Error != nil {
		t.Errorf("tools/list should not be rate limited: %+v", resp.Error)
	}
}

func TestMCPRegisterMethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	m := newTestMCP(t, nil)
	m.Register(mux)
	for _, method := range []string{"GET", "DELETE", "PUT"} {
		r := httptest.NewRequest(method, "/mcp", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /mcp: status = %d, want 405", method, w.Code)
		}
	}
	// POST routes to the real handler (401 without workspace header).
	r := httptest.NewRequest("POST", "/v1/mcp", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/mcp unauthenticated: status = %d, want 401", w.Code)
	}
}
