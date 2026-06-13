// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/gorilla/websocket"
)

var ptyUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// PTY wire protocol:
//   client -> server: binary frame = raw stdin bytes
//                     text frame   = JSON control msg, e.g. {"resize":{"rows":40,"cols":120}}
//   server -> client: binary frame = raw stdout/stderr bytes
//                     text frame   = JSON status/error, e.g. {"exit":0} or {"error":"..."}
func registerPTY(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/exec/pty", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
		cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))

		conn, err := ptyUpgrader.Upgrade(w, r, nil)
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

		done := make(chan struct{})

		// guest stdout -> client (binary)
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
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

		// keepalive pings
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		go func() {
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					_ = send(websocket.PingMessage, nil)
				}
			}
		}()

		// client -> guest stdin
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				if _, werr := pty.Stdin.Write(data); werr != nil {
					return
				}
			case websocket.TextMessage:
				var ctrl struct {
					Resize *struct {
						Rows int `json:"rows"`
						Cols int `json:"cols"`
					} `json:"resize,omitempty"`
					Stdin string `json:"stdin,omitempty"`
				}
				if json.Unmarshal(data, &ctrl) == nil {
					if ctrl.Resize != nil {
						_ = pty.Resize(ctrl.Resize.Rows, ctrl.Resize.Cols)
					}
					if ctrl.Stdin != "" {
						_, _ = pty.Stdin.Write([]byte(ctrl.Stdin))
					}
				}
			}
		}
	})
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
