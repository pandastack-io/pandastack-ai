// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
)

func registerLogs(mux *http.ServeMux, mgr *sandbox.Manager) {
	// GET /sandboxes/{id}/logs?follow=1&tail=200
	mux.HandleFunc("GET /sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		drv := mgr.Driver(r.PathValue("id"))
		if drv == nil {
			writeErr(w, 404, errString("sandbox not found or not running"))
			return
		}
		path := drv.ConsolePath()
		if path == "" {
			path = drv.LogPath()
		}
		follow := r.URL.Query().Get("follow") == "1"

		if !follow {
			data, err := os.ReadFile(path)
			if err != nil {
				writeErr(w, 404, err)
				return
			}
			w.Header().Set("content-type", "text/plain")
			_, _ = w.Write(data)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			writeErr(w, 500, errString("streaming unsupported"))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.WriteHeader(200)

		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			flusher.Flush()
			return
		}
		defer f.Close()
		// For follow, replay last ~4 KB so the user sees recent context, then tail.
		if st, err := f.Stat(); err == nil {
			from := int64(0)
			if st.Size() > 4096 {
				from = st.Size() - 4096
			}
			_, _ = f.Seek(from, 0)
		}
		reader := bufio.NewReader(f)
		send := func(event string, payload any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
			flusher.Flush()
		}
		// Send an initial ping so the client knows the stream is live even if the
		// log file is momentarily empty.
		send("open", map[string]string{"path": path})
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			line, err := reader.ReadString('\n')
			if line != "" {
				send("line", map[string]string{"line": line})
			}
			if err != nil {
				time.Sleep(250 * time.Millisecond)
			}
		}
	})
}
