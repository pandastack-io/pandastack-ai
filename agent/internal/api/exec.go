// SPDX-License-Identifier: Apache-2.0
package api

import (
	"net/http"

	"github.com/pandastack/agent/internal/execstream"
	"github.com/pandastack/agent/internal/sandbox"
)

func registerExec(mux *http.ServeMux, mgr *sandbox.Manager) {
	// POST /sandboxes/{id}/exec   { "cmd": "uname -a", "timeout_seconds": 60 }
	mux.HandleFunc("POST /sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		serveExec(w, r, mgr, execstream.Blocking)
	})

	// POST /sandboxes/{id}/exec/stream  -> SSE: event=stdout|stderr|exit|error
	mux.HandleFunc("POST /sandboxes/{id}/exec/stream", func(w http.ResponseWriter, r *http.Request) {
		serveExec(w, r, mgr, execstream.Stream)
	})
}

// serveExec resolves the sandbox to its guest and validates the body; the
// endpoint itself (timeout, stream framing, keep-alive) is in
// internal/execstream, which is tested without a sandbox manager.
func serveExec(w http.ResponseWriter, r *http.Request, mgr *sandbox.Manager,
	run func(http.ResponseWriter, *http.Request, execstream.Guest, execstream.Request)) {
	if err := mgr.CheckRunnable(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 409, err)
		return
	}
	gc, err := mgr.Guest(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	var req execstream.Request
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Cmd == "" {
		writeErr(w, 400, errString("cmd required"))
		return
	}
	run(w, r, gc, req)
}
