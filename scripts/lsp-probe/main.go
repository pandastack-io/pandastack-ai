// Standalone LSP integration probe. Run from the host:
//
//	go run ./scripts/lsp-probe ws://127.0.0.1:8080/v1/sandboxes/<id>/lsp/python
//
// Performs initialize → didOpen (with a broken Python file) → expects a
// publishDiagnostics notification, then shuts down cleanly. Prints a tiny
// summary and exits non-zero on failure.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: lsp-probe <ws-url>")
		os.Exit(2)
	}
	wsURL := os.Args[1]

	hdr := http.Header{}
	hdr.Set("Sec-WebSocket-Protocol", "lsp")
	hdr.Set("X-Fcs-Workspace", "default")

	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 10 * time.Second
	c, resp, err := d.DialContext(context.Background(), wsURL, hdr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v (resp=%v)\n", err, resp)
		os.Exit(1)
	}
	defer c.Close()
	fmt.Println("✓ WebSocket upgrade ok")

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
				fmt.Printf("· control: %s\n", string(data))
				continue
			}
			buf = append(buf, data...)
			for {
				msg, rest, ok := readFrame(buf)
				if !ok {
					break
				}
				buf = rest
				inbox <- msg
			}
		}
	}()

	send := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		frame := []byte("Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
		frame = append(frame, body...)
		if err := c.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v\n", err)
			os.Exit(1)
		}
	}

	send(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"processId": nil, "rootUri": nil,
			"capabilities": map[string]any{},
			"initializationOptions": map[string]any{
				"pylsp": map[string]any{
					"plugins": map[string]any{
						"pycodestyle":  map[string]any{"enabled": true, "maxLineLength": 80},
						"pyflakes":     map[string]any{"enabled": true},
						"jedi_hover":   map[string]any{"enabled": true},
						"jedi_completion": map[string]any{"enabled": true},
					},
				},
			},
		},
	})

	if !waitFor(inbox, 180*time.Second, func(m map[string]any) bool {
		id, _ := m["id"].(float64)
		return id == 1 && m["result"] != nil
	}) {
		fmt.Fprintln(os.Stderr, "✗ no initialize response within 180s")
		os.Exit(1)
	}
	fmt.Println("✓ initialize handshake ok")

	send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didOpen",
		"params": map[string]any{
			"textDocument": map[string]any{
				"uri": "file:///tmp/pandastack-lsp-test.py", "languageId": "python", "version": 1,
				"text": "import os, sys\nimport os\n\ndef bar(  ):\n    x = undefined_variable_xyz + 1\n    return x\n",
			},
		},
	})
	// pylsp also responds to workspace/didChangeConfiguration with plugin opts.
	send(map[string]any{
		"jsonrpc": "2.0", "method": "workspace/didChangeConfiguration",
		"params": map[string]any{
			"settings": map[string]any{
				"pylsp": map[string]any{
					"plugins": map[string]any{
						"pycodestyle": map[string]any{"enabled": true, "maxLineLength": 80},
						"pyflakes":    map[string]any{"enabled": true},
					},
				},
			},
		},
	})
	// Trigger reanalysis with a no-op change.
	send(map[string]any{
		"jsonrpc": "2.0", "method": "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": "file:///tmp/pandastack-lsp-test.py", "version": 2},
			"contentChanges": []any{map[string]any{"text": "import os, sys\nimport os\n\ndef bar(  ):\n    x = undefined_variable_xyz + 1\n    return x\n"}},
		},
	})

	if !waitFor(inbox, 180*time.Second, func(m map[string]any) bool {
		if m["method"] != "textDocument/publishDiagnostics" {
			return false
		}
		params, _ := m["params"].(map[string]any)
		diags, _ := params["diagnostics"].([]any)
		fmt.Printf("✓ publishDiagnostics: %d diagnostic(s)\n", len(diags))
		for _, d := range diags {
			if dm, ok := d.(map[string]any); ok {
				fmt.Printf("    - %v\n", dm["message"])
			}
		}
		// Require at least 1 real diagnostic for the script to count as a pass.
		return len(diags) > 0
	}) {
		fmt.Fprintln(os.Stderr, "✗ no publishDiagnostics with diagnostics within 30s")
		os.Exit(1)
	}

	send(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "shutdown"})
	send(map[string]any{"jsonrpc": "2.0", "method": "exit"})
	fmt.Println("✓ shutdown sent")
	fmt.Println("OK")
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

func readFrame(buf []byte) (map[string]any, []byte, bool) {
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
