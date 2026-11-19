// SPDX-License-Identifier: Apache-2.0
package api

import (
	"bufio"
	"net/http"
	"os"
	"time"

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/sse"
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

		// sse.Stream: headers at once, a keep-alive comment while the log is
		// quiet (a follow that is idle for ~100 s is otherwise dropped by the
		// proxies in front), and a writer the ticker can share.
		stream, err := sse.Start(w)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		defer stream.Close()
		send := func(event string, payload any) { _ = stream.Event(event, payload) }

		f, err := os.Open(path)
		if err != nil {
			send("error", map[string]string{"error": err.Error()})
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
