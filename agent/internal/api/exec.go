// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pandastack/agent/internal/sandbox"
)

type execReq struct {
	Cmd     string `json:"cmd"`
	Timeout int    `json:"timeout_seconds,omitempty"`
}

func registerExec(mux *http.ServeMux, mgr *sandbox.Manager) {
	// POST /sandboxes/{id}/exec   { "cmd": "uname -a" }
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var req execReq
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Cmd == "" {
			writeErr(w, 400, errString("cmd required"))
			return
		}
		res, err := gc.Exec(r.Context(), req.Cmd)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, res)
	})

	// POST /sandboxes/{id}/exec/stream  -> SSE: event=stdout|stderr|exit
	mux.HandleFunc("POST /sandboxes/{id}/exec/stream", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		var req execReq
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Cmd == "" {
			writeErr(w, 400, errString("cmd required"))
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, 500, errString("streaming unsupported"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("x-accel-buffering", "no")
		w.WriteHeader(200)

		send := func(event string, payload any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			flusher.Flush()
		}

		exit, err := gc.ExecStream(r.Context(), req.Cmd, func(stream string, data []byte) {
			send(stream, map[string]string{"chunk": string(data)})
		})
		if err != nil {
			send("error", map[string]string{"error": err.Error()})
			return
		}
		send("exit", map[string]int{"exit_code": exit})
	})
}
