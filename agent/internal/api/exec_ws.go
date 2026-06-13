// SPDX-License-Identifier: Apache-2.0
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/gorilla/websocket"
)

// Multiplexed WebSocket exec endpoint.
//
// Why: today clients use either POST /exec (blocking, one TCP roundtrip per
// command, ~150-300ms incl TLS) or POST /exec/stream (SSE; can't send more
// commands without a new TCP setup). AI agents that run dozens of commands
// per session waste ~5-10s/session in handshakes. This endpoint multiplexes
// many exec calls on a single persistent WS so per-command latency is
// dominated by SSH dial inside the guest (~5ms) instead of TLS+HTTP.
//
// Wire protocol (all frames are TextMessage JSON):
//
//   client -> server:
//     {"id":"<req-id>", "cmd":"ls -la", "timeout_seconds": 60}
//     {"id":"<req-id>", "cancel": true}
//
//   server -> client:
//     {"id":"<req-id>", "stream":"stdout", "data":"..."}
//     {"id":"<req-id>", "stream":"stderr", "data":"..."}
//     {"id":"<req-id>", "exit": 0}
//     {"id":"<req-id>", "error": "..."}
//
// Notes:
//   - Multiple in-flight requests are allowed; the server tags every frame
//     with the request id, so the client can fan them out.
//   - The connection survives forever; client closes when done.
//   - Ping/pong keepalives every 20s (matches PTY handler).
var execWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

type execWSReq struct {
	ID             string `json:"id"`
	Cmd            string `json:"cmd"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	Cancel         bool   `json:"cancel,omitempty"`
}

type execWSResp struct {
	ID     string `json:"id"`
	Stream string `json:"stream,omitempty"`
	Data   string `json:"data,omitempty"`
	Exit   *int   `json:"exit,omitempty"`
	Error  string `json:"error,omitempty"`
}

func registerExecWS(mux *http.ServeMux, mgr *sandbox.Manager) {
	mux.HandleFunc("GET /sandboxes/{id}/exec/ws", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		conn, err := execWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		var writeMu sync.Mutex
		send := func(msg execWSResp) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteJSON(msg)
		}

		// Track in-flight requests so {"cancel":true} can abort them.
		type inflight struct {
			cancel context.CancelFunc
		}
		var (
			liveMu sync.Mutex
			live   = map[string]*inflight{}
		)
		register := func(id string, cf context.CancelFunc) {
			liveMu.Lock()
			live[id] = &inflight{cancel: cf}
			liveMu.Unlock()
		}
		finish := func(id string) {
			liveMu.Lock()
			delete(live, id)
			liveMu.Unlock()
		}
		cancel := func(id string) {
			liveMu.Lock()
			if f, ok := live[id]; ok {
				f.cancel()
			}
			liveMu.Unlock()
		}

		// Keepalive pings.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					writeMu.Lock()
					_ = conn.WriteMessage(websocket.PingMessage, nil)
					writeMu.Unlock()
				}
			}
		}()

		// Main read loop. Each request runs in its own goroutine so multiple
		// commands can be in flight at once on the same socket.
		for {
			_, raw, rerr := conn.ReadMessage()
			if rerr != nil {
				// Cancel any in-flight requests on disconnect.
				liveMu.Lock()
				for _, f := range live {
					f.cancel()
				}
				liveMu.Unlock()
				return
			}
			var req execWSReq
			if jerr := json.Unmarshal(raw, &req); jerr != nil {
				_ = send(execWSResp{Error: "bad json: " + jerr.Error()})
				continue
			}
			if req.ID == "" {
				_ = send(execWSResp{Error: "id required"})
				continue
			}
			if req.Cancel {
				cancel(req.ID)
				continue
			}
			if req.Cmd == "" {
				_ = send(execWSResp{ID: req.ID, Error: "cmd required"})
				continue
			}

			ctx, cf := context.WithCancel(r.Context())
			if req.TimeoutSeconds > 0 {
				ctx, cf = context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
			}
			register(req.ID, cf)

			go func(req execWSReq, ctx context.Context, cf context.CancelFunc) {
				defer cf()
				defer finish(req.ID)
				exit, err := gc.ExecStream(ctx, req.Cmd, func(stream string, data []byte) {
					_ = send(execWSResp{ID: req.ID, Stream: stream, Data: string(data)})
				})
				if err != nil {
					_ = send(execWSResp{ID: req.ID, Error: err.Error()})
					return
				}
				_ = send(execWSResp{ID: req.ID, Exit: &exit})
			}(req, ctx, cf)
		}
	})
}
