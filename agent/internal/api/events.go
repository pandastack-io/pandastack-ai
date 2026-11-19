// SPDX-License-Identifier: Apache-2.0
package api

import (
	"net/http"
	"strconv"

	"github.com/pandastack/agent/internal/sandbox"
	"github.com/pandastack/agent/internal/sse"
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
		// sse.Stream: headers at once, a keep-alive comment while no event
		// arrives (a follow that is idle for ~100 s is otherwise dropped by
		// the proxies in front), and a writer the ticker can share.
		stream, err := sse.Start(w)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		defer stream.Close()
		send := func(typ string, payload any) { _ = stream.Event(typ, payload) }
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
