// SPDX-License-Identifier: Apache-2.0
//go:build linux

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestLSP_PythonRoundTrip exercises the LSP WebSocket endpoint against a live
// sandbox. Skipped unless PANDASTACK_LSP_TEST_WS_URL points at a running endpoint:
//
//	PANDASTACK_LSP_TEST_WS_URL=ws://127.0.0.1:9100/sandboxes/<id>/lsp/python \
//	  go test ./agent/internal/api -run TestLSP_PythonRoundTrip -v
//
// What it asserts:
//   - WebSocket upgrade succeeds with subprotocol "lsp".
//   - The Python language server (pylsp) responds to `initialize`.
//   - Opening a doc with a syntax error yields a `textDocument/publishDiagnostics`
//     notification.
//
// Why an env-gated test instead of an in-process unit test: the agent needs a
// real guest with pylsp; mocking SSH+pylsp would test plumbing, not behaviour.
func TestLSP_PythonRoundTrip(t *testing.T) {
	wsURL := os.Getenv("PANDASTACK_LSP_TEST_WS_URL")
	if wsURL == "" {
		t.Skip("set PANDASTACK_LSP_TEST_WS_URL=ws://host:port/sandboxes/<id>/lsp/python to run")
	}

	hdr := http.Header{}
	hdr.Set("Sec-WebSocket-Protocol", "lsp")

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	c, _, err := dialer.DialContext(context.Background(), wsURL, hdr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	inbox := make(chan map[string]any, 16)
	go func() {
		defer close(inbox)
		var buf []byte
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				t.Logf("control: %s", string(data))
				continue
			}
			buf = append(buf, data...)
			for {
				msg, rest, ok := readLSPMessage(buf)
				if !ok {
					break
				}
				buf = rest
				inbox <- msg
			}
		}
	}()

	send := func(payload map[string]any) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		frame := []byte("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
		frame = append(frame, body...)
		if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"processId": nil, "rootUri": nil, "capabilities": map[string]any{}},
	})
	if !waitFor(inbox, 30*time.Second, func(m map[string]any) bool {
		id, _ := m["id"].(float64)
		return id == 1 && m["result"] != nil
	}) {
		t.Fatal("no initialize response")
	}

	send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri": "file:///tmp/pandastack-lsp-test.py", "languageId": "python", "version": 1,
				"text": "def foo(:\n    pass\n",
			},
		},
	})
	if !waitFor(inbox, 30*time.Second, func(m map[string]any) bool {
		return m["method"] == "textDocument/publishDiagnostics"
	}) {
		t.Fatal("no publishDiagnostics within timeout")
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "shutdown"})
	send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
}

func waitFor(ch <-chan map[string]any, d time.Duration, pred func(map[string]any) bool) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case m, ok := <-ch:
			if !ok {
				return false
			}
			if pred(m) {
				return true
			}
		case <-t.C:
			return false
		}
	}
}

func readLSPMessage(buf []byte) (map[string]any, []byte, bool) {
	end := strings.Index(string(buf), "\r\n\r\n")
	if end < 0 {
		return nil, buf, false
	}
	header := string(buf[:end])
	rest := buf[end+4:]
	var length int
	for _, line := range strings.Split(header, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			length, _ = strconv.Atoi(strings.TrimSpace(line[len("content-length:"):]))
		}
	}
	if length <= 0 || len(rest) < length {
		return nil, buf, false
	}
	var m map[string]any
	if err := json.Unmarshal(rest[:length], &m); err != nil {
		return nil, rest[length:], false
	}
	return m, rest[length:], true
}
