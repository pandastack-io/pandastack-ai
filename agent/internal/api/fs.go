// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/pandastack/agent/internal/sandbox"
)

func registerFS(mux *http.ServeMux, mgr *sandbox.Manager) {
	// GET /sandboxes/{id}/fs?path=/etc/hostname
	mux.HandleFunc("GET /sandboxes/{id}/fs", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		path := r.URL.Query().Get("path")
		data, err := gc.ReadFile(r.Context(), path)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		w.Header().Set("content-type", "application/octet-stream")
		_, _ = w.Write(data)
	})

	// PUT /sandboxes/{id}/fs?path=/tmp/foo  (body = raw bytes)
	mux.HandleFunc("PUT /sandboxes/{id}/fs", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		path := r.URL.Query().Get("path")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20)) // 32 MiB cap
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := gc.WriteFile(r.Context(), path, body); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"path": path, "bytes": len(body)})
	})

	// DELETE /sandboxes/{id}/fs?path=/tmp/foo
	mux.HandleFunc("DELETE /sandboxes/{id}/fs", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		if err := gc.DeletePath(r.Context(), r.URL.Query().Get("path")); err != nil {
			writeErr(w, 500, err)
			return
		}
		w.WriteHeader(204)
	})

	// GET /sandboxes/{id}/fs/dir?path=/
	mux.HandleFunc("GET /sandboxes/{id}/fs/dir", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			path = "/"
		}
		entries, err := gc.ListDir(r.Context(), path)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, map[string]any{"path": path, "entries": entries})
	})

	// GET /sandboxes/{id}/fs/stat?path=/etc
	mux.HandleFunc("GET /sandboxes/{id}/fs/stat", func(w http.ResponseWriter, r *http.Request) {
		gc, err := mgr.Guest(r.PathValue("id"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		st, err := gc.Stat(r.Context(), r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		writeJSON(w, 200, st)
	})
}

// Decode a JSON body or write 400 + return false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, 400, err)
		return false
	}
	return true
}
