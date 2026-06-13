// SPDX-License-Identifier: Apache-2.0
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/pandastack/agent/internal/sandbox"
)

func registerEvents(mux *http.ServeMux, mgr *sandbox.Manager) {
	bus := mgr.Bus()

	// GET /sandboxes/{id}/events?tail=N&follow=1
	mux.HandleFunc("GET /sandboxes/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		tail := 50
		if v := r.URL.Query().Get("tail"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				tail = n
			}
		}
		past, err := bus.Tail(id, tail)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		follow := r.URL.Query().Get("follow") == "1"
		if !follow {
			writeJSON(w, 200, map[string]any{"events": past})
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
		send := func(typ string, payload any) {
			b, _ := json.Marshal(payload)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, b)
			flusher.Flush()
		}
		for _, ev := range past {
			send("event", ev)
		}
		ch := bus.Subscribe(r.Context(), id)
		for {
			select {
			case <-r.Context().Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				send("event", ev)
			}
		}
	})
}
