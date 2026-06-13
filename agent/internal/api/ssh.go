// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pandastack/agent/internal/sandbox"
	xssh "golang.org/x/crypto/ssh"
)

var sshWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  32 * 1024,
	WriteBufferSize: 32 * 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// registerSSH mounts GET /sandboxes/{id}/ssh as a WebSocket endpoint.
//
// Wire protocol (identical to /exec/pty):
//
//	client → server: binary frame = raw stdin bytes
//	                 text frame   = JSON {"resize":{"rows":N,"cols":M}}
//	server → client: binary frame = raw stdout/stderr bytes
//	                 text frame   = JSON {"exit":N} | {"error":"..."}
func registerSSH(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/ssh", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")

		gc, err := mgr.Guest(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}

		rows, cols := 24, 80
		if v := r.URL.Query().Get("rows"); v != "" {
			if n, _ := strconv.Atoi(v); n > 0 {
				rows = n
			}
		}
		if v := r.URL.Query().Get("cols"); v != "" {
			if n, _ := strconv.Atoi(v); n > 0 {
				cols = n
			}
		}

		conn, err := sshWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		pty, err := gc.OpenPTY(r.Context(), rows, cols)
		if err != nil {
			_ = conn.WriteMessage(websocket.TextMessage,
				mustJSON(map[string]string{"error": err.Error()}))
			return
		}
		defer pty.Close()

		var writeMu sync.Mutex
		send := func(t int, b []byte) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteMessage(t, b)
		}

		stdoutDone := make(chan struct{})

		// guest stdout/stderr → WS binary frames
		go func() {
			defer close(stdoutDone)
			buf := make([]byte, 32*1024)
			for {
				n, err := pty.Stdout.Read(buf)
				if n > 0 {
					if werr := send(websocket.BinaryMessage, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()

		// wait for shell exit; send {"exit":N} after stdout is fully drained
		go func() {
			exitCode := 0
			if err := pty.Wait(); err != nil {
				if ee, ok := err.(*xssh.ExitError); ok {
					exitCode = ee.ExitStatus()
				} else {
					exitCode = 1
				}
			}
			<-stdoutDone
			_ = send(websocket.TextMessage, mustJSON(map[string]int{"exit": exitCode}))
			_ = conn.Close()
		}()

		// keepalive pings so proxies don't drop idle connections
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-stdoutDone:
					return
				case <-ticker.C:
					_ = send(websocket.PingMessage, nil)
				}
			}
		}()

		// WS → guest stdin + resize
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				_, _ = pty.Stdin.Write(data)
			case websocket.TextMessage:
				var ctrl struct {
					Resize *struct {
						Rows int `json:"rows"`
						Cols int `json:"cols"`
					} `json:"resize,omitempty"`
				}
				if json.Unmarshal(data, &ctrl) == nil && ctrl.Resize != nil {
					_ = pty.Resize(ctrl.Resize.Rows, ctrl.Resize.Cols)
				}
			}
		}
	})
}
